package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const channel = -1000000000010 // marked id of PeerChannel(10)

var ctx = context.Background()

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func message(text string, id int, edit ...func(*tg.Message)) *tg.Message {
	m := &tg.Message{ID: id, PeerID: &tg.PeerChannel{ChannelID: 10}, Date: int(clock.NowUTC().Unix()), Message: text}
	for _, e := range edit {
		e(m)
	}
	return m
}

func document(mime string, attributes ...tg.DocumentAttributeClass) *tg.MessageMediaDocument {
	return &tg.MessageMediaDocument{Document: &tg.Document{ID: 1, AccessHash: 1, MimeType: mime, Size: 2048, DCID: 2,
		Attributes: attributes}}
}

func TestHiddenLinks(t *testing.T) {
	text := "😀 Форма и ещё ссылка"
	m := message(text, 5, func(m *tg.Message) {
		m.Entities = []tg.MessageEntityClass{
			&tg.MessageEntityTextURL{Offset: 3, Length: 5, URL: "https://forms.gle/a"}, // 😀 is 2 UTF-16 units
			&tg.MessageEntityTextURL{Offset: 15, Length: 6, URL: "https://b.ru"},
		}
	})
	if got := MessageText(m); got != "😀 Форма (https://forms.gle/a) и ещё ссылка (https://b.ru)" {
		t.Fatal(got)
	}
	if got := MessageText(message("просто текст", 5)); got != "просто текст" {
		t.Fatal(got)
	}
}

func TestFiles(t *testing.T) {
	pdf := message("", 5, func(m *tg.Message) {
		m.Media = document("application/pdf", &tg.DocumentAttributeFilename{FileName: "hw5.pdf"})
	})
	want := []inbox.File{{"kind": "pdf", "name": "hw5.pdf", "mime": "application/pdf", "size": 2048}}
	if got := MessageFiles(pdf); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	voice := message("", 5, func(m *tg.Message) {
		m.Media = document("audio/ogg", &tg.DocumentAttributeAudio{Duration: 3, Voice: true})
	})
	if got := MessageFiles(voice); got[0]["kind"] != "voice" || got[0]["name"] != nil {
		t.Fatal(got)
	}
	photo := message("", 5, func(m *tg.Message) {
		m.Media = &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 1, AccessHash: 1, DCID: 2, Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "m", W: 320, H: 240, Size: 900},
			&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 960, Sizes: []int{300, 5000}}}}}
	})
	if got := MessageFiles(photo); !reflect.DeepEqual(got, []inbox.File{{"kind": "photo", "mime": "image/jpeg",
		"size": 5000}}) { // the largest
		t.Fatal(got)
	}
	if location, _ := MediaLocation(photo); location.(*tg.InputPhotoFileLocation).ThumbSize != "y" {
		t.Fatal(location)
	}
	if got := MessageFiles(message("текст", 5)); len(got) != 0 {
		t.Fatal(got)
	}
	// A link preview's picture or file is no file of the message.
	preview := &tg.WebPage{ID: 1, URL: "https://hse.ru", DisplayURL: "hse.ru", Photo: &tg.Photo{ID: 2, AccessHash: 1,
		DCID: 2, Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "m", W: 1, H: 1, Size: 900}}}}
	withPreview := func(m *tg.Message) { m.Media = &tg.MessageMediaWebPage{Webpage: preview} }
	if got := MessageFiles(message("https://hse.ru", 5, withPreview)); len(got) != 0 {
		t.Fatal(got)
	}
	preview.Photo, preview.Document = nil, document("application/pdf").Document
	if got := MessageFiles(message("https://hse.ru", 5, withPreview)); len(got) != 0 {
		t.Fatal(got)
	}
	// Telethon's rules: the first attribute of a kind decides; a video note is a round video.
	note := message("", 5, func(m *tg.Message) {
		m.Media = document("video/mp4", &tg.DocumentAttributeVideo{RoundMessage: true})
	})
	if got := MessageFiles(note)[0]["kind"]; got != "video_note" {
		t.Fatal(got)
	}
	named := message("", 5, func(m *tg.Message) {
		m.Media = document("application/octet-stream", &tg.DocumentAttributeFilename{FileName: "Notes.PDF"})
	})
	if got := MessageFiles(named)[0]["kind"]; got != "pdf" {
		t.Fatal(got)
	}
}

func TestForumTopics(t *testing.T) {
	check := func(header *tg.MessageReplyHeader, topic, reply *int64) {
		t.Helper()
		f := MessageFields(message("x", 5, func(m *tg.Message) { m.ReplyTo = header }), tg.Entities{})
		if !reflect.DeepEqual(f.TopicID, topic) || !reflect.DeepEqual(f.ReplyTo, reply) {
			t.Fatal(f.TopicID, f.ReplyTo)
		}
	}
	p := func(n int64) *int64 { return &n }
	check(&tg.MessageReplyHeader{ReplyToMsgID: 40, ForumTopic: true}, p(40), nil)
	check(&tg.MessageReplyHeader{ReplyToMsgID: 55, ReplyToTopID: 40, ForumTopic: true}, p(40), p(55))
	check(&tg.MessageReplyHeader{ReplyToMsgID: 55}, nil, p(55))
}

func TestForward(t *testing.T) {
	forwarded := message("пост", 5, func(m *tg.Message) {
		m.FwdFrom = tg.MessageFwdHeader{Date: int(clock.NowUTC().Unix()), FromID: &tg.PeerChannel{ChannelID: 20},
			ChannelPost: 7, PostAuthor: "Деканат"}
	})
	want := map[string]any{"from": "Деканат", "chat_id": int64(-1000000000020), "msg_id": 7}
	if got := MessageFields(forwarded, tg.Entities{}).Forward; !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	// Without a name in the header the source's own name, if it came with the message.
	bare := message("пост", 5, func(m *tg.Message) {
		m.FwdFrom = tg.MessageFwdHeader{Date: 1, FromID: &tg.PeerChannel{ChannelID: 20}}
	})
	entities := tg.Entities{Channels: map[int64]*tg.Channel{20: {ID: 20, Title: "Новости"}}}
	if got := MessageForward(bare, entities)["from"]; got != "Новости" {
		t.Fatal(got)
	}
	if got := MessageForward(message("не пересланное", 5), entities); got != nil {
		t.Fatal(got)
	}
}

func TestChatKind(t *testing.T) {
	cases := map[string]any{
		"bot":        &tg.User{ID: 1, Bot: true},
		"group":      &tg.Chat{ID: 1, Title: "g"},
		"channel":    &tg.Channel{ID: 1, Title: "c", Broadcast: true},
		"forum":      &tg.Channel{ID: 1, Title: "c", Megagroup: true, Forum: true},
		"supergroup": &tg.Channel{ID: 1, Title: "c", Megagroup: true},
	}
	for want, entity := range cases {
		if got := ChatKind(entity); got != want {
			t.Fatal(want, got)
		}
	}
	if got := ChatTitle(&tg.ChatForbidden{ID: 5}); got != "-5" {
		t.Fatal(got)
	}
	if got := Masked("+79161234567"); got != "+7916•••••67" {
		t.Fatal(got)
	}
}

// --- fakes ------------------------------------------------------------------------

// fakeClient is a logged-in connection: messages by id for get_messages, files written by Download.
type fakeClient struct {
	me        *tg.User
	handler   Handler
	messages  map[int]*tg.Message
	err       error // of Message
	dialogs   []Dialog
	history   map[int64][]*tg.Message
	connected bool
	done      chan struct{}
	session   string
}

func newFake(session string) *fakeClient {
	return &fakeClient{me: &tg.User{ID: 777, FirstName: "Twin", Username: "twin_acc"}, connected: true,
		done: make(chan struct{}), session: session, messages: map[int]*tg.Message{}}
}

func (f *fakeClient) Self(context.Context) (*tg.User, error) { return f.me, nil }

func (f *fakeClient) Dialogs(_ context.Context, fn func(Dialog) error) error {
	for _, d := range f.dialogs {
		if err := fn(d); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeClient) History(_ context.Context, d Dialog, minID int, fn func(Msg) error) error {
	for _, m := range f.history[d.ID] {
		if m.ID > minID {
			if err := fn(Msg{Message: m}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeClient) Message(_ context.Context, _ int64, msgID int) (*Msg, error) {
	if f.err != nil {
		return nil, f.err
	}
	if m, ok := f.messages[msgID]; ok {
		return &Msg{Message: m}, nil
	}
	return nil, nil
}

func (f *fakeClient) Entity(context.Context, int64) (any, error) { return nil, errors.New("unknown") }

func (f *fakeClient) Download(_ context.Context, m *tg.Message, path string) (bool, error) {
	return true, os.WriteFile(path, []byte("file"), 0o600)
}

func (f *fakeClient) LogOut(context.Context) error {
	f.connected = false
	return os.Remove(f.session)
}

func (f *fakeClient) Connected() bool       { return f.connected }
func (f *fakeClient) Done() <-chan struct{} { return f.done }
func (f *fakeClient) Close() error {
	f.connected = false
	return nil
}

// fakeLogin is the login: the code 12345, the cloud password pw when password is set.
type fakeLogin struct {
	session   string
	password  bool
	connected bool
	signed    bool
	me        *tg.User
}

func (f *fakeLogin) SendCode(_ context.Context, phone string) (string, error) {
	if phone == "+70000000000" {
		return "", tgerr.New(400, "PHONE_NUMBER_INVALID")
	}
	return "hash", nil
}

func (f *fakeLogin) SignIn(_ context.Context, _, code, hash string) error {
	if hash != "hash" {
		panic("hash")
	}
	if code != "12345" {
		return tgerr.New(400, "PHONE_CODE_INVALID")
	}
	if f.password {
		return auth.ErrPasswordAuthNeeded
	}
	f.signed = true
	return nil
}

func (f *fakeLogin) Password(_ context.Context, password string) error {
	if password != "pw" {
		return auth.ErrPasswordInvalid
	}
	f.signed = true
	return nil
}

func (f *fakeLogin) Self(context.Context) (*tg.User, error) {
	if !f.signed {
		return nil, tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	}
	return f.me, nil
}

func (f *fakeLogin) LogOut(context.Context) error {
	f.connected = false
	return os.Remove(f.session)
}

func (f *fakeLogin) Close() error {
	f.connected = false
	return nil
}

type fixture struct {
	twin     *Twin
	notices  []string
	logins   []*fakeLogin
	clients  []*fakeClient
	password bool
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &fixture{}
	in := inbox.Open(s)
	f.twin = New(config.Env{APIID: 1, APIHash: "h", OwnerID: 42}, in, settings.NewChats(s),
		filepath.Join(t.TempDir(), "twin.gotd.json"),
		func(p i18n.Phrase) { f.notices = append(f.notices, p.Ru) }) // the notifier picks the language
	me := &tg.User{ID: 777, FirstName: "Twin", Username: "twin_acc"}
	f.twin.Dial = func(_ context.Context, session string) (LoginClient, error) {
		if err := os.WriteFile(session, nil, 0o644); err != nil { // Telethon's connect makes the file
			return nil, err
		}
		login := &fakeLogin{session: session, password: f.password, connected: true, me: me}
		f.logins = append(f.logins, login)
		return login, nil
	}
	f.twin.Connect = func(_ context.Context, session string, h Handler) (Client, error) {
		if _, err := os.Stat(session); err != nil {
			return nil, ErrNotAuthorized
		}
		c := newFake(session)
		c.me, c.handler = me, h
		f.clients = append(f.clients, c)
		return c, nil
	}
	must(in.PutChat(channel, "Матан", "supergroup", nil, 4))
	return f
}

func (f *fixture) enable(t *testing.T, chatID int64) {
	t.Helper()
	must(f.twin.chats.Update(chatID, map[string]any{"enabled": true}))
}

func (f *fixture) message(t *testing.T, msgID int64) *inbox.Message {
	t.Helper()
	return must(f.twin.inbox.Message(channel, msgID, "tg"))
}

func (f *fixture) has(t *testing.T, chatID, msgID int64) bool {
	t.Helper()
	return must(f.twin.inbox.Has(chatID, msgID, "tg"))
}

// live is a message as an update brings it: from the starosta, who came with it.
func live(m *tg.Message) Msg {
	m.FromID = &tg.PeerUser{UserID: 7}
	return Msg{Message: m, Entities: tg.Entities{Users: map[int64]*tg.User{7: {ID: 7, FirstName: "Староста"}}}}
}

// --- the twin ------------------------------------------------------------------------

func TestLiveMessages(t *testing.T) {
	f := setup(t)
	f.twin.OnNew(ctx, channel, live(message("до включения", 5)))
	if f.has(t, channel, 5) || must(f.twin.inbox.Chat(channel)).LastID != 5 { // disabled: only moves on
		t.Fatal("stored")
	}

	f.enable(t, channel)
	fake := newFake(f.twin.Session)
	fake.messages[2] = message("Консультация в пятницу в R405", 2)
	f.twin.client = fake
	reply := message("А во сколько?", 6, func(m *tg.Message) { m.ReplyTo = &tg.MessageReplyHeader{ReplyToMsgID: 2} })
	f.twin.OnNew(ctx, channel, live(reply))
	stored := f.message(t, 6)
	if *stored.SenderName != "Староста" || *stored.ReplyQuote != "Консультация в пятницу в R405" {
		t.Fatal(*stored.SenderName, stored.ReplyQuote)
	}
	if must(f.twin.inbox.Chat(channel)).LastID != 6 {
		t.Fatal("last_id")
	}

	f.twin.OnNew(ctx, channel, live(message("", 7))) // nothing to keep: no text, no file
	if f.has(t, channel, 7) {
		t.Fatal("empty stored")
	}
	if f.twin.Status()["ok_at"] == nil {
		t.Fatal("ok_at")
	}
}

func TestNewChatIsAnnouncedAndOff(t *testing.T) {
	f := setup(t)
	m := live(message("привет", 3, func(m *tg.Message) { m.PeerID = &tg.PeerChannel{ChannelID: 99} }))
	m.Entities.Channels = map[int64]*tg.Channel{99: {ID: 99, Title: "Новости ВШЭ", Broadcast: true, Username: "hse"}}
	f.twin.OnNew(ctx, -1000000000099, m)
	chat := must(f.twin.inbox.Chat(-1000000000099))
	if chat.Kind != "channel" || *chat.Username != "hse" || chat.LastID != 3 {
		t.Fatal(chat)
	}
	if f.has(t, -1000000000099, 3) || !strings.Contains(f.notices[0], "Новости ВШЭ") {
		t.Fatal(f.notices)
	}
}

func TestEditAndPin(t *testing.T) {
	f := setup(t)
	f.enable(t, channel)
	now := clock.NowUTC()
	days := func(n int) int { return int(now.Add(-time.Duration(n) * 24 * time.Hour).Unix()) }
	f.twin.OnNew(ctx, channel, live(message("ДЗ до 22.10", 6)))
	f.twin.OnEdit(ctx, channel, live(message("ДЗ до 24.10", 6, func(m *tg.Message) { m.EditDate = days(0) })))
	if f.message(t, 6).Text != "ДЗ до 24.10" {
		t.Fatal(f.message(t, 6).Text)
	}

	f.twin.OnEdit(ctx, channel, live(message("старое, но правленое", 3, func(m *tg.Message) { m.Date = days(2) })))
	if !f.has(t, channel, 3) {
		t.Fatal("older not stored")
	}
	// No new edit_date: a reaction or a link preview.
	f.twin.OnEdit(ctx, channel, live(message("совсем старое", 2, func(m *tg.Message) { m.Date = days(30) })))
	if f.has(t, channel, 2) {
		t.Fatal("ancient stored")
	}
	f.twin.OnEdit(ctx, channel, live(message("правлено давно", 2, func(m *tg.Message) {
		m.Date, m.EditDate = days(30), days(20)
	})))
	if f.has(t, channel, 2) {
		t.Fatal("stale stored")
	}

	missed := now.Add(-3 * 24 * time.Hour).Truncate(time.Second) // edited while the twin was offline, a reaction now
	f.twin.OnEdit(ctx, channel, live(message("правлено без твинка", 1, func(m *tg.Message) {
		m.Date, m.EditDate = days(43), int(missed.Unix())
	})))
	if got := f.message(t, 1).EditedAt; got == nil || *got != clock.LocalISO(missed) {
		t.Fatal(got)
	}

	justNow := now.Truncate(time.Second)
	f.twin.OnEdit(ctx, channel, live(message("Дедлайн перенесён на 30.11", 2, func(m *tg.Message) {
		m.Date, m.EditDate = days(30), int(justNow.Unix())
	}))) // past its 14 days, edited just now (D74): stored, kept 14 days from the edit
	stored := f.message(t, 2)
	if stored.Text != "Дедлайн перенесён на 30.11" || stored.ParsedAt != nil || *stored.EditedAt != clock.LocalISO(justNow) {
		t.Fatal(stored)
	}
	must(f.twin.inbox.Prune(justNow.Add(13 * 24 * time.Hour)))
	if !f.has(t, channel, 2) {
		t.Fatal("pruned")
	}

	f.twin.OnPin(ctx, channel, []int{6}, true)
	if !f.message(t, 6).Pinned {
		t.Fatal("not pinned")
	}
	f.twin.OnDelete(ctx, []int{6}, func() *int64 { c := int64(channel); return &c }())
	if f.message(t, 6).DeletedAt == nil {
		t.Fatal("not deleted")
	}
	f.twin.OnTitle(ctx, channel, "Матан 2")
	if must(f.twin.inbox.Chat(channel)).Title != "Матан 2" {
		t.Fatal("not renamed")
	}
}

func TestNoSessionNeedsLogin(t *testing.T) {
	f := setup(t)
	if ok, err := f.twin.Start(ctx); ok || err != nil {
		t.Fatal(ok, err)
	}
	if f.twin.Status()["login_needed"] != true || !strings.Contains(f.notices[0], "login") {
		t.Fatal(f.twin.Status(), f.notices)
	}
}

// Files.download: the message's own file; NotNow when it cannot be done now, false when it is gone.
func TestDownload(t *testing.T) {
	f := setup(t)
	path := filepath.Join(t.TempDir(), "f.part")
	var notNow *NotNow
	if _, err := f.twin.Download(ctx, channel, 5, path, 0); !errors.As(err, &notNow) { // not logged in
		t.Fatal(err)
	}
	photo := message("", 5, func(m *tg.Message) {
		m.Media = &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 1, AccessHash: 1, DCID: 2}}
	})
	preview := message("https://hse.ru", 6, func(m *tg.Message) {
		m.Media = &tg.MessageMediaWebPage{Webpage: &tg.WebPage{ID: 1, URL: "https://hse.ru", DisplayURL: "hse.ru",
			Photo: photo.Media.(*tg.MessageMediaPhoto).Photo}}
	})
	fake := newFake(f.twin.Session)
	fake.messages = map[int]*tg.Message{5: photo, 6: preview}
	f.twin.client = fake
	if ok, err := f.twin.Download(ctx, channel, 5, path, 0); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := f.twin.Download(ctx, channel, 6, path, 0); ok { // a link preview is no file of the message
		t.Fatal("preview")
	}
	if ok, _ := f.twin.Download(ctx, channel, 7, path, 0); ok { // deleted
		t.Fatal("deleted")
	}
	big := message("", 8, func(m *tg.Message) { m.Media = document("application/pdf") })
	fake.messages[8] = big
	if ok, err := f.twin.Download(ctx, channel, 8, path, 1000); ok || err != nil { // over the limit now
		t.Fatal(ok, err)
	}
	fake.err = tgerr.New(420, "FLOOD_WAIT_30")
	if _, err := f.twin.Download(ctx, channel, 5, path, 0); !errors.As(err, &notNow) {
		t.Fatal(err)
	}
	private := tgerr.New(400, "CHANNEL_PRIVATE")
	fake.err = private
	// Closed to the twin: final, Files keeps it as the file's error.
	if _, err := f.twin.Download(ctx, channel, 5, path, 0); !errors.Is(err, private) || errors.As(err, &notNow) {
		t.Fatal(err)
	}
	fake.connected = false
	if _, err := f.twin.Download(ctx, channel, 5, path, 0); !errors.As(err, &notNow) {
		t.Fatal(err)
	}
}

func reason(err error) string {
	if err == nil {
		return ""
	}
	return i18n.Reason(err, "en")
}

func expect(t *testing.T, err error, text string) {
	t.Helper()
	var login *LoginError
	if !errors.As(err, &login) || !strings.Contains(reason(err), text) {
		t.Fatalf("want %q, got %v", text, err)
	}
}

func changed(twin *Twin) bool {
	select {
	case <-twin.Changed():
		return true
	default:
		return false
	}
}

func TestLoginAndLogout(t *testing.T) {
	f := setup(t)
	twin := f.twin
	expect(t, twin.SignIn(ctx, "12345", ""), "ask for a code first")
	expect(t, twin.SendCode(ctx, "+7 000 000-00-00"), "does not know")
	if err := twin.SendCode(ctx, "+7 (916) 123-45-67"); err != nil {
		t.Fatal(err)
	}
	if got := twin.Status()["login"]; !reflect.DeepEqual(got, map[string]any{"step": "code", "phone": "+7916•••••67"}) {
		t.Fatal(got)
	}
	expect(t, twin.SignIn(ctx, "11111", ""), "wrong code")
	if twin.LoggedIn() || twin.Status()["login"].(map[string]any)["step"] != "code" { // one more try, same code
		t.Fatal(twin.Status())
	}

	if err := twin.SignIn(ctx, "1 2 3 4 5", ""); err != nil {
		t.Fatal(err)
	}
	status := twin.Status()
	if !reflect.DeepEqual(status["account"], map[string]any{"id": int64(777), "name": "Twin", "username": "twin_acc"}) ||
		status["login"] != nil || status["login_needed"] != false || status["error"] != nil || !changed(twin) {
		t.Fatal(status)
	}
	if f.clients[0].handler != Handler(twin) || !strings.Contains(f.notices[len(f.notices)-1], "Твинк вошёл: Twin") {
		t.Fatal(f.notices)
	}
	if info, err := os.Stat(twin.Session); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
	if f.logins[0].connected {
		t.Fatal("the login client is left open")
	}
	expect(t, twin.SendCode(ctx, "+79161234567"), "logged in already")

	if err := twin.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(twin.Session); twin.LoggedIn() || !errors.Is(err, os.ErrNotExist) || !changed(twin) {
		t.Fatal("still logged in")
	}
	if twin.Status()["login_needed"] != true || twin.Status()["account"] != nil {
		t.Fatal(twin.Status())
	}
	if !strings.Contains(f.notices[len(f.notices)-1], "вышел из аккаунта Twin") {
		t.Fatal(f.notices)
	}
	expect(t, twin.Logout(ctx), "not logged in anyway")
}

func TestLoginWithACloudPassword(t *testing.T) {
	f := setup(t)
	f.password = true
	twin := f.twin
	must(0, twin.SendCode(ctx, "+79161234567"))
	must(0, twin.SignIn(ctx, "12345", ""))
	if twin.Status()["login"].(map[string]any)["step"] != "password" || twin.LoggedIn() {
		t.Fatal(twin.Status())
	}
	expect(t, twin.SignIn(ctx, "", "nope"), "wrong cloud password")
	must(0, twin.SignIn(ctx, "", "pw"))
	if twin.Status()["account"].(map[string]any)["name"] != "Twin" {
		t.Fatal(twin.Status())
	}

	// A new code drops the pending login; cancelling drops it too.
	must(0, twin.Logout(ctx))
	must(0, twin.SendCode(ctx, "+79161234567"))
	must(0, twin.SendCode(ctx, "+79161234567"))
	if n := len(f.logins); f.logins[n-2].connected || !f.logins[n-1].connected {
		t.Fatal("the older login is open")
	}
	twin.CancelLogin()
	if twin.Status()["login"] != nil || f.logins[len(f.logins)-1].connected {
		t.Fatal(twin.Status())
	}
}

func TestTheOwnersAccountIsNoTwin(t *testing.T) {
	f := setup(t)
	f.twin.Env.OwnerID = 777
	must(0, f.twin.SendCode(ctx, "+79161234567"))
	expect(t, f.twin.SignIn(ctx, "12345", ""), "this is no twin")
	if _, err := os.Stat(f.twin.Session); f.twin.LoggedIn() || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("logged in")
	}
}

// Not in the Python tests: the start lists the chats, catches up an enabled one oldest first, and keeps a chat's
// last_id when its catch-up fails.
func TestStartCatchesUp(t *testing.T) {
	f := setup(t)
	f.enable(t, channel)
	must(0, os.WriteFile(f.twin.Session, nil, 0o644))
	closed := int64(-1000000000030)
	must(f.twin.inbox.PutChat(closed, "Закрытый", "supergroup", nil, 1))
	f.enable(t, closed)
	f.twin.Connect = func(_ context.Context, session string, h Handler) (Client, error) {
		c := newFake(session)
		c.dialogs = []Dialog{
			{ID: channel, Entity: &tg.Channel{ID: 10, Title: "Матан", Megagroup: true}, Top: 7},
			{ID: closed, Entity: &tg.Channel{ID: 30, Title: "Закрытый", Megagroup: true}, Top: 9},
			{ID: 55, Entity: &tg.User{ID: 55, FirstName: "Новый"}, Top: 3},
		}
		reply := message("ответ", 7, func(m *tg.Message) { m.ReplyTo = &tg.MessageReplyHeader{ReplyToMsgID: 5} })
		c.history = map[int64][]*tg.Message{channel: {message("старое", 4), message("вопрос", 5), message("", 6),
			reply}}
		c.err = errors.New("no request for the replied message: it is stored")
		f.clients = append(f.clients, c)
		return &closedChat{c, closed}, nil
	}
	if ok, err := f.twin.Start(ctx); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if f.has(t, channel, 4) || !f.has(t, channel, 5) || f.has(t, channel, 6) || !f.has(t, channel, 7) {
		t.Fatal("caught up wrong")
	}
	if must(f.twin.inbox.Chat(channel)).LastID != 7 || must(f.twin.inbox.Chat(closed)).LastID != 1 {
		t.Fatal("last_id")
	}
	if must(f.twin.inbox.Chat(55)).Kind != "user" || !strings.Contains(f.notices[0], "Новый") {
		t.Fatal(f.notices)
	}
	if err := f.twin.Status()["error"]; err == nil || !strings.Contains(err.(i18n.Phrase).En, "Закрытый") {
		t.Fatal(err)
	}
}

// closedChat fails the history of one chat as Telegram does for a chat closed to the twin.
type closedChat struct {
	*fakeClient
	chat int64
}

func (c *closedChat) History(ctx context.Context, d Dialog, minID int, fn func(Msg) error) error {
	if d.ID == c.chat {
		return tgerr.New(400, "CHANNEL_PRIVATE")
	}
	return c.fakeClient.History(ctx, d, minID, fn)
}

// The one-time login from a terminal: a wrong code and a wrong password are asked again.
func TestTerminalLogin(t *testing.T) {
	f := setup(t)
	f.password = true
	var out strings.Builder
	dial := func(ctx context.Context, _ config.Env, session string) (LoginClient, error) {
		return f.twin.Dial(ctx, session)
	}
	in := strings.NewReader("+79161234567\n11111\n12345\nnope\npw\n")
	if err := Login(ctx, f.twin.Env, f.twin.Session, in, &out, dial); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "The twin logged in: Twin (id 777). Restart the service.\n") ||
		!strings.Contains(out.String(), "Invalid code") || !strings.Contains(out.String(), "Invalid password") {
		t.Fatal(out.String())
	}
	if info, err := os.Stat(f.twin.Session); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
	f.twin.Env.OwnerID = 777
	err := Login(ctx, f.twin.Env, f.twin.Session, strings.NewReader("+79161234567\n12345\npw\n"), &out, dial)
	if err == nil || !strings.Contains(err.Error(), "this is no twin") {
		t.Fatal(err)
	}
}

func TestPhoneNumber(t *testing.T) {
	for in, want := range map[string]string{
		"+7 912 345-67-89": "+79123456789",
		"8 912 345-67-89":  "+79123456789",
		"9123456789":       "+79123456789",
		"79123456789":      "+79123456789",
		"+8 912 345 67 89": "+89123456789", // its own + is kept as typed
		"+44 20 7946 0958": "+442079460958",
	} {
		if got := PhoneNumber(in); got != want {
			t.Errorf("PhoneNumber(%q) = %q, want %q", in, got, want)
		}
	}
}
