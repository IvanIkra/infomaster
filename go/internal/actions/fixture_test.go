package actions_test

// The fixtures of the actions' tests (tests/conftest.py, tests/actions_app.py, test_http.py's env and mini): the
// owner's actions over HTTP at /app/api, for the tests only, as Python's tests mount them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/actions"
	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/outputs/api"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	hsedl "github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const publicURL = "https://infomaster.example.ts.net"

func env() config.Env {
	return config.Env{APIID: 1, APIHash: "h", BotToken: "123:secret", OwnerID: 42, APIToken: "tok", PublicURL: publicURL}
}

type fakeClock struct {
	mu     sync.Mutex
	moment time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.moment
}

var names = map[string][2]string{
	"Математический анализ": {"Calculus", "Calc"},
	"Алгебра": {"Algebra", "Alg"},
	"Введение в программирование": {"Intro to Prog", "Prog"},
}

func namer() *llm.Fake {
	return &llm.Fake{Respond: func(req llm.Request) (any, error) {
		items := []map[string]any{}
		for _, line := range strings.Split(req.Prompt, "\n") {
			if rest, ok := strings.CutPrefix(line, "- id "); ok {
				index, name, _ := strings.Cut(rest, ": ")
				if named, ok := names[strings.Trim(name, `"`)]; ok {
					items = append(items, map[string]any{"id": index, "title": named[0], "abbr": named[1]})
				}
			}
		}
		return map[string]any{"items": items}, nil
	}}
}

// twinStatus and ruzStatus are TWIN of test_http.py.
func twinStatus() (any, error) {
	return httpx.Object{{Key: "ok_at", Value: nil}, {Key: "error", Value: nil}, {Key: "login_needed", Value: false}}, nil
}

func ruzStatus() (any, error) {
	return httpx.Object{{Key: "ok_at", Value: nil}, {Key: "error", Value: "RUZ_ICAL_URL is not set"},
		{Key: "classes", Value: 0}, {Key: "subjects_error", Value: nil}}, nil
}

func twin() []api.Source {
	return []api.Source{{Name: "telegram", Status: twinStatus}, {Name: "ruz", Status: ruzStatus}}
}

type fixture struct {
	t        *testing.T
	store    *store.Store
	clock    *fakeClock
	settings *settings.Settings
	budget   *llm.Budget
	journal  *llm.Journal
	schedule *schedule.Schedule
	merged   *merge.DeadlineList
	services actions.Services
	server   *httptest.Server
	actions  *actions.Actions
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// setup is the conftest fixtures: the store, the clock at noon in Moscow, the budget, the schedule and the merged
// deadlines; the services as test_http.mini gives them, without the app's settings (settings=None) unless with.
func setup(t *testing.T) *fixture {
	t.Helper()
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	c := &fakeClock{moment: time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}
	st := must(settings.New(s))
	journal := llm.NewJournal(s)
	budget := llm.NewBudget(st, journal, s)
	budget.Now = c.now
	r := ruz.New("", s)
	r.Now = c.now
	sub := subjects.New(s, namer())
	in := inbox.Open(s)
	sch := schedule.New(s, r, events.Open(s), deadlines.Open(s), sub, in)
	sch.Now = c.now
	h := hsedl.New(s, sub, &llm.Fake{})
	h.Now = c.now
	merged := merge.New(s, sch.Deadlines, h, sub)
	merged.Now = c.now
	hse := hseapp.New(s, must(settings.New(s)), publicURL+hseapp.CallbackPath, c.now)
	f := &fixture{t: t, store: s, clock: c, settings: st, budget: budget, journal: journal, schedule: sch,
		merged: merged}
	f.services = actions.Services{Env: env(), Budget: budget, Journal: journal, Inbox: in,
		ChatSettings: settings.NewChats(s), Sources: twin(), Subjects: sub, Schedule: sch, HseApp: hse,
		Deadlines: merged}
	return f
}

// serve mounts the actions over the services at /app/api (tests/actions_app.py) on the public port's mux.
func (f *fixture) serve() *fixture {
	a, mux := actions.PublicApp(f.services)
	f.actions = a
	mount(mux, a)
	if f.server != nil {
		f.server.Close()
	}
	f.server = httptest.NewServer(httpx.Router(mux))
	f.t.Cleanup(f.server.Close)
	return f
}

func (f *fixture) syncFeed() *ruz.Diff {
	return must(f.schedule.Ruz.Sync(must(ruz.Parse(must(os.ReadFile("testdata/ruz.ics")))), false))
}

type response struct {
	status int
	body   []byte
	header http.Header
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("not a JSON object (%d): %s", r.status, r.body)
	}
	return out
}

func (r response) list(t *testing.T) []any {
	t.Helper()
	var out []any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("not a JSON list (%d): %s", r.status, r.body)
	}
	return out
}

func (r response) text() string { return string(r.body) }

// do sends a request: body is a JSON value, or a string sent as it is.
func (f *fixture) do(method, path string, body any) response {
	f.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		reader = bytes.NewReader(must(json.Marshal(b)))
	}
	req := must(http.NewRequest(method, f.server.URL+path, reader))
	resp := must(http.DefaultClient.Do(req))
	defer resp.Body.Close()
	return response{resp.StatusCode, must(io.ReadAll(resp.Body)), resp.Header}
}

func (f *fixture) get(path string) response             { return f.do("GET", "/app/api"+path, nil) }
func (f *fixture) patch(path string, body any) response { return f.do("PATCH", "/app/api"+path, body) }
func (f *fixture) post(path string, body any) response  { return f.do("POST", "/app/api"+path, body) }
func (f *fixture) put(path string, body any) response   { return f.do("PUT", "/app/api"+path, body) }
func (f *fixture) delete(path string, body any) response {
	return f.do("DELETE", "/app/api"+path, body)
}

type handler func(a *actions.Actions, r *http.Request, body map[string]any) (any, error)

// mount is tests/actions_app.py: every action at /app/api + its path.
func mount(mux *http.ServeMux, a *actions.Actions) {
	ctx := func(r *http.Request) context.Context { return r.Context() }
	v := func(r *http.Request, name string) string { return r.PathValue(name) }
	routes := []struct {
		method, path string
		body         bool
		run          handler
	}{
		{"GET", "/chats", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.Chats(ctx(r))
		}},
		{"PATCH", "/chats/{chat_id}", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateChat(ctx(r), v(r, "chat_id"), b)
		}},
		{"GET", "/chats/{chat_id}/senders", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.Senders(ctx(r), v(r, "chat_id"))
		}},
		{"POST", "/twin/login", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.TwinLogin(ctx(r), b)
		}},
		{"POST", "/twin/cancel", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.TwinCancel(ctx(r))
		}},
		{"POST", "/twin/logout", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.TwinLogout(ctx(r))
		}},
		{"GET", "/llm", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.LLM(ctx(r))
		}},
		{"PATCH", "/llm", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateLLM(ctx(r), b)
		}},
		{"POST", "/llm/raise", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.RaiseLLM(ctx(r))
		}},
		{"PATCH", "/images", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateImages(ctx(r), b)
		}},
		{"GET", "/general", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.General(ctx(r))
		}},
		{"PATCH", "/general", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateGeneral(ctx(r), b)
		}},
		{"GET", "/subjects", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.Subjects(ctx(r))
		}},
		{"PUT", "/subjects", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.PutSubject(ctx(r), b)
		}},
		{"DELETE", "/subjects", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.DeleteSubject(ctx(r), b)
		}},
		{"GET", "/agenda", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.Agenda(ctx(r))
		}},
		{"GET", "/message", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			q := r.URL.Query()
			if !q.Has("chat") || !q.Has("msg") {
				return nil, httpx.BadRequest("expected ?chat=<id>&msg=<id>")
			}
			return a.Message(ctx(r), q.Get("chat"), q.Get("msg"))
		}},
		{"POST", "/events", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.AddEvent(ctx(r), b)
		}},
		{"PATCH", "/events/{event_id}", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateEvent(ctx(r), v(r, "event_id"), b)
		}},
		{"DELETE", "/events/{event_id}", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.DeleteEvent(ctx(r), v(r, "event_id"))
		}},
		{"POST", "/corrections", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.CorrectClass(ctx(r), b)
		}},
		{"GET", "/deadlines", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.DeadlinesList(ctx(r))
		}},
		{"POST", "/deadlines", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.AddDeadline(ctx(r), b)
		}},
		{"PATCH", "/deadlines/{uid}", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.EditDeadline(ctx(r), v(r, "uid"), b)
		}},
		{"DELETE", "/deadlines/{uid}", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.DeleteDeadline(ctx(r), v(r, "uid"))
		}},
		{"DELETE", "/hidden/{uid}", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.UnhideDeadline(ctx(r), v(r, "uid"))
		}},
		{"GET", "/sheets", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.SheetsList(ctx(r))
		}},
		{"PATCH", "/sheets", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateSheets(ctx(r), b)
		}},
		{"PATCH", "/sheets/{sheet_id}", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.EditSheet(ctx(r), v(r, "sheet_id"), b)
		}},
		{"GET", "/reminders", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.ReminderSettings(ctx(r))
		}},
		{"PATCH", "/reminders", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateReminders(ctx(r), b)
		}},
		{"GET", "/grades", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.GradeSheets(ctx(r))
		}},
		{"PATCH", "/grades", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateGrades(ctx(r), b)
		}},
		{"POST", "/grades/items", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.AddGrade(ctx(r), b)
		}},
		{"PATCH", "/grades/items/{grade}", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.ChangeGrade(ctx(r), v(r, "grade"), b)
		}},
		{"DELETE", "/grades/items/{grade}", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.DropGrade(ctx(r), v(r, "grade"))
		}},
		{"GET", "/grades/sheets/{sid}", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.Gradebook(ctx(r), v(r, "sid"))
		}},
		{"PATCH", "/grades/sheets/{sid}", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateGradebook(ctx(r), v(r, "sid"), b)
		}},
		{"POST", "/grades/sheets/{sid}/read", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.ReadGradebook(ctx(r), v(r, "sid"))
		}},
		{"PUT", "/grades/sheets/{sid}/tabs/{tab}", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.SetLayout(ctx(r), v(r, "sid"), v(r, "tab"), b)
		}},
		{"DELETE", "/grades/sheets/{sid}/tabs/{tab}", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.ClearLayout(ctx(r), v(r, "sid"), v(r, "tab"))
		}},
		{"GET", "/hseapp", false, func(a *actions.Actions, r *http.Request, _ map[string]any) (any, error) {
			return a.HseApp(ctx(r))
		}},
		{"PATCH", "/hseapp", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.UpdateHseApp(ctx(r), b)
		}},
		{"POST", "/hseapp/link", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.HseAppLink(ctx(r), b)
		}},
		{"POST", "/hseapp/login", true, func(a *actions.Actions, r *http.Request, b map[string]any) (any, error) {
			return a.HseAppLogin(ctx(r), b)
		}},
	}
	for _, route := range routes {
		route := route
		mux.Handle(route.method+" /app/api"+route.path, httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
			var body map[string]any
			if route.body { // actions._json
				value, err := httpx.ReadJSON(r, httpx.BodyMax)
				if errors.Is(err, httpx.ErrNotJSON) {
					return httpx.BadRequest("expected JSON")
				}
				if err != nil {
					return err
				}
				object, ok := value.(map[string]any)
				if !ok {
					return httpx.BadRequest("expected a JSON object")
				}
				body = object
			}
			answer, err := route.run(a, r, body)
			if err != nil {
				return err
			}
			httpx.JSONLang(w, http.StatusOK, answer, a.Lang())
			return nil
		}))
	}
}

// spend is conftest.spend: a run of the journal.
func spend(t *testing.T, j *llm.Journal, usd float64, at time.Time, task string, chatID *int64) {
	t.Helper()
	noErr(t, j.Add(llm.Run{At: clock.UTCISO(at), Task: task, ChatID: chatID, Model: "claude-haiku-5-5", OK: true,
		InputTokens: 1, OutputTokens: 1, CostUSD: pyjson.Float(usd), DurationS: 0.1}))
}

// addRow is test_http.add_row: a deadline of a chat (or a letter).
func (f *fixture) addRow(label, due, source, course string) int64 {
	chat := int64(-1001)
	return must(f.merged.Deadlines.Add(deadlines.New{Source: source, ChatID: &chat, MsgIDs: []int64{1},
		Course: course, Label: label, Due: due}, f.merged.Now()))
}

func ptr[T any](v T) *T { return &v }
