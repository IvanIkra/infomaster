package settings

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// Float is pyjson.Float: a float that JSON writes as Python does (1.0, 0.5, 1e-05).
type Float = pyjson.Float

// Round is Python's round(value, digits): the nearest decimal of the exact binary value, halves to even.
func Round(value float64, digits int) float64 {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', digits, 64), 64)
	if err != nil {
		return value
	}
	return rounded
}

// integer is a whole number that is not a bool: ints, whole floats (encoding/json decodes every number as float64)
// and json.Number without a fraction or exponent (a decoder with UseNumber keeps Python's int and float apart).
func integer(value any) (int64, bool) {
	switch v := value.(type) {
	case bool:
		return 0, false
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		return int64(v), true
	case float32:
		return wholeFloat(float64(v))
	case float64:
		return wholeFloat(v)
	case Float:
		return wholeFloat(float64(v))
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			return 0, false
		}
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}

func wholeFloat(v float64) (int64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || math.Abs(v) > 1<<53 {
		return 0, false
	}
	return int64(v), true
}

// number is any number that is not a bool, as a float.
func number(value any) (float64, bool) {
	switch v := value.(type) {
	case bool:
		return 0, false
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case Float:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	if n, ok := integer(value); ok {
		return float64(n), true
	}
	return 0, false
}

// repr is Python's repr() of a JSON-like value, for the error messages ({value!r}).
func repr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return strRepr(v)
	case json.Number:
		return string(v)
	case float32:
		return floatRepr(float64(v))
	case float64:
		return floatRepr(v)
	case Float:
		return floatRepr(float64(v))
	}
	if n, ok := integer(value); ok {
		return strconv.FormatInt(n, 10)
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return "None"
		}
		return repr(rv.Elem().Interface())
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return "[]"
		}
		items := make([]string, rv.Len())
		for i := range items {
			items[i] = repr(rv.Index(i).Interface())
		}
		return "[" + strings.Join(items, ", ") + "]"
	case reflect.Map:
		keys := rv.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		items := make([]string, len(keys))
		for i, k := range keys {
			items[i] = repr(k.Interface()) + ": " + repr(rv.MapIndex(k).Interface())
		}
		return "{" + strings.Join(items, ", ") + "}"
	}
	return fmt.Sprint(value)
}

func floatRepr(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	return pyjson.FormatFloat(v)
}

// strRepr is Python's repr() of a str: single quotes unless the text has one and no double quote.
func strRepr(text string) string {
	quote := byte('\'')
	if strings.Contains(text, "'") && !strings.Contains(text, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range text {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteByte(quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == utf8.RuneError || !unicode.IsPrint(r):
			switch {
			case r < 0x100:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// tupleRepr is Python's repr() of a tuple of ints: (1, 2, 3).
func tupleRepr(values []int) string {
	items := make([]string, len(values))
	for i, v := range values {
		items[i] = strconv.Itoa(v)
	}
	return "(" + strings.Join(items, ", ") + ")"
}

// squeeze is " ".join(text.split()).
func squeeze(text string) string { return strings.Join(pytext.Split(text), " ") }

// splitLines is Python's str.splitlines(): \n, \r, \r\n and the other line boundaries, no ends kept.
func splitLines(text string) []string {
	var lines []string
	start := 0
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, string(runes[start:i]))
			start = i + 1
		case '\r':
			lines = append(lines, string(runes[start:i]))
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(runes) {
		lines = append(lines, string(runes[start:]))
	}
	return lines
}
