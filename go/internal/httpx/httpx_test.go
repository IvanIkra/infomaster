package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
)

func TestRouter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /x", func(w http.ResponseWriter, _ *http.Request) { Text(w, 200, "x") })
	h := Router(mux)
	for _, c := range []struct{ method, path, body, allow string }{
		{"GET", "/x", "x", ""}, {"GET", "/y", "404: Not Found", ""}, {"POST", "/x", "405: Method Not Allowed", "GET, HEAD"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		body, _ := io.ReadAll(w.Body)
		if string(body) != c.body || w.Header().Get("Allow") != c.allow ||
			w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatal(c, string(body), w.Header())
		}
	}
}

type status struct {
	OkAt   *string      `json:"ok_at"`
	Error  *i18n.Phrase `json:"error"`
	Hidden string       `json:"-"`
	Empty  string       `json:"empty,omitempty"`
	Share  pyjson.Float `json:"share"`
	inner
}

type inner struct {
	Items []i18n.Phrase `json:"items"`
}

func TestJSONLang(t *testing.T) {
	p := i18n.P("down", "не работает")
	w := httptest.NewRecorder()
	JSONLang(w, 200, map[string]any{"s": status{Error: &p, Hidden: "x", Share: 8, inner: inner{[]i18n.Phrase{p}}}}, "ru")
	ru := "\\u043d\\u0435 \\u0440\\u0430\\u0431\\u043e\\u0442\\u0430\\u0435\\u0442" // ASCII-escaped, as json.dumps writes
	want := `{"s": {"ok_at": null, "error": "` + ru + `", "share": 8.0, "items": ["` + ru + `"]}}`
	if got := w.Body.String(); got != want || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("%s\n%s", got, want)
	}
	w = httptest.NewRecorder()
	JSON(w, 200, map[string]any{"a": 8.0, "b": []any{1e-07, int64(3)}, "c": "<&>"})
	if got := w.Body.String(); got != `{"a": 8.0, "b": [1e-07, 3], "c": "<&>"}` {
		t.Fatal(got)
	}
}

func TestBodies(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat(" ", 11)))
	if _, err := ReadJSON(r, 10); err == nil || err.Error() != "Maximum request body size 10 exceeded." {
		t.Fatal(err)
	}
	r = httptest.NewRequest("POST", "/", strings.NewReader(`{"a": 1, "b": 1.0} x`))
	if _, err := ReadJSON(r, 100); err != ErrNotJSON {
		t.Fatal(err)
	}
	value, _ := Decode([]byte(`{"a": 1, "b": 1.0, "c": 1e2, "d": 99999999999999999999}`))
	m := value.(map[string]any)
	if m["a"] != int64(1) || m["b"] != 1.0 || m["c"] != 100.0 || m["d"] != 1e20 {
		t.Fatal(m)
	}
	ordered, _ := DecodeOrdered([]byte(`{"z": 1, "a": {"y": 2.0, "b": [1]}, "z": 3}`))
	if text, _ := pyjson.Dumps(PyValue(ordered)); text != `{"z": 3, "a": {"y": 2.0, "b": [1]}}` {
		t.Fatal(text)
	}
	r = httptest.NewRequest("POST", "/?q=1", strings.NewReader("a=1&a=2"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if form, err := ReadForm(r, 100); err != nil || strings.Join(form["a"], ",") != "1,2" || form.Has("q") {
		t.Fatal(form, err)
	}
}
