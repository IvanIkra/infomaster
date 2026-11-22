// Package sheets is the problem sheets (листочки, §6.3, D14): PDFs with tasks from the chats, kept as rows here; the
// web app's shelves show them. The port of processing/sheets.py.
//
//  1. The batch reads a PDF by its name and its first page (a scan: the first page as an image) and says whether it
//     is a sheet, with its course, number and due date (processing/batches). The sheet becomes a row here, `new`, in
//     the transaction of the batch, so nothing is lost when a run fails. The same file again (forwarded, posted twice)
//     is no new sheet.
//  2. Before it is published, the sheet is prepared once: its pages are read locally for the next version's diff,
//     and it is matched against earlier sheets (`previous`). A newer version: the same message with a new file, or
//     the same chat and course with the same number or almost the same file name (no number to tell them apart), the
//     older sheet at most 30 days old. With no certainty it is a new sheet; the owner can join it to an older one in
//     the web app.
//  3. With `pdf_content` on in the chat, the model reads the sheet (task "pdf", an optional one: off above 80 % of
//     the limit): at most `pdf_pages` pages and `pdf_tokens` tokens, what comes first. It writes a short summary of
//     it and gives the deadlines of its pages; they join the deadlines from the chat (source tg, the sheet's
//     message), and an edit of the message's text does not withdraw them, its deletion does. For a newer version the
//     model sees only the diff of the texts and says what changed. The summary and the changes are written in the
//     app's language (D41), as it is when the sheet is prepared.
//  4. The owner changes a sheet's course, number (or a short title instead, `numbered`), due date and summary in the
//     web app, and adds sheets of their own: a PDF uploaded there, of the chat OwnChat (D44). An own sheet is
//     published as a chat's, gets newer versions by the same rules among the own ones, and is the only kind the
//     owner may delete.
//  5. A sheet without a due date is due before the next seminar of its course (`due_assumed`): the first seminar or
//     practical class in the timetable as the feeds show it (corrections applied, cancelled ones skipped) that starts
//     after its message, within 21 days (processing/schedule, NextClass); a course without seminars takes its next
//     class of any kind; none found, no date. The date is a deadline of its own (source tg, the sheet's message,
//     `assumed`), "<abbr> sheet <number>", and both are shown as assumed. It follows the timetable: synced with the
//     Midterms after every RUZ poll, batch run and correction by the owner (Follow), it moves with its class and is
//     withdrawn when no class is left; once the class has started it stays. A real date wins and the mark goes: the
//     sheet's pages, a later message (the batch moves the assumed deadline or gives the same work with a date), or
//     the owner (the sheet's date, or the date of its deadline). A newer version takes the older one's assumed
//     deadline over and finds its own date the same way. Sheets of before the rule are not assumed.
//  6. A prepared sheet, a newer version too, is copied into the library (D58, §6.8) as kind `sheet`, at the class of
//     its course that was on when it was sent (the seminar it was handed out at), else at its course alone: there it
//     outlives the 14 days of its file here.
//
// Publishing is outputs/sheets; the status `posted` means published (D69). Writing methods commit themselves, except
// Add: the batch calls it inside its own transaction.
//
// One connection (PORTING.md): the next seminar is read from the timetable before a transaction opens, never inside
// it; everything inside uses repositories bound to the transaction.
package sheets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/pdf"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	VersionWindow = 30 * 24 * time.Hour // D14: an older sheet gets no newer version by the rules
	TextPages     = 50                  // read locally for the diff of the next version
	TextChars     = 200_000
	DiffChars     = 8_000 // of the diff sent to the model
	SummaryChars  = 600
	ChangesChars  = 400
	MaxTokens     = 2_000
	// FollowWindow: past that, no class can be a sheet's next seminar any more.
	FollowWindow = (schedule.SheetDays + 1) * 24 * time.Hour
)

// LabelChars is a deadline label's budget, as in the batches (Tablo's column, §2.1): hseapp.LabelChars, the one copy.
const LabelChars = hseapp.LabelChars

// PromptWords are the words of the prompts in each language the summary may be written in.
var PromptWords = map[string]map[string]string{
	"en": {"language": "English", "no_intro": "this sheet", "layout_only": "Only the layout changed."},
	"ru": {"language": "Russian", "no_intro": "в этом листке", "layout_only": "Изменилось только оформление."},
}

// Sheet is a row of `sheets`.
type Sheet struct {
	ID         int64   `json:"id"`
	ChatID     int64   `json:"chat_id"`
	MsgID      int64   `json:"msg_id"`
	Name       string  `json:"name"`
	Sha256     string  `json:"sha256"`
	Course     *string `json:"course"`
	Number     string  `json:"number"`
	Due        *string `json:"due"`
	DueAssumed int64   `json:"due_assumed"`
	DueAt      *string `json:"due_at"`
	SentAt     *string `json:"sent_at"`
	Caption    *string `json:"caption"`
	Status     string  `json:"status"`
	Error      *string `json:"error"`
	Text       *string `json:"text"`
	Summary    *string `json:"summary"`
	Changes    *string `json:"changes"`
	Previous   *int64  `json:"previous"`
	Version    int64   `json:"version"`
	PostID     *int64  `json:"post_id"`
	CreatedAt  string  `json:"created_at"`
}

// CourseName is the course, "" for none.
func (s *Sheet) CourseName() string { return text(s.Course) }

const columns = "id, chat_id, msg_id, name, sha256, course, number, due, due_assumed, due_at, sent_at, caption, " +
	"status, error, text, summary, changes, previous, version, post_id, created_at"

func scanSheet(scan func(...any) error) (*Sheet, error) {
	var s Sheet
	err := scan(&s.ID, &s.ChatID, &s.MsgID, &s.Name, &s.Sha256, &s.Course, &s.Number, &s.Due, &s.DueAssumed,
		&s.DueAt, &s.SentAt, &s.Caption, &s.Status, &s.Error, &s.Text, &s.Summary, &s.Changes, &s.Previous,
		&s.Version, &s.PostID, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func one(q store.Querier, query string, args ...any) (*Sheet, error) {
	s, err := scanSheet(q.QueryRow(query, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

func list(q store.Querier, query string, args ...any) ([]*Sheet, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Sheet{}
	for rows.Next() {
		s, err := scanSheet(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptr[T any](v T) *T { return &v }

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func squeeze(s string) string { return strings.Join(pytext.Split(s), " ") }

// Origin is the source of a sheet's message: a letter's PDF (D59) is of the mailbox, any other of a chat.
func Origin(sheet *Sheet) string {
	if sheet.ChatID == inbox.Mailbox {
		return "mail"
	}
	return "tg"
}

func dropMarks(s string) string {
	return strings.Map(func(r rune) rune {
		if pytext.IsSpace(r) || r == '№' || r == '#' {
			return -1
		}
		return r
	}, s)
}

// NumberKey is '№ 3' -> '3', 'ДЗ-2.1' -> 'дз-2.1': what two sheets are compared by.
func NumberKey(number string) string { return dropMarks(pytext.Casefold(number)) }

// Numbered: a sheet's `number` holds its number ("3", "№ 2.1", "4b") or, without one, a short title ("Test 1
// problems").
func Numbered(number string) bool {
	for _, r := range number {
		if pytext.IsSpace(r) || r == '№' || r == '#' {
			continue
		}
		return unicode.IsDigit(r)
	}
	return false
}

// SheetTitle is '3' -> 'Sheet 3', a title as it is, nothing -> 'Sheet'.
func SheetTitle(number string) string {
	if Numbered(number) {
		return squeeze("Sheet " + number)
	}
	if t := squeeze(number); t != "" {
		return t
	}
	return "Sheet"
}

// SheetLabel is the label of a sheet's assumed deadline, as the batches write a sheet's: "Calc sheet 3", "Calc: Test
// 1 tasks".
func SheetLabel(course, number string) string {
	if Numbered(number) || pytext.Strip(number) == "" {
		return squeeze(course + " sheet " + number)
	}
	if pytext.Strip(course) != "" {
		return squeeze(course + ": " + number)
	}
	return squeeze(number)
}

var datePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// DueDate is a model's YYYY-MM-DD when it is a real date; nil otherwise.
func DueDate(value any) *string {
	s, ok := value.(string)
	if !ok {
		return nil
	}
	s = pytext.Strip(s)
	if !datePattern.MatchString(s) {
		return nil
	}
	if _, err := time.Parse(clock.DateLayout, s); err != nil {
		return nil
	}
	return &s
}

// Found is a sheet the batch found in a message: its course, number (or title) and due date.
type Found struct {
	MsgID  int64   `json:"msg_id"`
	Course string  `json:"course"`
	Number string  `json:"number"`
	Due    *string `json:"due"`
}

// Status is status().
type Status struct {
	Waiting int64   `json:"waiting"`
	Error   *string `json:"error"`
}

// Sheets keeps the sheets.
type Sheets struct {
	store        *store.Store
	db           *sql.DB
	Settings     *settings.Settings
	ChatSettings *settings.ChatSettings
	Inbox        *inbox.Inbox
	Files        *files.Files
	Deadlines    *deadlines.Deadlines
	Subjects     *subjects.Subjects
	LLM          llm.Model
	Now          func() time.Time
	// Schedule is where the next seminar is found; without it no date is assumed.
	Schedule *schedule.Schedule
	// OnChange hears of the owner's changes of the assumed deadlines (the refs it touched): no news to notify of.
	OnChange func(refs map[string]bool)
	// Library is where every sheet's PDF is kept for good (D58).
	Library *library.Library

	mu  sync.Mutex
	err string // of the last reading that failed, for the status
}

// New makes the sheets; now nil is clock.NowUTC.
func New(s *store.Store, st *settings.Settings, chats *settings.ChatSettings, in *inbox.Inbox, f *files.Files,
	dl *deadlines.Deadlines, sub *subjects.Subjects, model llm.Model, now func() time.Time) *Sheets {
	if now == nil {
		now = clock.NowUTC
	}
	return &Sheets{store: s, db: s.DB, Settings: st, ChatSettings: chats, Inbox: in, Files: f, Deadlines: dl,
		Subjects: sub, LLM: model, Now: now}
}

func (s *Sheets) setError(e string) {
	s.mu.Lock()
	s.err = e
	s.mu.Unlock()
}

// LastError is the error of the last reading that failed, "" for none.
func (s *Sheets) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Status is the number waiting and the last error.
func (s *Sheets) Status() (Status, error) {
	var n int64
	if err := s.db.QueryRow("select count(*) n from sheets where status = 'new'").Scan(&n); err != nil {
		return Status{}, err
	}
	return Status{Waiting: n, Error: nullable(s.LastError())}, nil
}

// --- rows -------------------------------------------------------------------------------

// Get is one sheet; nil for none.
func (s *Sheets) Get(id int64) (*Sheet, error) { return get(s.db, id) }

func get(q store.Querier, id int64) (*Sheet, error) {
	return one(q, "select "+columns+" from sheets where id = ?", id)
}

// Add keeps a sheet the batch found in a message whose file was read (`file`, its files row; `msgAt` the message's
// time). Nil when the file is no sheet to post: not downloaded, unreadable, or posted already. Call it with the
// batch's transaction.
func (s *Sheets) Add(q store.Querier, chatID, msgID int64, msgAt string, file *files.Row, sheet Found,
	at time.Time) (*int64, error) {
	if file == nil || file.Sha256 == nil || *file.Sha256 == "" {
		return nil, nil
	}
	known, err := knownIn(q, *file.Sha256)
	if err != nil || known {
		return nil, err
	}
	name := text(file.Name)
	if name == "" {
		name = "sheet.pdf"
	}
	assumed := 0
	if sheet.Due == nil || *sheet.Due == "" {
		assumed = 1
	}
	result, err := q.Exec("insert into sheets(chat_id, msg_id, name, sha256, course, number, due, due_assumed, "+
		"sent_at, status, created_at) values(?, ?, ?, ?, ?, ?, ?, ?, ?, 'new', ?)",
		chatID, msgID, name, *file.Sha256, nullable(sheet.Course), sheet.Number, sheet.Due, assumed, msgAt,
		clock.LocalISO(at))
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	return &id, err
}

// AddOwn is an own sheet of an uploaded PDF (`file`: its files row), or of one sent to the bot with its `caption`
// (D73); it waits to be published as a chat's.
func (s *Sheets) AddOwn(file *files.Row, course, number string, due *string, at time.Time,
	caption string) (int64, error) {
	name := text(file.Name)
	if name == "" {
		name = "sheet.pdf"
	}
	assumed := 0
	if due == nil || *due == "" {
		assumed = 1
	}
	result, err := s.db.Exec("insert into sheets(chat_id, msg_id, name, sha256, course, number, due, due_assumed, "+
		"sent_at, caption, status, created_at) values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'new', ?)",
		files.OwnChat, file.MsgID, name, text(file.Sha256), nullable(course), number, due, assumed,
		clock.LocalISO(at), nullable(caption), clock.LocalISO(at))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// Known: the file is a sheet already (not a failed one): the same PDF again is no new sheet.
func (s *Sheets) Known(sha string) (bool, error) { return knownIn(s.db, sha) }

func knownIn(q store.Querier, sha string) (bool, error) {
	var one int
	err := q.QueryRow("select 1 from sheets where sha256 = ? and status != 'failed'", sha).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Edit is the owner's change: course, number and due date of the sheet as it is now (its newest version), the
// summary of its first version. `fields` holds the names changed ("course", "number", "due", "summary"); a nil value
// is null. A due date given (or cleared) is real: the assumed one and its mark go; a new course or number makes the
// assumed date follow it. The sheet, or nil for no such sheet.
func (s *Sheets) Edit(sheetID int64, fields map[string]*string) (*Sheet, error) {
	sheet, err := s.Get(sheetID)
	if err != nil || sheet == nil {
		return nil, err
	}
	_, hasDue := fields["due"]
	_, hasCourse := fields["course"]
	_, hasNumber := fields["number"]
	var next *schedule.Entry
	if !hasDue && (hasCourse || hasNumber) {
		course := sheet.Course
		if hasCourse {
			course = fields["course"]
		}
		if next, err = s.nextFor(course, sheet.SentAt); err != nil {
			return nil, err
		}
	}
	touched := map[int64]bool{}
	err = s.store.InTx(func(tx *sql.Tx) error {
		for _, name := range []string{"course", "number"} {
			if value, ok := fields[name]; ok {
				if _, err := tx.Exec("update sheets set "+name+" = ? where id = ?", value, sheetID); err != nil {
					return err
				}
			}
		}
		if value, ok := fields["summary"]; ok {
			first, err := firstOf(tx, sheet)
			if err != nil {
				return err
			}
			if _, err := tx.Exec("update sheets set summary = ? where id = ?", value, first.ID); err != nil {
				return err
			}
		}
		now, err := get(tx, sheetID)
		if err != nil {
			return err
		}
		if hasDue {
			touched, err = s.settle(tx, now, fields["due"], nil)
		} else if hasCourse || hasNumber {
			touched, err = s.follow(tx, now, &schedule.MidtermChanges{}, next)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	s.changed(touched)
	return s.Get(sheetID)
}

// changed: the owner changed these deadlines, they are no news.
func (s *Sheets) changed(ids map[int64]bool) {
	if len(ids) > 0 && s.OnChange != nil {
		refs := map[string]bool{}
		for id := range ids {
			refs[fmt.Sprintf("dl:%d", id)] = true
		}
		s.OnChange(refs)
	}
}

// RemoveOwn deletes an own sheet with every version of it: the rows deleted, or nil when it is no own sheet.
func (s *Sheets) RemoveOwn(sheetID int64) ([]*Sheet, error) {
	sheet, err := s.Get(sheetID)
	if err != nil || sheet == nil || sheet.ChatID != files.OwnChat {
		return nil, err
	}
	first, err := firstOf(s.db, sheet)
	if err != nil {
		return nil, err
	}
	chain := []*Sheet{first}
	for {
		newer, err := one(s.db, "select "+columns+" from sheets where previous = ?", chain[len(chain)-1].ID)
		if err != nil {
			return nil, err
		}
		if newer == nil || slices.ContainsFunc(chain, func(c *Sheet) bool { return c.ID == newer.ID }) {
			break
		}
		chain = append(chain, newer)
	}
	for _, c := range chain {
		if c.ChatID != files.OwnChat {
			return nil, nil // a chat's sheet joined to it: that one is not the owner's to delete
		}
	}
	var assumed []int64
	for _, c := range chain {
		rows, err := assumedOf(s.Deadlines, c.ID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			assumed = append(assumed, row.ID)
		}
	}
	err = s.store.InTx(func(tx *sql.Tx) error {
		ids := make([]any, len(chain))
		for i, c := range chain {
			ids[i] = c.ID
		}
		if _, err := tx.Exec("delete from sheets where id in ("+marks(len(ids))+")", ids...); err != nil {
			return err
		}
		if len(assumed) > 0 {
			if _, err := s.Deadlines.With(tx).Drop(assumed, s.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	set := map[int64]bool{}
	for _, id := range assumed {
		set[id] = true
	}
	s.changed(set)
	return chain, nil
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

// Waiting are the sheets not published yet, oldest first.
func (s *Sheets) Waiting() ([]*Sheet, error) {
	return list(s.db, "select "+columns+" from sheets where status = 'new' order by id")
}

// First is the first version of a sheet: the one whose post every version edits.
func (s *Sheets) First(sheet *Sheet) (*Sheet, error) { return firstOf(s.db, sheet) }

func firstOf(q store.Querier, sheet *Sheet) (*Sheet, error) {
	seen := map[int64]bool{}
	for sheet.Previous != nil && !seen[sheet.ID] {
		seen[sheet.ID] = true
		older, err := get(q, *sheet.Previous)
		if err != nil {
			return nil, err
		}
		if older == nil {
			break
		}
		sheet = older
	}
	return sheet, nil
}

// Versions are a sheet's versions in order, from its first one.
func (s *Sheets) Versions(first *Sheet) ([]*Sheet, error) { return versionsOf(s.db, first) }

func versionsOf(q store.Querier, first *Sheet) ([]*Sheet, error) {
	chain := []*Sheet{first}
	for {
		newer, err := one(q, "select "+columns+" from sheets where previous = ? and status = 'posted' order by id "+
			"limit 1", chain[len(chain)-1].ID)
		if err != nil {
			return nil, err
		}
		if newer == nil {
			return chain, nil
		}
		chain = append(chain, newer)
	}
}

// latest: no posted sheet updates it.
func latest(q store.Querier, sheetID int64) (bool, error) {
	var one int
	err := q.QueryRow("select 1 from sheets where previous = ? and status = 'posted'", sheetID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return false, err
}

// CourseNames are the courses of the recent sheets: the dictionary names them.
func (s *Sheets) CourseNames(today time.Time) (map[string]bool, error) {
	since := clock.LocalISO(clock.LocalDay(today).Add(-VersionWindow))
	rows, err := s.db.Query("select distinct course from sheets where course is not null and created_at >= ?", since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = true
	}
	return names, rows.Err()
}

// Posted marks a sheet published (D69).
func (s *Sheets) Posted(sheetID int64) error {
	_, err := s.db.Exec("update sheets set status = 'posted' where id = ?", sheetID)
	return err
}

// FileGone is the reason a sheet failed when its file is gone, for the web app: kept in the row, so in its language
// of now.
func (s *Sheets) FileGone() string {
	lang, _ := s.Settings.String("language")
	return i18n.Tr(lang, "the file is deleted already", "файл уже удалён")
}

// Failed marks a sheet failed with its reason.
func (s *Sheets) Failed(sheetID int64, reason string) error {
	_, err := s.db.Exec("update sheets set status = 'failed', error = ? where id = ?", reason, sheetID)
	return err
}

// --- versions ----------------------------------------------------------------------------

func courseKey(sub *subjects.Subjects, course *string) (string, error) {
	abbr, err := sub.Abbr(text(course))
	if err != nil {
		return "", err
	}
	if abbr == "" {
		abbr = subjects.Clean(text(course))
	}
	return pytext.Casefold(abbr), nil
}

// Same: a newer version of `old` by the rules of D14 (the age is the caller's).
func (s *Sheets) Same(newer, old *Sheet) (bool, error) {
	if newer.ChatID == old.ChatID && newer.MsgID == old.MsgID {
		return true, nil // the message's file was replaced
	}
	if newer.ChatID != old.ChatID {
		return false, nil
	}
	a, err := courseKey(s.Subjects, newer.Course)
	if err != nil {
		return false, err
	}
	b, err := courseKey(s.Subjects, old.Course)
	if err != nil || a != b {
		return false, err
	}
	ka, kb := NumberKey(newer.Number), NumberKey(old.Number)
	if ka != "" && kb != "" {
		return ka == kb, nil
	}
	return pdf.SimilarNames(newer.Name, old.Name), nil
}

// FindPrevious is the newest version of an earlier sheet this one updates, if any.
func (s *Sheets) FindPrevious(sheet *Sheet) (*Sheet, error) {
	created, err := clock.ParseLocal(sheet.CreatedAt)
	if err != nil {
		return nil, err
	}
	rows, err := list(s.db, "select "+columns+" from sheets where status = 'posted' and id != ? and chat_id = ? "+
		"and created_at >= ? order by id desc", sheet.ID, sheet.ChatID, clock.LocalISO(created.Add(-VersionWindow)))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		last, err := latest(s.db, row.ID)
		if err != nil {
			return nil, err
		}
		if !last {
			continue
		}
		same, err := s.Same(sheet, row)
		if err != nil {
			return nil, err
		}
		if same {
			return row, nil
		}
	}
	return nil, nil
}

// --- preparing for publishing ---------------------------------------------------------------

// Prepare reads the sheet, finds what it updates, and with `pdf_content` lets the model read it; once. Nil when its
// file is gone (the sheet fails). A prepared sheet also goes to the library.
func (s *Sheets) Prepare(ctx context.Context, sheetID int64) (*Sheet, error) {
	sheet, err := s.prepare(ctx, sheetID)
	if err != nil {
		return nil, err
	}
	if sheet != nil && s.Library != nil {
		if _, err := s.shelve(sheet); err != nil { // the sheet is published all the same
			slog.Error("the sheet did not go to the library", "sheet", sheetID, "err", err)
		}
	}
	return sheet, nil
}

// shelve puts the sheet's PDF into the library (D58), kind `sheet`, at its seminar (the nearest one); once per file,
// so a new version in the same message is taken too. Its item's id, or nil when it is there already.
func (s *Sheets) shelve(sheet *Sheet) (*int64, error) {
	found, err := s.Library.FromOrigin("sheet", sheet.ChatID, sheet.MsgID, sheet.Sha256)
	if err != nil || found != nil {
		return nil, err
	}
	path := s.Files.ByHash(sheet.Sha256)
	if path == "" {
		return nil, nil
	}
	var sent time.Time
	if sheet.SentAt != nil && *sheet.SentAt != "" {
		if sent, err = clock.ParseLocal(*sheet.SentAt); err != nil {
			return nil, err
		}
	}
	classUID := ""
	if sheet.CourseName() != "" && !sent.IsZero() {
		class, err := s.Library.FindClass(sheet.CourseName(), sent, true)
		if err != nil {
			return nil, err
		}
		if class != nil {
			classUID = class.UID
		}
	}
	author := "owner"
	if sheet.ChatID != files.OwnChat {
		message, err := s.Inbox.Message(sheet.ChatID, sheet.MsgID, Origin(sheet))
		if err != nil {
			return nil, err
		}
		author = ""
		if message != nil && message.SenderName != nil {
			author = *message.SenderName
		}
	}
	note := ""
	if sheet.CourseName() == "" {
		note = "the sheet's course is not known"
	}
	copyFile := func(part string) (bool, error) { return true, copyPath(path, part) }
	chatID, msgID := sheet.ChatID, sheet.MsgID
	return s.Library.AddDownload(copyFile, sheet.Name, "application/pdf", "sheet", library.Item{
		Source: "sheet", ChatID: &chatID, MsgID: &msgID, Course: sheet.CourseName(), ClassUID: classUID,
		Title: SheetTitle(sheet.Number), Author: author, At: sent, Note: note})
}

func copyPath(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// nextFor is the class an assumed date would be, read before a transaction opens (one connection).
func (s *Sheets) nextFor(course, sentAt *string) (*schedule.Entry, error) {
	if s.Schedule == nil || sentAt == nil || *sentAt == "" || text(course) == "" {
		return nil, nil
	}
	at, err := clock.ParseLocal(*sentAt)
	if err != nil {
		return nil, err
	}
	return s.Schedule.NextClass(*course, at, schedule.SheetDays)
}

type labelDue struct{ label, due string }

func (s *Sheets) prepare(ctx context.Context, sheetID int64) (*Sheet, error) {
	sheet, err := s.Get(sheetID)
	if err != nil || sheet == nil {
		return nil, err
	}
	if sheet.Text != nil {
		return sheet, nil
	}
	path := s.Files.ByHash(sheet.Sha256)
	if path == "" {
		return nil, s.Failed(sheetID, s.FileGone())
	}
	extract, err := pdf.ExtractText(path, TextPages)
	if err != nil {
		var p *pdf.PdfError
		if errors.As(err, &p) {
			return nil, s.Failed(sheetID, p.Msg)
		}
		return nil, err
	}
	previous, err := s.FindPrevious(sheet)
	if err != nil {
		return nil, err
	}
	pages := pytext.Cut(extract.Text(), TextChars)
	var summary, changes *string
	var found []labelDue
	chat, err := s.ChatSettings.All(sheet.ChatID)
	if err != nil {
		return nil, err
	}
	if chat.PDFContent {
		if previous == nil {
			if summary, found, err = s.read(ctx, sheet, path, extract); err != nil {
				return nil, err
			}
		} else if changes, err = s.describe(ctx, sheet, text(previous.Text), pages); err != nil {
			return nil, err
		}
	}
	next, err := s.nextFor(sheet.Course, sheet.SentAt)
	if err != nil {
		return nil, err
	}
	var touched map[int64]bool
	err = s.store.InTx(func(tx *sql.Tx) error {
		var previousID *int64
		version := int64(1)
		if previous != nil {
			previousID, version = &previous.ID, previous.Version+1
		}
		if _, err := tx.Exec("update sheets set text = ?, summary = ?, changes = ?, previous = ?, version = ? "+
			"where id = ?", pages, summary, changes, previousID, version, sheetID); err != nil {
			return err
		}
		if err := s.saveDeadlines(tx, sheet, found); err != nil {
			return err
		}
		if previous != nil {
			if err := s.inherit(tx, previous, sheet); err != nil {
				return err
			}
		}
		now, err := get(tx, sheetID)
		if err != nil {
			return err
		}
		touched, err = s.follow(tx, now, &schedule.MidtermChanges{}, next)
		return err
	})
	if err != nil {
		return nil, err
	}
	if sheet.ChatID == files.OwnChat {
		s.changed(touched) // an own sheet is the owner's change; a chat's is told after its batch run
	}
	return s.Get(sheetID)
}

// saveDeadlines keeps the deadlines of the sheet's pages that the chat has not given yet; one the service only
// assumed is told for real now.
func (s *Sheets) saveDeadlines(q store.Querier, sheet *Sheet, found []labelDue) error {
	dl := s.Deadlines.With(q)
	for _, d := range found {
		known, err := dl.Find(d.label, d.due)
		if err != nil {
			return err
		}
		if known == nil {
			chatID := sheet.ChatID
			if _, err := dl.Add(deadlines.New{Source: Origin(sheet), ChatID: &chatID, MsgIDs: []int64{sheet.MsgID},
				Course: sheet.CourseName(), Label: d.label, Due: d.due, SheetID: &sheet.ID}, s.Now()); err != nil {
				return err
			}
		} else if known.Assumed {
			if err := dl.Confirm(known.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- the assumed due date (§6.3) ---------------------------------------------------------------

// assumedOf are the sheet's assumed deadlines in force, oldest first (one, unless a version took another's over).
func assumedOf(dl *deadlines.Deadlines, sheetID int64) ([]*deadlines.Deadline, error) {
	rows, err := dl.OfSheet(sheetID)
	if err != nil {
		return nil, err
	}
	out := []*deadlines.Deadline{}
	for _, row := range rows {
		if row.Assumed && row.MovedBy == nil && row.GoneAt == nil {
			out = append(out, row)
		}
	}
	return out, nil
}

// label is the label of the sheet's assumed deadline: the course by its abbr once the dictionary knows it.
func label(sub *subjects.Subjects, sheet *Sheet) (string, error) {
	abbr, err := sub.Abbr(sheet.CourseName())
	if err != nil {
		return "", err
	}
	if abbr == "" {
		abbr = subjects.Clean(sheet.CourseName())
	}
	return SheetLabel(abbr, sheet.Number), nil
}

// told is the earliest real date of the sheet in force: a deadline read in its pages, an assumed one a message moved
// or told again, or the same work from the chats or mail due on the sheet's day or later.
func (s *Sheets) told(q store.Querier, sheet *Sheet) (*deadlines.Deadline, error) {
	dl, sub := s.Deadlines.With(q), s.Subjects.With(q)
	var found []*deadlines.Deadline
	rows, err := dl.OfSheet(sheet.ID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		last, err := dl.Latest(row.ID)
		if err != nil {
			return nil, err
		}
		if last != nil && last.MovedBy == nil && last.GoneAt == nil && !last.Assumed {
			found = append(found, last)
		}
	}
	if sheet.CourseName() != "" && sheet.SentAt != nil && *sheet.SentAt != "" {
		mineLabel, err := label(sub, sheet)
		if err != nil {
			return nil, err
		}
		day, err := todo.ParseDate(pytext.Cut(*sheet.SentAt, 10))
		if err != nil {
			return nil, err
		}
		abbr, err := sub.Abbr(sheet.CourseName())
		if err != nil {
			return nil, err
		}
		mine := todo.Deadline{Title: mineLabel, Due: day, Category: "study", Source: "tg", Course: nullable(abbr)}
		upcoming, err := dl.Upcoming(day, 0)
		if err != nil {
			return nil, err
		}
		for _, row := range upcoming {
			if row.Assumed {
				continue
			}
			due, err := todo.ParseDate(row.Due)
			if err != nil {
				return nil, err
			}
			theirAbbr, err := sub.Abbr(row.CourseName())
			if err != nil {
				return nil, err
			}
			theirs := todo.Deadline{Title: row.Label, Due: due, Category: "study", Source: row.Source,
				Course: nullable(theirAbbr)}
			// Without a number, "Calc sheet" would be the same work as every sheet of the course.
			if deadlines.SameLabel(row.Label, mineLabel) || (sheet.Number != "" && merge.SameWork(mine, theirs)) {
				found = append(found, row)
			}
		}
	}
	var best *deadlines.Deadline
	for _, row := range found {
		if best == nil || row.Due < best.Due || (row.Due == best.Due && row.ID < best.ID) {
			best = row
		}
	}
	return best, nil
}

// hidden: the owner hid the deadline (or changed it into an own copy): it is theirs now, it does not move.
func hidden(q store.Querier, deadlineID int64) (bool, error) {
	var one int
	err := q.QueryRow("select 1 from hidden_deadlines where ref = ?", fmt.Sprintf("dl:%d", deadlineID)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func samePtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// setDue is the sheet's date as it is now.
func setDue(q store.Querier, sheet *Sheet, due *string, assumed bool, dueAt *string) error {
	if samePtr(sheet.Due, due) && (sheet.DueAssumed != 0) == assumed && samePtr(sheet.DueAt, dueAt) {
		return nil
	}
	flag := 0
	if assumed {
		flag = 1
	}
	_, err := q.Exec("update sheets set due = ?, due_assumed = ?, due_at = ? where id = ?", due, flag, dueAt, sheet.ID)
	return err
}

// settle is the sheet's real date (nil: no date at all): the mark goes, and its assumed deadlines move to the one
// that tells it (`told`; without one, a new one on that date), are told for real when the date is the same (or the
// owner hid them), or are withdrawn without a date. The deadlines touched.
func (s *Sheets) settle(q store.Querier, sheet *Sheet, due *string, told *deadlines.Deadline) (map[int64]bool,
	error) {
	if err := setDue(q, sheet, due, false, nil); err != nil {
		return nil, err
	}
	dl := s.Deadlines.With(q)
	touched := map[int64]bool{}
	rows, err := assumedOf(dl, sheet.ID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		touched[row.ID] = true
		isHidden, err := hidden(q, row.ID)
		if err != nil {
			return nil, err
		}
		switch {
		case isHidden || (told == nil && due != nil && row.Due == *due):
			err = dl.Confirm(row.ID)
		case due == nil:
			_, err = dl.Drop([]int64{row.ID}, s.Now())
		default:
			if told == nil {
				chatID := sheet.ChatID
				id, err := dl.Add(deadlines.New{Source: Origin(sheet), ChatID: &chatID, MsgIDs: row.MsgIDs,
					Course: row.CourseName(), Label: row.Label, Due: *due, SheetID: &sheet.ID}, s.Now())
				if err != nil {
					return nil, err
				}
				if told, err = dl.Get(id); err != nil {
					return nil, err
				}
				touched[told.ID] = true
			}
			_, err = dl.Move(row.ID, told.ID)
		}
		if err != nil {
			return nil, err
		}
	}
	return touched, nil
}

// inherit: a newer version takes the older one's assumed deadline over: moved into its own one if it has one, else
// made its own (with its message: deleting the older message does not withdraw it).
func (s *Sheets) inherit(q store.Querier, previous, sheet *Sheet) error {
	dl := s.Deadlines.With(q)
	mine, err := assumedOf(dl, sheet.ID)
	if err != nil {
		return err
	}
	theirs, err := assumedOf(dl, previous.ID)
	if err != nil {
		return err
	}
	for _, row := range theirs {
		if len(mine) > 0 {
			_, err = dl.Move(row.ID, mine[0].ID)
		} else {
			_, err = q.Exec("update deadlines set sheet_id = ?, chat_id = ?, msg_ids = ? where id = ?",
				sheet.ID, sheet.ChatID, fmt.Sprintf("[%d]", sheet.MsgID), row.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Follow is a follower of the schedule (Schedule.Followers): the assumed due dates of the recent sheets follow the
// timetable and give way to real ones; what changed, as no news.
func (s *Sheets) Follow() (schedule.MidtermChanges, error) {
	changes := schedule.MidtermChanges{}
	since := clock.LocalISO(s.Now().Add(-FollowWindow))
	rows, err := list(s.db, "select "+columns+" from sheets where status != 'failed' and sent_at >= ? and "+
		"(due_assumed = 1 or id in (select sheet_id from deadlines where assumed = 1 and moved_by is null and "+
		"gone_at is null)) order by id", since)
	if err != nil {
		return changes, err
	}
	next := make([]*schedule.Entry, len(rows))
	for i, row := range rows {
		if next[i], err = s.nextFor(row.Course, row.SentAt); err != nil {
			return changes, err
		}
	}
	err = s.store.InTx(func(tx *sql.Tx) error {
		for i, row := range rows {
			last, err := latest(tx, row.ID)
			if err != nil {
				return err
			}
			if last {
				if _, err := s.follow(tx, row, &changes, next[i]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return changes, err
}

// follow is one sheet: a real date told since settles it; else its assumed date is the next seminar's as the
// timetable is now (`next`, read before the transaction), unless that class has started or the owner hid the
// deadline. The deadlines touched.
func (s *Sheets) follow(q store.Querier, sheet *Sheet, changes *schedule.MidtermChanges,
	next *schedule.Entry) (map[int64]bool, error) {
	dl, sub := s.Deadlines.With(q), s.Subjects.With(q)
	current, err := assumedOf(dl, sheet.ID)
	if err != nil {
		return nil, err
	}
	if sheet.DueAssumed == 0 && len(current) == 0 {
		return map[int64]bool{}, nil
	}
	told, err := s.told(q, sheet)
	if err != nil {
		return nil, err
	}
	if told != nil {
		for _, row := range current {
			changes.Moved = append(changes.Moved, [2]int64{row.ID, told.ID})
		}
		return s.settle(q, sheet, &told.Due, told)
	}
	if sheet.DueAssumed == 0 { // a version with a date of its own took an older one's assumed deadline over
		return s.settle(q, sheet, sheet.Due, nil)
	}
	all, err := dl.OfSheet(sheet.ID)
	if err != nil {
		return nil, err
	}
	var assumed []*deadlines.Deadline
	for _, row := range all {
		if row.Assumed {
			assumed = append(assumed, row)
		}
	}
	if len(current) == 0 && len(assumed) > 0 && assumed[len(assumed)-1].GoneAt != nil &&
		assumed[len(assumed)-1].MovedBy == nil && sheet.Due != nil { // withdrawn with its message: no date any more
		return map[int64]bool{}, setDue(q, sheet, nil, false, nil)
	}
	if s.Schedule == nil || (sheet.DueAt != nil && *sheet.DueAt != "" && *sheet.DueAt <= clock.LocalISO(s.Now())) {
		return map[int64]bool{}, nil
	}
	for _, row := range current {
		isHidden, err := hidden(q, row.ID)
		if err != nil || isHidden {
			return map[int64]bool{}, err
		}
	}
	touched := map[int64]bool{}
	var keep *deadlines.Deadline
	if len(current) > 0 {
		keep = current[0]
	}
	for _, extra := range current[min(1, len(current)):] { // an older version's, taken over
		if _, err := dl.Move(extra.ID, keep.ID); err != nil {
			return nil, err
		}
		touched[extra.ID] = true
	}
	var due, dueAt *string
	if sheet.SentAt != nil && *sheet.SentAt != "" && next != nil {
		due = ptr(next.Start.In(clock.TZ).Format(clock.DateLayout))
		dueAt = ptr(next.Start.In(clock.TZ).Format(clock.LocalLayout))
	}
	if err := setDue(q, sheet, due, true, dueAt); err != nil {
		return nil, err
	}
	lbl, err := label(sub, sheet)
	if err != nil {
		return nil, err
	}
	chatID := sheet.ChatID
	fresh := deadlines.New{Source: Origin(sheet), ChatID: &chatID, MsgIDs: []int64{sheet.MsgID},
		Course: sheet.CourseName(), Label: lbl, SheetID: &sheet.ID, Assumed: true}
	switch {
	case due == nil:
		if keep != nil {
			if _, err := dl.Drop([]int64{keep.ID}, s.Now()); err != nil {
				return nil, err
			}
			changes.Gone = append(changes.Gone, keep.ID)
			touched[keep.ID] = true
		}
	case keep == nil:
		fresh.Due = *due
		id, err := dl.Add(fresh, s.Now())
		if err != nil {
			return nil, err
		}
		changes.Deadlines = append(changes.Deadlines, id)
		touched[id] = true
	case keep.Due != *due:
		fresh.Due, fresh.MsgIDs = *due, keep.MsgIDs
		id, err := dl.Add(fresh, s.Now())
		if err != nil {
			return nil, err
		}
		if _, err := dl.Move(keep.ID, id); err != nil {
			return nil, err
		}
		changes.Moved = append(changes.Moved, [2]int64{keep.ID, id})
		touched[keep.ID], touched[id] = true, true
	case keep.Label != lbl || !samePtr(keep.Course, sheet.Course):
		// the dictionary named the course, or the owner
		if err := dl.Rename(keep.ID, lbl, sheet.Course); err != nil {
			return nil, err
		}
		touched[keep.ID] = true
	}
	return touched, nil
}

// Context are the lines that tell the model where the sheet comes from.
func (s *Sheets) Context(sheet *Sheet) ([]string, error) {
	chat, err := s.Inbox.Chat(sheet.ChatID)
	if err != nil {
		return nil, err
	}
	abbr, err := s.Subjects.Abbr(sheet.CourseName())
	if err != nil {
		return nil, err
	}
	where := "the university mailbox"
	if Origin(sheet) != "mail" {
		if chat != nil {
			where = `"` + chat.Title + `"`
		} else {
			where = fmt.Sprintf(`"%d"`, sheet.ChatID)
		}
	}
	created, err := clock.ParseLocal(sheet.CreatedAt)
	if err != nil {
		return nil, err
	}
	course := sheet.CourseName()
	if course == "" {
		course = "(none)"
	}
	if abbr != "" {
		course += ", abbreviation " + abbr
	}
	number := sheet.Number
	if number == "" {
		number = "(none)"
	}
	lines := []string{"Posted: " + created.Format("Mon 2006-01-02"), "Chat: " + where, "File: " + sheet.Name,
		"Course: " + course, "Number or title: " + number}
	if text(sheet.Caption) != "" {
		lines = append(lines, "The owner's caption: "+*sheet.Caption)
	}
	return lines, nil
}

// llmFailure sorts a failed call: a spent budget is logged (true), an LLM error is kept for the status (true);
// anything else is no answer of the model's and passes through (false).
func (s *Sheets) llmFailure(err error, what string, sheetID int64) bool {
	if llm.IsBudget(err) {
		slog.Info(what+": the limit", "sheet", sheetID, "err", err)
		return true
	}
	var e *llm.Error
	if errors.As(err, &e) {
		slog.Warn(what+" failed", "sheet", sheetID, "err", err)
		s.setError(err.Error())
		return true
	}
	return false
}

// pyStr is Python's str() of a JSON value, as the answers are read.
func pyStr(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprint(v)
	}
}

// truthy is Python's `value or ""` before str().
func orEmpty(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case bool:
		if !v {
			return ""
		}
	case float64:
		if v == 0 {
			return ""
		}
	case []any:
		if len(v) == 0 {
			return ""
		}
	case map[string]any:
		if len(v) == 0 {
			return ""
		}
	}
	return pyStr(value)
}

// read lets the model read the sheet within the limits: its summary and deadlines; nothing when it cannot.
func (s *Sheets) read(ctx context.Context, sheet *Sheet, path string, extract pdf.Extract) (*string, []labelDue,
	error) {
	maxPages, err := s.Settings.Int("pdf_pages")
	if err != nil {
		return nil, nil, err
	}
	tokens, err := s.Settings.Int("pdf_tokens")
	if err != nil {
		return nil, nil, err
	}
	pages := min(extract.Pages, maxPages)
	lines, err := s.Context(sheet)
	if err != nil {
		return nil, nil, err
	}
	lines = append(lines, "")
	var documents []llm.Document
	if pdf.Scanned(extract.First()) { // a scan: its pages as images, as many as the tokens allow
		pages = max(1, min(pages, tokens/llm.PageTokens))
		head, err := pdf.Head(path, pages)
		if err != nil {
			return nil, nil, err
		}
		documents = []llm.Document{{Data: head, Pages: pages}}
		lines = append(lines, fmt.Sprintf("The sheet is a scan: its first %d of %d pages are attached.", pages,
			extract.Pages))
	} else {
		parts := []string{}
		for i, page := range extract.Texts[:min(pages, len(extract.Texts))] {
			parts = append(parts, fmt.Sprintf("--- page %d ---\n%s", i+1, page))
		}
		body := strings.Join(parts, "\n")
		limit := tokens * llm.CharsPerToken
		cut := ""
		if pytext.Len(body) > limit {
			cut = ", cut at the limit"
		}
		lines = append(lines, fmt.Sprintf("Text of the sheet (%d of %d pages%s):", pages, extract.Pages, cut),
			pytext.Cut(body, limit))
	}
	chatID := sheet.ChatID
	raw, err := s.LLM.JSON(ctx, llm.Request{Task: "pdf", System: s.prompt(ReadSystem), Prompt: strings.Join(lines, "\n"),
		Schema: ReadSchema, MaxTokens: MaxTokens, ChatID: &chatID, Documents: documents})
	if err != nil {
		if s.llmFailure(err, "reading the sheet", sheet.ID) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	s.setError("")
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, nil, err
	}
	var found []labelDue
	items, _ := answer["deadlines"].([]any)
	for _, item := range items {
		d, ok := item.(map[string]any)
		if !ok {
			continue
		}
		lbl := squeeze(orEmpty(d["label"]))
		if lbl == "" || pytext.Len(lbl) > LabelChars {
			continue
		}
		if due := DueDate(d["due"]); due != nil {
			found = append(found, labelDue{lbl, *due})
		}
	}
	return nullable(pytext.Cut(pytext.Strip(orEmpty(answer["summary"])), SummaryChars)), found, nil
}

// describe is what changed against the older version, from the diff alone (D14).
func (s *Sheets) describe(ctx context.Context, sheet *Sheet, old, newer string) (*string, error) {
	changed := pdf.Diff(old, newer, DiffChars)
	if changed == "" { // a scan has no text to compare
		if old != "" && newer != "" {
			lang, _ := s.Settings.String("language")
			return ptr(i18n.Tr(lang, "The text is the same.", "Текст не изменился.")), nil
		}
		return nil, nil
	}
	lines, err := s.Context(sheet)
	if err != nil {
		return nil, err
	}
	lines = append(lines, "", "Changed lines:", changed)
	chatID := sheet.ChatID
	raw, err := s.LLM.JSON(ctx, llm.Request{Task: "pdf", System: s.prompt(ChangesSystem),
		Prompt: strings.Join(lines, "\n"), Schema: ChangesSchema, MaxTokens: MaxTokens, ChatID: &chatID})
	if err != nil {
		if s.llmFailure(err, "describing the changes of the sheet", sheet.ID) {
			return nil, nil
		}
		return nil, err
	}
	s.setError("")
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	return nullable(pytext.Cut(pytext.Strip(orEmpty(answer["changes"])), ChangesChars)), nil
}

// prompt is a system prompt asking for the app's language.
func (s *Sheets) prompt(system string) string {
	lang, _ := s.Settings.String("language")
	words, ok := PromptWords[lang]
	if !ok {
		words = PromptWords["en"]
	}
	return strings.NewReplacer("{language}", words["language"], "{no_intro}", words["no_intro"],
		"{layout_only}", words["layout_only"]).Replace(system)
}

// --- the owner's joining (the web app) -------------------------------------------------------

// Join makes a sheet published as new a newer version of another one: the sheet now. An *i18n.Error when it cannot
// be: a sheet with versions of its own, the same sheet.
func (s *Sheets) Join(ctx context.Context, sheetID, previousID int64) (*Sheet, error) {
	sheet, err := s.Get(sheetID)
	if err != nil {
		return nil, err
	}
	older, err := s.Get(previousID)
	if err != nil {
		return nil, err
	}
	if sheet == nil || older == nil || sheet.Status != "posted" || older.Status != "posted" {
		return nil, i18n.Err("no such sheet", "такого листка нет")
	}
	last, err := latest(s.db, sheetID)
	if err != nil {
		return nil, err
	}
	if sheet.Previous != nil || !last {
		return nil, i18n.Err("this sheet has versions already: only a sheet without them can be joined",
			"у этого листка уже есть версии: присоединить можно только листок без них")
	}
	first, err := s.First(older)
	if err != nil {
		return nil, err
	}
	chain, err := s.Versions(first)
	if err != nil {
		return nil, err
	}
	older = chain[len(chain)-1] // the newest version of the other sheet
	if first, err = s.First(older); err != nil {
		return nil, err
	}
	if first.ID == sheetID {
		return nil, i18n.Err("it is the same sheet", "это тот же листок")
	}
	var changes *string
	chat, err := s.ChatSettings.All(sheet.ChatID)
	if err != nil {
		return nil, err
	}
	if chat.PDFContent {
		if changes, err = s.describe(ctx, sheet, text(older.Text), text(sheet.Text)); err != nil {
			return nil, err
		}
	}
	next, err := s.nextFor(sheet.Course, sheet.SentAt)
	if err != nil {
		return nil, err
	}
	var touched map[int64]bool
	err = s.store.InTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("update sheets set previous = ?, version = ?, changes = ? where id = ?",
			older.ID, older.Version+1, changes, sheetID); err != nil {
			return err
		}
		if err := s.inherit(tx, older, sheet); err != nil {
			return err
		}
		now, err := get(tx, sheetID)
		if err != nil {
			return err
		}
		touched, err = s.follow(tx, now, &schedule.MidtermChanges{}, next)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.changed(touched)
	return s.Get(sheetID)
}

// Listed is a sheet of the Листочки screen.
type Listed struct {
	ID         int64   `json:"id"`
	First      int64   `json:"first"`
	Versions   int     `json:"versions"`
	Status     string  `json:"status"`
	Error      *string `json:"error"`
	Course     *string `json:"course"`
	Title      *string `json:"title"`
	Number     string  `json:"number"`
	Name       string  `json:"name"`
	Due        *string `json:"due"`
	DueAssumed bool    `json:"due_assumed"`
	DueAt      *string `json:"due_at"`
	Chat       *string `json:"chat"`
	Own        bool    `json:"own"`
	CreatedAt  string  `json:"created_at"`
	Caption    *string `json:"caption"`
	UpdatedAt  string  `json:"updated_at"`
	Summary    *string `json:"summary"`
	File       bool    `json:"file"`
}

// Listing is the Листочки screen: every sheet with its versions, the newest first.
func (s *Sheets) Listing() ([]Listed, error) {
	firsts, err := list(s.db, "select "+columns+" from sheets where previous is null order by id desc limit 200")
	if err != nil {
		return nil, err
	}
	listed := []Listed{}
	for _, first := range firsts {
		chain := []*Sheet{first}
		if first.Status == "posted" {
			if chain, err = s.Versions(first); err != nil {
				return nil, err
			}
		}
		last := chain[len(chain)-1]
		var chatTitle *string
		if Origin(first) == "mail" {
			subject, err := s.Inbox.Subject([]int64{first.MsgID})
			if err != nil {
				return nil, err
			}
			title := "mail"
			if subject != nil && *subject != "" {
				title = *subject
			}
			chatTitle = ptr("✉ " + title)
		} else {
			chat, err := s.Inbox.Chat(first.ChatID)
			if err != nil {
				return nil, err
			}
			if chat != nil {
				chatTitle = &chat.Title
			}
		}
		var title *string
		if last.CourseName() != "" {
			t, err := s.Subjects.Title(last.CourseName())
			if err != nil {
				return nil, err
			}
			title = &t
		}
		listed = append(listed, Listed{ID: last.ID, First: first.ID, Versions: len(chain), Status: first.Status,
			Error: first.Error, Course: last.Course, Title: title, Number: last.Number, Name: last.Name, Due: last.Due,
			DueAssumed: last.DueAssumed != 0, DueAt: last.DueAt, Chat: chatTitle, Own: first.ChatID == files.OwnChat,
			CreatedAt: first.CreatedAt, Caption: first.Caption, UpdatedAt: last.CreatedAt, Summary: first.Summary,
			File: s.Files.ByHash(last.Sha256) != ""})
	}
	return listed, nil
}
