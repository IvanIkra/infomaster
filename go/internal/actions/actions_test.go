package actions_test

// The owner's actions (tests/test_http.py test_actions_* and test_env, and the actions' parts of test_images.py,
// test_hseapp.py, test_grades.py and test_ical_feed.py).

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/actions"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/outputs/api"
	"github.com/IvanIkra/infomaster/go/internal/outputs/icalfeed"
	"github.com/IvanIkra/infomaster/go/internal/outputs/reminders"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

func check(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

func equal(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any         { l, _ := v.([]any); return l }

func TestActionsLLM(t *testing.T) {
	f := setup(t)
	f.services.Settings = f.settings
	f.serve()
	must(f.schedule.Inbox.PutChat(-1001, "Матан", "supergroup", nil, 0))
	now := f.clock.now()
	spend(t, f.journal, 0.20, now, "batch", ptr(int64(-1001)))
	spend(t, f.journal, 0.05, now, "batch", ptr(int64(0))) // the mailbox
	spend(t, f.journal, 0.05, now, "subjects", nil)        // no chat
	spend(t, f.journal, 0.30, now.Add(-72*time.Hour), "image", ptr(int64(-1001)))
	body := f.get("/llm").json(t)
	periods := obj(body["periods"])
	today, month := obj(periods["today"]), obj(periods["month"])
	equal(t, today["cost_usd"], 0.3)
	equal(t, today["runs"], 3.0)
	equal(t, month["cost_usd"], 0.6)
	equal(t, today["by_task"], []any{
		map[string]any{"task": "batch", "runs": 2.0, "failed": 0.0, "cost_usd": 0.25, "tokens": 4.0},
		map[string]any{"task": "subjects", "runs": 1.0, "failed": 0.0, "cost_usd": 0.05, "tokens": 2.0}})
	chats := list(today["by_chat"])
	equal(t, []any{obj(chats[0])["title"], obj(chats[0])["cost_usd"], obj(chats[1])["title"]},
		[]any{"Матан", 0.2, "Mail"})
	must(f.settings.Set("language", "ru")) // what the service names itself follows the web app's language (D41)
	body = f.get("/llm").json(t)
	chats = list(obj(obj(body["periods"])["today"])["by_chat"])
	equal(t, obj(chats[1])["title"], "Почта")
	noErr(t, f.settings.Reset("language"))
	days := list(body["days"])
	equal(t, len(days), 30)
	equal(t, days[29], map[string]any{"day": "2026-10-20", "runs": 3.0, "failed": 0.0, "cost_usd": 0.3,
		"stopped": false})
	equal(t, obj(days[26])["cost_usd"], 0.3)
	equal(t, body["month_forecast_usd"], math.Round(0.6/20*31*1e4)/1e4)
	equal(t, body["daily_limit_usd"], 0.5)
	check(t, strings.Contains(f.get("/llm").text(), `"soft_share": 0.8`), "soft_share")

	equal(t, f.patch("/llm", map[string]any{"daily_limit_usd": 0}).status, 400)
	equal(t, f.patch("/llm", map[string]any{"daily_limit_usd": 1, "x": 1}).status, 400)
	body = f.patch("/llm", map[string]any{"daily_limit_usd": 0.75}).json(t)
	equal(t, []any{body["daily_limit_usd"], body["limit_usd"]}, []any{0.75, 0.75})
	body = f.post("/llm/raise", nil).json(t)
	equal(t, []any{body["limit_usd"], body["raised_usd"], body["level"]}, []any{1.5, 0.75, "ok"})
}

func TestActionsGeneral(t *testing.T) {
	f := setup(t)
	f.services.Settings = f.settings
	f.serve()
	body := f.get("/general").json(t)
	equal(t, []any{body["batch_hours"], body["news_ttl_hours"]}, []any{3.0, 24.0})
	check(t, strings.Contains(fmt.Sprint(body["batch_choices"]), "24"), "batch choices")
	prices := obj(body["llm_prices"])
	haiku := obj(prices["claude-haiku-5-5"])
	haiku["input"] = 0.2
	body = f.patch("/general", map[string]any{"batch_hours": 6, "llm_prices": prices}).json(t)
	equal(t, body["batch_hours"], 6.0)
	stored := must(f.settings.Prices())["claude-haiku-5-5"]
	equal(t, float64(stored.Input), 0.2)
	bad := f.patch("/general", map[string]any{"batch_hours": 2, "news_ttl_hours": 500})
	equal(t, bad.status, 400)
	equal(t, must(f.settings.Int("batch_hours")), 6) // nothing saved from a rejected request
	equal(t, f.patch("/general", map[string]any{"pdf_pages": 3}).status, 400)
	equal(t, f.patch("/general", map[string]any{}).status, 400)

	equal(t, []any{body["language"], body["languages"]}, []any{"en", []any{"en", "ru"}})
	_, forum := body["forum_language"]
	check(t, !forum, "forum_language")
	body = f.patch("/general", map[string]any{"language": "ru"}).json(t)
	equal(t, body["language"], "ru")
	equal(t, f.patch("/general", map[string]any{"language": "de"}).status, 400)
	equal(t, must(f.settings.String("language")), "ru")
	equal(t, f.patch("/general", map[string]any{"forum_language": "ru"}).status, 400)

	equal(t, body["about_me"], "")
	body = f.patch("/general", map[string]any{"about_me": " Group 3 \n subgroup 2"}).json(t)
	equal(t, body["about_me"], "Group 3\nsubgroup 2")
	equal(t, f.patch("/general", map[string]any{"about_me": strings.Repeat("x", 501)}).status, 400)
}

func TestActionsChats(t *testing.T) {
	f := setup(t)
	f.serve()
	in := f.schedule.Inbox
	must(in.PutChat(-1001, "Матан", "supergroup", nil, 10))
	must(in.PutChat(-1002, "Болталка", "group", nil, 5))
	must(in.Add(-1001, 11, inbox.Fields{SenderID: ptr(int64(7)), SenderName: ptr("Староста"), At: f.clock.now(),
		Text: "Дедлайн"}, "tg", time.Time{}))

	equal(t, f.patch("/chats/-1002", map[string]any{"enabled": true, "people": []any{7, 7, 3}}).json(t)["people"],
		[]any{3.0, 7.0})
	body := f.get("/chats").json(t)
	chats := list(body["chats"])
	equal(t, []any{obj(chats[0])["title"], obj(chats[1])["title"]}, []any{"Болталка", "Матан"}) // enabled first
	equal(t, obj(chats[1])["messages"], 1.0)
	equal(t, obj(body["twin"])["login_needed"], false)

	equal(t, f.patch("/chats/-1001", map[string]any{"study": true, "filter": "some"}).status, 400)
	equal(t, must(f.services.ChatSettings.Get(-1001, "study")), false) // nothing saved from a rejected request
	equal(t, f.patch("/chats/-5", map[string]any{"enabled": true}).status, 404)
	equal(t, f.patch("/chats/-1001", "[").status, 400)
	equal(t, f.patch("/chats/-1001", map[string]any{"summary": true}).status, 400) // test_actions_has_no_summaries
	equal(t, f.get("/summaries").status, 404)

	senders := f.get("/chats/-1002/senders").list(t)
	ids := map[float64]bool{}
	for _, s := range senders {
		ids[obj(s)["id"].(float64)] = true
	}
	equal(t, ids, map[float64]bool{3: true, 7: true}) // chosen people are listed before they write
	equal(t, f.get("/chats/-1001/senders").list(t), []any{map[string]any{"id": 7.0, "name": "Староста",
		"messages": 1.0}})
}

func TestActionsDailyNumber(t *testing.T) { // test_images.py: test_actions_daily_number
	f := setup(t)
	f.services.Settings = f.settings
	f.serve()
	equal(t, f.patch("/images", map[string]any{"daily": 5}).json(t), map[string]any{"daily": 5.0})
	equal(t, must(f.settings.Int("images_daily")), 5)
	equal(t, f.patch("/images", map[string]any{"daily": -1}).status, 400)
	equal(t, f.patch("/images", map[string]any{"daily": 5, "x": 1}).status, 400)
}

func TestEnv(t *testing.T) {
	base := map[string]string{"TG_API_ID": "1", "TG_API_HASH": "h", "TG_BOT_TOKEN": "t", "TG_OWNER_ID": "42",
		"TG_FORUM_ID": "-1001", "API_TOKEN": "a", "PUBLIC_URL": "https://x.ts.net/"}
	load := func(extra map[string]string, drop ...string) (config.Env, error) {
		values := map[string]string{}
		for k, v := range base {
			values[k] = v
		}
		for k, v := range extra {
			values[k] = v
		}
		for _, k := range drop {
			delete(values, k)
		}
		return config.FromEnviron(func(k string) string { return values[k] })
	}
	loaded := must(load(nil))
	equal(t, []any{loaded.PublicURL, loaded.PublicPort}, []any{"https://x.ts.net", 8080})
	equal(t, must(load(nil, "TG_FORUM_ID")), loaded) // no forum (D69)
	_, err := load(map[string]string{"TG_BOT_TOKEN": ""})
	check(t, err != nil && strings.Contains(err.Error(), "TG_BOT_TOKEN"), "bot token: %v", err)
	_, err = load(map[string]string{"PUBLIC_URL": "http://x"})
	check(t, err != nil, "http public url")
	equal(t, []any{loaded.RuzICalURL, loaded.CalendarSecret, loaded.CalendarPublicSecret}, []any{"", "", ""})
	secret := "s3cr3t-calendar_link"
	loaded = must(load(map[string]string{"RUZ_ICAL_URL": " https://api.hseapp.ru/v3/ruz/cal/abc ",
		"CALENDAR_SECRET": secret}))
	equal(t, []any{loaded.RuzICalURL, loaded.CalendarSecret}, []any{"https://api.hseapp.ru/v3/ruz/cal/abc", secret})
	equal(t, must(load(map[string]string{"CALENDAR_PUBLIC_SECRET": secret})).CalendarPublicSecret, secret)
	for _, bad := range []map[string]string{{"RUZ_ICAL_URL": "api.hseapp.ru/cal"}, {"CALENDAR_SECRET": "short"},
		{"CALENDAR_SECRET": "with/slash-and-long-enough"}, {"CALENDAR_PUBLIC_SECRET": "short"},
		{"CALENDAR_SECRET": secret, "CALENDAR_PUBLIC_SECRET": secret}} {
		_, err := load(bad)
		check(t, err != nil, "accepted %v", bad)
	}
}

func TestActionsSubjects(t *testing.T) {
	f := setup(t)
	f.serve()
	sub := f.services.Subjects
	noErr(t, sub.TryResolve(context.Background(), []string{"Алгебра (рус)"}, "test"))

	body := f.get("/subjects").json(t)
	equal(t, body["subjects"], []any{map[string]any{"name": "Алгебра", "title": "Algebra", "abbr": "Alg",
		"manual": false}})
	equal(t, []any{body["error"], body["title_chars"], body["abbr_chars"]}, []any{nil, 13.0, 6.0})

	saved := f.put("/subjects", map[string]any{"name": "Алгебра", "title": "Linear Alg", "abbr": "LA"})
	equal(t, saved.json(t), map[string]any{"name": "Алгебра", "title": "Linear Alg", "abbr": "LA", "manual": true})
	for _, bad := range []map[string]any{{"name": "", "title": "X", "abbr": "X"},
		{"name": "Матан", "title": "Calculus", "abbr": "Calculus"}, {"name": "Матан", "title": "", "abbr": "C"},
		{"name": "Матан", "title": 5, "abbr": "C"}} {
		equal(t, f.put("/subjects", bad).status, 400)
	}
	check(t, strings.Contains(f.put("/subjects", map[string]any{"name": "Матан", "title": "Calculus",
		"abbr": "Calculus"}).text(), "the abbreviation"), "abbreviation")
	equal(t, f.put("/subjects", "[").status, 400)

	noErr(t, sub.TryResolve(context.Background(), []string{"Алгебра"}, "test"))
	equal(t, must(sub.Get("Алгебра")).Title, "Linear Alg") // Haiku is not asked about a manual entry
	equal(t, f.delete("/subjects", map[string]any{"name": "Алгебра"}).status, 200)
	check(t, must(sub.Get("Алгебра")) == nil, "deleted")
	equal(t, f.delete("/subjects", map[string]any{"name": "Алгебра"}).status, 404)
}

func toldEvent(f *fixture) int64 {
	return must(f.schedule.Events.Add(events.New{UID: "tg-1@infomaster", Source: "tg", MsgIDs: []int64{5},
		Course: "Алгебра", Title: "Alg consult", Kind: "consultation", Start: "2026-10-23T18:10:00", Room: "R205",
		Required: true}, f.clock.now()))
}

func items(body map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, item := range list(body["items"]) {
		out[obj(item)["uid"].(string)] = obj(item)
	}
	return out
}

func TestActionsAgenda(t *testing.T) {
	f := setup(t)
	f.syncFeed()
	f.serve()
	told := toldEvent(f)

	body := f.get("/agenda").json(t)
	equal(t, []any{body["today"], body["calendar"]}, []any{"2026-10-20", false})
	check(t, obj(body["ruz"])["error"] != nil, "ruz error")
	byUID := items(body)
	equal(t, []any{byUID["tg-1@infomaster"]["type"], byUID["tg-1@infomaster"]["end"]},
		[]any{"Consultation", "2026-10-23T19:30:00"})
	check(t, byUID["2000005@ruz.hse.ru"]["event"] == nil, "class without a correction")
	_, past := byUID["2000001@ruz.hse.ru"]
	check(t, !past, "Oct 19 is past")

	body = f.patch(fmt.Sprintf("/events/%d", told), map[string]any{"required": false, "hidden": true}).json(t)
	_, shown := items(body)["tg-1@infomaster"]
	check(t, !shown, "hidden")
	equal(t, obj(list(body["hidden"])[0])["id"], float64(told))
	ev := must(f.schedule.Events.Get(told))
	check(t, ev.Chosen && !ev.Required, "chosen, optional")

	fresh := map[string]any{"title": " Supervisor ", "kind": "Event", "start": "2026-10-24T15:00",
		"end": "2026-10-24T16:00", "room": "R205", "required": true}
	body = f.post("/events", fresh).json(t)
	var own map[string]any
	for _, item := range items(body) {
		if item["title"] == "Supervisor" {
			own = item
		}
	}
	equal(t, []any{own["start"], own["source"], obj(own["event"])["own"]}, []any{"2026-10-24T15:00:00", "own", true})
	ownID := int64(obj(own["event"])["id"].(float64))
	body = f.patch(fmt.Sprintf("/events/%d", ownID), map[string]any{"title": "Supervisor meeting"}).json(t)
	for _, item := range items(body) {
		if item["source"] == "own" {
			own = item
		}
	}
	equal(t, own["title"], "Supervisor meeting")
	response := f.delete(fmt.Sprintf("/events/%d", int64(obj(own["event"])["id"].(float64))), nil)
	equal(t, response.status, 200)
	for _, item := range items(response.json(t)) {
		check(t, item["source"] != "own", "own deleted")
	}
	equal(t, f.delete(fmt.Sprintf("/events/%d", told), nil).status, 404) // a chat's: hidden only

	fix := map[string]any{"class_uid": "2000005@ruz.hse.ru", "start": "2026-10-23T15:00", "end": "2026-10-23T16:20",
		"room": "R510", "cancelled": false}
	moved := items(f.post("/corrections", fix).json(t))["2000005@ruz.hse.ru"]
	event := obj(moved["event"])
	equal(t, []any{moved["start"], moved["location"], event["own"], obj(event["class"])["start"]},
		[]any{"2026-10-23T15:00:00", "R510", true, "2026-10-23T09:30:00"})
	equal(t, f.patch(fmt.Sprintf("/events/%d", int64(event["id"].(float64))), map[string]any{"title": "x"}).status, 400)

	for _, bad := range []any{map[string]any{}, map[string]any{"required": 1}, map[string]any{"title": ""},
		map[string]any{"hidden": true, "title": "x"}, map[string]any{"start": "2026-10-24T17:00"},
		map[string]any{"start": "2026-10-24T17:00", "end": "2026-10-24T16:00"}, map[string]any{"kind": ""}} {
		equal(t, f.patch(fmt.Sprintf("/events/%d", told), bad).status, 400)
	}
	with := func(key string, value any) map[string]any {
		out := map[string]any{}
		for k, v := range fresh {
			out[k] = v
		}
		out[key] = value
		return out
	}
	for _, bad := range []any{with("title", strings.Repeat("x", 121)), with("start", "tomorrow"),
		with("required", "yes")} {
		equal(t, f.post("/events", bad).status, 400)
	}
	fix["class_uid"] = "nope"
	equal(t, f.post("/corrections", fix).status, 404)
	equal(t, f.patch("/events/999", map[string]any{"hidden": false}).status, 404)
	equal(t, f.patch("/events/x", map[string]any{"hidden": false}).status, 404)
}

func TestActionsOneFormForEveryEntry(t *testing.T) {
	f := setup(t)
	f.syncFeed()
	f.serve()
	told := toldEvent(f)
	fix := map[string]any{"class_uid": "2000005@ruz.hse.ru", "title": "SE lab", "kind": "Lab",
		"start": "2026-10-23T09:30", "end": "2026-10-23T10:50", "room": "109", "required": false, "cancelled": false}
	changed := items(f.post("/corrections", fix).json(t))["2000005@ruz.hse.ru"]
	equal(t, []any{changed["title"], changed["type"], changed["required"], changed["start"], changed["room"]},
		[]any{"SE lab", "Lab", false, "2026-10-23T09:30:00", "109"})
	equal(t, changed["notes"], []any{"Changed by hand"})
	title := must(f.schedule.ClassTitle(must(f.schedule.Ruz.ByUID("2000005@ruz.hse.ru"))))
	fix["title"], fix["kind"], fix["required"], fix["room"] = title, "Seminar", true, "R301"
	changed = items(f.post("/corrections", fix).json(t))["2000005@ruz.hse.ru"]
	equal(t, must(f.schedule.Events.OwnCorrection("2000005@ruz.hse.ru")).Title, "")
	equal(t, []any{changed["title"], changed["type"], changed["location"], changed["notes"]},
		[]any{title, "Seminar", "R301", []any{"Room changed by hand"}})
	body := f.delete(fmt.Sprintf("/events/%d", int64(obj(changed["event"])["id"].(float64))), nil).json(t)
	check(t, items(body)["2000005@ruz.hse.ru"]["event"] == nil, "as it was")

	body = f.patch(fmt.Sprintf("/events/%d", told), map[string]any{"start": "2026-10-24", "end": nil,
		"cancelled": true}).json(t)
	var copied map[string]any
	for _, item := range items(body) {
		if item["source"] == "own" {
			copied = item
		}
	}
	event := obj(copied["event"])
	equal(t, []any{copied["start"], copied["end"], copied["cancelled"], event["copy"]},
		[]any{"2026-10-24", nil, true, true})
	equal(t, obj(event["original"])["start"], "2026-10-23T18:10:00")
	equal(t, body["hidden"], []any{})
	body = f.delete(fmt.Sprintf("/events/%d", int64(event["id"].(float64))), nil).json(t)
	equal(t, obj(items(body)["tg-1@infomaster"]["event"])["hidden"], false)

	fresh := map[string]any{"title": "Open day", "kind": "Event", "start": "2026-10-25", "end": nil, "room": "",
		"required": false}
	body = f.post("/events", fresh).json(t)
	for _, item := range items(body) {
		if item["title"] == "Open day" {
			equal(t, []any{item["start"], item["end"], item["required"]}, []any{"2026-10-25", nil, false})
		}
	}
	fresh["start"] = "2026-13-01"
	equal(t, f.post("/events", fresh).status, 400)
	fresh["start"], fresh["end"] = "2026-10-25", "2026-10-25T10:00"
	equal(t, f.post("/events", fresh).status, 400)
}

func TestActionsHidingACorrectionMovesItsMidterm(t *testing.T) {
	f := setup(t)
	must(f.schedule.Inbox.PutChat(-1001, "Матан", "supergroup", nil, 0))
	must(f.schedule.AfterPoll(context.Background(), *f.syncFeed()))
	var published []schedule.MidtermChanges
	f.schedule.OnMidterms = func(_ context.Context, changes schedule.MidtermChanges) error {
		published = append(published, changes)
		return nil
	}
	uid := "2000007@ruz.hse.ru" // the interim assessment, Thu Oct 29
	chat := int64(-1001)
	moved := must(f.schedule.Events.Add(events.New{UID: uid, ClassUID: uid, Source: "tg", ChatID: &chat,
		MsgIDs: []int64{5}, Course: "Введение в программирование", Title: "Intro to Prog",
		Kind: "Текущий рубежный контроль", Start: "2026-11-05T10:00:00", End: "2026-11-05T13:00:00", Room: "R401",
		Required: true}, f.clock.now()))
	must(f.schedule.SyncMidterms())
	f.serve()
	response := f.patch(fmt.Sprintf("/events/%d", moved), map[string]any{"hidden": true})
	equal(t, response.status, 200)
	item := items(response.json(t))[uid]
	check(t, obj(item["event"])["hidden"] == true && item["start"] == "2026-10-29T10:00:00", "moved back: %v", item)
	check(t, len(published) == 1, "published %v", published)
	back := published[0].Moved[0][1]
	equal(t, must(f.schedule.Deadlines.Get(back)).Due, "2026-10-29")
}

func TestActionsDeadlines(t *testing.T) {
	f := setup(t)
	f.services.Sources = append(twin(), api.Source{Name: "hseapp", Status: func() (any, error) {
		return map[string]any{"login_needed": true, "error": nil, "enabled": true}, nil
	}})
	f.serve()
	must(f.schedule.Inbox.PutChat(-1001, "Матан", "group", nil, 0))
	row := f.addRow("Calc HW 3", "2026-10-21", "tg", "Математический анализ")
	f.addRow("Phil essay", "2026-10-22", "mail", "Философия")
	body := f.get("/deadlines").json(t)
	equal(t, body["hseapp"], map[string]any{"login_needed": true, "error": nil})
	equal(t, body["today"], "2026-10-20")
	listed := list(body["items"])
	item, essay := obj(listed[0]), obj(listed[1])
	uid := must(f.merged.Items())[0].UID
	equal(t, item, map[string]any{"uid": uid, "title": "Calc HW 3", "due": "2026-10-21", "category": "study",
		"source": "tg", "course": "Математический анализ", "overdue": false, "assumed": false, "from": "Матан",
		"link": nil, "message": map[string]any{"chat": -1001.0, "msg": 1.0}})
	equal(t, []any{essay["from"], essay["link"]}, []any{"letter", nil})
	check(t, essay["message"] != nil, "a letter's message")
	equal(t, []any{body["own"], body["hidden"]}, []any{[]any{}, []any{}})
	f.merged.Deadlines.Drop([]int64{row + 1}, f.clock.now())

	for _, bad := range []map[string]any{{"title": "", "due": "2026-10-25", "category": "study"},
		{"title": "Gym", "due": "25.10", "category": "study"}, {"title": "Gym", "due": "2026-10-25", "category": "sport"},
		{"title": strings.Repeat("x", 201), "due": "2026-10-25", "category": "study"}} {
		equal(t, f.post("/deadlines", bad).status, 400)
	}
	body = f.post("/deadlines", map[string]any{"title": "Gym", "due": "2026-10-25", "category": "personal"}).json(t)
	equal(t, body["own"], []any{map[string]any{"uid": "ha-1", "title": "Gym", "due": "2026-10-25",
		"category": "personal"}})
	titles := func(b map[string]any) []any {
		out := []any{}
		for _, i := range list(b["items"]) {
			out = append(out, obj(i)["title"])
		}
		return out
	}
	equal(t, titles(body), []any{"Calc HW 3", "Gym"})
	equal(t, obj(list(f.patch("/deadlines/ha-1", map[string]any{"due": "2026-10-26"}).json(t)["own"])[0])["due"],
		"2026-10-26")
	equal(t, obj(list(f.patch("/deadlines/ha-1", map[string]any{"category": "study"}).json(t)["own"])[0])["category"],
		"study")
	equal(t, f.patch("/deadlines/ha-1", map[string]any{"category": "sport"}).status, 400)
	equal(t, f.patch("/deadlines/ha-1", map[string]any{}).status, 400)
	equal(t, f.patch("/deadlines/ha-9", map[string]any{"title": "x"}).status, 404)

	body = f.delete("/deadlines/"+uid, nil).json(t)
	equal(t, body["hidden"], []any{map[string]any{"uid": uid, "title": "Calc HW 3", "due": "2026-10-21",
		"source": "tg"}})
	equal(t, titles(body), []any{"Gym"})
	body = f.delete("/hidden/"+uid, nil).json(t)
	equal(t, body["hidden"], []any{})
	equal(t, titles(body), []any{"Calc HW 3", "Gym"})
	equal(t, f.delete("/hidden/"+uid, nil).status, 404)

	// An own one the merge leaves out as a copy of another is still the owner's to change.
	body = f.post("/deadlines", map[string]any{"title": "Calc HW 3", "due": "2026-10-21", "category": "study"}).json(t)
	equal(t, len(list(body["own"])), 2)
	body = f.delete("/deadlines/ha-2", nil).json(t)
	equal(t, obj(list(body["own"])[0])["uid"], "ha-1")
}

func TestActionsReminders(t *testing.T) {
	f := setup(t)
	f.services.Settings = f.settings
	r := reminders.New(f.store, f.merged, f.settings)
	f.services.Reminders = r
	f.serve()
	equal(t, f.get("/reminders").json(t), map[string]any{"enabled": true, "eve": "20:00", "day": "09:00"})
	equal(t, f.patch("/reminders", map[string]any{"eve": "21:30", "enabled": false}).json(t),
		map[string]any{"enabled": false, "eve": "21:30", "day": "09:00"})
	bad := f.patch("/reminders", map[string]any{"day": "8:00", "eve": "25:00"})
	equal(t, bad.status, 400)
	equal(t, must(f.settings.String("reminder_day")), "09:00") // nothing kept from a bad request
	equal(t, f.patch("/reminders", map[string]any{"x": 1}).status, 400)
}

func TestActionsMessage(t *testing.T) {
	f := setup(t)
	f.serve()
	in, now := f.schedule.Inbox, f.clock.now()
	chat := int64(-1001234567890)
	must(in.PutChat(chat, "Матан", "supergroup", nil, 0))
	must(in.Add(chat, 4, inbox.Fields{SenderID: ptr(int64(7)), SenderName: ptr("Студент"), At: now,
		Text: "А семинар будет?"}, "tg", time.Time{}))
	must(in.Add(chat, 5, inbox.Fields{SenderID: ptr(int64(8)), SenderName: ptr("Староста"), At: now,
		ReplyTo: ptr(int64(4)), Text: "Консультация завтра в S301",
		Files: []inbox.File{{"kind": "pdf", "name": "plan.pdf", "mime": "application/pdf", "size": 1}}}, "tg",
		time.Time{}))
	must(in.AddLetter(7, inbox.Fields{SenderName: ptr("Office"), At: now, Text: "Choose an elective",
		Subject: "Electives"}))

	body := f.get("/message?chat=-1001234567890&msg=5").json(t)
	equal(t, []any{body["source"], obj(body["chat"])["title"], body["link"], body["stored"]},
		[]any{"tg", "Матан", "https://t.me/c/1234567890/5", true})
	message := obj(body["message"])
	equal(t, []any{message["sender"], message["text"], message["files"]},
		[]any{"Староста", "Консультация завтра в S301", []any{map[string]any{"kind": "pdf", "name": "plan.pdf"}}})
	equal(t, obj(body["reply"])["text"], "А семинар будет?")

	body = f.get("/message?chat=0&msg=7").json(t)
	equal(t, []any{body["source"], body["subject"], body["link"], obj(body["message"])["text"]},
		[]any{"mail", "Electives", nil, "Choose an elective"})
	body = f.get("/message?chat=-1001234567890&msg=99").json(t)
	equal(t, []any{body["stored"], body["link"]}, []any{false, "https://t.me/c/1234567890/99"}) // after 14 days
	equal(t, f.get("/message?chat=-5&msg=1").status, 404)
	equal(t, f.get("/message?chat=x&msg=1").status, 400)
}

func TestActionsHseAppScreen(t *testing.T) { // test_hseapp.py: test_actions_screen, without a login server
	f := setup(t)
	f.services.Settings = f.settings
	f.services.Sources = append(twin(), api.Source{Name: "hseapp", Status: func() (any, error) {
		return f.services.HseApp.Status(), nil
	}})
	f.serve()
	body := f.get("/hseapp").json(t)
	equal(t, body["callback"], publicURL+"/hseapp/callback")
	check(t, obj(body["status"])["enabled"] != nil, "status")
	link := f.post("/hseapp/link", map[string]any{}).json(t)
	check(t, strings.Contains(link["url"].(string), "state="), "link %v", link)
	appLink := f.post("/hseapp/link", map[string]any{"app": true}).json(t)
	check(t, appLink["url"] != link["url"], "the app's link")
	equal(t, f.post("/hseapp/link", map[string]any{"app": "yes"}).status, 400)
	bad := f.post("/hseapp/login", map[string]any{"url": publicURL + "/hseapp/callback?state=x&code=y"})
	equal(t, bad.status, 400)
	check(t, strings.Contains(bad.text(), "out of date"), "stale state: %s", bad.text())
	equal(t, f.post("/hseapp/login", map[string]any{"url": " "}).status, 400)
	for _, wrong := range []any{map[string]any{}, map[string]any{"enabled": "no"},
		map[string]any{"enabled": true, "x": 1}} {
		equal(t, f.patch("/hseapp", wrong).status, 400)
	}
	body = f.patch("/hseapp", map[string]any{"enabled": false}).json(t)
	equal(t, obj(body["status"])["enabled"], false)
	// the callback of the public port: a page in the app's language, 400 for a state not known
	page := f.do("GET", "/hseapp/callback?state=forged&code=c", nil)
	equal(t, page.status, 400)
	check(t, strings.Contains(page.text(), "The login failed") && page.header.Get("Cache-Control") == "no-store",
		"page %s", page.text())
}

func TestActionsGrades(t *testing.T) { // test_grades.py: the /app/api half of test_api_and_miniapp, simplified
	f := setup(t)
	f.services.Settings = f.settings
	g := grades.New(f.store, f.settings, nil)
	g.Now = f.clock.now
	woken := 0
	g.Wake = func() { woken++ }
	f.services.Grades = g
	f.serve()
	body := f.get("/grades").json(t)
	equal(t, []any{body["sheets"], body["manual"], body["llm"], body["label_chars"]},
		[]any{[]any{}, []any{}, true, float64(settings.SheetLabelChars)})
	body = f.post("/grades/items", map[string]any{"course": "DM", "label": "Quiz 1", "value": 7}).json(t)
	mine := obj(list(body["manual"])[0])
	equal(t, []any{mine["course"], mine["label"], mine["value"]}, []any{"DM", "Quiz 1", 7.0})
	check(t, strings.Contains(f.get("/grades").text(), `"value": 7.0`), "7.0")
	id := mine["id"].(string)
	body = f.patch("/grades/items/"+id, map[string]any{"value": 9.456}).json(t)
	equal(t, obj(list(body["manual"])[0])["value"], 9.46)
	for _, bad := range []map[string]any{{"value": 11}, {"value": true}, {"value": "9"}, {"value": 5, "x": 1},
		{"label": "x"}, {"value": 5, "label": strings.Repeat("x", 41)}} {
		equal(t, f.patch("/grades/items/"+id, bad).status, 400)
	}
	for _, bad := range []map[string]any{{"course": "DM", "label": "x"}, {"course": "", "label": "x", "value": 1},
		{"course": strings.Repeat("x", 11), "label": "x", "value": 1}} {
		equal(t, f.post("/grades/items", bad).status, 400)
	}
	equal(t, f.patch("/grades/items/nope", map[string]any{"value": 5}).status, 404)
	body = f.delete("/grades/items/"+id, nil).json(t)
	equal(t, body["manual"], []any{})
	equal(t, f.delete("/grades/items/"+id, nil).status, 404)

	equal(t, f.patch("/grades", map[string]any{"sheets": []any{map[string]any{"url": "https://x.y/z"}}}).status,
		400)
	equal(t, f.patch("/grades", map[string]any{"pdf_pages": 3}).status, 400)
	url := "https://docs.google.com/spreadsheets/d/abc123/edit"
	body = f.patch("/grades", map[string]any{"sheets": []any{map[string]any{"url": url, "label": "DM"}},
		"window_days": 14}).json(t)
	equal(t, body["window_days"], 14.0)
	sheet := obj(list(body["sheets"])[0])
	equal(t, []any{sheet["id"], sheet["label"], sheet["name"], sheet["grades"]}, []any{"abc123", "DM", "", []any{}})

	// test_actions_gradebook
	book := f.get("/grades/sheets/abc123").json(t)
	equal(t, []any{book["id"], book["course"], book["label_budget"], book["items"], book["tabs"]},
		[]any{"abc123", "DM", 20.0, 0.0, []any{}})
	equal(t, f.get("/grades/sheets/zzz999").status, 404)
	book = f.patch("/grades/sheets/abc123", map[string]any{"name": "Discrete"}).json(t)
	equal(t, book["name"], "Discrete")
	equal(t, f.patch("/grades/sheets/abc123", map[string]any{"x": 1}).status, 400)
	before := woken
	equal(t, f.post("/grades/sheets/abc123/read", nil).status, 200)
	check(t, woken > before, "woken")
	layout := map[string]any{"skip": false, "works": []any{map[string]any{"column": "C", "label": " HW  1 ",
		"max": 10, "bonus_column": nil}}}
	equal(t, f.put("/grades/sheets/abc123/tabs/Sheet1", layout).status, 404) // no such tab before a read
	equal(t, f.put("/grades/sheets/abc123/tabs/Sheet1", map[string]any{"skip": false}).status, 400)
	equal(t, f.delete("/grades/sheets/abc123/tabs/Sheet1", nil).status, 404)
}

func TestPublicRoutes(t *testing.T) { // test_ical_feed.py: test_public_routes(_off)
	f := setup(t)
	feed := icalfeed.New(f.schedule)
	f.services.Calendar = feed
	f.serve()
	equal(t, f.do("GET", "/calendar/s3cr3t-calendar_link/main.ics", nil).status, 404) // no public secret: off
	f.services.Env.CalendarPublicSecret = "s3cr3t-calendar_link"
	f.serve()
	response := f.do("GET", "/calendar/s3cr3t-calendar_link/main.ics", nil)
	equal(t, response.status, 200)
	check(t, strings.HasPrefix(response.header.Get("Content-Type"), "text/calendar"), "calendar")
	equal(t, f.do("GET", "/calendar/s3cr3t-calendar_link/tablo.ics", nil).status, 404) // Tablo's stays inside
	equal(t, f.do("GET", "/calendar/wrong-secret-of-length/main.ics", nil).status, 404)
	unknown := f.do("GET", "/nope", nil)
	equal(t, []any{unknown.status, unknown.text()}, []any{404, "404: Not Found"})
	equal(t, f.do("POST", "/app/api/llm", nil).status, http.StatusMethodNotAllowed)
}

func TestFromISO(t *testing.T) {
	for _, good := range []string{"2026-10-22T10:00", "2026-10-22 10:00", "2026-10-22T10", "2026-10-22T10:00:05.5",
		"2026-10-22", "20261022T1000"} {
		if _, err := actions.ExportFromISO(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"tomorrow", "2026-13-01T10:00", "2026-10-22T25:00", "2026-10-22T", ""} {
		if _, err := actions.ExportFromISO(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	equal(t, actions.PyStr([]any{"a", int64(1), 2.0, nil, true}), "['a', 1, 2.0, None, True]")
	equal(t, actions.PyStr(httpx.Object{{Key: "k", Value: "it's"}}), `{'k': "it's"}`)
}
