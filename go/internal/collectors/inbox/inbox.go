// Package inbox keeps the raw chats and messages in SQLite (§9): what the collectors store and the processing reads.
//
// Messages are written once and then only amended: an edit replaces the text and sets `edited_at`, a deletion sets
// `deleted_at`. A message waits for its batch until `parsed_at` is set; one edited after that waits again, and its
// batch replaces what its earlier text gave. A deleted message is never parsed, and what it gave is withdrawn.
//
// Letters of the university mailbox (collectors/mail, §5.2) are messages too, with source "mail": all in one pseudo
// chat, Mailbox (no Telegram chat has id 0), which is no row of `chats`. A letter is never edited or deleted, and its
// subject is kept for the app, where it stands instead of a link.
package inbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Retention (D29): from the message's last edit, or from the message when never edited.
const Retention = 14 * 24 * time.Hour

// Mailbox is the chat_id of the letters.
const Mailbox = 0

// AccountWideIDs: ids of private chats and basic groups are unique across the account, so their deletions come
// without a chat.
var AccountWideIDs = []string{"user", "bot", "group"}

// Pending is the SQL condition of a message waiting for its batch.
const Pending = "deleted_at is null and (parsed_at is null or edited_at > parsed_at)"

// Querier is what *sql.DB and *sql.Tx both do.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func inTx(q Querier, fn func(Querier) error) error {
	db, ok := q.(*sql.DB)
	if !ok {
		return fn(q)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Chat is a row of `chats`.
type Chat struct {
	ID       int64   `json:"id"`
	Title    string  `json:"title"`
	Kind     string  `json:"kind"`
	Username *string `json:"username"`
	LastID   int64   `json:"last_id"`
	AddedAt  string  `json:"added_at"`
}

// ChatStats is a chat with the number of its stored messages and the time of the newest one.
type ChatStats struct {
	Chat
	Messages int64   `json:"messages"`
	LastAt   *string `json:"last_at"`
}

// File is one file of a message as the collectors describe it: {kind, name, mime, size}, keys as they came.
type File = map[string]any

// Message is a row of `messages`, decoded: Files is [] without files, Forward nil without a forward.
type Message struct {
	ID           int64          `json:"id"`
	Source       string         `json:"source"`
	ChatID       int64          `json:"chat_id"`
	MsgID        int64          `json:"msg_id"`
	SenderID     *int64         `json:"sender_id"`
	SenderName   *string        `json:"sender_name"`
	At           string         `json:"at"`
	Text         string         `json:"text"`
	TopicID      *int64         `json:"topic_id"`
	ReplyTo      *int64         `json:"reply_to"`
	ReplyQuote   *string        `json:"reply_quote"`
	Forward      map[string]any `json:"forward"`
	GroupedID    *int64         `json:"grouped_id"`
	Files        []File         `json:"files"`
	Pinned       bool           `json:"pinned"`
	EditedAt     *string        `json:"edited_at"`
	DeletedAt    *string        `json:"deleted_at"`
	ParsedAt     *string        `json:"parsed_at"`
	SummarizedAt *string        `json:"summarized_at"`
	Subject      *string        `json:"subject"`
}

// Fields are what a collector stores of a message; Subject is a letter's.
type Fields struct {
	SenderID   *int64
	SenderName *string
	At         time.Time
	Text       string
	TopicID    *int64
	ReplyTo    *int64
	ReplyQuote *string
	Forward    map[string]any // {from, chat_id, msg_id}; nil for none
	GroupedID  *int64
	Files      []File
	Pinned     bool
	Subject    string
}

const messageColumns = "id, source, chat_id, msg_id, sender_id, sender_name, at, text, topic_id, reply_to, " +
	"reply_quote, forward, grouped_id, files, pinned, edited_at, deleted_at, parsed_at, summarized_at, subject"

const fieldColumns = "sender_id, sender_name, at, text, topic_id, reply_to, reply_quote, forward, grouped_id, files, " +
	"pinned"

// Inbox is the chats and the messages.
type Inbox struct {
	db Querier
	// Wake hears of new or pinned messages: the "urgent" trigger.
	Wake func(chatID int64, msgIDs []int64)
}

// Open gives the inbox of the store.
func Open(s *store.Store) *Inbox { return &Inbox{db: s.DB} }

// With is the same inbox over a transaction.
func (in *Inbox) With(q Querier) *Inbox {
	c := *in
	c.db = q
	return &c
}

// --- chats ------------------------------------------------------------------

const chatColumns = "id, title, kind, username, last_id, added_at"

// Chat is one chat; nil when there is none.
func (in *Inbox) Chat(chatID int64) (*Chat, error) {
	var c Chat
	err := in.db.QueryRow("select "+chatColumns+" from chats where id = ?", chatID).
		Scan(&c.ID, &c.Title, &c.Kind, &c.Username, &c.LastID, &c.AddedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Chats are every chat with the number of stored messages and the time of the newest one, by title.
func (in *Inbox) Chats() ([]ChatStats, error) {
	rows, err := in.db.Query("select c.id, c.title, c.kind, c.username, c.last_id, c.added_at, count(m.id) messages, " +
		"max(m.at) last_at from chats c left join messages m on m.source = 'tg' and m.chat_id = c.id group by c.id " +
		"order by c.title")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatStats{}
	for rows.Next() {
		var c ChatStats
		if err := rows.Scan(&c.ID, &c.Title, &c.Kind, &c.Username, &c.LastID, &c.AddedAt, &c.Messages,
			&c.LastAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutChat adds a chat or refreshes its name; it reports whether the chat is new. `lastID` is kept for a known chat.
func (in *Inbox) PutChat(chatID int64, title, kind string, username *string, lastID int64) (bool, error) {
	added := false
	err := inTx(in.db, func(q Querier) error {
		result, err := q.Exec("insert into chats(id, title, kind, username, last_id, added_at) values(?, ?, ?, ?, ?, ?) "+
			"on conflict(id) do nothing", chatID, title, kind, username, lastID, clock.LocalISO(clock.NowUTC()))
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			added = true
			return nil
		}
		_, err = q.Exec("update chats set title = ?, kind = ?, username = ? where id = ?", title, kind, username, chatID)
		return err
	})
	return added, err
}

// RenameChat gives a chat its new title.
func (in *Inbox) RenameChat(chatID int64, title string) error {
	_, err := in.db.Exec("update chats set title = ? where id = ?", title, chatID)
	return err
}

// Advance: the newest message seen in a chat, stored or not: a disabled chat moves on too, so enabling it later does
// not pull its history into the queue.
func (in *Inbox) Advance(chatID, msgID int64) error {
	_, err := in.db.Exec("update chats set last_id = max(last_id, ?) where id = ?", msgID, chatID)
	return err
}

// --- messages ---------------------------------------------------------------

func scanMessage(scan func(...any) error) (*Message, error) {
	var m Message
	var forward, files *string
	if err := scan(&m.ID, &m.Source, &m.ChatID, &m.MsgID, &m.SenderID, &m.SenderName, &m.At, &m.Text, &m.TopicID,
		&m.ReplyTo, &m.ReplyQuote, &forward, &m.GroupedID, &files, &m.Pinned, &m.EditedAt, &m.DeletedAt, &m.ParsedAt,
		&m.SummarizedAt, &m.Subject); err != nil {
		return nil, err
	}
	if forward != nil && *forward != "" {
		if err := json.Unmarshal([]byte(*forward), &m.Forward); err != nil {
			return nil, err
		}
	}
	m.Files = []File{}
	if files != nil && *files != "" {
		if err := json.Unmarshal([]byte(*files), &m.Files); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

func (in *Inbox) messageList(query string, args ...any) ([]*Message, error) {
	rows, err := in.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Message{}
	for rows.Next() {
		m, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Message is one stored message ("tg" or "mail"); nil when it is not stored.
func (in *Inbox) Message(chatID, msgID int64, source string) (*Message, error) {
	m, err := scanMessage(in.db.QueryRow("select "+messageColumns+" from messages where source = ? and chat_id = ? "+
		"and msg_id = ?", source, chatID, msgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// Has tells whether a message is stored.
func (in *Inbox) Has(chatID, msgID int64, source string) (bool, error) {
	var one int
	err := in.db.QueryRow("select 1 from messages where source = ? and chat_id = ? and msg_id = ?", source, chatID,
		msgID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func encodeJSON(value any) (any, error) {
	text, err := pyjson.Dumps(value)
	if err != nil {
		return nil, err
	}
	return text, nil
}

// values are the MESSAGE_FIELDS of Python's _encode, in order.
func values(f Fields) ([]any, error) {
	var forward, files any
	var err error
	if len(f.Forward) > 0 {
		if forward, err = encodeJSON(f.Forward); err != nil {
			return nil, err
		}
	}
	if len(f.Files) > 0 {
		if files, err = encodeJSON(f.Files); err != nil {
			return nil, err
		}
	}
	return []any{f.SenderID, f.SenderName, clock.LocalISO(f.At), f.Text, f.TopicID, f.ReplyTo, f.ReplyQuote, forward,
		f.GroupedID, files, f.Pinned}, nil
}

// Add stores a new message; it reports false if it is stored already (the catch-up and live updates overlap).
// `editedAt` (zero for none): a message older than its 14 days, stored because it was edited just now (D74).
func (in *Inbox) Add(chatID, msgID int64, fields Fields, source string, editedAt time.Time) (bool, error) {
	vals, err := values(fields)
	if err != nil {
		return false, err
	}
	var edited any
	if !editedAt.IsZero() {
		edited = clock.LocalISO(editedAt)
	}
	args := append([]any{source, chatID, msgID}, vals...)
	args = append(args, edited)
	result, err := in.db.Exec("insert into messages(source, chat_id, msg_id, "+fieldColumns+", edited_at) "+
		"values(?, ?, ?, "+marks(len(vals))+", ?) on conflict do nothing", args...)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n > 0 && in.Wake != nil {
		in.Wake(chatID, []int64{msgID})
	}
	return n > 0, nil
}

// Edit replaces the content; it reports false if the message is not stored or nothing in it changed (Telegram also
// sends edits for reactions and link previews).
func (in *Inbox) Edit(chatID, msgID int64, text string, files []File, at time.Time) (bool, error) {
	stored, err := in.Message(chatID, msgID, "tg")
	if err != nil || stored == nil {
		return false, err
	}
	same, err := SameFiles(stored.Files, files)
	if err != nil {
		return false, err
	}
	if stored.Text == text && same {
		return false, nil
	}
	var filesJSON any
	if len(files) > 0 {
		if filesJSON, err = encodeJSON(files); err != nil {
			return false, err
		}
	}
	result, err := in.db.Exec("update messages set text = ?, files = ?, edited_at = ? where source = 'tg' and "+
		"chat_id = ? and msg_id = ? and (text != ? or files is not ?)",
		text, filesJSON, clock.LocalISO(at), chatID, msgID, text, filesJSON)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// Delete marks messages deleted; with chatID nil the ids are account-wide (private chats and basic groups).
func (in *Inbox) Delete(msgIDs []int64, chatID *int64, at time.Time) (int64, error) {
	args := []any{clock.LocalISO(at)}
	var where string
	if chatID == nil {
		where = "chat_id in (select id from chats where kind in (" + marks(len(AccountWideIDs)) + "))"
		for _, kind := range AccountWideIDs {
			args = append(args, kind)
		}
	} else {
		where = "chat_id = ?"
		args = append(args, *chatID)
	}
	for _, id := range msgIDs {
		args = append(args, id)
	}
	result, err := in.db.Exec("update messages set deleted_at = ? where source = 'tg' and deleted_at is null and "+
		where+" and msg_id in ("+marks(len(msgIDs))+")", args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Pin marks messages pinned or not.
func (in *Inbox) Pin(chatID int64, msgIDs []int64, pinned bool) error {
	args := []any{pinned, chatID}
	for _, id := range msgIDs {
		args = append(args, id)
	}
	if _, err := in.db.Exec("update messages set pinned = ? where source = 'tg' and chat_id = ? and msg_id in ("+
		marks(len(msgIDs))+")", args...); err != nil {
		return err
	}
	if pinned && in.Wake != nil {
		in.Wake(chatID, msgIDs)
	}
	return nil
}

// Messages are the stored ones of these messages, by msg_id.
func (in *Inbox) Messages(chatID int64, msgIDs []int64, source string) (map[int64]*Message, error) {
	args := []any{source, chatID}
	for _, id := range msgIDs {
		args = append(args, id)
	}
	list, err := in.messageList("select "+messageColumns+" from messages where source = ? and chat_id = ? and "+
		"msg_id in ("+marks(len(msgIDs))+")", args...)
	if err != nil {
		return nil, err
	}
	out := map[int64]*Message{}
	for _, m := range list {
		out[m.MsgID] = m
	}
	return out, nil
}

// --- the queue of the batches -------------------------------------------------

// Pending are the messages waiting for a batch, oldest first: never parsed, or edited since.
func (in *Inbox) Pending(chatID int64, source string) ([]*Message, error) {
	return in.messageList("select "+messageColumns+" from messages where source = ? and chat_id = ? and "+Pending+
		" order by at, msg_id", source, chatID)
}

// Before are up to `limit` messages of the chat just before `message`, in its forum topic, not older than `since`,
// oldest first: the context of a batch. Deleted ones, those waiting for a batch of their own and those with neither
// text nor files are left out.
func (in *Inbox) Before(chatID int64, message *Message, since time.Time, limit int) ([]*Message, error) {
	list, err := in.messageList("select "+messageColumns+" from messages where source = ? and chat_id = ? and "+
		"topic_id is ? and deleted_at is null and parsed_at is not null and (edited_at is null or edited_at <= parsed_at) "+
		"and at >= ? and (at < ? or (at = ? and msg_id < ?)) and (text <> '' or files is not null) "+
		"order by at desc, msg_id desc limit ?", message.Source, chatID, message.TopicID, clock.LocalISO(since),
		message.At, message.At, message.MsgID, limit)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, nil
}

// PendingCounts are the numbers of waiting messages, by chat.
func (in *Inbox) PendingCounts() (map[int64]int64, error) {
	rows, err := in.db.Query("select chat_id, count(*) n from messages where " + Pending + " group by chat_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var chat, n int64
		if err := rows.Scan(&chat, &n); err != nil {
			return nil, err
		}
		out[chat] = n
	}
	return out, rows.Err()
}

// MarkParsed marks messages parsed: call it on an Inbox bound to the transaction that saves what they gave.
func (in *Inbox) MarkParsed(chatID int64, msgIDs []int64, at time.Time, source string) error {
	args := []any{clock.LocalISO(at), source, chatID}
	for _, id := range msgIDs {
		args = append(args, id)
	}
	_, err := in.db.Exec("update messages set parsed_at = ? where source = ? and chat_id = ? and msg_id in ("+
		marks(len(msgIDs))+")", args...)
	return err
}

// --- letters ------------------------------------------------------------------

// AddLetter stores a letter (SenderName, At, Text, Files, Subject); false if it is stored already. Call it on an
// Inbox bound to the transaction that saves the mailbox state, so a letter is never stored twice.
func (in *Inbox) AddLetter(msgID int64, fields Fields) (bool, error) {
	vals, err := values(fields)
	if err != nil {
		return false, err
	}
	var subject any
	if fields.Subject != "" {
		subject = fields.Subject
	}
	args := append([]any{Mailbox, msgID}, vals...)
	args = append(args, subject)
	result, err := in.db.Exec("insert into messages(source, chat_id, msg_id, "+fieldColumns+", subject) "+
		"values('mail', ?, ?, "+marks(len(vals))+", ?) on conflict do nothing", args...)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// NewestLetter is the highest msg_id of the letters stored, 0 for none.
func (in *Inbox) NewestLetter() (int64, error) {
	var n *int64
	err := in.db.QueryRow("select max(msg_id) n from messages where source = 'mail' and chat_id = ?", Mailbox).Scan(&n)
	if err != nil || n == nil {
		return 0, err
	}
	return *n, nil
}

// Letters is the number of letters stored: those of the last 14 days.
func (in *Inbox) Letters() (int64, error) {
	var n int64
	err := in.db.QueryRow("select count(*) n from messages where source = 'mail'").Scan(&n)
	return n, err
}

// Subject is the subject of the first of these letters while it is stored; nil after its 14 days or without one.
func (in *Inbox) Subject(msgIDs []int64) (*string, error) {
	if len(msgIDs) == 0 {
		return nil, nil
	}
	var subject *string
	err := in.db.QueryRow("select subject from messages where source = 'mail' and chat_id = ? and msg_id = ?",
		Mailbox, msgIDs[0]).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return subject, err
}

// Sender is who wrote in a chat and how much.
type Sender struct {
	ID       int64   `json:"id"`
	Name     *string `json:"name"`
	Messages int64   `json:"messages"`
}

// Senders are who wrote in a chat, most active first: the web app picks the important people from them.
func (in *Inbox) Senders(chatID int64) ([]Sender, error) {
	rows, err := in.db.Query("select sender_id id, sender_name name, count(*) messages from messages "+
		"where source = 'tg' and chat_id = ? and sender_id is not null "+
		"group by sender_id order by messages desc, max(at) desc", chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sender{}
	for rows.Next() {
		var s Sender
		if err := rows.Scan(&s.ID, &s.Name, &s.Messages); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Search finds stored messages and letters, newest first (the MCP connector, §8.5): those whose text or subject has
// `text` (in any case), of one chat (nil: all), from the day `since` to the day `until` (YYYY-MM-DD, both included;
// "" for no bound). They are kept 14 days (D29), so the scan stays small; SQLite's LIKE folds the case of ASCII only.
func (in *Inbox) Search(text string, chatID *int64, since, until string, limit int) ([]*Message, error) {
	query, params := "select "+messageColumns+" from messages where deleted_at is null", []any{}
	if chatID != nil {
		query += " and chat_id = ?"
		params = append(params, *chatID)
	}
	if since != "" {
		query += " and at >= ?"
		params = append(params, since)
	}
	if until != "" {
		day, err := time.Parse(clock.DateLayout, until)
		if err != nil {
			return nil, err
		}
		query += " and at < ?"
		params = append(params, day.AddDate(0, 0, 1).Format(clock.DateLayout))
	}
	rows, err := in.messageList(query+" order by at desc, id desc", params...)
	if err != nil {
		return nil, err
	}
	needle := pytext.Casefold(strings.Join(pytext.Split(text), " "))
	found := []*Message{}
	for _, m := range rows {
		subject := ""
		if m.Subject != nil {
			subject = *m.Subject
		}
		hay := pytext.Casefold(strings.Join(pytext.Split(m.Text+" "+subject), " "))
		if needle != "" && !strings.Contains(hay, needle) {
			continue
		}
		found = append(found, m)
		if len(found) >= limit {
			break
		}
	}
	return found, nil
}

// Prune removes raw messages older than 14 days (D29), counted from their last edit (D74).
func (in *Inbox) Prune(now time.Time) (int64, error) {
	result, err := in.db.Exec("delete from messages where coalesce(edited_at, at) < ?", clock.LocalISO(now.Add(-Retention)))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// LegacyPhoto is a photo as messages stored before stage 9 have it: without its size, and a link preview's picture
// counted as one.
var LegacyPhoto = File{"kind": "photo"}

// SameFiles tells whether the files of an edit are those stored. A legacy photo is the same as a photo of any size, or
// as nothing (it was a link preview, no longer counted): an edit of such a message is no new content. Gone with the 14
// days of the messages of stage 8. Values compare as JSON does (10 == 10.0).
func SameFiles(stored, files []File) (bool, error) {
	a, err := normal(stored)
	if err != nil {
		return false, err
	}
	b, err := normal(files)
	if err != nil {
		return false, err
	}
	if reflect.DeepEqual(a, b) {
		return true, nil
	}
	legacy, _ := normal([]File{LegacyPhoto})
	if !reflect.DeepEqual(a, legacy) {
		return false, nil
	}
	return len(files) == 0 || (len(files) == 1 && files[0]["kind"] == "photo"), nil
}

func normal(files []File) (any, error) {
	if files == nil {
		files = []File{}
	}
	data, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(data, &out)
	return out, err
}

// KeptFrom is the moment a message's 14 days count from: its last edit, or the message itself (D29, D74).
func KeptFrom(m *Message) string {
	if m.EditedAt != nil && *m.EditedAt != "" {
		return *m.EditedAt
	}
	return m.At
}

// MessageLink is the t.me link to a message; private chats and basic groups have none ("").
func MessageLink(chat *Chat, msgID int64) string {
	if chat.Username != nil && *chat.Username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", *chat.Username, msgID)
	}
	switch chat.Kind {
	case "supergroup", "forum", "channel":
		return fmt.Sprintf("https://t.me/c/%d/%d", -chat.ID-1_000_000_000_000, msgID) // -100XXXXXXXXXX -> XXXXXXXXXX
	}
	return ""
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }
