package formulas

import (
	"errors"
	"math"
	"regexp"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// Lookup reads a cell: tab "" is the formula's own tab.
type Lookup func(tab, col string, row int) (Value, error)

// IsNumber reports a float (a bool is no number).
func IsNumber(v Value) bool {
	_, ok := v.(float64)
	return ok
}

// Str is Python's str() of a value: 1.0, True, the text; "" for an empty cell.
func Str(v Value) string {
	switch x := v.(type) {
	case nil:
		return ""
	case float64:
		return Repr(x)
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	}
	return ""
}

var errValue = &CellError{"#VALUE!"}

// ToNumber is a value as a number: an empty cell is 0, a text is read or #VALUE!.
func ToNumber(v Value) (float64, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float64:
		return x, nil
	case string:
		text := pytext.Strip(x)
		if text == "" {
			return 0, nil
		}
		n, ok := PyFloat(strings.ReplaceAll(text, ",", "."))
		if !ok || math.IsInf(n, 0) || math.IsNaN(n) { // "nan", "inf" are no numbers of a sheet
			return 0, errValue
		}
		return n, nil
	}
	return 0, errValue
}

func toBool(v Value) (bool, error) {
	if s, ok := v.(string); ok {
		switch strings.ToUpper(s) {
		case "TRUE":
			return true, nil
		case "FALSE":
			return false, nil
		}
		return false, errValue
	}
	n, err := ToNumber(v)
	return n != 0, err
}

// roundHalfUp is Excel's ROUND: half away from zero.
func roundHalfUp(x float64, digits int) (float64, error) {
	digits = max(-15, min(15, digits))
	q := math.Pow(10, float64(digits))
	scaled := math.Abs(x)*q + 0.5 + 1e-9
	if math.IsInf(scaled, 0) || math.IsNaN(scaled) {
		return 0, &ArithError{"cannot convert float infinity to integer"}
	}
	return math.Copysign(math.Floor(scaled)/q, x), nil
}

func wildcard(pattern string) *regexp.Regexp {
	var out strings.Builder
	escape := false
	for _, ch := range pattern {
		switch {
		case escape:
			out.WriteString(regexp.QuoteMeta(string(ch)))
			escape = false
		case ch == '~':
			escape = true
		case ch == '*':
			out.WriteString(".*")
		case ch == '?':
			out.WriteString(".")
		default:
			out.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	return regexp.MustCompile(`(?is)^(?:` + out.String() + `)$`)
}

var criterionRe = regexp.MustCompile(`(?s)^(<>|<=|>=|<|>|=)?(.*)$`)

// criterion is SUMIF's criteria: "*~*" (a header ending with *), ">=5", 3.
func criterion(spec Value) (func(Value) bool, error) {
	if IsNumber(spec) {
		target, _ := ToNumber(spec)
		return func(v Value) bool { x, ok := v.(float64); return ok && x == target }, nil
	}
	if b, ok := spec.(bool); ok {
		target := 0.0
		if b {
			target = 1
		}
		return func(v Value) bool { x, ok := v.(float64); return ok && x == target }, nil
	}
	text := ""
	if spec != nil {
		text = Str(spec)
	}
	m := criterionRe.FindStringSubmatch(text)
	op, rest := m[1], m[2]
	target, isTarget := PyFloat(strings.ReplaceAll(rest, ",", "."))
	if isTarget && op != "" {
		return func(v Value) bool {
			x, ok := v.(float64)
			return ok && compareFloats(op, x, target)
		}, nil
	}
	if isTarget {
		return func(v Value) bool { x, ok := v.(float64); return ok && x == target }, nil
	}
	pattern := wildcard(rest)
	if op == "<>" {
		return func(v Value) bool { return !pattern.MatchString(Str(v)) }, nil
	}
	return func(v Value) bool { return v != nil && pattern.MatchString(Str(v)) }, nil
}

func compareFloats(op string, a, b float64) bool {
	switch op {
	case "=":
		return a == b
	case "<>":
		return a != b
	case "<":
		return a < b
	case ">":
		return a > b
	case "<=":
		return a <= b
	}
	return a >= b
}

func compareStrings(op string, a, b string) bool {
	switch op {
	case "=":
		return a == b
	case "<>":
		return a != b
	case "<":
		return a < b
	case ">":
		return a > b
	case "<=":
		return a <= b
	}
	return a >= b
}

func rank(v Value) int {
	switch v.(type) {
	case bool:
		return 2
	case string:
		return 1
	}
	return 0
}

// comparison is Excel's: numbers before text before booleans; an empty cell is 0 or "" by the other side.
func comparison(op string, a, b Value) bool {
	blank := func(other Value) Value {
		switch other.(type) {
		case string:
			return ""
		case bool:
			return false
		}
		return 0.0
	}
	if a == nil {
		a = blank(b)
	}
	if b == nil {
		b = blank(a)
	}
	if rank(a) != rank(b) {
		return compareFloats(op, float64(rank(a)), float64(rank(b)))
	}
	if x, ok := a.(string); ok {
		return compareStrings(op, pytext.Casefold(x), pytext.Casefold(b.(string)))
	}
	x, _ := ToNumber(a)
	y, _ := ToNumber(b)
	return compareFloats(op, x, y)
}

// Evaluate is the value of a tree, cells read through lookup. A *CellError for an error value.
func Evaluate(node *Node, lookup Lookup) (Value, error) {
	switch node.Kind {
	case "num":
		return node.Num, nil
	case "str":
		return node.Str, nil
	case "bool":
		return node.Bool, nil
	case "ref":
		return lookup(node.Tab, node.Col, node.Row)
	case "range":
		return nil, errValue // a range outside a function
	case "neg":
		v, err := Evaluate(node.Args[0], lookup)
		if err != nil {
			return nil, err
		}
		x, err := ToNumber(v)
		return -x, err
	case "bin":
		return binary(node, lookup)
	}
	return call(node.Str, node.Args, lookup)
}

func binary(node *Node, lookup Lookup) (Value, error) {
	op := node.Str
	a, err := Evaluate(node.Args[0], lookup)
	if err != nil {
		return nil, err
	}
	b, err := Evaluate(node.Args[1], lookup)
	if err != nil {
		return nil, err
	}
	switch op {
	case "&":
		return Str(a) + Str(b), nil
	case "=", "<>", "<", ">", "<=", ">=":
		return comparison(op, a, b), nil
	}
	x, err := ToNumber(a)
	if err != nil {
		return nil, err
	}
	y, err := ToNumber(b)
	if err != nil {
		return nil, err
	}
	switch op {
	case "+":
		return x + y, nil
	case "-":
		return x - y, nil
	case "*":
		return x * y, nil
	case "/":
		if y == 0 {
			return nil, &CellError{"#DIV/0!"}
		}
		return x / y, nil
	}
	result := math.Pow(x, y)
	if (math.IsInf(result, 0) && !math.IsInf(x, 0) && !math.IsInf(y, 0)) || (math.IsNaN(result) && !math.IsNaN(x) && !math.IsNaN(y)) {
		return nil, &CellError{"#NUM!"} // an overflow, 0 to a negative power, a negative base to a fraction
	}
	return result, nil
}

type item struct {
	cell bool // from a reference: text and blanks are skipped
	v    Value
}

// values are the values of the arguments, a range spread into its cells.
func values(args []*Node, lookup Lookup) ([]item, error) {
	var found []item
	for _, arg := range args {
		if arg.Kind == "ref" || arg.Kind == "range" {
			for _, c := range Cells(arg) {
				v, err := lookup(c.Tab, c.Col, c.Row)
				if err != nil {
					return nil, err
				}
				found = append(found, item{true, v})
			}
			continue
		}
		v, err := Evaluate(arg, lookup)
		if err != nil {
			return nil, err
		}
		found = append(found, item{false, v})
	}
	return found, nil
}

func numbers(args []*Node, lookup Lookup) ([]float64, error) {
	found, err := values(args, lookup)
	if err != nil {
		return nil, err
	}
	var out []float64
	for _, it := range found {
		if it.cell {
			if x, ok := it.v.(float64); ok {
				out = append(out, x)
			}
		} else if it.v != nil {
			x, err := ToNumber(it.v)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
	}
	return out, nil
}

func call(name string, args []*Node, lookup Lookup) (Value, error) {
	arg := func(i int, fallback Value) (Value, error) {
		if i < len(args) {
			return Evaluate(args[i], lookup)
		}
		return fallback, nil
	}
	switch name {
	case "IF":
		test, err := arg(0, nil)
		if err != nil {
			return nil, err
		}
		yes, err := toBool(test)
		if err != nil {
			return nil, err
		}
		if yes {
			return arg(1, true)
		}
		return arg(2, false)
	case "IFERROR":
		v, err := arg(0, nil)
		var cellErr *CellError
		if errors.As(err, &cellErr) {
			return arg(1, "")
		}
		return v, err
	case "SUM", "AVERAGE", "MIN", "MAX":
		found, err := numbers(args, lookup)
		if err != nil {
			return nil, err
		}
		switch name {
		case "SUM":
			return Sum(found), nil
		case "AVERAGE":
			if len(found) == 0 {
				return nil, &CellError{"#DIV/0!"}
			}
			return Sum(found) / float64(len(found)), nil
		}
		if len(found) == 0 {
			return 0.0, nil
		}
		best := found[0]
		for _, x := range found[1:] {
			if (name == "MIN" && x < best) || (name == "MAX" && x > best) {
				best = x
			}
		}
		return best, nil
	case "COUNT", "COUNTA":
		found, err := values(args, lookup)
		if err != nil {
			return nil, err
		}
		n := 0
		for _, it := range found {
			_, isBool := it.v.(bool)
			if name == "COUNT" && (IsNumber(it.v) || (!it.cell && isBool)) ||
				name == "COUNTA" && it.v != nil && it.v != "" {
				n++
			}
		}
		return float64(n), nil
	case "ROUND", "ROUNDUP", "ROUNDDOWN":
		first, err := arg(0, nil)
		if err != nil {
			return nil, err
		}
		x, err := ToNumber(first)
		if err != nil {
			return nil, err
		}
		second, err := arg(1, 0.0)
		if err != nil {
			return nil, err
		}
		d, err := ToNumber(second)
		if err != nil {
			return nil, err
		}
		if math.IsInf(d, 0) || math.IsNaN(d) {
			return nil, &ArithError{"cannot convert float infinity to integer"}
		}
		digits := int(max(-15, min(15, math.Trunc(d))))
		if name == "ROUND" {
			return roundHalfUp(x, digits)
		}
		q := math.Pow(10, float64(digits))
		scaled := math.Abs(x) * q
		if math.IsInf(scaled, 0) || math.IsNaN(scaled) {
			return nil, &ArithError{"cannot convert float infinity to integer"}
		}
		if name == "ROUNDUP" {
			scaled = math.Ceil(scaled - 1e-9)
		} else {
			scaled = math.Floor(scaled + 1e-9)
		}
		return math.Copysign(scaled/q, x), nil
	case "AND", "OR":
		found, err := values(args, lookup)
		if err != nil {
			return nil, err
		}
		var flags []bool
		for _, it := range found {
			if it.cell && it.v == nil {
				continue
			}
			b, err := toBool(it.v)
			if err != nil {
				return nil, err
			}
			flags = append(flags, b)
		}
		result := name == "AND"
		for _, b := range flags {
			if name == "AND" && !b {
				result = false
			}
			if name == "OR" && b {
				result = true
			}
		}
		return result, nil
	case "NOT":
		v, err := arg(0, nil)
		if err != nil {
			return nil, err
		}
		b, err := toBool(v)
		return !b, err
	case "ISNUMBER", "ISBLANK", "ABS":
		v, err := arg(0, nil)
		if err != nil {
			return nil, err
		}
		switch name {
		case "ISNUMBER":
			return IsNumber(v), nil
		case "ISBLANK":
			return v == nil, nil
		}
		x, err := ToNumber(v)
		return math.Abs(x), err
	case "SUMIF":
		return sumif(args, lookup)
	}
	return nil, unsupported("the function %s", name)
}

func sumif(args []*Node, lookup Lookup) (Value, error) {
	if len(args) < 2 || (args[0].Kind != "ref" && args[0].Kind != "range") {
		return nil, errValue
	}
	spec, err := Evaluate(args[1], lookup)
	if err != nil {
		return nil, err
	}
	test, err := criterion(spec)
	if err != nil {
		return nil, err
	}
	where := Cells(args[0])
	add := where
	if len(args) > 2 && (args[2].Kind == "ref" || args[2].Kind == "range") {
		add = Cells(args[2])
	}
	if len(args) > 2 && len(add) != len(where) { // Excel takes the sum range's corner and the criteria's shape
		first := add[0]
		c0, r0 := ColIndex(first.Col), first.Row
		cw, rw := ColIndex(where[0].Col), where[0].Row
		shaped := make([]Cell, len(where))
		for i, c := range where {
			shaped[i] = Cell{first.Tab, ColName(c0 + ColIndex(c.Col) - cw), r0 + c.Row - rw}
		}
		add = shaped
	}
	var total []float64
	for i, c := range where {
		v, err := lookup(c.Tab, c.Col, c.Row)
		if err != nil {
			return nil, err
		}
		if test(v) {
			s, err := lookup(add[i].Tab, add[i].Col, add[i].Row)
			if err != nil {
				return nil, err
			}
			if x, ok := s.(float64); ok {
				total = append(total, x)
			}
		}
	}
	result := 0.0
	for _, x := range total { // total += x, one by one, as the Python loop adds
		result += x
	}
	return result, nil
}

// --- written out ---------------------------------------------------------------------------------------------

var precedence = map[string]int{"=": 0, "<>": 0, "<": 0, ">": 0, "<=": 0, ">=": 0, "&": 1, "+": 2, "-": 2, "*": 3,
	"/": 3, "^": 4}

var signs = map[string]string{"*": "·", "/": " / ", "-": " − ", "+": " + ", "<>": " ≠ ", "<=": " ≤ ", ">=": " ≥ "}

// Render writes the formula with names for its cells (name of a ref or a range): `0.35·КР1 + 0.35·КР2 + 0.3·ДЗ`.
func Render(node *Node, name func(*Node) string) string { return render(node, name, -1) }

func render(node *Node, name func(*Node) string, parent int) string {
	switch node.Kind {
	case "num":
		return NumberText(node.Num)
	case "str":
		return "«" + node.Str + "»"
	case "bool":
		if node.Bool {
			return "TRUE"
		}
		return "FALSE"
	case "ref", "range":
		return name(node)
	case "neg":
		return "−" + render(node.Args[0], name, 5)
	case "bin":
		op := node.Str
		level := precedence[op]
		right := level
		if op == "-" || op == "/" || op == "^" {
			right = level + 1
		}
		sign, ok := signs[op]
		if !ok {
			sign = " " + op + " "
		}
		text := render(node.Args[0], name, level) + sign + render(node.Args[1], name, right)
		if level < parent {
			return "(" + text + ")"
		}
		return text
	}
	parts := make([]string, len(node.Args))
	for i, arg := range node.Args {
		parts[i] = render(arg, name, -1)
	}
	return node.Str + "(" + strings.Join(parts, "; ") + ")"
}
