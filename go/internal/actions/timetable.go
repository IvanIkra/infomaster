package actions

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

var dayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// agendaState is the timetable from today on with the RUZ status and whether the feeds are on.
func (a *Actions) agendaState() (any, error) {
	today := clock.LocalDay(a.Schedule.Now())
	ruz, err := a.required("ruz")
	if err != nil {
		return nil, err
	}
	agenda, err := a.Schedule.Agenda(today, schedule.AgendaDays)
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "today", Value: today.Format(clock.DateLayout)}, {Key: "ruz", Value: ruz},
		{Key: "calendar", Value: a.Env.CalendarSecret != "" || a.Env.CalendarPublicSecret != ""},
		{Key: "items", Value: agenda.Items}, {Key: "hidden", Value: agenda.Hidden}}, nil
}

// Agenda is the timetable: the classes with their corrections and the events, as the feeds have them, from today
// on; every change answers it anew.
func (a *Actions) Agenda(_ context.Context) (any, error) { return a.agendaState() }

// moment is a naive local time the owner gives.
func (a *Actions) moment(value any) (string, error) {
	s, _ := value.(string)
	moment, zoned, err := fromISO(s)
	if err != nil || zoned {
		return "", a.badRequest("a time like 2026-10-22T10:00", "время вида 2026-10-22T10:00")
	}
	return isoSeconds(moment), nil
}

// eventFields are the checked fields of an event, the ones named: title, kind, start and end (Moscow), room,
// required, cancelled. End "" is none (all day).
type eventFields struct {
	names                            map[string]bool
	title, kind, start, end, room    string
	required, cancelled              bool
	hasRequired, hasCancelled, dated bool
}

func (a *Actions) eventFields(body map[string]any, names map[string]bool) (*eventFields, error) {
	f := &eventFields{names: names}
	if names["title"] {
		title, ok := body["title"].(string)
		if !ok || squeeze(title) == "" || pytext.Len(title) > EventTitleLimit {
			return nil, a.badRequest(fmt.Sprintf("a title is needed, up to %d characters", EventTitleLimit),
				fmt.Sprintf("нужно название, до %d символов", EventTitleLimit))
		}
		f.title = squeeze(title)
	}
	if names["kind"] {
		kind, ok := body["kind"].(string)
		if !ok || pytext.Strip(kind) == "" || pytext.Len(kind) > KindLimit {
			return nil, httpx.BadRequest(fmt.Sprintf("kind: a word up to %d characters, as Event or Consultation",
				KindLimit))
		}
		f.kind = pytext.Strip(kind)
	}
	if names["start"] || names["end"] {
		f.dated = true
		day, _ := body["start"].(string)
		if dayPattern.MatchString(day) { // all day: a date, no end (D46)
			if truthy(body["end"]) {
				return nil, a.badRequest("an all-day event has no end", "у события на весь день нет конца")
			}
			parsed, err := fromISODate(day)
			if err != nil {
				return nil, a.badRequest("a date like 2026-10-22", "дата вида 2026-10-22")
			}
			f.start, f.end = parsed.Format(clock.DateLayout), ""
		} else {
			start, err := a.moment(body["start"])
			if err != nil {
				return nil, err
			}
			end, err := a.moment(body["end"])
			if err != nil {
				return nil, err
			}
			if end <= start {
				return nil, a.badRequest("the end comes after the start", "конец — позже начала")
			}
			f.start, f.end = start, end
		}
	}
	if names["room"] {
		room, ok := body["room"]
		if !ok {
			room = ""
		}
		s, isText := room.(string)
		if !isText || pytext.Len(s) > RoomLimit {
			return nil, httpx.BadRequest(fmt.Sprintf("room: up to %d characters", RoomLimit))
		}
		f.room = pytext.Strip(s)
	}
	for _, flag := range []string{"required", "cancelled"} {
		if !names[flag] {
			continue
		}
		value, ok := body[flag].(bool)
		if !ok {
			return nil, httpx.BadRequest(flag + ": true or false")
		}
		if flag == "required" {
			f.required, f.hasRequired = value, true
		} else {
			f.cancelled, f.hasCancelled = value, true
		}
	}
	return f, nil
}

func set(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		out[name] = true
	}
	return out
}

// eventID is the event of a path: 404 for no number.
func eventID(text string) (int64, error) {
	id, err := pyInt(text)
	if err != nil {
		return 0, httpx.NotFound("")
	}
	return id, nil
}

// AddEvent is {title, kind, start, end, room, required, cancelled?}: an own event; a start that is a date alone and
// no end make it an all-day one.
func (a *Actions) AddEvent(_ context.Context, body map[string]any) (any, error) {
	names := set("title", "kind", "start", "end", "room", "required")
	if _, ok := body["cancelled"]; ok {
		names["cancelled"] = true
	}
	f, err := a.eventFields(body, names)
	if err != nil {
		return nil, err
	}
	s := a.Schedule
	if _, err := s.Events.Add(events.New{UID: events.OwnUID(), Source: events.Own, Title: f.title, Kind: f.kind,
		Start: f.start, End: f.end, Room: f.room, Required: f.required, Cancelled: f.cancelled, Chosen: true},
		s.Now()); err != nil {
		return nil, err
	}
	return a.agendaState()
}

// UpdateEvent is {required?, hidden?} for the newest version of an event, as the web app's toggles; or some of
// {title, kind, start, end, room, required, cancelled}: the owner's change of an event of its own (not a correction).
func (a *Actions) UpdateEvent(ctx context.Context, eventIDText string, body map[string]any) (any, error) {
	id, err := eventID(eventIDText)
	if err != nil {
		return nil, err
	}
	timetable := a.Schedule
	event, err := timetable.Events.Current(id)
	if err != nil {
		return nil, err
	}
	if event == nil || event.GoneAt != nil {
		return nil, httpx.NotFound("no such event")
	}
	switch {
	case len(body) > 0 && only(body, "required", "hidden"):
		for _, value := range body {
			if _, ok := value.(bool); !ok {
				return nil, httpx.BadRequest("expected {required: true|false, hidden: true|false}")
			}
		}
		if _, ok := body["required"]; ok && event.ClassUID != nil {
			return nil, a.badRequest("a class of RUZ is always in the main calendar",
				"пара из РУЗ всегда в основном календаре")
		}
		if required, ok := body["required"].(bool); ok {
			if _, err := timetable.Events.SetRequired(id, required); err != nil {
				return nil, err
			}
		}
		if hidden, ok := body["hidden"].(bool); ok { // a correction of an interim assessment moves its Midterm
			if _, err := timetable.SetHidden(ctx, id, hidden); err != nil {
				return nil, err
			}
		}
	case len(body) > 0 && only(body, EventFields...):
		if event.ClassUID != nil {
			return nil, a.badRequest("a class is moved or cancelled as a correction",
				"пару переносят или отменяют правкой")
		}
		names := set(keys(body)...)
		if names["start"] || names["end"] {
			names["start"], names["end"] = true, true
		}
		f, err := a.eventFields(body, names)
		if err != nil {
			return nil, err
		}
		if _, err := timetable.EditEvent(id, f.change()); err != nil {
			return nil, err
		}
	default:
		return nil, httpx.BadRequest("expected {required?, hidden?} or some of " + strings.Join(EventFields, ", "))
	}
	return a.agendaState()
}

func (f *eventFields) change() schedule.Change {
	var c schedule.Change
	if f.names["title"] {
		c.Title = &f.title
	}
	if f.names["kind"] {
		c.Kind = &f.kind
	}
	if f.dated {
		c.Start, c.End = &f.start, &f.end
	}
	if f.names["room"] {
		c.Room = &f.room
	}
	if f.hasRequired {
		c.Required = &f.required
	}
	if f.hasCancelled {
		c.Cancelled = &f.cancelled
	}
	return c
}

// DeleteEvent deletes an own event or undoes an own correction; the chats' events are hidden instead.
func (a *Actions) DeleteEvent(ctx context.Context, eventIDText string) (any, error) {
	id, err := eventID(eventIDText)
	if err != nil {
		return nil, err
	}
	removed, err := a.Schedule.RemoveOwn(ctx, id)
	if err != nil {
		return nil, err
	}
	if removed == nil {
		return nil, httpx.NotFound("no such own event")
	}
	return a.agendaState()
}

// CorrectClass is {class_uid, start, end, room, cancelled, title?, kind?, required?}: the owner changes a RUZ class
// as any other entry (D49): moves, cancels, renames it, gives it another type or the optional calendar.
func (a *Actions) CorrectClass(ctx context.Context, body map[string]any) (any, error) {
	names := set("start", "end", "room", "cancelled")
	for _, name := range []string{"title", "kind", "required"} {
		if _, ok := body[name]; ok {
			names[name] = true
		}
	}
	f, err := a.eventFields(body, names)
	if err != nil {
		return nil, err
	}
	classUID, ok := body["class_uid"].(string)
	if !ok {
		return nil, httpx.NotFound("no such class")
	}
	var title, kind *string
	if names["title"] {
		title = &f.title
	}
	if names["kind"] {
		kind = &f.kind
	}
	required := true
	if f.hasRequired {
		required = f.required
	}
	id, err := a.Schedule.CorrectClass(ctx, classUID, f.start, f.end, f.room, f.cancelled, title, kind, required)
	if err != nil {
		return nil, err
	}
	if id == nil {
		return nil, httpx.NotFound("no such class")
	}
	return a.agendaState()
}

// --- a source message (D50) --------------------------------------------------------------------------

func messageView(m *inbox.Message) httpx.Object {
	files := []httpx.Object{}
	for _, f := range m.Files {
		files = append(files, httpx.Object{{Key: "kind", Value: f["kind"]}, {Key: "name", Value: f["name"]}})
	}
	var forward any
	if m.Forward != nil {
		forward = m.Forward["from"]
	}
	return httpx.Object{{Key: "sender", Value: m.SenderName}, {Key: "at", Value: m.At}, {Key: "text", Value: m.Text},
		{Key: "files", Value: files}, {Key: "forward", Value: forward}, {Key: "edited", Value: m.EditedAt != nil},
		{Key: "deleted", Value: m.DeletedAt != nil}}
}

// nonEmpty is a file's field as Python's dict.get gives it: None when it is not there.
func nonEmpty(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// Message is ?chat=<chat_id>&msg=<msg_id>: a message as the twin read it (a letter for chat 0), with what it answers
// and its own t.me link, for a chat your own account is not in. Kept 14 days (D29): after that only the chat and the
// link. An empty text is a parameter not given.
func (a *Actions) Message(_ context.Context, chat, msg string) (any, error) {
	chatID, err1 := pyInt(chat)
	msgID, err2 := pyInt(msg)
	if err1 != nil || err2 != nil {
		return nil, httpx.BadRequest("expected ?chat=<id>&msg=<id>")
	}
	source := "tg"
	if chatID == inbox.Mailbox {
		source = "mail"
	}
	var found *inbox.Chat
	if source == "tg" {
		if found, err1 = a.Inbox.Chat(chatID); err1 != nil {
			return nil, err1
		}
		if found == nil {
			return nil, httpx.NotFound("no such chat")
		}
	}
	stored, err := a.Inbox.Message(chatID, msgID, source)
	if err != nil {
		return nil, err
	}
	var about, link, subject, view, reply any
	if found != nil {
		about = httpx.Object{{Key: "id", Value: found.ID}, {Key: "title", Value: found.Title},
			{Key: "kind", Value: found.Kind}}
		link = nonEmpty(inbox.MessageLink(found, msgID))
	}
	if stored != nil {
		subject, view = stored.Subject, messageView(stored)
		if stored.ReplyTo != nil && *stored.ReplyTo != 0 {
			replied, err := a.Inbox.Message(chatID, *stored.ReplyTo, source)
			if err != nil {
				return nil, err
			}
			if replied != nil {
				reply = messageView(replied)
			} else {
				quote := ""
				if stored.ReplyQuote != nil {
					quote = *stored.ReplyQuote
				}
				reply = httpx.Object{{Key: "text", Value: quote}, {Key: "sender", Value: nil}}
			}
		} else if stored.ReplyQuote != nil && *stored.ReplyQuote != "" {
			reply = httpx.Object{{Key: "text", Value: *stored.ReplyQuote}, {Key: "sender", Value: nil}}
		}
	}
	return httpx.Object{{Key: "source", Value: source}, {Key: "chat", Value: about}, {Key: "link", Value: link},
		{Key: "stored", Value: stored != nil}, {Key: "subject", Value: subject}, {Key: "message", Value: view},
		{Key: "reply", Value: reply}}, nil
}
