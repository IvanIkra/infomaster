// Package formulas is the formulas of the student's own row in a gradebook (stage 17.6): parsed, evaluated,
// written out by name.
//
// A teacher's sheet computes a work from its problems (`=10*SUM(E59:J59)/6`) and a course's grade from its works
// (`=ROUND(0.35*D59+0.35*E59+0.3*F59,0)`). The own row's formulas come with the read (collectors/grades), and this
// package is what the web app's grade card and calculator stand on: a small spreadsheet language, the part of
// Excel's the gradebooks use, evaluated as Excel would. No library: a formula is parsed into a tree, and a function
// or a form this parser does not know makes the formula Unsupported, so it is shown but not computed.
//
// Values are float64, string, bool or nil (an empty cell). An error of Excel's (#DIV/0!, #VALUE!) is a *CellError:
// IFERROR catches it, and anything else gives no value.
package formulas

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// Value is float64, string, bool or nil.
type Value = any

// Node is a formula's tree. Kind is one of num, str, bool, ref, range, bin, neg, call; Tab is "" for the formula's
// own tab.
type Node struct {
	Kind string
	Num  float64
	Str  string // a str's text, a bin's operator, a call's function
	Bool bool
	Tab  string
	Col  string
	Row  int
	Col2 string // a range's corner
	Row2 int
	Args []*Node // a call's arguments; a bin's two sides; a neg's one
}

// Cell is one cell a reference covers.
type Cell struct {
	Tab string
	Col string
	Row int
}

// Unsupported is a formula this parser does not read: shown as it is, never computed.
type Unsupported struct{ Msg string }

func (e *Unsupported) Error() string { return e.Msg }

// CellError is an Excel error value (#DIV/0!, #VALUE!, #REF!).
type CellError struct{ Code string }

func (e *CellError) Error() string { return e.Code }

// ArithError is Python's ArithmeticError: an overflow IFERROR does not catch; the cell has no value.
type ArithError struct{ Msg string }

func (e *ArithError) Error() string { return e.Msg }

func unsupported(format string, args ...any) error { return &Unsupported{fmt.Sprintf(format, args...)} }

// --- references ------------------------------------------------------------------------------------------------

// ColIndex is A -> 0, AA -> 26.
func ColIndex(col string) int {
	n := 0
	for _, ch := range col {
		n = n*26 + int(ch) - 64
	}
	return n - 1
}

// ColName is 0 -> A, 26 -> AA.
func ColName(index int) string {
	letters := ""
	index++
	for index > 0 {
		rest := (index - 1) % 26
		index = (index - 1) / 26
		letters = string(rune(65+rest)) + letters
	}
	return letters
}

// --- tokens ----------------------------------------------------------------------------------------------------

type token struct{ kind, text string }

// isWord is Python's \w: Unicode letters and numbers, and "_".
func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func isASCIILetter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isCyrillic(r rune) bool { return r >= 'А' && r <= 'я' || r == 'Ё' || r == 'ё' }

// quoted is the ends a `'(?:[^']|”)+'` (or `"(?:[^"]|"")*"`) match starting at s[i] may have, the greedy one first,
// then those a regex reaches by backtracking: each doubled quote taken as the closing one.
func quoted(s []rune, i int, q rune, empty bool) []int {
	var pairs []int
	j := i + 1
	for j < len(s) {
		if s[j] != q {
			j++
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			pairs = append(pairs, j)
			j += 2
			continue
		}
		break
	}
	var ends []int
	if j < len(s) && (empty || j > i+1) {
		ends = append(ends, j+1)
	}
	for k := len(pairs) - 1; k >= 0; k-- {
		if empty || pairs[k] > i+1 {
			ends = append(ends, pairs[k]+1)
		}
	}
	return ends
}

// cellEnd matches `\$?[A-Za-z]{1,3}\$?\d+` at s[i].
func cellEnd(s []rune, i int) (int, bool) {
	if i < len(s) && s[i] == '$' {
		i++
	}
	start := i
	for i < len(s) && isASCIILetter(s[i]) {
		i++
	}
	if n := i - start; n < 1 || n > 3 {
		return 0, false
	}
	if i < len(s) && s[i] == '$' {
		i++
	}
	start = i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i, i > start
}

func refFollow(s []rune, i int) bool { return i >= len(s) || !(isWord(s[i]) || s[i] == '(') }

// refAt matches the ref token: an optional tab, a cell, an optional second cell, not followed by a word or "(".
func refAt(s []rune, i int) (int, bool) {
	var starts []int
	switch {
	case i < len(s) && s[i] == '\'':
		for _, end := range quoted(s, i, '\'', false) {
			if end < len(s) && s[end] == '!' {
				starts = append(starts, end+1)
			}
		}
	case i < len(s) && (isASCIILetter(s[i]) || isCyrillic(s[i]) || s[i] == '_'):
		j := i + 1
		for j < len(s) && (isWord(s[j]) || isCyrillic(s[j]) || s[j] == '.') {
			j++
		}
		if j < len(s) && s[j] == '!' {
			starts = append(starts, j+1)
		}
	}
	starts = append(starts, i)
	for _, start := range starts {
		end, ok := cellEnd(s, start)
		if !ok {
			continue
		}
		if end < len(s) && s[end] == ':' {
			if end2, ok := cellEnd(s, end+1); ok && refFollow(s, end2) {
				return end2, true
			}
		}
		if refFollow(s, end) {
			return end, true
		}
	}
	return 0, false
}

func digitsEnd(s []rune, i int) int {
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i
}

func exponentEnd(s []rune, i int) int {
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if k := digitsEnd(s, j); k > j {
			return k
		}
	}
	return i
}

func numAt(s []rune, i int) (int, bool) {
	if j := digitsEnd(s, i); j > i {
		if j < len(s) && s[j] == '.' {
			j = digitsEnd(s, j+1)
		}
		return exponentEnd(s, j), true
	}
	if i < len(s) && s[i] == '.' {
		if j := digitsEnd(s, i+1); j > i+1 {
			return exponentEnd(s, j), true
		}
	}
	return 0, false
}

func nameAt(s []rune, i int) (int, bool) {
	if i >= len(s) || !(isASCIILetter(s[i]) || s[i] == '_') {
		return 0, false
	}
	j := i + 1
	for j < len(s) && (isASCIILetter(s[j]) || isDigit(s[j]) || s[j] == '_' || s[j] == '.') {
		j++
	}
	k := j
	for k < len(s) && pytext.IsSpace(s[k]) {
		k++
	}
	return j, k < len(s) && s[k] == '('
}

func boolAt(s []rune, i int) (int, bool) {
	for _, word := range []string{"TRUE", "FALSE"} {
		n := len(word)
		if i+n <= len(s) && strings.EqualFold(string(s[i:i+n]), word) && refFollow(s, i+n) {
			return i + n, true
		}
	}
	return 0, false
}

func opAt(s []rune, i int) (int, bool) {
	if i+1 < len(s) {
		if two := string(s[i : i+2]); two == "<>" || two == "<=" || two == ">=" {
			return i + 2, true
		}
	}
	if i < len(s) && strings.ContainsRune("-+*/^&=<>(),;%", s[i]) {
		return i + 1, true
	}
	return 0, false
}

func tokens(formula string) ([]token, error) {
	s := []rune(formula)
	var found []token
	for at := 0; at < len(s); {
		if pytext.IsSpace(s[at]) {
			for at < len(s) && pytext.IsSpace(s[at]) {
				at++
			}
			continue
		}
		kind, end := "", 0
		if s[at] == '"' {
			if ends := quoted(s, at, '"', true); len(ends) > 0 {
				kind, end = "str", ends[0]
			}
		}
		matchers := []struct {
			kind string
			at   func([]rune, int) (int, bool)
		}{{"ref", refAt}, {"num", numAt}, {"name", nameAt}, {"bool", boolAt}, {"op", opAt}}
		for _, m := range matchers {
			if kind != "" {
				break
			}
			if e, ok := m.at(s, at); ok {
				kind, end = m.kind, e
			}
		}
		if kind == "" {
			rest := s[at:]
			if len(rest) > 12 {
				rest = rest[:12]
			}
			return nil, unsupported("cannot read '%s'", string(rest))
		}
		found = append(found, token{kind, string(s[at:end])})
		at = end
	}
	return found, nil
}

// Limits (D79): a teacher's formula cannot exhaust the service.
const (
	MaxRow       = 1_048_576 // a sheet's last row
	RangeCells   = 20_000    // cells of one range: a gradebook's column is a few hundred
	FormulaChars = 4000
	DepthMax     = 64 // nested parentheses and signs
)

var cellRe = regexp.MustCompile(`^\$?([A-Za-z]{1,3})\$?([0-9]+)$`)

func reference(text string) (*Node, error) {
	tab := ""
	if i := strings.LastIndex(text, "!"); i >= 0 {
		tab, text = text[:i], text[i+1:]
		if strings.HasPrefix(tab, "'") {
			tab = strings.ReplaceAll(tab[1:len(tab)-1], "''", "'")
		}
	}
	parts := strings.Split(text, ":")
	cols, rows := make([]string, len(parts)), make([]int, len(parts))
	for i, part := range parts {
		m := cellRe.FindStringSubmatch(part)
		if m == nil {
			return nil, unsupported("cannot read %s", part)
		}
		cols[i] = strings.ToUpper(m[1])
		row, err := strconv.Atoi(m[2])
		if err != nil {
			return nil, unsupported("cannot read %s", part)
		}
		rows[i] = row
	}
	if len(parts) == 1 {
		return &Node{Kind: "ref", Tab: tab, Col: cols[0], Row: rows[0]}, nil
	}
	c1, c2, r1, r2 := cols[0], cols[1], rows[0], rows[1]
	if ColIndex(c2) < ColIndex(c1) {
		c1, c2 = c2, c1
	}
	if r2 < r1 {
		r1, r2 = r2, r1
	}
	if r2 > MaxRow || (ColIndex(c2)-ColIndex(c1)+1)*(r2-r1+1) > RangeCells {
		return nil, unsupported("a range too big to count") // D79: it is expanded cell by cell
	}
	return &Node{Kind: "range", Tab: tab, Col: c1, Row: r1, Col2: c2, Row2: r2}, nil
}

// --- the parser ------------------------------------------------------------------------------------------------

// Functions are the functions this package computes.
var Functions = map[string]bool{"SUM": true, "AVERAGE": true, "COUNT": true, "COUNTA": true, "MIN": true,
	"MAX": true, "ROUND": true, "ROUNDUP": true, "ROUNDDOWN": true, "IF": true, "IFERROR": true, "AND": true,
	"OR": true, "NOT": true, "ISNUMBER": true, "ISBLANK": true, "SUMIF": true, "ABS": true}

var levels = []map[string]bool{{"=": true, "<>": true, "<": true, ">": true, "<=": true, ">=": true}, {"&": true},
	{"+": true, "-": true}, {"*": true, "/": true}, {"^": true}}

type parser struct {
	toks []token
	at   int
}

func (p *parser) peek() *token {
	if p.at < len(p.toks) {
		return &p.toks[p.at]
	}
	return nil
}

func (p *parser) take(value string) (token, error) {
	tok := p.peek()
	if tok == nil || (value != "" && tok.text != value) {
		if value == "" {
			value = "more"
		}
		return token{}, unsupported("expected %s", value)
	}
	p.at++
	return *tok, nil
}

func (p *parser) isOp(text string) bool {
	tok := p.peek()
	return tok != nil && tok.kind == "op" && tok.text == text
}

func (p *parser) level(n int) (*Node, error) {
	if n == len(levels) {
		return p.unary()
	}
	node, err := p.level(n + 1)
	if err != nil {
		return nil, err
	}
	for tok := p.peek(); tok != nil && tok.kind == "op" && levels[n][tok.text]; tok = p.peek() {
		op := tok.text
		p.at++
		right, err := p.level(n + 1)
		if err != nil {
			return nil, err
		}
		node = &Node{Kind: "bin", Str: op, Args: []*Node{node, right}}
	}
	return node, nil
}

func (p *parser) unary() (*Node, error) {
	if tok := p.peek(); tok != nil && tok.kind == "op" && (tok.text == "+" || tok.text == "-") {
		sign := tok.text
		p.at++
		inner, err := p.unary()
		if err != nil {
			return nil, err
		}
		if sign == "-" {
			return &Node{Kind: "neg", Args: []*Node{inner}}, nil
		}
		return inner, nil
	}
	node, err := p.primary()
	if err != nil {
		return nil, err
	}
	if p.isOp("%") {
		p.at++
		node = &Node{Kind: "bin", Str: "/", Args: []*Node{node, {Kind: "num", Num: 100}}}
	}
	return node, nil
}

func (p *parser) primary() (*Node, error) {
	tok, err := p.take("")
	if err != nil {
		return nil, err
	}
	switch tok.kind {
	case "num":
		x, err := strconv.ParseFloat(tok.text, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, unsupported("cannot read %s", tok.text)
		}
		return &Node{Kind: "num", Num: x}, nil // out of range: inf, as Python's float()
	case "str":
		return &Node{Kind: "str", Str: strings.ReplaceAll(tok.text[1:len(tok.text)-1], `""`, `"`)}, nil
	case "bool":
		return &Node{Kind: "bool", Bool: strings.ToUpper(tok.text) == "TRUE"}, nil
	case "ref":
		return reference(tok.text)
	case "name":
		name := strings.TrimPrefix(strings.ToUpper(tok.text), "_XLFN.")
		if !Functions[name] {
			return nil, unsupported("the function %s", name)
		}
		if _, err := p.take("("); err != nil {
			return nil, err
		}
		var args []*Node
		if !p.isOp(")") {
			for {
				arg, err := p.level(0)
				if err != nil {
					return nil, err
				}
				args = append(args, arg)
				if p.isOp(",") || p.isOp(";") {
					p.at++
					continue
				}
				break
			}
		}
		if _, err := p.take(")"); err != nil {
			return nil, err
		}
		return &Node{Kind: "call", Str: name, Args: args}, nil
	}
	if tok.kind == "op" && tok.text == "(" {
		node, err := p.level(0)
		if err != nil {
			return nil, err
		}
		if _, err := p.take(")"); err != nil {
			return nil, err
		}
		return node, nil
	}
	return nil, unsupported("unexpected '%s'", tok.text)
}

// Parse reads `=...` into a tree: *Unsupported for anything else, or a function this package does not compute.
func Parse(formula string) (*Node, error) {
	if !strings.HasPrefix(formula, "=") {
		return nil, unsupported("not a formula")
	}
	if pytext.Len(formula) > FormulaChars {
		return nil, unsupported("a formula too long")
	}
	toks, err := tokens(formula[1:])
	if err != nil {
		return nil, err
	}
	depth, deepest, signs := 0, 0, 0
	for _, tok := range toks { // the parser recurses per parenthesis and per sign in a row
		if tok.kind == "op" {
			if tok.text == "(" {
				depth++
			} else if tok.text == ")" {
				depth--
			}
		}
		if tok.kind == "op" && (tok.text == "+" || tok.text == "-") {
			signs++
		} else {
			signs = 0
		}
		deepest = max(deepest, depth, signs)
	}
	if deepest > DepthMax {
		return nil, unsupported("a formula nested too deep")
	}
	p := &parser{toks: toks}
	tree, err := p.level(0)
	if err != nil {
		return nil, err
	}
	if p.at != len(toks) {
		return nil, unsupported("unexpected '%s'", toks[p.at].text)
	}
	return tree, nil
}

// Refs are the references and ranges of a tree, in order.
func Refs(node *Node) []*Node {
	switch node.Kind {
	case "ref", "range":
		return []*Node{node}
	case "bin", "neg", "call":
		var out []*Node
		for _, arg := range node.Args {
			out = append(out, Refs(arg)...)
		}
		return out
	}
	return nil
}

// Cells is every cell a reference or a range covers, row by row.
func Cells(node *Node) []Cell {
	if node.Kind == "ref" {
		return []Cell{{node.Tab, node.Col, node.Row}}
	}
	var out []Cell
	for r := node.Row; r <= node.Row2; r++ {
		for c := ColIndex(node.Col); c <= ColIndex(node.Col2); c++ {
			out = append(out, Cell{node.Tab, ColName(c), r})
		}
	}
	return out
}
