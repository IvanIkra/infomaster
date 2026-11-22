package ruz

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func feed() []byte { return must(os.ReadFile("testdata/ruz.ics")) }

// vevent is a VEVENT with Moscow times "2026-10-20 13:00", written in UTC as RUZ does.
func vevent(uid, summary, start, end string, extra ...string) string {
	kind, location, status := "Seminar", "", ""
	if len(extra) > 0 {
		kind = extra[0]
	}
	if len(extra) > 1 {
		location = extra[1]
	}
	if len(extra) > 2 {
		status = extra[2]
	}
	utc := func(text string) string {
		t := must(time.ParseInLocation("2006-01-02 15:04", text, clock.TZ))
		return t.UTC().Format("20060102T150405Z")
	}
	lines := []string{"BEGIN:VEVENT", "UID:" + uid, "DTSTART:" + utc(start), "DTEND:" + utc(end), "SUMMARY:" + summary,
		"DESCRIPTION:" + kind}
	if location != "" {
		lines = append(lines, "LOCATION:"+location)
	}
	if status != "" {
		lines = append(lines, "STATUS:"+status)
	}
	return strings.Join(append(lines, "END:VEVENT"), "\r\n")
}

func calendar(events ...string) []byte {
	lines := append([]string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN"}, events...)
	return []byte(strings.Join(append(lines, "END:VCALENDAR", ""), "\r\n"))
}

type fakeClock struct {
	mu     sync.Mutex
	moment time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.moment }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.moment = c.moment.Add(d)
}

func newClock() *fakeClock {
	return &fakeClock{moment: time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}
}

func openStore(t *testing.T) *store.Store {
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	return s
}

type server struct {
	mu          sync.Mutex
	ifNoneMatch []string
	status      int
	body        []byte
	etag        string
	url         string
}

func (s *server) set(body []byte, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body, s.etag = body, etag
}

func (s *server) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ifNoneMatch[len(s.ifNoneMatch)-1]
}

func newServer(t *testing.T) *server {
	state := &server{status: 200, body: feed(), etag: `W/"v1"`}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/ruz/cal/test", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		state.ifNoneMatch = append(state.ifNoneMatch, r.Header.Get("If-None-Match"))
		if state.status != 200 {
			w.WriteHeader(state.status)
			return
		}
		if r.Header.Get("If-None-Match") == state.etag {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("Content-Type", "text/calendar")
		w.Header().Set("ETag", state.etag)
		w.Write(state.body)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	state.url = ts.URL + "/v3/ruz/cal/test"
	return state
}

type collected struct {
	*Ruz
	polls   []Diff
	notices []string
}

func collector(address string, s *store.Store, c *fakeClock) *collected {
	r := &collected{Ruz: New(address, s)}
	r.Now = c.now
	r.Notify = func(line i18n.Phrase) { r.notices = append(r.notices, line.Ru) }
	r.OnPoll = func(ctx context.Context, diff Diff) error { r.polls = append(r.polls, diff); return nil }
	return r
}

func TestParse(t *testing.T) {
	classes := map[byte]Class{}
	for _, c := range must(Parse(feed())) {
		classes[c.UID[6]] = c
	}
	calc := classes['1']
	if calc.Subject != "Математический анализ" { // a folded "(р ус)" is dropped
		t.Fatal(calc.Subject)
	}
	if !calc.Start.Equal(time.Date(2026, 10, 19, 9, 30, 0, 0, clock.TZ)) ||
		!calc.End.Equal(time.Date(2026, 10, 19, 10, 50, 0, 0, clock.TZ)) {
		t.Fatal(calc.Start, calc.End)
	}
	if calc.Kind != "Проведение семинаров (студенты)" {
		t.Fatal(calc.Kind)
	}
	var rooms []string
	for _, k := range "12345" {
		rooms = append(rooms, classes[byte(k)].Room)
	}
	if !slices.Equal(rooms, []string{"D208", "R401", "online", "online", "109"}) {
		t.Fatal(rooms)
	}
	if *classes['3'].Link != "https://my.mts-link.ru/j/11111111/22222222222" {
		t.Fatal(*classes['3'].Link)
	}
	if *classes['4'].Link != "https://example.ktalk.ru/abcdef" { // from DESCRIPTION, unfolded
		t.Fatal(*classes['4'].Link)
	}
	if classes['6'].Room != "" || classes['6'].Link != nil {
		t.Fatal(classes['6'])
	}
	if !classes['8'].Cancelled || classes['9'].Cancelled { // kept with its mark, not skipped
		t.Fatal("cancelled")
	}
	parsed := must(Parse(feed()))
	if !slices.IsSortedFunc(parsed, func(a, b Class) int { return a.Start.Compare(b.Start) }) {
		t.Fatal("order")
	}
	if parsed[6].UID != "2000008@ruz.hse.ru" || parsed[7].UID != "2000007@ruz.hse.ru" {
		t.Fatal("by start")
	}
}

func TestParseOddEvents(t *testing.T) {
	text := calendar(
		"BEGIN:VEVENT\r\nSUMMARY:Algebra\r\nDTSTART;VALUE=DATE:20261024\r\nEND:VEVENT", // all day, no UID
		"BEGIN:VEVENT\r\nUID:x\r\nSUMMARY: \r\nDTSTART:20261024T100000Z\r\nEND:VEVENT", // no subject
		"BEGIN:VEVENT\r\nUID:y\r\nSUMMARY:Algebra\r\nEND:VEVENT",                       // no start
		"BEGIN:VEVENT\r\nUID:z\r\nSUMMARY:History\r\nDTSTART;TZID=Europe/Moscow:20261024T100000\r\nEND:VEVENT")
	parsed := must(Parse(text))
	if len(parsed) != 2 {
		t.Fatal(parsed)
	}
	day, history := parsed[0], parsed[1]
	midnight := time.Date(2026, 10, 24, 0, 0, 0, 0, clock.TZ)
	if !day.Start.Equal(midnight) || !day.End.Equal(midnight) {
		t.Fatal(day)
	}
	if !strings.HasPrefix(day.UID, "ruz-") || day.UID != must(Parse(text))[0].UID { // stable without a UID
		t.Fatal(day.UID)
	}
	ten := time.Date(2026, 10, 24, 10, 0, 0, 0, clock.TZ)
	if !history.Start.Equal(ten) || !history.End.Equal(ten) {
		t.Fatal(history)
	}
	if _, err := Parse([]byte("not a calendar")); err == nil {
		t.Fatal("not a calendar")
	}
}

func TestFallbackUIDVector(t *testing.T) {
	if FallbackUID("Calculus", time.Date(2026, 11, 23, 9, 30, 0, 0, clock.TZ)) != "ruz-58d7f40dda535008@infomaster" {
		t.Fatal("fallback uid")
	}
}

func TestPollUsesEtag(t *testing.T) {
	srv, s, c := newServer(t), openStore(t), newClock()
	ctx := context.Background()
	r := collector(srv.url, s, c)
	first := must(r.Poll(ctx))
	if !first.First || len(first.Changes) != 0 || must(r.Count()) != 9 {
		t.Fatal(first)
	}
	status := must(r.Status())
	if *status.OkAt != "2026-10-20T12:00:00" || status.Error != nil || status.Classes != 9 {
		t.Fatal(status)
	}
	again := must(r.Poll(ctx))
	if !slices.Equal(srv.ifNoneMatch, []string{"", `W/"v1"`}) || len(again.Changes) != 0 || again.First {
		t.Fatal(srv.ifNoneMatch, again)
	}
	if len(r.polls) != 2 { // also after 304: the subjects get another try
		t.Fatal(r.polls)
	}
	other := collector(srv.url+"?other", s, c) // a new link: the old ETag is not sent
	must(other.Poll(ctx))
	if srv.last() != "" {
		t.Fatal(srv.last())
	}
}

func TestFailureKeepsClassesAndIsTold(t *testing.T) {
	srv, s, c := newServer(t), openStore(t), newClock()
	ctx := context.Background()
	r := collector(srv.url, s, c)
	must(r.Poll(ctx))
	srv.status = 503
	if must(r.Poll(ctx)) != nil {
		t.Fatal("a failed poll")
	}
	if must(r.Count()) != 9 || !strings.Contains(r.LastError(), "503") || len(r.notices) != 0 {
		t.Fatal(r.LastError())
	}
	c.add(12 * time.Hour)
	must(r.Poll(ctx))
	must(r.Poll(ctx))
	if len(r.notices) != 1 ||
		!strings.HasPrefix(r.notices[0], "РУЗ недоступен с 20.10 12:00: HTTP 503 Service Unavailable.") {
		t.Fatal(r.notices)
	}
	srv.status = 200
	must(r.Poll(ctx))
	if r.notices[1] != "РУЗ снова доступен." || r.LastError() != "" {
		t.Fatal(r.notices)
	}
	if len(r.polls) != 2 { // failed polls give the processing nothing
		t.Fatal(r.polls)
	}
}

func TestEmptyFeedIsAGlitch(t *testing.T) {
	srv, s, c := newServer(t), openStore(t), newClock()
	r := collector(srv.url, s, c)
	must(r.Poll(context.Background()))
	srv.set(calendar(), `W/"v2"`)
	if must(r.Poll(context.Background())) != nil {
		t.Fatal("a glitch")
	}
	if must(r.Count()) != 9 || !strings.Contains(r.LastError(), "empty") {
		t.Fatal(r.LastError())
	}
}

func TestNoLink(t *testing.T) {
	s, c := openStore(t), newClock()
	r := collector("", s, c)
	status := must(r.Status())
	if status.OkAt != nil || *status.Error != "RUZ_ICAL_URL is not set" || status.Classes != 0 {
		t.Fatal(status)
	}
	if err := r.Run(context.Background()); err != nil { // returns at once
		t.Fatal(err)
	}
}

func TestRunPollsEveryHour(t *testing.T) {
	srv, s, c := newServer(t), openStore(t), newClock()
	r := collector(srv.url, s, c)
	var sleeps []time.Duration
	stop := errors.New("stop")
	r.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) == 2 {
			return stop
		}
		return nil
	}
	if err := r.Run(context.Background()); err != stop {
		t.Fatal(err)
	}
	if !slices.Equal(sleeps, []time.Duration{time.Hour, time.Hour}) || len(r.polls) != 2 {
		t.Fatal(sleeps, r.polls)
	}
}

func TestDiff(t *testing.T) {
	s, c := openStore(t), newClock()
	r := collector("", s, c) // Tue 2026-10-20 12:00
	base := map[string]string{
		"past":    vevent("past", "Calculus", "2026-10-19 09:30", "2026-10-19 10:50"),
		"moved":   vevent("moved", "Algebra", "2026-10-21 13:00", "2026-10-21 14:20", "Lecture", "R401"),
		"room":    vevent("room", "Discrete Mathematics", "2026-10-22 16:40", "2026-10-22 18:00", "Seminar", "R205"),
		"cancel":  vevent("cancel", "Calculus", "2026-10-23 09:30", "2026-10-23 10:50"),
		"removed": vevent("removed", "History", "2026-10-26 11:10", "2026-10-26 12:30"),
		"last":    vevent("last", "Algebra", "2026-11-03 13:00", "2026-11-03 14:20"),
	}
	all := func(m map[string]string, keys ...string) []byte {
		var events []string
		for _, k := range keys {
			events = append(events, m[k])
		}
		return calendar(events...)
	}
	first := must(r.Sync(must(Parse(all(base, "past", "moved", "room", "cancel", "removed", "last"))), false))
	if !first.First || len(first.Changes) != 0 || must(r.Count()) != 6 {
		t.Fatal(first)
	}
	changed := map[string]string{
		"moved":  vevent("moved", "Algebra", "2026-10-22 11:10", "2026-10-22 12:30", "Lecture", "R401"),
		"room":   vevent("room", "Discrete Mathematics", "2026-10-22 16:40", "2026-10-22 18:00", "Seminar", "R510"),
		"cancel": vevent("cancel", "Calculus", "2026-10-23 09:30", "2026-10-23 10:50", "Seminar", "", "CANCELLED"),
		"last":   base["last"],
		"inside": vevent("inside", "Algebra", "2026-10-28 13:00", "2026-10-28 14:20"),
		"beyond": vevent("beyond", "Algebra", "2026-11-10 13:00", "2026-11-10 14:20"), // the weeks moving on
	}
	diff := must(r.Sync(must(Parse(all(changed, "moved", "room", "cancel", "last", "inside", "beyond"))), false))
	if diff.First {
		t.Fatal("not first")
	}
	kinds := func(d *Diff) []string {
		var out []string
		for _, change := range d.Changes {
			out = append(out, change.Kind+" "+change.New.UID)
		}
		slices.Sort(out)
		return out
	}
	if got := kinds(diff); !slices.Equal(got, []string{"cancelled cancel", "moved moved", "new inside",
		"removed removed", "room room"}) {
		t.Fatal(got)
	}
	for _, change := range diff.Changes {
		if change.Kind == "moved" && (change.Old.Start != "2026-10-21T13:00:00" || change.New.Start != "2026-10-22T11:10:00") {
			t.Fatal(change)
		}
	}
	if must(r.ByUID("past")).GoneAt != nil { // aged out of the feed: kept as it was
		t.Fatal("past")
	}
	if *must(r.ByUID("removed")).GoneAt != "2026-10-20T12:00:00" {
		t.Fatal("removed")
	}
	var uids []string
	for _, row := range must(r.Classes("2026-10-26", "9999")) {
		uids = append(uids, row.UID)
	}
	if !slices.Equal(uids, []string{"inside", "last", "beyond"}) {
		t.Fatal(uids)
	}
	changed["cancel"], changed["removed"] = base["cancel"], base["removed"]
	back := must(r.Sync(must(Parse(all(changed, "moved", "room", "cancel", "last", "inside", "beyond", "removed"))),
		false))
	if got := kinds(back); !slices.Equal(got, []string{"restored cancel", "restored removed"}) {
		t.Fatal(got)
	}
}

func TestStatusCountsTheLastFeed(t *testing.T) {
	srv, s, c := newServer(t), openStore(t), newClock()
	ctx := context.Background()
	r := collector(srv.url, s, c)
	must(r.Poll(ctx))
	if must(r.Status()).Classes != 9 {
		t.Fatal("9")
	}
	c.moment = time.Date(2026, 11, 4, 12, 0, 0, 0, clock.TZ) // every class of the feed is past
	srv.set(calendar(vevent("a", "Algebra", "2026-11-05 13:00", "2026-11-05 14:20"),
		vevent("b", "Calculus", "2026-11-06 09:30", "2026-11-06 10:50")), `W/"v2"`)
	must(r.Poll(ctx))
	if must(r.Status()).Classes != 2 || must(r.Count()) != 11 { // the past ones are kept, not counted
		t.Fatal(must(r.Status()))
	}
	must(r.Poll(ctx)) // 304
	if srv.last() != `W/"v2"` || must(r.Status()).Classes != 2 {
		t.Fatal(srv.last())
	}
}

// captureLog sends slog's output to a buffer for the test: the log must never tell the link.
func captureLog(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestErrorsNeverTellTheLink(t *testing.T) {
	logged := captureLog(t)
	for _, status := range []int{404, 503} {
		secret := "/v3/ruz/cal/0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		s, c := openStore(t), newClock()
		r := collector(ts.URL+secret, s, c)
		must(r.Poll(context.Background()))
		c.add(12 * time.Hour)
		must(r.Poll(context.Background()))
		if r.LastError() != fmt.Sprintf("HTTP %d %s", status, http.StatusText(status)) {
			t.Fatal(r.LastError())
		}
		if len(r.notices) != 1 || strings.Contains(r.notices[0], secret) || strings.Contains(r.LastError(), "0f1e2d3c") {
			t.Fatal(r.notices)
		}
		if strings.Contains(logged.String(), "0f1e2d3c") || !strings.Contains(logged.String(), fmt.Sprint(status)) {
			t.Fatal(logged.String())
		}
		ts.Close()
	}
}

func TestOtherErrorsHideTheLink(t *testing.T) {
	logged := captureLog(t)
	s, c := openStore(t), newClock()
	r := collector("ftp://example.invalid/v3/ruz/cal/0f1e2d3c-secret", s, c) // refused before connecting
	if must(r.Poll(context.Background())) != nil {
		t.Fatal("a failed poll")
	}
	if !strings.Contains(r.LastError(), HiddenURL) || strings.Contains(r.LastError(), "0f1e2d3c") {
		t.Fatal(r.LastError())
	}
	if strings.Contains(logged.String(), "0f1e2d3c") || !strings.Contains(logged.String(), HiddenURL) {
		t.Fatal(logged.String())
	}
	if got := r.hideURL("GET /v3/ruz/cal/0f1e2d3c-secret failed"); got != "GET <RUZ_ICAL_URL> failed" {
		t.Fatal(got)
	}
}

func TestEmptyFeedBeforeTheTimetable(t *testing.T) {
	// Between terms the feed is empty: no 'first poll' every hour, only once classes come.
	srv, s, c := newServer(t), openStore(t), newClock()
	ctx := context.Background()
	r := collector(srv.url, s, c)
	srv.set(calendar(), `W/"v1"`)
	for range 3 {
		must(r.Poll(ctx))
	}
	for _, diff := range r.polls {
		if diff.First {
			t.Fatal("first")
		}
	}
	status := must(r.Status())
	if *status.OkAt != "2026-10-20T12:00:00" || status.Error != nil || status.Classes != 0 {
		t.Fatal(status)
	}
	srv.set(feed(), `W/"v2"`)
	must(r.Poll(ctx))
	must(r.Poll(ctx))
	if !r.polls[3].First || r.polls[4].First {
		t.Fatal(r.polls[3:])
	}
}

func TestANewLinkIsAFirstPoll(t *testing.T) {
	// RUZ_ICAL_URL changed: another feed, not a list of changes. No «убрана» for every class of the old one.
	srv, s, c := newServer(t), openStore(t), newClock()
	ctx := context.Background()
	r := collector(srv.url, s, c)
	must(r.Poll(ctx))
	srv.set(calendar(vevent("other-1@ruz.hse.ru", "Calculus", "2026-10-21 09:30", "2026-10-21 10:50"),
		vevent("2000008@ruz.hse.ru", "Calculus", "2026-10-28 09:30", "2026-10-28 10:50")), `W/"other"`)
	relinked := collector(srv.url+"?new-link", s, c)
	diff := must(relinked.Poll(ctx))
	if !diff.First || !diff.Relinked || srv.last() != "" {
		t.Fatal(diff)
	}
	var uids []string
	for _, row := range must(relinked.Classes("2026-10-20T12:00:00", "9999")) {
		uids = append(uids, row.UID)
	}
	if !slices.Equal(uids, []string{"other-1@ruz.hse.ru", "2000008@ruz.hse.ru"}) { // the old feed's are gone
		t.Fatal(uids)
	}
	again := must(relinked.Poll(ctx)) // the link is the saved one now
	if again.First || again.Relinked || srv.last() != `W/"other"` {
		t.Fatal(again)
	}
	back := collector(srv.url, s, c) // and the first link again: one more first poll
	srv.set(srv.body, `W/"other2"`)
	if !must(back.Poll(ctx)).First {
		t.Fatal("first again")
	}
}

func TestANewLinkWaitsForClasses(t *testing.T) {
	// An empty feed behind the new link is no first poll: the next one with classes is.
	srv, s, c := newServer(t), openStore(t), newClock()
	ctx := context.Background()
	r := collector(srv.url, s, c)
	must(r.Poll(ctx))
	c.add(60 * 24 * time.Hour) // nothing of the old feed lies ahead
	srv.set(calendar(), `W/"empty"`)
	relinked := collector(srv.url+"?new-link", s, c)
	if must(relinked.Poll(ctx)).First {
		t.Fatal("first")
	}
}
