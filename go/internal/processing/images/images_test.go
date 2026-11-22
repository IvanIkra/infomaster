package images

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const chat = -1001234567890

var (
	now       = time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()
	jpeg      = []byte("\xff\xd8\xff\xe0\x00\x10JFIF" + strings.Repeat("timetable", 20))
	png       = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("notice", 20))
	timetable = map[string]any{"shows": "Screenshot of the Calculus timetable",
		"text": "Пн 26.10 | 9:30 | R405\nКР-1 30.10 18:10"}
)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// fakeLLM is Haiku behind the real budget and journal: an image call is logged like a real one (the daily number
// counts them); the readings come in order.
type fakeLLM struct {
	budget   *llm.Budget
	readings []any
	calls    []llm.Request
}

func (f *fakeLLM) Available() bool { return true }

func (f *fakeLLM) JSON(ctx context.Context, req llm.Request) (json.RawMessage, error) {
	ticket, err := f.budget.Reserve(req.Task, 0.0001)
	if err != nil {
		return nil, err
	}
	defer f.budget.Release(ticket)
	f.calls = append(f.calls, req)
	spend(f.budget.Journal, 0, f.budget.Now(), req.Task)
	answer := f.readings[0]
	f.readings = f.readings[1:]
	if err, ok := answer.(error); ok {
		return nil, err
	}
	return json.Marshal(answer)
}

func spend(j *llm.Journal, usd float64, at time.Time, task string) {
	if err := j.Add(llm.Run{At: clock.UTCISO(at), Task: task, Model: llm.ModelID, OK: true, InputTokens: 1,
		OutputTokens: 1, CostUSD: pyjson.Float(usd), DurationS: 0.1}); err != nil {
		panic(err)
	}
}

type fixture struct {
	images *Images
	files  *files.Files
	inbox  *inbox.Inbox
	llm    *fakeLLM
	remote map[int64][]byte
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	s := must(store.Open(filepath.Join(dir, "x.db")))
	t.Cleanup(func() { s.Close() })
	general := must(settings.New(s))
	budget := llm.NewBudget(general, llm.NewJournal(s), s)
	budget.Now = func() time.Time { return now }
	f := &fixture{files: files.New(s, dir), inbox: inbox.Open(s), llm: &fakeLLM{budget: budget},
		remote: map[int64][]byte{}}
	f.files.Download = func(ctx context.Context, chatID, msgID int64, path string) (bool, error) {
		data, ok := f.remote[msgID]
		if !ok {
			return false, nil
		}
		return true, os.WriteFile(path, data, 0o600)
	}
	f.images = New(general, budget, f.files, f.llm, func() time.Time { return now })
	must(f.inbox.PutChat(chat, "Матан", "supergroup", nil, 0))
	return f
}

func (f *fixture) send(msgID int64, data []byte, text string) {
	f.remote[msgID] = data
	name, sender := "Староста", int64(7)
	must(f.inbox.Add(chat, msgID, inbox.Fields{SenderID: &sender, SenderName: &name, At: now.Add(-30 * time.Minute),
		Text: text, Files: []inbox.File{{"kind": "photo", "mime": "image/jpeg", "size": len(data)}}}, "tg",
		time.Time{}))
}

func (f *fixture) read(msgID int64) (*files.ImageReading, error) {
	ctx := context.Background()
	message := must(f.inbox.Message(chat, msgID, "tg"))
	row := must(f.files.FetchImage(ctx, message))
	return f.images.Read(ctx, must(f.inbox.Chat(chat)), true, message, row)
}

func want(t *testing.T, got *files.ImageReading, shows, text string) {
	t.Helper()
	if got == nil || got.Shows != shows || got.Text != text {
		t.Fatalf("%+v", got)
	}
}

func TestAnImageIsReadOnce(t *testing.T) {
	f := setup(t)
	f.send(1, jpeg, "Расписание на неделю")
	f.llm.readings = []any{timetable}
	want(t, must(f.read(1)), timetable["shows"].(string), timetable["text"].(string))
	call := f.llm.calls[0]
	if call.Task != "image" || !reflect.DeepEqual(call.Images, []llm.Image{{Data: jpeg, MediaType: "image/jpeg"}}) {
		t.Fatal(call.Task)
	}
	if !strings.Contains(call.Prompt, `Chat: "Матан" (study)`) ||
		!strings.Contains(call.Prompt, "The message with the image: Расписание на неделю") ||
		!strings.Contains(call.Prompt, "Posted: Tue 2026-10-20 11:30") {
		t.Fatal(call.Prompt)
	}
	// Kept: an edit does not read it again.
	if got := must(f.read(1)); got == nil || len(f.llm.calls) != 1 {
		t.Fatal("read again")
	}
	f.send(2, jpeg, "") // the same picture forwarded: its reading is taken
	want(t, must(f.read(2)), timetable["shows"].(string), timetable["text"].(string))
	if len(f.llm.calls) != 1 {
		t.Fatal("read twice")
	}
	status := must(f.images.Status())
	if *status.OKAt != "2026-10-20T12:00:00" || status.Error != nil || status.Daily != 30 || status.Today != 1 {
		t.Fatalf("%+v", status)
	}
	row := must(f.files.Row("tg", chat, 1))
	if *row.Reading != `{"shows": "Screenshot of the Calculus timetable", "text": "Пн 26.10 | 9:30 | R405\nКР-1 30.10 18:10"}` {
		t.Fatal(*row.Reading)
	}
}

func TestTheDailyNumberOfImages(t *testing.T) {
	f := setup(t)
	must(f.images.Settings.Set("images_daily", 2))
	spend(f.images.Budget.Journal, 0, now.Add(-24*time.Hour), "image") // yesterday's does not count
	spend(f.images.Budget.Journal, 0, now, "image")
	f.send(1, jpeg, "")
	f.send(2, png, "")
	f.llm.readings = []any{timetable}
	want(t, must(f.read(1)), timetable["shows"].(string), timetable["text"].(string))
	if got := must(f.read(2)); got != nil || len(f.llm.calls) != 1 { // 2 of 2 today
		t.Fatal("read past the daily number")
	}
	// The row stays unread: an edit tomorrow may read it.
	if row := must(f.files.Row("tg", chat, 2)); row.Reading != nil {
		t.Fatal(*row.Reading)
	}
}

func TestImagesStopWithTheBudget(t *testing.T) {
	f := setup(t)
	f.send(1, jpeg, "")
	spend(f.images.Budget.Journal, 0.42, now, "batch") // 84 % of $0.50: optional tasks are off
	if got := must(f.read(1)); got != nil || len(f.llm.calls) != 0 {
		t.Fatal("read with optional tasks off")
	}
	spend(f.images.Budget.Journal, 0.10, now, "batch") // all of it: the batch waits for midnight with its images
	if _, err := f.read(1); !llm.IsBudget(err) {
		t.Fatal(err)
	}
}

func TestAFailedReading(t *testing.T) {
	f := setup(t)
	f.send(1, jpeg, "")
	f.send(2, png, "")
	f.send(3, []byte("\xff\xd8\xff"+strings.Repeat("other", 9)), "")
	f.llm.readings = []any{llm.NewError(llm.BadAnswer, "request declined (None)"),
		llm.NewError(llm.Rejected, "Anthropic API error 400: bad image"),
		llm.NewError(llm.Failed, "cannot reach the Anthropic API")}
	for _, id := range []int64{1, 2, 1, 2} {
		if got := must(f.read(id)); got != nil {
			t.Fatal(id)
		}
	}
	if len(f.llm.calls) != 2 { // the same image would fail again: not tried twice
		t.Fatal(len(f.llm.calls))
	}
	if _, err := f.read(3); err == nil || llm.IsBadAnswer(err) { // on the way: the message stays in the queue
		t.Fatal(err)
	}
	if status := must(f.images.Status()); *status.Error != "cannot reach the Anthropic API" {
		t.Fatal(*status.Error)
	}
	f.llm.readings = []any{map[string]any{"shows": "", "text": ""}} // nothing on it
	if got := must(f.read(3)); got != nil || must(f.images.Status()).Error != nil {
		t.Fatal(got)
	}
	if row := must(f.files.Row("tg", chat, 3)); *row.Reading != "" {
		t.Fatal(*row.Reading)
	}
}

func TestALongTextIsCut(t *testing.T) {
	f := setup(t)
	f.send(1, jpeg, "")
	long := strings.Repeat("строка текста\n", 300)
	f.llm.readings = []any{map[string]any{"shows": "  Slide \n of  notes ", "text": "  a  \r\n\r\n" + long}}
	got := must(f.read(1))
	if got.Shows != "Slide of notes" || !strings.HasPrefix(got.Text, "a\nстрока текста\n") ||
		!strings.HasSuffix(got.Text, "строка текста\n…") || len([]rune(got.Text)) > TextChars+2 {
		t.Fatal(got.Shows, len([]rune(got.Text)))
	}
}
