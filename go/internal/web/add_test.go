package web

// «+» (tests/test_web_add.py): material at the class in progress, one line read by the model and confirmed, a
// deadline, an event or a grade by hand; every one journaled with its undo.

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
)

var png = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

// model is Haiku as the tests want it: the answer given, the prompt kept.
type model struct {
	mu        sync.Mutex
	available bool
	answer    map[string]any
	prompts   []string
	refuse    error
}

func (m *model) Available() bool { return m.available }

func (m *model) JSON(_ context.Context, r llm.Request) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Task != "quick" || !strings.Contains(r.System, "data from the student") || r.MaxTokens != 600 {
		panic("the quick task")
	}
	m.prompts = append(m.prompts, r.Prompt)
	if m.refuse != nil {
		return nil, m.refuse
	}
	return json.Marshal(m.answer)
}

type adder struct {
	*harness
	lib   *library.Library
	model *model
}

func newAdder(t *testing.T) *adder {
	a := &adder{model: &model{available: true}}
	h := newHarness(t, options{lang: "ru", screens: true, connector: true, model: a.model, data: func(h *harness, d *Data) {
		must(h.sch.Subjects.SetManual("Algebra", "Algebra", "Alg"))
		must(h.sch.Subjects.SetManual("Discrete Mathematics", "Discrete Mathematics", "DM"))
		h.clock.set(13, 30) // during Algebra (13:00–14:20)
		a.lib = must(library.New(h.store, t.TempDir()))
		a.lib.Now = h.clock.now
		a.lib.SameCourse = func(x, y string) bool { return must(h.sch.SameCourse(x, y)) }
		d.Library = a.lib
	}})
	a.harness = h
	deadlineTools(t, h)
	stub(t, h.connector, "remove_material", []string{"id", "ids"}, func(_ *tools.Context, args map[string]any) (tools.Output, error) {
		id := args["id"].(int64)
		return tools.Output{Summary: "Removed", Undo: []tools.Step{{Tool: "_restore_material",
			Arguments: map[string]any{"id": id}}}}, a.lib.Remove(id)
	})
	h.connector.Registry.Op("_restore_material", func(_ *tools.Context, args map[string]any) (tools.Output, error) {
		return tools.Output{}, a.lib.Restore(args["id"].(int64))
	})
	stub(t, h.connector, "add_event", []string{"title", "start", "end", "room", "kind"}, func(c *tools.Context, args map[string]any) (tools.Output, error) {
		text := func(name string) string { s, _ := args[name].(string); return s }
		id, err := h.sch.AddEvent(schedule.Fields{Title: text("title"), Kind: text("kind"), Start: text("start") + ":00",
			End: text("end") + ":00", Room: text("room"), Required: true})
		return tools.Output{Summary: "Added an event", Undo: []tools.Step{{Tool: "_remove_event",
			Arguments: map[string]any{"id": id}}}}, err
	})
	h.connector.Registry.Op("_remove_event", func(c *tools.Context, args map[string]any) (tools.Output, error) {
		_, err := h.sch.RemoveOwn(c.Ctx, args["id"].(int64))
		return tools.Output{}, err
	})
	h.serve().in()
	return a
}

type formPart struct {
	name, file, contentType string
	data                    []byte
}

func (a *adder) upload(path string, parts []formPart, status int) map[string]any {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, p := range parts {
		if p.file == "" {
			noErr(a.t, writer.WriteField(p.name, string(p.data)))
			continue
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="`+p.name+`"; filename="`+p.file+`"`)
		header.Set("Content-Type", p.contentType)
		part := must(writer.CreatePart(header))
		must(part.Write(p.data))
	}
	noErr(a.t, writer.Close())
	request := httptest.NewRequest("POST", "/web/api"+path, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", testOrigin)
	request.Header.Set("User-Agent", iphone)
	request.Header.Set("Cookie", Cookie+"="+a.token)
	recorder := httptest.NewRecorder()
	a.handler.ServeHTTP(recorder, request)
	if recorder.Code != status {
		a.t.Fatalf("%s: %d, want %d: %s", path, recorder.Code, status, recorder.Body)
	}
	return obj(decode(a.t, recorder))
}

func TestPlace(t *testing.T) {
	a := newAdder(t)
	state := a.read("/add")
	current := obj(state["current"])
	if current["title"] != "Algebra" || current["course"] != "Alg" {
		t.Fatal(current)
	}
	classes := list(state["classes"])
	if obj(classes[0])["title"] != "Algebra" || obj(classes[1])["title"] != "Математический анализ" {
		t.Fatal(classes) // the newest first
	}
	if state["line"] != true || state["library"] != true || state["grades"] != false {
		t.Fatal(state)
	}
	found := false
	for _, c := range list(state["courses"]) {
		found = found || obj(c)["key"] == "Alg" && obj(c)["title"] == "Algebra"
	}
	if !found {
		t.Fatal(state["courses"])
	}
	a.clock.set(15, 40) // Algebra ended at 14:20: 80 minutes ago, still the class in progress
	if obj(a.read("/add")["current"])["title"] != "Algebra" {
		t.Fatal("still")
	}
	a.clock.set(16, 0)
	if a.read("/add")["current"] != nil {
		t.Fatal("over")
	}
}

func TestFilesToTheClassAndUndo(t *testing.T) {
	a := newAdder(t)
	uid := obj(a.read("/add")["current"])["uid"].(string)
	added := a.upload("/add/files", []formPart{{name: "class_uid", data: []byte(uid)},
		{name: "file", file: "board.png", contentType: "image/png", data: png},
		{name: "file", file: "talk.m4a", contentType: "audio/mp4", data: append([]byte("ID3"), make([]byte, 64)...)}}, 200)
	kinds := []string{}
	for _, id := range list(added["added"]) {
		item := must(a.lib.Get(id.(int64), false, false))
		if deref(item.Course) != "Algebra" || deref(item.ClassUID) != uid || item.Source != "web" || item.Author != "owner" {
			t.Fatal(item)
		}
		kinds = append(kinds, item.Kind)
	}
	if strings.Join(kinds, ",") != "photo,audio" || obj(added["change"])["undo"] != true {
		t.Fatal(kinds, added)
	}
	a.undo(added["change"])
	for _, id := range list(added["added"]) {
		if must(a.lib.Get(id.(int64), false, false)) != nil {
			t.Fatal("removed, back for 30 days")
		}
	}
	inbox := a.upload("/add/files", []formPart{{name: "file", file: "x.png", contentType: "image/png", data: png}}, 200)
	if item := must(a.lib.Get(list(inbox["added"])[0].(int64), false, false)); item.Course != nil {
		t.Fatal("the library's inbox")
	}
	a.upload("/add/files", []formPart{{name: "class_uid", data: []byte("nothing")},
		{name: "file", file: "x.png", contentType: "image/png", data: png}}, 400)
	if r := a.upload("/add/files", []formPart{{name: "file", file: "empty.png", contentType: "image/png"}}, 400); !strings.Contains(r["error"].(string), "empty") {
		t.Fatal(r)
	}
	a.upload("/add/files", []formPart{{name: "file", file: "x.png", contentType: "image/png", data: png},
		{name: "course", data: []byte("Alg")}}, 400) // the place comes first
	if r := a.upload("/add/files", []formPart{{name: "course", data: []byte("Alg")}}, 400); r["error"] != "Выберите файл." {
		t.Fatal(r)
	}
	if r := a.upload("/add/files", []formPart{{name: "course", data: bytes.Repeat([]byte("x"), 401)}}, 400); r["error"] != "course: up to 400 bytes" {
		t.Fatal(r)
	}
	// a later file refused: the ones before it stay, journaled
	partial := a.upload("/add/files", []formPart{{name: "file", file: "x.png", contentType: "image/png", data: png},
		{name: "file", file: "empty.png", contentType: "image/png"}}, 207)
	if len(list(partial["added"])) != 1 || !strings.Contains(partial["error"].(string), "empty") {
		t.Fatal(partial)
	}
}

func TestNotesAndLinks(t *testing.T) {
	a := newAdder(t)
	note := a.ask("POST", "/add/note", map[string]any{"text": "Лемма 3 — на зачёте", "course": "Alg"}, 200)
	item := must(a.lib.Get(list(note["added"])[0].(int64), false, false))
	if item.Kind != "text" || deref(item.Course) != "Algebra" || item.ClassUID != nil || deref(item.Text) != "Лемма 3 — на зачёте" {
		t.Fatal(item)
	}
	link := a.ask("POST", "/add/note", map[string]any{"text": "https://example.com/slides"}, 200)
	item = must(a.lib.Get(list(link["added"])[0].(int64), false, false))
	if item.Kind != "link" || deref(item.URL) != "https://example.com/slides" || item.Course != nil {
		t.Fatal(item)
	}
	a.ask("POST", "/add/note", map[string]any{"text": " "}, 400)
	a.ask("POST", "/add/note", map[string]any{"text": "x", "course": "Nope"}, 400)
}

func TestOneLine(t *testing.T) {
	a := newAdder(t)
	a.model.answer = map[string]any{"kind": "deadline", "title": "Лаба 3, 18:00", "course": "Alg", "due": "2026-10-21",
		"start": "", "end": "", "room": "", "category": "study", "why": "«До среды» — ближайшая среда."}
	r := a.call("POST", "/add/line", map[string]any{"text": "лаба 3 по алгебре до среды 18:00"}, req{token: a.token})
	understood := obj(decode(t, r))
	want := map[string]any{"kind": "deadline", "title": "Лаба 3, 18:00", "course": "Alg", "course_title": "Algebra",
		"due": "2026-10-21", "start": nil, "end": nil, "room": "", "category": "study",
		"why": "«До среды» — ближайшая среда.", "line": "лаба 3 по алгебре до среды 18:00"}
	if must(json.Marshal(understood)) == nil || string(must(json.Marshal(understood))) != string(must(json.Marshal(want))) ||
		!strings.HasPrefix(r.Body.String(), `{"kind": "deadline", "title": "\u041b`) {
		t.Fatal(r.Body.String())
	}
	prompt := a.model.prompts[len(a.model.prompts)-1]
	if !strings.Contains(prompt, "Today: 2026-10-20, Tuesday, 13:30") || !strings.Contains(prompt, "- Alg: Algebra") ||
		!strings.Contains(prompt, "2026-10-22 Thu 16:40 DM Seminar") ||
		!strings.HasSuffix(prompt, "The line:\nлаба 3 по алгебре до среды 18:00") {
		t.Fatal(prompt)
	}
	if len(must(a.merged.Todo.Own())) != 0 {
		t.Fatal("nothing saved before the owner confirms")
	}
	saved := a.ask("POST", "/add/deadline", map[string]any{"title": understood["title"], "due": understood["due"],
		"course": "Alg"}, 200)
	owned := must(a.merged.Todo.Own())
	if len(owned) != 1 || owned[0].Title != "Лаба 3, 18:00" || deref(owned[0].Course) != "Alg" {
		t.Fatal(owned)
	}
	if got := titles(a.read("/feed?course=Alg")); strings.Join(got, "|") != "Лаба 3, 18:00" {
		t.Fatal(got)
	}
	a.undo(saved["change"])
	if len(must(a.merged.Todo.Own())) != 0 {
		t.Fatal("undone")
	}
	// What the model may not decide: an unknown course, a bad date, a deadline without one.
	a.model.answer["course"], a.model.answer["due"] = "Biology", "2026-02-30"
	odd := a.ask("POST", "/add/line", map[string]any{"text": "что-то"}, 200)
	if odd["kind"] != "note" || odd["course"] != nil || odd["due"] != nil {
		t.Fatal(odd)
	}
	a.model.refuse = &llm.BudgetExceeded{Msg: "spent"}
	if r := a.ask("POST", "/add/line", map[string]any{"text": "x"}, 429); !strings.Contains(r["error"].(string), "лимит") {
		t.Fatal(r)
	}
	a.model.refuse = llm.NewError(llm.Failed, "overloaded")
	if r := a.ask("POST", "/add/line", map[string]any{"text": "x"}, 502); r["error"] != "Строку не разобрали: failed: overloaded" {
		t.Fatal(r)
	}
	a.ask("POST", "/add/line", map[string]any{"text": "  "}, 400)
	a.model.available = false
	if r := a.ask("POST", "/add/line", map[string]any{"text": "x"}, 503); !strings.Contains(r["error"].(string), "Anthropic") {
		t.Fatal(r)
	}
}

func TestCheckedEvent(t *testing.T) {
	a := newAdder(t)
	quick := a.app.Quick
	answer := map[string]any{"kind": "event", "title": "Consultation", "course": "", "due": "",
		"start": "2026-10-22T15:00", "end": "2026-10-22T16:00", "room": "R205", "category": "study", "why": ""}
	event := must(quick.Checked(answer, "консультация в четверг 15–16 R205"))
	if event.Get("kind") != "event" || event.Get("start") != "2026-10-22T15:00" ||
		event.Get("end") != "2026-10-22T16:00" || event.Get("room") != "R205" {
		t.Fatal(event)
	}
	answer["start"] = "2026-10-22"
	wholeDay := must(quick.Checked(answer, "x"))
	if wholeDay.Get("start") != "2026-10-22" || wholeDay.Get("end") != nil {
		t.Fatal(wholeDay)
	}
	answer["start"] = ""
	if must(quick.Checked(answer, "x")).Get("kind") != "note" {
		t.Fatal("no start")
	}
	if must(quick.Checked(map[string]any{"kind": "?", "title": ""}, "just a thought")).Get("title") != "just a thought" {
		t.Fatal("the line")
	}
}

func TestByHand(t *testing.T) {
	a := newAdder(t)
	event := a.ask("POST", "/add/event", map[string]any{"title": "Consultation", "start": "2026-10-22T15:00",
		"end": "2026-10-22T16:00", "room": "R205"}, 200)
	if obj(event["change"])["undo"] != true {
		t.Fatal(event)
	}
	consultations := func() []*events.Event {
		out := []*events.Event{}
		for _, e := range must(a.sch.Events.InForceAll()) {
			if e.Title == "Consultation" {
				out = append(out, e)
			}
		}
		return out
	}
	if found := consultations(); len(found) != 1 || found[0].Room != "R205" {
		t.Fatal(found)
	}
	a.undo(event["change"])
	if len(consultations()) != 0 {
		t.Fatal("undone")
	}
	personal := a.ask("POST", "/add/deadline", map[string]any{"title": "Passport", "due": "2026-10-30",
		"category": "personal"}, 200)
	if obj(personal["change"])["undo"] != true || must(a.merged.Todo.Own())[0].Category != "personal" {
		t.Fatal(personal)
	}
	a.ask("POST", "/add/deadline", map[string]any{"title": "", "due": "2026-10-30"}, 400)
	if r := a.ask("POST", "/add/grade", map[string]any{"course": "Alg", "label": "Quiz", "value": 8}, 400); !strings.Contains(r["error"].(string), "add_grade") {
		t.Fatal(r)
	}
	a.ask("POST", "/add/nothing", map[string]any{}, 404)
	_ = todo.Date
}
