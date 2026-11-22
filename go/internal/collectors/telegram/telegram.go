// Package telegram is the twin: a separate Telegram account that reads chats and channels (§5.1). It never writes
// anywhere (D15).
//
// Every chat of the account is listed in `chats`, a new one disabled (D11). Messages are stored only from enabled
// chats: new ones live, edits, deletions and pins as they happen. After a restart each enabled chat is caught up from
// its `last_id`; edits and deletions made while the service was down are not caught up. A file (a PDF, an image) is
// downloaded only when a batch asks for it (collectors/files).
//
// The account logs in and out in the web app, Machine → Twin and chats: the phone, the code Telegram sends to the
// account, the cloud password if it has one (SendCode, SignIn, Logout). The service runs on without it and starts
// reading as soon as the login is done, without a restart. A terminal works too (Login).
//
// The session is gotd's, in its own file (SessionPath); Telethon's `twin.session` of the Python service is never read
// or touched: the twin logs in again at the switch to Go.
package telegram

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

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tgerr"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

// QuoteLimit: characters of a replied message that is not stored itself.
const QuoteLimit = 500

// LoginHint is the terminal login command the notices name; the binary's main may replace it.
var LoginHint = "docker compose run --rm infomaster infomaster login"

// SessionPath is the twin's gotd session under the data directory.
func SessionPath(dataDir string) string { return filepath.Join(dataDir, "twin.gotd.json") }

// LoginError is a step of the login that did not go; its Phrase is for the owner in the web app. i18n.Reason reads
// it in the request's language.
type LoginError struct{ Phrase i18n.Phrase }

func (e *LoginError) Error() string { return e.Phrase.En }

// As lets errors.As find it as an *i18n.Error.
func (e *LoginError) As(target any) bool {
	if t, ok := target.(**i18n.Error); ok {
		*t = &i18n.Error{Phrase: e.Phrase}
		return true
	}
	return false
}

func loginError(en, ru string) error { return &LoginError{Phrase: i18n.P(en, ru)} }

// NotNow: the download cannot be done now (the twin is offline, or it failed on the way); the message waits for its
// next batch. The app's wiring turns it into collectors/files' NotNow.
type NotNow struct{ Msg string }

func (e *NotNow) Error() string { return e.Msg }

// pending is a login under way: the client that asked for the code, until it signs in or is dropped.
type pending struct {
	client   LoginClient
	phone    string
	codeHash string
	step     string // code, password
}

// Twin is the account.
type Twin struct {
	Env     config.Env
	inbox   *inbox.Inbox
	chats   *settings.ChatSettings
	Session string // the gotd session file

	// Notify is a line for the owner (Web Push).
	Notify func(i18n.Phrase)
	// NotifyLogin is the same, with a button to the login screen; Notify when nil.
	NotifyLogin func(i18n.Phrase)
	// Dial connects a client for a login; Connect starts a logged-in one that passes live updates to the handler
	// (ErrNotAuthorized when the session holds no login). Both are gotd's unless a test replaces them.
	Dial    func(ctx context.Context, session string) (LoginClient, error)
	Connect func(ctx context.Context, session string, h Handler) (Client, error)

	mu          sync.Mutex // the state below
	client      Client
	okAt        string
	err         *i18n.Phrase
	loginNeeded bool
	account     map[string]any
	pending     *pending

	loginMu sync.Mutex // one login step at a time
	changed chan struct{}
}

// New makes the twin; Start connects it.
func New(env config.Env, in *inbox.Inbox, chats *settings.ChatSettings, session string, notify func(i18n.Phrase)) *Twin {
	t := &Twin{Env: env, inbox: in, chats: chats, Session: session, Notify: notify, changed: make(chan struct{}, 1)}
	t.Dial = func(ctx context.Context, session string) (LoginClient, error) {
		return DialGotd(ctx, t.Env, session)
	}
	t.Connect = func(ctx context.Context, session string, h Handler) (Client, error) {
		return ConnectGotd(ctx, t.Env, session, h)
	}
	return t
}

// Changed gets a value when the client changes: a login or a logout (the app's watcher listens).
func (t *Twin) Changed() <-chan struct{} { return t.changed }

// Done is closed when the current client loses Telegram; nil (never ready) without a client.
func (t *Twin) Done() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == nil {
		return nil
	}
	return t.client.Done()
}

// LoggedIn tells whether there is a client.
func (t *Twin) LoggedIn() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.client != nil
}

func (t *Twin) signalChanged() {
	select {
	case t.changed <- struct{}{}:
	default:
	}
}

func (t *Twin) notify(text i18n.Phrase) {
	if t.Notify != nil {
		t.Notify(text)
	}
}

func phrase(en, ru string) *i18n.Phrase {
	p := i18n.P(en, ru)
	return &p
}

func optional(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Status is status(): ok_at, error (a Phrase, for i18n.Localize), login_needed, account, login.
func (t *Twin) Status() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	var login, account, errValue any
	if t.pending != nil {
		login = map[string]any{"step": t.pending.step, "phone": Masked(t.pending.phone)}
	}
	if t.account != nil {
		account = t.account
	}
	if t.err != nil {
		errValue = *t.err
	}
	return map[string]any{"ok_at": optional(t.okAt), "error": errValue, "login_needed": t.loginNeeded,
		"account": account, "login": login}
}

func (t *Twin) setError(p *i18n.Phrase) {
	t.mu.Lock()
	t.err = p
	t.mu.Unlock()
}

func (t *Twin) touch() {
	t.mu.Lock()
	t.okAt = clock.LocalISO(clock.NowUTC())
	t.mu.Unlock()
}

func (t *Twin) current() Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.client
}

// Start connects, lists the chats and catches up; false when the account is not logged in (the service runs on).
func (t *Twin) Start(ctx context.Context) (bool, error) {
	if _, err := os.Stat(t.Session); err != nil {
		return t.needsLogin(i18n.P("the twin has not logged in to Telegram yet", "твинк ещё не входил в Telegram")),
			nil
	}
	client, err := t.Connect(ctx, t.Session, t)
	if errors.Is(err, ErrNotAuthorized) {
		return t.needsLogin(i18n.P("the twin's session is no longer valid", "сессия твинка больше не действует")), nil
	}
	if err != nil {
		return false, err
	}
	t.mu.Lock()
	t.client = client
	t.mu.Unlock()
	if err := t.listen(ctx, client); err != nil {
		return false, err
	}
	return true, nil
}

// listen: the client is logged in: remember the account and catch up (live updates come since Connect).
func (t *Twin) listen(ctx context.Context, client Client) error {
	if err := os.Chmod(t.Session, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	me, err := client.Self(ctx)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.account = Describe(me)
	t.loginNeeded, t.err = false, nil
	t.mu.Unlock()
	if err := t.sync(ctx, client); err != nil {
		return err
	}
	t.touch()
	return nil
}

// Stop drops a login under way and disconnects.
func (t *Twin) Stop() {
	t.dropPending()
	if client := t.current(); client != nil {
		if err := client.Close(); err != nil {
			slog.Info("twin: closing the client", "err", err)
		}
	}
}

func (t *Twin) needsLogin(why i18n.Phrase) bool {
	t.mu.Lock()
	t.loginNeeded, t.err = true, &why
	t.mu.Unlock()
	slog.Warn(fmt.Sprintf("%s; log in in the web app or with: %s", why.En, LoginHint))
	alert := i18n.AlertP(
		fmt.Sprintf("The chats are not read: %s.\nLog in in the web app, Machine → Twin and chats (or on the server: "+
			"%s).", why.En, LoginHint),
		fmt.Sprintf("Чаты не читаются: %s.\nВойти — в приложении, Машина → Твинк и чаты (или на сервере: %s).",
			why.Ru, LoginHint))
	if t.NotifyLogin != nil {
		t.NotifyLogin(alert)
	} else {
		t.notify(alert)
	}
	return false
}

// --- login and logout from the web app ---------------------------------------

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, s)
}

func floodSeconds(err error) (int, bool) {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return int(d / time.Second), true
	}
	return 0, false
}

// SendCode asks Telegram for a login code; it comes to the account's other sessions or by SMS.
func (t *Twin) SendCode(ctx context.Context, phone string) error {
	phone = "+" + digits(phone)
	if pytext.Len(phone) < 8 {
		return loginError("the twin's phone number is needed in the international format: +7…",
			"нужен номер телефона твинка в международном формате: +7…")
	}
	t.loginMu.Lock()
	defer t.loginMu.Unlock()
	if t.current() != nil {
		return loginError("the twin is logged in already; to change the account, log out first",
			"твинк уже вошёл; чтобы сменить аккаунт, сначала выйдите")
	}
	t.dropPending()
	client, err := t.Dial(ctx, t.Session)
	if err != nil {
		return loginError("Telegram did not answer: "+err.Error(), "Telegram не ответил: "+err.Error())
	}
	hash, err := client.SendCode(ctx, phone)
	if err != nil {
		client.Close()
		if tgerr.Is(err, "PHONE_NUMBER_INVALID") {
			return loginError("Telegram does not know this number", "Telegram не знает такого номера")
		}
		if s, ok := floodSeconds(err); ok {
			return loginError(fmt.Sprintf("Telegram asks to wait %d s before a new code", s),
				fmt.Sprintf("Telegram просит подождать %d с перед новым кодом", s))
		}
		return loginError("Telegram did not answer: "+err.Error(), "Telegram не ответил: "+err.Error())
	}
	t.mu.Lock()
	t.pending = &pending{client: client, phone: phone, codeHash: hash, step: "code"}
	t.mu.Unlock()
	return nil
}

// SignIn takes the code, then the cloud password if the account has one (the step tells which is asked).
func (t *Twin) SignIn(ctx context.Context, code, password string) error {
	t.loginMu.Lock()
	defer t.loginMu.Unlock()
	t.mu.Lock()
	p := t.pending
	t.mu.Unlock()
	if p == nil {
		return loginError("ask for a code first", "сначала запросите код")
	}
	var err error
	if p.step == "code" {
		code = digits(code)
		if code == "" {
			return loginError("enter the code from Telegram", "введите код из Telegram")
		}
		err = p.client.SignIn(ctx, p.phone, code, p.codeHash)
	} else {
		if password == "" {
			return loginError("enter the cloud password", "введите облачный пароль")
		}
		err = p.client.Password(ctx, password)
	}
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrPasswordAuthNeeded):
		t.mu.Lock()
		p.step = "password"
		t.mu.Unlock()
		return nil
	case tgerr.Is(err, "PHONE_CODE_INVALID"):
		return loginError("wrong code", "неверный код")
	case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
		t.dropPending()
		return loginError("the code expired or was shared in Telegram: ask for a new one",
			"код устарел или им поделились в Telegram — запросите новый")
	case errors.Is(err, auth.ErrPasswordInvalid) || tgerr.Is(err, "PASSWORD_HASH_INVALID"):
		return loginError("wrong cloud password", "неверный облачный пароль")
	default:
		if s, ok := floodSeconds(err); ok {
			return loginError(fmt.Sprintf("Telegram asks to wait %d s", s),
				fmt.Sprintf("Telegram просит подождать %d с", s))
		}
		return loginError("Telegram did not answer: "+err.Error(), "Telegram не ответил: "+err.Error())
	}
	me, err := p.client.Self(ctx)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.pending = nil
	t.mu.Unlock()
	if me.Bot || me.ID == t.Env.OwnerID {
		// The twin is its own account (D15).
		if err := p.client.LogOut(ctx); err != nil {
			slog.Warn("twin: logging out of a wrong account failed", "err", err)
		}
		p.client.Close()
		if err := os.Remove(t.Session); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("twin: removing the session failed", "err", err)
		}
		return loginError("this is no twin: a separate account is needed, not a bot and not your own",
			"это не твинк: нужен отдельный аккаунт, а не бот и не ваш основной")
	}
	// The login's client hands the session over to one that hears live updates.
	if err := p.client.Close(); err != nil {
		slog.Info("twin: closing the login client", "err", err)
	}
	// The first catch-up runs to its end even if the web request that asked for it is gone.
	ctx = context.WithoutCancel(ctx)
	client, err := t.Connect(ctx, t.Session, t)
	if err == nil {
		t.mu.Lock()
		t.client = client
		t.mu.Unlock()
		t.signalChanged()
		err = t.listen(ctx, client)
	} else {
		t.mu.Lock()
		t.account, t.loginNeeded = Describe(me), false
		t.mu.Unlock()
	}
	if err != nil { // logged in; the next start catches up
		slog.Warn("twin: the first sync after the login failed", "err", err)
		t.setError(phrase("logged in, but the chats did not catch up: "+err.Error(),
			"вход выполнен, но чаты не догнались: "+err.Error()))
	}
	t.mu.Lock()
	var name any = me.ID
	if t.account != nil {
		name = t.account["name"]
	}
	t.mu.Unlock()
	t.notify(i18n.P(fmt.Sprintf("The twin logged in: %v. The chats are read.", name),
		fmt.Sprintf("Твинк вошёл: %v. Чаты читаются.", name)))
	return nil
}

// CancelLogin drops a login under way.
func (t *Twin) CancelLogin() {
	t.loginMu.Lock()
	defer t.loginMu.Unlock()
	t.dropPending()
}

// Logout ends the session on Telegram's side and forgets it here; the chats and their settings stay.
func (t *Twin) Logout(ctx context.Context) error {
	t.loginMu.Lock()
	defer t.loginMu.Unlock()
	t.mu.Lock()
	client := t.client
	name := any("?")
	if t.account != nil {
		name = t.account["name"]
	}
	t.client = nil
	t.mu.Unlock()
	if client == nil {
		return loginError("the twin is not logged in anyway", "твинк и так не вошёл")
	}
	t.signalChanged() // first: the watcher must not take the disconnect for a lost connection
	if err := client.LogOut(ctx); err != nil {
		slog.Warn("logging the twin out failed", "err", err)
	}
	if err := client.Close(); err != nil {
		slog.Info("twin: closing the client", "err", err)
	}
	if err := os.Remove(t.Session); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("twin: removing the session failed", "err", err)
	}
	t.mu.Lock()
	t.account = nil
	t.loginNeeded, t.err = true, phrase("you logged out of the twin's account", "вы вышли из аккаунта твинка")
	t.mu.Unlock()
	t.notify(i18n.P(fmt.Sprintf("The twin logged out of %v. The chats are not read until it logs in again.", name),
		fmt.Sprintf("Твинк вышел из аккаунта %v. Чаты не читаются, пока он снова не войдёт.", name)))
	return nil
}

func (t *Twin) dropPending() {
	t.mu.Lock()
	p := t.pending
	t.pending = nil
	t.mu.Unlock()
	if p != nil {
		p.client.Close()
	}
}

// --- chats and the catch-up -------------------------------------------------

func (t *Twin) sync(ctx context.Context, client Client) error {
	known, err := t.inbox.Chats()
	if err != nil {
		return err
	}
	first := len(known) == 0 // the very first listing: no news about every chat
	enabled, err := t.chats.EnabledIDs()
	if err != nil {
		return err
	}
	added, caughtUp := 0, 0
	err = client.Dialogs(ctx, func(d Dialog) error {
		chat, err := t.inbox.Chat(d.ID)
		if err != nil {
			return err
		}
		isNew, err := t.inbox.PutChat(d.ID, ChatTitle(d.Entity), ChatKind(d.Entity), Username(d.Entity), int64(d.Top))
		if err != nil {
			return err
		}
		if isNew {
			added++
			if !first {
				t.announce(d.ID)
			}
		} else if chat != nil && enabled[d.ID] && int64(d.Top) > chat.LastID {
			n, err := t.catchUp(ctx, client, d, chat.LastID)
			caughtUp += n
			if err != nil {
				if _, rpc := tgerr.As(err); !rpc {
					return err
				}
				// One chat closed to the twin must not stop the others; last_id stays: the next start tries again.
				slog.Warn("twin: catch-up failed", "chat", d.ID, "err", err)
				t.setError(phrase(fmt.Sprintf("could not catch up with «%s»: %v", chat.Title, err),
					fmt.Sprintf("не удалось догнать «%s»: %v", chat.Title, err)))
				return nil
			}
		}
		return t.inbox.Advance(d.ID, int64(d.Top))
	})
	if err != nil {
		return err
	}
	slog.Info(fmt.Sprintf("twin: %d new chats, %d messages caught up, %d chats enabled", added, caughtUp,
		len(enabled)))
	return nil
}

func (t *Twin) catchUp(ctx context.Context, client Client, d Dialog, lastID int64) (int, error) {
	oldest := clock.NowUTC().Add(-inbox.Retention) // older ones would be pruned at once
	stored := 0
	err := client.History(ctx, d, int(lastID), func(m Msg) error {
		if Date(m.Message.Date).Before(oldest) {
			return nil
		}
		ok, err := t.store(ctx, client, m, d.ID, time.Time{})
		if ok {
			stored++
		}
		return err
	})
	return stored, err
}

func (t *Twin) announce(chatID int64) {
	chat, err := t.inbox.Chat(chatID)
	if err != nil || chat == nil {
		slog.Warn("twin: the new chat is not stored", "chat", chatID, "err", err)
		return
	}
	t.notify(i18n.P(
		fmt.Sprintf("A new chat in the twin's account: «%s» (%s). It is off; it can be enabled in the web app, "+
			"Machine → Twin and chats.", chat.Title, chat.Kind),
		fmt.Sprintf("Новый чат в аккаунте твинка: «%s» (%s). Он выключен, включить можно в приложении, "+
			"Машина → Твинк и чаты.", chat.Title, chat.Kind)))
}

// --- live updates -----------------------------------------------------------

func (t *Twin) enabled(chatID int64) bool {
	on, err := t.chats.Enabled(chatID)
	if err != nil {
		slog.Error("twin: cannot read the chat's settings", "chat", chatID, "err", err)
	}
	return on
}

func logged(what string, chatID int64, err error) {
	if err != nil {
		slog.Error("twin: "+what+" failed", "chat", chatID, "err", err)
	}
}

// OnNew is a new message: a new chat is listed (and off), an enabled chat's message stored, and the chat moves on.
func (t *Twin) OnNew(ctx context.Context, chatID int64, m Msg) {
	chat, err := t.inbox.Chat(chatID)
	if err != nil {
		logged("a new message", chatID, err)
		return
	}
	if chat == nil {
		entity := Lookup(m.Entities, chatID)
		if entity == nil {
			if client := t.current(); client != nil {
				entity, err = client.Entity(ctx, chatID)
				logged("finding a new chat", chatID, err)
			}
		}
		if entity == nil {
			return
		}
		isNew, err := t.inbox.PutChat(chatID, ChatTitle(entity), ChatKind(entity), Username(entity),
			int64(m.Message.ID))
		logged("listing a new chat", chatID, err)
		if isNew {
			t.announce(chatID)
		}
	} else if t.enabled(chatID) {
		_, err := t.store(ctx, t.current(), m, chatID, time.Time{})
		logged("storing a message", chatID, err)
	}
	logged("moving on", chatID, t.inbox.Advance(chatID, int64(m.Message.ID)))
	t.touch()
}

// OnEdit is an edited message of an enabled chat.
func (t *Twin) OnEdit(ctx context.Context, chatID int64, m Msg) {
	if !t.enabled(chatID) {
		return
	}
	msg := m.Message
	at := clock.NowUTC()
	if msg.EditDate != 0 {
		at = Date(msg.EditDate)
	}
	edited, err := t.inbox.Edit(chatID, int64(msg.ID), MessageText(msg), MessageFiles(msg), at)
	if err != nil {
		logged("an edit", chatID, err)
		return
	}
	if edited {
		return
	}
	if has, err := t.inbox.Has(chatID, int64(msg.ID), "tg"); err != nil || has {
		logged("an edit", chatID, err)
		return
	}
	// A message not stored, whatever its age (D74): kept while its last edit is within the 14 days, which count from
	// the edit. Telegram also sends an edit for a reaction or a link preview, with the old edit_date: it brings back
	// a message whose real edit the twin missed (offline), never one edited before its 14 days.
	now := clock.NowUTC()
	if !Date(msg.Date).Before(now.Add(-inbox.Retention)) {
		_, err = t.store(ctx, t.current(), m, chatID, time.Time{})
	} else if msg.EditDate != 0 && !Date(msg.EditDate).Before(now.Add(-inbox.Retention)) {
		_, err = t.store(ctx, t.current(), m, chatID, Date(msg.EditDate))
	}
	logged("storing an edited message", chatID, err)
}

// OnDelete marks deleted messages.
func (t *Twin) OnDelete(_ context.Context, msgIDs []int, chatID *int64) {
	ids := make([]int64, len(msgIDs))
	for i, id := range msgIDs {
		ids[i] = int64(id)
	}
	var chat int64
	if chatID != nil {
		chat = *chatID
	}
	_, err := t.inbox.Delete(ids, chatID, clock.NowUTC())
	logged("a deletion", chat, err)
}

// OnPin marks messages pinned or unpinned.
func (t *Twin) OnPin(_ context.Context, chatID int64, msgIDs []int, pinned bool) {
	ids := make([]int64, len(msgIDs))
	for i, id := range msgIDs {
		ids[i] = int64(id)
	}
	logged("a pin", chatID, t.inbox.Pin(chatID, ids, pinned))
}

// OnTitle gives a chat its new title.
func (t *Twin) OnTitle(_ context.Context, chatID int64, title string) {
	logged("a new title", chatID, t.inbox.RenameChat(chatID, title))
}

// --- files --------------------------------------------------------------------

func downloadError(err error) error {
	if rpc, ok := tgerr.As(err); ok {
		if _, flood := tgerr.AsFloodWait(err); flood || rpc.Code == 420 || rpc.Code >= 500 {
			return &NotNow{Msg: "the download failed on the way: " + err.Error()}
		}
		return err // final: the chat is closed to the twin
	}
	return &NotNow{Msg: "the download failed on the way: " + err.Error()} // a connection or a timeout
}

// Download is what Files.download asks: the file of a message into `path`; false when the message or its file is
// gone, or is over `limit` bytes now (an edit may have replaced a small file with a big one since it was stored;
// limit <= 0: no limit). *NotNow when the twin is offline or the download failed on the way: the message waits for
// its next batch. Any other Telegram error (the chat is closed to the twin) is final for the file.
func (t *Twin) Download(ctx context.Context, chatID, msgID int64, path string, limit int64) (bool, error) {
	client := t.current()
	if client == nil || !client.Connected() {
		return false, &NotNow{Msg: "the twin is not connected to Telegram"}
	}
	m, err := client.Message(ctx, chatID, int(msgID))
	if err != nil {
		return false, downloadError(err)
	}
	if m == nil || m.Message == nil || !OwnMedia(m.Message) {
		return false, nil
	}
	if limit > 0 && FileSize(m.Message) > limit {
		slog.Info(fmt.Sprintf("message %d in %d: its file is over %d bytes now, not downloaded", msgID, chatID, limit))
		return false, nil
	}
	ok, err := client.Download(ctx, m.Message, path)
	if err != nil {
		return false, downloadError(err)
	}
	return ok, nil
}

// store keeps one message of an enabled chat; false when there is nothing to keep or it is stored already.
// `editedAt`: a message past its 14 days, edited just now (D74).
func (t *Twin) store(ctx context.Context, client Client, m Msg, chatID int64, editedAt time.Time) (bool, error) {
	msg := m.Message
	if msg == nil {
		return false, nil
	}
	if has, err := t.inbox.Has(chatID, int64(msg.ID), "tg"); err != nil || has {
		return false, err
	}
	fields := MessageFields(msg, m.Entities)
	if fields.Text == "" && len(fields.Files) == 0 {
		return false, nil // polls, locations, dice: nothing for the batches
	}
	if msg.PostAuthor != "" {
		author := msg.PostAuthor
		fields.SenderName = &author
	} else if fields.SenderID != nil {
		sender := Lookup(m.Entities, *fields.SenderID)
		if sender == nil && client != nil {
			found, err := client.Entity(ctx, *fields.SenderID)
			if err != nil {
				slog.Info("twin: the sender is unknown", "sender", *fields.SenderID, "err", err)
			} else {
				sender = found
			}
		}
		if sender != nil {
			name := DisplayName(sender)
			fields.SenderName = &name
		}
	}
	if fields.ReplyTo != nil && client != nil {
		has, err := t.inbox.Has(chatID, *fields.ReplyTo, "tg")
		if err != nil {
			return false, err
		}
		if !has {
			replied, err := client.Message(ctx, chatID, int(*fields.ReplyTo))
			if err != nil {
				slog.Info("twin: the replied message is not read", "chat", chatID, "err", err)
			} else if replied != nil && replied.Message != nil {
				if quote := pytext.Cut(MessageText(replied.Message), QuoteLimit); quote != "" {
					fields.ReplyQuote = &quote
				}
			}
		}
	}
	return t.inbox.Add(chatID, int64(msg.ID), fields, "tg", editedAt)
}
