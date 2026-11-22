package telegram

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/gotd/td/tg"
)

// recorder writes down what the handler hears.
type recorder struct{ heard []string }

func (r *recorder) OnNew(_ context.Context, chatID int64, m Msg) {
	r.heard = append(r.heard, fmt.Sprintf("new %d %d", chatID, m.Message.ID))
}

func (r *recorder) OnEdit(_ context.Context, chatID int64, m Msg) {
	r.heard = append(r.heard, fmt.Sprintf("edit %d %d", chatID, m.Message.ID))
}

func (r *recorder) OnDelete(_ context.Context, ids []int, chatID *int64) {
	chat := "nil"
	if chatID != nil {
		chat = fmt.Sprint(*chatID)
	}
	r.heard = append(r.heard, fmt.Sprintf("delete %s %v", chat, ids))
}

func (r *recorder) OnPin(_ context.Context, chatID int64, ids []int, pinned bool) {
	r.heard = append(r.heard, fmt.Sprintf("pin %d %v %t", chatID, ids, pinned))
}

func (r *recorder) OnTitle(_ context.Context, chatID int64, title string) {
	r.heard = append(r.heard, fmt.Sprintf("title %d %s", chatID, title))
}

// The updates reach the twin with marked chat ids, as Telethon's events gave them.
func TestRoute(t *testing.T) {
	d := tg.NewUpdateDispatcher()
	r := &recorder{}
	route(d, r)
	updates := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 1, PeerID: &tg.PeerChannel{ChannelID: 10}}},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 2, PeerID: &tg.PeerUser{UserID: 7}}},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 3, PeerID: &tg.PeerChat{ChatID: 5}}},
		&tg.UpdateNewChannelMessage{Message: &tg.MessageService{ID: 4, PeerID: &tg.PeerChannel{ChannelID: 10},
			Action: &tg.MessageActionChatEditTitle{Title: "Матан 2"}}},
		&tg.UpdateNewMessage{Message: &tg.MessageService{ID: 5, PeerID: &tg.PeerChat{ChatID: 5},
			Action: &tg.MessageActionPinMessage{}}},
		&tg.UpdateEditChannelMessage{Message: &tg.Message{ID: 1, PeerID: &tg.PeerChannel{ChannelID: 10}}},
		&tg.UpdateEditMessage{Message: &tg.Message{ID: 2, PeerID: &tg.PeerUser{UserID: 7}}},
		&tg.UpdateDeleteMessages{Messages: []int{2, 3}},
		&tg.UpdateDeleteChannelMessages{ChannelID: 10, Messages: []int{1}},
		&tg.UpdatePinnedMessages{Peer: &tg.PeerChat{ChatID: 5}, Messages: []int{3}, Pinned: true},
		&tg.UpdatePinnedChannelMessages{ChannelID: 10, Messages: []int{1}},
	}}
	if err := d.Handle(context.Background(), updates); err != nil {
		t.Fatal(err)
	}
	want := []string{"new -1000000000010 1", "new 7 2", "new -5 3", "title -1000000000010 Матан 2",
		"edit -1000000000010 1", "edit 7 2", "delete nil [2 3]", "delete -1000000000010 [1]", "pin -5 [3] true",
		"pin -1000000000010 [1] false"}
	if !reflect.DeepEqual(r.heard, want) {
		t.Fatal(r.heard)
	}
}
