package actions

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
)

// PyStr is Python's str() of a value: None, True, 8.0, a text as it is, and the repr of a list or a dict
// (['a', 'b'], {'k': 1}). The summaries of the journal write values this way.
func PyStr(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return pyRepr(value)
}

func pyRepr(value any) string {
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
	case float64:
		return pyjson.FormatFloat(v)
	case float32:
		return pyjson.FormatFloat(float64(v))
	case pyjson.Float:
		return pyjson.FormatFloat(float64(v))
	case i18n.Phrase:
		return strRepr(v.En)
	case httpx.Object:
		parts := make([]string, len(v))
		for i, f := range v {
			parts[i] = strRepr(f.Key) + ": " + pyRepr(f.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8,
		reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fmt.Sprint(value)
	case reflect.Pointer:
		if rv.IsNil() {
			return "None"
		}
		return pyRepr(rv.Elem().Interface())
	case reflect.Slice, reflect.Array:
		parts := make([]string, rv.Len())
		for i := range parts {
			parts[i] = pyRepr(rv.Index(i).Interface())
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Map:
		keys := rv.MapKeys()
		parts := []string{}
		for _, k := range keys {
			parts = append(parts, pyRepr(k.Interface())+": "+pyRepr(rv.MapIndex(k).Interface()))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	if o, ok := httpx.Localize(value, "en").(httpx.Object); ok {
		return pyRepr(o)
	}
	return fmt.Sprint(value)
}

// strRepr is Python's repr of a text: single quotes unless the text has one and no double quote.
func strRepr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == quote:
			b.WriteString(`\` + quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

func pyStr(value any) string { return PyStr(value) }
