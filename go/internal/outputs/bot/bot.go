// Package bot is the bot (§8.3, D69). The web app (§8.6) took the place of the forum and the Mini App; the bot stays
// for two jobs:
//
//   - it sends the owner's login codes of the web app and the connector to the owner's private chat (Send);
//   - it takes material for the library (D58): a private message of the owner that is no command (a forward, a
//     photo, a file, a recording, a link, a note) goes to the library's inbox (OnMaterial), and the bot says so,
//     quietly; a PDF that is a sheet goes to the sheets with its caption instead (D73, processing/bot_sheets). Its
//     file is downloaded by the bot itself, straight to disk.
//
// /start answers with the web app's address, for an old link of the forum or the calendar too. Anyone else's
// messages are ignored. The twin never writes (D15).
//
// The session is gotd's, made from TG_BOT_TOKEN in its own file (SessionPath); Telethon's `bot.session` is not read.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gotd/td/tg"

	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/telegram"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library"
)

// SessionPath is the bot's gotd session under the data directory.
func SessionPath(dataDir string) string { return filepath.Join(dataDir, "bot.gotd.json") }

// Material is a private message of the owner as the twin's rows have it: what the intake (processing/bot_sheets,
// library feed from_bot) reads.
type Material struct {
	ChatID  int64
	MsgID   int64
	At      time.Time
	Text    string
	Files   []inbox.File
	Forward map[string]any // nil for none
}

// Download streams the message's file into path; false when there is none.
type Download func(ctx context.Context, path string) (bool, error)

// Event is a private message to the bot.
type Event struct {
	SenderID int64 // 0 when unknown
	ChatID   int64
	Message  *tg.Message
	Entities tg.Entities
}

// Button is a URL button under a message.
type Button struct{ Text, URL string }

// SendOptions of a message.
type SendOptions struct {
	Silent    bool
	NoPreview bool
	Button    *Button
}

// API is what the bot does on Telegram (gotd in production, a fake in the tests).
type API interface {
	Send(ctx context.Context, userID int64, text string, options SendOptions) error
	// ResetMenu sets the default menu button: for one user, or for everyone with userID 0.
	ResetMenu(ctx context.Context, userID int64) error
	ResetCommands(ctx context.Context) error
	Download(ctx context.Context, m *tg.Message, path string) (bool, error)
	Done() <-chan struct{}
	Close() error
}

// Bot is the bot.
type Bot struct {
	Env     config.Env
	Session string
	Lang    func() string
	// OnMaterial is the intake of the owner's private messages: where the message went ("sheet", "known", "inbox"),
	// or "" for nothing. A *library.Error is a file the library refuses.
	OnMaterial func(ctx context.Context, m Material, download Download) (string, error)
	// Connect logs the bot in from its token and passes private messages to the handler; gotd's unless a test
	// replaces it.
	Connect func(ctx context.Context, session string, handle func(context.Context, Event)) (API, error)

	mu       sync.Mutex
	api      API
	album    int64 // the album answered last: one answer for all its messages
	ctx      context.Context
	cancel   context.CancelFunc
	tasks    sync.WaitGroup
	stopOnce sync.Once
}

// New makes the bot; Start logs it in.
func New(env config.Env, session string, lang func() string) *Bot {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Bot{Env: env, Session: session, Lang: lang, ctx: ctx, cancel: cancel}
	b.Connect = func(ctx context.Context, session string, handle func(context.Context, Event)) (API, error) {
		return ConnectGotd(ctx, b.Env, session, handle)
	}
	return b
}

func (b *Bot) tr(en, ru string) string { return i18n.Tr(b.Lang(), en, ru) }

// AppURL is the web app's address.
func (b *Bot) AppURL() string { return b.Env.PublicURL + "/web/" }

// Start logs in and takes away the Mini App's menu button and the old commands.
func (b *Bot) Start(ctx context.Context) error {
	api, err := b.Connect(ctx, b.Session, b.handle)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.api = api
	b.mu.Unlock()
	if err := os.Chmod(b.Session, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := api.ResetMenu(ctx, 0); err != nil {
		slog.Info("the bot's menu not reset", "err", err)
	} else if err := api.ResetCommands(ctx); err != nil {
		slog.Info("the bot's menu not reset", "err", err)
	}
	return nil
}

func (b *Bot) current() API {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.api
}

// Send is a message to the owner's private chat: a login code. It fails until the owner has pressed Start there.
func (b *Bot) Send(ctx context.Context, text string) error {
	api := b.current()
	if api == nil {
		return errors.New("the bot is not connected")
	}
	return api.Send(ctx, b.Env.OwnerID, text, SendOptions{})
}

// Spawn runs a task in the background; failures go to the log. Stop cancels its context.
func (b *Bot) Spawn(task func(ctx context.Context) error) {
	b.tasks.Add(1)
	go func() {
		defer b.tasks.Done()
		if err := task(b.ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("a background task failed", "err", err)
		}
	}()
}

// Done is closed when the bot loses Telegram; nil before Start.
func (b *Bot) Done() <-chan struct{} {
	if api := b.current(); api != nil {
		return api.Done()
	}
	return nil
}

// Stop cancels the background tasks and disconnects.
func (b *Bot) Stop() {
	b.stopOnce.Do(func() {
		b.cancel()
		if api := b.current(); api != nil {
			if err := api.Close(); err != nil {
				slog.Info("the bot: closing the client", "err", err)
			}
		}
	})
}

// isStart is Python's re.match(r"^/start\b"): /start not followed by a word character.
func isStart(text string) bool {
	rest, ok := strings.CutPrefix(text, "/start")
	if !ok {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_')
}

// handle is a private message: /start first, then the intake (Telethon ran both handlers).
func (b *Bot) handle(ctx context.Context, e Event) {
	if isStart(e.Message.Message) {
		b.OnStart(ctx, e)
	}
	if !e.Message.Out {
		b.OnPrivate(ctx, e)
	}
}

func (b *Bot) respond(ctx context.Context, e Event, text string, options SendOptions) {
	api := b.current()
	if api == nil {
		return
	}
	if err := api.Send(ctx, e.ChatID, text, options); err != nil {
		slog.Error("the bot's answer failed", "chat", e.ChatID, "err", err)
	}
}

// OnStart answers /start of the owner with the web app's address.
func (b *Bot) OnStart(ctx context.Context, e Event) {
	if e.SenderID != b.Env.OwnerID {
		return
	}
	if api := b.current(); api != nil {
		// The Mini App's button was set for the owner alone: taken away for them too.
		if err := api.ResetMenu(ctx, e.SenderID); err != nil {
			slog.Info("the owner's menu button not reset", "err", err)
		}
	}
	url := b.AppURL()
	b.respond(ctx, e, b.tr("infomaster is a web app: "+url, "infomaster — веб-приложение: "+url),
		SendOptions{NoPreview: true, Button: &Button{Text: b.tr("Open", "Открыть"), URL: url}})
}

// OnPrivate takes a private message of the owner that is no command into the library's inbox (D58). Each message of
// an album is an item of its own; the album gets one answer.
func (b *Bot) OnPrivate(ctx context.Context, e Event) {
	if e.SenderID != b.Env.OwnerID || b.OnMaterial == nil || strings.HasPrefix(e.Message.Message, "/") {
		return
	}
	m := e.Message
	b.mu.Lock()
	album := m.GroupedID
	first := album == 0 || album != b.album
	b.album = album // before the intake: the album's other messages come at once
	b.mu.Unlock()
	material := Material{ChatID: e.ChatID, MsgID: int64(m.ID), At: telegram.Date(m.Date),
		Text: telegram.MessageText(m), Files: telegram.MessageFiles(m), Forward: telegram.MessageForward(m, e.Entities)}
	download := func(ctx context.Context, path string) (bool, error) {
		api := b.current()
		if api == nil {
			return false, errors.New("the bot is not connected")
		}
		return api.Download(ctx, m, path)
	}
	saved, err := b.OnMaterial(ctx, material, download)
	var answer string
	var refused *library.Error
	switch {
	case errors.As(err, &refused): // the library refuses it: too big, empty
		answer = b.tr(fmt.Sprintf("Not saved: %s.", refused.Msg), fmt.Sprintf("Не сохранено: %s.", refused.Msg))
	case err != nil:
		slog.Error(fmt.Sprintf("message %d to the bot did not go to the library", m.ID), "err", err)
		answer = b.tr("Not saved: something went wrong, see the log.",
			"Не сохранено: что-то пошло не так, подробности в журнале.")
	case saved == "" || (saved != "sheet" && saved != "known" && !first):
		return
	case saved == "sheet":
		answer = b.tr("Saved as a sheet: it is on the Sheets shelf.", "Сохранено как листочек: он на полке «Листочки».")
	case saved == "known":
		answer = b.tr("This sheet is on the shelf already.", "Этот листочек уже на полке.")
	default:
		answer = b.tr("Saved to the inbox.", "Сохранено во входящие.")
	}
	b.respond(ctx, e, answer, SendOptions{Silent: true})
}
