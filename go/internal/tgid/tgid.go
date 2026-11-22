// Package tgid turns Telegram peers into the marked ids Telethon's utils.get_peer_id gives, the keys of the chats and
// messages tables: users > 0, basic groups -id, channels and supergroups -(1000000000000 + id).
package tgid

const channelShift = 1000000000000

// Kind of a peer.
type Kind int

const (
	User Kind = iota
	Chat
	Channel
)

// Marked is the marked id of a peer.
func Marked(kind Kind, id int64) int64 {
	switch kind {
	case Chat:
		return -id
	case Channel:
		return -(channelShift + id)
	default:
		return id
	}
}

// Resolve splits a marked id into the peer's kind and its bare id.
func Resolve(marked int64) (Kind, int64) {
	switch {
	case marked >= 0:
		return User, marked
	case marked <= -channelShift:
		return Channel, -marked - channelShift
	default:
		return Chat, -marked
	}
}
