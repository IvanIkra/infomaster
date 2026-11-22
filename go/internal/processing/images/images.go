// Package images is the images of chat messages (§5.1, D12): what Haiku reads on a photo or a screenshot, for the
// batches.
//
// In a chat with «Картинки» on, the image of a message waiting for its batch is downloaded by the twin
// (collectors/files) and read by Haiku in a call of its own (task `image`): what the picture is and the text on it,
// word for word. The batch sees the reading in place of the picture, so a screenshot of the timetable or a photo of a
// notice gives news, deadlines and events like a text does.
//
// Limits (§7): reading images is optional, so it stops above 80 % of the daily LLM limit, and at most `images_daily`
// images are read a Moscow day (failed calls count too). An image left unread goes to the batch as a "photo" only,
// and a message with nothing else to read is parsed without a call. When the whole limit is spent the batch waits for
// midnight with its images. The reading is kept with the file: an edited message is not read again, and the same
// image in another message (a forward, a repost) takes the reading it has.
package images

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

const (
	MaxTokens    = 3_000
	ShowsChars   = 300   // of what the image is
	TextChars    = 2_500 // of the text on it
	MessageChars = 500   // of the message's own text in the prompt
)

// System is the system prompt of an image's reading.
const System = `You read images from the Telegram chats of a university student (HSE University, Moscow, first year, ` +
	`Software Engineering). Another model then reads your answer together with the chat's messages, without the ` +
	`image, and picks out news, deadlines and events, so it has only what you write.

- shows: what the image is and what it shows, in 1-2 short English sentences: "Screenshot of the Calculus timetable ` +
	`for Oct 26-31", "Photo of a notice on the door of R405", "Lecture slide on series convergence", "Meme".
- text: the text on the image as written, in its language. Keep dates, times, rooms, names, numbers and links ` +
	`exactly. Keep the lines; a table goes row by row, its cells separated by " | ". Leave out what tells nothing: ` +
	`buttons and menus of an app, status bars, watermarks. "" when the image has no text worth reading. At most about ` +
	`2000 characters: for a longer text keep what matters for a student (dates, tasks, changes) and drop the rest.

Write only what is on the image; a word you cannot make out is "[?]". The message the image came with is given for ` +
	`context only: do not repeat it.
`

// Schema is the answer: {shows, text}, in this order.
var Schema = json.RawMessage(`{"type": "object", "properties": {"shows": {"type": "string"}, "text": {"type": ` +
	`"string"}}, "required": ["shows", "text"], "additionalProperties": false}`)

// Status is status(): the last reading, the last failure and the daily number.
type Status struct {
	OKAt  *string `json:"ok_at"`
	Error *string `json:"error"`
	Daily int     `json:"daily"`
	Today int     `json:"today"`
}

// Images reads images.
type Images struct {
	Settings *settings.Settings
	Budget   *llm.Budget
	Files    *files.Files
	LLM      llm.Model
	Now      func() time.Time

	mu   sync.Mutex
	okAt string // the last image read
	err  string // the last call failed on the way to the model
}

// New makes the reader; now nil is clock.NowUTC.
func New(st *settings.Settings, budget *llm.Budget, f *files.Files, model llm.Model, now func() time.Time) *Images {
	if now == nil {
		now = clock.NowUTC
	}
	return &Images{Settings: st, Budget: budget, Files: f, LLM: model, Now: now}
}

// Today is the images read today, Moscow day: the calls of the task, failed ones too.
func (im *Images) Today() (int, error) {
	return im.Budget.Journal.CountSince(clock.DayStartUTC(clock.LocalDay(im.Now())), "image")
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Status is the reader's status for the web app.
func (im *Images) Status() (Status, error) {
	daily, err := im.Settings.Int("images_daily")
	if err != nil {
		return Status{}, err
	}
	today, err := im.Today()
	if err != nil {
		return Status{}, err
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	return Status{OKAt: optional(im.okAt), Error: optional(im.err), Daily: daily, Today: today}, nil
}

// Read is what the image of a row (downloaded, see Files.FetchImage) shows, read now if need be. Nil when it is not
// read: the daily number is reached, optional tasks are off, the model could not.
//
// A *llm.BudgetExceeded when the whole limit is spent (the batch waits with its images) and an *llm.Error when the
// call failed on the way: the message stays in the queue.
func (im *Images) Read(ctx context.Context, chat *inbox.Chat, study bool, message *inbox.Message,
	row *files.Row) (*files.ImageReading, error) {
	if row.Reading != nil {
		return files.Reading(row), nil
	}
	same, err := im.Files.ReadAs(row)
	if err != nil {
		return nil, err
	}
	if same != nil {
		kept, err := im.Files.SetReading(row, *same, "")
		if err != nil {
			return nil, err
		}
		return files.Reading(kept), nil
	}
	daily, err := im.Settings.Int("images_daily")
	if err != nil {
		return nil, err
	}
	today, err := im.Today()
	if err != nil {
		return nil, err
	}
	if today >= daily {
		slog.Info(fmt.Sprintf("image of message %d in %d not read: the daily number of images is reached",
			message.MsgID, chat.ID))
		return nil, nil
	}
	path := im.Files.Path(row)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	chatID := chat.ID
	answer, err := im.LLM.JSON(ctx, llm.Request{Task: "image", System: System, Prompt: prompt(chat, study, message),
		Schema: Schema, MaxTokens: MaxTokens, ChatID: &chatID,
		Images: []llm.Image{{Data: data, MediaType: files.MediaType(data)}}})
	if err != nil {
		if llm.IsBudget(err) {
			level, lerr := im.Budget.Level()
			if lerr != nil {
				return nil, lerr
			}
			if level == llm.Hard {
				return nil, err
			}
			slog.Info(fmt.Sprintf("image of message %d in %d not read: optional LLM tasks are off", message.MsgID,
				chat.ID))
			return nil, nil
		}
		if llm.IsBadAnswer(err) || llm.IsRejected(err) { // the same image would fail again: it is not tried twice
			slog.Warn(fmt.Sprintf("image of message %d in %d not read: %v", message.MsgID, chat.ID, err))
			_, serr := im.Files.SetReading(row, "", err.Error())
			return nil, serr
		}
		im.mu.Lock()
		im.err = err.Error()
		if im.err == "" {
			im.err = fmt.Sprintf("%T", err)
		}
		im.mu.Unlock()
		return nil, err
	}
	im.mu.Lock()
	im.okAt, im.err = clock.LocalISO(im.Now()), ""
	im.mu.Unlock()
	var got map[string]any
	if err := json.Unmarshal(answer, &got); err != nil {
		return nil, llm.NewError(llm.BadAnswer, "the answer is not JSON: "+err.Error())
	}
	shows := pytext.Cut(strings.Join(pytext.Split(field(got, "shows")), " "), ShowsChars)
	var lines []string
	for _, line := range splitlines(field(got, "text")) {
		if line = pytext.Strip(line); line != "" {
			lines = append(lines, line)
		}
	}
	text := strings.Join(lines, "\n")
	if pytext.Len(text) > TextChars {
		text = pytext.Cut(text, TextChars)
		if i := strings.LastIndex(text, "\n"); i >= 0 {
			text = text[:i]
		}
		text += "\n…"
	}
	value := ""
	if shows != "" || text != "" {
		if value, err = pyjson.Dumps(files.ImageReading{Shows: shows, Text: text}); err != nil {
			return nil, err
		}
	}
	kept, err := im.Files.SetReading(row, value, "")
	if err != nil {
		return nil, err
	}
	return files.Reading(kept), nil
}

// field is str(answer.get(name) or "").
func field(answer map[string]any, name string) string {
	switch v := answer[name].(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if !v {
			return ""
		}
		return "True"
	default:
		return fmt.Sprint(v)
	}
}

func prompt(chat *inbox.Chat, study bool, message *inbox.Message) string {
	posted, err := clock.ParseLocal(message.At)
	when := message.At
	if err == nil {
		when = posted.Format("Mon 2006-01-02 15:04")
	}
	text := pytext.Cut(strings.Join(pytext.Split(message.Text), " "), MessageChars)
	if text == "" {
		text = "(no text)"
	}
	kind := "not a study chat"
	if study {
		kind = "study"
	}
	return strings.Join([]string{
		fmt.Sprintf("Chat: \"%s\" (%s)", chat.Title, kind),
		"Posted: " + when,
		"The message with the image: " + text,
	}, "\n")
}

// splitlines is str.splitlines(): lines split at \n, \r, \r\n, \v, \f, \x1c-\x1e, \x85, U+2028 and U+2029.
func splitlines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, s[start:i])
			start = i + size
		case '\r':
			lines = append(lines, s[start:i])
			if i+1 < len(s) && s[i+1] == '\n' {
				size = 2
			}
			start = i + size
		}
		i += size
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
