// Package events keeps the events from chats and the chat corrections of RUZ classes (§6.1, §6.5, D28).
//
// An event of its own is a consultation, a talk, a meeting: something to attend that is not in the timetable. A
// correction is a chat message that moves or cancels a RUZ class: it carries the class's `class_uid` and the class's
// UID, so the feed shows the class itself moved or cancelled.
//
// Versions: a move, a cancellation or an edit of the message adds a new row with the same `uid` and points the old one
// at it (`replaced_by`); the newest one is in force. Withdrawing a version (its messages deleted or edited into
// something else) brings back the newest version before it that is not withdrawn itself, if other messages told that
// one; a superseded version whose messages are deleted is withdrawn too, so it never comes back. The owner's choices
// (`required` once `chosen`, `hidden`) pass to every newer version.
//
// The owner's own events and corrections come from the web app (source `own`, D44). An own correction of a class wins
// over the chats' corrections of it; the chats' versions go on under it, so they apply again once it is undone. The
// batches see the own events, marked, so a chat does not tell them again, but never move them, nor does a chat's
// message supersede them.
//
// Writing methods of the processing do not commit: call them on an Events bound to the caller's transaction (With).
// The owner's choices commit themselves.
package events

import (
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// InForce is the SQL condition of a version in force.
const InForce = "replaced_by is null and gone_at is null"

// Own is the source of the owner's own events and corrections.
const Own = "own"

// UID is the stable UID of an event told by a message (§6.5): a hash of the chat and the message; `n` tells apart
// several events of one message.
func UID(source string, chatID, msgID int64, n int) string {
	key := fmt.Sprintf("%d:%d", chatID, msgID)
	if n != 0 {
		key += fmt.Sprintf(":%d", n)
	}
	sum := sha1.Sum([]byte(key))
	return source + "-" + hex.EncodeToString(sum[:])[:16] + "@infomaster"
}

// OwnUID is the UID of an own event or correction: no message to hash, it is the same through its versions.
func OwnUID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return Own + "-" + hex.EncodeToString(b[:]) + "@infomaster"
}

// Querier is what *sql.DB and *sql.Tx both do.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Event is a row of `events`.
type Event struct {
	ID         int64   `json:"id"`
	UID        string  `json:"uid"`
	Source     string  `json:"source"`
	ChatID     *int64  `json:"chat_id"`
	MsgIDs     []int64 `json:"msg_ids"`
	ClassUID   *string `json:"class_uid"`
	Course     *string `json:"course"`
	Title      string  `json:"title"`
	Kind       string  `json:"kind"`
	Start      string  `json:"start"`
	End        *string `json:"end"`
	Room       string  `json:"room"`
	Required   bool    `json:"required"`
	Chosen     bool    `json:"chosen"`
	Hidden     bool    `json:"hidden"`
	Cancelled  bool    `json:"cancelled"`
	CopyOf     *string `json:"copy_of"`
	ReplacedBy *int64  `json:"replaced_by"`
	GoneAt     *string `json:"gone_at"`
	CreatedAt  string  `json:"created_at"`
}

// EndText is the end, "" for none.
func (e *Event) EndText() string {
	if e.End == nil {
		return ""
	}
	return *e.End
}

// CourseName is the course, "" for none.
func (e *Event) CourseName() string {
	if e.Course == nil {
		return ""
	}
	return *e.Course
}

// New is an event to add. Course "" and End "" are null; ClassUID and CopyOf "" are null.
type New struct {
	UID       string
	Source    string
	ChatID    *int64
	MsgIDs    []int64
	ClassUID  string
	Course    string
	Title     string
	Kind      string
	Start     string
	End       string
	Room      string
	Required  bool
	Chosen    bool
	Hidden    bool
	Cancelled bool
	CopyOf    string
}

const columns = "id, uid, source, chat_id, msg_ids, class_uid, course, title, kind, start, end, room, required, " +
	"chosen, hidden, cancelled, copy_of, replaced_by, gone_at, created_at"

// Events is the table.
type Events struct{ db Querier }

// Open gives the events of the store.
func Open(s *store.Store) *Events { return &Events{db: s.DB} }

// With is the same table over a transaction.
func (e *Events) With(q Querier) *Events { return &Events{db: q} }

// DB is what the table runs on.
func (e *Events) DB() Querier { return e.db }

func scan(scan func(...any) error) (*Event, error) {
	var ev Event
	var ids string
	if err := scan(&ev.ID, &ev.UID, &ev.Source, &ev.ChatID, &ids, &ev.ClassUID, &ev.Course, &ev.Title, &ev.Kind,
		&ev.Start, &ev.End, &ev.Room, &ev.Required, &ev.Chosen, &ev.Hidden, &ev.Cancelled, &ev.CopyOf, &ev.ReplacedBy,
		&ev.GoneAt, &ev.CreatedAt); err != nil {
		return nil, err
	}
	var err error
	ev.MsgIDs, err = deadlines.DecodeIDs(&ids)
	return &ev, err
}

func (e *Events) one(query string, args ...any) (*Event, error) {
	ev, err := scan(e.db.QueryRow(query, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return ev, err
}

func (e *Events) list(query string, args ...any) ([]*Event, error) {
	rows, err := e.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Event{}
	for rows.Next() {
		ev, err := scan(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Get is one version; nil when there is none.
func (e *Events) Get(id int64) (*Event, error) {
	return e.one("select "+columns+" from events where id = ?", id)
}

// Current is the newest version of an event: a button under an old post acts on it.
func (e *Events) Current(id int64) (*Event, error) {
	ev, err := e.Get(id)
	seen := map[int64]bool{}
	for err == nil && ev != nil && ev.ReplacedBy != nil && !seen[ev.ID] {
		seen[ev.ID] = true
		ev, err = e.Get(*ev.ReplacedBy)
	}
	return ev, err
}

// Previous is the version a version replaced.
func (e *Events) Previous(id int64) (*Event, error) {
	return e.one("select "+columns+" from events where replaced_by = ? order by id desc", id)
}

// InForceAll is every version in force, by start.
func (e *Events) InForceAll() ([]*Event, error) {
	return e.list("select " + columns + " from events where " + InForce + " order by start, id")
}

// Upcoming are the events of their own in force from today on, by time, the owner's own too: the batches show them to
// the model, so a chat does not repeat them.
func (e *Events) Upcoming(today time.Time) ([]*Event, error) {
	return e.list("select "+columns+" from events where "+InForce+" and class_uid is null and start >= ? "+
		"order by start, id", today.Format(clock.DateLayout))
}

// Corrections is the correction in force of each corrected class, by its uid: the owner's own one, else the chats'
// newest.
func (e *Events) Corrections() (map[string]*Event, error) {
	rows, err := e.list("select "+columns+" from events where "+InForce+" and class_uid is not null "+
		"order by source = ?, id", Own)
	if err != nil {
		return nil, err
	}
	out := map[string]*Event{}
	for _, ev := range rows {
		out[*ev.ClassUID] = ev
	}
	return out, nil
}

// Correction is the chats' correction in force of a class: the one a chat's new message supersedes.
func (e *Events) Correction(classUID string) (*Event, error) {
	return e.one("select "+columns+" from events where "+InForce+" and class_uid = ? and source != ? "+
		"order by id desc", classUID, Own)
}

// OwnCorrection is the owner's correction in force of a class.
func (e *Events) OwnCorrection(classUID string) (*Event, error) {
	return e.one("select "+columns+" from events where "+InForce+" and class_uid = ? and source = ?", classUID, Own)
}

// Find is an event of its own in force with the same title and start, from any chat.
func (e *Events) Find(title, start string) (*Event, error) {
	rows, err := e.list("select "+columns+" from events where "+InForce+" and class_uid is null and start = ?", start)
	if err != nil {
		return nil, err
	}
	for _, ev := range rows {
		if deadlines.SameLabel(ev.Title, title) {
			return ev, nil
		}
	}
	return nil, nil
}

// OnDay are the events of their own in force on a day ('2026-11-05'), all-day ones too.
func (e *Events) OnDay(day string) ([]*Event, error) {
	return e.list("select "+columns+" from events where "+InForce+" and class_uid is null "+
		"and substr(start, 1, 10) = ? order by start, id", day)
}

// ByUID is the version in force of a uid.
func (e *Events) ByUID(uid string) (*Event, error) {
	return e.one("select "+columns+" from events where "+InForce+" and uid = ?", uid)
}

// MadeOf are the versions in force a message told.
func (e *Events) MadeOf(source string, chatID, msgID int64) ([]*Event, error) {
	return e.list("select "+columns+" from events where source = ? and chat_id = ? and "+InForce+
		" and exists (select 1 from json_each(events.msg_ids) where value = ?)", source, chatID, msgID)
}

// Versions are the versions not withdrawn, in force or superseded, by id: those whose messages are deleted are
// withdrawn oldest first, so a superseded one is gone before a newer one could bring it back.
func (e *Events) Versions(source string, chatID int64, since time.Time) ([]*Event, error) {
	return e.list("select "+columns+" from events where source = ? and chat_id = ? and gone_at is null "+
		"and created_at >= ? order by id", source, chatID, clock.LocalISO(since))
}

// CourseNames are the courses of events in force from today on: the dictionary names them (§6.2).
func (e *Events) CourseNames(today time.Time) (map[string]bool, error) {
	rows, err := e.db.Query("select distinct course from events where "+InForce+" and course is not null "+
		"and start >= ?", today.Format(clock.DateLayout))
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

// --- writing ----------------------------------------------------------------------

func null(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// Add adds a version; its id.
func (e *Events) Add(ev New, at time.Time) (int64, error) {
	result, err := e.db.Exec("insert into events(uid, source, chat_id, msg_ids, class_uid, course, title, kind, start, "+
		"end, room, required, chosen, hidden, cancelled, copy_of, created_at) "+
		"values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		ev.UID, ev.Source, ev.ChatID, deadlines.EncodeIDs(ev.MsgIDs), null(ev.ClassUID), null(ev.Course), ev.Title,
		ev.Kind, ev.Start, null(ev.End), ev.Room, ev.Required, ev.Chosen, ev.Hidden, ev.Cancelled, null(ev.CopyOf),
		clock.LocalISO(at))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// Supersede makes the new row the next version of the old one in force; the owner's choices pass to it.
func (e *Events) Supersede(oldID, newID int64) (bool, error) {
	old, err := e.Get(oldID)
	if err != nil || old == nil || old.ReplacedBy != nil || old.GoneAt != nil {
		return false, err
	}
	if _, err := e.db.Exec("update events set replaced_by = ? where id = ?", newID, oldID); err != nil {
		return false, err
	}
	if _, err := e.db.Exec("update events set hidden = ? where id = ?", old.Hidden, newID); err != nil {
		return false, err
	}
	if old.Chosen {
		if _, err := e.db.Exec("update events set required = ?, chosen = 1 where id = ?", old.Required,
			newID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Drop withdraws versions; for one in force, the newest version before it comes back if other messages told it and it
// is not withdrawn itself. It returns the ids of the versions that came back.
func (e *Events) Drop(ids []int64, at time.Time) ([]int64, error) {
	restored := []int64{}
	for _, id := range ids {
		ev, err := e.Get(id)
		if err != nil {
			return nil, err
		}
		if ev == nil || ev.GoneAt != nil {
			continue
		}
		if _, err := e.db.Exec("update events set gone_at = ? where id = ?", clock.LocalISO(at), id); err != nil {
			return nil, err
		}
		if ev.ReplacedBy != nil {
			continue
		}
		// Past an earlier text of the same message, and past versions withdrawn already (their messages deleted while
		// a newer version was in force).
		earlier, err := e.Previous(id)
		for err == nil && earlier != nil && (sameMessages(earlier, ev) || earlier.GoneAt != nil) {
			earlier, err = e.Previous(earlier.ID)
		}
		if err != nil {
			return nil, err
		}
		if earlier != nil {
			if _, err := e.db.Exec("update events set replaced_by = null where id = ?", earlier.ID); err != nil {
				return nil, err
			}
			restored = append(restored, earlier.ID)
		}
	}
	return restored, nil
}

// Withdraw: every version of an own event or correction is gone; none comes back, as with a deleted message.
func (e *Events) Withdraw(uid string, at time.Time) error {
	_, err := e.db.Exec("update events set gone_at = ? where uid = ? and source = ? and gone_at is null",
		clock.LocalISO(at), uid, Own)
	return err
}

// Overrule: RUZ changed the class itself, so it wins (§6.5): every correction of it is gone for good. It returns the
// corrections that were in force.
func (e *Events) Overrule(classUID string, at time.Time) ([]int64, error) {
	rows, err := e.db.Query("select id from events where class_uid = ? and "+InForce, classUID)
	if err != nil {
		return nil, err
	}
	inForce := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		inForce = append(inForce, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, err = e.db.Exec("update events set gone_at = ? where class_uid = ? and gone_at is null", clock.LocalISO(at),
		classUID)
	return inForce, err
}

// --- the owner's choices (the web app, Claude) -----------------------------------------

// SetRequired puts the newest version in the main or the optional calendar; it returns it. A correction stays a
// class: main.
func (e *Events) SetRequired(id int64, required bool) (*Event, error) {
	ev, err := e.Current(id)
	if err != nil || ev == nil || ev.ClassUID != nil {
		return ev, err
	}
	if _, err := e.db.Exec("update events set required = ?, chosen = 1 where id = ?", required, ev.ID); err != nil {
		return nil, err
	}
	return e.Get(ev.ID)
}

// SetHidden hides the newest version from the feeds, or shows it again; it returns it.
func (e *Events) SetHidden(id int64, hidden bool) (*Event, error) {
	ev, err := e.Current(id)
	if err != nil || ev == nil {
		return nil, err
	}
	if _, err := e.db.Exec("update events set hidden = ? where id = ?", hidden, ev.ID); err != nil {
		return nil, err
	}
	return e.Get(ev.ID)
}

func sameMessages(a, b *Event) bool {
	if a.Source != b.Source || !sameChat(a.ChatID, b.ChatID) {
		return false
	}
	for _, x := range a.MsgIDs {
		for _, y := range b.MsgIDs {
			if x == y {
				return true
			}
		}
	}
	return false
}

func sameChat(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
