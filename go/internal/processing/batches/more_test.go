package batches

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/library/feed"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/images"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
)

// --- images (D12) --------------------------------------------------------------------------------

var (
	jpeg       = []byte("\xff\xd8\xff\xe0\x00\x10JFIF" + strings.Repeat("timetable", 20))
	png        = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("notice", 20))
	screenshot = map[string]any{"shows": "Screenshot of the Calculus timetable",
		"text": "Пн 26.10 | 9:30 | R405\nКР-1 30.10 18:10"}
)

// imageLLM is Haiku behind the real budget and journal: a call is logged like a real one (the daily number of images
// counts them); the batches and the readings come in order, each from their own list.
type imageLLM struct {
	mu       sync.Mutex
	budget   *llm.Budget
	answers  []any
	readings []any
	calls    []llm.Request
}

func (f *imageLLM) Available() bool { return true }

func (f *imageLLM) JSON(ctx context.Context, req llm.Request) (json.RawMessage, error) {
	ticket, err := f.budget.Reserve(req.Task, 0.0001)
	if err != nil {
		return nil, err
	}
	defer f.budget.Release(ticket)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	spend(f.budget.Journal, 0, f.budget.Now(), req.Task)
	list := &f.readings
	if req.Task == "batch" {
		list = &f.answers
	}
	answer := (*list)[0]
	*list = (*list)[1:]
	if err, ok := answer.(error); ok {
		return nil, err
	}
	return json.Marshal(answer)
}

func (f *imageLLM) tasks() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Task)
	}
	return out
}

func spend(j *llm.Journal, usd float64, at time.Time, task string) {
	noErr(j.Add(llm.Run{At: clock.UTCISO(at), Task: task, Model: llm.ModelID, OK: true, InputTokens: 1,
		OutputTokens: 1, CostUSD: pyjson.Float(usd), DurationS: 0.1}))
}

type imageFixture struct {
	*fixture
	model     *imageLLM
	files     *files.Files
	remote    map[int64][]byte
	downloads []int64
	offline   bool
}

func imagesSetup(t *testing.T) *imageFixture {
	f := &imageFixture{fixture: setup(t), remote: map[int64][]byte{}}
	dir := t.TempDir()
	budget := llm.NewBudget(f.Settings, llm.NewJournal(f.store), f.store)
	budget.Now = f.clock.now
	f.model = &imageLLM{budget: budget}
	f.LLM = f.model
	f.files = files.New(f.store, dir)
	f.files.Download = func(ctx context.Context, chatID, msgID int64, path string) (bool, error) {
		f.downloads = append(f.downloads, msgID)
		if f.offline {
			return false, &files.NotNow{Msg: "the twin is not connected to Telegram"}
		}
		data, ok := f.remote[msgID]
		if !ok {
			return false, nil
		}
		return true, os.WriteFile(path, data, 0o600)
	}
	f.Files = f.files
	f.Images = images.New(f.Settings, budget, f.files, f.model, f.clock.now)
	must(f.ChatSettings.Update(chat, map[string]any{"images": true}))
	return f
}

// sendImage is a message of the chat with an image the twin can download.
func (f *imageFixture) sendImage(msgID int64, data []byte, body string) {
	var fs []inbox.File
	if data != nil {
		f.remote[msgID] = data
		fs = []inbox.File{{"kind": "photo", "mime": "image/jpeg", "size": len(data)}}
	}
	f.add(msgID, body, attach(fs...))
}

func newsItem(msgID int64, text string) map[string]any {
	return map[string]any{"msg_ids": l(msgID), "news": map[string]any{"full": text, "short": "Calc KR-1 Oct 30",
		"importance": 5, "replaces": ""}, "deadlines": []any{}, "events": []any{}, "sheets": []any{}}
}

func TestBatchSeesWhatWasRead(t *testing.T) {
	f := imagesSetup(t)
	f.sendImage(1, jpeg, "") // a photo alone
	f.sendImage(2, png, "кто-нибудь понял?")
	f.sendImage(3, []byte("\xff\xd8\xff"+strings.Repeat("cat", 9)), "") // nothing to read on it
	must(f.Inbox.Add(chat, 4, inbox.Fields{SenderID: ptr(int64(7)), At: f.Now(), Files: []inbox.File{{"kind": "sticker"}}},
		"tg", time.Time{}))
	f.model.readings = []any{screenshot, map[string]any{"shows": "Meme about exams", "text": ""},
		map[string]any{"shows": "", "text": ""}}
	f.model.answers = []any{answer(newsItem(1, "Calc KR-1 Oct 30 18:10"))}
	changes := f.parseChat(chat)

	expect(t, reflect.DeepEqual(f.model.tasks(), []string{"image", "image", "image", "batch"}), "%v", f.model.tasks())
	prompt := f.model.calls[len(f.model.calls)-1].Prompt
	contains(t, prompt, "- 1 · \"Староста\"")
	contains(t, prompt, "- 2 · \"Староста\"")
	lacks(t, prompt, "- 3 ·") // nothing to read: parsed without a call
	lacks(t, prompt, "- 4 ·")
	contains(t, prompt, "  Image, as another model read it: Screenshot of the Calculus timetable\n  | Пн 26.10 | 9:30 | "+
		"R405\n  | КР-1 30.10 18:10")
	contains(t, prompt, "  кто-нибудь понял?\n  Image, as another model read it: Meme about exams")
	expect(t, len(changes.News) == 1 && len(f.pending(chat)) == 0, "changes %+v", changes)

	// An edit parses the message again, and its image is not read again.
	must(f.Inbox.Edit(chat, 1, "КР перенесли", []inbox.File{{"kind": "photo", "mime": "image/jpeg", "size": len(jpeg)}},
		f.Now().Add(time.Minute)))
	f.model.answers = []any{answer(newsItem(1, "Calc KR-1 moved"))}
	f.parseChat(chat)
	expect(t, reflect.DeepEqual(f.model.tasks()[4:], []string{"batch"}), "%v", f.model.tasks())
	contains(t, f.model.calls[len(f.model.calls)-1].Prompt, "Screenshot of the Calculus timetable")
}

func TestWithoutImagesAPhotoAloneIsSkipped(t *testing.T) {
	f := imagesSetup(t)
	must(f.ChatSettings.Update(chat, map[string]any{"images": false}))
	f.sendImage(1, jpeg, "")
	f.sendImage(2, jpeg, "Смотрите расписание")
	f.model.answers = []any{answer()}
	f.parseChat(chat)
	expect(t, reflect.DeepEqual(f.model.tasks(), []string{"batch"}) && len(f.downloads) == 0, "no images")
	_, newer, _ := strings.Cut(f.model.calls[0].Prompt, "New messages:\n")
	lacks(t, newer, "- 1 ·")
	contains(t, newer, "files: photo") // a photo alone is only context
	expect(t, len(f.pending(chat)) == 0, "pending")

	must(f.ChatSettings.Update(chat, map[string]any{"images": true}))
	must(f.Settings.Set("images_daily", 0)) // none a day: a photo alone is parsed without a call
	f.sendImage(3, png, "")
	f.parseChat(chat)
	expect(t, reflect.DeepEqual(f.model.tasks(), []string{"batch"}) && len(f.pending(chat)) == 0, "no call")
}

func TestAFailedImageKeepsTheMessagesQueued(t *testing.T) {
	f := imagesSetup(t)
	f.sendImage(1, jpeg, "")
	f.sendImage(2, nil, "Завтра пары не будет")
	f.model.readings = []any{llm.NewError(llm.Failed, "cannot reach the Anthropic API")}
	err := f.ParseChat(ctx, chat, &Changes{})
	var e *llm.Error
	expect(t, err != nil && errors.As(err, &e), "err %v", err)
	expect(t, reflect.DeepEqual(f.pending(chat), l(1, 2)) && reflect.DeepEqual(f.model.tasks(), []string{"image"}),
		"queued")

	spend(f.model.budget.Journal, 0.60, f.Now(), "batch") // the limit is spent: the run waits with its images
	err = f.ParseChat(ctx, chat, &Changes{})
	expect(t, llm.IsBudget(err), "err %v", err)
	expect(t, reflect.DeepEqual(f.pending(chat), l(1, 2)), "queued")
}

func TestAFileTheTwinCannotDownloadNowWaits(t *testing.T) {
	f := imagesSetup(t)
	f.offline = true
	f.sendImage(1, jpeg, "")
	f.sendImage(2, png, "Расписание")
	f.sendImage(3, nil, "Завтра пары не будет")
	f.model.answers = []any{answer(newsItem(3, "Calc KR-1 Oct 30 18:10"))}
	f.parseChat(chat)
	expect(t, reflect.DeepEqual(f.model.tasks(), []string{"batch"}), "%v", f.model.tasks())
	lacks(t, f.model.calls[0].Prompt, "- 1 ·")
	lacks(t, f.model.calls[0].Prompt, "- 2 ·")
	expect(t, reflect.DeepEqual(f.pending(chat), l(1, 2)), "nothing is kept of the failure")
	var n int
	noErr(f.store.DB.QueryRow("select count(*) from files").Scan(&n))
	expect(t, n == 0, "files %d", n)

	f.offline = false
	f.model.readings = []any{screenshot, map[string]any{"shows": "Notice", "text": "R405 закрыта"}}
	f.model.answers = []any{answer(newsItem(1, "Calc KR-1 Oct 30 18:10"))}
	f.parseChat(chat)
	expect(t, reflect.DeepEqual(f.model.tasks()[1:], []string{"image", "image", "batch"}) && len(f.pending(chat)) == 0,
		"%v", f.model.tasks())
}

// --- letters (§5.2) -------------------------------------------------------------------------------

func TestBatchesParseLettersAsStudyMail(t *testing.T) {
	f := setup(t)
	// The letter as the mail collector keeps it (its IMAP part is collectors/mail's).
	must(f.Inbox.AddLetter(4, inbox.Fields{SenderName: ptr("Петров Иван <petrov@hse.ru>"),
		At: f.Now().Add(-48 * time.Hour), Text: "Сдать до пятницы 23:59.", Subject: "ДЗ 3 по алгебре"}))
	f.llm.push(answer(map[string]any{"msg_ids": l(4), "news": map[string]any{"full": "Alg HW 3 due Fri 23:59",
		"short": "Alg HW 3 → Fri", "importance": 5, "replaces": ""}, "deadlines": []any{map[string]any{
		"course": "Алгебра", "label": "Alg HW 3", "due": "2026-10-23", "moves": ""}}, "events": []any{}}))
	f.Mail = true
	must(f.ChatSettings.Update(chat, map[string]any{"enabled": false}))
	must(f.Inbox.PutChat(-1002, "Мемы", "supergroup", nil, 0))
	must(f.Inbox.PutChat(-1003, "Алгебра", "supergroup", nil, 0))
	must(f.ChatSettings.Update(-1002, map[string]any{"enabled": true}))
	must(f.ChatSettings.Update(-1003, map[string]any{"enabled": true, "study": true}))
	var boxes []int64
	for id := range must(f.boxes()) {
		boxes = append(boxes, id)
	}
	expect(t, reflect.DeepEqual(must(f.ordered(boxes)), l(-1003, inbox.Mailbox, -1002)), "D30")

	changes := f.parseChat(inbox.Mailbox)
	contains(t, f.prompt(0), "university mailbox")
	expect(t, len(changes.News) == 1, "two days old, still parsed: every letter is study")
	n := must(f.News.Get(changes.News[0]))
	expect(t, n.Source == "mail" && n.Study && n.ChatID == inbox.Mailbox, "news %+v", n)
	var labels []string
	for _, d := range f.upcoming() {
		if d.Source == "mail" {
			labels = append(labels, d.Label)
		}
	}
	expect(t, reflect.DeepEqual(labels, []string{"Alg HW 3"}), "labels %v", labels)
	expect(t, len(must(f.Inbox.Pending(inbox.Mailbox, "mail"))) == 0, "parsed")
}

// --- materials (D58) -------------------------------------------------------------------------------

func material(msgID int64, kind string) map[string]any {
	return map[string]any{"msg_id": msgID, "course": "Математический анализ", "kind": kind,
		"title": "Calc seminar board"}
}

// quiet is an item that only carries material: no news.
func quiet(msgIDs []int64, materials ...map[string]any) map[string]any {
	if materials == nil {
		materials = []map[string]any{}
	}
	return map[string]any{"msg_ids": msgIDs, "news": map[string]any{"full": "", "short": "", "importance": 1,
		"replaces": ""}, "deadlines": []any{}, "events": []any{}, "sheets": []any{}, "materials": materials}
}

func TestMaterialsCheck(t *testing.T) {
	withSheet := quiet(l(2), material(2, "photo"))
	withSheet["sheets"] = []any{map[string]any{"msg_id": 2, "course": "", "number": "1", "due": ""}}
	a := decoded(t, map[string]any{"items": []any{quiet(l(1), material(1, "photo"), material(1, "odd"),
		material(9, "photo")), withSheet}})
	items := Check(a, set(1, 2), set(2), true)
	expect(t, len(items) == 2, "items %+v", items)
	expect(t, items[0].News == nil && reflect.DeepEqual(items[0].Materials, []feed.Material{{MsgID: 1,
		Course: "Математический анализ", Kind: "file", Title: "Calc seminar board"}}), "one per message: %+v", items[0])
	expect(t, len(items[1].Materials) == 0, "message 2 is a sheet: no material")
	items = Check(a, set(1, 2), set(2), false) // not asked for: none
	expect(t, len(items) == 1 && reflect.DeepEqual(items[0].MsgIDs, l(2)) && len(items[0].Materials) == 0,
		"items %+v", items)
}

type schemaLLM struct {
	answers []any
	prompts []string
	schemas []any
}

func (f *schemaLLM) Available() bool { return true }

func (f *schemaLLM) JSON(ctx context.Context, req llm.Request) (json.RawMessage, error) {
	f.prompts = append(f.prompts, req.Prompt)
	f.schemas = append(f.schemas, req.Schema)
	answer := f.answers[0]
	f.answers = f.answers[1:]
	return json.Marshal(answer)
}

func TestOtherChatsAreNotAsked(t *testing.T) {
	f := setup(t)
	model := &schemaLLM{}
	f.LLM = model
	other := int64(-1002)
	must(f.Inbox.PutChat(other, "Флуд", "supergroup", nil, 0))
	must(f.ChatSettings.Update(other, map[string]any{"enabled": true, "study": false}))
	lib := must(library.New(f.store, filepath.Join(t.TempDir(), "lib")))
	f.Materials = must(feed.New(f.store, lib, f.Inbox, f.clock.now))
	pic := inbox.File{"kind": "photo", "mime": "image/jpeg", "size": 5000}

	f.add(1, "мем", inChat(other))
	f.add(2, "", inChat(other), attach(pic)) // a photo alone outside a study chat: skipped as before
	model.answers = []any{answer(quiet(l(1), material(1, "photo")))}
	noErr(f.ParseChat(ctx, other, &Changes{}))
	expect(t, len(model.schemas) == 1 && reflect.DeepEqual(model.schemas[0], Schema), "schema")
	lacks(t, model.prompts[0], "- 2 ·")
	expect(t, must(f.Materials.Waiting()) == 0, "nothing queued")

	f.Materials = nil // without the library a study chat is not asked either
	f.add(3, "", attach(pic))
	f.add(4, "доска")
	model.answers = []any{answer()}
	noErr(f.ParseChat(ctx, chat, &Changes{}))
	_, newer, _ := strings.Cut(model.prompts[len(model.prompts)-1], "New messages:\n")
	expect(t, reflect.DeepEqual(model.schemas[len(model.schemas)-1], Schema), "schema")
	lacks(t, newer, "- 3 ·") // 3, skipped, is an earlier message now
}
