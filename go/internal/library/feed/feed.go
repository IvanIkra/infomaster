// Package feed is what feeds the library (D58) from the chats and the bot; the sheets add themselves
// (processing/sheets).
//
// Study chats. The batch model marks the messages that carry course material (processing/batches, `materials`):
// a photo of the board, notes, slides, a recording, a link to one. They are queued in `library_queue` in the batch's
// own transaction, so a run that ends after its messages are marked parsed loses none of them, and taken into the
// library after it (Drain), outside any transaction: a recording may be hundreds of megabytes, and the twin streams
// it to disk. A file the twin cannot download now (files.NotNow: it is offline, a flood wait) keeps its row and is
// tried again after the next batch run, at most every hour; so is one that fails otherwise, up to MaxTries times. A
// row goes when its item is made, when its message is deleted, or when the message leaves the inbox after its 14
// days.
//
// The bot. A private message of the owner to the bot (a forward, a file, a link, a note) goes to the library's inbox
// as it is: Claude sorts it later.
package feed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Schema is the queue's table, run at every start.
const Schema = `
create table if not exists library_queue (  -- materials the batches found, waiting to go into the library (D58)
    chat_id integer not null,
    msg_id integer not null,
    course text,                -- as the model named it; null: not sure, the item goes to the inbox
    kind text not null,         -- what the model took it for: photo, notes, slides, audio, video, link, text, file
    title text not null default '',
    tries integer not null default 0,   -- failures other than NotNow
    created_at text not null,
    primary key (chat_id, msg_id)
);
`

const MaxTries = 3

var (
	// MaterialKinds are the kinds the batch model may give a material (the library's kinds without sheets, which
	// have their own field, and transcripts, which Claude makes).
	MaterialKinds = []string{"photo", "notes", "slides", "audio", "video", "link", "text", "file"}
	// FileKinds are what the model may tell of a file beyond its type: else the type decides.
	FileKinds = []string{"notes", "slides"}
	// Media are the files of a message that can be material: stickers and GIF animations never are.
	Media = []string{"photo", "document", "pdf", "audio", "voice", "video", "video_note"}
)

var urlPattern = regexp.MustCompile(`https?://[^\s\x0b\x1c-\x1f\x85\xa0\x{1680}\x{2000}-\x{200a}\x{2028}` +
	`\x{2029}\x{202f}\x{205f}\x{3000}<>()"'«»]+`)

func text(entry inbox.File, key string) string {
	s, _ := entry[key].(string)
	return s
}

// MaterialFile is the message's own file that can be material (its `files` entry), if any.
func MaterialFile(fs []inbox.File) inbox.File {
	for _, f := range fs {
		if slices.Contains(Media, text(f, "kind")) {
			return f
		}
	}
	return nil
}

// FirstURL is the first link of a text, without the punctuation after it; "" for none.
func FirstURL(s string) string {
	return strings.TrimRight(urlPattern.FindString(s), ".,;:!?")
}

// extensions are mimetypes.guess_extension of the types a message's file may have.
var extensions = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif", "image/heic": ".heic",
	"image/bmp": ".bmp", "image/tiff": ".tiff", "image/svg+xml": ".svg",
	"audio/ogg": ".oga", "audio/mpeg": ".mp3", "audio/mp4": ".m4a", "audio/x-wav": ".wav", "audio/aac": ".aac",
	"audio/webm": ".weba", "audio/opus": ".opus",
	"video/mp4": ".mp4", "video/quicktime": ".mov", "video/webm": ".webm", "video/x-matroska": ".mkv",
	"video/mpeg":      ".mpeg",
	"application/pdf": ".pdf", "application/zip": ".zip", "application/octet-stream": ".bin", "text/plain": ".txt",
	"text/html": ".html", "text/csv": ".csv", "application/json": ".json", "application/msword": ".doc",
	"application/vnd.ms-powerpoint": ".ppt", "application/vnd.ms-excel": ".xls",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
}

// FileName is the file's own name; a photo or a voice message has none: «photo-<msg_id>.jpg».
func FileName(file inbox.File, msgID int64) string {
	if name := text(file, "name"); name != "" {
		return name
	}
	kind := text(file, "kind")
	if kind == "" {
		kind = "file"
	}
	return fmt.Sprintf("%s-%d%s", kind, msgID, extensions[strings.ToLower(text(file, "mime"))])
}

// Material is a material of a batch's answer.
type Material struct {
	MsgID  int64  `json:"msg_id"`
	Course string `json:"course"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
}

// ChatMaterials takes the materials of the study chats into the library.
type ChatMaterials struct {
	db      *sql.DB
	Library *library.Library
	Inbox   *inbox.Inbox
	Now     func() time.Time
	// Download is the twin's download of a message's file into a path: false when it is gone, a *files.NotNow when
	// it cannot be done now. Nil without a twin: files wait.
	Download func(ctx context.Context, chatID, msgID int64, path string) (bool, error)

	drain   sync.Mutex // one drain at a time
	mu      sync.Mutex // running
	running bool
	done    chan struct{}
}

// New makes the feed and its table; now nil is clock.NowUTC.
func New(s *store.Store, lib *library.Library, in *inbox.Inbox, now func() time.Time) (*ChatMaterials, error) {
	if now == nil {
		now = clock.NowUTC
	}
	if _, err := s.DB.Exec(Schema); err != nil {
		return nil, err
	}
	return &ChatMaterials{db: s.DB, Library: lib, Inbox: in, Now: now}, nil
}

// Queue keeps materials of a batch's answer; a message told again (an edit) updates its row. Call it with the
// batch's transaction.
func Queue(q store.Querier, chatID int64, materials []Material, at time.Time) error {
	for _, m := range materials {
		var course any
		if m.Course != "" {
			course = m.Course
		}
		if _, err := q.Exec("insert into library_queue(chat_id, msg_id, course, kind, title, created_at) "+
			"values(?, ?, ?, ?, ?, ?) on conflict(chat_id, msg_id) do update set course = excluded.course, "+
			"kind = excluded.kind, title = excluded.title",
			chatID, m.MsgID, course, m.Kind, m.Title, clock.LocalISO(at)); err != nil {
			return err
		}
	}
	return nil
}

// Waiting is the number of queued materials.
func (c *ChatMaterials) Waiting() (int, error) {
	var n int
	err := c.db.QueryRow("select count(*) from library_queue").Scan(&n)
	return n, err
}

// Kick drains the queue in the background (after a batch run), unless a drain is running already.
func (c *ChatMaterials) Kick(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return
	}
	c.running, c.done = true, make(chan struct{})
	go func(done chan struct{}) {
		defer func() {
			c.mu.Lock()
			c.running = false
			c.mu.Unlock()
			close(done)
		}()
		if _, err := c.Drain(ctx); err != nil {
			slog.Error("taking the chats' materials into the library failed", "err", err)
		}
	}(c.done)
}

// Settle waits for the background drain (tests).
func (c *ChatMaterials) Settle() {
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

type queued struct {
	chatID, msgID int64
	course        *string
	kind, title   string
	tries         int
}

// Drain takes every queued material into the library, the oldest first; how many items were made. After the first
// NotNow (the twin cannot download now) the other files wait too, without asking; texts and links go on.
func (c *ChatMaterials) Drain(ctx context.Context) (int, error) {
	c.drain.Lock()
	defer c.drain.Unlock()
	rows, err := c.db.Query("select chat_id, msg_id, course, kind, title, tries from library_queue " +
		"order by created_at, msg_id")
	if err != nil {
		return 0, err
	}
	var all []queued
	for rows.Next() {
		var q queued
		if err := rows.Scan(&q.chatID, &q.msgID, &q.course, &q.kind, &q.title, &q.tries); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	made, offline := 0, false
	for _, row := range all {
		id, err := c.take(ctx, row, offline)
		if files.IsNotNow(err) {
			if !offline {
				slog.Info(fmt.Sprintf("library materials wait for the twin: %v", err))
			}
			offline = true
			continue
		}
		if err != nil {
			tries := row.tries + 1
			slog.Warn(fmt.Sprintf("material %d of %d not taken (try %d): %v", row.msgID, row.chatID, tries, err))
			if tries < MaxTries {
				if _, err := c.db.Exec("update library_queue set tries = ? where chat_id = ? and msg_id = ?", tries,
					row.chatID, row.msgID); err != nil {
					return made, err
				}
				continue
			}
			id = nil
		}
		if id != nil {
			made++
		}
		if _, err := c.db.Exec("delete from library_queue where chat_id = ? and msg_id = ?", row.chatID,
			row.msgID); err != nil {
			return made, err
		}
	}
	return made, nil
}

// take is one queued material: its item's id, or nil when there is nothing to take (the message is gone or deleted,
// it is in the library already, its file is gone or too big). NotNow passes through.
func (c *ChatMaterials) take(ctx context.Context, row queued, offline bool) (*int64, error) {
	chatID, msgID := row.chatID, row.msgID
	message, err := c.Inbox.Message(chatID, msgID, "tg")
	if err != nil || message == nil || message.DeletedAt != nil {
		return nil, err
	}
	if found, err := c.Library.FromOrigin("chat", chatID, msgID, ""); err != nil || found != nil {
		return nil, err
	}
	at, err := clock.ParseLocal(message.At)
	if err != nil {
		return nil, err
	}
	course := ""
	if row.course != nil {
		if course, err = c.Library.Course(*row.course); err != nil {
			return nil, err
		}
	}
	classUID := ""
	if course != "" {
		class, err := c.Library.FindClass(course, at, false)
		if err != nil {
			return nil, err
		}
		if class != nil {
			classUID = class.UID
		}
	}
	author := ""
	if message.SenderName != nil && *message.SenderName != "" {
		author = *message.SenderName
	} else if chat, err := c.Inbox.Chat(chatID); err != nil {
		return nil, err
	} else if chat != nil {
		author = chat.Title
	}
	note := ""
	if course == "" {
		note = "the batch could not tell its course"
	}
	item := library.Item{Source: "chat", ChatID: &chatID, MsgID: &msgID, Course: course, ClassUID: classUID,
		Title: row.title, Author: author, At: at, Note: note}
	var textPtr *string
	if t := pytext.Strip(message.Text); t != "" {
		textPtr = &t
	}
	item.Text = textPtr
	if file := MaterialFile(message.Files); file != nil {
		if files.Size(file) > library.MaxFile {
			slog.Info(fmt.Sprintf("material %d of %d skipped: its file is over %d MB", msgID, chatID,
				library.MaxFile>>20))
			return nil, nil
		}
		if c.Download == nil || offline {
			return nil, &files.NotNow{Msg: "no twin to download the file"}
		}
		kind := ""
		if slices.Contains(FileKinds, row.kind) {
			kind = row.kind // else by the file's type
		}
		download := func(path string) (bool, error) { return c.Download(ctx, chatID, msgID, path) }
		id, err := c.Library.AddDownload(download, FileName(file, msgID), text(file, "mime"), kind, item)
		var refused *library.Error
		if errors.As(err, &refused) {
			slog.Info(fmt.Sprintf("material %d of %d skipped: %v", msgID, chatID, err))
			return nil, nil
		}
		return id, err
	}
	return addText(c.Library, textPtr, item)
}

// addText makes a link of a text with a link, else a text item; nil without a text.
func addText(lib *library.Library, text *string, item library.Item) (*int64, error) {
	if text == nil {
		return nil, nil
	}
	kind := "text"
	if url := FirstURL(*text); url != "" {
		kind, item.URL = "link", &url
	}
	id, err := lib.Add(kind, item)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// BotMessage is a private message of the owner to the bot, as the bot gives it.
type BotMessage struct {
	ChatID  int64
	MsgID   int64
	At      time.Time // zero: now
	Text    string
	Files   []inbox.File
	Forward map[string]any // {from, ...}; nil for none
}

// FromBot puts a private message of the owner to the bot into the library's inbox: the item's id, or nil when it
// carries nothing to keep (a sticker) or was taken already. `download(ctx, path)` streams its file. A *library.Error
// for a file the library refuses (too big, empty).
func FromBot(ctx context.Context, lib *library.Library, message BotMessage,
	download func(ctx context.Context, path string) (bool, error)) (*int64, error) {
	chatID, msgID := message.ChatID, message.MsgID
	if found, err := lib.FromOrigin("bot", chatID, msgID, ""); err != nil || found != nil {
		return nil, err
	}
	var textPtr *string
	if t := pytext.Strip(message.Text); t != "" {
		textPtr = &t
	}
	author := "owner"
	if len(message.Forward) > 0 {
		author, _ = message.Forward["from"].(string)
	}
	item := library.Item{Source: "bot", ChatID: &chatID, MsgID: &msgID, At: message.At, Author: author,
		Note: "sent to the bot", Text: textPtr}
	if file := MaterialFile(message.Files); file != nil {
		if files.Size(file) > library.MaxFile {
			return nil, &library.Error{Msg: fmt.Sprintf("the file is over %d MB", library.MaxFile>>20)}
		}
		item.Title = pytext.Cut(strings.Join(pytext.Split(message.Text), " "), 200)
		save := func(path string) (bool, error) { return download(ctx, path) }
		return lib.AddDownload(save, FileName(file, msgID), text(file, "mime"), "", item)
	}
	return addText(lib, textPtr, item)
}
