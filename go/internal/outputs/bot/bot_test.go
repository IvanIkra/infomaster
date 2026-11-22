package bot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/library"
)

const owner = 4242

var ctx = context.Background()

type sent struct {
	chat    int64
	text    string
	options SendOptions
}

// fakeAPI records what the bot sends; a download writes the message's data.
type fakeAPI struct {
	sent   []sent
	menus  []int64
	data   map[int][]byte
	done   chan struct{}
	closed bool
}

func (f *fakeAPI) Send(_ context.Context, chat int64, text string, options SendOptions) error {
	f.sent = append(f.sent, sent{chat, text, options})
	return nil
}

func (f *fakeAPI) ResetMenu(_ context.Context, user int64) error {
	f.menus = append(f.menus, user)
	return nil
}

func (f *fakeAPI) ResetCommands(context.Context) error { return nil }

func (f *fakeAPI) Download(_ context.Context, m *tg.Message, path string) (bool, error) {
	return true, os.WriteFile(path, f.data[m.ID], 0o600)
}

func (f *fakeAPI) Done() <-chan struct{} { return f.done }

func (f *fakeAPI) Close() error {
	f.closed = true
	return nil
}

func setup(t *testing.T) (*Bot, *fakeAPI, *string) {
	t.Helper()
	lang := "en"
	api := &fakeAPI{data: map[int][]byte{}, done: make(chan struct{})}
	b := New(config.Env{OwnerID: owner, PublicURL: "https://example.test"},
		filepath.Join(t.TempDir(), "bot.gotd.json"), func() string { return lang })
	b.Connect = func(context.Context, string, func(context.Context, Event)) (API, error) { return api, nil }
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.menus, []int64{0}) { // the Mini App's menu button is taken away
		t.Fatal(api.menus)
	}
	api.menus = nil
	return b, api, &lang
}

// private is a private message to the bot, as the update gives it.
func private(id int, text string, sender int64, edit ...func(*tg.Message)) Event {
	m := &tg.Message{ID: id, PeerID: &tg.PeerUser{UserID: sender}, Date: int(clock.NowUTC().Unix()), Message: text}
	for _, e := range edit {
		e(m)
	}
	return Event{SenderID: sender, ChatID: sender, Message: m}
}

func document(mime, name string, size int64, attributes ...tg.DocumentAttributeClass) func(*tg.Message) {
	if name != "" {
		attributes = append(attributes, &tg.DocumentAttributeFilename{FileName: name})
	}
	return func(m *tg.Message) {
		m.Media = &tg.MessageMediaDocument{Document: &tg.Document{ID: 1, AccessHash: 1, MimeType: mime, Size: size,
			DCID: 2, Attributes: attributes}}
	}
}

func picture(m *tg.Message) {
	m.Media = &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 1, AccessHash: 1, DCID: 2,
		Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "y", W: 1280, H: 960, Size: 4000}}}}
}

func album(id int64) func(*tg.Message) { return func(m *tg.Message) { m.GroupedID = id } }

const maxFile = 10000

// intake stands in for the library's from_bot: a message once, nothing for a sticker, a refusal over maxFile.
type intake struct {
	taken map[int64]Material
	files map[int64][]byte
}

func (in *intake) take(ctx context.Context, m Material, download Download) (string, error) {
	if _, ok := in.taken[m.MsgID]; ok {
		return "", nil
	}
	if len(m.Files) > 0 {
		file := m.Files[0]
		if file["kind"] == "sticker" {
			return "", nil
		}
		if size, _ := file["size"].(int); size > maxFile {
			return "", &library.Error{Msg: "the file is over 1 MB"}
		}
		path := filepath.Join(os.TempDir(), "bot-test.part")
		defer os.Remove(path)
		if ok, err := download(ctx, path); !ok || err != nil {
			return "", err
		}
		in.files[m.MsgID], _ = os.ReadFile(path)
	} else if m.Text == "" {
		return "", nil
	}
	in.taken[m.MsgID] = m
	return "inbox", nil
}

func answers(api *fakeAPI) []string {
	var out []string
	for _, s := range api.sent {
		out = append(out, s.text)
	}
	return out
}

func TestBotTakesTheOwnersMessages(t *testing.T) {
	b, api, lang := setup(t)
	in := &intake{taken: map[int64]Material{}, files: map[int64][]byte{}}
	b.OnMaterial = in.take

	forwarded := private(1, "Lecture 4 slides", owner, document("application/pdf", "lec4.pdf", 300),
		func(m *tg.Message) { m.FwdFrom = tg.MessageFwdHeader{Date: 1, FromName: "Classmate B"} })
	api.data[1] = []byte("%PDF-slides")
	b.handle(ctx, forwarded)
	item := in.taken[1]
	if item.ChatID != owner || item.Text != "Lecture 4 slides" || item.Forward["from"] != "Classmate B" ||
		item.Files[0]["name"] != "lec4.pdf" || string(in.files[1]) != "%PDF-slides" ||
		item.At.Before(time.Now().Add(-time.Minute)) {
		t.Fatal(item)
	}
	if !reflect.DeepEqual(api.sent, []sent{{owner, "Saved to the inbox.", SendOptions{Silent: true}}}) {
		t.Fatal(api.sent)
	}

	// An album: an item per photo, one answer.
	api.sent = nil
	b.handle(ctx, private(2, "доска", owner, picture, album(55)))
	b.handle(ctx, private(3, "", owner, picture, album(55)))
	if len(api.sent) != 1 || len(in.taken) != 3 || in.taken[3].Files[0]["kind"] != "photo" {
		t.Fatal(api.sent, in.taken)
	}

	*lang = "ru"
	link := private(5, "Запись семинара https://video.example.org/s5", owner)
	b.handle(ctx, link)
	if got := answers(api); got[len(got)-1] != "Сохранено во входящие." {
		t.Fatal(got)
	}
	b.handle(ctx, link) // the same message again: taken once, no answer
	if len(api.sent) != 2 {
		t.Fatal(answers(api))
	}

	b.handle(ctx, private(7, "", owner, document("video/mp4", "rec.mp4", maxFile+1)))
	if got := answers(api); got[len(got)-1] != "Не сохранено: the file is over 1 MB." {
		t.Fatal(got)
	}

	before := len(api.sent)
	for _, ignored := range []Event{private(8, "/help", owner), private(9, "чужое", 7),
		private(10, "", owner, document("image/webp", "", 10, &tg.DocumentAttributeSticker{Alt: "x"}))} {
		b.handle(ctx, ignored)
	}
	if len(api.sent) != before || len(in.taken) != 4 {
		t.Fatal(answers(api), len(in.taken))
	}

	b.OnMaterial = func(context.Context, Material, Download) (string, error) { return "", errors.New("boom") }
	b.handle(ctx, private(11, "note", owner))
	if got := answers(api); got[len(got)-1] != "Не сохранено: что-то пошло не так, подробности в журнале." {
		t.Fatal(got)
	}
}

// D69: the bot has no forum and no Mini App: /start, an old link's too, gives the web app's address.
func TestStartAnswersWithTheWebApp(t *testing.T) {
	b, api, _ := setup(t)
	for _, c := range []struct {
		sender int64
		text   string
	}{{owner, "/start"}, {owner, "/start ev-0123456789ab"}, {99, "/start"}, {owner, "/startx"}} {
		api.sent = nil
		b.handle(ctx, private(1, c.text, c.sender))
		if c.sender != owner || c.text == "/startx" {
			if len(api.sent) != 0 {
				t.Fatal(c, api.sent)
			}
			continue
		}
		want := sent{owner, "infomaster is a web app: https://example.test/web/", SendOptions{NoPreview: true,
			Button: &Button{Text: "Open", URL: "https://example.test/web/"}}}
		if !reflect.DeepEqual(api.sent, []sent{want}) {
			t.Fatal(api.sent)
		}
	}
	if !reflect.DeepEqual(api.menus, []int64{owner, owner}) { // the owner's menu button is reset
		t.Fatal(api.menus)
	}
}

// D73: a PDF read as a sheet is answered as one, every time; the same sheet again too.
func TestBotSaysWhereASheetWent(t *testing.T) {
	b, api, _ := setup(t)
	for _, c := range [][2]string{{"sheet", "Saved as a sheet: it is on the Sheets shelf."},
		{"known", "This sheet is on the shelf already."}} {
		b.OnMaterial = func(context.Context, Material, Download) (string, error) { return c[0], nil }
		api.sent = nil
		event := private(20, "ДЗ 5", owner, document("application/pdf", "hw5.pdf", 4), album(77))
		b.handle(ctx, event)
		b.handle(ctx, event) // a second message of the album is answered too
		if len(api.sent) != 2 || api.sent[1].text != c[1] || !api.sent[1].options.Silent {
			t.Fatal(api.sent)
		}
	}
}

func TestSendSpawnAndStop(t *testing.T) {
	b, api, _ := setup(t)
	if err := b.Send(ctx, "123456"); err != nil || api.sent[0] != (sent{owner, "123456", SendOptions{}}) {
		t.Fatal(api.sent, err)
	}
	stopped := make(chan struct{})
	b.Spawn(func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})
	if b.Done() != (<-chan struct{})(api.done) {
		t.Fatal("done")
	}
	b.Stop()
	<-stopped
	if !api.closed {
		t.Fatal("not closed")
	}
}

func TestPrivateEvent(t *testing.T) {
	e, ok := PrivateEvent(&tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: owner}}, tg.Entities{})
	if !ok || e.SenderID != owner || e.ChatID != owner {
		t.Fatal(e, ok)
	}
	for _, m := range []tg.MessageClass{&tg.Message{ID: 2, PeerID: &tg.PeerChat{ChatID: 5}},
		&tg.Message{ID: 3, PeerID: &tg.PeerChannel{ChannelID: 10}},
		&tg.MessageService{ID: 4, PeerID: &tg.PeerUser{UserID: owner}}} {
		if _, ok := PrivateEvent(m, tg.Entities{}); ok {
			t.Fatal(m)
		}
	}
}
