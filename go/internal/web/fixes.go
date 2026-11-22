package web

// The corrections of the web app (§8.6, stage 16.9, D68): moving, cancelling and changing a class, changing, hiding
// and deleting an event, the main or the optional calendar, the hidden deadlines and events with «Restore», and
// correcting grades. The port of web/fixes.py.
//
// What stands behind a class or an event comes with its card (Data.Fix), with every field as it is now. The changes
// are the connector's write tools, journaled with `via` web and their undo:
//
//   - a RUZ class: correct_class moves, cancels or renames it and puts it in the main or the optional calendar; the
//     tool resets a field it is not given, so the app sends all of them; «As it was» takes the own correction back
//     (remove_event); the chat's correction of the class is ignored or applied again with set_event_options
//     (hidden), which moves the class's Midterm with it;
//   - an event: edit_event changes it (a chat's one is hidden and an own copy stands for it), set_event_options hides
//     it or picks its calendar, remove_event deletes an own one or takes an own copy back;
//   - the hidden lists: unhide_deadline, and set_event_options with hidden false;
//   - a grade: change_grade, and delete_grade, which gives a sheet's grade back as the sheet has it and deletes one
//     added by hand.

import (
	"net/http"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
)

var (
	classFields = []string{"start", "end", "room", "cancelled", "kind", "required", "title"}
	eventFields = []string{"title", "kind", "start", "end", "room", "cancelled"}
)

func (w *WebApp) fixRoutes() []route {
	return []route{
		{"POST", "/class", false, fixClass},
		{"POST", "/event", false, fixEvent},
		{"POST", "/options", false, eventOptions},
		{"POST", "/remove", false, removeEvent},
		{"GET", "/hidden", false, hidden},
		{"POST", "/hidden/deadline", false, unhideDeadline},
		{"POST", "/grade", false, grade},
		{"POST", "/grade/reset", false, resetGrade},
	}
}

// fixAct is a change through the connector's tool, journaled; the answer is the change, for its undo.
func fixAct(c *call, tool string, arguments map[string]any) error {
	connector := c.app.Connector
	if connector == nil || connector.Registry.Tool(tool) == nil {
		return httpx.Fail(http.StatusServiceUnavailable, "changes are not wired")
	}
	id, output, err := connector.Act(c.ctx(), tool, arguments, "web")
	if text, refused := tools.Refusal(err); refused {
		return httpx.BadRequest(text)
	}
	if err != nil {
		return err
	}
	return c.json(httpx.Object{{Key: "change", Value: change(id, output)}})
}

func fixUID(values map[string]any) (string, error) {
	uid, ok := values["uid"].(string)
	if !ok || strings.TrimFunc(uid, isSpace) == "" {
		return "", httpx.BadRequest("uid expected")
	}
	return strings.TrimFunc(uid, isSpace), nil
}

func eventID(values map[string]any) (int64, error) {
	id, ok := values["id"].(int64)
	if !ok {
		return 0, httpx.BadRequest("id: the event's id expected")
	}
	return id, nil
}

func (c *call) fix(uid string) (string, any, error) {
	data, err := c.data()
	if err != nil {
		return "", nil, err
	}
	entry, err := data.entry(uid)
	if err != nil {
		return "", nil, err
	}
	var found any
	if entry != nil {
		if found, err = data.Fix(*entry); err != nil {
			return "", nil, err
		}
	}
	o, _ := found.(httpx.Object)
	if o == nil {
		return "", nil, httpx.NotFound("no such class or event")
	}
	return str(o.Get("of")), o.Get("id"), nil
}

func others(values map[string]any, allowed ...string) bool {
	for key := range values {
		known := false
		for _, name := range allowed {
			known = known || key == name
		}
		if !known {
			return true
		}
	}
	return false
}

// fixClass is {uid, start, end, room, cancelled, kind, required, title?}: the owner's correction of a RUZ class,
// every field as the card gave it with the owner's changes; the connector's correct_class.
func fixClass(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	uid, err := fixUID(values)
	if err != nil {
		return err
	}
	of, _, err := c.fix(uid)
	if err != nil {
		return err
	}
	if of != "class" {
		return httpx.BadRequest("this is an event, not a class of RUZ")
	}
	if others(values, append([]string{"uid"}, classFields...)...) {
		return httpx.BadRequest("expected uid and " + strings.Join(classFields, ", "))
	}
	arguments := map[string]any{"class_uid": uid}
	for _, name := range classFields {
		if value, ok := values[name]; ok {
			arguments[name] = value
		}
	}
	return fixAct(c, "correct_class", arguments)
}

// fixEvent is {uid, some of title, kind, start, end, room, cancelled}: a change of an event; a chat's one is hidden
// and an own copy stands for it. The connector's edit_event.
func fixEvent(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	uid, err := fixUID(values)
	if err != nil {
		return err
	}
	of, id, err := c.fix(uid)
	if err != nil {
		return err
	}
	if of != "event" {
		return httpx.BadRequest("a class of RUZ is changed as a class")
	}
	arguments := map[string]any{"event_id": id}
	for _, name := range eventFields {
		if value, ok := values[name]; ok {
			arguments[name] = value
		}
	}
	if len(arguments) == 1 || others(values, append([]string{"uid"}, eventFields...)...) {
		return httpx.BadRequest("expected uid and some of " + strings.Join(eventFields, ", "))
	}
	return fixAct(c, "edit_event", arguments)
}

// eventOptions is {id, required?, hidden?}: an event hidden or shown again, in the main or the optional calendar; a
// chat's correction of a class ignored or applied again. The connector's set_event_options.
func eventOptions(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	arguments := map[string]any{}
	for _, name := range []string{"required", "hidden"} {
		if value, ok := values[name]; ok {
			arguments[name] = value
		}
	}
	if len(arguments) == 0 || others(values, "id", "required", "hidden") {
		return httpx.BadRequest("expected {id, required?, hidden?}")
	}
	id, err := eventID(values)
	if err != nil {
		return err
	}
	arguments["event_id"] = id
	return fixAct(c, "set_event_options", arguments)
}

// removeEvent is {id}: an own event deleted, an own correction or an own copy taken back. The connector's
// remove_event.
func removeEvent(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	id, err := eventID(values)
	if err != nil {
		return err
	}
	return fixAct(c, "remove_event", map[string]any{"event_id": id})
}

// hidden is the hidden deadlines and the hidden events from today on.
func hidden(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	found, err := data.Hidden()
	if err != nil {
		return err
	}
	return c.json(found)
}

// unhideDeadline is {uid}: a hidden deadline back in the list. The connector's unhide_deadline.
func unhideDeadline(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	uid, err := fixUID(values)
	if err != nil {
		return err
	}
	return fixAct(c, "unhide_deadline", map[string]any{"uid": uid})
}

// grade is {id, value, label?}: a grade changed by the owner. The connector's change_grade.
func grade(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	id, ok := values["id"].(string)
	if !ok || id == "" {
		return httpx.BadRequest("id: the grade's id expected")
	}
	if _, has := values["value"]; !has || others(values, "id", "value", "label") {
		return httpx.BadRequest("expected {id, value, label?}")
	}
	arguments := map[string]any{"grade_id": id, "value": values["value"]}
	if truthy(values["label"]) {
		arguments["label"] = values["label"]
	}
	return fixAct(c, "change_grade", arguments)
}

// resetGrade is {id}: a sheet's grade back as the sheet has it; a grade added by hand deleted. The connector's
// delete_grade.
func resetGrade(c *call) error {
	values, err := c.body()
	if err != nil {
		return err
	}
	id, ok := values["id"].(string)
	if !ok || id == "" {
		return httpx.BadRequest("id: the grade's id expected")
	}
	return fixAct(c, "delete_grade", map[string]any{"grade_id": id})
}
