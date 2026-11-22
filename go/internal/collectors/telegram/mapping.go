package telegram

import (
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/tgid"
)

// PDFTypes are the MIME types of a PDF (collectors/files PDF_TYPES).
var PDFTypes = []string{"application/pdf", "application/x-pdf", "application/acrobat"}

// Telegram objects to rows. The functions read the gotd structs field by field and take a zero value for an absent
// one, the way Telethon's attributes are None: a message built by hand in a test reads the same as a decoded one.

// PeerID is the marked id of a peer (utils.get_peer_id); 0 for nil.
func PeerID(peer tg.PeerClass) int64 {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return tgid.Marked(tgid.User, p.UserID)
	case *tg.PeerChat:
		return tgid.Marked(tgid.Chat, p.ChatID)
	case *tg.PeerChannel:
		return tgid.Marked(tgid.Channel, p.ChannelID)
	}
	return 0
}

// EntityID is the marked id of a user, chat or channel; 0 for anything else.
func EntityID(entity any) int64 {
	switch e := entity.(type) {
	case *tg.User:
		return tgid.Marked(tgid.User, e.ID)
	case *tg.Chat:
		return tgid.Marked(tgid.Chat, e.ID)
	case *tg.ChatForbidden:
		return tgid.Marked(tgid.Chat, e.ID)
	case *tg.Channel:
		return tgid.Marked(tgid.Channel, e.ID)
	case *tg.ChannelForbidden:
		return tgid.Marked(tgid.Channel, e.ID)
	}
	return 0
}

// Lookup finds the entity (*tg.User, *tg.Chat or *tg.Channel) of a marked id among those that came with a message;
// nil when it did not come.
func Lookup(entities tg.Entities, marked int64) any {
	kind, id := tgid.Resolve(marked)
	switch kind {
	case tgid.User:
		if u, ok := entities.Users[id]; ok && u != nil {
			return u
		}
	case tgid.Chat:
		if c, ok := entities.Chats[id]; ok && c != nil {
			return c
		}
	case tgid.Channel:
		if c, ok := entities.Channels[id]; ok && c != nil {
			return c
		}
	}
	return nil
}

// DisplayName is utils.get_display_name: the user's names, the chat's title, "" for anything else.
func DisplayName(entity any) string {
	switch e := entity.(type) {
	case *tg.User:
		switch {
		case e.FirstName != "" && e.LastName != "":
			return e.FirstName + " " + e.LastName
		case e.FirstName != "":
			return e.FirstName
		default:
			return e.LastName
		}
	case *tg.Chat:
		return e.Title
	case *tg.ChatForbidden:
		return e.Title
	case *tg.Channel:
		return e.Title
	case *tg.ChannelForbidden:
		return e.Title
	}
	return ""
}

// ChatKind is user, bot, group, supergroup, forum or channel.
func ChatKind(entity any) string {
	switch e := entity.(type) {
	case *tg.User:
		if e.Bot {
			return "bot"
		}
		return "user"
	case *tg.Chat, *tg.ChatForbidden:
		return "group"
	case *tg.Channel:
		if e.Broadcast {
			return "channel"
		}
		if e.Forum {
			return "forum"
		}
	case *tg.ChannelForbidden:
		if e.Broadcast {
			return "channel"
		}
	}
	return "supergroup"
}

// ChatTitle is the display name, or the marked id when there is none.
func ChatTitle(entity any) string {
	if name := DisplayName(entity); name != "" {
		return name
	}
	return strconv.FormatInt(EntityID(entity), 10)
}

// Username is the entity's username for t.me links; nil without one.
func Username(entity any) *string {
	var name string
	switch e := entity.(type) {
	case *tg.User:
		name = e.Username
	case *tg.Channel:
		name = e.Username
	}
	if name == "" {
		return nil
	}
	return &name
}

// Describe is the logged-in account as the status shows it: id, name, username.
func Describe(me *tg.User) map[string]any {
	name := DisplayName(me)
	if name == "" {
		name = strconv.FormatInt(me.ID, 10)
	}
	var username any
	if me.Username != "" {
		username = me.Username
	}
	return map[string]any{"id": me.ID, "name": name, "username": username}
}

// Masked: +79161234567 -> +7916•••••67, enough to recognise it on the screen.
func Masked(phone string) string {
	r := []rune(phone)
	head := r[:min(5, len(r))]
	tail := r[max(0, len(r)-2):]
	return string(head) + strings.Repeat("•", max(0, len(r)-7)) + string(tail)
}

// MessageText is the plain text with hidden links spelled out: "форма (https://…)".
func MessageText(m *tg.Message) string {
	var links []*tg.MessageEntityTextURL
	for _, e := range m.Entities {
		if link, ok := e.(*tg.MessageEntityTextURL); ok {
			links = append(links, link)
		}
	}
	if len(links) == 0 {
		return m.Message
	}
	// Entity offsets count UTF-16 units; the last link first, so the earlier offsets stay right.
	sort.SliceStable(links, func(i, j int) bool {
		return links[i].Offset+links[i].Length > links[j].Offset+links[j].Length
	})
	text := utf16.Encode([]rune(m.Message))
	for _, link := range links {
		end := min(max(link.Offset+link.Length, 0), len(text))
		insert := utf16.Encode([]rune(" (" + link.URL + ")"))
		text = append(text[:end:end], append(insert, text[end:]...)...)
	}
	return string(utf16.Decode(text))
}

// OwnMedia: a photo or a document of the message itself. A link preview's picture or file tells nothing the link does
// not and often arrives later by an edit.
func OwnMedia(m *tg.Message) bool {
	switch media := m.Media.(type) {
	case *tg.MessageMediaPhoto:
		_, ok := media.Photo.(*tg.Photo)
		return ok
	case *tg.MessageMediaDocument:
		_, ok := media.Document.(*tg.Document)
		return ok
	}
	return false
}

func photoOf(m *tg.Message) *tg.Photo {
	if media, ok := m.Media.(*tg.MessageMediaPhoto); ok {
		if photo, ok := media.Photo.(*tg.Photo); ok {
			return photo
		}
	}
	return nil
}

func documentOf(m *tg.Message) *tg.Document {
	if media, ok := m.Media.(*tg.MessageMediaDocument); ok {
		if doc, ok := media.Document.(*tg.Document); ok {
			return doc
		}
	}
	return nil
}

// sizeBytes is utils._photo_size_byte_count; ok false where Telethon gives None.
func sizeBytes(size tg.PhotoSizeClass) (int, bool) {
	switch s := size.(type) {
	case *tg.PhotoSize:
		return s.Size, true
	case *tg.PhotoStrippedSize:
		if len(s.Bytes) < 3 || s.Bytes[0] != 1 {
			return len(s.Bytes), true
		}
		return len(s.Bytes) + 622, true
	case *tg.PhotoCachedSize:
		return len(s.Bytes), true
	case *tg.PhotoSizeEmpty:
		return 0, true
	case *tg.PhotoSizeProgressive:
		best := 0
		for i, n := range s.Sizes {
			if i == 0 || n > best {
				best = n
			}
		}
		return best, len(s.Sizes) > 0
	}
	return 0, false
}

// largestSize is the heaviest size of a photo (Telethon's file.size: often the largest dimensions) and its type;
// ok false when no size has bytes.
func largestSize(photo *tg.Photo) (int, string, bool) {
	best, kind, found := 0, "", false
	for _, size := range photo.Sizes {
		n, ok := sizeBytes(size)
		if !ok || n == 0 {
			continue
		}
		if !found || n > best {
			best, kind, found = n, size.GetType(), true
		}
	}
	return best, kind, found
}

// FileSize is the size of the message's own file (Telethon's message.file.size); 0 when unknown.
func FileSize(m *tg.Message) int64 {
	if photo := photoOf(m); photo != nil {
		n, _, _ := largestSize(photo)
		return int64(n)
	}
	if doc := documentOf(m); doc != nil {
		return doc.Size
	}
	return 0
}

func fileName(doc *tg.Document) (string, bool) {
	for _, attr := range doc.Attributes {
		if a, ok := attr.(*tg.DocumentAttributeFilename); ok {
			return a.FileName, true
		}
	}
	return "", false
}

// byAttribute is Telethon's _document_by_attribute: the first attribute of the kind decides, by the condition.
func byAttribute[T tg.DocumentAttributeClass](doc *tg.Document, condition func(T) bool) bool {
	for _, attr := range doc.Attributes {
		if a, ok := attr.(T); ok {
			return condition == nil || condition(a)
		}
	}
	return false
}

func documentKind(doc *tg.Document, name, mime string) string {
	switch {
	case byAttribute[*tg.DocumentAttributeSticker](doc, nil):
		return "sticker"
	case byAttribute(doc, func(a *tg.DocumentAttributeAudio) bool { return a.Voice }):
		return "voice"
	case byAttribute(doc, func(a *tg.DocumentAttributeVideo) bool { return a.RoundMessage }):
		return "video_note"
	case byAttribute[*tg.DocumentAttributeAnimated](doc, nil):
		return "gif"
	case byAttribute[*tg.DocumentAttributeVideo](doc, nil):
		return "video"
	case byAttribute(doc, func(a *tg.DocumentAttributeAudio) bool { return !a.Voice }):
		return "audio"
	}
	for _, t := range PDFTypes {
		if mime == t {
			return "pdf"
		}
	}
	if strings.HasSuffix(strings.ToLower(name), ".pdf") {
		return "pdf"
	}
	return "document"
}

// MessageFiles are the message's own files: [{kind, mime, size}] for a photo (its largest size), [{kind, name, mime,
// size}] for a document; nil without one.
func MessageFiles(m *tg.Message) []inbox.File {
	if !OwnMedia(m) {
		return nil
	}
	if photo := photoOf(m); photo != nil {
		var size any
		if n, _, ok := largestSize(photo); ok {
			size = n
		}
		return []inbox.File{{"kind": "photo", "mime": "image/jpeg", "size": size}}
	}
	doc := documentOf(m)
	name, hasName := fileName(doc)
	var nameValue any
	if hasName {
		nameValue = name
	}
	kind := documentKind(doc, name, strings.ToLower(doc.MimeType))
	return []inbox.File{{"kind": kind, "name": nameValue, "mime": doc.MimeType, "size": int(doc.Size)}}
}

// MessageForward is {from, chat_id, msg_id} of a forwarded message, nil for another one. `entities` give the name of
// the original sender or chat.
func MessageForward(m *tg.Message, entities tg.Entities) map[string]any {
	h := m.FwdFrom
	if !m.Flags.Has(2) && h.Date == 0 && h.FromID == nil && h.FromName == "" {
		return nil
	}
	var from, chatID, msgID any
	switch {
	case h.FromName != "":
		from = h.FromName
	case h.PostAuthor != "":
		from = h.PostAuthor
	default:
		if source := Lookup(entities, PeerID(h.FromID)); h.FromID != nil && source != nil {
			from = DisplayName(source)
		}
	}
	if h.FromID != nil {
		chatID = PeerID(h.FromID)
	}
	if h.ChannelPost != 0 {
		msgID = h.ChannelPost
	}
	return map[string]any{"from": from, "chat_id": chatID, "msg_id": msgID}
}

// SenderID is Telethon's sender_id: from_id, else the chat itself for a channel post or an incoming private message.
func SenderID(m *tg.Message) *int64 {
	if m.FromID != nil {
		id := PeerID(m.FromID)
		return &id
	}
	if m.PeerID != nil {
		if _, user := m.PeerID.(*tg.PeerUser); m.Post || (!m.Out && user) {
			id := PeerID(m.PeerID)
			return &id
		}
	}
	return nil
}

func optionalInt(n int) *int64 {
	if n == 0 {
		return nil
	}
	v := int64(n)
	return &v
}

// MessageFields are what a message carries for the batches, without the sender's name and the replied text (those
// may need a request).
func MessageFields(m *tg.Message, entities tg.Entities) inbox.Fields {
	var topicID, replyTo *int64
	if header, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok {
		replyTo = optionalInt(header.ReplyToMsgID)
		if header.ForumTopic {
			// In a forum topic every message "replies" to the topic; a real reply also has the topic as its top.
			if header.ReplyToTopID != 0 {
				topicID, replyTo = optionalInt(header.ReplyToTopID), optionalInt(header.ReplyToMsgID)
			} else {
				topicID, replyTo = optionalInt(header.ReplyToMsgID), nil
			}
		}
	}
	var grouped *int64
	if m.GroupedID != 0 {
		g := m.GroupedID
		grouped = &g
	}
	return inbox.Fields{
		SenderID:  SenderID(m),
		At:        Date(m.Date),
		Text:      MessageText(m),
		TopicID:   topicID,
		ReplyTo:   replyTo,
		Forward:   MessageForward(m, entities),
		GroupedID: grouped,
		Files:     MessageFiles(m),
		Pinned:    m.Pinned,
	}
}

// Date is a Telegram unix time in UTC.
func Date(unix int) time.Time { return time.Unix(int64(unix), 0).UTC() }

// MediaLocation is where the message's own file is downloaded from: the largest size of a photo, or the document.
func MediaLocation(m *tg.Message) (tg.InputFileLocationClass, bool) {
	if photo := photoOf(m); photo != nil {
		_, kind, ok := largestSize(photo)
		if !ok {
			return nil, false
		}
		return &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash,
			FileReference: photo.FileReference, ThumbSize: kind}, true
	}
	if doc := documentOf(m); doc != nil {
		return &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash,
			FileReference: doc.FileReference}, true
	}
	return nil, false
}
