// Package calc is a gradebook as a small spreadsheet of the student's own row (stage 17.6): how a grade is
// counted, and what if.
//
// Built from the view a read keeps (processing/grades: each tab's own cells under their headers, its formulas and
// the works laid out in it) and the few cells outside the own row its formulas read, the header's (Constants: a
// maximum in row 1, the stars marking bonus problems in row 2). A formula reading another student's row is not
// computed: that row is never kept.
//
//   - Name and Written: a formula written with the names of its cells, «10·SUM(1…6) / 6», for the grade card.
//   - Targets, Inputs, Value, Solve: the calculator. A target is a formula cell (the course's grade, a work's
//     points), its inputs the works and the hand-filled cells it is counted from, a formula that is no work being
//     followed through. Any input may be given another value, and the target is counted again as the sheet would;
//     Solve finds the least value of one input that brings the target to a goal (by bisection: a grade grows with
//     its works, rounding and thresholds included).
package calc

import (
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"

	collector "github.com/IvanIkra/infomaster/go/internal/collectors/grades"
	fx "github.com/IvanIkra/infomaster/go/internal/processing/formulas"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

const (
	MaxInputs    = 40
	MaxDepth     = 60
	PendingShare = 0.5 // as grades.GradedShare: a zero in a work most of the group has no points in yet
)

// CellID is "tab!COL".
func CellID(tab, col string) string { return tab + "!" + col }

// Split is a cell id's tab and column.
func Split(cell string) (string, string) {
	i := strings.LastIndex(cell, "!")
	if i < 0 {
		return "", cell
	}
	return cell[:i], cell[i+1:]
}

var numeric = regexp.MustCompile(`(?i)^[\p{Z}\t\n\v\f\r\x{1c}-\x{1f}\x{85}]*-?[\p{Nd}.,]+(e[-+]?\p{Nd}+)?[\p{Z}\t\n\v\f\r\x{1c}-\x{1f}\x{85}]*$`)

// CellValue is a kept cell's text as the value a formula reads: a number when it is one.
func CellValue(raw string) fx.Value {
	if raw == "" {
		return nil
	}
	if numeric.MatchString(raw) {
		if x, ok := fx.PyFloat(strings.ReplaceAll(raw, ",", ".")); ok {
			return x
		}
	}
	return raw
}

// Short is a column's name without the header rows that hold numbers only (a maximum): «7 / 1а» -> «1а».
func Short(header string) string {
	var parts, named []string
	for _, p := range strings.Split(header, " / ") {
		if p = pytext.Strip(p); p != "" {
			parts = append(parts, p)
			if _, isText := CellValue(p).(string); isText {
				named = append(named, p)
			}
		}
	}
	if len(named) > 0 {
		return named[len(named)-1]
	}
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return ""
}

// isPersonRow reports a row whose name column holds a person's name.
func isPersonRow(name string) bool { return name != "" && collector.IsPersonName(name) }

func rowCells(v collector.TabView, row int) (map[string]string, bool) {
	for _, r := range v.Rows {
		if r.Number == row {
			return r.Cells, true
		}
	}
	return nil, false
}

func eachRef(views []collector.TabView, visit func(v collector.TabView, c fx.Cell)) {
	for _, v := range views {
		cols := make([]string, 0, len(v.Formulas))
		for c := range v.Formulas {
			cols = append(cols, c)
		}
		collector.SortColumns(cols)
		for _, col := range cols {
			tree, err := fx.Parse(v.Formulas[col])
			if err != nil {
				continue
			}
			for _, node := range fx.Refs(tree) {
				for _, c := range fx.Cells(node) {
					visit(v, c)
				}
			}
		}
	}
}

// Constants is the cells outside the own rows that the own rows' formulas read, by "tab!A1": a header's, never a
// student's (a row whose name column holds a person's name is left out, and so the formula reading it is not
// computed).
func Constants(views []collector.TabView) map[string]string {
	byTab := map[string]collector.TabView{}
	for _, v := range views {
		byTab[v.Tab] = v
	}
	found := map[string]string{}
	eachRef(views, func(v collector.TabView, c fx.Cell) {
		tab := c.Tab
		if tab == "" {
			tab = v.Tab
		}
		other, ok := byTab[tab]
		if !ok || c.Row == other.OwnRow {
			return
		}
		cells, ok := rowCells(other, c.Row)
		if !ok || isPersonRow(cells[other.NameCol]) {
			return
		}
		if text, ok := cells[c.Col]; ok {
			found[other.Tab+"!"+c.Col+itoa(c.Row)] = text
		}
	})
	return found
}

func itoa(n int) string { return fx.NumberText(float64(n)) }

// Referenced is the own row's columns the own rows' formulas read, by tab, empty ones too (a test not written yet):
// the view keeps them under their headers, so the calculator has them as inputs.
func Referenced(views []collector.TabView) map[string][]string {
	byTab := map[string]collector.TabView{}
	for _, v := range views {
		byTab[v.Tab] = v
	}
	found := map[string][]string{}
	eachRef(views, func(v collector.TabView, c fx.Cell) {
		tab := c.Tab
		if tab == "" {
			tab = v.Tab
		}
		other, ok := byTab[tab]
		if ok && c.Row == other.OwnRow && c.Col != other.NameCol && !slices.Contains(found[other.Tab], c.Col) {
			found[other.Tab] = append(found[other.Tab], c.Col)
		}
	})
	return found
}

type work struct {
	label  string
	max    *float64
	bonus  bool
	graded *float64
}

// Tab is one tab of a Book.
type Tab struct {
	Name     string
	OwnRow   int
	Values   map[string]fx.Value
	Headers  map[string]string
	Formulas map[string]string
	Notes    map[string]string
	works    map[string]work // by column: the works laid out, and their bonus columns
	order    []string        // the formula columns, left to right
	NameCol  string
	trees    map[string]*fx.Node
	parsed   map[string]bool
}

func (t *Tab) tree(col string) *fx.Node {
	if !t.parsed[col] {
		t.parsed[col] = true
		if formula, ok := t.Formulas[col]; ok {
			if tree, err := fx.Parse(formula); err == nil {
				t.trees[col] = tree
			}
		}
	}
	return t.trees[col]
}

// Book is a gradebook's own rows as a spreadsheet.
type Book struct {
	Tabs      map[string]*Tab
	order     []string
	Constants map[string]fx.Value
}

func ptr(f *pyjson.Float) *float64 {
	if f == nil {
		return nil
	}
	x := float64(*f)
	return &x
}

// NewBook builds the book of a view.
func NewBook(view View) *Book {
	b := &Book{Tabs: map[string]*Tab{}, Constants: map[string]fx.Value{}}
	for _, t := range view.Tabs {
		works := map[string]work{}
		for _, w := range t.Works {
			if w.Column == "" {
				continue
			}
			maximum := float64(w.Max)
			works[w.Column] = work{w.Label, &maximum, false, ptr(w.Graded)}
			if w.BonusColumn != nil && *w.BonusColumn != "" {
				if _, ok := works[*w.BonusColumn]; !ok {
					works[*w.BonusColumn] = work{w.Label, nil, true, ptr(w.Graded)}
				}
			}
		}
		tab := &Tab{Name: t.Tab, OwnRow: t.OwnRow, Values: map[string]fx.Value{}, Headers: map[string]string{},
			Formulas: t.Formulas, Notes: t.Notes, works: works, NameCol: t.NameCol, trees: map[string]*fx.Node{},
			parsed: map[string]bool{}}
		if tab.Formulas == nil {
			tab.Formulas = map[string]string{}
		}
		for _, c := range t.Columns {
			tab.Values[c.Column] = CellValue(c.Value)
			tab.Headers[c.Column] = c.Header
		}
		for c := range tab.Formulas {
			tab.order = append(tab.order, c)
		}
		collector.SortColumns(tab.order)
		if _, seen := b.Tabs[t.Tab]; !seen {
			b.order = append(b.order, t.Tab)
		}
		b.Tabs[t.Tab] = tab
	}
	for key, value := range view.Constants {
		b.Constants[key] = CellValue(value)
	}
	return b
}

// --- names ------------------------------------------------------------------------------------------------------

// Label is a cell's name: its work's label (a star for a bonus column), else its short header, else its column.
func (b *Book) Label(tab, col string) string {
	t := b.Tabs[tab]
	if t == nil {
		return tab + "!" + col
	}
	if w, ok := t.works[col]; ok && w.label != "" {
		if w.bonus {
			return w.label + "*"
		}
		return w.label
	}
	if s := Short(t.Headers[col]); s != "" {
		return s
	}
	return col
}

// Name is a reference or a range by the names of its cells, the tab added when it is another one.
func (b *Book) Name(node *fx.Node, home string) string {
	tab := node.Tab
	if tab == "" {
		tab = home
	}
	t := b.Tabs[tab]
	where := ""
	if tab != home {
		where = tab + ": "
	}
	if node.Kind == "ref" {
		if t != nil && node.Row == t.OwnRow {
			return where + b.Label(tab, node.Col)
		}
		value, ok := b.Constants[tab+"!"+node.Col+itoa(node.Row)]
		if x, isNumber := value.(float64); isNumber {
			return fx.NumberText(x)
		}
		if !ok || value == nil {
			return tab + "!" + node.Col + itoa(node.Row)
		}
		return "«" + fx.Str(value) + "»"
	}
	if t != nil && node.Row == t.OwnRow && node.Row2 == t.OwnRow {
		return where + b.Label(tab, node.Col) + "…" + b.Label(tab, node.Col2)
	}
	return where + node.Col + itoa(node.Row) + ":" + node.Col2 + itoa(node.Row2)
}

// Written is the formula of an own cell with names: «10·SUM(1…6) / 6»; "" for no formula, or one not read.
func (b *Book) Written(tab, col string) string {
	t := b.Tabs[tab]
	if t == nil {
		return ""
	}
	tree := t.tree(col)
	if tree == nil || len(fx.Refs(tree)) == 0 {
		return ""
	}
	return fx.Render(tree, func(node *fx.Node) string { return b.Name(node, tab) })
}

// FormulaCells are the own formula cells that read cells, tab by tab, left to right.
func (b *Book) FormulaCells() [][2]string {
	var out [][2]string
	for _, name := range b.order {
		t := b.Tabs[name]
		for _, col := range t.order {
			if tree := t.tree(col); tree != nil && len(fx.Refs(tree)) > 0 {
				out = append(out, [2]string{name, col})
			}
		}
	}
	return out
}

// --- values -------------------------------------------------------------------------------------------------------

// Compute is the value of an own cell, counted from its formula with the given values over the sheet's. An error
// when the sheet would show an error, or the formula reads what is not kept.
func (b *Book) Compute(tab, col string, given map[string]*float64) (fx.Value, error) {
	cache := map[string]fx.Value{}
	var own func(tab, col string, depth int) (fx.Value, error)
	lookup := func(tab, col string, row, depth int) (fx.Value, error) {
		t := b.Tabs[tab]
		if t != nil && row == t.OwnRow {
			return own(tab, col, depth)
		}
		key := tab + "!" + col + itoa(row)
		if v, ok := b.Constants[key]; ok {
			return v, nil
		}
		if t != nil && row < t.OwnRow { // a header cell left empty
			return nil, nil
		}
		return nil, &fx.CellError{Code: "#REF!"} // another student's row, or a tab not kept
	}
	own = func(tab, col string, depth int) (fx.Value, error) {
		key := CellID(tab, col)
		if v, ok := given[key]; ok {
			if v == nil {
				return nil, nil
			}
			return *v, nil
		}
		if v, ok := cache[key]; ok {
			return v, nil
		}
		t := b.Tabs[tab]
		if t == nil {
			return nil, &fx.CellError{Code: "#REF!"}
		}
		tree := t.tree(col)
		if tree == nil {
			if _, ok := t.Formulas[col]; ok {
				return nil, &fx.CellError{Code: "unsupported"}
			}
			return t.Values[col], nil
		}
		if depth > MaxDepth {
			return nil, &fx.CellError{Code: "#CYCLE!"}
		}
		value, err := fx.Evaluate(tree, func(tb, c string, r int) (fx.Value, error) {
			if tb == "" {
				tb = tab
			}
			return lookup(tb, c, r, depth+1)
		})
		if err != nil {
			return nil, err
		}
		cache[key] = value
		return value, nil
	}
	return own(tab, col, 0)
}

// Value is Compute, nil for any error: a teacher's formula the evaluator cannot count leaves the cell unknown, not
// the page broken.
func (b *Book) Value(tab, col string, given map[string]*float64) fx.Value {
	v, err := b.Compute(tab, col, given)
	if err != nil {
		return nil
	}
	return v
}

// --- the calculator -----------------------------------------------------------------------------------------------

// Depends is the inputs of a formula cell: the works and the own cells without formulas it is counted from, in
// order; a formula that is no work is followed into.
func (b *Book) Depends(tab, col string) [][2]string {
	var found [][2]string
	seen := map[string]bool{}
	var walk func(tab, col string, top bool, depth int)
	walk = func(tab, col string, top bool, depth int) {
		key := CellID(tab, col)
		if seen[key] || depth > MaxDepth || len(found) >= MaxInputs {
			return
		}
		seen[key] = true
		t := b.Tabs[tab]
		if t == nil {
			return
		}
		if !top && t.NameCol != "" && fx.ColIndex(col) <= fx.ColIndex(t.NameCol) {
			return // the group's number, the name: no input
		}
		_, isWork := t.works[col]
		_, hasFormula := t.Formulas[col]
		if !top && (isWork || !hasFormula) {
			_, hasValue := t.Values[col]
			_, hasHeader := t.Headers[col] // a column of the own row's span, empty now
			if hasValue || isWork || hasHeader {
				found = append(found, [2]string{tab, col})
			}
			return
		}
		tree := t.tree(col)
		if tree == nil {
			return
		}
		for _, node := range fx.Refs(tree) {
			for _, c := range fx.Cells(node) {
				name := c.Tab
				if name == "" {
					name = tab
				}
				if other := b.Tabs[name]; other != nil && c.Row == other.OwnRow {
					walk(other.Name, c.Col, false, depth+1)
				}
			}
		}
	}
	walk(tab, col, true, 0)
	return found
}

// Target is a formula cell a calculator may aim at.
type Target struct {
	Cell   string `json:"cell"`
	Tab    string `json:"tab"`
	Name   string `json:"name"`
	Value  any    `json:"value"` // nil, a pyjson.Float or a string
	Inputs int    `json:"inputs"`
	Text   bool   `json:"text"`
	Header string `json:"header"`
}

func shown(v fx.Value) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		if x {
			return pyjson.Float(1)
		}
		return pyjson.Float(0)
	case float64:
		return pyjson.Float(x)
	}
	return fx.Str(v)
}

// Targets is the formula cells a calculator may aim at, each with its value and how many inputs it has.
func (b *Book) Targets() []Target {
	var listed []Target
	for _, cell := range b.FormulaCells() {
		tab, col := cell[0], cell[1]
		value := b.Value(tab, col, nil)
		inputs := b.Depends(tab, col)
		if len(inputs) == 0 {
			continue
		}
		s, isText := value.(string)
		listed = append(listed, Target{CellID(tab, col), tab, b.Label(tab, col), shown(value), len(inputs),
			isText && s != "", b.Tabs[tab].Headers[col]})
	}
	return listed
}

// Height is how many formulas deep a cell is counted: a course's grade sits on top of its works.
func (b *Book) Height(tab, col string, depth int) int {
	t := b.Tabs[tab]
	if t == nil || depth > MaxDepth {
		return 0
	}
	tree := t.tree(col)
	if tree == nil {
		return 0
	}
	best := 0
	for _, node := range fx.Refs(tree) {
		for _, c := range fx.Cells(node) {
			name := c.Tab
			if name == "" {
				name = tab
			}
			if o := b.Tabs[name]; o != nil && c.Row == o.OwnRow {
				best = max(best, b.Height(name, c.Col, depth+1))
			}
		}
	}
	return 1 + best
}

// final is `итог|оценк|grade|final|\bИК|\bНК` with Python's Unicode word boundary.
func final(header string) bool {
	lower := []rune(strings.ToLower(header))
	text := string(lower)
	for _, word := range []string{"итог", "оценк", "grade", "final"} {
		if strings.Contains(text, word) {
			return true
		}
	}
	for i := 0; i+1 < len(lower); i++ {
		if (lower[i] == 'и' || lower[i] == 'н') && lower[i+1] == 'к' && (i == 0 || !isWord(lower[i-1])) {
			return true
		}
	}
	return false
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

// DefaultTarget is the course's grade, as well as the sheet tells it: of the cells whose value is a grade (no text,
// no 0/1 flag), the one on top of the most formulas; a header like «Итог» or «Оценка» first. "" for none.
func (b *Book) DefaultTarget(listed []Target) string {
	var fit, named []int
	for i, t := range listed {
		x, isNumber := t.Value.(pyjson.Float)
		if t.Text || (isNumber && (x == 0 || x == 1) && !final(t.Header)) {
			continue
		}
		fit = append(fit, i)
		if final(t.Header) {
			named = append(named, i)
		}
	}
	if len(fit) == 0 {
		return ""
	}
	pool := fit
	if len(named) > 0 {
		pool = named
	}
	best := -1
	var bestKey [3]int
	for _, i := range pool {
		tab, col := Split(listed[i].Cell)
		key := [3]int{b.Height(tab, col, 0), listed[i].Inputs, -i}
		if best < 0 || key[0] > bestKey[0] || key[0] == bestKey[0] && (key[1] > bestKey[1] ||
			key[1] == bestKey[1] && key[2] > bestKey[2]) {
			best, bestKey = i, key
		}
	}
	return listed[best].Cell
}

// Input is one input of a target.
type Input struct {
	Cell    string        `json:"cell"`
	Tab     string        `json:"tab"`
	Name    string        `json:"name"`
	Sheet   any           `json:"sheet"` // nil, a pyjson.Float or a string
	Value   any           `json:"value"`
	Max     *pyjson.Float `json:"max"`
	Work    bool          `json:"work"`
	Changed bool          `json:"changed"`
	Pending bool          `json:"pending"`
}

// Inputs is the inputs of a target with the sheet's values and the given ones.
func (b *Book) Inputs(target string, given map[string]*float64) []Input {
	tab, col := Split(target)
	var listed []Input
	for _, cell := range b.Depends(tab, col) {
		t := b.Tabs[cell[0]]
		w, isWork := t.works[cell[1]]
		var sheet fx.Value
		if _, ok := t.Formulas[cell[1]]; ok {
			sheet = b.Value(cell[0], cell[1], nil)
		} else {
			sheet = t.Values[cell[1]]
		}
		value := shown(sheet)
		key := CellID(cell[0], cell[1])
		in := Input{Cell: key, Tab: cell[0], Name: b.Label(cell[0], cell[1]), Sheet: value, Value: value,
			Work: isWork}
		if v, ok := given[key]; ok {
			in.Changed = true
			if v == nil {
				in.Value = nil
			} else {
				in.Value = pyjson.Float(*v)
			}
		}
		if isWork && w.max != nil {
			m := pyjson.Float(*w.max)
			in.Max = &m
		}
		zero := value == nil || value == pyjson.Float(0)
		in.Pending = isWork && !w.bonus && zero && (w.graded == nil || *w.graded < PendingShare)
		listed = append(listed, in)
	}
	return listed
}

// Solved is what Solve found: the least value needed (nil when even the maximum does not reach the goal).
type Solved struct {
	Need    *pyjson.Float `json:"need"`
	Reached bool          `json:"reached"`
	Max     pyjson.Float  `json:"max"`
}

// Solve is the least value of cell in [0, its maximum or 10] that brings target to goal or above, the others as
// given: the need rounded up to 0.01; Need nil when even the maximum does not bring it there, 0 when nothing is
// needed.
func (b *Book) Solve(target, cell string, goal float64, given map[string]*float64) Solved {
	tab, col := Split(target)
	top := 10.0
	if t := b.Tabs[func() string { tb, _ := Split(cell); return tb }()]; t != nil {
		_, c := Split(cell)
		if w, ok := t.works[c]; ok && w.max != nil && *w.max != 0 {
			top = *w.max
		}
	}
	at := func(x float64) (float64, bool) {
		with := map[string]*float64{}
		for k, v := range given {
			with[k] = v
		}
		with[cell] = &x
		switch v := b.Value(tab, col, with).(type) {
		case float64:
			return v, true
		case bool:
			if v {
				return 1, true
			}
			return 0, true
		}
		return 0, false
	}
	high, highOK := at(top)
	if !highOK || high < goal-1e-9 {
		return Solved{nil, false, pyjson.Float(top)}
	}
	if low, ok := at(0); ok && low >= goal-1e-9 {
		zero := pyjson.Float(0)
		return Solved{&zero, true, pyjson.Float(top)}
	}
	lo, hi := 0.0, top
	for range 60 {
		mid := (lo + hi) / 2
		if v, ok := at(mid); ok && v >= goal-1e-9 {
			hi = mid
		} else {
			lo = mid
		}
	}
	need := math.Ceil(hi*100-1e-6) / 100
	if v, ok := at(need); !ok || v < goal-1e-9 {
		need = hi
	}
	result := pyjson.Float(min(fx.Round(need, 2), top))
	return Solved{&result, true, pyjson.Float(top)}
}

// WrittenCells is every own formula cell with refs, written with names: "tab!COL" -> text.
func WrittenCells(b *Book) map[string]string {
	out := map[string]string{}
	for _, cell := range b.FormulaCells() {
		if text := b.Written(cell[0], cell[1]); text != "" {
			out[CellID(cell[0], cell[1])] = text
		}
	}
	return out
}
