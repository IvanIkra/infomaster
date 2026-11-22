// Package news keeps the parsed news (§6.1, §8.3, D23): one table and one set of rules for the web app, Claude and
// Tablo.
//
// A live item is neither replaced nor gone nor hidden by the owner and is younger than `news_ttl_hours`. Rank is
// `importance + study`, the newer item first among equals. Hiding passes to the item that replaces a hidden one, so an
// edit of its message keeps it hidden (the model never sees hidden items, so no other item replaces one). Writing
// methods do not commit: call them on a News bound to the caller's transaction (With).
package news

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Querier is what *sql.DB and *sql.Tx both do.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Item is a row of `news`.
type Item struct {
	ID         int64   `json:"id"`
	Source     string  `json:"source"`
	ChatID     int64   `json:"chat_id"`
	MsgIDs     []int64 `json:"msg_ids"`
	Study      bool    `json:"study"`
	PostedAt   string  `json:"posted_at"`
	Full       string  `json:"full"`
	Short      string  `json:"short"`
	Importance int64   `json:"importance"`
	ReplacedBy *int64  `json:"replaced_by"`
	GoneAt     *string `json:"gone_at"`
	Hidden     bool    `json:"hidden"`
	CreatedAt  string  `json:"created_at"`
}

// New is an item to add: PostedAt is local ISO.
type New struct {
	Source     string
	ChatID     int64
	MsgIDs     []int64
	Study      bool
	PostedAt   string
	Full       string
	Short      string
	Importance int64
}

const columns = "id, source, chat_id, msg_ids, study, posted_at, full, short, importance, replaced_by, gone_at, " +
	"hidden, created_at"

// News is the table.
type News struct{ db Querier }

// Open gives the news of the store.
func Open(s *store.Store) *News { return &News{db: s.DB} }

// With is the same table over a transaction.
func (n *News) With(q Querier) *News { return &News{db: q} }

func scan(scan func(...any) error) (*Item, error) {
	var item Item
	var ids string
	if err := scan(&item.ID, &item.Source, &item.ChatID, &ids, &item.Study, &item.PostedAt, &item.Full, &item.Short,
		&item.Importance, &item.ReplacedBy, &item.GoneAt, &item.Hidden, &item.CreatedAt); err != nil {
		return nil, err
	}
	var err error
	item.MsgIDs, err = deadlines.DecodeIDs(&ids)
	return &item, err
}

func (n *News) list(query string, args ...any) ([]*Item, error) {
	rows, err := n.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Item{}
	for rows.Next() {
		item, err := scan(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Get is one item; nil when there is none.
func (n *News) Get(id int64) (*Item, error) {
	item, err := scan(n.db.QueryRow("select "+columns+" from news where id = ?", id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return item, err
}

// Live are the live items, in rank order.
func (n *News) Live(now time.Time, ttlHours int) ([]*Item, error) {
	return n.list("select "+columns+" from news where replaced_by is null and gone_at is null and not hidden "+
		"and posted_at >= ? order by importance + study desc, posted_at desc, id desc",
		clock.LocalISO(now.Add(-time.Duration(ttlHours)*time.Hour)))
}

// MadeOf are the items in force that one of the message's versions gave.
func (n *News) MadeOf(source string, chatID, msgID int64) ([]*Item, error) {
	return n.list("select "+columns+" from news where source = ? and chat_id = ? and replaced_by is null "+
		"and gone_at is null and exists (select 1 from json_each(news.msg_ids) where value = ?)", source, chatID, msgID)
}

// InForce are a chat's items in force made since then.
func (n *News) InForce(source string, chatID int64, since time.Time) ([]*Item, error) {
	return n.list("select "+columns+" from news where source = ? and chat_id = ? and replaced_by is null "+
		"and gone_at is null and created_at >= ?", source, chatID, clock.LocalISO(since))
}

// Add adds an item; its id.
func (n *News) Add(item New, at time.Time) (int64, error) {
	result, err := n.db.Exec("insert into news(source, chat_id, msg_ids, study, posted_at, full, short, importance, "+
		"created_at) values(?, ?, ?, ?, ?, ?, ?, ?, ?)", item.Source, item.ChatID, deadlines.EncodeIDs(item.MsgIDs),
		item.Study, item.PostedAt, item.Full, item.Short, item.Importance, clock.LocalISO(at))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// Replace points an item in force at the newer one that tells the same story; a hidden one stays hidden in it.
func (n *News) Replace(oldID, newID int64) error {
	result, err := n.db.Exec("update news set replaced_by = ? where id = ? and replaced_by is null and gone_at is null",
		newID, oldID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count > 0 {
		_, err = n.db.Exec("update news set hidden = 1 where id = ? and exists "+
			"(select 1 from news where id = ? and hidden)", newID, oldID)
	}
	return err
}

// Hide is the owner's «Скрыть» under an alert: off the list and the API for good.
func (n *News) Hide(id int64) (bool, error) {
	result, err := n.db.Exec("update news set hidden = 1 where id = ? and not hidden", id)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

// Unhide is the undo of a hide: the item and the ones that replaced it since are shown again.
func (n *News) Unhide(id int64) (bool, error) {
	found := false
	current := &id
	for current != nil {
		result, err := n.db.Exec("update news set hidden = 0 where id = ? and hidden", *current)
		if err != nil {
			return false, err
		}
		if count, _ := result.RowsAffected(); count > 0 {
			found = true
		}
		var next *int64
		err = n.db.QueryRow("select replaced_by from news where id = ?", *current).Scan(&next)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		current = next
	}
	return found, nil
}

// Drop withdraws items.
func (n *News) Drop(ids []int64, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{clock.LocalISO(at)}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := n.db.Exec("update news set gone_at = ? where gone_at is null and id in ("+marks(len(ids))+")", args...)
	return err
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }
