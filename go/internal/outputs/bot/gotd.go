package bot

import (
	"context"
	"crypto/rand"
	"encoding/binary"

	"github.com/gotd/td/telegram/bot"
	"github.com/gotd/td/telegram/message/markup"
	"github.com/gotd/td/tg"

	"github.com/IvanIkra/infomaster/go/internal/collectors/telegram"
	"github.com/IvanIkra/infomaster/go/internal/config"
)

// The gotd side of the bot: nothing in it runs in the tests.

type gotdBot struct{ b *bot.Bot }

// ConnectGotd logs the bot in from TG_BOT_TOKEN, keeping its session in sessionPath, and passes the private messages
// to handle. The update state is in memory, as Telethon's was.
func ConnectGotd(ctx context.Context, env config.Env, sessionPath string,
	handle func(context.Context, Event)) (API, error) {
	d := tg.NewUpdateDispatcher()
	b, err := bot.New(int(env.APIID), env.APIHash, env.BotToken, bot.Options{Client: telegram.Options(sessionPath),
		Handler: d})
	if err != nil {
		return nil, err
	}
	d.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		if event, ok := PrivateEvent(u.Message, e); ok {
			handle(ctx, event)
		}
		return nil
	})
	if err := b.Start(ctx); err != nil {
		return nil, err
	}
	return &gotdBot{b: b}, nil
}

func (g *gotdBot) inputUser(ctx context.Context, userID int64) (tg.InputPeerClass, tg.InputUserClass) {
	if u, err := g.b.Peers().ResolveUserID(ctx, userID); err == nil {
		if input, ok := u.InputPeer().(*tg.InputPeerUser); ok {
			return input, &tg.InputUser{UserID: input.UserID, AccessHash: input.AccessHash}
		}
	}
	// A bot may name a user by the id alone, as Telethon does.
	return &tg.InputPeerUser{UserID: userID}, &tg.InputUser{UserID: userID}
}

func randomID() int64 {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return int64(binary.LittleEndian.Uint64(buf[:]))
}

func (g *gotdBot) Send(ctx context.Context, userID int64, text string, options SendOptions) error {
	peer, _ := g.inputUser(ctx, userID)
	request := &tg.MessagesSendMessageRequest{Peer: peer, Message: text, RandomID: randomID(),
		Silent: options.Silent, NoWebpage: options.NoPreview}
	if options.Button != nil {
		request.ReplyMarkup = markup.InlineRow(markup.URL(options.Button.Text, options.Button.URL))
	}
	_, err := g.b.API().MessagesSendMessage(ctx, request)
	return err
}

func (g *gotdBot) ResetMenu(ctx context.Context, userID int64) error {
	var user tg.InputUserClass = &tg.InputUserEmpty{}
	if userID != 0 {
		_, user = g.inputUser(ctx, userID)
	}
	_, err := g.b.API().BotsSetBotMenuButton(ctx, &tg.BotsSetBotMenuButtonRequest{UserID: user,
		Button: &tg.BotMenuButtonDefault{}})
	return err
}

func (g *gotdBot) ResetCommands(ctx context.Context) error {
	_, err := g.b.API().BotsResetBotCommands(ctx, &tg.BotsResetBotCommandsRequest{
		Scope: &tg.BotCommandScopeDefault{}, LangCode: ""})
	return err
}

func (g *gotdBot) Download(ctx context.Context, m *tg.Message, path string) (bool, error) {
	return telegram.DownloadMedia(ctx, g.b.Client(), m, path)
}

func (g *gotdBot) Done() <-chan struct{} { return g.b.Done() }

func (g *gotdBot) Close() error { return g.b.Close() }

// PrivateEvent is a new message of a private chat with the bot (Telethon's is_private); false for anything else.
func PrivateEvent(message tg.MessageClass, entities tg.Entities) (Event, bool) {
	m, ok := message.(*tg.Message)
	if !ok {
		return Event{}, false
	}
	if _, private := m.PeerID.(*tg.PeerUser); !private {
		return Event{}, false
	}
	var sender int64
	if id := telegram.SenderID(m); id != nil {
		sender = *id
	}
	return Event{SenderID: sender, ChatID: telegram.PeerID(m.PeerID), Message: m, Entities: entities}, true
}
