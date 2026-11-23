package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/telegram/user"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/tgid"
)

// The gotd side: the only code here that talks to Telegram. Nothing in it runs in the tests.

// FloodSleep: a FLOOD_WAIT up to this long is waited out and the request sent again, as Telethon's
// flood_sleep_threshold does; a longer one is the caller's error.
const FloodSleep = 60 * time.Second

// FloodWaiter is the middleware that waits out short FLOOD_WAITs.
var FloodWaiter = telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		for {
			err := next.Invoke(ctx, input, output)
			d, ok := tgerr.AsFloodWait(err)
			if !ok || d > FloodSleep {
				return err
			}
			select {
			case <-time.After(d + time.Second):
			case <-ctx.Done():
				return err
			}
		}
	}
})

// Options are the client options of a session file.
func Options(sessionPath string) telegram.Options {
	return telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath},
		Middlewares:    []telegram.Middleware{FloodWaiter},
	}
}

const startTimeout = 2 * time.Minute

// --- the login ---------------------------------------------------------------

type gotdLogin struct{ client *telegram.Client }

// DialGotd connects a client for a login, keeping its session in sessionPath.
func DialGotd(ctx context.Context, env config.Env, sessionPath string) (LoginClient, error) {
	client := telegram.NewClient(int(env.APIID), env.APIHash, Options(sessionPath))
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		return nil, err
	}
	return &gotdLogin{client: client}, nil
}

func (g *gotdLogin) SendCode(ctx context.Context, phone string) (string, error) {
	sent, err := g.client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return "", err
	}
	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return "", fmt.Errorf("unexpected answer %T", sent)
	}
	next := ""
	if t, ok := code.GetNextType(); ok {
		next = codeVia(t)
	}
	slog.Info("twin: a login code was sent", "phone", Masked(phone), "via", codeVia(code.Type), "next", next, "timeout", code.Timeout)
	return code.PhoneCodeHash, nil
}

// codeVia is where Telegram sent a login code: the owner looks for it there.
func codeVia(t interface{ TypeName() string }) string {
	switch v := t.(type) {
	case *tg.AuthSentCodeTypeApp:
		return "app (the account's other sessions, the chat «Telegram»)"
	case *tg.AuthSentCodeTypeSMS:
		return "sms"
	case *tg.AuthSentCodeTypeCall:
		return "call"
	case *tg.AuthSentCodeTypeFlashCall, *tg.AuthSentCodeTypeMissedCall:
		return "missed call (the code is the caller's number)"
	case *tg.AuthSentCodeTypeEmailCode:
		return "email " + v.EmailPattern
	case *tg.AuthSentCodeTypeSetUpEmailRequired:
		return "email required: Telegram wants a login email set up first"
	case *tg.AuthSentCodeTypeFragmentSMS:
		return "fragment " + v.URL
	case *tg.AuthSentCodeTypeFirebaseSMS:
		return "firebase sms (official apps only)"
	case *tg.AuthSentCodeTypeSMSWord, *tg.AuthSentCodeTypeSMSPhrase:
		return "sms (a word or phrase)"
	}
	return t.TypeName()
}

func (g *gotdLogin) SignIn(ctx context.Context, phone, code, codeHash string) error {
	_, err := g.client.Auth().SignIn(ctx, phone, code, codeHash)
	var signUp *auth.SignUpRequired
	if errors.As(err, &signUp) {
		return errors.New("this number has no Telegram account")
	}
	return err
}

func (g *gotdLogin) Password(ctx context.Context, password string) error {
	_, err := g.client.Auth().Password(ctx, password)
	return err
}

func (g *gotdLogin) Self(ctx context.Context) (*tg.User, error) { return g.client.Self(ctx) }

func (g *gotdLogin) LogOut(ctx context.Context) error {
	_, err := g.client.LogOut(ctx)
	return err
}

func (g *gotdLogin) Close() error { return g.client.Close() }

// --- the logged-in client -------------------------------------------------------

type gotdClient struct {
	u *user.User
}

// ConnectGotd starts the account of sessionPath with live updates for h; ErrNotAuthorized when it is not logged in.
// The update state is kept in memory: after a restart the twin catches up from `last_id` itself, and edits and
// deletions made while it was down are not caught up (as with Telethon).
func ConnectGotd(ctx context.Context, env config.Env, sessionPath string, h Handler) (Client, error) {
	d := tg.NewUpdateDispatcher()
	u, err := user.New(int(env.APIID), env.APIHash, user.Options{Client: Options(sessionPath), Handler: d})
	if err != nil {
		return nil, err
	}
	route(d, h)
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if err := u.Start(ctx); err != nil {
		if errors.Is(err, user.ErrNotAuthorized) {
			return nil, ErrNotAuthorized
		}
		return nil, err
	}
	return &gotdClient{u: u}, nil
}

// route passes the dispatcher's updates to the handler as Telethon's events would: new and edited messages (service
// messages only for a new title), deletions, pins.
func route(d tg.UpdateDispatcher, h Handler) {
	onNew := func(ctx context.Context, e tg.Entities, message tg.MessageClass) {
		switch m := message.(type) {
		case *tg.Message:
			h.OnNew(ctx, PeerID(m.PeerID), Msg{Message: m, Entities: e})
		case *tg.MessageService:
			if action, ok := m.Action.(*tg.MessageActionChatEditTitle); ok {
				h.OnTitle(ctx, PeerID(m.PeerID), action.Title)
			}
		}
	}
	onEdit := func(ctx context.Context, e tg.Entities, message tg.MessageClass) {
		if m, ok := message.(*tg.Message); ok {
			h.OnEdit(ctx, PeerID(m.PeerID), Msg{Message: m, Entities: e})
		}
	}
	d.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		onNew(ctx, e, u.Message)
		return nil
	})
	d.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		onNew(ctx, e, u.Message)
		return nil
	})
	d.OnEditMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditMessage) error {
		onEdit(ctx, e, u.Message)
		return nil
	})
	d.OnEditChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditChannelMessage) error {
		onEdit(ctx, e, u.Message)
		return nil
	})
	d.OnDeleteMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdateDeleteMessages) error {
		h.OnDelete(ctx, u.Messages, nil)
		return nil
	})
	d.OnDeleteChannelMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdateDeleteChannelMessages) error {
		chat := tgid.Marked(tgid.Channel, u.ChannelID)
		h.OnDelete(ctx, u.Messages, &chat)
		return nil
	})
	d.OnPinnedMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdatePinnedMessages) error {
		h.OnPin(ctx, PeerID(u.Peer), u.Messages, u.Pinned)
		return nil
	})
	d.OnPinnedChannelMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdatePinnedChannelMessages) error {
		h.OnPin(ctx, tgid.Marked(tgid.Channel, u.ChannelID), u.Messages, u.Pinned)
		return nil
	})
}

func (c *gotdClient) Self(ctx context.Context) (*tg.User, error) {
	if me := c.u.Self(); me != nil {
		return me, nil
	}
	return c.u.Client().Self(ctx)
}

func (c *gotdClient) apply(ctx context.Context, e tg.Entities) {
	var users []tg.UserClass
	var chats []tg.ChatClass
	for _, u := range e.Users {
		users = append(users, u)
	}
	for _, ch := range e.Chats {
		chats = append(chats, ch)
	}
	for _, ch := range e.Channels {
		chats = append(chats, ch)
	}
	_ = c.u.Peers().Apply(ctx, users, chats) // the access hashes, for downloads and replies later
}

func (c *gotdClient) Dialogs(ctx context.Context, fn func(Dialog) error) error {
	return dialogs.NewQueryBuilder(c.u.API()).GetDialogs().BatchSize(100).ForEach(ctx,
		func(ctx context.Context, e dialogs.Elem) error {
			dialog, ok := e.Dialog.(*tg.Dialog)
			if !ok {
				return nil // a folder
			}
			entities := tg.Entities{Users: e.Entities.Users(), Chats: e.Entities.Chats(),
				Channels: e.Entities.Channels()}
			c.apply(ctx, entities)
			id := PeerID(dialog.Peer)
			entity := Lookup(entities, id)
			if entity == nil { // a group the account was removed from: no title is given
				kind, bare := tgid.Resolve(id)
				switch kind {
				case tgid.User:
					entity = &tg.User{ID: bare}
				case tgid.Chat:
					entity = &tg.ChatForbidden{ID: bare}
				default:
					entity = &tg.ChannelForbidden{ID: bare}
				}
			}
			top := 0
			if e.Last != nil {
				top = e.Last.GetID()
			}
			return fn(Dialog{ID: id, Entity: entity, Peer: e.Peer, Top: top})
		})
}

func unpack(result tg.MessagesMessagesClass) ([]tg.MessageClass, tg.Entities) {
	modified, ok := result.AsModified()
	if !ok {
		return nil, tg.Entities{}
	}
	return modified.GetMessages(), tg.EntitiesFrom(modified.GetUsers(), modified.GetChats())
}

func (c *gotdClient) History(ctx context.Context, d Dialog, minID int, fn func(Msg) error) error {
	// Newest first, down to minID; then stored oldest first, so a reply finds what it replies to.
	var found []Msg
	offset := 0
	for {
		result, err := c.u.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: d.Peer,
			OffsetID: offset, Limit: 100, MinID: minID})
		if err != nil {
			return err
		}
		messages, entities := unpack(result)
		c.apply(ctx, entities)
		older := false
		for _, message := range messages {
			id := message.GetID()
			if id <= minID || (offset != 0 && id >= offset) {
				continue
			}
			older, offset = true, id
			if m, ok := message.(*tg.Message); ok {
				found = append(found, Msg{Message: m, Entities: entities})
			}
		}
		if !older {
			break
		}
	}
	for i := len(found) - 1; i >= 0; i-- {
		if err := fn(found[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *gotdClient) peer(ctx context.Context, chatID int64) (peers.Peer, error) {
	kind, id := tgid.Resolve(chatID)
	switch kind {
	case tgid.User:
		return c.u.Peers().ResolveUserID(ctx, id)
	case tgid.Chat:
		return c.u.Peers().ResolveChatID(ctx, id)
	default:
		return c.u.Peers().ResolveChannelID(ctx, id)
	}
}

func (c *gotdClient) Message(ctx context.Context, chatID int64, msgID int) (*Msg, error) {
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}}
	var result tg.MessagesMessagesClass
	var err error
	if kind, id := tgid.Resolve(chatID); kind == tgid.Channel {
		channel, err := c.u.Peers().ResolveChannelID(ctx, id)
		if err != nil {
			return nil, err
		}
		result, err = c.u.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: channel.InputChannel(), ID: ids})
		if err != nil {
			return nil, err
		}
	} else if result, err = c.u.API().MessagesGetMessages(ctx, ids); err != nil {
		return nil, err
	}
	messages, entities := unpack(result)
	c.apply(ctx, entities)
	for _, message := range messages {
		if m, ok := message.(*tg.Message); ok && m.ID == msgID {
			return &Msg{Message: m, Entities: entities}, nil
		}
	}
	return nil, nil
}

func (c *gotdClient) Entity(ctx context.Context, chatID int64) (any, error) {
	p, err := c.peer(ctx, chatID)
	if err != nil {
		return nil, err
	}
	switch v := p.(type) {
	case peers.User:
		return v.Raw(), nil
	case peers.Chat:
		return v.Raw(), nil
	case peers.Channel:
		return v.Raw(), nil
	}
	return nil, fmt.Errorf("unknown peer %T", p)
}

func (c *gotdClient) Download(ctx context.Context, m *tg.Message, path string) (bool, error) {
	return DownloadMedia(ctx, c.u.Client(), m, path)
}

func (c *gotdClient) LogOut(ctx context.Context) error {
	_, err := c.u.Client().LogOut(ctx)
	return err
}

func (c *gotdClient) Connected() bool {
	select {
	case <-c.u.Done():
		return false
	default:
		return true
	}
}

func (c *gotdClient) Done() <-chan struct{} { return c.u.Done() }

func (c *gotdClient) Close() error { return c.u.Close() }

// DownloadMedia writes the message's own file (the largest size of a photo, or the document) to path; false when
// the message has none. The bot uses it too.
func DownloadMedia(ctx context.Context, client *telegram.Client, m *tg.Message, path string) (bool, error) {
	location, ok := MediaLocation(m)
	if !ok {
		return false, nil
	}
	if _, err := client.Download(location).ToPath(ctx, path); err != nil {
		return false, err
	}
	return true, nil
}
