package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/actions"
	collector "github.com/IvanIkra/infomaster/go/internal/collectors/grades"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

func ctxOf(t *tools.Context) context.Context {
	if t == nil || t.Ctx == nil {
		return context.Background()
	}
	return t.Ctx
}

// --- writes: deadlines ----------------------------------------------------------------------------

func (c *Catalog) ownKeys() (map[string]bool, error) {
	own, err := c.A.Deadlines.Todo.Own()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, d := range own {
		out[d.Key] = true
	}
	return out, nil
}

// newKeys are the own keys not there before, sorted.
func (c *Catalog) newKeys(before map[string]bool) ([]string, error) {
	after, err := c.ownKeys()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for key := range after {
		if !before[key] {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
}

func first(keys []string) any {
	if len(keys) == 0 {
		return nil
	}
	return keys[0]
}

// listed is a deadline of the list by uid; an own one also when the merge left it out as a copy of another.
func (c *Catalog) listed(uid string) (todo.Deadline, error) {
	d, err := c.A.Deadlines.Find(uid)
	if err != nil {
		return todo.Deadline{}, err
	}
	if d != nil {
		return *d, nil
	}
	own, err := c.A.Deadlines.Todo.Own()
	if err != nil {
		return todo.Deadline{}, err
	}
	for _, d := range own {
		if d.Key == uid {
			return d, nil
		}
	}
	return todo.Deadline{}, refuse("no such deadline in the list: take the uid from list_deadlines")
}

func (c *Catalog) addDeadline(t *tools.Context, args map[string]any) (tools.Output, error) {
	before, err := c.ownKeys()
	if err != nil {
		return tools.Output{}, err
	}
	category := any("study")
	if has(args, "category") {
		category = args["category"]
	}
	body := httpx.Object{{Key: "title", Value: args["title"]}, {Key: "due", Value: args["due"]},
		{Key: "category", Value: category}}
	if truthy(args, "course") {
		body = append(body, httpx.Field{Key: "course", Value: args["course"]})
	}
	if _, err := c.call(c.A.AddDeadline(ctxOf(t), objectMap(body))); err != nil {
		return tools.Output{}, err
	}
	fresh, err := c.newKeys(before)
	if err != nil {
		return tools.Output{}, err
	}
	undo := []tools.Step{}
	for _, key := range fresh {
		undo = append(undo, step("delete_deadline", "uid", key))
	}
	return tools.Output{Data: append(httpx.Object{{Key: "uid", Value: first(fresh)}}, body...),
		Summary: "Added the deadline «" + actions.PyStr(args["title"]) + "», due " + actions.PyStr(args["due"]),
		Undo:    undo}, nil
}

func objectMap(o httpx.Object) map[string]any {
	out := map[string]any{}
	for _, f := range o {
		out[f.Key] = f.Value
	}
	return out
}

func (c *Catalog) editDeadline(t *tools.Context, args map[string]any) (tools.Output, error) {
	uid := str(args, "uid")
	d, err := c.listed(uid)
	if err != nil {
		return tools.Output{}, err
	}
	names := pick(args, "title", "due", "category")
	if len(names) == 0 {
		return tools.Output{}, refuse("give at least one of title, due, category")
	}
	body := spreadArgs(nil, args, names)
	before, err := c.ownKeys()
	if err != nil {
		return tools.Output{}, err
	}
	if _, err := c.call(c.A.EditDeadline(ctxOf(t), uid, objectMap(body))); err != nil {
		return tools.Output{}, err
	}
	due := todo.DueText(d.Due)
	summary := "Changed the deadline «" + d.Title + "» (" + due + "): " + changes(args, names)
	if d.Source == "ha" {
		return tools.Output{Data: append(httpx.Object{{Key: "uid", Value: d.Key}}, body...), Summary: summary,
			Undo: steps(step("edit_deadline", "uid", d.Key, "title", d.Title, "due", due, "category", d.Category))}, nil
	}
	fresh, err := c.newKeys(before)
	if err != nil {
		return tools.Output{}, err
	}
	if len(fresh) == 0 && d.Assumed { // a sheet's assumed date given a real one: the chats' deadline moved (§6.3)
		return tools.Output{Data: append(httpx.Object{{Key: "uid", Value: nil}}, body...),
			Summary: summary + " (the sheet's real date now)",
			Note:    "the sheet's date is real now: change it again to move it"}, nil
	}
	undo := []tools.Step{}
	for _, key := range fresh {
		undo = append(undo, step("delete_deadline", "uid", key))
	}
	undo = append(undo, step("unhide_deadline", "uid", todo.UID(d)))
	return tools.Output{Data: append(httpx.Object{{Key: "uid", Value: first(fresh)},
		{Key: "hidden_original", Value: todo.UID(d)}}, body...),
		Summary: summary + " (the source's one is hidden, an own copy stands for it)", Undo: undo}, nil
}

// ReasonWords are the words of a reason to hide a deadline in the journal.
var ReasonWords = map[string]string{"done": "as done", "not_mine": "as not the owner's",
	"duplicate": "as a duplicate"}

func (c *Catalog) hideDeadline(t *tools.Context, args map[string]any) (tools.Output, error) {
	uid := str(args, "uid")
	d, err := c.listed(uid)
	if err != nil {
		return tools.Output{}, err
	}
	if d.Source == "ha" {
		return tools.Output{}, refuse("this is an own deadline: delete it with delete_deadline")
	}
	reason, kept := str(args, "reason"), str(args, "kept")
	if reason == "duplicate" {
		if kept == "" {
			return tools.Output{}, refuse("a duplicate needs `kept`: the uid of the deadline that stays")
		}
		if kept == uid {
			return tools.Output{}, refuse("a deadline cannot be a duplicate of itself")
		}
		if _, err := c.listed(kept); err != nil {
			return tools.Output{}, err
		}
	}
	if reason == "" {
		if _, err := c.call(c.A.DeleteDeadline(ctxOf(t), uid)); err != nil {
			return tools.Output{}, err
		}
	} else if err := c.A.Deadlines.Remove(d, reason, kept); err != nil {
		return tools.Output{}, err
	}
	words := ""
	var reasonValue any
	if reason != "" {
		words, reasonValue = " "+ReasonWords[reason], reason
	}
	return tools.Output{Data: httpx.Object{{Key: "hidden", Value: todo.UID(d)}, {Key: "reason", Value: reasonValue}},
		Summary: fmt.Sprintf("Hid the deadline «%s» (%s) of %s%s", d.Title, todo.DueText(d.Due), d.Source, words),
		Undo:    steps(step("unhide_deadline", "uid", todo.UID(d)))}, nil
}

// confirmDeadlineDate: a sheet's assumed date (§6.3) is right: it is the sheet's real date now, and the mark goes.
func (c *Catalog) confirmDeadlineDate(t *tools.Context, args map[string]any) (tools.Output, error) {
	uid := str(args, "uid")
	d, err := c.listed(uid)
	if err != nil {
		return tools.Output{}, err
	}
	row, err := c.A.Row(d)
	if err != nil {
		return tools.Output{}, err
	}
	if !d.Assumed || row == nil {
		return tools.Output{}, refuse("this deadline's date is not an assumed one")
	}
	due := todo.DueText(d.Due)
	var sheetID, dueAt any
	found := false
	if row.SheetID != nil && c.A.Sheets != nil {
		sheet, err := c.A.Sheets.Sheets.Get(*row.SheetID)
		if err != nil {
			return tools.Output{}, err
		}
		if sheet != nil {
			found, sheetID, dueAt = true, sheet.ID, sheet.DueAt
		}
	}
	if found { // the sheet's way: the sheet's date given, the same one
		if _, err := c.call(c.A.EditDeadline(ctxOf(t), uid, map[string]any{"due": due})); err != nil {
			return tools.Output{}, err
		}
	} else if err := c.A.Deadlines.Deadlines.Confirm(row.ID); err != nil {
		return tools.Output{}, err
	}
	note := ""
	if found {
		note = "the sheet keeps the date as confirmed"
	}
	return tools.Output{Data: httpx.Object{{Key: "uid", Value: uid}, {Key: "due", Value: due}},
		Summary: "Confirmed the date of «" + d.Title + "»: " + due,
		Undo:    steps(step("_assume_again", "deadline_id", row.ID, "sheet_id", sheetID, "due_at", dueAt)),
		Note:    note}, nil
}

// assumeAgain is the undo of confirm_deadline_date: the date is an assumed one again and follows the timetable.
func (c *Catalog) assumeAgain(_ *tools.Context, args map[string]any) (tools.Output, error) {
	id, _ := integer(args, "deadline_id")
	err := c.Conn.Store.InTx(func(tx *sql.Tx) error {
		row, err := c.A.Deadlines.Deadlines.With(tx).Get(id)
		if err != nil {
			return err
		}
		if row == nil || row.MovedBy != nil || row.GoneAt != nil {
			return refuse("the deadline has changed since")
		}
		if _, err := tx.Exec("update deadlines set assumed = 1 where id = ?", row.ID); err != nil {
			return err
		}
		if sheetID, ok := integer(args, "sheet_id"); ok {
			_, err = tx.Exec("update sheets set due_assumed = 1, due_at = coalesce(?, due_at) where id = ?",
				args["due_at"], sheetID)
		}
		return err
	})
	return tools.Output{}, err
}

// setDeadlineCourse is the course of a deadline whose chat or letter named none (or named it wrong).
func (c *Catalog) setDeadlineCourse(_ *tools.Context, args map[string]any) (tools.Output, error) {
	uid := str(args, "uid")
	d, err := c.listed(uid)
	if err != nil {
		return tools.Output{}, err
	}
	row, err := c.A.Row(d)
	if err != nil {
		return tools.Output{}, err
	}
	if row == nil {
		return tools.Output{}, refuse("only a deadline of a chat, a letter or RUZ has a course to set")
	}
	course := strings.Join(pytext.Split(str(args, "course")), " ")
	if course == "" {
		return tools.Output{}, refuse("course is empty")
	}
	if err := c.A.Deadlines.Deadlines.Rename(row.ID, row.Label, &course); err != nil {
		return tools.Output{}, err
	}
	return tools.Output{Data: httpx.Object{{Key: "uid", Value: uid}, {Key: "course", Value: course}},
		Summary: "Set the course of «" + d.Title + "» to " + course,
		Undo:    steps(step("_set_course_back", "deadline_id", row.ID, "course", row.Course))}, nil
}

func (c *Catalog) setCourseBack(_ *tools.Context, args map[string]any) (tools.Output, error) {
	id, _ := integer(args, "deadline_id")
	row, err := c.A.Deadlines.Deadlines.Get(id)
	if err != nil {
		return tools.Output{}, err
	}
	if row == nil {
		return tools.Output{}, refuse("the deadline is gone")
	}
	var course *string
	if s, ok := args["course"].(string); ok {
		course = &s
	}
	return tools.Output{}, c.A.Deadlines.Deadlines.Rename(row.ID, row.Label, course)
}

func (c *Catalog) deleteDeadline(t *tools.Context, args map[string]any) (tools.Output, error) {
	uid := str(args, "uid")
	d, err := c.listed(uid)
	if err != nil {
		return tools.Output{}, err
	}
	if d.Source != "ha" {
		return tools.Output{}, refuse("this deadline comes from a source: hide it with hide_deadline")
	}
	if _, err := c.call(c.A.DeleteDeadline(ctxOf(t), uid)); err != nil {
		return tools.Output{}, err
	}
	due := todo.DueText(d.Due)
	return tools.Output{Data: httpx.Object{{Key: "deleted", Value: d.Key}},
		Summary: "Deleted the own deadline «" + d.Title + "» (" + due + ")",
		Undo:    steps(step("add_deadline", "title", d.Title, "due", due, "category", d.Category))}, nil
}

func (c *Catalog) unhideDeadline(t *tools.Context, args map[string]any) (tools.Output, error) {
	uid := str(args, "uid")
	rows, err := c.A.Deadlines.Todo.HiddenRows()
	if err != nil {
		return tools.Output{}, err
	}
	var row *todo.HiddenRow
	for i := range rows {
		if rows[i].UID == uid {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		return tools.Output{}, refuse("no such hidden deadline: take the uid from list_hidden_deadlines")
	}
	if _, err := c.call(c.A.UnhideDeadline(ctxOf(t), uid)); err != nil {
		return tools.Output{}, err
	}
	found, err := c.A.Deadlines.Find(uid)
	if err != nil {
		return tools.Output{}, err
	}
	back := found != nil
	out := tools.Output{Data: httpx.Object{{Key: "unhidden", Value: uid}, {Key: "back_in_list", Value: back}},
		Summary: "Restored the hidden deadline «" + row.Title + "» (" + row.Due + ")"}
	if back {
		out.Undo = steps(step("hide_deadline", "uid", uid))
	} else {
		out.Note = "its source no longer has it, so it is not back in the list"
	}
	return out, nil
}

// --- writes: the timetable ---------------------------------------------------------------------

func (c *Catalog) newestOwn(after int64, correction bool) (any, error) {
	not := ""
	if correction {
		not = "not "
	}
	var id int64
	err := c.A.Schedule.Events.DB().QueryRow("select id from events where id > ? and source = ? and class_uid is "+
		not+"null order by id desc limit 1", after, events.Own).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return id, nil
}

func (c *Catalog) lastEventID() (int64, error) {
	var n int64
	err := c.A.Schedule.Events.DB().QueryRow("select coalesce(max(id), 0) n from events").Scan(&n)
	return n, err
}

// eventFields are an event's fields as add_event and edit_event take them.
func eventFields(event *events.Event) (httpx.Object, error) {
	var end any
	if !schedule.AllDay(event.Start) {
		if event.End != nil && *event.End != "" {
			end = *event.End
		} else {
			start, err := schedule.Local(event.Start)
			if err != nil {
				return nil, err
			}
			end = pyISO(start.Add(schedule.DefaultLength))
		}
	}
	return httpx.Object{{Key: "title", Value: event.Title}, {Key: "kind", Value: event.Kind},
		{Key: "start", Value: event.Start}, {Key: "end", Value: end}, {Key: "room", Value: event.Room},
		{Key: "cancelled", Value: event.Cancelled}}, nil
}

// pyISO is isoformat() of a naive moment: microseconds only when there are some.
func pyISO(t interface {
	Format(string) string
	Nanosecond() int
}) string {
	if t.Nanosecond()/1000 != 0 {
		return t.Format("2006-01-02T15:04:05.000000")
	}
	return t.Format("2006-01-02T15:04:05")
}

func (c *Catalog) currentEvent(id int64) (*events.Event, error) {
	event, err := c.A.Schedule.Events.Current(id)
	if err != nil {
		return nil, err
	}
	if event == nil || event.GoneAt != nil {
		return nil, refuse("no such event: take the id from get_timetable (items[].event.id or hidden_events[].id)")
	}
	return event, nil
}

func (c *Catalog) addEvent(t *tools.Context, args map[string]any) (tools.Output, error) {
	kind, room, required := any("Event"), any(""), any(true)
	if has(args, "kind") {
		kind = args["kind"]
	}
	if has(args, "room") {
		room = args["room"]
	}
	if has(args, "required") {
		required = args["required"]
	}
	body := httpx.Object{{Key: "title", Value: args["title"]}, {Key: "kind", Value: kind},
		{Key: "start", Value: args["start"]}, {Key: "end", Value: args["end"]}, {Key: "room", Value: room},
		{Key: "required", Value: required}}
	if truthy(args, "cancelled") {
		body = append(body, httpx.Field{Key: "cancelled", Value: true})
	}
	after, err := c.lastEventID()
	if err != nil {
		return tools.Output{}, err
	}
	if _, err := c.call(c.A.AddEvent(ctxOf(t), objectMap(body))); err != nil {
		return tools.Output{}, err
	}
	fresh, err := c.newestOwn(after, false)
	if err != nil {
		return tools.Output{}, err
	}
	out := tools.Output{Data: append(httpx.Object{{Key: "event_id", Value: fresh}}, body...),
		Summary: "Added the event «" + actions.PyStr(args["title"]) + "» at " + actions.PyStr(args["start"])}
	if fresh != nil {
		out.Undo = steps(step("remove_event", "event_id", fresh))
	}
	return out, nil
}

func (c *Catalog) editEvent(t *tools.Context, args map[string]any) (tools.Output, error) {
	id, _ := integer(args, "event_id")
	event, err := c.currentEvent(id)
	if err != nil {
		return tools.Output{}, err
	}
	if event.ClassUID != nil {
		return tools.Output{}, refuse("this is a class of RUZ: change it with correct_class")
	}
	names := pick(args, "title", "kind", "start", "end", "room", "cancelled")
	if len(names) == 0 {
		return tools.Output{}, refuse("give at least one of title, kind, start, end, room, cancelled")
	}
	body := spreadArgs(nil, args, names)
	values := objectMap(body)
	if has(values, "start") && !has(values, "end") {
		body, values["end"] = append(body, httpx.Field{Key: "end", Value: nil}), nil
		names = append(names, "end")
	}
	after, err := c.lastEventID()
	if err != nil {
		return tools.Output{}, err
	}
	if _, err := c.call(c.A.UpdateEvent(ctxOf(t), itoa(event.ID), values)); err != nil {
		return tools.Output{}, err
	}
	fresh, err := c.newestOwn(after, false)
	if err != nil {
		return tools.Output{}, err
	}
	summary := "Changed the event «" + event.Title + "» (" + event.Start + "): " + changes(values, names)
	if event.Source == events.Own {
		before, err := eventFields(event)
		if err != nil {
			return tools.Output{}, err
		}
		out := tools.Output{Data: append(httpx.Object{{Key: "event_id", Value: fresh}}, body...), Summary: summary}
		if fresh != nil {
			s := step("edit_event", "event_id", fresh)
			for _, name := range names {
				s.Arguments[name] = before.Get(name)
			}
			out.Undo = steps(s)
		}
		return out, nil
	}
	out := tools.Output{Data: append(httpx.Object{{Key: "event_id", Value: fresh},
		{Key: "hidden_original", Value: event.ID}}, body...),
		Summary: summary + " (the chat's one is hidden, an own copy stands for it)"}
	if fresh != nil {
		out.Undo = steps(step("remove_event", "event_id", fresh))
	}
	return out, nil
}

func (c *Catalog) setEventOptions(t *tools.Context, args map[string]any) (tools.Output, error) {
	id, _ := integer(args, "event_id")
	event, err := c.currentEvent(id)
	if err != nil {
		return tools.Output{}, err
	}
	names := pick(args, "required", "hidden")
	if len(names) == 0 {
		return tools.Output{}, refuse("give required, hidden or both")
	}
	body := spreadArgs(nil, args, names)
	if _, err := c.call(c.A.UpdateEvent(ctxOf(t), itoa(event.ID), objectMap(body))); err != nil {
		return tools.Output{}, err
	}
	after, err := c.A.Schedule.Events.Current(event.ID)
	if err != nil {
		return tools.Output{}, err
	}
	afterID := event.ID
	if after != nil {
		afterID = after.ID
	}
	words := map[string][2]string{"required": {"in the main calendar", "in the optional calendar"},
		"hidden": {"hidden", "shown again"}}
	said := []string{}
	undo := step("set_event_options", "event_id", afterID)
	for _, name := range names {
		if args[name] == true {
			said = append(said, words[name][0])
		} else {
			said = append(said, words[name][1])
		}
		if name == "required" {
			undo.Arguments[name] = event.Required
		} else {
			undo.Arguments[name] = event.Hidden
		}
	}
	return tools.Output{Data: append(httpx.Object{{Key: "event_id", Value: afterID}}, body...),
		Summary: "«" + event.Title + "» (" + event.Start + "): " + strings.Join(said, ", "), Undo: steps(undo)}, nil
}

func (c *Catalog) removeEvent(t *tools.Context, args map[string]any) (tools.Output, error) {
	id, _ := integer(args, "event_id")
	event, err := c.currentEvent(id)
	if err != nil {
		return tools.Output{}, err
	}
	if event.Source != events.Own {
		return tools.Output{}, refuse("only own events, own copies and own class corrections are removed; hide a " +
			"chat's event with set_event_options")
	}
	if _, err := c.call(c.A.DeleteEvent(ctxOf(t), itoa(event.ID))); err != nil {
		return tools.Output{}, err
	}
	fields, err := eventFields(event)
	if err != nil {
		return tools.Output{}, err
	}
	removed := httpx.Object{{Key: "removed", Value: event.ID}}
	if event.ClassUID != nil {
		undo := classStep(*event.ClassUID, fields, event)
		return tools.Output{Data: removed, Summary: "Took back the own change of the class at " + event.Start,
			Undo: steps(undo)}, nil
	}
	if event.CopyOf != nil && *event.CopyOf != "" {
		var rowID int64
		err := c.A.Schedule.Events.DB().QueryRow("select id from events where uid = ? and "+events.InForce+
			" order by id desc limit 1", *event.CopyOf).Scan(&rowID)
		out := tools.Output{Data: removed,
			Summary: "Took back the own change of «" + event.Title + "»: the chat's one is back"}
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out.Note = "the chat's event is gone"
		case err != nil:
			return tools.Output{}, err
		default:
			s := step("edit_event", "event_id", rowID)
			for _, f := range fields {
				s.Arguments[f.Key] = f.Value
			}
			out.Undo = steps(s)
		}
		return out, nil
	}
	s := step("add_event", "required", event.Required)
	for _, f := range fields {
		s.Arguments[f.Key] = f.Value
	}
	return tools.Output{Data: removed, Summary: "Deleted the own event «" + event.Title + "» (" + event.Start + ")",
		Undo: steps(s)}, nil
}

// classStep is correct_class with a correction's fields: how a class was.
func classStep(classUID string, fields httpx.Object, event *events.Event) tools.Step {
	s := step("correct_class", "class_uid", classUID, "start", fields.Get("start"), "end", fields.Get("end"),
		"room", fields.Get("room"), "cancelled", fields.Get("cancelled"), "kind", fields.Get("kind"),
		"required", event.Required)
	if event.Title != "" {
		s.Arguments["title"] = event.Title
	}
	return s
}

func (c *Catalog) correctClass(t *tools.Context, args map[string]any) (tools.Output, error) {
	classUID := str(args, "class_uid")
	earlier, err := c.A.Schedule.Events.OwnCorrection(classUID)
	if err != nil {
		return tools.Output{}, err
	}
	room, cancelled := any(""), any(false)
	if has(args, "room") {
		room = args["room"]
	}
	if has(args, "cancelled") {
		cancelled = args["cancelled"]
	}
	body := spreadArgs(httpx.Object{{Key: "class_uid", Value: classUID}, {Key: "start", Value: args["start"]},
		{Key: "end", Value: args["end"]}, {Key: "room", Value: room}, {Key: "cancelled", Value: cancelled}},
		args, pick(args, "title", "kind", "required"))
	if _, err := c.call(c.A.CorrectClass(ctxOf(t), objectMap(body))); err != nil {
		return tools.Output{}, err
	}
	now, err := c.A.Schedule.Events.OwnCorrection(classUID)
	if err != nil {
		return tools.Output{}, err
	}
	what := "Changed"
	if cancelled == true {
		what = "Cancelled"
	}
	summary := what + " the class " + pytext.Cut(classUID, 12) + "…: " + actions.PyStr(args["start"]) + "–" +
		actions.PyStr(args["end"])
	if actions.PyStr(room) != "" {
		summary += ", " + actions.PyStr(room)
	}
	var correction any
	if now != nil {
		correction = now.ID
	}
	out := tools.Output{Data: append(httpx.Object{{Key: "correction_id", Value: correction}}, body...),
		Summary: summary}
	if earlier == nil {
		if now != nil {
			out.Undo = steps(step("remove_event", "event_id", now.ID))
		}
		return out, nil
	}
	fields, err := eventFields(earlier)
	if err != nil {
		return tools.Output{}, err
	}
	out.Undo = steps(classStep(classUID, fields, earlier))
	return out, nil
}

// --- writes: sheets, chats, the dictionary ------------------------------------------------------

func (c *Catalog) editSheet(t *tools.Context, args map[string]any) (tools.Output, error) {
	if c.A.Sheets == nil {
		return tools.Output{}, notRunning
	}
	sheets := c.A.Sheets.Sheets
	id, _ := integer(args, "sheet_id")
	sheet, err := sheets.Get(id)
	if err != nil {
		return tools.Output{}, err
	}
	if sheet == nil {
		return tools.Output{}, refuse("no such sheet")
	}
	names := pick(args, "course", "number", "due", "summary")
	if len(names) == 0 {
		return tools.Output{}, refuse("give at least one of course, number, due, summary")
	}
	firstVersion, err := sheets.First(sheet)
	if err != nil {
		return tools.Output{}, err
	}
	body := spreadArgs(nil, args, names)
	if _, err := c.call(c.A.EditSheet(ctxOf(t), itoa(sheet.ID), objectMap(body))); err != nil {
		return tools.Output{}, err
	}
	orEmpty := func(p *string) any {
		if p == nil {
			return ""
		}
		return *p
	}
	undo := step("edit_sheet", "sheet_id", sheet.ID)
	for _, name := range names {
		switch name {
		case "course":
			undo.Arguments[name] = orEmpty(sheet.Course)
		case "number":
			undo.Arguments[name] = sheet.Number
		case "summary":
			undo.Arguments[name] = orEmpty(firstVersion.Summary)
		case "due":
			if sheet.Due != nil {
				undo.Arguments[name] = *sheet.Due
			} else {
				undo.Arguments[name] = nil
			}
		}
	}
	return tools.Output{Data: append(httpx.Object{{Key: "sheet_id", Value: sheet.ID}}, body...),
		Summary: "Changed the sheet «" + sheet.Name + "»: " + strings.Join(names, ", "), Undo: steps(undo)}, nil
}

// deleteSheet deletes an own sheet with all its versions; a chat's sheet is not the owner's to delete.
func (c *Catalog) deleteSheet(_ *tools.Context, args map[string]any) (tools.Output, error) {
	topic := c.A.Sheets
	if topic == nil {
		return tools.Output{}, notRunning
	}
	id, _ := integer(args, "sheet_id")
	sheet, err := topic.Sheets.Get(id)
	if err != nil {
		return tools.Output{}, err
	}
	if sheet == nil {
		return tools.Output{}, refuse("no such sheet")
	}
	removed, err := topic.Remove(sheet.ID)
	if err != nil {
		return tools.Output{}, err
	}
	if !removed {
		return tools.Output{}, refuse("it is not an own sheet: only the sheets the owner uploaded can be deleted")
	}
	return tools.Output{Data: httpx.Object{{Key: "deleted", Value: sheet.ID}},
		Summary: "Deleted the own sheet «" + sheet.Name + "»", Note: "upload the file again to bring it back"}, nil
}

// removeOwnSheet is the undo of an own sheet's upload.
func (c *Catalog) removeOwnSheet(_ *tools.Context, args map[string]any) (tools.Output, error) {
	if c.A.Sheets == nil {
		return tools.Output{}, notRunning
	}
	id, _ := integer(args, "sheet_id")
	removed, err := c.A.Sheets.Remove(id)
	if err != nil {
		return tools.Output{}, err
	}
	if !removed {
		return tools.Output{}, refuse("the sheet is gone already")
	}
	return tools.Output{}, nil
}

// joinSheet: a sheet posted as new becomes a newer version of another one; the file replaces the older one's.
func (c *Catalog) joinSheet(t *tools.Context, args map[string]any) (tools.Output, error) {
	topic := c.A.Sheets
	if topic == nil {
		return tools.Output{}, notRunning
	}
	id, _ := integer(args, "sheet_id")
	previous, _ := integer(args, "previous")
	sheet, err := topic.Sheets.Get(id)
	if err != nil {
		return tools.Output{}, err
	}
	older, err := topic.Sheets.Get(previous)
	if err != nil {
		return tools.Output{}, err
	}
	if sheet == nil || older == nil {
		return tools.Output{}, refuse("no such sheet")
	}
	if err := topic.Join(ctxOf(t), sheet.ID, older.ID); err != nil {
		var refused *i18n.Error
		if errors.As(err, &refused) {
			return tools.Output{}, &tools.ToolError{Msg: i18n.Reason(err, "en")}
		}
		return tools.Output{}, err
	}
	return tools.Output{Data: httpx.Object{{Key: "sheet_id", Value: sheet.ID}, {Key: "previous", Value: older.ID}},
		Summary: "Joined the sheet «" + sheet.Name + "» to «" + older.Name + "» as its newer version",
		Note:    "the two are one sheet now"}, nil
}

func (c *Catalog) updateChatSettings(t *tools.Context, args map[string]any) (tools.Output, error) {
	chatID, _ := integer(args, "chat_id")
	chat, err := c.A.Inbox.Chat(chatID)
	if err != nil {
		return tools.Output{}, err
	}
	if chat == nil {
		return tools.Output{}, refuse("no such chat: take the id from list_chats")
	}
	names := []string{}
	for _, name := range order("update_chat_settings", args) {
		if name != "chat_id" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return tools.Output{}, refuse("give at least one setting")
	}
	before, err := c.A.ChatSettings.All(chatID)
	if err != nil {
		return tools.Output{}, err
	}
	if args["enabled"] == true && !before.Enabled && t != nil && t.GrantID != nil {
		// D79: a chat read anew widens what strangers can tell the service; the owner turns it on in the web app.
		return tools.Output{}, refuse("a chat is turned on by the owner in the web app (Machine → Twin and chats), " +
			"not here")
	}
	body := spreadArgs(nil, args, names)
	if _, err := c.call(c.A.UpdateChat(ctxOf(t), itoa(chatID), objectMap(body))); err != nil {
		return tools.Output{}, err
	}
	undo := step("update_chat_settings", "chat_id", chatID)
	for _, name := range names {
		value, err := before.Get(name)
		if err != nil {
			return tools.Output{}, err
		}
		undo.Arguments[name] = value
	}
	return tools.Output{Data: append(httpx.Object{{Key: "chat_id", Value: chatID}}, body...),
		Summary: "Chat «" + chat.Title + "»: " + changes(args, names), Undo: steps(undo)}, nil
}

func (c *Catalog) setCourse(t *tools.Context, args map[string]any) (tools.Output, error) {
	dictionary := c.A.Services.Subjects
	before, err := dictionary.Get(str(args, "name"))
	if err != nil {
		return tools.Output{}, err
	}
	entry, err := c.object(c.A.PutSubject(ctxOf(t), map[string]any{"name": args["name"], "title": args["title"],
		"abbr": args["abbr"]}))
	if err != nil {
		return tools.Output{}, err
	}
	name := entry.Get("name")
	undo := step("delete_course", "name", name)
	if before != nil {
		undo = step("_restore_course", "name", name, "title", before.Title, "abbr", before.Abbr,
			"manual", before.Manual)
	}
	return tools.Output{Data: entry, Summary: "Dictionary: «" + actions.PyStr(name) + "» → " +
		actions.PyStr(entry.Get("title")) + " (" + actions.PyStr(entry.Get("abbr")) + ")", Undo: steps(undo)}, nil
}

func (c *Catalog) deleteCourse(t *tools.Context, args map[string]any) (tools.Output, error) {
	name := str(args, "name")
	before, err := c.A.Services.Subjects.Get(name)
	if err != nil {
		return tools.Output{}, err
	}
	if _, err := c.call(c.A.DeleteSubject(ctxOf(t), map[string]any{"name": name})); err != nil {
		return tools.Output{}, err
	}
	out := tools.Output{Data: httpx.Object{{Key: "deleted", Value: name}}, Summary: "Dictionary: deleted «" + name + "»"}
	if before != nil {
		out.Undo = steps(step("_restore_course", "name", name, "title", before.Title, "abbr", before.Abbr,
			"manual", before.Manual))
	}
	return out, nil
}

func (c *Catalog) restoreCourse(_ *tools.Context, args map[string]any) (tools.Output, error) {
	manual, _ := args["manual"].(bool)
	return tools.Output{}, c.A.Services.Subjects.Restore(str(args, "name"), str(args, "title"), str(args, "abbr"),
		manual)
}

// --- writes: settings ---------------------------------------------------------------------------

// settingChange is a change of general settings through an action; name maps an argument to its setting.
func (c *Catalog) settingChange(tool, what string, args map[string]any, name func(string) string,
	action func(map[string]any) (any, error)) (tools.Output, error) {
	if c.A.Settings == nil {
		return tools.Output{}, notRunning
	}
	names := order(tool, args)
	if len(names) == 0 {
		return tools.Output{}, refuse("give at least one setting")
	}
	undo := step(tool)
	for _, key := range names {
		value, err := c.A.Settings.Get(name(key))
		if err != nil {
			return tools.Output{}, err
		}
		undo.Arguments[key] = value
	}
	if _, err := c.call(action(args)); err != nil {
		return tools.Output{}, err
	}
	return tools.Output{Data: spreadArgs(nil, args, names), Summary: what + ": " + changes(args, names),
		Undo: steps(undo)}, nil
}

func same(name string) string { return name }

func (c *Catalog) updateGeneralSettings(t *tools.Context, args map[string]any) (tools.Output, error) {
	return c.settingChange("update_general_settings", "General settings", args, same,
		func(body map[string]any) (any, error) { return c.A.UpdateGeneral(ctxOf(t), body) })
}

func (c *Catalog) updateReminders(t *tools.Context, args map[string]any) (tools.Output, error) {
	if c.A.Reminders == nil {
		return tools.Output{}, notRunning
	}
	return c.settingChange("update_reminders", "Deadline reminders", args, actions.ReminderSetting,
		func(body map[string]any) (any, error) { return c.A.UpdateReminders(ctxOf(t), body) })
}

func (c *Catalog) updateImages(t *tools.Context, args map[string]any) (tools.Output, error) {
	return c.settingChange("update_images", "Images read a day", args,
		func(string) string { return "images_daily" },
		func(body map[string]any) (any, error) { return c.A.UpdateImages(ctxOf(t), body) })
}

func (c *Catalog) updatePDFLimits(t *tools.Context, args map[string]any) (tools.Output, error) {
	if c.A.Sheets == nil {
		return tools.Output{}, notRunning
	}
	return c.settingChange("update_pdf_limits", "Reading whole PDFs", args, same,
		func(body map[string]any) (any, error) { return c.A.UpdateSheets(ctxOf(t), body) })
}

// updateNotifications: the web app's notifications (§8.6, D65): which kinds come, and the quiet hours.
func (c *Catalog) updateNotifications(_ *tools.Context, args map[string]any) (tools.Output, error) {
	s := c.A.Settings
	if s == nil {
		return tools.Output{}, notRunning
	}
	names := order("update_notifications", args)
	if len(names) == 0 {
		return tools.Output{}, refuse("give kinds, quiet or both")
	}
	checked := httpx.Object{}
	for _, key := range names {
		value, err := settings.Check(notificationSetting(key), args[key])
		if err != nil {
			return tools.Output{}, &tools.ToolError{Msg: err.Error()}
		}
		checked = append(checked, httpx.Field{Key: key, Value: value})
	}
	undo := step("update_notifications")
	for _, f := range checked {
		value, err := s.Get(notificationSetting(f.Key))
		if err != nil {
			return tools.Output{}, err
		}
		undo.Arguments[f.Key] = value
	}
	shown := []string{}
	for _, f := range checked {
		if _, err := s.Set(notificationSetting(f.Key), f.Value); err != nil {
			return tools.Output{}, err
		}
		text := actions.PyStr(f.Value)
		if list, ok := f.Value.([]string); ok {
			text = strings.Join(list, ", ")
		}
		if text == "" {
			text = "none"
		}
		shown = append(shown, f.Key+" → "+text)
	}
	return tools.Output{Data: checked, Summary: "Notifications: " + strings.Join(shown, ", "), Undo: steps(undo)}, nil
}

// --- writes: grades -----------------------------------------------------------------------------

// Forgotten is what removing a gradebook, or renaming the student, loses.
const Forgotten = "the grades are read again from scratch: own changes of the sheet's grades and its tab layouts " +
	"are gone"

func (c *Catalog) updateGradeSettings(t *tools.Context, args map[string]any) (tools.Output, error) {
	g := c.A.Grades
	if g == nil {
		return tools.Output{}, notRunning
	}
	names := order("update_grade_settings", args)
	if len(names) == 0 {
		return tools.Output{}, refuse("give student, window_days or both")
	}
	settingOf := map[string]string{"student": "grades_student", "window_days": "grades_window_days"}
	undo := step("update_grade_settings")
	for _, key := range names {
		value, err := g.Settings.Get(settingOf[key])
		if err != nil {
			return tools.Output{}, err
		}
		undo.Arguments[key] = value
	}
	if _, err := c.call(c.A.UpdateGrades(ctxOf(t), spreadMap(args, names))); err != nil {
		return tools.Output{}, err
	}
	note := ""
	if has(args, "student") && args["student"] != undo.Arguments["student"] {
		note = "every sheet: " + Forgotten
	}
	return tools.Output{Data: spreadArgs(nil, args, names), Summary: "Grades: " + changes(args, names),
		Undo: steps(undo), Note: note}, nil
}

func spreadMap(args map[string]any, names []string) map[string]any {
	out := map[string]any{}
	for _, name := range names {
		out[name] = args[name]
	}
	return out
}

func (c *Catalog) gradebooks() ([]settings.GradeSheet, error) {
	if c.A.Grades == nil {
		return nil, notRunning
	}
	return c.A.Grades.Settings.GradeSheets()
}

func (c *Catalog) gradebookOf(sid string) (settings.GradeSheet, error) {
	sheets, err := c.gradebooks()
	if err != nil {
		return settings.GradeSheet{}, err
	}
	for _, sheet := range sheets {
		if id, err := collector.SheetID(sheet.URL); err == nil && id == sid {
			return sheet, nil
		}
	}
	return settings.GradeSheet{}, refuse("no such gradebook: take the id from list_grades")
}

func sheetList(sheets []settings.GradeSheet) []any {
	out := []any{}
	for _, sheet := range sheets {
		out = append(out, actions.SheetMap(sheet))
	}
	return out
}

func (c *Catalog) addGradebook(t *tools.Context, args map[string]any) (tools.Output, error) {
	sheets, err := c.gradebooks()
	if err != nil {
		return tools.Output{}, err
	}
	label := any("")
	if has(args, "label") {
		label = args["label"]
	}
	fresh := httpx.Object{{Key: "url", Value: args["url"]}, {Key: "label", Value: label}}
	if truthy(args, "name") {
		fresh = append(fresh, httpx.Field{Key: "name", Value: args["name"]})
	}
	if _, err := c.call(c.A.UpdateGrades(ctxOf(t), map[string]any{
		"sheets": append(sheetList(sheets), objectMap(fresh))})); err != nil {
		return tools.Output{}, err
	}
	sid, err := collector.SheetID(str(args, "url"))
	if err != nil {
		return tools.Output{}, &tools.ToolError{Msg: i18n.Reason(err, "en")}
	}
	what := sid
	if truthy(args, "name") {
		what = str(args, "name")
	} else if s := actions.PyStr(label); s != "" {
		what = s
	}
	return tools.Output{Data: append(httpx.Object{{Key: "sheet_id", Value: sid}}, fresh...),
		Summary: "Added the gradebook " + what, Undo: steps(step("remove_gradebook", "sheet_id", sid))}, nil
}

func sheetName(sheet settings.GradeSheet) string {
	if sheet.Name != "" {
		return sheet.Name
	}
	return sheet.Label
}

func (c *Catalog) removeGradebook(t *tools.Context, args map[string]any) (tools.Output, error) {
	sid := str(args, "sheet_id")
	sheet, err := c.gradebookOf(sid)
	if err != nil {
		return tools.Output{}, err
	}
	sheets, err := c.gradebooks()
	if err != nil {
		return tools.Output{}, err
	}
	kept := []settings.GradeSheet{}
	for _, s := range sheets {
		if id, _ := collector.SheetID(s.URL); id != sid {
			kept = append(kept, s)
		}
	}
	if _, err := c.call(c.A.UpdateGrades(ctxOf(t), map[string]any{"sheets": sheetList(kept)})); err != nil {
		return tools.Output{}, err
	}
	undo := step("add_gradebook", "url", sheet.URL, "label", sheet.Label)
	if sheet.Name != "" {
		undo.Arguments["name"] = sheet.Name
	}
	return tools.Output{Data: httpx.Object{{Key: "removed", Value: sid}},
		Summary: "Removed the gradebook " + sheetName(sheet), Undo: steps(undo), Note: Forgotten}, nil
}

func (c *Catalog) updateGradebook(t *tools.Context, args map[string]any) (tools.Output, error) {
	sid := str(args, "sheet_id")
	sheet, err := c.gradebookOf(sid)
	if err != nil {
		return tools.Output{}, err
	}
	names := pick(args, "name", "label")
	if len(names) == 0 {
		return tools.Output{}, refuse("give name, label or both")
	}
	if _, err := c.call(c.A.UpdateGradebook(ctxOf(t), sid, spreadMap(args, names))); err != nil {
		return tools.Output{}, err
	}
	undo := step("update_gradebook", "sheet_id", sid)
	for _, name := range names {
		if name == "name" {
			undo.Arguments[name] = sheet.Name
		} else {
			undo.Arguments[name] = sheet.Label
		}
	}
	return tools.Output{Data: spreadArgs(httpx.Object{{Key: "sheet_id", Value: sid}}, args, names),
		Summary: "Gradebook " + sheetName(sheet) + ": " + changes(args, names), Undo: steps(undo)}, nil
}

func (c *Catalog) readGradebookNow(t *tools.Context, args map[string]any) (tools.Output, error) {
	sid := str(args, "sheet_id")
	if _, err := c.call(c.A.ReadGradebook(ctxOf(t), sid)); err != nil {
		return tools.Output{}, err
	}
	return tools.Output{Data: httpx.Object{{Key: "reading", Value: sid},
		{Key: "hint", Value: "call get_gradebook in a minute to see what was read"}},
		Summary: "Asked to read the gradebooks now (" + sid + ")", Note: "nothing to undo"}, nil
}

func (c *Catalog) ownLayout(sid, tab string) (*grades.Spec, error) {
	if c.A.Grades == nil {
		return nil, notRunning
	}
	var spec grades.Spec
	ok, err := c.A.Grades.Store.GetJSON(grades.OwnLayoutKey+sid+"|"+tab, &spec)
	if err != nil || !ok {
		return nil, err
	}
	return &spec, nil
}

func layoutWorks(spec *grades.Spec) []any {
	out := []any{}
	for _, item := range spec.Items {
		out = append(out, httpx.Object{{Key: "column", Value: item.Column}, {Key: "label", Value: item.Label},
			{Key: "max", Value: httpx.PyValue(item.Max)}, {Key: "bonus_column", Value: item.BonusColumn}})
	}
	return out
}

func (c *Catalog) setTabLayout(t *tools.Context, args map[string]any) (tools.Output, error) {
	sid, tab := str(args, "sheet_id"), str(args, "tab")
	before, err := c.ownLayout(sid, tab)
	if err != nil {
		return tools.Output{}, err
	}
	if _, err := c.call(c.A.SetLayout(ctxOf(t), sid, tab, map[string]any{"skip": args["skip"],
		"works": args["works"]})); err != nil {
		return tools.Output{}, err
	}
	undo := step("reset_tab_layout", "sheet_id", sid, "tab", tab)
	if before != nil {
		undo = step("set_tab_layout", "sheet_id", sid, "tab", tab, "skip", before.Skip, "works", layoutWorks(before))
	}
	what := "skipped"
	if args["skip"] != true {
		works, _ := args["works"].([]any)
		what = fmt.Sprintf("%d work(s)", len(works))
	}
	return tools.Output{Data: httpx.Object{{Key: "sheet_id", Value: sid}, {Key: "tab", Value: tab},
		{Key: "skip", Value: args["skip"]}}, Summary: "Laid out the tab «" + tab + "» by hand: " + what,
		Undo: steps(undo)}, nil
}

func (c *Catalog) resetTabLayout(t *tools.Context, args map[string]any) (tools.Output, error) {
	sid, tab := str(args, "sheet_id"), str(args, "tab")
	before, err := c.ownLayout(sid, tab)
	if err != nil {
		return tools.Output{}, err
	}
	if _, err := c.call(c.A.ClearLayout(ctxOf(t), sid, tab)); err != nil {
		return tools.Output{}, err
	}
	out := tools.Output{Data: httpx.Object{{Key: "sheet_id", Value: sid}, {Key: "tab", Value: tab}},
		Summary: "Gave the tab «" + tab + "» back to Haiku"}
	if before != nil {
		out.Undo = steps(step("set_tab_layout", "sheet_id", sid, "tab", tab, "skip", before.Skip,
			"works", layoutWorks(before)))
	} else {
		out.Note = "it had no own layout"
	}
	return out, nil
}

// gradeBefore is a grade as it is: one added by hand, or a sheet's with the owner's change.
type gradeBefore struct {
	course       *string
	label        string
	value        pyjson.Float
	own, edited  bool
	courseString string
}

func (c *Catalog) gradeBefore(gradeID string) (*gradeBefore, error) {
	g := c.A.Grades
	if g == nil {
		return nil, notRunning
	}
	manual, err := g.Manual()
	if err != nil {
		return nil, err
	}
	for _, grade := range manual {
		if grade.ID == gradeID {
			b := &gradeBefore{course: grade.Course, label: grade.Label, value: grade.Value, own: true}
			if grade.Course != nil {
				b.courseString = *grade.Course
			}
			return b, nil
		}
	}
	sheets, err := c.gradebooks()
	if err != nil {
		return nil, err
	}
	for _, sheet := range sheets {
		sid, err := collector.SheetID(sheet.URL)
		if err != nil {
			continue
		}
		items, err := g.Items(sid)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item.ID == gradeID {
				return &gradeBefore{label: item.Label, value: item.Value, edited: item.Edited}, nil
			}
		}
	}
	return nil, nil
}

func (c *Catalog) manualIDs() (map[string]bool, error) {
	manual, err := c.A.Grades.Manual()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, grade := range manual {
		out[grade.ID] = true
	}
	return out, nil
}

func (c *Catalog) addGrade(t *tools.Context, args map[string]any) (tools.Output, error) {
	if c.A.Grades == nil {
		return tools.Output{}, notRunning
	}
	before, err := c.manualIDs()
	if err != nil {
		return tools.Output{}, err
	}
	body := spreadArgs(nil, args, []string{"course", "label", "value"})
	if _, err := c.call(c.A.AddGrade(ctxOf(t), objectMap(body))); err != nil {
		return tools.Output{}, err
	}
	after, err := c.manualIDs()
	if err != nil {
		return tools.Output{}, err
	}
	fresh := []string{}
	for id := range after {
		if !before[id] {
			fresh = append(fresh, id)
		}
	}
	sort.Strings(fresh)
	undo := []tools.Step{}
	for _, id := range fresh {
		undo = append(undo, step("delete_grade", "grade_id", id))
	}
	return tools.Output{Data: append(httpx.Object{{Key: "grade_id", Value: first(fresh)}}, body...),
		Summary: "Added the grade " + actions.PyStr(args["course"]) + " " + actions.PyStr(args["label"]) + ": " +
			actions.PyStr(args["value"]), Undo: undo}, nil
}

func (c *Catalog) changeGrade(t *tools.Context, args map[string]any) (tools.Output, error) {
	gradeID := str(args, "grade_id")
	before, err := c.gradeBefore(gradeID)
	if err != nil {
		return tools.Output{}, err
	}
	if before == nil {
		return tools.Output{}, refuse("no such grade: take the id from list_grades")
	}
	names := pick(args, "value", "label", "course")
	body := spreadArgs(nil, args, names)
	if _, err := c.call(c.A.ChangeGrade(ctxOf(t), gradeID, objectMap(body))); err != nil {
		return tools.Output{}, err
	}
	var undo tools.Step
	switch {
	case before.own:
		undo = step("change_grade", "grade_id", gradeID, "value", before.value, "label", before.label,
			"course", before.course)
	case before.edited:
		undo = step("change_grade", "grade_id", gradeID, "value", before.value, "label", before.label)
	default:
		undo = step("delete_grade", "grade_id", gradeID)
	}
	return tools.Output{Data: append(httpx.Object{{Key: "grade_id", Value: gradeID}}, body...),
		Summary: "Changed the grade " + before.label + ": " + actions.PyStr(before.value) + " → " +
			actions.PyStr(args["value"]), Undo: steps(undo)}, nil
}

func (c *Catalog) deleteGrade(t *tools.Context, args map[string]any) (tools.Output, error) {
	gradeID := str(args, "grade_id")
	before, err := c.gradeBefore(gradeID)
	if err != nil {
		return tools.Output{}, err
	}
	if before == nil {
		return tools.Output{}, refuse("no such grade: take the id from list_grades")
	}
	if _, err := c.call(c.A.DropGrade(ctxOf(t), gradeID)); err != nil {
		return tools.Output{}, err
	}
	if before.own {
		out := tools.Output{Data: httpx.Object{{Key: "deleted", Value: gradeID}},
			Summary: "Deleted the grade " + before.label + " (" + actions.PyStr(before.value) + ")"}
		if before.courseString != "" {
			out.Undo = steps(step("add_grade", "course", before.courseString, "label", before.label,
				"value", before.value))
		} else {
			out.Note = "it had no course"
		}
		return out, nil
	}
	out := tools.Output{Data: httpx.Object{{Key: "reset", Value: gradeID}},
		Summary: "The grade " + before.label + " is back as the sheet has it"}
	if before.edited {
		out.Undo = steps(step("change_grade", "grade_id", gradeID, "value", before.value, "label", before.label))
	} else {
		out.Note = "it was not changed"
	}
	return out, nil
}
