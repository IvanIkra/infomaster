package tools

import (
	"strings"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/httpx"
)

func decode(t *testing.T, text string) any {
	t.Helper()
	value, err := httpx.Decode([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestCheck(t *testing.T) {
	schema := decode(t, `{"type": "object", "properties": {
		"title": {"type": "string", "minLength": 1, "maxLength": 3},
		"n": {"type": "integer", "minimum": 1, "maximum": 5},
		"share": {"type": "number", "minimum": 0.5, "maximum": 10.0},
		"kind": {"enum": ["a", 1, null]},
		"ids": {"type": "array", "items": {"type": "integer"}, "minItems": 1, "maxItems": 1},
		"maybe": {"type": ["string", "null"]}},
		"required": ["title"], "additionalProperties": false}`)
	for args, want := range map[string]string{
		`{"title": "яяя"}`:                   "",
		`{"title": "яяяя"}`:                  "arguments.title: at most 3 characters",
		`{"title": ""}`:                      "arguments.title: at least 1 characters",
		`{}`:                                 "arguments.title: required",
		`{"title": "a", "n": 1.0}`:           "arguments.n: expected integer",
		`{"title": "a", "n": true}`:          "arguments.n: expected integer",
		`{"title": "a", "n": 9}`:             "arguments.n: at most 5",
		`{"title": "a", "share": 0.25}`:      "arguments.share: at least 0.5",
		`{"title": "a", "share": 1}`:         "",
		`{"title": "a", "kind": 1.0}`:        "",
		`{"title": "a", "kind": true}`:       "",
		`{"title": "a", "kind": "b"}`:        `arguments.kind: one of "a", 1, null`,
		`{"title": "a", "ids": []}`:          "", // minItems is not enforced, as in Python
		`{"title": "a", "ids": [1, "x", 2]}`: "arguments.ids[1]: expected integer",
		`{"title": "a", "maybe": 3}`:         "arguments.maybe: expected string or null",
		`{"title": "a", "maybe": null}`:      "",
		`{"title": "a", "zz": 1, "aa": 2}`:   "arguments.aa: unknown; arguments.zz: unknown",
		`[]`:                                 "arguments: expected object",
	} {
		if got := Join(Check(schema, decode(t, args), "arguments")); got != want {
			t.Errorf("%s: %q, want %q", args, got, want)
		}
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	run := func(*Context, map[string]any) (Output, error) { return Result(nil), nil }
	if err := r.Add(Tool{Name: "a", Schema: Schema(nil), Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Tool{Name: "a", Schema: Schema(nil), Run: run}); err == nil ||
		!strings.Contains(err.Error(), "registered twice") {
		t.Fatal(err)
	}
	if err := r.Step(&Context{}, Step{Tool: "_none"}); err == nil || err.Error() != "unknown undo step _none" {
		t.Fatal(err)
	}
	called := false
	r.Op("_op", func(_ *Context, args map[string]any) (Output, error) { called = args != nil; return Output{}, nil })
	if err := r.Step(&Context{}, Step{Tool: "_op"}); err != nil || !called {
		t.Fatal(err)
	}
	if text, _ := Dumps(Schema(map[string]any{"x": map[string]any{"type": "string"}}, "x")); text !=
		`{"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x"], "additionalProperties": false}` {
		t.Fatal(text)
	}
}
