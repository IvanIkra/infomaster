package settings

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newSettings(t *testing.T, s *store.Store) *Settings {
	t.Helper()
	settings, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func mustGet(t *testing.T, s *Settings, name string) any {
	t.Helper()
	v, err := s.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func stored(t *testing.T, s *store.Store, name string) string {
	t.Helper()
	var text string
	if err := s.DB.QueryRow("select value from settings where name = ?", name).Scan(&text); err != nil {
		t.Fatal(err)
	}
	return text
}

func isSettingsError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

func TestDefaults(t *testing.T) {
	s := newSettings(t, open(t))
	if v := mustGet(t, s, "llm_daily_limit_usd"); v != Float(0.5) {
		t.Fatalf("limit %v", v)
	}
	prices, err := s.Prices()
	if err != nil || prices["claude-haiku-5-5"].Output != 0.5 {
		t.Fatalf("prices %v %v", prices, err)
	}
}

func TestDefaultIsNotShared(t *testing.T) {
	s := newSettings(t, open(t))
	prices, _ := s.Prices()
	p := prices["claude-haiku-5-5"]
	p.Input = 99
	prices["claude-haiku-5-5"] = p
	again, _ := s.Prices()
	if again["claude-haiku-5-5"].Input != 0.1 {
		t.Fatal("the default was changed")
	}
	kinds, _ := s.Strings("push_kinds")
	kinds[0] = "x"
	if again, _ := s.Strings("push_kinds"); again[0] != "deadlines" {
		t.Fatal("the default list was changed")
	}
}

func TestSetReset(t *testing.T) {
	db := open(t)
	s := newSettings(t, db)
	v, err := s.Set("llm_daily_limit_usd", 1)
	if err != nil || v != Float(1) {
		t.Fatalf("%v %v", v, err)
	}
	if got := stored(t, db, "llm_daily_limit_usd"); got != "1.0" { // float(value), as Python stores it
		t.Fatalf("stored %q", got)
	}
	if f, _ := s.Float("llm_daily_limit_usd"); f != 1.0 {
		t.Fatal(f)
	}
	if err := s.Reset("llm_daily_limit_usd"); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.Float("llm_daily_limit_usd"); f != 0.5 {
		t.Fatal(f)
	}
}

func TestSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSettings(t, db).Set("llm_daily_limit_usd", 2); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if f, _ := newSettings(t, db).Float("llm_daily_limit_usd"); f != 2.0 {
		t.Fatal(f)
	}
}

func TestBadLimit(t *testing.T) {
	s := newSettings(t, open(t))
	for _, value := range []any{0, -1, "1", true, nil} {
		if _, err := s.Set("llm_daily_limit_usd", value); !isSettingsError(err) {
			t.Fatalf("%v: %v", value, err)
		}
	}
}

func TestBadPrices(t *testing.T) {
	s := newSettings(t, open(t))
	for _, value := range []any{
		map[string]any{},
		map[string]any{"m": map[string]any{"input": 1}},
		map[string]any{"m": map[string]any{"input": 1, "output": 1, "long_input": 1, "long_output": -1}},
	} {
		if _, err := s.Set("llm_prices", value); !isSettingsError(err) {
			t.Fatalf("%v: %v", value, err)
		}
	}
}

func TestPricesAreStoredInPythonsOrder(t *testing.T) {
	db := open(t)
	s := newSettings(t, db)
	if _, err := s.Set("llm_prices", map[string]any{"m": map[string]any{"long_output": 2, "input": 0.1,
		"output": 1, "long_input": 0.00001}}); err != nil {
		t.Fatal(err)
	}
	want := `{"m": {"input": 0.1, "output": 1.0, "long_input": 1e-05, "long_output": 2.0}}`
	if got := stored(t, db, "llm_prices"); got != want {
		t.Fatalf("stored %s", got)
	}
}

func TestUnknown(t *testing.T) {
	s := newSettings(t, open(t))
	if _, err := s.Get("nope"); !isSettingsError(err) || err.Error() != "unknown setting 'nope'" {
		t.Fatal(err)
	}
	if _, err := s.Set("nope", 1); !isSettingsError(err) {
		t.Fatal(err)
	}
}

func TestChatDefaults(t *testing.T) {
	chats := NewChats(open(t))
	all, err := chats.All(-1001)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(all)
	want := `{"enabled":false,"study":false,"filter":"all","people":[],"images":false,"pdf_content":false,` +
		`"urgent":false,"batch_hours":null,"note":""}`
	if string(text) != want {
		t.Fatal(string(text))
	}
	if ids, _ := chats.EnabledIDs(); len(ids) != 0 {
		t.Fatal(ids)
	}
}

func TestChatUpdate(t *testing.T) {
	chats := NewChats(open(t))
	all, err := chats.Update(-1001, map[string]any{"enabled": true, "batch_hours": 6})
	if err != nil || all.BatchHours == nil || *all.BatchHours != 6 {
		t.Fatalf("%+v %v", all, err)
	}
	chats.Update(-1002, map[string]any{"enabled": true})
	chats.Update(-1002, map[string]any{"enabled": false})
	if ids, _ := chats.EnabledIDs(); !reflect.DeepEqual(ids, map[int64]bool{-1001: true}) {
		t.Fatal(ids)
	}
	if all, _ := chats.Update(-1001, map[string]any{"batch_hours": nil}); all.BatchHours != nil {
		t.Fatal(*all.BatchHours)
	}
	if all, _ := chats.Update(-1001, map[string]any{"people": []any{3.0, 1.0, 3.0}}); !reflect.DeepEqual(all.People,
		[]int64{1, 3}) {
		t.Fatal(all.People)
	}
}

func TestBadChatSettings(t *testing.T) {
	for _, values := range []map[string]any{{"enabled": 1}, {"filter": "none"}, {"people": []any{1, "2"}},
		{"people": []any{true}}, {"batch_hours": 5}, {"batch_hours": true}, {"nope": 1}} {
		chats := NewChats(open(t))
		values["study"] = true
		if _, err := chats.Update(-1001, values); !isSettingsError(err) {
			t.Fatalf("%v: %v", values, err)
		}
		if study, _ := chats.Get(-1001, "study"); study != false { // checked before anything is saved
			t.Fatal("saved")
		}
	}
}

func TestLanguages(t *testing.T) {
	s := newSettings(t, open(t))
	if v := mustGet(t, s, "language"); v != "en" {
		t.Fatal(v)
	}
	if _, err := s.Set("forum_language", "ru"); !isSettingsError(err) {
		t.Fatal(err)
	}
	for _, wrong := range []any{"de", "RU", "", nil} {
		if _, err := s.Set("language", wrong); !isSettingsError(err) {
			t.Fatal(wrong)
		}
	}
	_, err := s.Set("language", "de")
	if err.Error() != "expected one of en, ru, got 'de'" {
		t.Fatal(err)
	}
}

func TestTextsForTheModels(t *testing.T) {
	s := newSettings(t, open(t))
	if v := mustGet(t, s, "about_me"); v != "" {
		t.Fatal(v)
	}
	if v, _ := s.Set("about_me", "  Group  3,\r\n\n subgroup 2 \n"); v != "Group 3,\nsubgroup 2" {
		t.Fatalf("%q", v)
	}
	if _, err := s.Set("about_me", strings.Repeat("x", 501)); !isSettingsError(err) {
		t.Fatal(err)
	}
	if _, err := s.Set("about_me", strings.Repeat("я", 500)); err != nil { // code points, not bytes
		t.Fatal(err)
	}
	if _, err := s.Set("about_me", nil); !isSettingsError(err) {
		t.Fatal(err)
	}
	chats := NewChats(open(t))
	if all, _ := chats.Update(-1001, map[string]any{"note": "Calc  chat"}); all.Note != "Calc chat" {
		t.Fatal(all.Note)
	}
	if _, err := chats.Update(-1001, map[string]any{"note": strings.Repeat("x", 301)}); !isSettingsError(err) {
		t.Fatal(err)
	}
}

func TestOldSummarySettingsAreLeftAlone(t *testing.T) {
	db := open(t)
	for _, q := range []string{
		`insert into settings(name, value) values('summary_time', '"21:00"')`,
		`insert into chat_settings(chat_id, name, value) values(-1001, 'summary', 'true')`,
		`insert into chat_settings(chat_id, name, value) values(-1001, 'enabled', 'true')`,
	} {
		if _, err := db.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	all, err := newSettings(t, db).All()
	if err != nil || all.Get("summary_time") != nil || len(all) != len(Names) {
		t.Fatal(all, err)
	}
	chats := NewChats(db)
	if c, _ := chats.All(-1001); !c.Enabled {
		t.Fatal(c)
	}
	if ids, _ := chats.EnabledIDs(); !reflect.DeepEqual(ids, map[int64]bool{-1001: true}) {
		t.Fatal(ids)
	}
	if _, err := newSettings(t, db).Get("summary_time"); !isSettingsError(err) {
		t.Fatal(err)
	}
}

func TestRenamedAndGoneSettingsMoveOnce(t *testing.T) {
	db := open(t)
	if _, err := db.DB.Exec(`insert into settings(name, value) values('miniapp_language', '"ru"'), ` +
		`('forum_language', '"ru"')`); err != nil {
		t.Fatal(err)
	}
	if v := mustGet(t, newSettings(t, db), "language"); v != "ru" {
		t.Fatal(v)
	}
	rows, _ := db.DB.Query("select name from settings")
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if !reflect.DeepEqual(names, []string{"language"}) {
		t.Fatal(names)
	}
	db.DB.Exec(`insert into settings(name, value) values('miniapp_language', '"en"')`)
	if v := mustGet(t, newSettings(t, db), "language"); v != "ru" {
		t.Fatal(v)
	}
}

func TestAllIsInOrderAndPythonJSON(t *testing.T) {
	all, err := newSettings(t, open(t)).All()
	if err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(all)
	if !strings.HasPrefix(string(text), `{"llm_daily_limit_usd":0.5,"llm_prices":{"claude-haiku-5-5":`+
		`{"input":0.1,"output":0.5,"long_input":0.5,"long_output":2.5}},"batch_hours":3,`) {
		t.Fatal(string(text))
	}
}

func TestChecksAndMessages(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  any
		err   string
	}{
		{"batch_hours", 5, nil, "expected one of (1, 2, 3, 4, 6, 8, 12, 24), got 5"},
		{"batch_hours", true, nil, "expected one of (1, 2, 3, 4, 6, 8, 12, 24), got True"},
		{"batch_hours", 6.0, 6, ""},
		{"news_ttl_hours", json.Number("3.0"), nil, "expected a whole number of hours from 1 to 168, got 3.0"},
		{"news_ttl_hours", json.Number("3"), 3, ""},
		{"pdf_pages", 51, nil, "expected a number of pages from 1 to 50, got 51"},
		{"reminder_eve", " 9:30 ", "09:30", ""},
		{"reminder_eve", "24:00", nil, "expected a time of day HH:MM, got '24:00'"},
		{"push_quiet", "23:00-8:00", "23:00-08:00", ""},
		{"push_quiet", "08:00-8:00", nil, "the quiet hours start and end at the same time"},
		{"push_quiet", "x", nil, "expected HH:MM-HH:MM or nothing, got 'x'"},
		{"push_kinds", []any{"llm", "deadlines"}, []string{"deadlines", "llm"}, ""},
		{"push_kinds", []any{"news"}, nil, "expected some of deadlines, reminders, failures, llm"},
		{"grades_student", "  Петров   Пётр ", "Петров Пётр", ""},
		{"hseapp_enabled", "yes", nil, "expected true or false, got 'yes'"},
		{"wiki_pages", []any{"https://wiki.example.org/Course", " https://wiki.example.org/Course "},
			[]string{"https://wiki.example.org/Course"}, ""},
		{"wiki_pages", []any{"ftp://x/y"}, nil, "not a wiki page's address: 'ftp://x/y'"},
		{"llm_daily_limit_usd", "it's", nil, `expected a non-negative number of dollars, got "it's"`},
	}
	for _, c := range cases {
		got, err := Check(c.name, c.value)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("%s %v: %v", c.name, c.value, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v: %#v %v", c.name, c.value, got, err)
		}
	}
}

func TestGradeSheets(t *testing.T) {
	db := open(t)
	s := newSettings(t, db)
	google := "https://docs.google.com/spreadsheets/d/abc_DEF-1/edit#gid=0"
	dropbox := "https://www.dropbox.com/scl/fi/xyz123/Grades%20Course.xlsx?rlkey=k&dl=0"
	v, err := s.Set("grades_sheets", []any{
		map[string]any{"url": " " + google + " ", "label": " DM  "},
		map[string]any{"url": dropbox, "label": "", "name": "Their  sheet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []GradeSheet{{URL: google, Label: "DM"}, {URL: dropbox, Name: "Their sheet"}}
	if !reflect.DeepEqual(v, want) {
		t.Fatal(v)
	}
	if got := stored(t, db, "grades_sheets"); got != `[{"url": "`+google+`", "label": "DM"}, {"url": "`+dropbox+
		`", "label": "", "name": "Their sheet"}]` {
		t.Fatal(got)
	}
	if id, _ := GradeSheetID(dropbox); id != "dropbox-xyz123" {
		t.Fatal(id)
	}
	if _, name, ok, err := DropboxFile(dropbox); !ok || err != nil || name != "Grades Course.xlsx" {
		t.Fatal(name, err)
	}
	if _, _, ok, err := DropboxFile("https://example.org/a.xlsx"); ok || err != nil {
		t.Fatal("not Dropbox's")
	}
	_, err = GradeSheetID("https://www.dropbox.com/scl/fo/abc/folder")
	if i18n.Reason(err, "ru") != "это ссылка на папку Dropbox: нужна ссылка на сам файл .xlsx" {
		t.Fatal(err)
	}
	_, err = GradeSheetID("https://dropbox.com/s/abc/notes.pdf")
	if err == nil || err.Error() != "the Dropbox file «notes.pdf» is not an .xlsx workbook" {
		t.Fatal(err)
	}
	if _, err := GradeSheetID("https://example.org/spreadsheets/d/abc"); err == nil ||
		i18n.Reason(err, "en") != NotASheet.En {
		t.Fatal(err)
	}
	for _, bad := range []any{
		[]any{"not a sheet"},
		[]any{map[string]any{"url": google, "extra": 1}},
		[]any{map[string]any{"url": google}, map[string]any{"url": "https://docs.google.com/spreadsheets/d/abc_DEF-1"}},
		[]any{map[string]any{"url": google, "label": "a long label"}},
	} {
		if _, err := s.Set("grades_sheets", bad); !isSettingsError(err) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
}

func TestPythonValues(t *testing.T) {
	for value, want := range map[float64]string{1: "1.0", 0.1: "0.1", 1e-5: "1e-05", 1e16: "1e+16", 1.5e15: "1500000000000000.0",
		0.0001: "0.0001", 123.456: "123.456"} {
		if got := pyjson.FormatFloat(value); got != want {
			t.Errorf("%v: %s", value, got)
		}
	}
	if Round(0.12345, 4) != 0.1235 && Round(0.12345, 4) != 0.1234 {
		t.Fatal(Round(0.12345, 4))
	}
	if Round(2.675, 2) != 2.67 { // the binary value is below the half, as in Python
		t.Fatal(Round(2.675, 2))
	}
	if got := strRepr("a'b\n"); got != `"a'b\n"` {
		t.Fatal(got)
	}
	if got := repr([]any{1.0, "x", nil, map[string]any{"k": true}}); got != "[1.0, 'x', None, {'k': True}]" {
		t.Fatal(got)
	}
}

func TestFloatHoursPythonStoredReadAsInts(t *testing.T) {
	// Python takes 3.0 for the batch hours (3.0 in BATCH_HOURS) and stores it as 3.0.
	db := open(t)
	for _, q := range []string{
		`insert into settings(name, value) values('batch_hours', '3.0')`,
		`insert into chat_settings(chat_id, name, value) values(-1001, 'batch_hours', '6.0')`,
		`insert into chat_settings(chat_id, name, value) values(-1001, 'enabled', 'true')`,
	} {
		if _, err := db.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := newSettings(t, db).Int("batch_hours"); err != nil || n != 3 {
		t.Fatal(n, err)
	}
	chat, err := NewChats(db).All(-1001)
	if err != nil || chat.BatchHours == nil || *chat.BatchHours != 6 || !chat.Enabled {
		t.Fatalf("%+v %v", chat, err)
	}
}

func TestAllKeepsLinksAsTheyAre(t *testing.T) {
	db := open(t)
	s := newSettings(t, db)
	link := "https://www.dropbox.com/scl/fi/xyz/a.xlsx?rlkey=k&dl=0"
	if _, err := s.Set("grades_sheets", []any{map[string]any{"url": link}}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.All()
	text, err := pyjson.Dumps(map[string]any{"general": all})
	if err != nil || !strings.Contains(text, `"grades_sheets": [{"url": "`+link+`", "label": ""}]`) {
		t.Fatal(text, err)
	}
}
