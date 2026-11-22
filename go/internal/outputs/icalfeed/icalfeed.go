// Package icalfeed serves our iCal feeds (§6.5, §8.1, §8.2, D2, D28, D32, D43): the port of outputs/ical_feed.py.
//
// On the internal port, for Tablo: GET /calendar/<CALENDAR_SECRET>/main.ics, optional.ics and tablo.ics. On the public
// port, behind Funnel, for one's own calendar: GET /calendar/<CALENDAR_PUBLIC_SECRET>/main.ics and optional.ics, never
// tablo.ics. Calendars send no headers, so the secret in the path is all the protection: it is compared in constant
// time, and anything else is 404. The two secrets are different, so a leaked public link is changed without Tablo; a
// port without its secret has no routes.
//
//   - main: the classes and the events one has to attend;
//   - optional: the events one may attend;
//   - tablo: both; an optional one is STATUS:TENTATIVE with X-INFOMASTER-OPTIONAL:TRUE. Tablo's ical.py skips only
//     CANCELLED, so it reads the feed as it is now and ignores the X- fields until it learns them.
//
// Every event has an English SUMMARY, a DESCRIPTION whose first line is the type (Lecture, Seminar, Consultation,
// Event, Interim assessment...), LOCATION with the room or the link, a stable UID, X-INFOMASTER-SOURCE (ruz, tg, mail,
// pdf) and, when there is a source message, X-INFOMASTER-LINK. Never URL: Tablo takes an event with URL for an online
// class (§4.2). Times are in UTC, which every calendar converts by itself; an event told without its time is an all-day
// one, a DTSTART date and the next day's DTEND. A cancelled event stays with STATUS:CANCELLED, so a calendar that has
// it shows the cancellation.
//
// In main and optional, a class's SUMMARY ends with its type, "Calculus · Seminar", since a calendar app shows the
// title alone. tablo.ics keeps the bare title: Tablo takes SUMMARY for the course name.
//
// main and optional also say where an event came from and how to change it, since a calendar app shows the notes and
// one link: DESCRIPTION ends with "From: <the chat> <link to the message>" (a letter's subject, RUZ, "you" for an own
// one) and "Edit: <link>", which opens the event's card in the web app (§8.6, D69), where the source message is quoted
// too; no URL, which iOS shows under Location as a web page card above the room. tablo.ics has neither.
//
// Python wrote the text with icalendar 7.3; this package writes the same bytes by hand (CONTRACT.md §4.4): CRLF line
// ends, lines folded at 75 octets, TEXT escaping, the calendar's and the events' property order.
package icalfeed

import (
	"crypto/subtle"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
)

// Feeds are every feed; PublicFeeds the ones served behind Funnel (tablo.ics with its X- fields is Tablo's, it stays
// inside).
var (
	Feeds       = []string{"main", "optional", "tablo"}
	PublicFeeds = []string{"main", "optional"}
)

// Names are the calendars' names.
var Names = map[string]string{"main": "Study", "optional": "Study: optional", "tablo": "Study for Tablo"}

// Refresh: RUZ is polled hourly; a calendar that honours the hint asks as often.
const Refresh = "PT1H"

// Marks are types written shorter in a class's title, where the word is too long.
var Marks = map[string]string{"Interim assessment": "Midterm"}

// utc reads a Moscow wall time (Entry.Start, End) as UTC.
func utc(local time.Time) time.Time {
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(),
		local.Nanosecond(), clock.TZ).UTC()
}

// Summary is the title, with a class's type in one's own calendar ("Calculus · Seminar"); a type not known stays out.
func Summary(entry schedule.Entry, feed string) string {
	if feed == "tablo" || !entry.OfClass || entry.Type == "Class" {
		return entry.Title
	}
	mark, ok := Marks[entry.Type]
	if !ok {
		mark = entry.Type
	}
	return entry.Title + " · " + mark
}

// OriginLine is where it came from, for one's own calendar: the chat with a link to the message, RUZ, the owner.
func OriginLine(entry schedule.Entry) string {
	switch entry.Source {
	case "ruz":
		return "From: RUZ"
	case "own":
		return "From: you"
	}
	origin := entry.Origin
	if origin == "" {
		origin = entry.Source
	}
	line := "From: " + origin
	if entry.Link != "" {
		line += " " + entry.Link
	}
	return line
}

// escape is icalendar's TEXT escaping; the order matters.
func escape(text string) string {
	return strings.NewReplacer("\r\n", `\n`, "\n", `\n`, "\r", `\n`).Replace(
		strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(text, `\N`, "\n"),
			`\`, `\\`), ";", `\;`), ",", `\,`))
}

// fold is icalendar's _foldline: segments of at most 74 octets joined by CRLF and a space; a `\` or `^` that would end
// a segment moves to the next one, so an escape is never split.
func fold(line string) string {
	const limit = 75
	var lines []string
	var current []rune
	count := 0
	for _, char := range line {
		size := utf8.RuneLen(char)
		if len(current) > 0 && count+size >= limit {
			if last := current[len(current)-1]; len(current) > 1 && (last == '\\' || last == '^') {
				lines = append(lines, string(current[:len(current)-1]))
				current = []rune{last}
				count = 1
			} else {
				lines = append(lines, string(current))
				current, count = nil, 0
			}
		}
		current = append(current, char)
		count += size
	}
	if len(current) > 0 {
		lines = append(lines, string(current))
	}
	return strings.Join(lines, "\r\n ")
}

type writer struct{ b strings.Builder }

func (w *writer) line(text string) {
	w.b.WriteString(fold(text))
	w.b.WriteString("\r\n")
}

// Build is one of Feeds as iCalendar text; `editLink(uid)` is the link to an entry's card in the web app ("" for
// none; nil for no links at all).
func Build(entries []schedule.Entry, feed string, stamp time.Time, editLink func(uid string) string) []byte {
	w := &writer{}
	w.line("BEGIN:VCALENDAR")
	w.line("VERSION:2.0")
	w.line("PRODID:-//infomaster//timetable//EN")
	w.line("CALSCALE:GREGORIAN")
	w.line("METHOD:PUBLISH")
	w.line("X-WR-CALNAME:" + escape(Names[feed]))
	w.line("REFRESH-INTERVAL;VALUE=DURATION:" + Refresh)
	w.line("X-PUBLISHED-TTL:" + Refresh)
	dtstamp := stamp.UTC().Format("20060102T150405Z")
	for _, entry := range entries {
		if feed == "main" && !entry.Required || feed == "optional" && entry.Required {
			continue
		}
		optional := feed == "tablo" && !entry.Required
		w.line("BEGIN:VEVENT")
		w.line("SUMMARY:" + escape(Summary(entry, feed)))
		if entry.AllDay {
			day := time.Date(entry.Start.Year(), entry.Start.Month(), entry.Start.Day(), 0, 0, 0, 0, time.UTC)
			w.line("DTSTART;VALUE=DATE:" + day.Format("20060102"))
			w.line("DTEND;VALUE=DATE:" + day.AddDate(0, 0, 1).Format("20060102"))
		} else {
			start, end := utc(entry.Start), utc(entry.End)
			w.line("DTSTART:" + start.Format("20060102T150405Z"))
			if end.After(start) { // DTEND must be later than DTSTART; without it the event ends where it starts
				w.line("DTEND:" + end.Format("20060102T150405Z"))
			}
		}
		w.line("DTSTAMP:" + dtstamp)
		w.line("UID:" + escape(entry.UID))
		lines := append([]string{entry.Type}, entry.Notes...)
		link := ""
		if editLink != nil && feed != "tablo" {
			link = editLink(entry.UID)
		}
		if feed != "tablo" {
			lines = append(lines, OriginLine(entry))
		}
		if link != "" {
			lines = append(lines, "Edit: "+link)
		}
		w.line("DESCRIPTION:" + escape(strings.Join(lines, "\n")))
		if entry.Location != "" {
			w.line("LOCATION:" + escape(entry.Location))
		}
		status := "CONFIRMED"
		if entry.Cancelled {
			status = "CANCELLED"
		} else if optional {
			status = "TENTATIVE"
		}
		w.line("STATUS:" + status)
		// icalendar writes unknown X- properties as they are, without TEXT escaping.
		if entry.Link != "" {
			w.line("X-INFOMASTER-LINK:" + entry.Link)
		}
		if optional {
			w.line("X-INFOMASTER-OPTIONAL:TRUE")
		}
		w.line("X-INFOMASTER-SOURCE:" + entry.Source)
		w.line("END:VEVENT")
	}
	w.line("END:VCALENDAR")
	return []byte(w.b.String())
}

// Entries is what the feed reads: *schedule.Schedule.
type Entries interface {
	Entries() ([]schedule.Entry, error)
}

// CalendarFeed renders the feeds of the timetable.
type CalendarFeed struct {
	Schedule Entries
	Now      func() time.Time
	// EditLink is uid -> its card in the web app; nil or "" for none.
	EditLink func(uid string) string
}

// New is the feed of a timetable.
func New(s Entries) *CalendarFeed { return &CalendarFeed{Schedule: s, Now: clock.NowUTC} }

// Render is one of Feeds.
func (c *CalendarFeed) Render(feed string) ([]byte, error) {
	entries, err := c.Schedule.Entries()
	if err != nil {
		return nil, err
	}
	return Build(entries, feed, c.Now(), c.EditLink), nil
}

// notFound is aiohttp's 404.
func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("404: Not Found"))
}

// Handler serves GET /calendar/{secret}/{feed}.ics for the given feeds; the route must name the path values `secret`
// and `file`.
func (c *CalendarFeed) Handler(secret string, feeds []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		given := r.PathValue("secret")
		feed, ok := strings.CutSuffix(r.PathValue("file"), ".ics")
		if subtle.ConstantTimeCompare([]byte(given), []byte(secret)) != 1 || !ok || !slices.Contains(feeds, feed) {
			notFound(w)
			return
		}
		body, err := c.Render(feed)
		if err != nil {
			http.Error(w, "500: Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(body)
	}
}

// Routes adds the feeds to a mux: Feeds on the internal port, PublicFeeds on the public one (nil feeds is Feeds);
// nothing without a secret.
func Routes(mux *http.ServeMux, secret string, c *CalendarFeed, feeds []string) {
	if secret == "" {
		return
	}
	if feeds == nil {
		feeds = Feeds
	}
	mux.HandleFunc("GET /calendar/{secret}/{file}", c.Handler(secret, feeds))
}
