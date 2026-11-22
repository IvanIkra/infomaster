package telegram

import (
	"context"
	"errors"

	"github.com/gotd/td/tg"
)

// The twin talks to Telegram only through these interfaces: gotd in production (gotd.go), fakes in the tests.

// ErrNotAuthorized: the session file holds no logged-in account.
var ErrNotAuthorized = errors.New("the session is not authorized")

// Msg is a message with the users, chats and channels that came with it (its sender, a forward's source).
type Msg struct {
	Message  *tg.Message
	Entities tg.Entities
}

// Dialog is a chat of the account as the dialog list gives it.
type Dialog struct {
	ID     int64 // marked
	Entity any   // *tg.User, *tg.Chat, *tg.ChatForbidden, *tg.Channel or *tg.ChannelForbidden
	Peer   tg.InputPeerClass
	Top    int // the newest message's id; 0 for none
}

// Client is a logged-in connection that reads the account's chats and passes live updates to a Handler.
type Client interface {
	Self(ctx context.Context) (*tg.User, error)
	// Dialogs calls fn for every chat of the account (archived ones too).
	Dialogs(ctx context.Context, fn func(Dialog) error) error
	// History calls fn for the messages newer than minID, oldest first.
	History(ctx context.Context, d Dialog, minID int, fn func(Msg) error) error
	// Message is one message of a chat; nil when it is gone.
	Message(ctx context.Context, chatID int64, msgID int) (*Msg, error)
	// Entity is the user, chat or channel of a marked id.
	Entity(ctx context.Context, chatID int64) (any, error)
	// Download writes the message's own file to path; false when it has none.
	Download(ctx context.Context, m *tg.Message, path string) (bool, error)
	// LogOut ends the session on Telegram's side.
	LogOut(ctx context.Context) error
	Connected() bool
	// Done is closed when the client stops: a lost connection or Close.
	Done() <-chan struct{}
	Close() error
}

// LoginClient is a connection that is logging an account in, step by step.
type LoginClient interface {
	// SendCode asks for a login code and gives its hash.
	SendCode(ctx context.Context, phone string) (string, error)
	// SignIn with the code; auth.ErrPasswordAuthNeeded when the account has a cloud password.
	SignIn(ctx context.Context, phone, code, codeHash string) error
	// Password is the cloud password; auth.ErrPasswordInvalid when it is wrong.
	Password(ctx context.Context, password string) error
	Self(ctx context.Context) (*tg.User, error)
	LogOut(ctx context.Context) error
	Close() error
}

// Handler hears the live updates of a Client. Chat ids are marked; a deletion without a chat (nil) is of private
// chats and basic groups, whose message ids are unique across the account.
type Handler interface {
	OnNew(ctx context.Context, chatID int64, m Msg)
	OnEdit(ctx context.Context, chatID int64, m Msg)
	OnDelete(ctx context.Context, msgIDs []int, chatID *int64)
	OnPin(ctx context.Context, chatID int64, msgIDs []int, pinned bool)
	OnTitle(ctx context.Context, chatID int64, title string)
}
