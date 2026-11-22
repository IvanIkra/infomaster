package inbox

import (
	"reflect"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

var now = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func noErr(err error) {
	if err != nil {
		panic(err)
	}
}

func ptr[T any](v T) *T { return &v }

func setup(t *testing.T) *Inbox {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	in := Open(s)
	must(in.PutChat(-1001, "Матан", "supergroup", nil, 100))
	must(in.PutChat(-5, "Группа", "group", nil, 100))
	must(in.PutChat(7, "Староста", "user", ptr("starosta"), 100))
	return in
}

func fields(text string, at time.Time) Fields {
	return Fields{SenderID: ptr(int64(7)), SenderName: ptr("Староста"), At: at, Text: text}
}

const hw = "Сдать ДЗ 5 до 22.10"

func TestChatPutKeepsLastID(t *testing.T) {
	in := setup(t)
	if must(in.PutChat(-1001, "Матанализ", "forum", ptr("calc"), 1)) {
		t.Fatal("not new")
	}
	chat := must(in.Chat(-1001))
	if chat.Title != "Матанализ" || chat.Kind != "forum" || *chat.Username != "calc" || chat.LastID != 100 {
		t.Fatal(chat)
	}
	noErr(in.Advance(-1001, 120))
	noErr(in.Advance(-1001, 110)) // a late live update never moves it back
	if must(in.Chat(-1001)).LastID != 120 {
		t.Fatal("moved back")
	}
}

func TestAddOnce(t *testing.T) {
	in := setup(t)
	files := []File{{"kind": "pdf", "name": "hw5.pdf", "mime": "application/pdf", "size": 10}}
	f := fields(hw, now)
	f.Files = files
	f.Forward = map[string]any{"from": "Кафедра", "chat_id": -1009, "msg_id": 3}
	if !must(in.Add(-1001, 101, f, "tg", time.Time{})) {
		t.Fatal("not added")
	}
	if must(in.Add(-1001, 101, fields("другое", now), "tg", time.Time{})) { // the catch-up meets a live update
		t.Fatal("added twice")
	}
	m := must(in.Message(-1001, 101, "tg"))
	if m.Text != hw || !must(SameFiles(m.Files, files)) || m.Files[0]["size"] != 10.0 {
		t.Fatal(m)
	}
	if m.At != "2026-10-20T12:00:00" { // Moscow local, no zone
		t.Fatal(m.At)
	}
	if m.Forward["from"] != "Кафедра" || m.ParsedAt != nil {
		t.Fatal(m)
	}
	chats := must(in.Chats()) // by title: Группа, Матан
	if chats[0].Messages != 0 || chats[1].Messages != 1 || chats[0].LastAt != nil {
		t.Fatal(chats)
	}
}

func TestEdit(t *testing.T) {
	in := setup(t)
	must(in.Add(-1001, 101, fields(hw, now), "tg", time.Time{}))
	if must(in.Edit(-1001, 101, hw, nil, now)) { // a reaction: the content is the same
		t.Fatal("edited")
	}
	if must(in.Message(-1001, 101, "tg")).EditedAt != nil {
		t.Fatal("edited_at")
	}
	if !must(in.Edit(-1001, 101, "Сдать ДЗ 5 до 24.10", nil, now.Add(time.Hour))) {
		t.Fatal("not edited")
	}
	m := must(in.Message(-1001, 101, "tg"))
	if m.Text != "Сдать ДЗ 5 до 24.10" || *m.EditedAt != "2026-10-20T13:00:00" {
		t.Fatal(m)
	}
	if must(in.Edit(-1001, 999, "нет такого", nil, now)) {
		t.Fatal("no such message")
	}
}

func TestEditOfALegacyPhoto(t *testing.T) {
	in := setup(t)
	f := fields("Расписание", now)
	f.Files = []File{{"kind": "photo"}}
	must(in.Add(-1001, 101, f, "tg", time.Time{}))
	f = fields("https://hse.ru", now)
	f.Files = []File{{"kind": "photo"}}
	must(in.Add(-1001, 102, f, "tg", time.Time{}))
	photo := []File{{"kind": "photo", "mime": "image/jpeg", "size": 900}}
	if must(in.Edit(-1001, 101, "Расписание", photo, now)) {
		t.Fatal("a sized photo")
	}
	if must(in.Edit(-1001, 102, "https://hse.ru", nil, now)) { // the preview is no file now
		t.Fatal("the preview")
	}
	if !reflect.DeepEqual(must(in.Message(-1001, 101, "tg")).Files, []File{{"kind": "photo"}}) {
		t.Fatal("files changed")
	}
	if !must(in.Edit(-1001, 101, "Расписание", []File{{"kind": "pdf", "name": "a.pdf", "size": 5}}, now)) {
		t.Fatal("a pdf is new")
	}
	if !must(in.Edit(-1001, 102, "https://hse.ru/news", nil, now)) { // a new text is
		t.Fatal("a new text")
	}
}

func TestDelete(t *testing.T) {
	in := setup(t)
	for _, chat := range []int64{-1001, -5, 7} {
		must(in.Add(chat, 101, fields(hw, now), "tg", time.Time{}))
	}
	if must(in.Delete([]int64{101}, nil, now)) != 2 { // account-wide ids: the group and the private chat
		t.Fatal("account-wide")
	}
	if must(in.Message(-1001, 101, "tg")).DeletedAt != nil {
		t.Fatal("the channel's")
	}
	if must(in.Delete([]int64{101}, ptr(int64(-1001)), now)) != 1 ||
		must(in.Delete([]int64{101}, ptr(int64(-1001)), now)) != 0 {
		t.Fatal("chat")
	}
}

func TestPinAndSenders(t *testing.T) {
	in := setup(t)
	must(in.Add(-1001, 101, fields(hw, now), "tg", time.Time{}))
	teacher := fields(hw, now)
	teacher.SenderID, teacher.SenderName = ptr(int64(8)), ptr("Преподаватель")
	must(in.Add(-1001, 102, teacher, "tg", time.Time{}))
	must(in.Add(-1001, 103, teacher, "tg", time.Time{}))
	noErr(in.Pin(-1001, []int64{101}, true))
	if !must(in.Message(-1001, 101, "tg")).Pinned {
		t.Fatal("pinned")
	}
	noErr(in.Pin(-1001, []int64{101}, false))
	if must(in.Message(-1001, 101, "tg")).Pinned {
		t.Fatal("unpinned")
	}
	senders := must(in.Senders(-1001))
	if len(senders) != 2 || senders[0].ID != 8 || senders[1].ID != 7 {
		t.Fatal(senders)
	}
}

func TestPrune(t *testing.T) {
	in := setup(t)
	day := 24 * time.Hour
	must(in.Add(-1001, 101, fields(hw, now.Add(-15*day)), "tg", time.Time{}))
	must(in.Add(-1001, 102, fields(hw, now.Add(-13*day)), "tg", time.Time{}))
	must(in.Add(-1001, 103, fields(hw, now.Add(-30*day)), "tg", now.Add(-2*day)))
	must(in.Add(-1001, 104, fields(hw, now.Add(-30*day)), "tg", time.Time{}))
	must(in.Edit(-1001, 104, "Сдать ДЗ 5 до 30.10", nil, now.Add(-day)))
	if must(in.Prune(now)) != 1 { // 103 and 104 count their 14 days from the edit (D74)
		t.Fatal("prune")
	}
	if must(in.Message(-1001, 101, "tg")) != nil || must(in.Message(-1001, 102, "tg")) == nil {
		t.Fatal("kept")
	}
	if *must(in.Message(-1001, 103, "tg")).EditedAt != "2026-10-18T12:00:00" || must(in.Message(-1001, 104, "tg")) == nil {
		t.Fatal("edited ones")
	}
	if clock.LocalISO(now) != "2026-10-20T12:00:00" {
		t.Fatal("clock")
	}
}

func TestQueue(t *testing.T) {
	in := setup(t)
	must(in.Add(-1001, 101, fields(hw, now), "tg", time.Time{}))
	must(in.Add(-1001, 102, fields("удалённое", now), "tg", time.Time{}))
	must(in.Delete([]int64{102}, ptr(int64(-1001)), now))
	pending := must(in.Pending(-1001, "tg"))
	if len(pending) != 1 || pending[0].MsgID != 101 || !reflect.DeepEqual(must(in.PendingCounts()),
		map[int64]int64{-1001: 1}) {
		t.Fatal(pending)
	}
	noErr(in.MarkParsed(-1001, []int64{101}, now, "tg"))
	if len(must(in.Pending(-1001, "tg"))) != 0 {
		t.Fatal("parsed")
	}
	must(in.Edit(-1001, 101, "Сдать ДЗ 5 до 24.10", nil, now.Add(time.Minute)))
	if pending := must(in.Pending(-1001, "tg")); len(pending) != 1 { // edited after its batch: parsed again
		t.Fatal(pending)
	}
}

func TestWake(t *testing.T) {
	in := setup(t)
	type woke struct {
		chat int64
		ids  []int64
	}
	var woken []woke
	in.Wake = func(chat int64, ids []int64) { woken = append(woken, woke{chat, ids}) }
	must(in.Add(-1001, 101, fields(hw, now), "tg", time.Time{}))
	must(in.Add(-1001, 101, fields(hw, now), "tg", time.Time{})) // stored already: no news
	noErr(in.Pin(-1001, []int64{101}, false))
	noErr(in.Pin(-1001, []int64{101}, true))
	if !reflect.DeepEqual(woken, []woke{{-1001, []int64{101}}, {-1001, []int64{101}}}) {
		t.Fatal(woken)
	}
}

func TestLettersAndSearch(t *testing.T) {
	in := setup(t)
	letter := fields("Расписание сессии", now)
	letter.Subject = "Сессия"
	if !must(in.AddLetter(5, letter)) || must(in.AddLetter(5, letter)) {
		t.Fatal("letter once")
	}
	if must(in.NewestLetter()) != 5 || must(in.Letters()) != 1 || *must(in.Subject([]int64{5})) != "Сессия" {
		t.Fatal("letters")
	}
	must(in.Add(-1001, 101, fields(hw, now.Add(time.Hour)), "tg", time.Time{}))
	found := must(in.Search("СЕССИ", nil, "", "", 30))
	if len(found) != 1 || found[0].Source != "mail" {
		t.Fatal(found)
	}
	if found := must(in.Search("", nil, "2026-10-20", "2026-10-20", 30)); len(found) != 2 || found[0].MsgID != 101 {
		t.Fatal(found)
	}
	if found := must(in.Search("", nil, "", "2026-10-19", 30)); len(found) != 0 {
		t.Fatal(found)
	}
}

func TestMessageLink(t *testing.T) {
	cases := []struct {
		chat Chat
		want string
	}{
		{Chat{ID: -1001234567890, Kind: "channel", Username: ptr("hse_news")}, "https://t.me/hse_news/5"},
		{Chat{ID: -1001234567890, Kind: "supergroup"}, "https://t.me/c/1234567890/5"},
		{Chat{ID: -5, Kind: "group"}, ""},
		{Chat{ID: 7, Kind: "user"}, ""},
	}
	for _, c := range cases {
		if got := MessageLink(&c.chat, 5); got != c.want {
			t.Fatal(got)
		}
	}
}
