package grades

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	fx "github.com/IvanIkra/infomaster/go/internal/processing/formulas"
)

// A shared formula is written once, in its first cell; every other cell of it has only its index. openpyxl moves
// the first cell's formula to each of them (its Translator over its Tokenizer), and so does this: the references
// without "$" shift by the offset, whitespace runs shrink to their first character, everything else stays as
// written.

var (
	errTranslate = errors.New("formula out of range")
	rowRange     = regexp.MustCompile(`^(\$?[1-9][0-9]{0,6}):(\$?[1-9][0-9]{0,6})$`)
	colRange     = regexp.MustCompile(`^(\$?[A-Za-z]{1,3}):(\$?[A-Za-z]{1,3})$`)
	cellRef      = regexp.MustCompile(`^(\$?[A-Za-z]{1,3})(\$?[1-9][0-9]{0,6})$`)
	scientific   = regexp.MustCompile(`^[1-9](\.[0-9]+)?[Ee]$`)
	errorCodes   = []string{"#NULL!", "#DIV/0!", "#VALUE!", "#REF!", "#NAME?", "#NUM!", "#N/A", "#GETTING_DATA"}
	tokenEnders  = ",;}) +-*/^&=><%"
)

type piece struct {
	text  string
	moves bool // an operand that is a range: shifted
}

type translator struct {
	row, col int
	pieces   []piece
	literal  string // a formula without "=": kept as it is
	empty    bool
}

func coordinate(text string) (row, col int, err error) {
	m := refRe.FindStringSubmatch(text)
	if m == nil {
		return 0, 0, errors.New("invalid cell coordinates (" + text + ")")
	}
	row, _ = strconv.Atoi(m[2])
	return row, fx.ColIndex(strings.ToUpper(m[1])) + 1, nil
}

func newTranslator(formula, origin string) (*translator, error) {
	row, col, err := coordinate(origin)
	if err != nil {
		return nil, err
	}
	t := &translator{row: row, col: col}
	if formula == "" {
		t.empty = true
		return t, nil
	}
	if !strings.HasPrefix(formula, "=") {
		t.literal = formula
		return t, nil
	}
	s := []rune(formula[1:])
	var token []string
	save := func() {
		if len(token) == 0 {
			return
		}
		value := strings.Join(token, "")
		token = nil
		t.pieces = append(t.pieces, piece{value, isRange(value)})
	}
	for i := 0; i < len(s); {
		c := s[i]
		if (c == '+' || c == '-') && len(token) > 0 && scientific.MatchString(strings.Join(token, "")) {
			token = append(token, string(c))
			i++
			continue
		}
		if strings.ContainsRune(tokenEnders, c) {
			save()
		}
		switch {
		case c == '"' || c == '\'':
			if len(token) > 0 && token[len(token)-1] != ":" {
				return nil, errors.New("unexpected character in a formula")
			}
			end := quotedEnd(s, i, c)
			if end < 0 {
				return nil, errors.New("reached end of formula while parsing a string")
			}
			if c == '"' {
				t.pieces = append(t.pieces, piece{string(s[i:end]), false})
			} else {
				token = append(token, string(s[i:end]))
			}
			i = end
		case c == '[':
			depth, j := 0, i
			for ; j < len(s); j++ {
				if s[j] == '[' {
					depth++
				} else if s[j] == ']' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if j >= len(s) {
				return nil, errors.New("unmatched '[' in a formula")
			}
			token = append(token, string(s[i:j+1]))
			i = j + 1
		case c == '#':
			if len(token) > 0 && token[len(token)-1] != "!" {
				return nil, errors.New("unexpected character in a formula")
			}
			rest, found := string(s[i:]), ""
			for _, code := range errorCodes {
				if strings.HasPrefix(rest, code) {
					found = code
					break
				}
			}
			if found == "" {
				return nil, errors.New("invalid error code in a formula")
			}
			t.pieces = append(t.pieces, piece{strings.Join(token, "") + found, false})
			token = nil
			i += len([]rune(found))
		case c == ' ' || c == '\n':
			t.pieces = append(t.pieces, piece{string(c), false})
			for i < len(s) && (s[i] == ' ' || s[i] == '\n') {
				i++
			}
		case strings.ContainsRune("+-*/^&=><%", c):
			if i+1 < len(s) {
				if two := string(s[i : i+2]); two == ">=" || two == "<=" || two == "<>" {
					t.pieces = append(t.pieces, piece{two, false})
					i += 2
					continue
				}
			}
			t.pieces = append(t.pieces, piece{string(c), false})
			i++
		case c == '(' || c == '{':
			if c == '(' && len(token) > 0 {
				t.pieces = append(t.pieces, piece{strings.Join(token, "") + "(", false})
				token = nil
			} else {
				t.pieces = append(t.pieces, piece{string(c), false})
			}
			i++
		case strings.ContainsRune(")};,", c):
			t.pieces = append(t.pieces, piece{string(c), false})
			i++
		default:
			token = append(token, string(c))
			i++
		}
	}
	save()
	return t, nil
}

// quotedEnd matches `"(?:[^"]*"")*[^"]*"(?!")` at s[i]: the end, or -1.
func quotedEnd(s []rune, i int, q rune) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			j++
			continue
		}
		return j + 1
	}
	return -1
}

// isRange is make_operand's test: no text, error, logical or number.
func isRange(value string) bool {
	if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "#") || value == "TRUE" || value == "FALSE" {
		return false
	}
	_, number := fx.PyFloat(value)
	return !number
}

func (t *translator) translate(dest string) (string, error) {
	if t.empty {
		return "", nil
	}
	if t.literal != "" {
		return t.literal, nil
	}
	row, col, err := coordinate(dest)
	if err != nil {
		return "", err
	}
	out := []string{"="}
	for _, p := range t.pieces {
		if !p.moves {
			out = append(out, p.text)
			continue
		}
		moved, err := translateRange(p.text, row-t.row, col-t.col)
		if err != nil {
			return "", err
		}
		out = append(out, moved)
	}
	return strings.Join(out, ""), nil
}

func translateRow(text string, delta int) (string, error) {
	if strings.HasPrefix(text, "$") {
		return text, nil
	}
	n, _ := strconv.Atoi(text)
	if n+delta <= 0 {
		return "", errTranslate
	}
	return strconv.Itoa(n + delta), nil
}

func translateCol(text string, delta int) (string, error) {
	if strings.HasPrefix(text, "$") {
		return text, nil
	}
	index := fx.ColIndex(strings.ToUpper(text)) + 1 + delta
	if index < 1 || index > 18278 {
		return "", errTranslate
	}
	return fx.ColName(index - 1), nil
}

func translateRange(text string, rows, cols int) (string, error) {
	sheet := ""
	if i := strings.LastIndex(text, "!"); i >= 0 {
		sheet, text = text[:i+1], text[i+1:]
	}
	if m := rowRange.FindStringSubmatch(text); m != nil {
		a, err := translateRow(m[1], rows)
		if err != nil {
			return "", err
		}
		b, err := translateRow(m[2], rows)
		return sheet + a + ":" + b, err
	}
	if m := colRange.FindStringSubmatch(text); m != nil {
		a, err := translateCol(m[1], cols)
		if err != nil {
			return "", err
		}
		b, err := translateCol(m[2], cols)
		return sheet + a + ":" + b, err
	}
	if strings.Contains(text, ":") {
		parts := strings.Split(text, ":")
		for i, part := range parts {
			moved, err := translateRange(part, rows, cols)
			if err != nil {
				return "", err
			}
			parts[i] = moved
		}
		return sheet + strings.Join(parts, ":"), nil
	}
	m := cellRef.FindStringSubmatch(text)
	if m == nil { // a named range
		return text, nil
	}
	c, err := translateCol(m[1], cols)
	if err != nil {
		return "", err
	}
	r, err := translateRow(m[2], rows)
	return sheet + c + r, err
}
