// Package deadlines keeps the deadlines from chats and mail (§6.4) and the Midterms of RUZ (source "ical", §4.4).
// HSE App X keeps its lists in kv (processing/hseapp), own and hidden ones have their tables (processing/todo);
// processing/merge makes one list of them all (§5.5).
//
// A deadline is in force until a later one moves it (`moved_by`) or its messages are deleted or edited into something
// else (`gone_at`); a Midterm follows its class. Only deadlines from chats and mail are shown to the batch model and
// moved by it: the others are moved by their own source (§6.5). A sheet without a due date gets one assumed, the next
// seminar of its course (`assumed`, processing/sheets): a real date moves it. Writing methods do not commit: call
// them on a Deadlines bound to the caller's transaction (With).
package deadlines

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Lookback: past deadlines still merged and named by the dictionary (§6.4, §5.3).
const Lookback = 3 * 24 * time.Hour

var lower = cases.Lower(language.Und)

// SameLabel tells two labels of one work apart from their spacing and case (Python's str.lower).
func SameLabel(a, b string) bool { return normalize(a) == normalize(b) }

func normalize(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if pytext.IsSpace(r) {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	if space {
		b.WriteByte(' ')
	}
	return lower.String(pytext.Strip(b.String()))
}

// Querier is what *sql.DB and *sql.Tx both do.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Deadline is a row of `deadlines`.
type Deadline struct {
	ID        int64   `json:"id"`
	Source    string  `json:"source"`
	ChatID    *int64  `json:"chat_id"`
	MsgIDs    []int64 `json:"msg_ids"`
	Course    *string `json:"course"`
	Label     string  `json:"label"`
	Due       string  `json:"due"`
	MovedBy   *int64  `json:"moved_by"`
	GoneAt    *string `json:"gone_at"`
	CreatedAt string  `json:"created_at"`
	SheetID   *int64  `json:"sheet_id"`
	Assumed   bool    `json:"assumed"`
}

// CourseName is the course, "" for none.
func (d *Deadline) CourseName() string {
	if d.Course == nil {
		return ""
	}
	return *d.Course
}

// New is a deadline to add: Course "" is none; SheetID for one read in a sheet's pages or assumed for it (Assumed).
type New struct {
	Source  string
	ChatID  *int64
	MsgIDs  []int64
	Course  string
	Label   string
	Due     string
	SheetID *int64
	Assumed bool
}

const columns = "id, source, chat_id, msg_ids, course, label, due, moved_by, gone_at, created_at, sheet_id, assumed"

// Deadlines is the table.
type Deadlines struct{ db Querier }

// Open gives the deadlines of the store.
func Open(s *store.Store) *Deadlines { return &Deadlines{db: s.DB} }

// With is the same table over a transaction.
func (d *Deadlines) With(q Querier) *Deadlines { return &Deadlines{db: q} }

func scan(scan func(...any) error) (*Deadline, error) {
	var row Deadline
	var ids *string
	if err := scan(&row.ID, &row.Source, &row.ChatID, &ids, &row.Course, &row.Label, &row.Due, &row.MovedBy,
		&row.GoneAt, &row.CreatedAt, &row.SheetID, &row.Assumed); err != nil {
		return nil, err
	}
	var err error
	row.MsgIDs, err = DecodeIDs(ids)
	return &row, err
}

// DecodeIDs reads a msg_ids column; NULL or "" is [].
func DecodeIDs(text *string) ([]int64, error) {
	ids := []int64{}
	if text == nil || *text == "" {
		return ids, nil
	}
	err := json.Unmarshal([]byte(*text), &ids)
	if ids == nil {
		ids = []int64{}
	}
	return ids, err
}

// EncodeIDs is json.dumps of msg ids: [101, 102].
func EncodeIDs(ids []int64) string {
	if ids == nil {
		ids = []int64{}
	}
	text, err := pyjson.DumpsASCII(ids)
	if err != nil {
		panic(err)
	}
	return text
}

func (d *Deadlines) one(query string, args ...any) (*Deadline, error) {
	row, err := scan(d.db.QueryRow(query, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

func (d *Deadlines) list(query string, args ...any) ([]*Deadline, error) {
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Deadline{}
	for rows.Next() {
		row, err := scan(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func day(t time.Time) string { return t.Format(clock.DateLayout) }

// Get is one deadline; nil when there is none.
func (d *Deadlines) Get(id int64) (*Deadline, error) {
	return d.one("select "+columns+" from deadlines where id = ?", id)
}

// Upcoming are those from chats and mail, in force and due `lookback` ago or later, by date. `today` is a Moscow
// date (its year, month and day are read).
func (d *Deadlines) Upcoming(today time.Time, lookback time.Duration) ([]*Deadline, error) {
	return d.list("select "+columns+" from deadlines where moved_by is null and gone_at is null and due >= ? "+
		"and source in ('tg', 'mail') order by due, id", day(dateOf(today).Add(-lookback)))
}

// dateOf is a date's midnight in UTC, so whole days subtract exactly.
func dateOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Listed is every source of the table, in force and due on `since` or later, by date: the merge reads them (§5.5).
func (d *Deadlines) Listed(since time.Time) ([]*Deadline, error) {
	return d.list("select "+columns+" from deadlines where moved_by is null and gone_at is null and due >= ? "+
		"order by due, id", day(since))
}

// Latest is the newest version of a deadline: the one its moves (`moved_by`) lead to.
func (d *Deadlines) Latest(id int64) (*Deadline, error) {
	row, err := d.Get(id)
	seen := map[int64]bool{}
	for err == nil && row != nil && row.MovedBy != nil && !seen[row.ID] {
		seen[row.ID] = true
		var newer *Deadline
		newer, err = d.Get(*row.MovedBy)
		if newer == nil {
			break
		}
		row = newer
	}
	return row, err
}

// Find is one in force from chats or mail with the same work and date.
func (d *Deadlines) Find(label, due string) (*Deadline, error) {
	rows, err := d.list("select "+columns+" from deadlines where moved_by is null and gone_at is null and due = ? "+
		"and source in ('tg', 'mail')", due)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if SameLabel(row.Label, label) {
			return row, nil
		}
	}
	return nil, nil
}

// CourseNames are the courses of deadlines in force that are not long past: the dictionary names them (§6.2).
func (d *Deadlines) CourseNames(today time.Time) (map[string]bool, error) {
	rows, err := d.db.Query("select distinct course from deadlines where moved_by is null and gone_at is null "+
		"and course is not null and due >= ?", day(dateOf(today).Add(-Lookback)))
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

// MadeOf are those in force from the message's text; those read in the pages of its sheet are the sheet's, not the
// text's.
func (d *Deadlines) MadeOf(source string, chatID, msgID int64) ([]*Deadline, error) {
	return d.list("select "+columns+" from deadlines where source = ? and chat_id = ? and moved_by is null "+
		"and gone_at is null and sheet_id is null and exists (select 1 from json_each(deadlines.msg_ids) "+
		"where value = ?)", source, chatID, msgID)
}

// InForce are a chat's deadlines in force made since then.
func (d *Deadlines) InForce(source string, chatID int64, since time.Time) ([]*Deadline, error) {
	return d.list("select "+columns+" from deadlines where source = ? and chat_id = ? and moved_by is null "+
		"and gone_at is null and created_at >= ?", source, chatID, clock.LocalISO(since))
}

// Add adds a deadline; its id.
func (d *Deadlines) Add(deadline New, at time.Time) (int64, error) {
	var course any
	if deadline.Course != "" {
		course = deadline.Course
	}
	result, err := d.db.Exec("insert into deadlines(source, chat_id, msg_ids, course, label, due, created_at, sheet_id, "+
		"assumed) values(?, ?, ?, ?, ?, ?, ?, ?, ?)", deadline.Source, deadline.ChatID, EncodeIDs(deadline.MsgIDs),
		course, deadline.Label, deadline.Due, clock.LocalISO(at), deadline.SheetID, deadline.Assumed)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// OfSheet is every row of a sheet, read in its pages or assumed for it, in force or not, oldest first.
func (d *Deadlines) OfSheet(sheetID int64) ([]*Deadline, error) {
	return d.list("select "+columns+" from deadlines where sheet_id = ? order by id", sheetID)
}

// Confirm: an assumed date told for real (the same work on the same date) is no longer assumed.
func (d *Deadlines) Confirm(id int64) error {
	_, err := d.db.Exec("update deadlines set assumed = 0 where id = ?", id)
	return err
}

// Move points an old deadline in force at the new one; false when it was not in force.
func (d *Deadlines) Move(oldID, newID int64) (bool, error) {
	result, err := d.db.Exec("update deadlines set moved_by = ? where id = ? and moved_by is null "+
		"and gone_at is null", newID, oldID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// Rename gives a deadline a new label and course (course nil: none).
func (d *Deadlines) Rename(id int64, label string, course *string) error {
	_, err := d.db.Exec("update deadlines set label = ?, course = ? where id = ?", label, course, id)
	return err
}

// Drop withdraws deadlines; for one in force, the deadline it had moved comes back (as Events.Drop does), past an
// earlier text of the same messages and past ones withdrawn already. It returns the ids that came back.
func (d *Deadlines) Drop(ids []int64, at time.Time) ([]int64, error) {
	restored := []int64{}
	for _, id := range ids {
		row, err := d.Get(id)
		if err != nil {
			return nil, err
		}
		if row == nil || row.GoneAt != nil {
			continue
		}
		if _, err := d.db.Exec("update deadlines set gone_at = ? where id = ?", clock.LocalISO(at), id); err != nil {
			return nil, err
		}
		if row.MovedBy != nil || row.SheetID != nil {
			continue // a sheet's date follows its versions (§6.3): the sheets bring back what they keep
		}
		told := map[int64]bool{}
		for _, msg := range row.MsgIDs {
			told[msg] = true
		}
		earlier, err := d.one("select "+columns+" from deadlines where moved_by = ?", id)
		for err == nil && earlier != nil && (earlier.GoneAt != nil || subset(earlier.MsgIDs, told)) {
			earlier, err = d.one("select "+columns+" from deadlines where moved_by = ?", earlier.ID)
		}
		if err != nil {
			return nil, err
		}
		if earlier != nil {
			if _, err := d.db.Exec("update deadlines set moved_by = null where id = ?", earlier.ID); err != nil {
				return nil, err
			}
			restored = append(restored, earlier.ID)
		}
	}
	return restored, nil
}

func subset(ids []int64, of map[int64]bool) bool {
	for _, id := range ids {
		if !of[id] {
			return false
		}
	}
	return true
}
