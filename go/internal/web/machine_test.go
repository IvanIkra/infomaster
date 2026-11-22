package web

// «Machine» (tests/test_web_machine.py): the sources and their freshness, what needs attention, the journal with its
// undo, the settings changed through the connector's tools. The owner's actions (Python's actions.py) are a fake
// here: what is tested is the screen's side.

import (
	"context"
	"strings"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/outputs/api"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

// fakeActions answer as the actions would, with a twin whose code is 12345.
type fakeActions struct{ account any }

func (f *fakeActions) twin() any {
	return map[string]any{"ok_at": nil, "error": nil, "login_needed": f.account == nil, "account": f.account}
}
func (f *fakeActions) Chats(context.Context) (any, error) {
	return httpx.Object{{Key: "twin", Value: f.twin()}, {Key: "chats", Value: []any{map[string]any{"title": "Course chat"}}}}, nil
}
func (f *fakeActions) Senders(context.Context, string) (any, error) { return []any{}, nil }
func (f *fakeActions) TwinLogin(_ context.Context, body map[string]any) (any, error) {
	if code, ok := body["code"]; ok {
		if code != "12345" {
			return nil, &tools.ToolError{Msg: "the code is wrong"}
		}
		f.account = map[string]any{"name": "Test Twin"}
	}
	return f.twin(), nil
}
func (f *fakeActions) TwinCancel(context.Context) (any, error) { return f.twin(), nil }
func (f *fakeActions) TwinLogout(context.Context) (any, error) {
	f.account = nil
	return f.twin(), nil
}
func (f *fakeActions) HseApp(context.Context) (any, error) {
	return map[string]any{"status": map[string]any{"login_needed": true}, "callback": testOrigin + "/hseapp/callback"}, nil
}
func (f *fakeActions) UpdateHseApp(context.Context, map[string]any) (any, error) {
	return f.HseApp(nil)
}
func (f *fakeActions) HseAppLink(context.Context, map[string]any) (any, error) {
	return map[string]any{"url": "https://auth.example/login"}, nil
}
func (f *fakeActions) HseAppLogin(_ context.Context, body map[string]any) (any, error) {
	return nil, httpx.BadRequest("url: the address from the browser expected")
}
func (f *fakeActions) Subjects(context.Context) (any, error) {
	return map[string]any{"subjects": []any{map[string]any{"name": "Discrete Mathematics"}}}, nil
}
func (f *fakeActions) LLM(context.Context) (any, error) {
	return map[string]any{"daily_limit_usd": 0.5}, nil
}
func (f *fakeActions) UpdateLLM(context.Context, map[string]any) (any, error) {
	return nil, &tools.ToolError{Msg: "daily_limit_usd: more than 0"}
}
func (f *fakeActions) RaiseLLM(context.Context) (any, error) {
	return map[string]any{"limit_usd": 3.0}, nil
}
func (f *fakeActions) GradeSheets(context.Context) (any, error) {
	return map[string]any{"sheets": []any{}}, nil
}
func (f *fakeActions) Gradebook(_ context.Context, sid string) (any, error) {
	return map[string]any{"id": sid, "tabs": []any{}}, nil
}
func (f *fakeActions) ReadGradebook(_ context.Context, sid string) (any, error) {
	return f.Gradebook(nil, sid)
}

func machine(t *testing.T) (*harness, *settings.Settings, *fakeActions) {
	h := newHarness(t, options{screens: true, connector: true})
	h.secs.value = float64(h.clock.now().Unix())
	s := must(settings.New(h.store))
	budget := llm.NewBudget(s, llm.NewJournal(h.store), h.store)
	budget.Now = h.clock.now
	actions := &fakeActions{}
	must(h.sch.Inbox.PutChat(testChat, "Course chat", "supergroup", nil, 0))
	status := func(value any) func() (any, error) { return func() (any, error) { return value, nil } }
	env := config.Env{PublicURL: testOrigin}
	h.app.Parts = Parts{
		Sources: []api.Source{
			{Name: "telegram", Status: func() (any, error) { return actions.twin(), nil }},
			{Name: "mail", Status: status(map[string]any{"configured": true, "ok_at": "2026-10-20T10:00:00", "error": nil, "letters": 12})},
			{Name: "ruz", Status: status(map[string]any{"ok_at": "2026-10-20T11:30:00", "error": nil, "classes": 70})},
			{Name: "hseapp", Status: status(map[string]any{"enabled": true, "login_needed": true, "ok_at": nil, "error": nil})},
			{Name: "wiki", Status: status(httpx.Object{{Key: "pages", Value: httpx.Object{{Key: "https://wiki.example.org/DM",
				Value: map[string]any{"title": "DM", "items": 0, "error": "HTTP 503"}}}}, {Key: "ok_at", Value: "2026-10-20T09:00:00"}})},
			{Name: "batches", Status: status(map[string]any{"ok_at": "2026-10-20T11:00:00", "error": nil, "queue": 2})},
			{Name: "grades", Status: status(map[string]any{"configured": true, "sheets": map[string]any{}})},
		},
		Env: &env, Budget: budget, Settings: s, Inbox: h.sch.Inbox, ChatSettings: settings.NewChats(h.store),
		Twin: true, Grades: true, Reminders: true, HseApp: true, Actions: actions,
	}
	stub(t, h.connector, "update_general_settings", []string{"news_ttl_hours", "about_me", "batch_hours"},
		func(_ *tools.Context, a map[string]any) (tools.Output, error) {
			undo := map[string]any{}
			for name, value := range a {
				undo[name] = must(s.Get(name))
				if _, err := s.Set(name, value); err != nil {
					return tools.Output{}, &tools.ToolError{Msg: i18n.Reason(err, "en")}
				}
			}
			return tools.Output{Summary: "Changed the settings", Undo: []tools.Step{{Tool: "update_general_settings",
				Arguments: undo}}}, nil
		})
	stub(t, h.connector, "update_reminders", []string{"eve", "day", "enabled"}, func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		_, err := s.Set("reminder_eve", a["eve"])
		return tools.Output{Summary: "Changed the reminders"}, err
	})
	for _, name := range []string{"update_chat_settings", "set_course", "delete_course", "add_gradebook",
		"update_gradebook", "remove_gradebook", "set_tab_layout", "reset_tab_layout"} {
		stub(t, h.connector, name, []string{"chat_id", "enabled", "study", "name", "title", "abbr", "url", "label",
			"sheet_id", "tab", "skip", "works"}, (&calls{}).recorder(name, true))
	}
	return h.serve().in(), s, actions
}

func (h *harness) machine(method, path string, body any, status int) map[string]any {
	h.t.Helper()
	return h.ask(method, "/machine"+path, body, status)
}

func TestOverview(t *testing.T) {
	h, _, _ := machine(t)
	seen := h.machine("GET", "", nil, 200)
	states := map[string]string{}
	for _, s := range list(seen["sources"]) {
		states[obj(s)["key"].(string)] = obj(s)["state"].(string)
		if obj(s)["key"] == "mail" && (obj(s)["ok_at"] != "2026-10-20T10:00:00" || obj(s)["count"] != int64(12)) {
			t.Fatal(s)
		}
	}
	sameJSON(t, "states", states, map[string]string{"twin": "login", "mail": "late", "ruz": "ok", "hseapp": "login",
		"grades": "off", "wiki": "error", "batches": "ok", "llm": "ok"}) // the mail was read two hours ago
	sameJSON(t, "attention", seen["attention"], []any{
		map[string]any{"text": "The twin is not logged in: no chat is read", "screen": "machine/chats"},
		map[string]any{"text": "HSE App X needs a login", "screen": "machine/hseapp"},
		map[string]any{"text": "A wiki page is not read: DM", "screen": "machine/wiki"},
		map[string]any{"text": "The calendar feeds are off: no CALENDAR_SECRET in .env", "screen": nil}})
	if seen["library"] != nil || obj(seen["llm"])["limit_usd"] != 0.5 || seen["connections"] != int64(0) || seen["lang"] != "en" {
		t.Fatal(seen)
	}
	sameJSON(t, "has", seen["has"], map[string]any{"twin": true, "settings": true, "grades": true, "reminders": true,
		"pdf": false, "connector": true, "hseapp": true})
	if h.read("/feed")["problems"] != int64(4) || h.read("/news")["problems"] != int64(4) {
		t.Fatal("the status dot")
	}
}

func TestSettingsJournaledWithUndo(t *testing.T) {
	h, s, _ := machine(t)
	got := h.machine("GET", "/settings", nil, 200)
	if obj(got["general"])["news_ttl_hours"] != int64(24) {
		t.Fatal(got)
	}
	if _, has := obj(got["general"])["llm_prices"]; has {
		t.Fatal("llm_prices")
	}
	if _, has := got["pdf"]; has {
		t.Fatal("pdf")
	}
	sameJSON(t, "reminders", got["reminders"], map[string]any{"enabled": true, "eve": "20:00", "day": "09:00"})
	sameJSON(t, "batch choices", got["batch_choices"], []int{1, 2, 3, 4, 6, 8, 12, 24})
	changed := h.machine("PATCH", "/settings/general", map[string]any{"news_ttl_hours": 48, "about_me": "Group 1"}, 200)
	if obj(obj(changed["settings"])["general"])["news_ttl_hours"] != int64(48) || obj(changed["change"])["undo"] != true {
		t.Fatal(changed)
	}
	h.machine("PATCH", "/settings/reminders", map[string]any{"eve": "21:30"}, 200)
	if must(s.String("reminder_eve")) != "21:30" {
		t.Fatal("reminders")
	}
	if bad := h.machine("PATCH", "/settings/general", map[string]any{"news_ttl_hours": 0}, 400); bad["error"] == nil {
		t.Fatal(bad)
	}
	if r := h.machine("PATCH", "/settings/llm", map[string]any{"x": 1}, 404); r["error"] != "no such group of settings" {
		t.Fatal(r)
	}
	journal := list(h.machine("GET", "/journal", nil, 200)["changes"])
	if len(journal) != 2 || obj(journal[0])["tool"] != "update_reminders" || obj(journal[1])["via"] != "web" {
		t.Fatal(journal)
	}
	h.undo(changed["change"])
	if must(s.Int("news_ttl_hours")) != 24 || must(s.String("about_me")) != "" {
		t.Fatal("undone")
	}
	if obj(list(h.machine("GET", "/journal", nil, 200)["changes"])[1])["undone_at"] == nil {
		t.Fatal("undone_at")
	}
	must(h.connector.Changes.Record(nil, "add_deadline", map[string]any{}, "Added a deadline", nil, "", "claude"))
	if obj(list(h.machine("GET", "/journal", nil, 200)["changes"])[0])["via"] != "claude" {
		t.Fatal("Claude's change")
	}
	if h.call("PATCH", "/machine/settings/general", map[string]any{"news_ttl_hours": 12}, req{token: h.token, origin: noOrigin()}).Code != 403 {
		t.Fatal("no Origin of the app")
	}
}

func TestTwinChatsAndActions(t *testing.T) {
	h, _, _ := machine(t)
	chats := h.machine("GET", "/chats", nil, 200)
	if obj(chats["twin"])["account"] != nil || obj(list(chats["chats"])[0])["title"] != "Course chat" {
		t.Fatal(chats)
	}
	if r := h.machine("POST", "/twin/login", map[string]any{"code": "1"}, 400); r["error"] != "the code is wrong" {
		t.Fatal(r)
	}
	if r := h.machine("POST", "/twin/login", map[string]any{"code": "12345"}, 200); obj(r["account"])["name"] != "Test Twin" {
		t.Fatal(r)
	}
	if r := h.machine("POST", "/twin/sideways", map[string]any{}, 404); r["error"] != "no such step" {
		t.Fatal(r)
	}
	for _, s := range list(h.machine("GET", "", nil, 200)["sources"]) {
		if obj(s)["key"] == "twin" && obj(s)["state"] != "ok" {
			t.Fatal(s)
		}
	}
	on := h.machine("PATCH", "/chats/-1001", map[string]any{"enabled": true}, 200)
	if _, ok := obj(on["settings"])["enabled"]; !ok || obj(on["change"])["undo"] != true {
		t.Fatal(on)
	}
	if r := h.call("GET", "/machine/chats/-1001/senders", nil, req{token: h.token}); r.Body.String() != "[]" {
		t.Fatal(r.Body)
	}
	if r := h.machine("POST", "/twin/logout", map[string]any{}, 200); r["account"] != nil {
		t.Fatal(r)
	}
	put := h.machine("PUT", "/courses", map[string]any{"name": "Algebra", "title": "Algebra", "abbr": "Alg"}, 200)
	if list(put["subjects"]) == nil || obj(put["change"])["undo"] != true {
		t.Fatal(put)
	}
	if h.machine("GET", "/llm", nil, 200)["daily_limit_usd"] != 0.5 || h.machine("POST", "/llm/raise", map[string]any{}, 200)["limit_usd"] != 3.0 {
		t.Fatal("llm")
	}
	h.machine("PATCH", "/llm", map[string]any{"daily_limit_usd": 0}, 400)
	added := h.machine("POST", "/grades", map[string]any{"url": "https://docs.google.com/spreadsheets/d/x/edit", "label": "DM"}, 200)
	if _, ok := added["sheets"]; !ok || obj(added["change"])["undo"] != true {
		t.Fatal(added)
	}
	laid := h.machine("PUT", "/grades/abc/tabs/Group%201", map[string]any{"skip": false, "works": []any{}}, 200)
	if laid["id"] != "abc" {
		t.Fatal(laid)
	}
	if h.machine("POST", "/grades/abc/read", map[string]any{}, 200)["id"] != "abc" {
		t.Fatal("read")
	}
	hse := h.machine("GET", "/hseapp", nil, 200)
	if !strings.HasSuffix(hse["callback"].(string), "/hseapp/callback") {
		t.Fatal(hse)
	}
	if !strings.HasPrefix(h.machine("POST", "/hseapp/link", map[string]any{"app": false}, 200)["url"].(string), "https://") {
		t.Fatal("link")
	}
	h.machine("POST", "/hseapp/login", map[string]any{"url": " "}, 400)
	state := h.machine("GET", "/connector", nil, 200)
	if !strings.HasSuffix(state["url"].(string), "/mcp") || len(list(state["connections"])) != 0 {
		t.Fatal(state)
	}
	if _, has := state["changes"]; has {
		t.Fatal("changes")
	}
	if r := h.machine("DELETE", "/connector/7", nil, 404); r["error"] != "no such connection" {
		t.Fatal(r)
	}
}

func TestWithoutParts(t *testing.T) {
	h := newHarness(t, options{}).serve().in()
	seen := h.machine("GET", "", nil, 200)
	if len(list(seen["sources"])) != 0 || len(list(seen["attention"])) != 0 || seen["llm"] != nil {
		t.Fatal(seen)
	}
	if len(list(h.machine("GET", "/journal", nil, 200)["changes"])) != 0 {
		t.Fatal("journal")
	}
	if h.machine("GET", "/settings", nil, 503)["error"] == nil ||
		h.machine("PATCH", "/settings/general", map[string]any{"batch_hours": 6}, 503)["error"] == nil {
		t.Fatal("refused")
	}
}
