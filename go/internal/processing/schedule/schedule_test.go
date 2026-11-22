package schedule

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const chat = int64(-1001234567890)

var names = map[string][2]string{
	"Математический анализ": {"Calculus", "Calc"},
	"Алгебра":                                {"Algebra", "Alg"},
	"Algebra":                                {"Algebra", "Alg"},
	"Discrete Mathematics":                   {"Discrete Math", "DM"},
	"Introduction into Software Engineering": {"Intro to SE", "SE"},
	"Основы российской государственности": {"Rus Statehood", "RSF"},
	"Foundations of Russian Statehood":    {"Rus Statehood", "RSF"},
	"Введение в программирование":         {"Intro to Prog", "Prog"},
	"Calculus": {"Calculus", "Calc"},
}

// namer is Haiku for the dictionary: names the courses it knows, or fails with err.
type namer struct {
	mu  sync.Mutex
	err error
}

func (n *namer) model() *llm.Fake {
	return &llm.Fake{Respond: func(req llm.Request) (any, error) {
		n.mu.Lock()
		err := n.err
		n.mu.Unlock()
		if err != nil {
			return nil, err
		}
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

type fakeClock struct{ moment time.Time }

func (c *fakeClock) now() time.Time { return c.moment }

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func noErr(err error) {
	if err != nil {
		panic(err)
	}
}

func ptr[T any](v T) *T { return &v }

type fixture struct {
	*Schedule
	store *store.Store
	clock *fakeClock
	namer *namer
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	c := &fakeClock{moment: time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}
	n := &namer{}
	r := ruz.New("", s)
	r.Now = c.now
	sch := New(s, r, events.Open(s), deadlines.Open(s), subjects.New(s, n.model()), inbox.Open(s))
	sch.Now = c.now
	return &fixture{Schedule: sch, store: s, clock: c, namer: n}
}

func loaded(t *testing.T) *fixture {
	f := setup(t)
	must(f.Inbox.PutChat(chat, "Матан", "supergroup", nil, 0))
	f.syncFeed()
	return f
}

func (f *fixture) syncFeed() {
	must(f.Ruz.Sync(must(ruz.Parse(must(os.ReadFile("testdata/ruz.ics")))), false))
}

func event() events.New {
	return events.New{UID: events.UID("tg", chat, 1, 0), Source: "tg", ChatID: ptr(chat), MsgIDs: []int64{1},
		Course: "Алгебра", Title: "Alg consult", Kind: "consultation", Start: "2026-10-23T18:10:00", Room: "R205",
		Required: true}
}

func (f *fixture) add(ev events.New) int64 { return must(f.Events.Add(ev, f.Now())) }

func (f *fixture) correction(uid string, change func(*events.New)) int64 {
	item := must(f.Ruz.ByUID(uid))
	ev := event()
	ev.UID, ev.ClassUID, ev.Course, ev.Title, ev.Kind = uid, uid, item.Subject, "Discrete Math", item.Kind
	ev.Start, ev.End, ev.Room = item.Start, item.End, item.Room
	if change != nil {
		change(&ev)
	}
	return f.add(ev)
}

func (f *fixture) entry(uid string) Entry {
	for _, e := range must(f.Entries()) {
		if e.UID == uid {
			return e
		}
	}
	panic("no entry " + uid)
}

func (f *fixture) entries() map[string]Entry {
	out := map[string]Entry{}
	for _, e := range must(f.Entries()) {
		out[e.UID] = e
	}
	return out
}

func local(text string) time.Time { return must(clock.ParseLocal(text)) }

func TestTypeOf(t *testing.T) {
	cases := []struct {
		kind    string
		ofClass bool
		want    string
	}{
		{"Лекция", true, "Lecture"}, {"Lecture", true, "Lecture"}, {"Проведение семинаров (студенты)", true, "Seminar"},
		{"Практические занятия", true, "Seminar"}, {"Ongoing interim assessment", true, "Interim assessment"},
		{"Текущий рубежный контроль", true, "Interim assessment"}, {"Экзамен", true, "Exam"},
		{"Консультация", true, "Consultation"}, {"consultation", false, "Consultation"}, {"talk", false, "Talk"},
		{"Лабораторная работа", true, "Lab"}, {"Что-то новое", true, "Class"}, {"", false, "Event"},
		{"встреча", false, "Event"},
	}
	for _, c := range cases {
		if got := TypeOf(c.kind, c.ofClass); got != c.want {
			t.Errorf("%q: %s", c.kind, got)
		}
	}
}

func TestClassEntries(t *testing.T) {
	f := loaded(t)
	if f.entry("2000001@ruz.hse.ru").Title != "Математический анализ" { // not named yet: as the feed has it
		t.Fatal("title")
	}
	must(f.AfterPoll(context.Background(), ruz.Diff{First: true}))
	calc, statehood, dm := f.entry("2000001@ruz.hse.ru"), f.entry("2000003@ruz.hse.ru"), f.entry("2000004@ruz.hse.ru")
	if calc.Title != "Calculus" || calc.Type != "Seminar" || calc.Location != "D208" || calc.Source != "ruz" ||
		!calc.Required {
		t.Fatal(calc)
	}
	if !calc.Start.Equal(local("2026-10-19T09:30:00")) || !calc.End.Equal(local("2026-10-19T10:50:00")) {
		t.Fatal(calc.Start)
	}
	if statehood.Location != "https://my.mts-link.ru/j/11111111/22222222222" { // online: the link
		t.Fatal(statehood.Location)
	}
	if dm.Location != "https://example.ktalk.ru/abcdef" || dm.Type != "Seminar" {
		t.Fatal(dm)
	}
	if !f.entry("2000008@ruz.hse.ru").Cancelled || f.entry("2000006@ruz.hse.ru").Type != "Interim assessment" ||
		f.entry("2000006@ruz.hse.ru").Location != "" {
		t.Fatal("8 and 6")
	}
	all := must(f.Entries())
	if !slices.IsSortedFunc(all, func(a, b Entry) int { return a.Start.Compare(b.Start) }) {
		t.Fatal("order")
	}
}

func TestCorrectionsApplyUntilRuzWins(t *testing.T) {
	f := loaded(t)
	ctx := context.Background()
	uid := "2000004@ruz.hse.ru" // Discrete Mathematics, Thu Oct 22 16:40, online
	moved := f.correction(uid, func(e *events.New) {
		e.Start, e.End, e.Room = "2026-10-23T13:00:00", "2026-10-23T14:20:00", "R405"
	})
	entry := f.entry(uid)
	if !entry.Start.Equal(local("2026-10-23T13:00:00")) || entry.Location != "R405" || entry.Source != "tg" {
		t.Fatal(entry)
	}
	if !slices.Equal(entry.Notes, []string{"Moved from Thu Oct 22 16:40 in a chat"}) ||
		entry.Link != "https://t.me/c/1234567890/1" {
		t.Fatal(entry.Notes, entry.Link)
	}
	must(f.Events.SetHidden(moved, true)) // the owner says the model was wrong
	entry = f.entry(uid)
	if !entry.Start.Equal(local("2026-10-22T16:40:00")) || entry.Source != "ruz" || len(entry.Notes) != 0 {
		t.Fatal(entry)
	}
	must(f.Events.SetHidden(moved, false))

	ev := event()
	ev.UID, ev.ClassUID, ev.Title, ev.Start, ev.End, ev.Room, ev.Cancelled, ev.MsgIDs = uid, uid, "Discrete Math",
		"2026-10-22T16:40:00", "2026-10-22T18:00:00", "online", true, []int64{2}
	cancelled := f.add(ev)
	must(f.Events.Supersede(moved, cancelled))
	entry = f.entry(uid)
	if !entry.Cancelled || !slices.Equal(entry.Notes, []string{"Cancelled in a chat"}) ||
		!strings.HasSuffix(entry.Link, "/2") {
		t.Fatal(entry)
	}

	item := must(f.Ruz.ByUID(uid))
	must(f.AfterPoll(ctx, ruz.Diff{Changes: []ruz.Change{{Kind: "new", New: item}}})) // no change of this one
	if must(f.Events.Correction(uid)).ID != cancelled {
		t.Fatal("kept")
	}
	must(f.AfterPoll(ctx, ruz.Diff{Changes: []ruz.Change{{Kind: "room", Old: item, New: item}}}))
	if must(f.Events.Correction(uid)) != nil || must(f.Events.Get(moved)).GoneAt == nil {
		t.Fatal("overruled")
	}
	entry = f.entry(uid)
	if entry.Cancelled || entry.Source != "ruz" {
		t.Fatal(entry)
	}
}

func TestMovedBack(t *testing.T) {
	f := loaded(t)
	uid := "2000005@ruz.hse.ru"
	item := must(f.Ruz.ByUID(uid))
	f.correction(uid, nil) // a chat put it back where RUZ has it
	entry := f.entry(uid)
	if len(entry.Notes) != 0 || entry.Source != "ruz" || !entry.Start.Equal(local(item.Start)) {
		t.Fatal(entry)
	}
}

func TestEventEntries(t *testing.T) {
	f := loaded(t)
	consult := f.add(event())
	talk := event()
	talk.UID, talk.Title, talk.Kind, talk.Start, talk.End, talk.Room, talk.Required, talk.Course = "talk@x",
		"Career fair", "event", "2026-10-24T12:00:00", "2026-10-24T16:00:00", "", false, ""
	talkID := f.add(talk)
	club := event()
	club.UID, club.Title, club.Kind, club.Hidden = "hidden@x", "Club", "meeting", true
	f.add(club)
	entries := f.entries()
	first := entries[must(f.Events.Get(consult)).UID]
	if first.Title != "Alg consult" || first.Type != "Consultation" || first.Location != "R205" || !first.Required ||
		first.Source != "tg" {
		t.Fatal(first)
	}
	if first.End.Sub(first.Start) != 80*time.Minute || len(first.Notes) != 0 { // the course is not named yet
		t.Fatal(first)
	}
	if entries["talk@x"].Required || len(entries["talk@x"].Notes) != 0 {
		t.Fatal(entries["talk@x"])
	}
	if _, ok := entries["hidden@x"]; ok {
		t.Fatal("hidden")
	}
	later := event()
	later.Start, later.MsgIDs = "2026-10-24T18:10:00", []int64{3}
	laterID := f.add(later)
	must(f.Events.Supersede(consult, laterID))
	must(f.Subjects.SetManual("Алгебра", "Algebra", "Alg"))
	entries = f.entries()
	if notes := entries[must(f.Events.Get(consult)).UID].Notes; !slices.Equal(notes,
		[]string{"Algebra", "Moved from Fri Oct 23 18:10"}) {
		t.Fatal(notes)
	}
	if entries["talk@x"].Type != "Event" || must(f.Events.Get(talkID)).UID != "talk@x" {
		t.Fatal(entries["talk@x"])
	}
}

func TestVersions(t *testing.T) {
	f := loaded(t)
	first := f.add(event())
	must(f.Events.SetRequired(first, false))
	must(f.Events.SetHidden(first, true))
	moved := event()
	moved.Start, moved.MsgIDs = "2026-10-24T18:10:00", []int64{5}
	movedID := f.add(moved)
	if !must(f.Events.Supersede(first, movedID)) || must(f.Events.Supersede(first, movedID)) {
		t.Fatal("supersede once")
	}
	if must(f.Events.Current(first)).ID != movedID {
		t.Fatal("current")
	}
	version := must(f.Events.Get(movedID))
	if version.Required || !version.Chosen || !version.Hidden || version.UID != must(f.Events.Get(first)).UID {
		t.Fatal(version)
	}
	edited := event()
	edited.Start, edited.MsgIDs = "2026-10-25T18:10:00", []int64{5} // message 5 edited
	editedID := f.add(edited)
	must(f.Events.Supersede(movedID, editedID))
	if back := must(f.Events.Drop([]int64{editedID}, f.Now())); !slices.Equal(back, []int64{first}) {
		t.Fatal(back) // message 5 is gone: the first version is back
	}
	if must(f.Events.ByUID(version.UID)).ID != first {
		t.Fatal("first")
	}
	if back := must(f.Events.Drop([]int64{first}, f.Now())); len(back) != 0 {
		t.Fatal(back)
	}
	if must(f.Events.ByUID(version.UID)) != nil {
		t.Fatal("gone")
	}
	fix := f.correction("2000005@ruz.hse.ru", nil)
	if !must(f.Events.SetRequired(fix, false)).Required { // a class stays in the main calendar
		t.Fatal("required")
	}
}

func labels(f *fixture, ids ...int64) []string {
	var out []string
	for _, id := range ids {
		out = append(out, must(f.Deadlines.Get(id)).Label)
	}
	return out
}

func TestMidtermsFollowTheirClasses(t *testing.T) {
	f := loaded(t)
	ctx := context.Background()
	f.namer.err = llm.NewError(llm.Failed, "ANTHROPIC_API_KEY is not set")
	changes := must(f.AfterPoll(ctx, ruz.Diff{First: true}))
	if f.Subjects.LastError() != "ANTHROPIC_API_KEY is not set" {
		t.Fatal(f.Subjects.LastError())
	}
	var midterms []*deadlines.Deadline
	for _, id := range changes.Deadlines {
		midterms = append(midterms, must(f.Deadlines.Get(id)))
	}
	if len(midterms) != 2 ||
		midterms[0].Label != "Foundations of Russian Statehood Midterm" || midterms[0].Due != "2026-10-26" ||
		*midterms[0].Course != "Foundations of Russian Statehood" || midterms[0].Source != "ical" ||
		midterms[1].Label != "Введение в программирование Midterm" || midterms[1].Due != "2026-10-29" {
		t.Fatal(midterms)
	}
	if up := must(f.Deadlines.Upcoming(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), deadlines.Lookback)); len(up) != 0 {
		t.Fatal("not for the batch model to move")
	}

	f.namer.err = nil
	changes = must(f.AfterPoll(ctx, ruz.Diff{})) // a 304: the names get another try
	if changes.Any() || f.Subjects.LastError() != "" {
		t.Fatal(changes)
	}
	if got := labels(f, midterms[0].ID, midterms[1].ID); !slices.Equal(got, []string{"RSF Midterm", "Prog Midterm"}) {
		t.Fatal(got) // renamed in place: no news
	}

	db := f.store.DB
	must(db.Exec("update classes set start = '2026-10-27T12:30:00' where uid = '2000006@ruz.hse.ru'"))
	must(db.Exec("update classes set cancelled = 1 where uid = '2000007@ruz.hse.ru'"))
	changes = must(f.SyncMidterms())
	if len(changes.Moved) != 1 || changes.Moved[0][0] != midterms[0].ID ||
		must(f.Deadlines.Get(changes.Moved[0][1])).Due != "2026-10-27" {
		t.Fatal(changes)
	}
	newID := changes.Moved[0][1]
	if !slices.Equal(changes.Gone, []int64{midterms[1].ID}) || must(f.Deadlines.Get(midterms[1].ID)).GoneAt == nil {
		t.Fatal(changes)
	}
	must(db.Exec("update classes set gone_at = '2026-10-20T12:00:00' where uid = '2000006@ruz.hse.ru'"))
	must(db.Exec("update classes set cancelled = 0 where uid = '2000007@ruz.hse.ru'"))
	changes = must(f.SyncMidterms())
	if !slices.Equal(changes.Gone, []int64{newID}) || len(changes.Deadlines) != 1 { // the class is back: its deadline
		t.Fatal(changes)
	}
	if must(f.SyncMidterms()).Any() {
		t.Fatal("nothing more")
	}
}

func TestMidtermsFollowChatCorrections(t *testing.T) {
	f := loaded(t)
	ctx := context.Background()
	must(f.AfterPoll(ctx, ruz.Diff{First: true}))
	uid := "2000007@ruz.hse.ru" // Введение в программирование, Thu Oct 29 10:00-13:00, the interim assessment
	first := *must(f.Ruz.ByUID(uid)).DeadlineID
	if d := must(f.Deadlines.Get(first)); d.Label != "Prog Midterm" || d.Due != "2026-10-29" {
		t.Fatal(d)
	}
	var published []MidtermChanges
	f.OnMidterms = func(ctx context.Context, m MidtermChanges) error { published = append(published, m); return nil }
	moved := f.correction(uid, func(e *events.New) {
		e.Title, e.Start, e.End = "Intro to Prog", "2026-11-05T10:00:00", "2026-11-05T13:00:00"
	})
	changes := must(f.SyncMidterms()) // a chat moved the class: its Midterm goes with it
	if len(changes.Moved) != 1 || changes.Moved[0][0] != first {
		t.Fatal(changes)
	}
	newID := changes.Moved[0][1]
	if d := must(f.Deadlines.Get(newID)); d.Label != "Prog Midterm" || d.Due != "2026-11-05" {
		t.Fatal(d)
	}
	if must(f.SyncMidterms()).Any() {
		t.Fatal("again")
	}
	if !must(f.SetHidden(ctx, moved, true)).Hidden { // the owner: the model was wrong, RUZ's date is back
		t.Fatal("hidden")
	}
	if len(published) != 1 || len(published[0].Moved) != 1 || published[0].Moved[0][0] != newID ||
		must(f.Deadlines.Get(published[0].Moved[0][1])).Due != "2026-10-29" {
		t.Fatal(published)
	}
	must(f.SetHidden(ctx, moved, false))
	again := published[1].Moved[0][1]
	if must(f.Deadlines.Get(again)).Due != "2026-11-05" {
		t.Fatal("again")
	}
	ev := event()
	ev.UID, ev.ClassUID, ev.Title, ev.Start, ev.End, ev.Room, ev.Cancelled, ev.MsgIDs = uid, uid, "Intro to Prog",
		"2026-11-05T10:00:00", "2026-11-05T13:00:00", "R401", true, []int64{2}
	cancelled := f.add(ev)
	must(f.Events.Supersede(moved, cancelled))
	changes = must(f.SyncMidterms()) // a chat cancelled it: no Midterm
	if !slices.Equal(changes.Gone, []int64{again}) || must(f.Deadlines.Get(again)).GoneAt == nil ||
		len(changes.Moved) != 0 {
		t.Fatal(changes)
	}
	if must(f.Ruz.ByUID(uid)).DeadlineID != nil || must(f.SyncMidterms()).Any() {
		t.Fatal("unlinked")
	}
	must(f.SetHidden(ctx, cancelled, true)) // the cancellation is not applied: the class and its Midterm are back
	if len(published[2].Deadlines) != 1 || must(f.Deadlines.Get(published[2].Deadlines[0])).Due != "2026-10-29" {
		t.Fatal(published[2])
	}
	own := event()
	own.UID, own.MsgIDs = "own@x", []int64{3}
	must(f.SetHidden(ctx, f.add(own), true)) // an event of its own has no Midterm
	if len(published) != 3 {
		t.Fatal(published)
	}
}

func TestAFailingMidtermPostDoesNotStopHiding(t *testing.T) {
	f := loaded(t)
	ctx := context.Background()
	must(f.AfterPoll(ctx, ruz.Diff{First: true}))
	f.OnMidterms = func(context.Context, MidtermChanges) error { return errors.New("the deadlines are down") }
	moved := f.correction("2000007@ruz.hse.ru", func(e *events.New) {
		e.Start, e.End = "2026-11-05T10:00:00", "2026-11-05T13:00:00"
	})
	must(f.SyncMidterms())
	if !must(f.SetHidden(ctx, moved, true)).Hidden {
		t.Fatal("hidden")
	}
	if must(f.Deadlines.Get(*must(f.Ruz.ByUID("2000007@ruz.hse.ru")).DeadlineID)).Due != "2026-10-29" {
		t.Fatal("due")
	}
}

func TestPastMidtermIsNoNews(t *testing.T) {
	f := setup(t)
	f.syncFeed()
	f.clock.moment = f.clock.moment.AddDate(0, 0, 7) // Oct 27: the Oct 26 assessment is past
	changes := must(f.AfterPoll(context.Background(), ruz.Diff{}))
	if len(changes.Deadlines) != 1 || must(f.Deadlines.Get(changes.Deadlines[0])).Due != "2026-10-29" {
		t.Fatal(changes)
	}
}

func TestAgenda(t *testing.T) {
	f := loaded(t)
	past := event()
	past.UID, past.Title, past.Start, past.MsgIDs = "past@x", "Past talk", "2026-10-19T18:00:00", []int64{7}
	f.add(past)
	talkEvent := event()
	talkEvent.End = "2026-10-23T19:00:00"
	talk := f.add(talkEvent)
	hiddenEvent := event()
	hiddenEvent.UID, hiddenEvent.Title, hiddenEvent.Start, hiddenEvent.MsgIDs, hiddenEvent.Hidden = "hidden@x",
		"Debate club", "2026-10-24T12:00:00", []int64{8}, true
	hidden := f.add(hiddenEvent)
	fix := f.correction("2000004@ruz.hse.ru", func(e *events.New) {
		e.Start, e.End = "2026-10-21T10:00:00", "2026-10-21T11:20:00"
	})
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, clock.TZ)
	agenda := must(f.Agenda(today, AgendaDays))
	items := map[string]AgendaItem{}
	for _, item := range agenda.Items {
		items[item.UID] = item
	}
	if _, ok := items["2000001@ruz.hse.ru"]; ok {
		t.Fatal("Oct 19: past")
	}
	if _, ok := items["past@x"]; ok {
		t.Fatal("past")
	}
	moved := items["2000004@ruz.hse.ru"] // by its corrected time, Wed, with RUZ's own Thu behind it
	if moved.Start != "2026-10-21T10:00:00" || moved.Source != "tg" || moved.Event.ID != fix || !moved.Event.Correction {
		t.Fatal(moved)
	}
	if moved.Event.Class.Start != "2026-10-22T16:40:00" || moved.Event.Chat != "Матан" {
		t.Fatal(moved.Event)
	}
	told := items[events.UID("tg", chat, 1, 0)]
	if told.Event.ID != talk || told.Event.Correction || *told.End != "2026-10-23T19:00:00" ||
		*told.Link != "https://t.me/c/1234567890/1" {
		t.Fatal(told)
	}
	if items["2000005@ruz.hse.ru"].Event != nil {
		t.Fatal("a class without a correction")
	}
	if _, ok := items["hidden@x"]; ok {
		t.Fatal("hidden")
	}
	if len(agenda.Hidden) != 1 || agenda.Hidden[0].ID != hidden || agenda.Hidden[0].Type != "Consultation" {
		t.Fatal(agenda.Hidden)
	}
	if _, ok := items["2000009@ruz.hse.ru"]; !ok { // Nov 3: within four weeks
		t.Fatal("Nov 3")
	}
	for _, item := range must(f.Agenda(today, 7)).Items {
		if item.UID == "2000009@ruz.hse.ru" {
			t.Fatal("past a week")
		}
	}
}

func TestOwnEventsAndCorrections(t *testing.T) {
	// The owner's own events and corrections (D44).
	f := loaded(t)
	ctx := context.Background()
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, clock.TZ)
	talk := f.add(event())
	own := must(f.AddEvent(Fields{Title: "Supervisor", Kind: "Event", Start: "2026-10-24T15:00:00",
		End: "2026-10-24T16:00:00", Room: "R205", Required: false}))
	var item AgendaItem
	for _, i := range must(f.Agenda(today, AgendaDays)).Items {
		if i.Title == "Supervisor" {
			item = i
		}
	}
	if item.Source != "own" || !item.Event.Own || item.Required {
		t.Fatal(item)
	}
	newer := *must(f.EditEvent(own, Change{Start: ptr("2026-10-24T17:00:00"), End: ptr("2026-10-24T18:00:00")}))
	if *must(f.Events.Get(own)).ReplacedBy != newer || must(f.Events.Get(newer)).UID != must(f.Events.Get(own)).UID {
		t.Fatal("a new version")
	}

	// A chat's event changed: hidden, an own copy in its place; the batches see the copy and the own events too, so a
	// chat does not tell them again (they never move them).
	copied := *must(f.EditEvent(talk, Change{Title: ptr("Alg consult, moved"), Start: ptr("2026-10-23T19:00:00"),
		End: ptr("2026-10-23T20:00:00")}))
	if talkRow := must(f.Events.Get(talk)); !talkRow.Hidden || talkRow.ReplacedBy != nil {
		t.Fatal(talkRow)
	}
	if c := must(f.Events.Get(copied)); c.Source != "own" || *c.Course != "Алгебра" {
		t.Fatal(c)
	}
	var upcoming []int64
	for _, e := range must(f.Events.Upcoming(today)) {
		upcoming = append(upcoming, e.ID)
	}
	if !slices.Equal(upcoming, []int64{talk, copied, newer}) {
		t.Fatal(upcoming)
	}

	// The owner's move of a class wins over the chats' corrections, a later one too; undone, the chats' newest applies.
	uid := "2000005@ruz.hse.ru"
	chats := f.correction(uid, func(e *events.New) { e.Start, e.End = "2026-10-23T11:10:00", "2026-10-23T12:30:00" })
	mine := *must(f.CorrectClass(ctx, uid, "2026-10-23T15:00:00", "2026-10-23T16:20:00", "R510", false, nil, nil, true))
	later := f.correction(uid, func(e *events.New) {
		e.MsgIDs, e.Start, e.End = []int64{9}, "2026-10-23T13:00:00", "2026-10-23T14:20:00"
	})
	entry := f.entry(uid)
	if !entry.Start.Equal(local("2026-10-23T15:00:00")) || entry.Location != "R510" || entry.Source != "own" {
		t.Fatal(entry)
	}
	if !slices.Equal(entry.Notes, []string{"Moved from Fri Oct 23 09:30 by hand"}) {
		t.Fatal(entry.Notes)
	}
	if must(f.Events.Correction(uid)).ID != later || chats == 0 { // what a chat's next message supersedes
		t.Fatal("chats' correction")
	}
	again := *must(f.CorrectClass(ctx, uid, "2026-10-23T15:00:00", "2026-10-23T16:20:00", "R510", true, nil, nil, true))
	if *must(f.Events.Get(mine)).ReplacedBy != again {
		t.Fatal("a new version of the own correction")
	}
	if must(f.RemoveOwn(ctx, again)).ID != again {
		t.Fatal("removed")
	}
	entry = f.entry(uid)
	if !entry.Start.Equal(local("2026-10-23T13:00:00")) || entry.Cancelled {
		t.Fatal(entry)
	}

	// Deleted, an own event does not come back with an older version; the chats' ones are hidden, never deleted.
	if must(f.RemoveOwn(ctx, newer)).ID != newer {
		t.Fatal("removed")
	}
	for _, e := range must(f.Events.InForceAll()) {
		if e.Title == "Supervisor" {
			t.Fatal("back")
		}
	}
	if must(f.RemoveOwn(ctx, talk)) != nil || must(f.EditEvent(later, Change{Title: ptr("x")})) != nil {
		t.Fatal("not own, a correction")
	}
	if must(f.CorrectClass(ctx, "x@ruz.hse.ru", "2026-10-23T15:00:00", "2026-10-23T16:00:00", "", false, nil, nil,
		true)) != nil {
		t.Fatal("no such class")
	}
}

func TestOwnCorrectionMovesTheMidterm(t *testing.T) {
	f := loaded(t)
	ctx := context.Background()
	must(f.AfterPoll(ctx, ruz.Diff{First: true}))
	var published []MidtermChanges
	f.OnMidterms = func(ctx context.Context, m MidtermChanges) error { published = append(published, m); return nil }
	uid := "2000007@ruz.hse.ru" // the interim assessment, Thu Oct 29
	mine := *must(f.CorrectClass(ctx, uid, "2026-11-05T10:00:00", "2026-11-05T13:00:00", "R401", false, nil, nil, true))
	if must(f.Deadlines.Get(published[0].Moved[0][1])).Due != "2026-11-05" {
		t.Fatal(published)
	}
	must(f.RemoveOwn(ctx, mine))
	if must(f.Deadlines.Get(published[1].Moved[0][1])).Due != "2026-10-29" {
		t.Fatal(published)
	}
}

func TestNewTablesJoinAnOlderDatabase(t *testing.T) {
	path := t.TempDir() + "/infomaster.db"
	s := must(store.Open(path))
	must(s.DB.Exec("insert into deadlines(source, label, due, created_at) values('tg', 'DM HW 3', '2026-10-21', " +
		"'2026-10-20T12:00:00')"))
	for _, table := range []string{"subjects", "classes", "events"} {
		must(s.DB.Exec("drop table " + table)) // as stage 3 left it
	}
	noErr(s.Close())
	s = must(store.Open(path))
	defer s.Close()
	var n int
	noErr(s.DB.QueryRow("select count(*) from deadlines").Scan(&n))
	if n != 1 {
		t.Fatal(n)
	}
	for _, table := range []string{"subjects", "classes", "events", "deadlines", "messages"} {
		noErr(s.DB.QueryRow("select count(*) from sqlite_master where type = 'table' and name = ?", table).Scan(&n))
		if n != 1 {
			t.Fatal(table)
		}
	}
}

func TestAllDayEventInTheAgenda(t *testing.T) {
	f := loaded(t)
	ev := event()
	ev.Start, ev.End, ev.Room = "2026-10-21", "", "S301"
	id := f.add(ev)
	for _, item := range must(f.Agenda(time.Date(2026, 10, 20, 0, 0, 0, 0, clock.TZ), AgendaDays)).Items {
		if item.Title == "Alg consult" {
			if item.Start != "2026-10-21" || item.End != nil || item.Event.Start != "2026-10-21" || item.Event.End != nil ||
				item.Event.ID != id {
				t.Fatal(item)
			}
			return
		}
	}
	t.Fatal("no all-day event")
}

func TestNextClassAndSameCourse(t *testing.T) {
	f := loaded(t)
	must(f.AfterPoll(context.Background(), ruz.Diff{First: true}))
	if !must(f.SameCourse("Calculus", "Математический анализ")) || must(f.SameCourse("Calculus", "")) {
		t.Fatal("same course")
	}
	next := must(f.NextClass("Математический анализ", local("2026-10-19T12:00:00"), SheetDays))
	if next != nil { // the Calculus seminar of Oct 27 is cancelled
		t.Fatal(next)
	}
	next = must(f.NextClass("Algebra", local("2026-10-19T12:00:00"), SheetDays))
	if next == nil || next.UID != "2000002@ruz.hse.ru" { // no seminar: the next lecture
		t.Fatal(next)
	}
	upcoming := must(f.UpcomingClasses(time.Date(2026, 10, 20, 0, 0, 0, 0, clock.TZ)))
	if upcoming[0].UID != "2000002@ruz.hse.ru" || upcoming[0].Title != "Algebra" || upcoming[0].Type != "Lecture" {
		t.Fatal(upcoming[0])
	}
}
