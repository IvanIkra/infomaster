// Package schedule is the timetable (§6.5): RUZ classes, events from chats and the chat corrections of classes.
//
// One list of entries serves the iCal feeds, the batch prompt and the web app:
//   - a RUZ class is always in the main calendar. A correction in force from a chat applies to it: a cancellation
//     makes it cancelled, a move gives it the new time and room with a note. RUZ wins: when RUZ itself changes or
//     removes the class, its corrections are gone (AfterPoll);
//   - an event of its own goes to the main or the optional calendar by `required` (D28), unless the owner hid it.
//
// The owner adds events of their own and corrects classes in the web app (D44): an own correction wins over the
// chats' ones until it is undone or RUZ changes the class; a change of an event from a chat hides it and puts an own
// copy in its place, so the chat's next message changes only the hidden one (as a changed deadline does, §5.4). Every
// entry is changed the same way (D49): title, type, day or whole day, time, place, calendar, cancelled. An own
// correction of a class keeps a title only when the owner renamed the class ("" follows the dictionary) and the
// class's own kind unless the owner chose another type. An own copy remembers the chat's event it stands in for
// (`copy_of`): undoing it brings that one back.
//
// Titles are English: a class takes its subject's title from the dictionary (the feed's name until there is one), an
// event has the model's own short title. Types are the words Tablo reads from the first line of DESCRIPTION (§4.2):
// Lecture, Seminar, Consultation, Event, Interim assessment, and a few more for kinds that are none of these.
//
// After every RUZ poll the subjects are named (only names not known yet go to Haiku) and each interim assessment keeps
// its deadline "<abbr> Midterm" in `deadlines` with source "ical" (§4.4): added, moved, renamed once the dictionary
// knows the course, withdrawn when the class is cancelled or removed. It follows the class as the feeds show it, so a
// chat's correction in force moves or withdraws it too: the Midterms are synced after every RUZ poll, every batch run,
// every hiding of a correction by the owner and every change of the dictionary by hand (its abbr starts the label).
// The merge with the other sources is processing/merge's.
//
// A sheet without a due date is due before the next seminar of its course (processing/sheets, §6.3): NextClass finds
// it in the timetable as the feeds show it, and the sheets follow it as the Midterms follow theirs (Followers, called
// wherever the Midterms are synced).
//
// An event whose day is told but not its time ("tomorrow a consultation in S301") is an all-day one: its start is a
// date alone, "2026-11-06", and it has no end; the feeds give it a date and no time.
//
// A chat's event that only repeats a class of the timetable (the model wrote "Calc seminar" for the Calculus seminar
// at its own time) is no event: a regular kind (a lecture, a seminar, a lab, a class) starting while a class of the
// same course, title or room goes on, as the feeds show it (RepeatsClass). The batches do not save such an event, and
// the feeds leave out one saved before or one whose class came to RUZ later. Own events are the owner's: they always
// stay.
package schedule

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// DefaultLength is an event whose end nobody said: one HSE class period.
const DefaultLength = 80 * time.Minute

// ClassDays are the days of classes shown to the model, from today on: moves and cancellations come close to the date.
const ClassDays = 21

// SheetDays: a sheet's next seminar is looked for this long after its message (§6.3).
const SheetDays = 21

// AgendaDays is the web app's timetable, from today on.
const AgendaDays = 28

// Seminars are where a sheet is handed in: seminars, practical classes, tutorials (TypeOf).
var Seminars = []string{"Seminar"}

// NotClasses are never a sheet's class, even of a course without seminars.
var NotClasses = []string{"Interim assessment", "Exam"}

// MidtermKinds are the kinds of an interim assessment (§4.4).
var MidtermKinds = []string{"рубежный контроль", "interim assessment"}

// Regular are the types of an event that may only repeat a class.
var Regular = []string{"Lecture", "Seminar", "Lab", "Class"}

// Types are the words of a kind, from RUZ (Russian or English) or from the model -> the type; the first match wins.
var Types = []struct {
	Words []string
	Name  string
}{
	{[]string{"рубежный контроль", "interim assessment"}, "Interim assessment"},
	{[]string{"экзамен", "зачёт", "зачет", "exam"}, "Exam"},
	{[]string{"консультац", "consultation"}, "Consultation"},
	{[]string{"лекци", "lecture"}, "Lecture"},
	{[]string{"семинар", "практическ", "seminar", "practical", "practice", "tutorial"}, "Seminar"},
	{[]string{"лаборатор", "lab"}, "Lab"},
	{[]string{"мероприят", "событ", "event"}, "Event"},
}

// TypeOf is the first line of DESCRIPTION for a kind; an English kind of none of the types keeps its word.
func TypeOf(kind string, ofClass bool) string {
	folded := pytext.Casefold(kind)
	for _, t := range Types {
		for _, word := range t.Words {
			if strings.Contains(folded, word) {
				return t.Name
			}
		}
	}
	if stripped := pytext.Strip(folded); stripped != "" && isASCII(folded) {
		return strings.ToUpper(stripped[:1]) + strings.ToLower(stripped[1:])
	}
	if ofClass {
		return "Class"
	}
	return "Event"
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// AllDay: an event told with its day but not its time has a start of a date alone.
func AllDay(start string) bool { return pytext.Len(start) == 10 }

// Midterm tells an interim assessment by its kind.
func Midterm(kind string) bool {
	folded := pytext.Casefold(kind)
	for _, word := range MidtermKinds {
		if strings.Contains(folded, word) {
			return true
		}
	}
	return false
}

// Entry is one VEVENT of our feeds.
type Entry struct {
	UID       string
	Title     string
	Start     time.Time // Moscow local
	End       time.Time
	Type      string
	Location  string // a room or a link
	Required  bool   // the main calendar, else the optional one
	Cancelled bool
	Source    string    // ruz, tg, mail, pdf, own
	Link      string    // the source message; "" for none
	Notes     []string  // DESCRIPTION after the type
	AllDay    bool      // an event without a time: start is the day's midnight, end the next one's
	OfClass   bool      // a RUZ class: its own calendar marks its type in the title
	Abbr      string    // a class's course abbreviation: an event may name the class by it; "" for none
	Origin    string    // who told it, in English: a chat's name, a letter's subject; "" for RUZ itself
	Message   *[2]int64 // the message that told it, (chat_id, msg_id)
}

// MidtermChanges is what a sync did to the Midterm deadlines, as no news.
type MidtermChanges struct {
	Deadlines []int64
	Moved     [][2]int64
	Gone      []int64
}

// Any is Python's bool() of the changes.
func (m MidtermChanges) Any() bool {
	return len(m.Deadlines) > 0 || len(m.Moved) > 0 || len(m.Gone) > 0
}

// Told is any row with source, chat_id and msg_ids: an event, a correction, a deadline.
type Told struct {
	Source string
	ChatID *int64
	MsgIDs []int64
}

// OfEvent is the Told of an event.
func OfEvent(e *events.Event) Told { return Told{e.Source, e.ChatID, e.MsgIDs} }

// OfDeadline is the Told of a deadline.
func OfDeadline(d *deadlines.Deadline) Told { return Told{d.Source, d.ChatID, d.MsgIDs} }

// ToldIn is the message that told an event, a correction or a deadline: (chat_id, its first msg_id).
func ToldIn(row Told) *[2]int64 {
	if row.ChatID == nil || len(row.MsgIDs) == 0 {
		return nil
	}
	return &[2]int64{*row.ChatID, row.MsgIDs[0]}
}

// MessageRef is ToldIn as {chat, msg}, for the connector and the web app (D50).
type MessageRef struct {
	Chat int64 `json:"chat"`
	Msg  int64 `json:"msg"`
}

// Ref is ToldIn as a MessageRef; nil for none.
func Ref(row Told) *MessageRef {
	told := ToldIn(row)
	if told == nil {
		return nil
	}
	return &MessageRef{told[0], told[1]}
}

// Local reads a stored local time (or a date: its midnight).
func Local(text string) (time.Time, error) { return clock.ParseLocal(text) }

func mustLocal(text string) time.Time {
	t, err := Local(text)
	if err != nil {
		slog.Warn("a time of the timetable cannot be read", "value", text)
	}
	return t
}

// When is 'Tue Oct 20 09:30': notes of the feed are English.
func When(moment time.Time) string { return moment.In(clock.TZ).Format("Mon Jan 2 15:04") }

func iso(t time.Time) string { return t.In(clock.TZ).Format(clock.LocalLayout) }

func day(t time.Time) string { return t.In(clock.TZ).Format(clock.DateLayout) }

func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func sameEnd(end *string, other string) bool { return end != nil && *end == other }

// MovedBack: a chat moved the class back where RUZ has it, so the correction changes nothing.
func MovedBack(item *ruz.Row, correction *events.Event) bool {
	return !correction.Cancelled && correction.Start == item.Start && sameEnd(correction.End, item.End) &&
		correction.Room == item.Room
}

// Overrides: an own correction renames the class, changes its type or puts it in the optional calendar.
func Overrides(item *ruz.Row, correction *events.Event) bool {
	return correction.Source == events.Own && (correction.Title != "" || !correction.Required ||
		TypeOf(correction.Kind, true) != TypeOf(item.Kind, true))
}

// Applies: the correction changes the class in the feeds: not hidden by the owner, the class not cancelled by RUZ
// itself, and not a move back to where RUZ has it (unless the owner changed something else).
func Applies(item *ruz.Row, correction *events.Event) bool {
	return correction != nil && !correction.Hidden && !item.Cancelled &&
		(!MovedBack(item, correction) || Overrides(item, correction))
}

// Location is LOCATION: the room, or the meeting link of an online class (Tablo reads a link as online, §4.2).
func Location(room string, link *string) string {
	if room == "online" {
		if link != nil && *link != "" {
			return *link
		}
		return room
	}
	return room
}

// Schedule is the timetable.
type Schedule struct {
	db        *sql.DB
	Ruz       *ruz.Ruz
	Events    *events.Events
	Deadlines *deadlines.Deadlines
	Subjects  *subjects.Subjects
	Inbox     *inbox.Inbox
	Now       func() time.Time
	// OnMidterms hears of the Midterms moved by the owner's choice (hiding a correction), as no news; a RUZ poll and
	// a batch run hand theirs over with their own changes.
	OnMidterms func(ctx context.Context, changes MidtermChanges) error
	// Followers are other deadlines that follow the timetable, synced with the Midterms: the sheets' assumed dates.
	Followers []func() (MidtermChanges, error)
}

// New makes the timetable.
func New(s *store.Store, r *ruz.Ruz, ev *events.Events, dl *deadlines.Deadlines, sub *subjects.Subjects,
	in *inbox.Inbox) *Schedule {
	return &Schedule{db: s.DB, Ruz: r, Events: ev, Deadlines: dl, Subjects: sub, Inbox: in, Now: clock.NowUTC}
}

// --- entries ----------------------------------------------------------------------

// Entries are every class not removed by RUZ and every event in force that is not hidden, by start.
func (s *Schedule) Entries() ([]Entry, error) {
	classes, err := s.ClassEntries()
	if err != nil {
		return nil, err
	}
	inForce, err := s.Events.InForceAll()
	if err != nil {
		return nil, err
	}
	entries := slices.Clone(classes)
	for _, event := range inForce {
		if event.ClassUID != nil || event.Hidden {
			continue
		}
		repeats, err := s.RepeatsClass(event, classes)
		if err != nil {
			return nil, err
		}
		if repeats {
			continue
		}
		entry, err := s.EventEntry(event)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	slices.SortStableFunc(entries, func(a, b Entry) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return strings.Compare(a.Title, b.Title)
	})
	return entries, nil
}

// ClassEntries are the classes not removed by RUZ, each with its correction in force.
func (s *Schedule) ClassEntries() ([]Entry, error) {
	corrections, err := s.Events.Corrections()
	if err != nil {
		return nil, err
	}
	classes, err := s.Ruz.Classes("", "9999")
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(classes))
	for _, item := range classes {
		entry, err := s.ClassEntry(item, corrections[item.UID])
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// RepeatsClass: a chat's event of a regular kind that starts while a class of the same course, title or room goes on
// only repeats the class. Own events, corrections and all-day events never do.
func (s *Schedule) RepeatsClass(event *events.Event, classes []Entry) (bool, error) {
	if event.Source == events.Own || event.ClassUID != nil || AllDay(event.Start) ||
		!slices.Contains(Regular, TypeOf(event.Kind, false)) {
		return false, nil
	}
	moment, room := mustLocal(event.Start), pytext.Casefold(event.Room)
	course, hasCourse := "", false
	if event.CourseName() != "" {
		title, err := s.Subjects.Title(event.CourseName())
		if err != nil {
			return false, err
		}
		course, hasCourse = title, true
	}
	words := pytext.Split(pytext.Casefold(event.Title))
	for _, entry := range classes {
		if entry.Cancelled || moment.Before(entry.Start) || !moment.Before(entry.End) {
			continue
		}
		if (hasCourse && course == entry.Title) || deadlines.SameLabel(event.Title, entry.Title) ||
			(entry.Abbr != "" && len(words) > 0 && words[0] == pytext.Casefold(entry.Abbr)) ||
			(room != "" && room == pytext.Casefold(entry.Location)) {
			return true, nil
		}
	}
	return false, nil
}

// ClassTitle is a class's English title from the dictionary.
func (s *Schedule) ClassTitle(item *ruz.Row) (string, error) { return s.Subjects.Title(item.Subject) }

// ClassEntry is a class as the feeds show it, with its correction (nil for none).
func (s *Schedule) ClassEntry(item *ruz.Row, correction *events.Event) (Entry, error) {
	title, err := s.ClassTitle(item)
	if err != nil {
		return Entry{}, err
	}
	abbr, err := s.Subjects.Abbr(item.Subject)
	if err != nil {
		return Entry{}, err
	}
	entry := Entry{UID: item.UID, Title: title, Start: mustLocal(item.Start), End: mustLocal(item.End),
		Type: TypeOf(item.Kind, true), Location: Location(item.Room, item.Link), Required: true,
		Cancelled: item.Cancelled, Source: "ruz", Notes: []string{}, OfClass: true, Abbr: abbr}
	if !Applies(item, correction) {
		return entry, nil
	}
	told := OfEvent(correction)
	if entry.Link, err = s.Link(told); err != nil {
		return Entry{}, err
	}
	if entry.Origin, err = s.Origin(told); err != nil {
		return Entry{}, err
	}
	entry.Source, entry.Message = correction.Source, ToldIn(told)
	by := "in a chat"
	if correction.Source == events.Own {
		by = "by hand"
		if correction.Title != "" {
			entry.Title = correction.Title
		}
		entry.Type, entry.Required = TypeOf(correction.Kind, true), correction.Required
	}
	if correction.Cancelled {
		entry.Cancelled = true
		entry.Notes = append(entry.Notes, "Cancelled "+by)
		return entry, nil
	}
	sameTime := correction.Start == item.Start && sameEnd(correction.End, item.End)
	if !sameTime {
		entry.Notes = append(entry.Notes, fmt.Sprintf("Moved from %s %s", When(entry.Start), by))
		entry.Start, entry.AllDay = mustLocal(correction.Start), AllDay(correction.Start)
		switch {
		case entry.AllDay:
			entry.End = entry.Start.AddDate(0, 0, 1)
		case correction.EndText() != "":
			entry.End = mustLocal(*correction.End)
		default:
			entry.End = entry.Start.Add(DefaultLength)
		}
	}
	if correction.Room != item.Room {
		entry.Location = correction.Room
		if sameTime {
			entry.Notes = append(entry.Notes, "Room changed "+by)
		}
	} else if sameTime {
		entry.Notes = append(entry.Notes, "Changed "+by)
	}
	return entry, nil
}

// EventEntry is an event of its own as the feeds show it.
func (s *Schedule) EventEntry(event *events.Event) (Entry, error) {
	start, wholeDay := mustLocal(event.Start), AllDay(event.Start)
	end := start.Add(DefaultLength)
	if wholeDay {
		end = start.AddDate(0, 0, 1)
	} else if event.EndText() != "" {
		end = mustLocal(*event.End)
	}
	told := OfEvent(event)
	link, err := s.Link(told)
	if err != nil {
		return Entry{}, err
	}
	origin, err := s.Origin(told)
	if err != nil {
		return Entry{}, err
	}
	entry := Entry{UID: event.UID, Title: event.Title, Start: start, End: end, Type: TypeOf(event.Kind, false),
		Location: event.Room, Required: event.Required, Cancelled: event.Cancelled, Source: event.Source, Link: link,
		Notes: []string{}, AllDay: wholeDay, Origin: origin, Message: ToldIn(told)}
	subject, err := s.Subjects.Get(event.CourseName()) // a course not named yet stays out: the feed is English
	if err != nil {
		return Entry{}, err
	}
	if subject != nil {
		entry.Notes = append(entry.Notes, subject.Title)
	}
	earlier, err := s.Events.Previous(event.ID)
	if err != nil {
		return Entry{}, err
	}
	if earlier != nil && earlier.Start != event.Start {
		entry.Notes = append(entry.Notes, "Moved from "+When(mustLocal(earlier.Start)))
	}
	return entry, nil
}

// Link is the t.me link to the message that told the event (or a deadline: any row with source, chat_id, msg_ids),
// where the chat has links; "" for none.
func (s *Schedule) Link(row Told) (string, error) {
	if row.ChatID == nil || len(row.MsgIDs) == 0 {
		return "", nil
	}
	chat, err := s.Inbox.Chat(*row.ChatID)
	if err != nil || chat == nil {
		return "", err
	}
	return inbox.MessageLink(chat, row.MsgIDs[0]), nil
}

// Origin is who told it, for the feeds' DESCRIPTION: English, as the feeds are; "" for nobody.
func (s *Schedule) Origin(row Told) (string, error) {
	told, err := s.ToldBy(row)
	switch v := told.(type) {
	case i18n.Phrase:
		return v.En, err
	case string:
		return v, err
	}
	return "", err
}

// ToldBy is who told it (an event, or any row with source, chat_id, msg_ids), for the web app: the chat's name (a
// string), or the letter's subject (an i18n.Phrase, §8.3); nil for nobody.
func (s *Schedule) ToldBy(row Told) (any, error) {
	if row.Source == "mail" {
		subject, err := s.Inbox.Subject(row.MsgIDs)
		if err != nil {
			return nil, err
		}
		if subject != nil && *subject != "" {
			return i18n.P("letter «"+*subject+"»", "письмо «"+*subject+"»"), nil
		}
		return i18n.P("letter", "письмо"), nil
	}
	if row.ChatID == nil {
		return nil, nil
	}
	chat, err := s.Inbox.Chat(*row.ChatID)
	if err != nil || chat == nil {
		return nil, err
	}
	return chat.Title, nil
}

// --- for the batches and the web app -------------------------------------------------

// UpcomingClass is a class for the batch prompt: the row with its title, type and correction in force.
type UpcomingClass struct {
	*ruz.Row
	Title      string        `json:"title"`
	Type       string        `json:"type"`
	Correction *events.Event `json:"correction"`
}

// UpcomingClasses are the classes of the next weeks with their titles and the correction in force, for the batch
// prompt. A class counts by the later of its RUZ time and its correction's: one moved to a later day stays listed
// after its RUZ day, so an edit of the message can restate the move and a later message can move or cancel it again.
func (s *Schedule) UpcomingClasses(today time.Time) ([]UpcomingClass, error) {
	corrections, err := s.Events.Corrections()
	if err != nil {
		return nil, err
	}
	since, until := today.Format(clock.DateLayout), today.AddDate(0, 0, ClassDays).Format(clock.DateLayout)
	classes, err := s.Ruz.Classes("", until)
	if err != nil {
		return nil, err
	}
	listed := []UpcomingClass{}
	for _, item := range classes {
		correction := corrections[item.UID]
		latest := item.Start
		if correction != nil && correction.Start > latest {
			latest = correction.Start
		}
		if latest < since {
			continue
		}
		title, err := s.ClassTitle(item)
		if err != nil {
			return nil, err
		}
		listed = append(listed, UpcomingClass{Row: item, Title: title, Type: TypeOf(item.Kind, true),
			Correction: correction})
	}
	return listed, nil
}

// SameCourse: two names of one course are the same name, or have the same title or abbr in the dictionary
// («Математический анализ» of a chat and *Calculus* of RUZ).
func (s *Schedule) SameCourse(a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, nil
	}
	if pytext.Casefold(subjects.Clean(a)) == pytext.Casefold(subjects.Clean(b)) {
		return true, nil
	}
	first, err := s.Subjects.Get(a)
	if err != nil {
		return false, err
	}
	second, err := s.Subjects.Get(b)
	if err != nil {
		return false, err
	}
	return first != nil && second != nil &&
		(first.Title == second.Title || pytext.Casefold(first.Abbr) == pytext.Casefold(second.Abbr)), nil
}

// NextClass is the first seminar of a course that starts after `after` and within `days`, as the feeds show it:
// moved by a correction, a cancelled one skipped. A course without any seminar in the timetable takes its next class
// of any kind (not an interim assessment or an exam). Nil when there is none.
func (s *Schedule) NextClass(course string, after time.Time, days int) (*Entry, error) {
	if course == "" {
		return nil, nil
	}
	classes, err := s.Ruz.Classes("", "9999")
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, item := range classes {
		if _, done := names[item.Subject]; done {
			continue
		}
		same, err := s.SameCourse(course, item.Subject)
		if err != nil {
			return nil, err
		}
		names[item.Subject] = same
	}
	corrections, err := s.Events.Corrections()
	if err != nil {
		return nil, err
	}
	var entries []Entry
	seminars := false
	for _, item := range classes {
		if !names[item.Subject] {
			continue
		}
		entry, err := s.ClassEntry(item, corrections[item.UID])
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		seminars = seminars || slices.Contains(Seminars, entry.Type)
	}
	until := after.AddDate(0, 0, days)
	var found *Entry
	for i := range entries {
		entry := &entries[i]
		if entry.Cancelled || entry.AllDay || !after.Before(entry.Start) || !entry.Start.Before(until) {
			continue
		}
		if seminars && !slices.Contains(Seminars, entry.Type) || !seminars && slices.Contains(NotClasses, entry.Type) {
			continue
		}
		if found == nil || entry.Start.Before(found.Start) {
			found = entry
		}
	}
	return found, nil
}

// Agenda is the Расписание screen (§8.4).
type Agenda struct {
	Items  []AgendaItem `json:"items"`
	Hidden []HiddenItem `json:"hidden"`
}

// AgendaItem is one entry of the screen with the event or correction behind it.
type AgendaItem struct {
	UID       string   `json:"uid"`
	Title     string   `json:"title"`
	Start     string   `json:"start"`
	End       *string  `json:"end"`
	Type      string   `json:"type"`
	Location  string   `json:"location"`
	Room      string   `json:"room"`
	Required  bool     `json:"required"`
	Cancelled bool     `json:"cancelled"`
	Source    string   `json:"source"`
	Link      *string  `json:"link"`
	Notes     []string `json:"notes"`
	Event     *Behind  `json:"event"`
}

// HiddenItem is an event the owner hid.
type HiddenItem struct {
	ID    int64   `json:"id"`
	Title string  `json:"title"`
	Start string  `json:"start"`
	End   *string `json:"end"`
	Type  string  `json:"type"`
	Chat  any     `json:"chat"` // a string, an i18n.Phrase or nil
}

// Behind is the event or correction behind an entry, as the web app acts on it: Class is RUZ's own time, Original
// the chat's event an own copy stands in for (what «As it was» brings back).
type Behind struct {
	ID         int64       `json:"id"`
	Correction bool        `json:"correction"`
	Own        bool        `json:"own"`
	Copy       bool        `json:"copy"`
	Hidden     bool        `json:"hidden"`
	Cancelled  bool        `json:"cancelled"`
	Start      string      `json:"start"`
	End        *string     `json:"end"`
	Room       string      `json:"room"`
	Chat       any         `json:"chat"`
	Link       *string     `json:"link"`
	Message    *MessageRef `json:"message"`
	Class      *ClassTime  `json:"class"`
	Original   *Original   `json:"original"`
}

// ClassTime is a class as RUZ has it.
type ClassTime struct {
	Title string `json:"title"`
	Start string `json:"start"`
	End   string `json:"end"`
	Room  string `json:"room"`
}

// Original is the chat's event an own copy stands in for.
type Original struct {
	Title   string      `json:"title"`
	Start   string      `json:"start"`
	End     *string     `json:"end"`
	Room    string      `json:"room"`
	Chat    any         `json:"chat"`
	Link    *string     `json:"link"`
	Message *MessageRef `json:"message"`
}

func nullable(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

// Agenda is the entries of the feeds from today on, each with the event or correction behind it for the owner to
// change, and the events the owner hid.
func (s *Schedule) Agenda(today time.Time, days int) (Agenda, error) {
	since, until := today.Format(clock.DateLayout), today.AddDate(0, 0, days).Format(clock.DateLayout)
	corrections, err := s.Events.Corrections()
	if err != nil {
		return Agenda{}, err
	}
	inForce, err := s.Events.InForceAll()
	if err != nil {
		return Agenda{}, err
	}
	agenda := Agenda{Items: []AgendaItem{}, Hidden: []HiddenItem{}}
	evs := map[string]*events.Event{}
	copied := map[string]bool{}
	for _, event := range inForce {
		if event.CopyOf != nil && *event.CopyOf != "" {
			copied[*event.CopyOf] = true
		}
	}
	for _, event := range inForce {
		if event.ClassUID != nil {
			continue
		}
		evs[event.UID] = event
		if event.Hidden && event.Start >= since && !copied[event.UID] {
			chat, err := s.ToldBy(OfEvent(event))
			if err != nil {
				return Agenda{}, err
			}
			agenda.Hidden = append(agenda.Hidden, HiddenItem{ID: event.ID, Title: event.Title, Start: event.Start,
				End: end(event), Type: TypeOf(event.Kind, false), Chat: chat})
		}
	}
	rows, err := s.Ruz.Classes("", "9999")
	if err != nil {
		return Agenda{}, err
	}
	classes := map[string]*ruz.Row{}
	for _, item := range rows {
		classes[item.UID] = item
	}
	entries, err := s.Entries()
	if err != nil {
		return Agenda{}, err
	}
	for _, entry := range entries {
		if d := day(entry.Start); d < since || d >= until {
			continue
		}
		item := classes[entry.UID]
		var event *events.Event
		if item != nil {
			event = corrections[entry.UID]
		} else {
			event = evs[entry.UID]
		}
		// The room as the form edits it: LOCATION may be an online class's link instead.
		room := ""
		if item != nil && !Applies(item, event) {
			room = item.Room
		} else if event != nil {
			room = event.Room
		}
		listed := AgendaItem{UID: entry.UID, Title: entry.Title, Start: iso(entry.Start), Type: entry.Type,
			Location: entry.Location, Room: room, Required: entry.Required, Cancelled: entry.Cancelled,
			Source: entry.Source, Link: nullable(entry.Link), Notes: entry.Notes}
		if entry.AllDay {
			listed.Start = day(entry.Start)
		} else {
			end := iso(entry.End)
			listed.End = &end
		}
		if event != nil {
			if listed.Event, err = s.behind(event, item, evs); err != nil {
				return Agenda{}, err
			}
		}
		agenda.Items = append(agenda.Items, listed)
	}
	return agenda, nil
}

func end(event *events.Event) *string {
	if AllDay(event.Start) {
		return nil
	}
	if event.EndText() != "" {
		return event.End
	}
	value := iso(mustLocal(event.Start).Add(DefaultLength))
	return &value
}

func (s *Schedule) behind(event *events.Event, item *ruz.Row, evs map[string]*events.Event) (*Behind, error) {
	var original *events.Event
	if event.CopyOf != nil && *event.CopyOf != "" {
		original = evs[*event.CopyOf]
	}
	told := OfEvent(event)
	chat, err := s.ToldBy(told)
	if err != nil {
		return nil, err
	}
	link, err := s.Link(told)
	if err != nil {
		return nil, err
	}
	b := &Behind{ID: event.ID, Correction: item != nil, Own: event.Source == events.Own, Copy: original != nil,
		Hidden: event.Hidden, Cancelled: event.Cancelled, Start: event.Start, End: end(event), Room: event.Room,
		Chat: chat, Link: nullable(link), Message: Ref(told)}
	if item != nil {
		title, err := s.ClassTitle(item)
		if err != nil {
			return nil, err
		}
		b.Class = &ClassTime{Title: title, Start: item.Start, End: item.End, Room: item.Room}
	}
	if original != nil {
		told := OfEvent(original)
		chat, err := s.ToldBy(told)
		if err != nil {
			return nil, err
		}
		link, err := s.Link(told)
		if err != nil {
			return nil, err
		}
		b.Original = &Original{Title: original.Title, Start: original.Start, End: end(original), Room: original.Room,
			Chat: chat, Link: nullable(link), Message: Ref(told)}
	}
	return b, nil
}

// --- the owner's choices ------------------------------------------------------------

// SetHidden is Events.SetHidden for the web app and Claude. Hiding a correction, or applying it again, moves the
// class back or forth, and its Midterm with it.
func (s *Schedule) SetHidden(ctx context.Context, id int64, hidden bool) (*events.Event, error) {
	row, err := s.Events.SetHidden(id, hidden)
	if err != nil {
		return nil, err
	}
	if row != nil && row.ClassUID != nil {
		if err := s.followMidterms(ctx); err != nil {
			return nil, err
		}
	}
	return row, nil
}

// followMidterms: the Midterms (and the sheets' assumed dates) follow the classes as the owner left them; the deadlines
// hear of the ones that moved.
func (s *Schedule) followMidterms(ctx context.Context) error {
	midterms, err := s.SyncDeadlines()
	if err != nil {
		return err
	}
	if midterms.Any() && s.OnMidterms != nil {
		if err := s.OnMidterms(ctx, midterms); err != nil {
			slog.Error("posting the Midterm changes failed", "error", err)
		}
	}
	return nil
}

// Fields are an own event: title, kind, start, end ("" for none), room, required.
type Fields struct {
	Title    string
	Kind     string
	Start    string
	End      string
	Room     string
	Required bool
}

// AddEvent adds an own event; it is in the feeds as the chats' events are.
func (s *Schedule) AddEvent(fields Fields) (int64, error) {
	return s.Events.Add(events.New{UID: events.OwnUID(), Source: events.Own, Title: fields.Title, Kind: fields.Kind,
		Start: fields.Start, End: fields.End, Room: fields.Room, Required: fields.Required, Chosen: true}, s.Now())
}

// Change is the owner's change of an event: nil fields are kept; End "" is none.
type Change struct {
	Title     *string
	Kind      *string
	Start     *string
	End       *string
	Room      *string
	Required  *bool
	Cancelled *bool
}

func pick[T any](value *T, keep T) T {
	if value != nil {
		return *value
	}
	return keep
}

// EditEvent is the owner's change of an event: an own event gets a new version; one from a chat or a letter is hidden
// and an own copy with the change takes its place, remembering it (`copy_of`). The id of the new version; nil for no
// such event.
func (s *Schedule) EditEvent(id int64, change Change) (*int64, error) {
	event, err := s.Events.Current(id)
	if err != nil || event == nil || event.GoneAt != nil || event.ClassUID != nil {
		return nil, err
	}
	fresh := events.New{Source: events.Own, Title: pick(change.Title, event.Title), Kind: pick(change.Kind, event.Kind),
		Start: pick(change.Start, event.Start), End: pick(change.End, event.EndText()), Room: pick(change.Room, event.Room),
		Required: pick(change.Required, event.Required), Cancelled: pick(change.Cancelled, event.Cancelled),
		Course: event.CourseName(), CopyOf: text(event.CopyOf), Chosen: true}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	evs := s.Events.With(tx)
	var newID int64
	if event.Source == events.Own {
		fresh.UID = event.UID
		if newID, err = evs.Add(fresh, s.Now()); err != nil {
			return nil, err
		}
		if _, err := evs.Supersede(event.ID, newID); err != nil {
			return nil, err
		}
	} else {
		fresh.UID, fresh.CopyOf = events.OwnUID(), event.UID
		if newID, err = evs.Add(fresh, s.Now()); err != nil {
			return nil, err
		}
		if _, err := tx.Exec("update events set hidden = 1 where id = ?", event.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &newID, nil
}

// CorrectClass is the owner's change of a RUZ class: a new version of their own correction, or the first one. A title
// the same as the class's is kept as "" (the dictionary names it), a kind of the class's type as the class's own.
// Title or kind nil: none given. Nil for no such class.
func (s *Schedule) CorrectClass(ctx context.Context, classUID, start, end, room string, cancelled bool, title,
	kind *string, required bool) (*int64, error) {
	item, err := s.Ruz.ByUID(classUID)
	if err != nil || item == nil || item.GoneAt != nil {
		return nil, err
	}
	classTitle, err := s.ClassTitle(item)
	if err != nil {
		return nil, err
	}
	newTitle := ""
	if title != nil && !deadlines.SameLabel(*title, classTitle) {
		newTitle = *title
	}
	newKind := item.Kind
	if kind != nil && TypeOf(*kind, true) != TypeOf(item.Kind, true) {
		newKind = *kind
	}
	earlier, err := s.Events.OwnCorrection(classUID)
	if err != nil {
		return nil, err
	}
	uid := events.OwnUID()
	if earlier != nil {
		uid = earlier.UID
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	evs := s.Events.With(tx)
	newID, err := evs.Add(events.New{UID: uid, Source: events.Own, ClassUID: classUID, Course: item.Subject,
		Title: newTitle, Kind: newKind, Start: start, End: end, Room: room, Required: required, Chosen: true,
		Cancelled: cancelled}, s.Now())
	if err != nil {
		return nil, err
	}
	if earlier != nil {
		if _, err := evs.Supersede(earlier.ID, newID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := s.followMidterms(ctx); err != nil {
		return nil, err
	}
	return &newID, nil
}

// RemoveOwn deletes an own event, undoes an own correction (the chats' one applies again, if there is one) or an own
// copy (the chat's event it stood in for is back); the event, or nil when it is not an own one in force.
func (s *Schedule) RemoveOwn(ctx context.Context, id int64) (*events.Event, error) {
	event, err := s.Events.Current(id)
	if err != nil || event == nil || event.GoneAt != nil || event.Source != events.Own {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.Events.With(tx).Withdraw(event.UID, s.Now()); err != nil {
		return nil, err
	}
	if event.CopyOf != nil && *event.CopyOf != "" { // the chat's event it stood in for is back
		if _, err := tx.Exec("update events set hidden = 0 where uid = ? and "+events.InForce, *event.CopyOf); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if event.ClassUID != nil {
		if err := s.followMidterms(ctx); err != nil {
			return nil, err
		}
	}
	return event, nil
}

// --- after a RUZ poll -------------------------------------------------------------

// AfterPoll is Ruz.OnPoll: RUZ wins over the chats, the subjects get named, the Midterm deadlines follow the classes.
func (s *Schedule) AfterPoll(ctx context.Context, diff ruz.Diff) (MidtermChanges, error) {
	now := s.Now()
	tx, err := s.db.Begin()
	if err != nil {
		return MidtermChanges{}, err
	}
	defer tx.Rollback()
	evs := s.Events.With(tx)
	for _, change := range diff.Changes {
		if change.Kind == "new" {
			continue
		}
		overruled, err := evs.Overrule(change.New.UID, now)
		if err != nil {
			return MidtermChanges{}, err
		}
		if len(overruled) > 0 {
			slog.Info(fmt.Sprintf("RUZ changed %s itself: chat corrections %v are gone", change.New.UID, overruled))
		}
	}
	if err := tx.Commit(); err != nil {
		return MidtermChanges{}, err
	}
	classes, err := s.Ruz.Classes("", "9999")
	if err != nil {
		return MidtermChanges{}, err
	}
	names := make([]string, len(classes))
	for i, item := range classes {
		names[i] = item.Subject
	}
	if err := s.Subjects.TryResolve(ctx, names, "ruz"); err != nil {
		return MidtermChanges{}, err
	}
	return s.SyncDeadlines()
}

// SyncDeadlines syncs the Midterms, then every other deadline that follows the timetable (Followers).
func (s *Schedule) SyncDeadlines() (MidtermChanges, error) {
	changes, err := s.SyncMidterms()
	if err != nil {
		return changes, err
	}
	for _, follow := range s.Followers {
		more, err := follow()
		if err != nil {
			return changes, err
		}
		changes.Deadlines = append(changes.Deadlines, more.Deadlines...)
		changes.Moved = append(changes.Moved, more.Moved...)
		changes.Gone = append(changes.Gone, more.Gone...)
	}
	return changes, nil
}

// SyncMidterms keeps each interim assessment's deadline as the feeds show the class: RUZ's own state, then the chat's
// correction in force (a cancellation withdraws it, a move moves it).
func (s *Schedule) SyncMidterms() (MidtermChanges, error) {
	changes, now := MidtermChanges{}, s.Now()
	today := clock.LocalDay(now).Format(clock.DateLayout)
	corrections, err := s.Events.Corrections()
	if err != nil {
		return changes, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return changes, err
	}
	defer tx.Rollback()
	dls, sub := s.Deadlines.With(tx), s.Subjects.With(tx)
	link := func(classID int64, deadlineID *int64) error {
		_, err := tx.Exec("update classes set deadline_id = ? where id = ?", deadlineID, classID)
		return err
	}
	items, err := ruz.Rows(tx, "order by start")
	if err != nil {
		return changes, err
	}
	for _, item := range items {
		var deadline *deadlines.Deadline
		if item.DeadlineID != nil && *item.DeadlineID != 0 {
			if deadline, err = dls.Get(*item.DeadlineID); err != nil {
				return changes, err
			}
		}
		if deadline != nil && (deadline.MovedBy != nil || deadline.GoneAt != nil) {
			deadline = nil
		}
		correction := corrections[item.UID]
		applied := Applies(item, correction)
		wanted := Midterm(item.Kind) && !item.Cancelled && item.GoneAt == nil && !(applied && correction.Cancelled)
		if !wanted {
			if deadline != nil {
				if _, err := dls.Drop([]int64{deadline.ID}, now); err != nil {
					return changes, err
				}
				changes.Gone = append(changes.Gone, deadline.ID)
			}
			if item.DeadlineID != nil && *item.DeadlineID != 0 {
				if err := link(item.ID, nil); err != nil {
					return changes, err
				}
			}
			continue
		}
		start := item.Start
		if applied {
			start = correction.Start
		}
		due := pytext.Cut(start, 10)
		abbr, err := sub.Abbr(item.Subject)
		if err != nil {
			return changes, err
		}
		if abbr == "" {
			abbr = item.Subject
		}
		label := abbr + " Midterm"
		fresh := deadlines.New{Source: "ical", MsgIDs: []int64{}, Course: item.Subject, Label: label, Due: due}
		switch {
		case deadline == nil:
			if due >= today { // a past one is no news
				newID, err := dls.Add(fresh, now)
				if err != nil {
					return changes, err
				}
				if err := link(item.ID, &newID); err != nil {
					return changes, err
				}
				changes.Deadlines = append(changes.Deadlines, newID)
			}
		case deadline.Due != due:
			newID, err := dls.Add(fresh, now)
			if err != nil {
				return changes, err
			}
			if _, err := dls.Move(deadline.ID, newID); err != nil {
				return changes, err
			}
			if err := link(item.ID, &newID); err != nil {
				return changes, err
			}
			changes.Moved = append(changes.Moved, [2]int64{deadline.ID, newID})
		case deadline.Label != label || deadline.CourseName() != item.Subject || deadline.Course == nil:
			subject := item.Subject
			if err := dls.Rename(deadline.ID, label, &subject); err != nil { // the dictionary has named it
				return changes, err
			}
		}
	}
	return changes, tx.Commit()
}
