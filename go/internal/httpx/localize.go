package httpx

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/i18n"
)

var (
	phraseType    = reflect.TypeOf(i18n.Phrase{})
	marshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	texterType    = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// Localize is json_default(lang): the value with every i18n.Phrase written in the language. It walks maps, slices,
// pointers and structs (a struct becomes an Object of its JSON fields, in order, honouring the json tags' names,
// "-" and omitempty); a value with its own MarshalJSON or MarshalText (pyjson.Float, time.Time) is kept as it is.
// i18n.Localize walks only maps and slices of any; this one also reaches a Phrase held in a struct field.
func Localize(value any, lang string) any {
	if value == nil {
		return nil
	}
	return localize(reflect.ValueOf(value), lang)
}

func localize(v reflect.Value, lang string) any {
	if !v.IsValid() {
		return nil
	}
	t := v.Type()
	if t == phraseType {
		return v.Interface().(i18n.Phrase).Get(lang)
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		if t.Kind() == reflect.Pointer && t.Elem() == phraseType {
			return v.Elem().Interface().(i18n.Phrase).Get(lang)
		}
		if t.Kind() == reflect.Pointer && (t.Implements(marshalerType) || t.Implements(texterType)) {
			return v.Interface() // writes itself (*pyjson.Float, a pointer-receiver MarshalJSON)
		}
		return localize(v.Elem(), lang)
	}
	if t.Implements(marshalerType) || t.Implements(texterType) {
		if o, ok := v.Interface().(Object); ok {
			out := make(Object, len(o))
			for i, field := range o {
				out[i] = Field{field.Key, Localize(field.Value, lang)}
			}
			return out
		}
		return v.Interface()
	}
	switch t.Kind() {
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		out := make(map[string]any, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out[mapKey(iter.Key())] = localize(iter.Value(), lang)
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		if t.Elem().Kind() == reflect.Uint8 {
			return v.Interface() // []byte: base64, as encoding/json writes it
		}
		fallthrough
	case reflect.Array:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = localize(v.Index(i), lang)
		}
		return out
	case reflect.Struct:
		out := Object{}
		addFields(&out, v, lang)
		return out
	default:
		return v.Interface()
	}
}

func mapKey(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	if texter, ok := k.Interface().(encoding.TextMarshaler); ok {
		if text, err := texter.MarshalText(); err == nil {
			return string(text)
		}
	}
	return fmt.Sprint(k.Interface())
}

func addFields(out *Object, v reflect.Value, lang string) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		value := v.Field(i)
		if field.Anonymous && name == "" {
			inner := value
			if inner.Kind() == reflect.Pointer {
				if inner.IsNil() {
					continue
				}
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				addFields(out, inner, lang)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if strings.Contains(","+options+",", ",omitempty,") && empty(value) {
			continue
		}
		*out = append(*out, Field{name, localize(value, lang)})
	}
}

func empty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32,
		reflect.Float64, reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}
