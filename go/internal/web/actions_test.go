package web

// The web app's changes (tests/test_web_actions.py and test_web_news.py): «Done», «Not mine», editing, merging
// duplicates, confirming an assumed date, a course for a deadline without one, the «Check» queue, news with «Hide»,
// and the journal with its undo. The connector's catalog is not ported yet: the tools here are stubs over the real
// stores, enough to show the web app's side.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/connector/changes"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/news"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
)

// deadlineTools are stubs of the connector's deadline tools over the merged list.
func deadlineTools(t *testing.T, h *harness) {
	c := h.connector
	find := func(uid string) (*todo.Deadline, error) {
		d, err := h.app.Data.find(uid)
		if err == nil && d == nil {
			err = tools.Errorf("no such deadline: %s", uid)
		}
		return d, err
	}
	stub(t, c, "hide_deadline", []string{"uid", "reason", "kept"}, func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		d, err := find(a["uid"].(string))
		if err != nil {
			return tools.Output{}, err
		}
		if kept, ok := a["kept"].(string); ok {
			if kept == a["uid"] {
				return tools.Output{}, tools.Errorf("kept: a deadline cannot be a duplicate of itself")
			}
			if other, _ := h.app.Data.find(kept); other == nil {
				return tools.Output{}, tools.Errorf("kept: no such deadline")
			}
		}
		kept, _ := a["kept"].(string)
		if err := h.merged.Remove(*d, a["reason"].(string), kept); err != nil {
			return tools.Output{}, err
		}
		return tools.Output{Summary: "Marked «" + d.Title + "» as done",
			Undo: []tools.Step{{Tool: "unhide_deadline", Arguments: map[string]any{"uid": a["uid"]}}}}, nil
	})
	stub(t, c, "unhide_deadline", []string{"uid"}, func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		back, err := h.merged.Unhide(a["uid"].(string))
		if err == nil && !back {
			err = tools.Errorf("it is not hidden")
		}
		return tools.Output{Summary: "Restored a deadline",
			Undo: []tools.Step{{Tool: "hide_deadline", Arguments: map[string]any{"uid": a["uid"], "reason": "not_mine"}}}}, err
	})
	stub(t, c, "delete_deadline", []string{"uid"}, func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		d, err := find(a["uid"].(string))
		if err != nil {
			return tools.Output{}, err
		}
		if err := h.merged.Remove(*d, "", ""); err != nil {
			return tools.Output{}, err
		}
		return tools.Output{Summary: "Deleted «" + d.Title + "»", Undo: []tools.Step{{Tool: "_add_own",
			Arguments: map[string]any{"title": d.Title, "due": todo.DueText(d.Due), "category": d.Category}}}}, nil
	})
	c.Registry.Op("_add_own", func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		_, err := h.merged.Add(a["title"].(string), must(todo.ParseDate(a["due"].(string))), a["category"].(string), "")
		return tools.Output{}, err
	})
	stub(t, c, "edit_deadline", []string{"uid", "title", "due", "category"}, func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		d, err := find(a["uid"].(string))
		if err != nil {
			return tools.Output{}, err
		}
		var title *string
		if text, ok := a["title"].(string); ok {
			if strings.TrimSpace(text) == "" {
				return tools.Output{}, tools.Errorf("title: an empty title")
			}
			title = &text
		}
		var due time.Time
		if text, ok := a["due"].(string); ok {
			due = must(todo.ParseDate(text))
		}
		own, err := h.merged.Edit(*d, title, due, "")
		if err != nil {
			return tools.Output{}, err
		}
		return tools.Output{Summary: "Edited a deadline", Undo: []tools.Step{{Tool: "_unedit",
			Arguments: map[string]any{"own": own.Key, "uid": a["uid"]}}}}, nil
	})
	c.Registry.Op("_unedit", func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		own, err := find(a["own"].(string))
		if err != nil {
			return tools.Output{}, err
		}
		if err := h.merged.Remove(*own, "", ""); err != nil {
			return tools.Output{}, err
		}
		_, err = h.merged.Unhide(a["uid"].(string))
		return tools.Output{}, err
	})
	row := func(uid string) (*deadlines.Deadline, error) {
		d, err := find(uid)
		if err != nil {
			return nil, err
		}
		return h.app.Data.row(*d)
	}
	stub(t, c, "confirm_deadline_date", []string{"uid"}, func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		r, err := row(a["uid"].(string))
		if err != nil {
			return tools.Output{}, err
		}
		if r == nil || !r.Assumed {
			return tools.Output{}, tools.Errorf("the date is not assumed")
		}
		return tools.Output{Summary: "Confirmed a date", Undo: []tools.Step{{Tool: "_assume_again",
			Arguments: map[string]any{"id": r.ID}}}}, h.sch.Deadlines.Confirm(r.ID)
	})
	c.Registry.Op("_assume_again", func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		_, err := h.store.DB.Exec("update deadlines set assumed = 1 where id = ?", a["id"])
		return tools.Output{}, err
	})
	stub(t, c, "set_deadline_course", []string{"uid", "course"}, func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		r, err := row(a["uid"].(string))
		if err != nil {
			return tools.Output{}, err
		}
		course := a["course"].(string)
		return tools.Output{Summary: "Set a course", Undo: []tools.Step{{Tool: "_rename",
			Arguments: map[string]any{"id": r.ID, "label": r.Label}}}}, h.sch.Deadlines.Rename(r.ID, r.Label, &course)
	})
	c.Registry.Op("_rename", func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		return tools.Output{}, h.sch.Deadlines.Rename(a["id"].(int64), a["label"].(string), nil)
	})
	stub(t, c, "add_deadline", []string{"title", "due", "category", "course"}, func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		title, _ := a["title"].(string)
		if strings.TrimSpace(title) == "" {
			return tools.Output{}, tools.Errorf("title: an empty title")
		}
		course, _ := a["course"].(string)
		d, err := h.merged.Add(title, must(todo.ParseDate(a["due"].(string))), a["category"].(string), course)
		if err != nil {
			return tools.Output{}, err
		}
		return tools.Output{Summary: "Added a deadline", Undo: []tools.Step{{Tool: "delete_deadline",
			Arguments: map[string]any{"uid": d.Key}}}}, nil
	})
}

func actor(t *testing.T) *harness {
	h := newHarness(t, options{screens: true, connector: true, data: func(h *harness, d *Data) {
		must(h.sch.Subjects.SetManual("Discrete Mathematics", "Discrete Mathematics", "DM"))
		must(h.sch.Subjects.SetManual("Algebra", "Algebra", "Alg"))
	}})
	deadlineTools(t, h)
	return h.serve().in()
}

func titles(feed map[string]any) []string {
	out := []string{}
	for _, day := range list(feed["days"]) {
		for _, item := range list(obj(day)["items"]) {
			if obj(item)["kind"] == "deadline" {
				out = append(out, obj(item)["title"].(string))
			}
		}
	}
	return out
}

func (h *harness) item(title string) todo.Deadline {
	for _, d := range must(h.merged.All()) {
		if d.Title == title {
			return d
		}
	}
	h.t.Fatalf("no %s", title)
	return todo.Deadline{}
}

func (h *harness) undo(change any) map[string]any {
	id := obj(change)["id"].(int64)
	return h.ask("POST", "/changes/"+strconv.FormatInt(id, 10)+"/undo", map[string]any{}, 200)
}

func TestDoneNotMineAndUndo(t *testing.T) {
	h := actor(t)
	h.addRow("Sheet 5", "2026-10-22", "", "Discrete Mathematics")
	h.addRow("Quiz 2", "2026-10-23", "", "Algebra")
	sheet, quiz := todo.UID(h.item("Sheet 5")), todo.UID(h.item("Quiz 2"))
	done := h.ask("POST", "/deadlines/done", map[string]any{"uid": sheet}, 200)
	change := obj(done["change"])
	if change["undo"] != true || !strings.Contains(change["summary"].(string), "as done") || change["note"] != "" {
		t.Fatal(done)
	}
	h.ask("POST", "/deadlines/not-mine", map[string]any{"uid": quiz}, 200)
	reasons := map[string]string{}
	for _, row := range must(h.merged.Todo.HiddenRows()) {
		reasons[row.Title] = deref(row.Reason)
	}
	if reasons["Sheet 5"] != "done" || reasons["Quiz 2"] != "not_mine" {
		t.Fatal(reasons)
	}
	if len(titles(h.read("/feed"))) != 0 || len(must(h.merged.Items())) != 0 {
		t.Fatal("hidden")
	}
	undone := h.undo(done["change"])
	if obj(undone["change"])["undone_at"] == nil {
		t.Fatal(undone)
	}
	if got := titles(h.read("/feed")); strings.Join(got, "|") != "Sheet 5" {
		t.Fatal(got)
	}
	again := h.ask("POST", "/changes/"+strconv.FormatInt(change["id"].(int64), 10)+"/undo", map[string]any{}, 400)
	if !strings.Contains(again["error"].(string), "undone already") {
		t.Fatal(again)
	}
	// An own deadline is deleted, and its undo adds it back.
	own := must(h.merged.Add("Dentist", todo.Date(2026, 10, 24), "personal", ""))
	gone := h.ask("POST", "/deadlines/done", map[string]any{"uid": own.Key}, 200)
	if len(must(h.merged.Todo.Own())) != 0 {
		t.Fatal("deleted")
	}
	h.undo(gone["change"])
	if owned := must(h.merged.Todo.Own()); len(owned) != 1 || owned[0].Title != "Dentist" {
		t.Fatal(owned)
	}
	for _, c := range must(h.connector.Changes.Recent(100, "")) {
		if c.Via != changes.ViaWeb {
			t.Fatal(c)
		}
	}
	if h.ask("POST", "/deadlines/done", map[string]any{"uid": "nothing"}, 404)["error"] == nil {
		t.Fatal("nothing")
	}
	if h.ask("POST", "/deadlines/later", map[string]any{"uid": sheet}, 404)["error"] != "no such action" {
		t.Fatal("later")
	}
	if h.call("POST", "/deadlines/done", map[string]any{"uid": sheet}, req{token: h.token, origin: noOrigin()}).Code != 403 {
		t.Fatal("no Origin of the app")
	}
}

func TestEdit(t *testing.T) {
	h := actor(t)
	h.addRow("Sheet 5", "2026-10-22", "", "Discrete Mathematics")
	sheet := todo.UID(h.item("Sheet 5"))
	edited := h.ask("POST", "/deadlines/edit", map[string]any{"uid": sheet, "title": "Sheet 5 (both parts)",
		"due": "2026-10-24"}, 200)
	owned := must(h.merged.Todo.Own())
	if len(owned) != 1 || owned[0].Title != "Sheet 5 (both parts)" || todo.DueText(owned[0].Due) != "2026-10-24" {
		t.Fatal(owned)
	}
	if got := titles(h.read("/feed")); strings.Join(got, "|") != "Sheet 5 (both parts)" {
		t.Fatal(got)
	}
	h.undo(edited["change"])
	if got := titles(h.read("/feed")); strings.Join(got, "|") != "Sheet 5" || len(must(h.merged.Todo.Own())) != 0 {
		t.Fatal(got)
	}
	if r := h.ask("POST", "/deadlines/edit", map[string]any{"uid": sheet, "title": " "}, 400); !strings.Contains(r["error"].(string), "title") {
		t.Fatal(r)
	}
}

func TestDuplicates(t *testing.T) {
	h := actor(t)
	h.addRow("Sheet 12", "2026-10-26", "", "Discrete Mathematics")
	h.addRow("HW 12", "2026-10-26", "mail", "Discrete Mathematics")
	h.addRow("Essay", "2026-10-26", "", "Discrete Mathematics") // no number: another work
	h.addRow("Quiz 12", "2026-10-26", "", "Algebra")            // another course
	sheet, hw := todo.UID(h.item("Sheet 12")), todo.UID(h.item("HW 12"))
	questions := list(h.read("/feed")["check"])
	if len(questions) != 1 {
		t.Fatal(questions)
	}
	question := obj(questions[0])
	pair := list(question["items"])
	if question["kind"] != "duplicate" || question["course"] != "DM" || question["keep"] != hw ||
		obj(pair[0])["title"] != "HW 12" || obj(pair[1])["title"] != "Sheet 12" {
		t.Fatal(question) // the letter is more reliable than the chat
	}
	if len(list(h.read("/feed?course=Alg")["check"])) != 0 {
		t.Fatal("another lens")
	}
	card := h.read("/deadline?uid=" + sheet)
	twins := []string{}
	for _, twin := range list(card["twins"]) {
		twins = append(twins, obj(twin)["title"].(string))
	}
	if strings.Join(twins, "|") != "HW 12|Essay" || card["own"] != false {
		t.Fatal(twins, card["own"])
	}
	merged := h.ask("POST", "/deadlines/merge", map[string]any{"uid": sheet, "keep": hw}, 200)
	rows := must(h.merged.Todo.HiddenRows())
	if len(rows) != 1 || rows[0].Title != "Sheet 12" || deref(rows[0].Reason) != "duplicate" || deref(rows[0].Kept) != hw {
		t.Fatal(rows)
	}
	if len(list(h.read("/check")["items"])) != 0 {
		t.Fatal("merged")
	}
	h.undo(merged["change"])
	if items := list(h.read("/check")["items"]); len(items) != 1 || obj(items[0])["id"] != question["id"] {
		t.Fatal(items)
	}
	// «They are different»: not asked again, until it is undone.
	aside := h.ask("POST", "/check/put-aside", map[string]any{"id": question["id"]}, 200)
	if change := obj(aside["change"]); change["summary"] != "Marked two deadlines as different" || change["undo"] != true {
		t.Fatal(aside)
	}
	if len(list(h.read("/check")["items"])) != 0 {
		t.Fatal("put aside")
	}
	h.undo(aside["change"])
	if len(list(h.read("/check")["items"])) != 1 {
		t.Fatal("asked again")
	}
	h.ask("POST", "/check/put-aside", map[string]any{"id": "dup:x|y"}, 404)
	h.ask("POST", "/deadlines/merge", map[string]any{"uid": sheet, "keep": sheet}, 400)
	h.ask("POST", "/deadlines/merge", map[string]any{"uid": sheet, "keep": "nothing"}, 400)
}

func TestLikelySame(t *testing.T) {
	d := func(title, due, source string, assumed bool) todo.Deadline {
		return todo.Deadline{Title: title, Due: must(todo.ParseDate(due)), Category: "study", Source: source,
			Course: ptr("DM"), Assumed: assumed}
	}
	if !LikelySame(d("Sheet 12", "2026-10-26", "tg", false), d("HW 12", "2026-10-30", "tg", false)) ||
		LikelySame(d("Sheet 12", "2026-10-26", "tg", false), d("HW 12", "2026-11-03", "tg", false)) ||
		LikelySame(d("Sheet 12", "2026-10-26", "tg", false), d("HW 13", "2026-10-26", "tg", false)) ||
		!LikelySame(d("Midterm", "2026-10-26", "tg", false), d("Midterm retake", "2026-10-28", "tg", false)) ||
		LikelySame(d("HW 1", "2026-10-26", "hseapp", false), d("HW 1 bonus", "2026-10-26", "hseapp", false)) {
		t.Fatal("likely same")
	}
	real, guessed := d("Sheet 12", "2026-10-26", "tg", false), d("HW 12", "2026-10-26", "mail", true)
	if Stays(guessed, real).Title != real.Title {
		t.Fatal("a real date over an assumed one, then the source")
	}
}

func TestAssumedDate(t *testing.T) {
	h := actor(t)
	must(h.sch.Deadlines.Add(deadlines.New{Source: "tg", ChatID: ptr(testChat), MsgIDs: []int64{1},
		Course: "Discrete Mathematics", Label: "DM sheet 3", Due: "2026-10-22", Assumed: true}, h.clock.now()))
	sheet := todo.UID(h.item("DM sheet 3"))
	questions := list(h.read("/check")["items"])
	question := obj(questions[0])
	if len(questions) != 1 || question["id"] != "assumed:"+sheet || question["kind"] != "assumed" ||
		question["course"] != "DM" || obj(question["item"])["assumed"] != true || len(question) != 4 {
		t.Fatal(questions)
	}
	confirmed := h.ask("POST", "/deadlines/confirm", map[string]any{"uid": sheet}, 200)
	if h.item("DM sheet 3").Assumed || len(list(h.read("/check")["items"])) != 0 {
		t.Fatal("confirmed")
	}
	h.undo(confirmed["change"])
	if !h.item("DM sheet 3").Assumed {
		t.Fatal("assumed again")
	}
	h.addRow("Quiz 2", "2026-10-23", "", "Algebra")
	h.ask("POST", "/deadlines/confirm", map[string]any{"uid": todo.UID(h.item("Quiz 2"))}, 400)
}

func TestCourseOfADeadline(t *testing.T) {
	h := actor(t)
	h.addRow("AI Challenge", "2026-10-25", "", "")
	challenge := todo.UID(h.item("AI Challenge"))
	check := h.read("/check")
	question := obj(list(check["items"])[0])
	if question["kind"] != "course" || question["id"] != "course:"+challenge || question["course"] != nil {
		t.Fatal(question)
	}
	if obj(list(check["courses"])[0])["key"] != "Alg" || check["today"] != "2026-10-20" {
		t.Fatal(check["courses"])
	}
	changed := h.ask("POST", "/deadlines/course", map[string]any{"uid": challenge, "course": "DM"}, 200)
	r := must(h.app.Data.row(h.item("AI Challenge")))
	if deref(r.Course) != "Discrete Mathematics" {
		t.Fatal(r.Course)
	}
	courses := []string{}
	for _, day := range list(h.read("/feed?course=DM")["days"]) {
		for _, item := range list(obj(day)["items"]) {
			if obj(item)["kind"] == "deadline" {
				courses = append(courses, obj(item)["course"].(string))
			}
		}
	}
	if strings.Join(courses, ",") != "DM" || len(list(h.read("/check")["items"])) != 0 {
		t.Fatal(courses)
	}
	h.undo(changed["change"])
	if must(h.sch.Deadlines.Get(r.ID)).Course != nil {
		t.Fatal("undone")
	}
	h.ask("POST", "/deadlines/course", map[string]any{"uid": challenge, "course": "Nope"}, 400)
	aside := h.ask("POST", "/check/put-aside", map[string]any{"id": question["id"]}, 200) // «Outside the courses»
	if obj(aside["change"])["summary"] != "Left a deadline outside the courses" || len(list(h.read("/check")["items"])) != 0 {
		t.Fatal(aside)
	}
}

func TestWithoutConnector(t *testing.T) {
	h := newHarness(t, options{screens: true}).serve()
	if r := h.call("POST", "/deadlines/done", map[string]any{"uid": "x"}, req{token: "x"}); r.Code != 401 {
		t.Fatal("the session first")
	}
	h.in()
	if r := h.ask("POST", "/deadlines/done", map[string]any{"uid": "x"}, 503); r["error"] != "changes are not wired" {
		t.Fatal(r)
	}
}

// --- news (stage 16.7) -------------------------------------------------------------------------------------------

func newsReader(t *testing.T) *harness {
	h := newHarness(t, options{screens: true, connector: true, data: func(h *harness, d *Data) {
		d.News = h.news
		must(h.sch.Inbox.PutChat(testChat, "Course chat", "supergroup", ptr("course_chat"), 0))
		at := h.clock.now().Add(-time.Hour)
		must(h.sch.Inbox.Add(testChat, 11, inboxFields("Assistant", "The test moves to Thursday, 13:00", at), "tg",
			time.Time{}))
	}})
	h.secs.value = float64(h.clock.now().Unix())
	stub(t, h.connector, "hide_news", []string{"news_id"}, func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		id := a["news_id"].(int64)
		item := must(h.news.Get(id))
		if item == nil {
			return tools.Output{}, tools.Errorf("no such news item")
		}
		hidden := must(h.news.Hide(id))
		if !hidden {
			return tools.Output{}, tools.Errorf("it is hidden already")
		}
		return tools.Output{Summary: "Hid «" + item.Short + "»", Undo: []tools.Step{{Tool: "_unhide_news",
			Arguments: map[string]any{"id": id}}}}, nil
	})
	h.connector.Registry.Op("_unhide_news", func(_ *tools.Context, a map[string]any) (tools.Output, error) {
		_, err := h.news.Unhide(a["id"].(int64))
		return tools.Output{}, err
	})
	return h.serve().in()
}

func (h *harness) addNews(chatID, msgID int64, full string, importance int64, study bool, hours int, source string) int64 {
	short := full
	if len([]rune(short)) > 20 {
		short = string([]rune(short)[:20])
	}
	return must(h.news.Add(news.New{Source: source, ChatID: chatID, MsgIDs: []int64{msgID}, Study: study,
		PostedAt: clock.LocalISO(h.clock.now().Add(-time.Duration(hours) * time.Hour)), Full: full, Short: short,
		Importance: importance}, h.clock.now()))
}

func ids(items any) []int64 {
	out := []int64{}
	for _, item := range list(items) {
		out = append(out, obj(item)["id"].(int64))
	}
	return out
}

func TestNewsListCardAndHide(t *testing.T) {
	h := newsReader(t)
	test := h.addNews(testChat, 11, "The test moves to Thursday, 13:00", 5, true, 1, "tg")
	party := h.addNews(testChat, 12, "Board games on Friday", 2, false, 1, "tg")
	letter := h.addNews(inbox.Mailbox, 3, "Minor applications until 25 November", 4, true, 3, "mail")
	h.addNews(testChat, 13, "Yesterday's news", 3, true, 30, "tg") // older than news_ttl_hours: not live
	listed := h.read("/news")
	if listed["ttl_hours"] != int64(24) || !equalIDs(ids(listed["items"]), test, letter, party) {
		t.Fatal(listed) // importance + study, as Tablo has them
	}
	first, second := obj(list(listed["items"])[0]), obj(list(listed["items"])[1])
	if from := obj(first["from"]); from["kind"] != "supergroup" || from["name"] != "Course chat" {
		t.Fatal(from)
	}
	if from := obj(second["from"]); from["kind"] != "mail" || from["name"] != nil {
		t.Fatal(from) // the letter is not kept
	}
	if _, has := h.read("/feed")["news"]; has {
		t.Fatal("the News view carries no count")
	}
	card := h.read("/news/" + strconv.FormatInt(test, 10))
	quote := obj(card["quote"])
	if quote["text"] != "The test moves to Thursday, 13:00" || quote["sender"] != "Assistant" ||
		card["link"] != "https://t.me/course_chat/11" || card["live"] != true {
		t.Fatal(card)
	}
	if r := h.ask("GET", "/news/999", nil, 404); r["error"] != "no such news item" {
		t.Fatal(r)
	}
	hidden := h.ask("POST", "/news/"+strconv.FormatInt(party, 10)+"/hide", map[string]any{}, 200)
	if obj(hidden["change"])["undo"] != true || !strings.Contains(obj(hidden["change"])["summary"].(string), "Board games") {
		t.Fatal(hidden)
	}
	if !equalIDs(ids(h.read("/news")["items"]), test, letter) {
		t.Fatal("hidden")
	}
	if r := h.ask("POST", "/news/"+strconv.FormatInt(party, 10)+"/hide", map[string]any{}, 400); r["error"] != "it is hidden already" {
		t.Fatal(r)
	}
	h.ask("GET", "/news/"+strconv.FormatInt(party, 10), nil, 404)
	if recent := must(h.connector.Changes.Recent(1, "")); recent[0].Via != "web" {
		t.Fatal(recent)
	}
	h.undo(hidden["change"])
	if !equalIDs(ids(h.read("/news")["items"]), test, letter, party) {
		t.Fatal("back")
	}
}

func equalIDs(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
