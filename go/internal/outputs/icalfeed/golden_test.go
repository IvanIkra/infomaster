package icalfeed

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

func local(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, clock.TZ)
}

// goldenEntries are the entries testdata/*.ics were written from by icalendar 7.3 (outputs/ical_feed.build).
func goldenEntries() []schedule.Entry {
	return []schedule.Entry{
		{UID: "2000001@ruz.hse.ru", Title: "Calculus", Start: local(2026, 10, 19, 9, 30), End: local(2026, 10, 19, 10, 50),
			Type: "Seminar", Location: "D208", Required: true, Source: "ruz", OfClass: true},
		{UID: "tg-ed3de03ad6dbd3b3@infomaster", Title: "Alg consult; with, commas\\ and a very long title that goes " +
			"past seventy-five octets for sure — да, по-русски тоже", Start: local(2026, 10, 23, 18, 10),
			End: local(2026, 10, 23, 18, 10), Type: "Consultation", Location: "R205, корпус", Required: true,
			Source: "tg", Link: "https://t.me/c/1234567890/1", Notes: []string{"Алгебра", "line; two"}, Origin: "Матан"},
		{UID: "tg-x,y;z@infomaster", Title: "Career fair", Start: local(2026, 10, 24, 12, 0),
			End: local(2026, 10, 24, 16, 0), Type: "Event", Location: "https://events.example.org/fair", Source: "tg",
			Link: "https://t.me/c/1/2;x,y"},
		{UID: "tg-y@infomaster", Title: "Day off", Start: local(2026, 10, 21, 0, 0), End: local(2026, 10, 22, 0, 0),
			Type: "Event", Required: true, Source: "mail", AllDay: true},
		{UID: "2000006@ruz.hse.ru", Title: "Rus Statehood", Start: local(2026, 10, 26, 13, 0),
			End: local(2026, 10, 26, 14, 20), Type: "Interim assessment", Required: true, Cancelled: true, Source: "ruz",
			OfClass: true},
		{UID: "own-1@infomaster", Title: "Own", Start: local(2026, 10, 27, 13, 0), End: local(2026, 10, 27, 14, 20),
			Type: "Class", Source: "own", OfClass: true},
		{UID: "tg-fold@infomaster", Title: strings.Repeat("x", 72) + "\\abc" + strings.Repeat("y", 69) + "^z" +
			strings.Repeat("Ж", 40) + "\\Nliteral", Start: local(2026, 10, 28, 13, 0), End: local(2026, 10, 28, 12, 0),
			Type: "Talk", Cancelled: true, Source: "pdf", Notes: []string{"a\r\nb\nc\rd", strings.Repeat("e", 73) + "\\"},
			Origin: "Letter: subj"},
		{UID: "mail-1@infomaster", Title: "Mail thing", Start: local(2026, 10, 29, 13, 0),
			End: local(2026, 10, 29, 15, 0), Type: "Event", Required: true, Source: "mail",
			Link: "https://mail.example/1", Origin: "Subject, here"},
	}
}

func goldenLink(uid string) string {
	if strings.HasPrefix(uid, "mail") {
		return ""
	}
	return "https://x.example.net/web/#/c?uid=" + pytext.Quote(uid, "")
}

func TestBuildMatchesIcalendar(t *testing.T) {
	stamp := time.Date(2026, 10, 20, 9, 0, 5, 123456000, time.UTC)
	for _, c := range []struct {
		file, feed string
		link       func(string) string
	}{{"main.ics", "main", goldenLink}, {"optional.ics", "optional", goldenLink}, {"tablo.ics", "tablo", goldenLink},
		{"nolink_main.ics", "main", nil}} {
		want, err := os.ReadFile("testdata/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		if got := Build(goldenEntries(), c.feed, stamp, c.link); string(got) != string(want) {
			t.Errorf("%s:\n%s\nwant:\n%s", c.file, got, want)
		}
	}
}

func TestFold(t *testing.T) {
	line := strings.Repeat("a", 73) + `\n` + "b"
	if got := fold(line); got != strings.Repeat("a", 73)+"\r\n "+`\nb` {
		t.Errorf("%q", got)
	}
}

type fixed []schedule.Entry

func (f fixed) Entries() ([]schedule.Entry, error) { return f, nil }

const secret = "Zx8-calendar_secret-42"

func TestRoutes(t *testing.T) {
	feed := New(fixed(goldenEntries()))
	feed.Now = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	Routes(mux, secret, feed, nil)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	w := get("/calendar/" + secret + "/tablo.ics")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/calendar; charset=utf-8" ||
		w.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(w.Body.String(), "BEGIN:VCALENDAR") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	for _, path := range []string{"/calendar/" + secret + "x/main.ics", "/calendar/wrong/main.ics",
		"/calendar/" + secret + "/all.ics", "/calendar/" + secret + "/main.txt",
		"/calendar/" + secret[:len(secret)-1] + "/main.ics"} {
		if w := get(path); w.Code != 404 || w.Body.String() != "404: Not Found" {
			t.Errorf("%s: %d %q", path, w.Code, w.Body.String())
		}
	}

	public := http.NewServeMux()
	Routes(public, "Pq3-public_calendar-17", feed, PublicFeeds)
	for name, code := range map[string]int{"main": 200, "optional": 200, "tablo": 404} {
		w := httptest.NewRecorder()
		public.ServeHTTP(w, httptest.NewRequest("GET", "/calendar/Pq3-public_calendar-17/"+name+".ics", nil))
		if w.Code != code {
			t.Errorf("%s: %d", name, w.Code)
		}
	}

	off := http.NewServeMux()
	Routes(off, "", feed, PublicFeeds)
	w = httptest.NewRecorder()
	off.ServeHTTP(w, httptest.NewRequest("GET", "/calendar/x/main.ics", nil))
	if w.Code != 404 {
		t.Errorf("no secret: %d", w.Code)
	}
}
