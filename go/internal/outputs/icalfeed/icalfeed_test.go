package icalfeed

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const chat = int64(-1001234567890)

var names = map[string][2]string{
	"Математический анализ": {"Calculus", "Calc"},
	"Алгебра":                                {"Algebra", "Alg"},
	"Discrete Mathematics":                   {"Discrete Math", "DM"},
	"Introduction into Software Engineering": {"Intro to SE", "SE"},
	"Основы российской государственности": {"Rus Statehood", "RSF"},
	"Foundations of Russian Statehood":    {"Rus Statehood", "RSF"},
	"Введение в программирование":         {"Intro to Prog", "Prog"},
	"Calculus": {"Calculus", "Calc"},
}

func namer() *llm.Fake {
	return &llm.Fake{Respond: func(req llm.Request) (any, error) {
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

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func ptr[T any](v T) *T { return &v }

func timetable(t *testing.T) *schedule.Schedule {
	t.Helper()
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	now := func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC() }
	r := ruz.New("", s)
	r.Now = now
	sch := schedule.New(s, r, events.Open(s), deadlines.Open(s), subjects.New(s, namer()), inbox.Open(s))
	sch.Now = now
	return sch
}

func event(change func(*events.New)) events.New {
	ev := events.New{UID: events.UID("tg", chat, 1, 0), Source: "tg", ChatID: ptr(chat), MsgIDs: []int64{1},
		Title: "Alg consult", Kind: "consultation", Start: "2026-10-23T18:10:00", End: "2026-10-23T19:30:00",
		Room: "R205", Required: true}
	if change != nil {
		change(&ev)
	}
	return ev
}

func addEvent(sch *schedule.Schedule, msgID int64, change func(*events.New)) {
	ev := event(func(e *events.New) { e.UID, e.MsgIDs = events.UID("tg", chat, msgID, 0), []int64{msgID} })
	if change != nil {
		change(&ev) // a uid given by the change wins
	}
	must(sch.Events.Add(ev, sch.Now()))
}

// loaded is the timetable behind the feeds of test_ical_feed.py.
func loaded(t *testing.T) *CalendarFeed {
	sch := timetable(t)
	ctx := context.Background()
	must(sch.Inbox.PutChat(chat, "Матан", "supergroup", nil, 0))
	diff := must(sch.Ruz.Sync(must(ruz.Parse(must(os.ReadFile("testdata/ruz.ics")))), false))
	must(sch.AfterPoll(ctx, *diff))
	must(sch.Subjects.Resolve(ctx, []string{"Алгебра"}))
	addEvent(sch, 1, func(e *events.New) { e.Course = "Алгебра" })
	addEvent(sch, 2, func(e *events.New) {
		e.Title, e.Kind, e.Start, e.End, e.Room, e.Required = "Career fair", "event", "2026-10-24T12:00:00",
			"2026-10-24T16:00:00", "https://events.example.org/fair", false
	})
	addEvent(sch, 3, func(e *events.New) {
		e.Title, e.Kind, e.Start, e.End, e.Room, e.Required, e.Hidden = "Club", "meeting", "2026-10-24T18:00:00", "",
			"", false, true
	})
	addEvent(sch, 4, func(e *events.New) {
		e.Title, e.Kind, e.Required, e.Cancelled, e.Start, e.End, e.Room = "Old talk", "talk", false, true,
			"2026-10-25T15:00:00", "", ""
	})
	item := must(sch.Ruz.ByUID("2000005@ruz.hse.ru")) // Intro to SE, Fri Oct 23 09:30: moved by a chat
	addEvent(sch, 5, func(e *events.New) {
		e.UID, e.ClassUID, e.Course, e.Title, e.Kind = item.UID, item.UID, item.Subject, "Intro to SE", item.Kind
		e.Start, e.End, e.Room = "2026-10-23T11:10:00", "2026-10-23T12:30:00", "R301"
	})
	item2 := must(sch.Ruz.ByUID("2000002@ruz.hse.ru")) // Algebra, Tue Oct 20 13:00: cancelled by a chat
	addEvent(sch, 6, func(e *events.New) {
		e.UID, e.ClassUID, e.Course, e.Title, e.Kind = item2.UID, item2.UID, item2.Subject, "Algebra", item2.Kind
		e.Start, e.End, e.Room, e.Cancelled = item2.Start, item2.End, item2.Room, true
	})
	feed := New(sch)
	feed.Now = sch.Now
	return feed
}

// vevent is one event as a calendar reads it: unfolded, the TEXT values unescaped.
type vevent map[string]string

func unescape(v string) string {
	return strings.NewReplacer(`\N`, "\n", `\n`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(v)
}

func parse(t *testing.T, text []byte) []vevent {
	t.Helper()
	unfolded := strings.ReplaceAll(string(text), "\r\n ", "")
	var out []vevent
	var current vevent
	for _, line := range strings.Split(strings.TrimSuffix(unfolded, "\r\n"), "\r\n") {
		name, value, _ := strings.Cut(line, ":")
		switch {
		case line == "BEGIN:VEVENT":
			current = vevent{}
		case line == "END:VEVENT":
			out = append(out, current)
			current = nil
		case current != nil:
			current[name] = unescape(value)
		}
	}
	return out
}

func render(t *testing.T, feed *CalendarFeed, name string) []byte {
	return must(feed.Render(name))
}

func byKey(t *testing.T, text []byte, key string) map[string]vevent {
	out := map[string]vevent{}
	for _, e := range parse(t, text) {
		out[e[key]] = e
	}
	return out
}

func TestMainAndOptional(t *testing.T) {
	feed := loaded(t)
	main, optional := byKey(t, render(t, feed, "main"), "SUMMARY"), byKey(t, render(t, feed, "optional"), "SUMMARY")
	for _, s := range []string{"Calculus · Seminar", "Algebra · Lecture", "Intro to SE · Seminar", "Alg consult",
		"Rus Statehood · Lecture", "Intro to Prog · Midterm"} {
		if _, ok := main[s]; !ok {
			t.Errorf("main lacks %q", s)
		}
	}
	if len(optional) != 2 || optional["Career fair"] == nil || optional["Old talk"] == nil {
		t.Fatal(optional)
	}
	if _, ok := main["Club"]; ok || optional["Career fair"]["STATUS"] != "CONFIRMED" {
		t.Fatal("club or status")
	}
	if !strings.Contains(string(render(t, feed, "main")), "\r\nX-WR-CALNAME:Study\r\n") {
		t.Fatal("name")
	}
}

func TestTabloFeed(t *testing.T) {
	feed := loaded(t)
	evs := byKey(t, render(t, feed, "tablo"), "UID")
	if len(evs) != 9+3 {
		t.Fatal(len(evs))
	}
	var fair vevent
	for _, e := range evs {
		if e["SUMMARY"] == "Career fair" {
			fair = e
		}
		if _, ok := e["URL"]; ok {
			t.Fatal("URL")
		}
	}
	if fair["STATUS"] != "TENTATIVE" || fair["X-INFOMASTER-OPTIONAL"] != "TRUE" || fair["X-INFOMASTER-SOURCE"] != "tg" ||
		fair["X-INFOMASTER-LINK"] != "https://t.me/c/1234567890/2" ||
		fair["LOCATION"] != "https://events.example.org/fair" {
		t.Fatal(fair)
	}
	calc := evs["2000001@ruz.hse.ru"]
	if calc["SUMMARY"] != "Calculus" || calc["DESCRIPTION"] != "Seminar" || calc["LOCATION"] != "D208" ||
		calc["STATUS"] != "CONFIRMED" || calc["X-INFOMASTER-SOURCE"] != "ruz" || calc["DTSTART"] != "20261019T063000Z" {
		t.Fatal(calc)
	}
	if _, ok := calc["X-INFOMASTER-LINK"]; ok {
		t.Fatal("link")
	}
	if _, ok := calc["X-INFOMASTER-OPTIONAL"]; ok {
		t.Fatal("optional")
	}
	moved := evs["2000005@ruz.hse.ru"]
	if moved["DESCRIPTION"] != "Seminar\nMoved from Fri Oct 23 09:30 in a chat" || moved["LOCATION"] != "R301" ||
		moved["X-INFOMASTER-SOURCE"] != "tg" || moved["X-INFOMASTER-LINK"] != "https://t.me/c/1234567890/5" {
		t.Fatal(moved)
	}
	cancelled := evs["2000002@ruz.hse.ru"]
	if cancelled["STATUS"] != "CANCELLED" || cancelled["DESCRIPTION"] != "Lecture\nCancelled in a chat" {
		t.Fatal(cancelled)
	}
	if evs["2000008@ruz.hse.ru"]["STATUS"] != "CANCELLED" ||
		evs["2000006@ruz.hse.ru"]["DESCRIPTION"] != "Interim assessment" {
		t.Fatal("8, 6")
	}
	if consult := evs[events.UID("tg", chat, 1, 0)]; consult["DESCRIPTION"] != "Consultation\nAlgebra" {
		t.Fatal(consult)
	}
}

func TestEventWithoutEnd(t *testing.T) {
	sch := timetable(t)
	addEvent(sch, 1, func(e *events.New) { e.End = "" })
	feed := New(sch)
	evs := parse(t, render(t, feed, "main"))
	// The default length: 80 minutes from 15:10 UTC.
	if len(evs) != 1 || evs[0]["DTSTART"] != "20261023T151000Z" || evs[0]["DTEND"] != "20261023T163000Z" {
		t.Fatal(evs)
	}
}

func TestAllDayEventAndClassTwins(t *testing.T) {
	feed := loaded(t)
	sch := feed.Schedule.(*schedule.Schedule)
	addEvent(sch, 7, func(e *events.New) { e.Title, e.Start, e.End, e.Room = "Alg consult", "2026-10-21", "", "S301" })
	addEvent(sch, 8, func(e *events.New) {
		e.Title, e.Kind, e.Course, e.Start, e.End, e.Room = "Calc seminar", "class", "Математический анализ",
			"2026-10-19T09:30:00", "", ""
	})
	addEvent(sch, 9, func(e *events.New) {
		e.Title, e.Kind, e.Course, e.Start, e.End, e.Room = "Algebra", "lecture", "", "2026-10-20T13:00:00", "", "R401"
	})
	evs := byKey(t, render(t, feed, "main"), "UID")
	day := evs[events.UID("tg", chat, 7, 0)]
	if day["DTSTART;VALUE=DATE"] != "20261021" || day["DTEND;VALUE=DATE"] != "20261022" {
		t.Fatal(day)
	}
	if _, ok := evs[events.UID("tg", chat, 8, 0)]; ok {
		t.Fatal("a class's twin")
	}
	if _, ok := evs[events.UID("tg", chat, 9, 0)]; !ok {
		t.Fatal("the twin of a cancelled class")
	}
}

func TestOriginAndEditLink(t *testing.T) {
	feed := loaded(t)
	feed.EditLink = func(uid string) string {
		if strings.HasSuffix(uid, "ruz.hse.ru") {
			return "https://x.example.net/web/#/c?uid=c"
		}
		return "https://x.example.net/web/#/c?uid=e"
	}
	evs := byKey(t, render(t, feed, "main"), "UID")
	consult, calc := evs[events.UID("tg", chat, 1, 0)], evs["2000001@ruz.hse.ru"]
	if consult["DESCRIPTION"] != "Consultation\nAlgebra\nFrom: Матан https://t.me/c/1234567890/1\n"+
		"Edit: https://x.example.net/web/#/c?uid=e" {
		t.Fatal(consult["DESCRIPTION"])
	}
	if calc["DESCRIPTION"] != "Seminar\nFrom: RUZ\nEdit: https://x.example.net/web/#/c?uid=c" {
		t.Fatal(calc["DESCRIPTION"])
	}
	if !strings.Contains(evs["2000005@ruz.hse.ru"]["DESCRIPTION"], "From: Матан https://t.me/c/1234567890/5") {
		t.Fatal("moved")
	}
	for _, e := range parse(t, render(t, feed, "tablo")) {
		if _, ok := e["URL"]; ok || strings.Contains(e["DESCRIPTION"], "From:") {
			t.Fatal(e)
		}
	}
	feed.EditLink = func(string) string { return "" }
	if d := byKey(t, render(t, feed, "main"), "UID")["2000001@ruz.hse.ru"]["DESCRIPTION"]; d != "Seminar\nFrom: RUZ" {
		t.Fatal(d)
	}
}

// tabloRead is Tablo's ical.py (service/tablo/sources/ical.py, §4.2) as it reads a feed: by uid, Moscow-local start
// and end, subject, room and kind; cancelled ones skipped.
type tabloEvent struct {
	start, end          time.Time
	subject, room, kind string
}

func tabloLocal(e vevent, name string) (time.Time, bool) {
	if v, ok := e[name]; ok {
		t, err := time.Parse("20060102T150405Z", v)
		if err != nil {
			panic(err)
		}
		return t.In(clock.TZ), true
	}
	if v, ok := e[name+";VALUE=DATE"]; ok {
		t, err := time.ParseInLocation("20060102", v, clock.TZ)
		if err != nil {
			panic(err)
		}
		return t, true
	}
	return time.Time{}, false
}

func tabloRoom(location, description, url string) string {
	location = strings.TrimSpace(location)
	link := func(s string) bool { return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") }
	if location != "" && !link(location) {
		room, _, _ := strings.Cut(location, ",")
		return strings.TrimSpace(room)
	}
	if location != "" || strings.Contains(description, "http://") || strings.Contains(description, "https://") ||
		url != "" {
		return "online"
	}
	return ""
}

func tabloRead(t *testing.T, text []byte) map[string]tabloEvent {
	out := map[string]tabloEvent{}
	for _, e := range parse(t, text) {
		if strings.ToUpper(e["STATUS"]) == "CANCELLED" {
			continue
		}
		start, ok := tabloLocal(e, "DTSTART")
		if !ok || e["SUMMARY"] == "" {
			continue
		}
		end, ok := tabloLocal(e, "DTEND")
		if !ok {
			end = start
		}
		kind, _, _ := strings.Cut(e["DESCRIPTION"], "\n")
		out[e["UID"]] = tabloEvent{start, end, e["SUMMARY"], tabloRoom(e["LOCATION"], e["DESCRIPTION"], e["URL"]),
			strings.TrimSpace(kind)}
	}
	return out
}

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, clock.TZ)
}

func TestTabloReadsTheFeed(t *testing.T) {
	feed := loaded(t)
	read := tabloRead(t, render(t, feed, "tablo"))
	check := func(uid string, want tabloEvent, times bool) {
		t.Helper()
		got, ok := read[uid]
		if !times {
			got.start, got.end = time.Time{}, time.Time{}
		}
		if !ok || !got.start.Equal(want.start) || !got.end.Equal(want.end) || got.subject != want.subject ||
			got.room != want.room || got.kind != want.kind {
			t.Errorf("%s: %+v", uid, got)
		}
	}
	check("2000001@ruz.hse.ru", tabloEvent{at(19, 9, 30), at(19, 10, 50), "Calculus", "D208", "Seminar"}, true)
	check("2000003@ruz.hse.ru", tabloEvent{subject: "Rus Statehood", room: "online", kind: "Lecture"}, false)
	check("2000006@ruz.hse.ru", tabloEvent{subject: "Rus Statehood", kind: "Interim assessment"}, false)
	check("2000005@ruz.hse.ru", tabloEvent{at(23, 11, 10), at(23, 12, 30), "Intro to SE", "R301", "Seminar"}, true)
	if _, ok := read["2000002@ruz.hse.ru"]; ok {
		t.Error("cancelled by a chat")
	}
	if _, ok := read["2000008@ruz.hse.ru"]; ok {
		t.Error("cancelled in RUZ")
	}
	check(events.UID("tg", chat, 1, 0), tabloEvent{at(23, 18, 10), at(23, 19, 30), "Alg consult", "R205",
		"Consultation"}, true)
	check(events.UID("tg", chat, 2, 0), tabloEvent{subject: "Career fair", room: "online", kind: "Event"}, false)
	if len(read) != 12-3 {
		t.Fatal(len(read))
	}

	sch := feed.Schedule.(*schedule.Schedule)
	addEvent(sch, 7, func(e *events.New) { e.Title, e.Start, e.End, e.Room = "Alg consult", "2026-10-21", "", "S301" })
	read = tabloRead(t, render(t, feed, "tablo"))
	if day := read[events.UID("tg", chat, 7, 0)]; !day.start.Equal(at(21, 0, 0)) || !day.end.Equal(at(22, 0, 0)) {
		t.Fatal(day)
	}
}
