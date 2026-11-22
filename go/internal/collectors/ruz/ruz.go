// Package ruz reads the RUZ timetable in iCal (§5.2, §4.1, §4.2): classes of about five weeks, polled every hour.
//
// The link (`RUZ_ICAL_URL`) is a secret and optional: without it the source says so and everything else works. It
// never goes into the status, the log or the notices: HTTP errors are told by their status, other errors with the link
// cut out. A poll sends the saved ETag and takes 304 for "nothing new"; a failed poll keeps the classes it had.
//
// Each VEVENT is read the way Tablo reads it (§4.2): the cleaned SUMMARY is the subject, start and end are Moscow local
// time, the kind is the first line of DESCRIPTION, the room is the part of LOCATION before the first comma or
// "online". A class cancelled in the feed (STATUS:CANCELLED) is kept with a mark rather than skipped, so our feed can
// show the cancellation (§11.2).
//
// `classes` holds the current feed and the past: a class that leaves the feed after it started has aged out of the
// five weeks and stays as it was; one that leaves before it started was removed by RUZ (`gone_at`). The status counts
// the classes of the last feed that came, not the past ones kept. After each poll, also after 304, OnPoll gets what
// changed; the processing names the subjects and keeps the Midterm deadlines (the collector itself never calls the
// LLM, §4). The first poll that brings classes is no news ("first"); an empty feed before it is just nothing yet. A new
// link (its hash differs from the one of the last good poll) makes the next poll that brings classes a first one
// again: another feed is no list of changes, and its classes the old feed had not would all come as removed and new
// ones.
package ruz

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// PollEvery is the time between polls (§4.1).
const PollEvery = time.Hour

// Timeout is the time a poll may take.
const Timeout = 60 * time.Second

// StaleAfter: failing longer than this is told to the owner (§4.5).
const StaleAfter = 12 * time.Hour

// EtagKey and SizeKey are the kv keys; SizeKey holds the classes in the last feed that came (a 304 leaves it).
const (
	EtagKey = "ruz.etag"
	SizeKey = "ruz.size"
)

// HiddenURL stands for the link in error texts.
const HiddenURL = "<RUZ_ICAL_URL>"

var link = regexp.MustCompile(`https?://\S+`)

// Class is one VEVENT of the feed.
type Class struct {
	UID       string
	Subject   string    // cleaned, as the feed names it
	Start     time.Time // Moscow local
	End       time.Time
	Kind      string // "Лекция", "Seminar", "Ongoing interim assessment"
	Room      string // "R609", "online" or ""
	Link      *string
	Cancelled bool
}

// Row is a row of `classes`.
type Row struct {
	ID         int64   `json:"id"`
	UID        string  `json:"uid"`
	Subject    string  `json:"subject"`
	Start      string  `json:"start"`
	End        string  `json:"end"`
	Kind       string  `json:"kind"`
	Room       string  `json:"room"`
	Link       *string `json:"link"`
	Cancelled  bool    `json:"cancelled"`
	DeadlineID *int64  `json:"deadline_id"`
	SeenAt     string  `json:"seen_at"`
	GoneAt     *string `json:"gone_at"`
}

// Room is LOCATION's part before the first comma ('R609, Покровский б-р, д.11' -> 'R609'); a meeting link ->
// 'online' (Tablo's rule, §4.2).
func Room(location, description, address string) string {
	location = strings.TrimSpace(location)
	if location != "" && !matchAtStart(location) {
		before, _, _ := strings.Cut(location, ",")
		return strings.TrimSpace(before)
	}
	if location != "" || link.MatchString(description) || address != "" {
		return "online"
	}
	return ""
}

func matchAtStart(text string) bool {
	loc := link.FindStringIndex(text)
	return loc != nil && loc[0] == 0
}

// MeetingLink is the first link in LOCATION, DESCRIPTION or URL.
func MeetingLink(location, description, address string) *string {
	for _, text := range []string{strings.TrimSpace(location), description, strings.TrimSpace(address)} {
		if found := link.FindString(text); found != "" {
			return &found
		}
	}
	return nil
}

// FallbackUID is the uid of a class the feed gives none: a hash of its subject and start.
func FallbackUID(subject string, start time.Time) string {
	sum := sha1.Sum([]byte(subject + "|" + isoformat(start)))
	return "ruz-" + hex.EncodeToString(sum[:])[:16] + "@infomaster"
}

// isoformat is Python's naive datetime.isoformat(): microseconds only when there are some.
func isoformat(t time.Time) string {
	t = t.In(clock.TZ)
	if t.Nanosecond()/1000 != 0 {
		return t.Format("2006-01-02T15:04:05.000000")
	}
	return t.Format(clock.LocalLayout)
}

// Parse reads the classes of a feed, by start and subject; an error on a broken feed.
func Parse(data []byte) ([]Class, error) {
	calendar, err := readCalendar(data)
	if err != nil {
		return nil, err
	}
	var classes []Class
	var failed error
	calendar.walk("VEVENT", func(event *component) {
		if failed != nil {
			return
		}
		subject := subjects.Clean(event.text("SUMMARY"))
		startProp, ok := event.get("DTSTART")
		if subject == "" || !ok {
			return
		}
		start, err := moment(startProp)
		if err != nil {
			failed = err
			return
		}
		end := start
		if endProp, ok := event.get("DTEND"); ok {
			if end, err = moment(endProp); err != nil {
				failed = err
				return
			}
		}
		description := event.text("DESCRIPTION")
		location, address := event.text("LOCATION"), event.text("URL")
		uid := strings.TrimSpace(event.text("UID"))
		if uid == "" {
			uid = FallbackUID(subject, start)
		}
		kind, _, _ := strings.Cut(description, "\n")
		classes = append(classes, Class{UID: uid, Subject: subject, Start: start, End: end,
			Kind: strings.TrimSpace(kind), Room: Room(location, description, address),
			Link: MeetingLink(location, description, address), Cancelled: strings.ToUpper(event.text("STATUS")) == "CANCELLED"})
	})
	if failed != nil {
		return nil, failed
	}
	slices.SortStableFunc(classes, func(a, b Class) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return strings.Compare(a.Subject, b.Subject)
	})
	return classes, nil
}

// Change is one change of the feed: new (inside the weeks the feed covered before), moved, room, cancelled, removed,
// restored.
type Change struct {
	Kind string
	Old  *Row // nil for a new class
	New  *Row
}

// Diff is what a poll changed.
type Diff struct {
	First    bool // the very first poll, or the first after the link changed: nothing to compare with
	Changes  []Change
	Relinked bool // first because RUZ_ICAL_URL changed
}

// Status is the source's status.
type Status struct {
	OkAt    *string `json:"ok_at"`
	Error   *string `json:"error"`
	Classes int     `json:"classes"`
}

// Ruz is the collector.
type Ruz struct {
	URL   string
	store *store.Store
	db    *sql.DB
	Now   func() time.Time
	// OnPoll runs after every good poll, also a 304.
	OnPoll func(ctx context.Context, diff Diff) error
	// Notify gets a line for the owner (Web Push).
	Notify func(i18n.Phrase)
	// Client fetches the feed; Sleep waits between polls (tests replace them).
	Client *http.Client
	Sleep  func(ctx context.Context, d time.Duration) error

	mu           sync.Mutex
	okAt         *string
	err          *string
	failingSince time.Time
	told         bool
}

// New makes the collector; an empty url turns the source off.
func New(address string, s *store.Store) *Ruz {
	r := &Ruz{URL: address, store: s, db: s.DB, Now: clock.NowUTC, Client: &http.Client{Timeout: Timeout},
		Sleep: sleep}
	if address == "" {
		message := "RUZ_ICAL_URL is not set"
		r.err = &message
	}
	return r
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Status is the status: when the last good poll was, why the last one failed, the classes of the last feed.
func (r *Ruz) Status() (Status, error) {
	size, err := r.Size()
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{OkAt: r.okAt, Error: r.err, Classes: size}, err
}

// LastError is why the last poll failed; "" after a good one.
func (r *Ruz) LastError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		return ""
	}
	return *r.err
}

// --- reading ----------------------------------------------------------------------

const columns = "id, uid, subject, start, end, kind, room, link, cancelled, deadline_id, seen_at, gone_at"

// Querier is what *sql.DB and *sql.Tx both do.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func scanRow(scan func(...any) error) (*Row, error) {
	var row Row
	err := scan(&row.ID, &row.UID, &row.Subject, &row.Start, &row.End, &row.Kind, &row.Room, &row.Link, &row.Cancelled,
		&row.DeadlineID, &row.SeenAt, &row.GoneAt)
	return &row, err
}

// Rows runs a select of the classes' columns ("select <columns> from classes ...").
func Rows(q Querier, where string, args ...any) ([]*Row, error) {
	rows, err := q.Query("select "+columns+" from classes "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Row{}
	for rows.Next() {
		row, err := scanRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func byUID(q Querier, uid string) (*Row, error) {
	row, err := scanRow(q.QueryRow("select "+columns+" from classes where uid = ?", uid).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

// ByUID is one class; nil when there is none.
func (r *Ruz) ByUID(uid string) (*Row, error) { return byUID(r.db, uid) }

// Classes are the classes not removed by RUZ that start in [since, until), local ISO bounds ("" and "9999" for
// none); by start.
func (r *Ruz) Classes(since, until string) ([]*Row, error) {
	return Rows(r.db, "where gone_at is null and start >= ? and start < ? order by start, subject", since, until)
}

// Count is every class kept, the past ones too: with none, the saved ETag is not sent.
func (r *Ruz) Count() (int, error) {
	var n int
	err := r.db.QueryRow("select count(*) n from classes where gone_at is null").Scan(&n)
	return n, err
}

// Size is the classes in the last feed that came: the status and the notices. The past ones kept are not counted, so
// the number does not grow over a semester; a legitimately empty feed counts 0.
func (r *Ruz) Size() (int, error) {
	size := 0
	_, err := r.store.GetJSON(SizeKey, &size)
	return size, err
}

// --- polling ----------------------------------------------------------------------

// Run polls every hour until ctx is done; without a link it returns at once.
func (r *Ruz) Run(ctx context.Context) error {
	if r.URL == "" {
		slog.Info("RUZ_ICAL_URL is not set: no classes from RUZ")
		return nil
	}
	for {
		if _, err := r.Poll(ctx); err != nil {
			slog.Error("RUZ poll failed", "error", err)
		}
		if err := r.Sleep(ctx, PollEvery); err != nil {
			return err
		}
	}
}

type etag struct {
	URL  string  `json:"url"`
	Etag *string `json:"etag"`
}

// Poll fetches the feed once: the diff, or nil when the fetch failed (the classes stay as they were). An error is a
// failure of the database, not of the source.
func (r *Ruz) Poll(ctx context.Context) (*Diff, error) {
	var saved etag
	if _, err := r.store.GetJSON(EtagKey, &saved); err != nil {
		return nil, err
	}
	relinked := saved.URL != "" && saved.URL != digest(r.URL)
	count, err := r.Count()
	if err != nil {
		return nil, err
	}
	var tag *string
	if !relinked && count > 0 {
		tag = saved.Etag
	}
	body, notModified, tag, failure := r.fetch(ctx, tag)
	var classes []Class
	if failure == "" && !notModified {
		var err error
		if classes, err = Parse(body); err != nil {
			failure = r.hideURL("ValueError: " + err.Error())
		} else if len(classes) == 0 {
			ahead, err := r.Classes(clock.LocalISO(r.Now()), "9999")
			if err != nil {
				return nil, err
			}
			if len(ahead) > 0 { // a glitch, not a holiday
				failure = "ValueError: the feed came empty while classes lie ahead"
			}
		}
	}
	if failure != "" {
		r.failed(failure)
		return nil, nil
	}
	diff := &Diff{}
	if !notModified {
		if diff, err = r.Sync(classes, relinked); err != nil {
			return nil, err
		}
	}
	if err := r.store.PutJSON(EtagKey, etag{URL: digest(r.URL), Etag: tag}); err != nil {
		return nil, err
	}
	r.recovered()
	if !notModified {
		slog.Info(fmt.Sprintf("RUZ: %d classes in the feed, %d changes", len(classes), len(diff.Changes)))
	}
	if r.OnPoll != nil {
		if err := r.OnPoll(ctx, *diff); err != nil {
			slog.Error("processing the RUZ poll failed", "error", err)
		}
	}
	return diff, nil
}

// fetch gets the feed: its body, or notModified for a 304; the ETag to save; a failure's text (the link hidden).
func (r *Ruz) fetch(ctx context.Context, tag *string) (body []byte, notModified bool, newTag *string, failure string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, false, tag, r.hideURL(errorText(err))
	}
	if tag != nil && *tag != "" {
		request.Header.Set("If-None-Match", *tag)
	}
	response, err := r.Client.Do(request)
	if err != nil {
		return nil, false, tag, r.hideURL(errorText(err))
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return nil, true, tag, ""
	}
	if response.StatusCode >= 400 { // its text would carry the request URL: the status says enough
		return nil, false, tag, strings.TrimSpace(fmt.Sprintf("HTTP %d %s", response.StatusCode,
			http.StatusText(response.StatusCode)))
	}
	body, err = io.ReadAll(response.Body)
	if err != nil {
		return nil, false, tag, r.hideURL(errorText(err))
	}
	newTag = nil
	if value := response.Header.Get("ETag"); value != "" {
		newTag = &value
	}
	return body, false, newTag, ""
}

func errorText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return "TimeoutError: " + err.Error()
		}
	}
	return err.Error()
}

// hideURL cuts the link, and its path where the secret is, out of an error's text.
func (r *Ruz) hideURL(text string) string {
	path := ""
	if parsed, err := url.Parse(r.URL); err == nil {
		path = parsed.EscapedPath()
	}
	for _, secret := range []string{r.URL, path} {
		if secret != "" && secret != "/" {
			text = strings.ReplaceAll(text, secret, HiddenURL)
		}
	}
	return text
}

func (r *Ruz) failed(message string) {
	slog.Warn("RUZ poll failed: " + message)
	now := r.Now()
	r.mu.Lock()
	r.err = &message
	if r.failingSince.IsZero() {
		r.failingSince = now
	}
	tell := !r.told && now.Sub(r.failingSince) >= StaleAfter
	if tell {
		r.told = true
	}
	since := r.failingSince.In(clock.TZ).Format("02.01 15:04")
	r.mu.Unlock()
	if tell && r.Notify != nil {
		r.Notify(i18n.AlertP(
			fmt.Sprintf("RUZ is unavailable since %s: %s.\nThe feed and Tablo have the last timetable received.", since,
				message),
			fmt.Sprintf("РУЗ недоступен с %s: %s.\nВ ленте и в Tablo — последнее полученное расписание.", since, message)))
	}
}

func (r *Ruz) recovered() {
	r.mu.Lock()
	told := r.told
	okAt := clock.LocalISO(r.Now())
	r.okAt, r.err = &okAt, nil
	r.failingSince, r.told = time.Time{}, false
	r.mu.Unlock()
	if told && r.Notify != nil {
		r.Notify(i18n.P("RUZ is available again.", "РУЗ снова доступен."))
	}
}

// Sync brings `classes` to the feed and tells what changed; `relinked`: the link is not the one of the last poll.
func (r *Ruz) Sync(classes []Class, relinked bool) (*Diff, error) {
	now := clock.LocalISO(r.Now())
	storedRows, err := Rows(r.db, "")
	if err != nil {
		return nil, err
	}
	stored := map[string]*Row{}
	for _, row := range storedRows {
		stored[row.UID] = row
	}
	// An empty feed with nothing stored (between terms, before the timetable is out) stores nothing either: it is no
	// first poll, or every hourly poll would be one. The first feed with classes is.
	first := (len(stored) == 0 || relinked) && len(classes) > 0
	diff := &Diff{First: first, Relinked: first && len(stored) > 0}
	// New classes past the end of what the feed covered are just the weeks moving on: no news.
	horizon := ""
	for _, row := range storedRows {
		if row.GoneAt == nil && row.Start > horizon {
			horizon = row.Start
		}
	}
	seen := map[string]bool{}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, item := range classes {
		if seen[item.UID] {
			continue
		}
		seen[item.UID] = true
		start, end := item.Start.In(clock.TZ).Format(clock.LocalLayout), item.End.In(clock.TZ).Format(clock.LocalLayout)
		old := stored[item.UID]
		if old == nil {
			if _, err := tx.Exec("insert into classes(uid, subject, start, end, kind, room, link, cancelled, seen_at) "+
				"values(?, ?, ?, ?, ?, ?, ?, ?, ?)", item.UID, item.Subject, start, end, item.Kind, item.Room, item.Link,
				item.Cancelled, now); err != nil {
				return nil, err
			}
			if !diff.First && start <= horizon {
				row, err := byUID(tx, item.UID)
				if err != nil {
					return nil, err
				}
				diff.Changes = append(diff.Changes, Change{Kind: "new", New: row})
			}
			continue
		}
		if _, err := tx.Exec("update classes set subject = ?, start = ?, end = ?, kind = ?, room = ?, link = ?, "+
			"cancelled = ?, seen_at = ?, gone_at = null where uid = ?", item.Subject, start, end, item.Kind, item.Room,
			item.Link, item.Cancelled, now, item.UID); err != nil {
			return nil, err
		}
		if kind := change(old, start, end, item); kind != "" {
			row, err := byUID(tx, item.UID)
			if err != nil {
				return nil, err
			}
			diff.Changes = append(diff.Changes, Change{Kind: kind, Old: old, New: row})
		}
	}
	for _, old := range storedRows {
		if !seen[old.UID] && old.GoneAt == nil && old.Start >= now {
			if _, err := tx.Exec("update classes set gone_at = ? where uid = ?", now, old.UID); err != nil {
				return nil, err
			}
			row, err := byUID(tx, old.UID)
			if err != nil {
				return nil, err
			}
			diff.Changes = append(diff.Changes, Change{Kind: "removed", Old: old, New: row})
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := r.store.PutJSON(SizeKey, len(seen)); err != nil {
		return nil, err
	}
	return diff, nil
}

func change(old *Row, start, end string, item Class) string {
	switch {
	case old.GoneAt != nil:
		return "restored"
	case old.Cancelled != item.Cancelled:
		if item.Cancelled {
			return "cancelled"
		}
		return "restored"
	case old.Start != start || old.End != end:
		return "moved"
	case old.Room != item.Room:
		return "room"
	}
	return ""
}

// digest: the saved ETag belongs to one link; the link itself is a secret and is not stored.
func digest(address string) string {
	sum := sha256.Sum256([]byte(address))
	return hex.EncodeToString(sum[:])[:16]
}
