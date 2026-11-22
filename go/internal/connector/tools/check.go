package tools

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/pyjson"
)

// Check is the problems of `value` against the subset of JSON Schema the tools use: type (one or a list), enum,
// properties, required, additionalProperties: false, items, minimum, maximum, minLength and maxLength (in code
// points). minItems and maxItems are not enforced, as in the Python service. The schema and the value are JSON
// decoded by httpx.Decode (integers int64, other numbers float64), so 1.0 is no integer, as in Python.
//
// Python walks an object's keys in the order the client sent them; a Go map has none, so the problems of several
// keys come in the keys' sorted order.
func Check(schema, value any, where string) []string {
	spec, _ := schema.(map[string]any)
	if spec == nil {
		return nil
	}
	if kinds, ok := spec["type"]; ok && kinds != nil {
		var names []string
		switch k := kinds.(type) {
		case string:
			names = []string{k}
		case []any:
			for _, item := range k {
				if name, ok := item.(string); ok {
					names = append(names, name)
				}
			}
		}
		matched := false
		for _, kind := range names {
			if is(value, kind) {
				matched = true
				break
			}
		}
		if !matched {
			return []string{fmt.Sprintf("%s: expected %s", where, strings.Join(names, " or "))}
		}
	}
	if enum, ok := spec["enum"]; ok {
		choices, _ := enum.([]any)
		found := false
		for _, choice := range choices {
			if pyEqual(value, choice) {
				found = true
				break
			}
		}
		if !found {
			texts := make([]string, len(choices))
			for i, choice := range choices {
				texts[i], _ = pyjson.DumpsASCII(pyValue(choice))
			}
			return []string{fmt.Sprintf("%s: one of %s", where, strings.Join(texts, ", "))}
		}
	}
	var problems []string
	if object, ok := value.(map[string]any); ok {
		if properties, ok := spec["properties"].(map[string]any); ok {
			required, _ := spec["required"].([]any)
			for _, name := range required {
				if key, ok := name.(string); ok {
					if _, present := object[key]; !present {
						problems = append(problems, fmt.Sprintf("%s.%s: required", where, key))
					}
				}
			}
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if closed, ok := spec["additionalProperties"].(bool); ok && !closed {
				for _, key := range keys {
					if _, known := properties[key]; !known {
						problems = append(problems, fmt.Sprintf("%s.%s: unknown", where, key))
					}
				}
			}
			for _, key := range keys {
				if property, known := properties[key]; known {
					problems = append(problems, Check(property, object[key], where+"."+key)...)
				}
			}
		}
	}
	if list, ok := value.([]any); ok {
		if items, ok := spec["items"]; ok {
			for i, item := range list {
				problems = append(problems, Check(items, item, fmt.Sprintf("%s[%d]", where, i))...)
			}
		}
	}
	if number, ok := numeric(value); ok {
		if limit, ok := spec["minimum"]; ok {
			if bound, ok := numeric(limit); ok && number < bound {
				problems = append(problems, fmt.Sprintf("%s: at least %s", where, pyStr(limit)))
			}
		}
		if limit, ok := spec["maximum"]; ok {
			if bound, ok := numeric(limit); ok && number > bound {
				problems = append(problems, fmt.Sprintf("%s: at most %s", where, pyStr(limit)))
			}
		}
	}
	if text, ok := value.(string); ok {
		length := utf8.RuneCountInString(text)
		if limit, ok := spec["minLength"]; ok {
			if bound, ok := numeric(limit); ok && float64(length) < bound {
				problems = append(problems, fmt.Sprintf("%s: at least %s characters", where, pyStr(limit)))
			}
		}
		if limit, ok := spec["maxLength"]; ok {
			if bound, ok := numeric(limit); ok && float64(length) > bound {
				problems = append(problems, fmt.Sprintf("%s: at most %s characters", where, pyStr(limit)))
			}
		}
	}
	return problems
}

// is is Python's _is: bool is never an integer or a number.
func is(value any, kind string) bool {
	switch kind {
	case "integer":
		_, ok := value.(int64)
		return ok
	case "number":
		_, ok := numeric(value)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "null":
		return value == nil
	}
	return false
}

func numeric(value any) (float64, bool) {
	switch v := value.(type) {
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case pyjson.Float:
		return float64(v), true
	case int:
		return float64(v), true
	}
	return 0, false
}

// pyEqual is Python's ==: 1 == 1.0 == True, deep for lists and dicts.
func pyEqual(a, b any) bool {
	asNumber := func(v any) (float64, bool) {
		if flag, ok := v.(bool); ok {
			if flag {
				return 1, true
			}
			return 0, true
		}
		return numeric(v)
	}
	if x, ok := asNumber(a); ok {
		y, ok := asNumber(b)
		return ok && x == y
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for key, item := range x {
			other, ok := y[key]
			if !ok || !pyEqual(item, other) {
				return false
			}
		}
		return true
	}
	return false
}

func pyValue(value any) any {
	if f, ok := value.(float64); ok {
		return pyjson.Float(f)
	}
	return value
}

// pyStr is Python's str() of a decoded JSON value, for the texts of the problems and the errors.
func pyStr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case int64:
		return fmt.Sprint(v)
	case float64:
		return pyjson.FormatFloat(v)
	case string:
		return v
	}
	text, _ := pyjson.Dumps(value)
	return text
}

// PyStr is Python's str() of a decoded JSON value: None, True, 8.0, a string as it is.
func PyStr(value any) string { return pyStr(value) }
