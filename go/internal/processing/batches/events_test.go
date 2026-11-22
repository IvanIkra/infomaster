package batches

import (
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
)

type happeningOpt func(map[string]any)

func with(key string, value any) happeningOpt { return func(m map[string]any) { m[key] = value } }

func happening(opts ...happeningOpt) map[string]any {
	m := map[string]any{"course": "Алгебра", "title": "Alg consult", "start": "2026-10-23T18:10", "end": "",
		"room": "R205", "kind": "consultation", "required": true, "moves": "", "cancels": ""}
	for _, o := range opts {
		o(m)
	}
	return m
}

// timetable is the RUZ classes of the fixture feed, their subjects named.
func timetable(t *testing.T) *fixture {
	f := setup(t)
	diff := must(f.Schedule.Ruz.Sync(must(ruz.Parse(must(os.ReadFile("testdata/ruz.ics")))), false))
	must(f.Schedule.AfterPoll(ctx, *diff))
	return f
}

func (f *fixture) runChats(chatIDs ...int64) Changes { return must(f.RunChats(ctx, chatIDs, nil)) }

func (f *fixture) event(id int64) *events.Event { return must(f.Events.Get(id)) }

func TestEventsAndTheTimetableInThePrompt(t *testing.T) {
	f := timetable(t)
	f.add(1, "Консультация по алгебре 23.10 в 18:10, R205")
	f.llm.push(answer(item(l(1), withEvents(happening(), happening(with("title", ""),
		with("start", "2026-10-24T10:00"))))))
	changes := f.runChats(chat)
	prompt := f.prompt(0)
	contains(t, prompt, "event title at most 13 characters")
	contains(t, prompt, "Upcoming events:\n(none)")
	contains(t, prompt, `- c2: Tue 2026-10-20 13:00-14:20 "Algebra" Lecture, R401`)
	lacks(t, prompt, "- c1:") // yesterday's class
	contains(t, prompt, `- c7: Tue 2026-10-27 09:30-10:50 "Calculus" Seminar, D208, cancelled`)
	contains(t, prompt, `- c8: Thu 2026-10-29 10:00-13:00 "Intro to Prog" (Введение в программирование) Interim `+
		"assessment, R401")
	expect(t, len(changes.Events) == 1, "events %v", changes.Events)
	eventID := changes.Events[0]
	ev := f.event(eventID)
	expect(t, ev.UID == events.UID("tg", chat, 1, 0) && ev.Title == "Alg consult" && ev.Start == "2026-10-23T18:10:00" &&
		ev.End == nil && ev.Required, "event %+v", ev)
	expect(t, must(f.Subjects.Abbr("Алгебра")) == "Alg", "the course went to the dictionary after the run")

	f.add(2, "Напоминаю про консультацию")
	f.llm.push(answer(item(l(2), withEvents(happening()))))
	changes = f.parseChat(chat)
	expect(t, len(changes.Events) == 0, "events %v", changes.Events)
	contains(t, f.prompt(1), fmt.Sprintf(`- e%d: Fri 2026-10-23 18:10 "Alg consult" consultation, R205, required`,
		eventID))
	lacks(t, f.prompt(1), `"Calculus" (Математический анализ)`) // the same name: no repeat
	contains(t, f.prompt(1), `"Rus Statehood" (Основы российской государственности) Lecture, online`)
}

func TestNoTimetableForAChatThatIsNotStudy(t *testing.T) {
	f := timetable(t)
	must(f.ChatSettings.Update(chat, map[string]any{"study": false}))
	f.add(1, "Ярмарка вакансий 24.10")
	f.llm.push(answer())
	f.parseChat(chat)
	lacks(t, f.prompt(0), "Timetable classes")
	contains(t, f.prompt(0), "Upcoming events:")
}

func TestMoveAndCancelAnEvent(t *testing.T) {
	f := timetable(t)
	f.add(1, "Консультация 23.10 в 18:10")
	f.llm.push(answer(item(l(1), withEvents(happening(with("end", "2026-10-23T19:30"))))))
	first := f.parseChat(chat).Events[0]
	must(f.Events.SetRequired(first, false)) // the owner's choice

	f.add(2, "Консультацию переносим на субботу 12:00")
	f.llm.push(answer(item(l(2), withEvents(happening(with("start", "2026-10-24T12:00"), with("room", ""),
		with("moves", fmt.Sprintf("e%d", first)))))))
	changes := f.parseChat(chat)
	expect(t, len(changes.EventsMoved) == 1 && changes.EventsMoved[0][0] == first, "moved %v", changes.EventsMoved)
	moved := changes.EventsMoved[0][1]
	version := f.event(moved)
	expect(t, version.Start == "2026-10-24T12:00:00" && *version.End == "2026-10-24T13:20:00" &&
		version.Room == "R205", "the same length and room: %+v", version)
	expect(t, version.UID == f.event(first).UID && !version.Required, "uid and choice")

	f.add(3, "Консультации не будет")
	f.llm.push(answer(item(l(3), withEvents(happening(with("start", ""), with("cancels", fmt.Sprintf("e%d", moved)))))))
	changes = f.parseChat(chat)
	cancelled := changes.EventsMoved[0][1]
	expect(t, f.event(cancelled).Cancelled && f.event(cancelled).Start == "2026-10-24T12:00:00", "cancelled")

	must(f.Inbox.Delete(l(3), ptr(chat), f.Now())) // the cancelling message is gone: the event is back
	changes = f.parseChat(chat)
	expect(t, reflect.DeepEqual(changes.EventsGone, l(cancelled)) && must(f.Events.ByUID(version.UID)).ID == moved,
		"back")
}

func TestMoveAndCancelAClass(t *testing.T) {
	f := timetable(t)
	f.add(1, "Семинар по дискретке в четверг переносится на пятницу 13:00, R405")
	f.llm.push(answer(item(l(1), withEvents(happening(with("title", "Discrete Math"), with("start", "2026-10-23T13:00"),
		with("room", "R405"), with("kind", "class"), with("moves", "c4"))))))
	fix := f.parseChat(chat).Events[0]
	correction := f.event(fix)
	expect(t, correction.UID == "2000004@ruz.hse.ru" && *correction.ClassUID == "2000004@ruz.hse.ru" &&
		*correction.End == "2026-10-23T14:20:00" && correction.Room == "R405", "correction %+v", correction)
	expect(t, *correction.Course == "Discrete Mathematics" && correction.Title == "Discrete Math" &&
		correction.Required, "correction %+v", correction)

	f.add(2, "Пары по алгебре сегодня не будет")
	f.llm.push(answer(item(l(2), withEvents(happening(with("start", ""), with("cancels", "c2")))),
		item(l(2), withEvents(happening(with("start", ""), with("cancels", "c7")))))) // cancelled in RUZ
	changes := f.parseChat(chat)
	expect(t, len(changes.Events) == 1, "events %v", changes.Events)
	cancel := f.event(changes.Events[0])
	expect(t, cancel.Cancelled && *cancel.ClassUID == "2000002@ruz.hse.ru", "cancel %+v", cancel)
	contains(t, f.prompt(1), `- c4: Thu 2026-10-22 16:40-18:00 "Discrete Math" (Discrete Mathematics) Seminar, `+
		"online, moved by a chat message to Fri 2026-10-23 13:00-14:20, R405")

	f.add(3, "Всё же семинар по дискретке остаётся в четверг")
	f.llm.push(answer(item(l(3), withEvents(happening(with("start", "2026-10-22T16:40"), with("end", "2026-10-22T18:00"),
		with("room", ""), with("moves", "c4"))))))
	back := f.parseChat(chat).Events[0]
	expect(t, *f.event(fix).ReplacedBy == back, "replaced")
	lacks(t, f.classLine(4), "moved by a chat")
	contains(t, f.classLine(2), "cancelled by a chat")
}

func (f *fixture) classLine(id int64) string {
	for _, c := range must(f.Schedule.UpcomingClasses(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))) {
		if c.ID == id {
			return ClassLine(c)
		}
	}
	panic("no class")
}

func TestEditsAndDeletionsOfEventMessages(t *testing.T) {
	f := setup(t)
	f.add(1, "Консультация 23.10 в 18:10 и лекция гостя 24.10 в 12:00")
	f.llm.push(answer(item(l(1), withEvents(happening(), happening(with("title", "Guest talk"), with("kind", "talk"),
		with("start", "2026-10-24T12:00"), with("required", false))))))
	evs := f.parseChat(chat).Events
	consult, talk := evs[0], evs[1]
	expect(t, f.event(talk).UID == events.UID("tg", chat, 1, 1), "two events of one message")

	f.clock.add(5 * time.Minute)
	must(f.Inbox.Edit(chat, 1, "Консультация 23.10 в 19:00", nil, f.Now()))
	f.llm.push(answer(item(l(1), withEvents(happening(with("start", "2026-10-23T19:00"))))))
	changes := f.parseChat(chat)
	expect(t, len(changes.EventsMoved) == 1 && changes.EventsMoved[0][0] == consult, "moved %v", changes.EventsMoved)
	newer := changes.EventsMoved[0][1]
	expect(t, f.event(newer).UID == f.event(consult).UID, "uid")
	expect(t, reflect.DeepEqual(changes.EventsGone, l(talk)) && len(changes.Events) == 0, "changes %+v", changes)

	f.clock.add(5 * time.Minute)
	must(f.Inbox.Edit(chat, 1, "Консультация 23.10 в 19:00!", nil, f.Now())) // the same event again: nothing
	f.llm.push(answer(item(l(1), withEvents(happening(with("start", "2026-10-23T19:00"))))))
	changes = f.parseChat(chat)
	expect(t, len(changes.Events)+len(changes.EventsMoved)+len(changes.EventsGone) == 0, "changes %+v", changes)

	must(f.Inbox.Delete(l(1), ptr(chat), f.Now()))
	changes = f.parseChat(chat)
	expect(t, reflect.DeepEqual(changes.EventsGone, l(newer)) &&
		len(must(f.Events.Upcoming(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)))) == 0, "gone")
}

func TestEventTitleRetry(t *testing.T) {
	f := setup(t)
	f.add(1, "Консультация по алгебре 23.10")
	f.llm.push(answer(item(l(1), withEvents(happening(with("title", "Algebra exam consultation"))))),
		answer(item(l(1), withEvents(happening()))))
	f.parseChat(chat)
	contains(t, f.prompt(1), `event title "Algebra exam consultation" has 25 characters, keep it within 13`)
	expect(t, must(f.Events.Upcoming(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)))[0].Title == "Alg consult",
		"title")
}

func TestCourseNamesWaitForHaiku(t *testing.T) {
	f := setup(t)
	f.namer.set(llm.NewError(llm.Failed, "down"))
	f.add(1, "Консультация по алгебре 23.10")
	f.llm.push(answer(item(l(1), withEvents(happening()))))
	f.runChats(chat)
	expect(t, must(f.Subjects.Get("Алгебра")) == nil && f.Subjects.LastError() == "down", "not named")
	expect(t, len(must(f.Events.Upcoming(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)))) > 0,
		"the event is saved all the same")
	f.namer.set(nil)
	must(f.Tick(ctx)) // the next run asks again
	expect(t, must(f.Subjects.Abbr("Алгебра")) == "Alg", "named")
}

func TestCheckEvents(t *testing.T) {
	a := decoded(t, map[string]any{"items": []any{map[string]any{"msg_ids": []int{1},
		"news": map[string]any{"full": "", "short": "", "importance": 3, "replaces": ""}, "deadlines": []any{},
		"events": []any{
			happening(with("start", "23.10 18:10")),                                                     // bad time
			happening(with("title", ""), with("start", ""), with("cancels", "e5"), with("moves", "c3")), // a cancel
			happening(with("end", "2026-10-23T17:00"), with("moves", "x5"), with("required", "yes"), with("kind", "")),
			happening(with("title", " "), with("start", "2026-10-23T18:10")),                     // no title
			happening(with("title", ""), with("start", "2026-10-24T12:00"), with("moves", "c3")), // a move: kept
			happening(with("title", ""), with("start", ""), with("moves", "e5")),                 // no time
		}}}})
	items := Check(a, set(1), nil, false)
	expect(t, len(items) == 1 && len(items[0].Events) == 3, "items %+v", items)
	cancel, odd, move := items[0].Events[0], items[0].Events[1], items[0].Events[2]
	expect(t, cancel.Cancels == "e5" && cancel.Moves == "" && cancel.Start == "", "cancel %+v", cancel)
	expect(t, odd.End == "" && odd.Moves == "" && odd.Required && odd.Kind == "event", "odd %+v", odd)
	expect(t, move.Title == "" && move.Moves == "c3" && move.Start == "2026-10-24T12:00:00", "move %+v", move)
	expect(t, items[0].News == nil, "news")
}

func TestCourseDictionaryInThePrompt(t *testing.T) {
	f := setup(t)
	f.add(1, "ДЗ 5 по матану до четверга")
	f.llm.push(answer(), answer())
	f.parseChat(chat)
	header := "Course dictionary (abbreviation \"title\": the names it goes by):\n"
	contains(t, f.prompt(0), header+"(empty)\n")

	must(f.Subjects.SetManual("Алгебра", "Algebra", "Alg"))
	must(f.Subjects.SetManual("Линал", "Algebra", "Alg"))
	f.add(2, "КР по алгебре 7.11")
	f.parseChat(chat)
	contains(t, f.prompt(1), header+`- Alg "Algebra": "Алгебра", "Линал"`+"\n")
}

func TestAChatMovesAndCancelsAMidterm(t *testing.T) {
	f := timetable(t)
	prog := must(f.Deadlines.Get(*must(f.Schedule.Ruz.ByUID("2000007@ruz.hse.ru")).DeadlineID))
	expect(t, prog.Label == "Prog Midterm" && prog.Due == "2026-10-29", "prog %+v", prog)
	f.add(1, "РК по программированию переносится на четверг 5 ноября, 10:00")
	f.llm.push(answer(item(l(1), withEvents(happening(with("title", ""), with("start", "2026-11-05T10:00"),
		with("room", ""), with("kind", "test"), with("moves", "c8"))))))
	changes := f.runChats(chat)
	expect(t, len(changes.Events) == 1 && f.event(changes.Events[0]).Title == "Intro to Prog",
		"a move without a title: the class keeps its own")
	expect(t, len(changes.Moved) == 1 && changes.Moved[0][0] == prog.ID, "the Midterm follows the class")
	newer := must(f.Deadlines.Get(changes.Moved[0][1]))
	expect(t, newer.Label == "Prog Midterm" && newer.Due == "2026-11-05", "newer %+v", newer)

	f.add(2, "РК по программированию отменяется")
	f.llm.push(answer(item(l(2), withEvents(happening(with("title", ""), with("start", ""), with("cancels", "c8"))))))
	changes = f.runChats(chat)
	expect(t, reflect.DeepEqual(changes.Gone, l(newer.ID)) && len(changes.Moved) == 0, "changes %+v", changes)

	must(f.Inbox.Delete(l(2), ptr(chat), f.Now())) // the cancellation is withdrawn: the move is in force again
	changes = f.runChats(chat)
	expect(t, len(changes.Deadlines) == 1 && must(f.Deadlines.Get(changes.Deadlines[0])).Due == "2026-11-05",
		"changes %+v", changes)
}

func TestAMoveNeedsNoTitle(t *testing.T) {
	f := timetable(t)
	f.add(1, "Консультация по алгебре 23.10 в 18:10")
	f.llm.push(answer(item(l(1), withEvents(happening()))))
	first := f.parseChat(chat).Events[0]
	f.add(2, "Консультацию переносим на субботу 12:00")
	f.llm.push(answer(item(l(2), withEvents(
		happening(with("title", ""), with("start", "2026-10-24T12:00"), with("moves", fmt.Sprintf("e%d", first))),
		happening(with("title", ""), with("start", "2026-10-25T12:00"), with("moves", "e999"))))))
	changes := f.parseChat(chat)
	expect(t, len(changes.EventsMoved) == 1 && changes.EventsMoved[0][0] == first &&
		f.event(changes.EventsMoved[0][1]).Title == "Alg consult", "moved %v", changes.EventsMoved)
	expect(t, len(changes.Events) == 0, "a move of nothing listed is no event of its own without a name")
}

func TestAMovedClassStaysListedAfterItsDay(t *testing.T) {
	f := timetable(t)
	uid := "2000004@ruz.hse.ru" // Discrete Mathematics, Thu Oct 22 16:40
	f.add(1, "Семинар по дискретке в четверг переносится на вторник 27.10, 13:00")
	move := happening(with("title", ""), with("start", "2026-10-27T13:00"), with("room", ""), with("kind", "class"),
		with("moves", "c4"))
	f.llm.push(answer(item(l(1), withEvents(move))))
	fix := f.parseChat(chat).Events[0]

	f.clock.add(3 * 24 * time.Hour) // Fri Oct 23: Thursday is past, the move is ahead
	must(f.Inbox.Edit(chat, 1, "Семинар по дискретке в четверг переносится на вторник 27.10, в 13:00", nil, f.Now()))
	f.llm.push(answer(item(l(1), withEvents(move))))
	changes := f.parseChat(chat)
	contains(t, f.prompt(1), `- c4: Thu 2026-10-22 16:40-18:00 "Discrete Math"`)
	expect(t, len(changes.EventsGone) == 0 && must(f.Events.Correction(uid)).ID == fix, "kept")
	lacks(t, f.prompt(1), "- c1:") // a class past without a move is still left out

	must(f.ChatSettings.Update(chat, map[string]any{"study": false})) // no classes shown: cannot be restated
	must(f.Inbox.Edit(chat, 1, "Семинар по дискретке переносится на вторник 27.10, 13:00", nil, f.Now()))
	f.llm.push(answer())
	changes = f.parseChat(chat)
	expect(t, len(changes.EventsGone) == 0 && must(f.Events.Correction(uid)).ID == fix, "kept")
}

func TestDeletedSupersededVersionNeverComesBack(t *testing.T) {
	f := timetable(t)
	f.add(1, "Консультация 23.10 в 18:10")
	f.llm.push(answer(item(l(1), withEvents(happening(with("end", "2026-10-23T19:30"))))))
	v1 := f.parseChat(chat).Events[0]
	f.add(2, "Консультация переносится на 24.10 12:00")
	f.llm.push(answer(item(l(2), withEvents(happening(with("start", "2026-10-24T12:00"),
		with("moves", fmt.Sprintf("e%d", v1)))))))
	v2 := f.parseChat(chat).EventsMoved[0][1]
	f.add(3, "Консультация переносится на 25.10 12:00")
	f.llm.push(answer(item(l(3), withEvents(happening(with("start", "2026-10-25T12:00"),
		with("moves", fmt.Sprintf("e%d", v2)))))))
	v3 := f.parseChat(chat).EventsMoved[0][1]

	must(f.Inbox.Delete(l(2), ptr(chat), f.Now())) // the message of a superseded version: no news
	changes := f.parseChat(chat)
	uid := f.event(v1).UID
	expect(t, len(changes.EventsGone) == 0 && f.event(v2).GoneAt != nil, "gone quietly")
	expect(t, must(f.Events.ByUID(uid)).ID == v3, "v3 in force")

	must(f.Inbox.Delete(l(3), ptr(chat), f.Now())) // the version in force is withdrawn: the first one is back
	changes = f.parseChat(chat)
	expect(t, reflect.DeepEqual(changes.EventsGone, l(v3)) && must(f.Events.ByUID(uid)).ID == v1, "v1 back")
	changes = f.parseChat(chat) // nothing more to withdraw
	expect(t, len(changes.EventsGone) == 0, "nothing more")
}

func TestAnEventThatRepeatsAClassIsNotSaved(t *testing.T) {
	f := timetable(t)
	f.add(1, "Завтра в 13:00 лекция по алгебре, не опаздывайте")
	f.add(2, "Семинар по SE в пятницу в 109")
	f.add(3, "Пара по алгебре переносится на 24.10 10:00")
	f.llm.push(answer(
		item(l(1), withEvents(happening(with("title", "Algebra"), with("start", "2026-10-20T13:00"), with("room", ""),
			with("kind", "lecture"), with("course", "")))),
		item(l(2), withEvents(happening(with("title", "SE seminar"), with("start", "2026-10-23T09:30"),
			with("room", "109"), with("kind", "class"), with("course", "")))),
		item(l(3), withEvents(happening(with("title", "Algebra"), with("start", "2026-10-24T10:00"),
			with("kind", "class"), with("moves", "c99"))))))
	changes := f.parseChat(chat)
	expect(t, len(changes.Events) == 0 && len(must(f.Events.InForceAll())) == 0,
		"the classes themselves; c99 is not listed")

	f.add(4, "Во время лекции по алгебре будет консультация по КР") // not a regular kind: it stays
	f.llm.push(answer(item(l(4), withEvents(happening(with("start", "2026-10-20T13:30"), with("room", "R401"))))))
	expect(t, len(f.parseChat(chat).Events) == 1, "saved")
}

func TestAnEventWithoutItsTimeIsAllDay(t *testing.T) {
	f := timetable(t)
	f.add(1, "Завтра консультация в S301")
	f.llm.push(answer(item(l(1), withEvents(happening(with("start", "2026-10-21"), with("end", "2026-10-21T19:00"),
		with("room", "S301"), with("required", false))))))
	eventID := f.parseChat(chat).Events[0]
	ev := f.event(eventID)
	expect(t, ev.Start == "2026-10-21" && ev.End == nil && ev.Room == "S301", "event %+v", ev)
	var entry *schedule.Entry
	for _, e := range must(f.Schedule.Entries()) {
		if e.UID == ev.UID {
			entry = &e
		}
	}
	expect(t, entry != nil && entry.AllDay && entry.Start.Format("2006-01-02T15:04") == "2026-10-21T00:00" &&
		entry.End.Format("2006-01-02T15:04") == "2026-10-22T00:00", "entry %+v", entry)

	f.add(2, "Консультация завтра в 18:10") // the time comes: the event moves, its day kept
	f.llm.push(answer(item(l(2), withEvents(happening(with("start", "2026-10-21T18:10"),
		with("moves", fmt.Sprintf("e%d", eventID)))))))
	moved := f.parseChat(chat).EventsMoved[0][1]
	expect(t, f.event(moved).Start == "2026-10-21T18:10:00" && f.event(moved).End == nil, "moved")
	contains(t, f.prompt(1), fmt.Sprintf(`- e%d: Wed 2026-10-21 (all day) "Alg consult" consultation, S301, optional`,
		eventID))
}

func TestOwnEventsAreListedButNotMoved(t *testing.T) {
	f := timetable(t)
	own := must(f.Schedule.AddEvent(schedule.Fields{Title: "Alg consult", Kind: "consultation",
		Start: "2026-10-23T18:10:00", End: "2026-10-23T19:30:00", Room: "R205", Required: true}))
	f.add(1, "Консультация по алгебре переносится на субботу 12:00")
	f.llm.push(answer(item(l(1), withEvents(happening(with("start", "2026-10-24T12:00"),
		with("moves", fmt.Sprintf("e%d", own)))))))
	changes := f.parseChat(chat)
	contains(t, f.prompt(0), fmt.Sprintf(`- e%d: Fri 2026-10-23 18:10-19:30 "Alg consult" consultation, R205, `+
		"required, own", own))
	expect(t, len(changes.EventsMoved)+len(changes.Events) == 0 && f.event(own).ReplacedBy == nil, "not moved")
}

func TestADayWithoutATimeJoinsTheEventOfThatDay(t *testing.T) {
	f := timetable(t)
	f.add(1, "Консультация по алгебре в пятницу в 18:10")
	f.llm.push(answer(item(l(1), withEvents(happening(with("room", ""), with("end", "2026-10-23T19:30"))))))
	first := f.parseChat(chat).Events[0]

	f.add(2, "Консультация в пятницу в S301") // the room of the known one, not an all-day event of its own
	f.llm.push(answer(item(l(2), withEvents(happening(with("start", "2026-10-23"), with("room", "S301"))))))
	changes := f.parseChat(chat)
	expect(t, len(changes.EventsMoved) == 1 && changes.EventsMoved[0][0] == first && len(changes.Events) == 0,
		"changes %+v", changes)
	version := f.event(changes.EventsMoved[0][1])
	expect(t, version.Start == "2026-10-23T18:10:00" && *version.End == "2026-10-23T19:30:00" &&
		version.Room == "S301" && version.UID == f.event(first).UID, "version %+v", version)

	f.add(3, "Консультация по алгебре в субботу") // all day first, its time later
	f.llm.push(answer(item(l(3), withEvents(happening(with("start", "2026-10-24"), with("room", "R205"))))))
	day := f.parseChat(chat).Events[0]
	f.add(4, "Консультация в субботу в 12:00")
	f.llm.push(answer(item(l(4), withEvents(happening(with("start", "2026-10-24T12:00"), with("room", ""))))))
	changes = f.parseChat(chat)
	expect(t, changes.EventsMoved[0][0] == day, "moved %v", changes.EventsMoved)
	timed := f.event(changes.EventsMoved[0][1])
	expect(t, timed.Start == "2026-10-24T12:00:00" && timed.Room == "R205", "timed %+v", timed)

	own := must(f.Schedule.AddEvent(schedule.Fields{Title: "Alg consult", Kind: "consultation",
		Start: "2026-10-26T15:00:00", Room: "", Required: true}))
	f.add(5, "В понедельник консультация в S301") // the owner's own: left as it is, no copy either
	f.llm.push(answer(item(l(5), withEvents(happening(with("start", "2026-10-26"), with("room", "S301"))))))
	changes = f.parseChat(chat)
	expect(t, len(changes.Events)+len(changes.EventsMoved) == 0 && f.event(own).ReplacedBy == nil, "own")
}
