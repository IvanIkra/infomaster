package web

// The web app's corrections (tests/test_web_fixes.py): a class moved, cancelled and put in the optional calendar, a
// chat's correction ignored, an event hidden and deleted, the hidden lists with «Restore», grades corrected. The
// connector's tools are stubs over the real timetable here.

import (
	"strings"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
)

func fixer(t *testing.T) (*harness, *calls) {
	h := newHarness(t, options{screens: true, connector: true, data: func(h *harness, d *Data) {
		must(h.sch.Subjects.SetManual("Discrete Mathematics", "Discrete Mathematics", "DM"))
		must(h.sch.Inbox.PutChat(testChat, "DM seminar chat", "supergroup", nil, 0))
	}})
	deadlineTools(t, h)
	seen := &calls{}
	c := h.connector
	text := func(a map[string]any, name string) string { s, _ := a[name].(string); return s }
	stub(t, c, "correct_class", []string{"class_uid", "start", "end", "room", "cancelled", "kind", "required", "title"},
		func(tc *tools.Context, a map[string]any) (tools.Output, error) {
			seen.add("correct_class", a)
			var title, kind *string
			if s, ok := a["title"].(string); ok {
				title = &s
			}
			if s, ok := a["kind"].(string); ok {
				kind = &s
			}
			cancelled, _ := a["cancelled"].(bool)
			required, _ := a["required"].(bool)
			id, err := h.sch.CorrectClass(tc.Ctx, text(a, "class_uid"), text(a, "start")+":00", text(a, "end")+":00",
				text(a, "room"), cancelled, title, kind, required)
			if err == nil && id == nil {
				err = tools.Errorf("no such class")
			}
			if err != nil {
				return tools.Output{}, err
			}
			return tools.Output{Summary: "Corrected a class", Undo: []tools.Step{{Tool: "remove_event",
				Arguments: map[string]any{"event_id": *id}}}}, nil
		})
	stub(t, c, "remove_event", []string{"event_id"}, func(tc *tools.Context, a map[string]any) (tools.Output, error) {
		seen.add("remove_event", a)
		event, err := h.sch.RemoveOwn(tc.Ctx, a["event_id"].(int64))
		if err == nil && event == nil {
			err = tools.Errorf("not an own event: a chat's one is hidden with set_event_options")
		}
		return tools.Output{Summary: "Removed an event", Undo: []tools.Step{{Tool: "_noop"}}}, err
	})
	c.Registry.Op("_noop", func(*tools.Context, map[string]any) (tools.Output, error) { return tools.Output{}, nil })
	stub(t, c, "set_event_options", []string{"event_id", "required", "hidden"}, func(tc *tools.Context, a map[string]any) (tools.Output, error) {
		seen.add("set_event_options", a)
		id := a["event_id"].(int64)
		undo := map[string]any{"event_id": id}
		before := must(h.sch.Events.Current(id))
		if hidden, ok := a["hidden"].(bool); ok {
			undo["hidden"] = before.Hidden
			if _, err := h.sch.SetHidden(tc.Ctx, id, hidden); err != nil {
				return tools.Output{}, err
			}
		}
		if required, ok := a["required"].(bool); ok {
			undo["required"] = before.Required
			if _, err := h.sch.Events.SetRequired(id, required); err != nil {
				return tools.Output{}, err
			}
		}
		return tools.Output{Summary: "Set an event's options", Undo: []tools.Step{{Tool: "set_event_options",
			Arguments: undo}}}, nil
	})
	for _, name := range []string{"edit_event", "change_grade", "delete_grade"} {
		stub(t, c, name, []string{"event_id", "title", "kind", "start", "end", "room", "cancelled", "grade_id", "value",
			"label"}, seen.recorder(name, true))
	}
	return h.serve().in(), seen
}

func entries(feed map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, day := range list(feed["days"]) {
		for _, item := range list(obj(day)["items"]) {
			if obj(item)["kind"] == "class" {
				out[obj(item)["title"].(string)] = obj(item)
			}
		}
	}
	return out
}

func (h *harness) seminar() map[string]any {
	found := entries(h.read("/feed"))["Discrete Mathematics"]
	return h.read("/class?uid=" + found["uid"].(string))
}

func with(fields map[string]any, uid string, changes map[string]any) map[string]any {
	out := map[string]any{"uid": uid}
	for k, v := range fields {
		out[k] = v
	}
	for k, v := range changes {
		out[k] = v
	}
	return out
}

func TestClassMovedCancelledAndBack(t *testing.T) {
	h, seen := fixer(t)
	card := h.seminar()
	fix := obj(card["fix"])
	fields := obj(fix["fields"])
	if fix["of"] != "class" || fix["own"] != nil || fix["chat"] != nil || fields["start"] != "2026-10-22T16:40" ||
		fields["end"] != "2026-10-22T18:00" || fields["room"] != obj(fix["ruz"])["room"] || fields["cancelled"] != false ||
		fields["required"] != true || len(fields) != 6 {
		t.Fatal(fix)
	}
	uid := card["uid"].(string)
	moved := h.ask("POST", "/fix/class", with(fields, uid, map[string]any{"start": "2026-10-22T17:40",
		"end": "2026-10-22T19:00", "room": "R101", "required": false}), 200)
	if obj(moved["change"])["undo"] != true || seen.last()["class_uid"] != uid {
		t.Fatal(moved, seen.last())
	}
	card = h.seminar()
	if card["start"] != "2026-10-22T17:40:00" || card["place"] != "R101" || obj(card["fix"])["own"] == nil ||
		obj(obj(card["fix"])["fields"])["required"] != false {
		t.Fatal(card)
	}
	// Cancelled, then «As it was» takes the own correction back.
	h.ask("POST", "/fix/class", with(obj(obj(card["fix"])["fields"]), uid, map[string]any{"cancelled": true}), 200)
	card = h.seminar()
	if card["cancelled"] != true {
		t.Fatal(card)
	}
	h.ask("POST", "/fix/remove", map[string]any{"id": obj(card["fix"])["own"]}, 200)
	card = h.seminar()
	if card["cancelled"] != false || card["start"] != "2026-10-22T16:40:00" || obj(card["fix"])["own"] != nil {
		t.Fatal(card)
	}
	for _, c := range must(h.connector.Changes.Recent(100, "")) {
		if c.Via != "web" {
			t.Fatal(c)
		}
	}
	if r := h.ask("POST", "/fix/class", map[string]any{"uid": uid, "colour": "red"}, 400); !strings.Contains(r["error"].(string), "expected") {
		t.Fatal(r)
	}
	if r := h.ask("POST", "/fix/class", map[string]any{"uid": "nothing", "start": "x"}, 404); r["error"] != "no such class or event" {
		t.Fatal(r)
	}
	h.ask("POST", "/fix/event", map[string]any{"uid": uid, "room": "R1"}, 400) // a class is changed as a class
}

func (h *harness) chatEvent(fields func(*events.New)) int64 {
	e := events.New{UID: "tg-ev1@infomaster", Source: "tg", ChatID: ptr(testChat), MsgIDs: []int64{7},
		Title: "Consultation", Kind: "Consultation", Start: "2026-10-24T15:00:00", End: "2026-10-24T16:00:00",
		Room: "R205", Required: true}
	if fields != nil {
		fields(&e)
	}
	return must(h.sch.Events.Add(e, h.clock.now()))
}

func TestChatCorrectionIgnored(t *testing.T) {
	h, _ := fixer(t)
	uid := h.seminar()["uid"].(string)
	h.chatEvent(func(e *events.New) {
		e.UID, e.MsgIDs, e.ClassUID, e.Course, e.Title, e.Kind = "tg-c1@infomaster", []int64{3}, uid,
			"Discrete Mathematics", "", "Seminar"
		e.Start, e.End, e.Room = "2026-10-23T10:00:00", "2026-10-23T11:20:00", "R300"
	})
	card := h.seminar()
	fix := obj(card["fix"])
	if card["start"] != "2026-10-23T10:00:00" || obj(fix["chat"])["from"] != "DM seminar chat" ||
		obj(fix["fields"])["room"] != "R300" {
		t.Fatal(card)
	}
	ignored := h.ask("POST", "/fix/options", map[string]any{"id": obj(fix["chat"])["id"], "hidden": true}, 200)
	card = h.seminar()
	fix = obj(card["fix"])
	if card["start"] != "2026-10-22T16:40:00" || obj(fix["chat"])["hidden"] != true ||
		obj(fix["fields"])["room"] != obj(fix["ruz"])["room"] {
		t.Fatal(card)
	}
	h.undo(ignored["change"])
	if h.seminar()["start"] != "2026-10-23T10:00:00" {
		t.Fatal("applied again")
	}
}

func TestEventHiddenAndRestored(t *testing.T) {
	h, seen := fixer(t)
	h.chatEvent(nil)
	event := entries(h.read("/feed"))["Consultation"]
	fix := obj(h.read("/class?uid=" + event["uid"].(string))["fix"])
	sameJSON(t, "fields", fix["fields"], map[string]any{"title": "Consultation", "kind": "Consultation",
		"start": "2026-10-24T15:00", "end": "2026-10-24T16:00", "room": "R205", "cancelled": false})
	if fix["of"] != "event" || fix["own"] != false || fix["copy"] != false || fix["required"] != true {
		t.Fatal(fix)
	}
	h.ask("POST", "/fix/options", map[string]any{"id": fix["id"], "required": false}, 200)
	if obj(h.read("/class?uid=" + event["uid"].(string))["fix"])["required"] != false {
		t.Fatal("optional")
	}
	h.ask("POST", "/fix/options", map[string]any{"id": fix["id"], "hidden": true}, 200)
	if _, there := entries(h.read("/feed"))["Consultation"]; there {
		t.Fatal("hidden")
	}
	listed := h.read("/fix/hidden")
	hiddenEvents := list(listed["events"])
	if len(hiddenEvents) != 1 || obj(hiddenEvents[0])["title"] != "Consultation" || obj(hiddenEvents[0])["chat"] != "DM seminar chat" {
		t.Fatal(listed)
	}
	h.ask("POST", "/fix/options", map[string]any{"id": obj(hiddenEvents[0])["id"], "hidden": false}, 200)
	if _, there := entries(h.read("/feed"))["Consultation"]; !there {
		t.Fatal("back")
	}
	h.ask("POST", "/fix/event", map[string]any{"uid": event["uid"], "room": "R301"}, 200)
	if got := seen.last(); got["_tool"] != "edit_event" || got["event_id"] != fix["id"] || got["room"] != "R301" || len(got) != 3 {
		t.Fatal(got)
	}
	if r := h.ask("POST", "/fix/remove", map[string]any{"id": fix["id"]}, 400); !strings.Contains(r["error"].(string), "set_event_options") {
		t.Fatal(r) // a chat's event is hidden, not deleted
	}
	h.ask("POST", "/fix/event", map[string]any{"uid": event["uid"]}, 400)
	h.ask("POST", "/fix/options", map[string]any{"id": "1", "hidden": true}, 400)
	h.ask("POST", "/fix/options", map[string]any{"id": fix["id"]}, 400)
}

func TestHiddenDeadlineRestored(t *testing.T) {
	h, _ := fixer(t)
	h.addRow("Sheet 5", "2026-10-22", "", "Discrete Mathematics")
	sheet := todo.UID(h.item("Sheet 5"))
	h.ask("POST", "/deadlines/not-mine", map[string]any{"uid": sheet}, 200)
	listed := list(h.read("/fix/hidden")["deadlines"])
	if len(listed) != 1 {
		t.Fatal(listed)
	}
	row := obj(listed[0])
	if row["title"] != "Sheet 5" || row["due"] != "2026-10-22" || row["reason"] != "not mine" || row["course"] != "DM" {
		t.Fatal(row)
	}
	restored := h.ask("POST", "/fix/hidden/deadline", map[string]any{"uid": sheet}, 200)
	if obj(restored["change"])["undo"] != true || len(list(h.read("/fix/hidden")["deadlines"])) != 0 {
		t.Fatal(restored)
	}
	h.undo(restored["change"])
	if len(list(h.read("/fix/hidden")["deadlines"])) != 1 {
		t.Fatal("hidden again")
	}
	if r := h.ask("POST", "/fix/hidden/deadline", map[string]any{"uid": "nothing"}, 400); r["error"] == nil {
		t.Fatal(r)
	}
}

func TestGradesCorrectedArguments(t *testing.T) {
	h, seen := fixer(t)
	h.ask("POST", "/fix/grade", map[string]any{"id": "k1", "value": 9.5}, 200)
	if got := seen.last(); got["_tool"] != "change_grade" || got["grade_id"] != "k1" || got["value"] != 9.5 || len(got) != 3 {
		t.Fatal(got)
	}
	h.ask("POST", "/fix/grade", map[string]any{"id": "own-1", "value": int64(8), "label": "Essay 1"}, 200)
	if got := seen.last(); got["label"] != "Essay 1" || got["value"] != int64(8) {
		t.Fatal(got)
	}
	h.ask("POST", "/fix/grade/reset", map[string]any{"id": "k1"}, 200)
	if got := seen.last(); got["_tool"] != "delete_grade" || got["grade_id"] != "k1" {
		t.Fatal(got)
	}
	if r := h.ask("POST", "/fix/grade", map[string]any{"id": "k1"}, 400); r["error"] != "expected {id, value, label?}" {
		t.Fatal(r)
	}
	h.ask("POST", "/fix/grade", map[string]any{"value": 5}, 400)
	h.ask("POST", "/fix/grade/reset", map[string]any{}, 400)
}
