// Package todo keeps the own and hidden deadlines (§5.4, D8): the port of Tablo's todo.py, the side HA's to-do lists
// see.
//
// HA shows every deadline of the merged list in two to-do lists, "Tablo · Study" and "Tablo · Personal", and the web
// app and Claude act the same way:
//   - an item added there is the owner's own deadline (`own_deadlines`, uid `ha-<id>`);
//   - ticking off or deleting an own one deletes it; one from a source is hidden for good (`hidden_deadlines`), even
//     when the source sends it again: that is how Haiku's mistakes go away. The row it came from is kept too, so a
//     rename of that row (a Midterm named by the dictionary, an HSE App X label once Haiku answers) does not bring it
//     back. The web app also keeps why (`reason`): done, not mine, or a duplicate of another (`kept`); HA, Tablo and
//     the merge see every reason the same, as hidden;
//   - editing one from a source hides it and keeps an own copy with the new title, date or list; editing an own one
//     changes it.
//
// Rows 30 days past their date are removed at every change of the lists.
//
// Writing methods do not commit: call them on a Todo bound to the caller's transaction (With); processing/merge does.
package todo

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Categories are the two lists.
var Categories = []string{"study", "personal"}

// Reasons are why the owner hid a deadline (the web app).
var Reasons = []string{"done", "not_mine", "duplicate"}

// KeepDays: past that, an own or a hidden deadline is off every list anyway.
const KeepDays = 30

// OwnPrefix starts the key of an own deadline.
const OwnPrefix = "ha-"

// ValueError is a value the lists refuse (Python's ValueError).
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Deadline is one deadline of the merge (§2.3): Tablo's model plus what finds it again.
type Deadline struct {
	Title    string
	Due      time.Time // a date: midnight UTC of the day (Date)
	Category string    // study, personal
	Source   string    // hseapp, ical, mail, tg, ha
	Course   *string   // the abbr from the dictionary: the merge compares by it (§5.5)
	Key      string    // own ones: "ha-<id>", their uid; the others are keyed by content
	// Ref is which row it is, through renames: dl:<deadlines.id>, hseapp:<id>, ha-<id>. The push of news diffs the
	// list by it.
	Ref     string
	Name    *string // the course as the source names it, for showing
	Assumed bool    // a sheet's date the service assumed (§6.3)
}

// Ident is what Python's dataclass equality compares: title, due, category, source and course.
type Ident struct {
	Title, Due, Category, Source string
	Course                       string
	HasCourse                    bool
}

// Ident is the deadline's equality key.
func (d Deadline) Ident() Ident {
	id := Ident{Title: d.Title, Due: DueText(d.Due), Category: d.Category, Source: d.Source}
	if d.Course != nil {
		id.Course, id.HasCourse = *d.Course, true
	}
	return id
}

// Equal is Python's ==.
func (d Deadline) Equal(other Deadline) bool { return d.Ident() == other.Ident() }

// Date is a calendar date as Deadline.Due holds it.
func Date(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// DateOf is the date of a moment as its location reads it (clock.LocalDay of a moment gives the Moscow one).
func DateOf(t time.Time) time.Time { return Date(t.Year(), t.Month(), t.Day()) }

// ParseDate reads YYYY-MM-DD.
func ParseDate(text string) (time.Time, error) { return time.Parse(clock.DateLayout, text) }

// DueText is YYYY-MM-DD.
func DueText(due time.Time) string { return due.Format(clock.DateLayout) }

// UID is the stable id of a to-do item: own deadlines by row, the others by source, title and date.
func UID(d Deadline) string {
	if d.Key != "" {
		return d.Key
	}
	raw := d.Source + "|" + strings.Join(pytext.Split(pytext.Casefold(d.Title)), " ") + "|" + DueText(d.Due)
	sum := sha1.Sum([]byte(raw))
	return d.Source + "-" + hex.EncodeToString(sum[:])[:10]
}

// Check is the title of a new or edited deadline, squeezed; an error for an empty title or an unknown category.
func Check(title string, category string) (string, error) {
	title = strings.Join(pytext.Split(title), " ")
	if title == "" {
		return "", &ValueError{"title is empty"}
	}
	if !slices.Contains(Categories, category) {
		return "", &ValueError{"category must be one of " + strings.Join(Categories, ", ")}
	}
	return title, nil
}

// OwnID is the row id of an own deadline.
func OwnID(d Deadline) int64 {
	id, _ := strconv.ParseInt(strings.TrimPrefix(d.Key, OwnPrefix), 10, 64)
	return id
}

// Querier is what *sql.DB and *sql.Tx both do.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Todo is the two tables.
type Todo struct{ db Querier }

// Open gives the lists of the store.
func Open(s *store.Store) *Todo { return &Todo{db: s.DB} }

// With is the same lists over a transaction.
func (t *Todo) With(q Querier) *Todo { return &Todo{db: q} }

// Own are the owner's deadlines, by date.
func (t *Todo) Own() ([]Deadline, error) {
	rows, err := t.db.Query("select id, title, due, category, course from own_deadlines order by due, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deadline{}
	for rows.Next() {
		var id int64
		var title, due, category string
		var course *string
		if err := rows.Scan(&id, &title, &due, &category, &course); err != nil {
			return nil, err
		}
		day, err := ParseDate(due)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s%d", OwnPrefix, id)
		out = append(out, Deadline{Title: title, Due: day, Category: category, Source: "ha", Course: course, Key: key,
			Ref: key})
	}
	return out, rows.Err()
}

// Hidden are the deadlines of the sources hidden for good.
func (t *Todo) Hidden() ([]Deadline, error) {
	rows, err := t.db.Query("select title, due, source, course, ref from hidden_deadlines")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deadline{}
	for rows.Next() {
		var title, due, source string
		var course, ref *string
		if err := rows.Scan(&title, &due, &source, &course, &ref); err != nil {
			return nil, err
		}
		day, err := ParseDate(due)
		if err != nil {
			return nil, err
		}
		d := Deadline{Title: title, Due: day, Category: "study", Source: source, Course: course}
		if ref != nil {
			d.Ref = *ref
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// HiddenRow is a row of `hidden_deadlines`.
type HiddenRow struct {
	UID    string  `json:"uid"`
	Source string  `json:"source"`
	Course *string `json:"course"`
	Title  string  `json:"title"`
	Due    string  `json:"due"`
	Ref    *string `json:"ref"`
	Reason *string `json:"reason"`
	Kept   *string `json:"kept"`
}

// HiddenRows are the hidden rows for the web app: by date, the newest first.
func (t *Todo) HiddenRows() ([]HiddenRow, error) {
	rows, err := t.db.Query("select uid, source, course, title, due, ref, reason, kept from hidden_deadlines " +
		"order by due desc, title")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HiddenRow{}
	for rows.Next() {
		var r HiddenRow
		if err := rows.Scan(&r.UID, &r.Source, &r.Course, &r.Title, &r.Due, &r.Ref, &r.Reason, &r.Kept); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Add adds an own deadline. `course` is its key (the abbr), as the web app gives it; HA and the web app give "".
func (t *Todo) Add(title string, due time.Time, category string, course string) (Deadline, error) {
	title, err := Check(title, category)
	if err != nil {
		return Deadline{}, err
	}
	var courseValue *string
	if squeezed := strings.Join(pytext.Split(course), " "); squeezed != "" {
		courseValue = &squeezed
	}
	result, err := t.db.Exec("insert into own_deadlines(title, due, category, course) values(?, ?, ?, ?)",
		title, DueText(due), category, courseValue)
	if err != nil {
		return Deadline{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Deadline{}, err
	}
	key := fmt.Sprintf("%s%d", OwnPrefix, id)
	return Deadline{Title: title, Due: DateOf(due), Category: category, Source: "ha", Course: courseValue, Key: key,
		Ref: key}, nil
}

// Remove: deleted or done, an own deadline goes away and one from a source is hidden, with the reason when given
// ("" for none). `kept` is the uid a duplicate was merged into.
func (t *Todo) Remove(d Deadline, reason, kept string) error {
	if reason != "" && !slices.Contains(Reasons, reason) {
		return &ValueError{"reason must be one of " + strings.Join(Reasons, ", ")}
	}
	if d.Source == "ha" {
		_, err := t.db.Exec("delete from own_deadlines where id = ?", OwnID(d))
		return err
	}
	var reasonValue, keptValue, ref any
	if reason != "" {
		reasonValue = reason
	}
	if reason == "duplicate" && kept != "" {
		keptValue = kept
	}
	if d.Ref != "" {
		ref = d.Ref
	}
	_, err := t.db.Exec("insert or ignore into hidden_deadlines(uid, source, course, title, due, ref, reason, kept) "+
		"values(?, ?, ?, ?, ?, ?, ?, ?)", UID(d), d.Source, d.Course, d.Title, DueText(d.Due), ref, reasonValue, keptValue)
	return err
}

// Edit changes a deadline: title nil keeps it, a zero due keeps it, category "" keeps it. One from a source is hidden
// and an own copy takes its place.
func (t *Todo) Edit(d Deadline, title *string, due time.Time, category string) (Deadline, error) {
	if due.IsZero() {
		due = d.Due
	}
	if category == "" {
		category = d.Category
	}
	newTitle := d.Title
	if title != nil {
		newTitle = *title
	}
	newTitle, err := Check(newTitle, category)
	if err != nil {
		return Deadline{}, err
	}
	if d.Source != "ha" {
		if err := t.Remove(d, "", ""); err != nil {
			return Deadline{}, err
		}
		course := ""
		if d.Course != nil {
			course = *d.Course
		}
		return t.Add(newTitle, due, category, course) // the copy keeps the course: it stays in its lens
	}
	if _, err := t.db.Exec("update own_deadlines set title = ?, due = ?, category = ? where id = ?",
		newTitle, DueText(due), category, OwnID(d)); err != nil {
		return Deadline{}, err
	}
	return Deadline{Title: newTitle, Due: DateOf(due), Category: category, Source: "ha", Course: d.Course, Key: d.Key,
		Ref: d.Key}, nil
}

// Unhide is «Вернуть» in the web app: the source's deadline is back unless another hidden row matches it.
func (t *Todo) Unhide(uid string) (bool, error) {
	result, err := t.db.Exec("delete from hidden_deadlines where uid = ?", uid)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// Prune removes the rows KeepDays past their date.
func (t *Todo) Prune(today time.Time) error {
	since := DueText(DateOf(today).AddDate(0, 0, -KeepDays))
	if _, err := t.db.Exec("delete from hidden_deadlines where due < ?", since); err != nil {
		return err
	}
	_, err := t.db.Exec("delete from own_deadlines where due < ?", since)
	return err
}
