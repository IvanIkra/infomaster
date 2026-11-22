package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/outputs/icalfeed"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	"github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/news"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const secret = "calendar-secret-0123456789"

type fixture struct {
	store    *store.Store
	settings *settings.Settings
	budget   *llm.Budget
	merged   *merge.DeadlineList
	now      time.Time
}

func env() config.Env {
	return config.Env{APIID: 1, APIHash: "h", BotToken: "123:secret", OwnerID: 42, APIToken: "tok",
		PublicURL: "https://infomaster.example.ts.net"}
}

// twin is the sources of test_http.TWIN.
func twin() []Source {
	return []Source{
		{"telegram", func() (any, error) {
			return map[string]any{"ok_at": nil, "error": nil, "login_needed": false}, nil
		}},
		{"ruz", func() (any, error) {
			return map[string]any{"ok_at": nil, "error": "RUZ_ICAL_URL is not set", "classes": 0,
				"subjects_error": nil}, nil
		}},
	}
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	set, err := settings.New(s)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()
	budget := llm.NewBudget(set, llm.NewJournal(s), s)
	budget.Now = func() time.Time { return now }
	sub := subjects.New(s, &llm.Fake{})
	h := hseapp.New(s, sub, &llm.Fake{})
	merged := merge.New(s, deadlines.Open(s), h, sub)
	merged.Now = func() time.Time { return now }
	return &fixture{store: s, settings: set, budget: budget, merged: merged, now: now}
}

func get(t *testing.T, h http.Handler, method, path, token string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(body), w.Result().Header
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return out
}

func TestInternalAPI(t *testing.T) {
	f := setup(t)
	h := New(env(), f.budget, twin(), nil, f.merged).Handler()
	if code, body, _ := get(t, h, "GET", "/api/status", ""); code != 401 || body != "Bearer token required" {
		t.Fatal(code, body)
	}
	for _, wrong := range []string{"Bearer wrong", "bearer tok", "Bearertok", "Bearer  tok"} {
		if code, _, _ := get(t, h, "GET", "/api/status", wrong); code != 401 {
			t.Fatal(wrong, code)
		}
	}
	code, body, header := get(t, h, "GET", "/api/status", "Bearer tok")
	if code != 200 || header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatal(code, header)
	}
	got := decode(t, body)
	if got["llm"].(map[string]any)["level"] != "ok" || got["telegram"].(map[string]any)["login_needed"] != false {
		t.Fatal(body)
	}
	if ruz := got["ruz"].(map[string]any); ruz["error"] != "RUZ_ICAL_URL is not set" || ruz["classes"] != 0.0 {
		t.Fatal(body)
	}
	// The sources in their order, then llm; Python's separators and floats.
	if !strings.HasPrefix(body, `{"telegram": {`) || !strings.Contains(body, `, "ruz": {`) ||
		!strings.Contains(body, `"llm": {"spent_usd": 0.0, "limit_usd": `) {
		t.Fatal(body)
	}
	// No CALENDAR_SECRET: no feeds.
	if code, body, _ := get(t, h, "GET", "/calendar/x/main.ics", ""); code != 404 || body != "404: Not Found" {
		t.Fatal(code, body)
	}
	// /api/ needs the token before anything is found.
	if code, _, _ := get(t, h, "GET", "/api/nothing", ""); code != 401 {
		t.Fatal(code)
	}
	if code, body, _ := get(t, h, "GET", "/api/nothing", "Bearer tok"); code != 404 || body != "404: Not Found" {
		t.Fatal(code, body)
	}
	// Without grades and news their routes are not there.
	for _, path := range []string{"/api/grades", "/api/news"} {
		if code, _, _ := get(t, h, "GET", path, "Bearer tok"); code != 404 {
			t.Fatal(path, code)
		}
	}
}

func TestStatusPhraseInEnglish(t *testing.T) {
	f := setup(t)
	type status struct {
		OkAt  *string      `json:"ok_at"`
		Error *i18n.Phrase `json:"error"`
	}
	phrase := i18n.P("the wiki is unavailable", "вики недоступна")
	h := New(env(), f.budget, []Source{
		{"wiki", func() (any, error) { return status{Error: &phrase}, nil }},
		{"mail", func() (any, error) { return map[string]any{"error": "Почта"}, nil }},
	}, nil, f.merged).Handler()
	_, body, _ := get(t, h, "GET", "/api/status", "Bearer tok")
	if !strings.HasPrefix(body, `{"wiki": {"ok_at": null, "error": "the wiki is unavailable"}, "mail": `) ||
		!strings.Contains(body, "\"error\": \"\\u041f\\u043e\\u0447\\u0442\\u0430\"") { // ASCII, as json.dumps writes
		t.Fatal(body)
	}
}

func TestNewsAPI(t *testing.T) {
	f := setup(t)
	all := news.Open(f.store)
	add := func(full string, importance int64, study bool, hoursAgo int) int64 {
		posted := clock.LocalISO(f.now.Add(-time.Duration(hoursAgo) * time.Hour))
		short := []rune(full)
		if len(short) > 10 {
			short = short[:10]
		}
		id, err := all.Add(news.New{Source: "tg", ChatID: -1001, MsgIDs: []int64{1}, Study: study, PostedAt: posted,
			Full: full, Short: string(short), Importance: importance}, f.now)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	chatter, study, old := add("Concert tonight", 3, false, 1), add("Calc KR-1 Oct 22", 4, true, 2), add("Old", 5, false, 30)
	replaced, hidden := add("Alg test Nov 6", 4, true, 3), add("Party", 5, false, 1)
	if err := all.Replace(replaced, add("Alg test Nov 7", 4, true, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := all.Hide(hidden); err != nil {
		t.Fatal(err)
	}
	a := New(env(), f.budget, twin(), nil, f.merged)
	a.News, a.Settings, a.Now = all, f.settings, func() time.Time { return f.now }
	h := a.Handler()
	if code, _, _ := get(t, h, "GET", "/api/news", ""); code != 401 {
		t.Fatal(code)
	}
	_, body, _ := get(t, h, "GET", "/api/news", "Bearer tok")
	got := decode(t, body)
	items := got["items"].([]any)
	if got["ttl_hours"] != 24.0 || len(items) != 3 {
		t.Fatal(body)
	}
	var fulls []string
	for _, item := range items {
		fulls = append(fulls, item.(map[string]any)["full"].(string))
		if item.(map[string]any)["id"] == float64(old) {
			t.Fatal("old news given")
		}
	}
	if strings.Join(fulls, "|") != "Alg test Nov 7|Calc KR-1 Oct 22|Concert tonight" {
		t.Fatal(fulls)
	}
	want := `{"id": ` + itoa(study) + `, "source": "tg", "study": true, "posted_at": "` +
		clock.LocalISO(f.now.Add(-2*time.Hour)) + `", "full": "Calc KR-1 Oct 22", "short": "Calc KR-1 ", "importance": 4}`
	if !strings.Contains(body, want) {
		t.Fatal(body)
	}
	if items[2].(map[string]any)["id"] != float64(chatter) {
		t.Fatal(body)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestDeadlinesAPI(t *testing.T) {
	f := setup(t)
	h := New(env(), f.budget, twin(), nil, f.merged).Handler()
	today := f.merged.Today()
	chat := int64(-1001)
	if _, err := f.merged.Deadlines.Add(deadlines.New{Source: "mail", ChatID: &chat, MsgIDs: []int64{1},
		Course: "Математический анализ", Label: "Calc HW 3", Due: today.AddDate(0, 0, 1).Format("2006-01-02")},
		f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutJSON("hseapp.deadlines", map[string]any{"fetched_at": nil, "done": []any{},
		"items": []any{map[string]any{"id": "p1", "label": "Prog HW 1", "due": today.AddDate(0, 0, -2).Format("2006-01-02"),
			"course": nil, "title": "ДЗ 1 по программированию"}}}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := get(t, h, "GET", "/api/deadlines", ""); code != 401 {
		t.Fatal(code)
	}
	_, body, _ := get(t, h, "GET", "/api/deadlines", "Bearer tok")
	items := decode(t, body)["items"].([]any)
	if len(items) != 2 {
		t.Fatal(body)
	}
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["title"] != "Calc HW 3" || first["source"] != "mail" || first["overdue"] != false ||
		second["title"] != "Prog HW 1" || second["source"] != "hseapp" || second["overdue"] != true {
		t.Fatal(body)
	}
	if len(first) != 7 || first["category"] != "study" || first["assumed"] != false {
		t.Fatal(first)
	}
	// Nothing changes the list through the API (D34).
	uid := first["uid"].(string)
	if code, body, header := get(t, h, "POST", "/api/deadlines", "Bearer tok"); code != 405 ||
		body != "405: Method Not Allowed" || !strings.Contains(header.Get("Allow"), "GET") {
		t.Fatal(code, body, header)
	}
	for _, method := range []string{"PATCH", "DELETE"} {
		if code, _, _ := get(t, h, method, "/api/deadlines/"+uid, "Bearer tok"); code != 404 {
			t.Fatal(method, code)
		}
	}
	if code, _, _ := get(t, h, "POST", "/api/deadlines", ""); code != 401 {
		t.Fatal(code)
	}
}

func TestGradesAPI(t *testing.T) {
	f := setup(t)
	if _, err := f.settings.Set("grades_student", "Ivanov Ivan"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.settings.Set("grades_sheets", []any{map[string]any{
		"url": "https://docs.google.com/spreadsheets/d/abc123/edit", "label": "DM"}}); err != nil {
		t.Fatal(err)
	}
	g := grades.New(f.store, f.settings, &llm.Fake{})
	moment := f.now
	g.Now = func() time.Time { return moment }
	title := "DM"
	if _, err := g.Record("abc123", &title, nil); err != nil {
		t.Fatal(err)
	}
	moment = moment.Add(time.Hour)
	bonus := 1.0
	if _, err := g.Record("abc123", &title, []grades.Item{{Key: "k1", Tab: "Sheet1", Header: "HW 1",
		Label: "DM HW 1", Value: 8, Raw: 8, Max: 10, Bonus: &bonus}}); err != nil {
		t.Fatal(err)
	}
	a := New(env(), f.budget, twin(), nil, f.merged)
	a.Grades = g
	h := a.Handler()
	_, body, _ := get(t, h, "GET", "/api/grades", "Bearer tok")
	want := `{"items": [{"key": "k1", "label": "DM HW 1", "value": 8.0, "bonus": 1.0, "seen_at": "` +
		clock.LocalISO(moment) + `"}], "window_days": 7}`
	if body != want {
		t.Fatalf("%s\n%s", body, want)
	}
	if code, _, _ := get(t, h, "GET", "/api/grades", ""); code != 401 {
		t.Fatal(code)
	}
}

// --- the feeds on the internal port (test_ical_feed.py: test_routes, test_public_routes) -------------------

type noEntries struct{}

func (noEntries) Entries() ([]schedule.Entry, error) { return nil, nil }

func TestCalendarRoutes(t *testing.T) {
	f := setup(t)
	e := env()
	e.CalendarSecret = secret
	e.CalendarPublicSecret = "public-secret-0123456789"
	feed := icalfeed.New(noEntries{})
	feed.Now = func() time.Time { return f.now }
	h := New(e, f.budget, nil, feed, f.merged).Handler()
	code, body, header := get(t, h, "GET", "/calendar/"+secret+"/tablo.ics", "")
	if code != 200 || !strings.HasPrefix(header.Get("Content-Type"), "text/calendar") ||
		!strings.HasPrefix(body, "BEGIN:VCALENDAR") {
		t.Fatal(code, header, body)
	}
	for _, path := range []string{"/calendar/" + secret + "x/main.ics", "/calendar/wrong/main.ics",
		"/calendar/" + secret + "/all.ics", "/calendar/" + secret + "/main.txt",
		"/calendar/" + secret[:len(secret)-1] + "/main.ics", "/calendar/" + e.CalendarPublicSecret + "/main.ics"} {
		if code, _, _ := get(t, h, "GET", path, ""); code != 404 {
			t.Fatal(path, code)
		}
	}
	if code, _, _ := get(t, h, "GET", "/calendar/"+secret+"/main.ics", "Bearer no"); code != 200 {
		t.Fatal(code)
	}
}
