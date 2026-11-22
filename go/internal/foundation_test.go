package internal_test

import (
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/store"
	"github.com/IvanIkra/infomaster/go/internal/tgid"
)

func TestPyJSONMatchesPython(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{[]int{1, 2, 3}, "[1, 2, 3]"},
		{map[string]any{"a": "Привет <b>", "b": []any{true, nil}}, `{"a": "Привет <b>", "b": [true, null]}`},
		{"a,b:c", `"a,b:c"`},
		{"quote \" and \\ slash", `"quote \" and \\ slash"`},
		{"line sep", "\"line sep\""},
	}
	for _, c := range cases {
		got, err := pyjson.Dumps(c.value)
		if err != nil || got != c.want {
			t.Errorf("Dumps(%v) = %q, %v; want %q", c.value, got, err, c.want)
		}
	}
	if got, _ := pyjson.DumpsASCII("Ж😀"); got != "\"\\u0416\\ud83d\\ude00\"" {
		t.Errorf("DumpsASCII = %q", got)
	}
	if got, _ := pyjson.Compact(map[string]int{"x": 1, "y": 2}); got != `{"x":1,"y":2}` {
		t.Errorf("Compact = %s", got)
	}
}

func TestClockFormats(t *testing.T) {
	moment := time.Date(2026, 11, 22, 21, 30, 5, 123456789, time.UTC)
	if got := clock.UTCISO(moment); got != "2026-11-22T21:30:05+00:00" {
		t.Errorf("UTCISO = %s", got)
	}
	if got := clock.LocalISO(moment); got != "2026-11-23T00:30:05" {
		t.Errorf("LocalISO = %s", got)
	}
	if got := clock.LocalDay(moment).Format(clock.DateLayout); got != "2026-11-23" {
		t.Errorf("LocalDay = %s", got)
	}
	if got := clock.UTCISO(clock.DayStartUTC(moment)); got != "2026-11-22T21:00:00+00:00" {
		t.Errorf("DayStartUTC = %s", got)
	}
	for _, text := range []string{"2026-11-23T00:30:05", "2026-11-23T00:30", "2026-11-23T00:30:05.5"} {
		if _, err := clock.ParseLocal(text); err != nil {
			t.Errorf("ParseLocal(%s): %v", text, err)
		}
	}
}

func TestStoreSchemaAndKV(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutJSON("k", map[string]any{"ids": []int{1, 2}}); err != nil {
		t.Fatal(err)
	}
	var raw string
	s.DB.QueryRow("select value from kv where key = 'k'").Scan(&raw)
	if raw != `{"ids": [1, 2]}` {
		t.Errorf("stored %q", raw)
	}
	var got map[string]any
	if ok, err := s.GetJSON("k", &got); !ok || err != nil || len(got["ids"].([]any)) != 2 {
		t.Errorf("GetJSON = %v %v %v", got, ok, err)
	}
	if ok, _ := s.GetJSON("missing", &got); ok {
		t.Error("missing key found")
	}
	var n int
	if err := s.DB.QueryRow("select count(*) from pragma_table_info('sheets') where name = 'caption'").Scan(&n); err != nil || n != 1 {
		t.Errorf("upgrade column missing: %d %v", n, err)
	}
}

func TestMarkedIDs(t *testing.T) {
	if tgid.Marked(tgid.Channel, 1234567890) != -1001234567890 || tgid.Marked(tgid.Chat, 42) != -42 {
		t.Error("marked ids")
	}
	if k, id := tgid.Resolve(-1001234567890); k != tgid.Channel || id != 1234567890 {
		t.Error("resolve channel")
	}
	if k, id := tgid.Resolve(-42); k != tgid.Chat || id != 42 {
		t.Error("resolve chat")
	}
}

func TestConfig(t *testing.T) {
	vars := map[string]string{"TG_API_ID": "1", "TG_API_HASH": "h", "TG_BOT_TOKEN": "t", "TG_OWNER_ID": "2",
		"API_TOKEN": "a", "PUBLIC_URL": "https://example.test/"}
	env, err := config.FromEnviron(func(k string) string { return vars[k] })
	if err != nil || env.PublicURL != "https://example.test" || env.PublicPort != 8080 || env.APIID != 1 {
		t.Fatalf("%+v %v", env, err)
	}
	vars["CALENDAR_SECRET"] = "short"
	if _, err := config.FromEnviron(func(k string) string { return vars[k] }); err == nil {
		t.Error("short secret accepted")
	}
	delete(vars, "CALENDAR_SECRET")
	vars["TG_API_ID"] = "x"
	if _, err := config.FromEnviron(func(k string) string { return vars[k] }); err == nil {
		t.Error("bad integer accepted")
	}
}

func TestPythonFloats(t *testing.T) {
	for f, want := range map[float64]string{8: "8.0", 0.5: "0.5", 1e16: "1e+16", 0.00001: "1e-05", 0: "0.0", -2.25: "-2.25"} {
		if got := pyjson.FormatFloat(f); got != want {
			t.Errorf("FormatFloat(%v) = %s, want %s", f, got, want)
		}
	}
	if got, _ := pyjson.Dumps(map[string]pyjson.Float{"v": 7}); got != `{"v": 7.0}` {
		t.Errorf("Dumps = %s", got)
	}
}
