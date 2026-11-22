package feed

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	chat  = -1001
	owner = 42
)

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// twin is the twin's downloads: bytes by message; offline gives NotNow, a broken file another error.
type twin struct {
	remote  map[int64][]byte
	offline bool
	broken  map[int64]bool
	calls   []int64
}

func (t *twin) download(ctx context.Context, chatID, msgID int64, path string) (bool, error) {
	t.calls = append(t.calls, msgID)
	if t.offline {
		return false, &files.NotNow{Msg: "offline"}
	}
	if t.broken[msgID] {
		return false, errors.New("broken file")
	}
	data, ok := t.remote[msgID]
	if !ok {
		return false, nil
	}
	return true, os.WriteFile(path, data, 0o600)
}

type fixture struct {
	store     *store.Store
	library   *library.Library
	inbox     *inbox.Inbox
	materials *ChatMaterials
	twin      *twin
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	s := must(store.Open(filepath.Join(dir, "x.db")))
	t.Cleanup(func() { s.Close() })
	must(s.DB.Exec("insert into classes(uid, subject, start, end, kind, room, seen_at) values('calc-s', 'Calculus', " +
		"'2026-10-20T10:00:00', '2026-10-20T11:20:00', 'Семинар', '', '2026-10-20T10:00:00')"))
	lib := must(library.New(s, dir))
	lib.Now = func() time.Time { return now }
	lib.SameCourse = func(a, b string) bool {
		return a != "" && b != "" && (pytext.Casefold(a) == pytext.Casefold(b) ||
			a == "Математический анализ" && b == "Calculus" || a == "Calculus" && b == "Математический анализ")
	}
	in := inbox.Open(s)
	must(in.PutChat(chat, "Матан", "supergroup", nil, 0))
	f := &fixture{store: s, library: lib, inbox: in, twin: &twin{remote: map[int64][]byte{}, broken: map[int64]bool{}}}
	f.materials = must(New(s, lib, in, func() time.Time { return now }))
	f.materials.Download = f.twin.download
	return f
}

func (f *fixture) add(msgID int64, text string, files ...inbox.File) {
	name, sender := "Студентка А", int64(7)
	must(f.inbox.Add(chat, msgID, inbox.Fields{SenderID: &sender, SenderName: &name, Text: text, Files: files,
		At: now.Add(-30 * time.Minute)}, "tg", time.Time{}))
}

func (f *fixture) queue(materials ...Material) {
	if err := f.store.InTx(func(tx *sql.Tx) error { return Queue(tx, chat, materials, now) }); err != nil {
		panic(err)
	}
}

func photo(size int64) inbox.File {
	return inbox.File{"kind": "photo", "mime": "image/jpeg", "size": size}
}

func material(msgID int64, course, kind, title string) Material {
	return Material{MsgID: msgID, Course: course, Kind: kind, Title: title}
}

func (f *fixture) items() map[int64]*library.Material {
	out := map[int64]*library.Material{}
	for _, item := range must(f.library.Items(library.Query{})) {
		out[*item.MsgID] = item
	}
	return out
}

func TestHelpers(t *testing.T) {
	if FirstURL("Запись: https://disk.example.org/rec1. Смотрите") != "https://disk.example.org/rec1" ||
		FirstURL("слайды (https://slides.example.org/a)") != "https://slides.example.org/a" ||
		FirstURL("без ссылок") != "" {
		t.Fatal("first url")
	}
	if FileName(inbox.File{"kind": "photo", "mime": "image/jpeg"}, 5) != "photo-5.jpg" ||
		FileName(inbox.File{"kind": "document", "name": "notes.pdf"}, 5) != "notes.pdf" ||
		FileName(inbox.File{"kind": "voice", "mime": "audio/ogg"}, 3) != "voice-3.oga" ||
		FileName(inbox.File{"mime": "x/unknown"}, 1) != "file-1" {
		t.Fatal("file name")
	}
	if MaterialFile([]inbox.File{{"kind": "sticker"}, {"kind": "gif"}}) != nil {
		t.Fatal("a sticker is material")
	}
}

func TestBatchMaterials(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.add(1, "Доска с семинара", photo(5000))
	f.add(2, "", photo(5000)) // the album's other photo, without text
	f.add(3, "", inbox.File{"kind": "voice", "mime": "audio/ogg", "size": 300})
	f.add(4, "Запись лекции: https://disk.example.org/rec1.")
	f.add(5, "Конспект: признак Коши, примеры 1-3")
	f.add(7, "", photo(library.MaxFile+1))
	f.twin.remote = map[int64][]byte{1: []byte("board-1"), 2: []byte("board-2"), 3: []byte("voice")}
	f.twin.offline = true
	f.queue(material(1, "Математический анализ", "photo", "Calc seminar board"),
		material(2, "Математический анализ", "photo", "Calc seminar board"),
		material(3, "", "audio", "Lecture recording"),
		material(4, "Математический анализ", "link", "Calc lecture recording"),
		material(5, "Математический анализ", "notes", "Calc seminar notes"),
		material(7, "Математический анализ", "photo", "Calc seminar board"))
	f.materials.Kick(ctx)
	f.materials.Settle()

	// The twin is offline: the link and the note are in, the files wait in the queue.
	items := f.items()
	link, note := items[4], items[5]
	if len(items) != 2 || link.Kind != "link" || *link.URL != "https://disk.example.org/rec1" ||
		*link.Course != "Calculus" || *link.ClassUID != "calc-s" || link.Title != "Calc lecture recording" {
		t.Fatalf("%+v", link)
	}
	if link.Source != "chat" || *link.ChatID != chat || link.Author != "Студентка А" || *link.Day != "2026-10-20" {
		t.Fatalf("%+v", link)
	}
	if note.Kind != "text" || *note.Text != "Конспект: признак Коши, примеры 1-3" {
		t.Fatalf("%+v", note)
	}
	if n := must(f.materials.Waiting()); n != 3 || !reflect.DeepEqual(f.twin.calls, []int64{1}) {
		t.Fatal(n, f.twin.calls) // one try, the others not asked; 7 is too big
	}

	f.twin.offline = false
	if n := must(f.materials.Drain(ctx)); n != 3 {
		t.Fatal(n)
	}
	items = f.items()
	if len(items) != 5 || items[7] != nil {
		t.Fatal(len(items)) // 7 is too big: skipped, not downloaded
	}
	board := items[1]
	if board.Kind != "photo" || *board.Course != "Calculus" || *board.ClassUID != "calc-s" ||
		*board.Text != "Доска с семинара" || *board.Name != "photo-1.jpg" {
		t.Fatalf("%+v", board)
	}
	if data := must(os.ReadFile(f.library.Path(items[2]))); string(data) != "board-2" {
		t.Fatal(string(data))
	}
	voice := items[3]
	if voice.Kind != "audio" || voice.Course != nil || *voice.MediaType != "audio/ogg" || voice.Note == "" ||
		must(f.library.InboxCount()) != 1 {
		t.Fatalf("%+v", voice)
	}
	if must(f.materials.Waiting()) != 0 || slices.Contains(f.twin.calls, 7) || must(f.materials.Drain(ctx)) != 0 {
		t.Fatal(f.twin.calls)
	}

	// An edited message told again is not taken twice; a deleted one is not taken at all.
	must(f.inbox.Edit(chat, 5, "Конспект: признак Коши, примеры 1-4", nil, now))
	f.add(8, "", photo(5000))
	c := int64(chat)
	must(f.inbox.Delete([]int64{8}, &c, now))
	f.queue(material(5, "Математический анализ", "notes", ""), material(8, "Математический анализ", "photo", ""))
	if n := must(f.materials.Drain(ctx)); n != 0 || must(f.materials.Waiting()) != 0 || len(f.items()) != 5 {
		t.Fatal(n)
	}
}

func TestFailuresAreTriedThreeTimes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.add(1, "", photo(5000))
	f.twin.broken[1] = true
	f.queue(material(1, "Математический анализ", "photo", "Calc seminar board"))
	for range 2 {
		if n := must(f.materials.Drain(ctx)); n != 0 || must(f.materials.Waiting()) != 1 {
			t.Fatal("given up early")
		}
	}
	if n := must(f.materials.Drain(ctx)); n != 0 || must(f.materials.Waiting()) != 0 {
		t.Fatal("not given up")
	}
	if !reflect.DeepEqual(f.twin.calls, []int64{1, 1, 1}) || len(f.items()) != 0 {
		t.Fatal(f.twin.calls)
	}
}

func TestFromBot(t *testing.T) {
	f := setup(t)
	saved := func(data string) func(context.Context, string) (bool, error) {
		return func(_ context.Context, path string) (bool, error) {
			return true, os.WriteFile(path, []byte(data), 0o600)
		}
	}
	id := must(FromBot(context.Background(), f.library, BotMessage{ChatID: owner, MsgID: 1, At: now, Text: "Lecture 4 slides",
		Files:   []inbox.File{{"kind": "document", "name": "lec4.pdf", "mime": "application/pdf", "size": 2048.0}},
		Forward: map[string]any{"from": "Classmate B"}}, saved("%PDF-slides")))
	item := must(f.library.Get(*id, false, false))
	if item.Source != "bot" || *item.ChatID != owner || *item.MsgID != 1 || item.Kind != "file" ||
		item.Author != "Classmate B" || item.Title != "Lecture 4 slides" || *item.Name != "lec4.pdf" ||
		item.Course != nil || item.Note != "sent to the bot" {
		t.Fatalf("%+v", item)
	}
	if again := must(FromBot(context.Background(), f.library, BotMessage{ChatID: owner, MsgID: 1, Text: "x"}, nil)); again != nil {
		t.Fatal("taken twice")
	}
	photoID := must(FromBot(context.Background(), f.library, BotMessage{ChatID: owner, MsgID: 2, Text: "доска",
		Files: []inbox.File{{"kind": "photo", "mime": "image/jpeg", "size": 4000.0}}}, saved("photo-2")))
	if p := must(f.library.Get(*photoID, false, false)); p.Kind != "photo" || p.Author != "owner" ||
		*p.Name != "photo-2.jpg" {
		t.Fatalf("%+v", p)
	}
	linkID := must(FromBot(context.Background(), f.library, BotMessage{ChatID: owner, MsgID: 5, Text: "Запись семинара https://video.example.org/s5"}, nil))
	if l := must(f.library.Get(*linkID, false, false)); l.Kind != "link" || *l.URL != "https://video.example.org/s5" {
		t.Fatalf("%+v", l)
	}
	noteID := must(FromBot(context.Background(), f.library, BotMessage{ChatID: owner, MsgID: 6, Text: "Заметка: на экзамене будет признак Даламбера"}, nil))
	if n := must(f.library.Get(*noteID, false, false)); n.Kind != "text" || *n.Text != "Заметка: на экзамене будет признак Даламбера" {
		t.Fatalf("%+v", n)
	}
	_, err := FromBot(context.Background(), f.library, BotMessage{ChatID: owner, MsgID: 7,
		Files: []inbox.File{{"kind": "video", "name": "rec.mp4", "size": float64(library.MaxFile + 1)}}}, nil)
	var refused *library.Error
	if !errors.As(err, &refused) {
		t.Fatal(err)
	}
	if sticker := must(FromBot(context.Background(), f.library, BotMessage{ChatID: owner, MsgID: 10,
		Files: []inbox.File{{"kind": "sticker"}}}, nil)); sticker != nil {
		t.Fatal("a sticker is kept")
	}
}
