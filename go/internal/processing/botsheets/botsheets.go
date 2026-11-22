// Package botsheets is a PDF the owner sends the bot (D73): a sheet goes to the sheets, anything else to the
// library's inbox. The port of processing/bot_sheets.py.
//
// The bot is the intake of the library (D58), and the owner also sends it sheets the twin does not see: one forwarded
// from a chat it is not in, one from a letter, a photo-scan made into a PDF. Such a PDF is read as the batch reads a
// chat's (processing/batches): its name, its first page (a scan: the page itself) and, in place of the message, the
// owner's caption. Haiku (task `pdf`, an optional one) says whether it is a sheet, with its course, number and due
// date.
//
//   - A sheet becomes an own sheet (OwnChat, as one uploaded in the web app): its file kept for 14 days, the caption
//     kept with it (`sheets.caption`) and shown on its card, and read with the sheet when the model reads it
//     (Sheets.Context). It is published at once, copied into the library as kind `sheet` like every sheet, and
//     journaled with an undo.
//   - No sheet, no answer (no key, the limit spent, the model failed), a file too big for a sheet: the library's
//     inbox, as before. The file is downloaded once either way.
//   - The same PDF a sheet already: nothing new; the bot says so.
package botsheets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/library/feed"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	outsheets "github.com/IvanIkra/infomaster/go/internal/outputs/sheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/batches"
	"github.com/IvanIkra/infomaster/go/internal/processing/pdf"
	"github.com/IvanIkra/infomaster/go/internal/processing/sheets"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

const (
	CaptionChars = 1_000
	NumberChars  = 20
	MaxTokens    = 400
)

// What a message to the bot became.
const (
	Sheet = "sheet" // a new sheet
	Known = "known" // a sheet already
	Inbox = "inbox" // an item of the library's inbox
)

// Undo is a step of a change's undo, as the connector's journal keeps it.
type Undo struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// Download streams the message's file into a path; false when it is gone.
type Download func(ctx context.Context, path string) (bool, error)

// BotSheets takes the PDFs sent to the bot.
type BotSheets struct {
	Publisher *outsheets.Publisher
	Sheets    *sheets.Sheets
	Library   *library.Library
	// Journal keeps the owner's change, for its undo (Connector.journal): (tool, arguments, summary, undo).
	Journal func(tool string, arguments map[string]any, summary string, undo []Undo)
	// CanUndo: the journal knows this undo operation.
	CanUndo func(op string) bool
}

// New takes the PDFs sent to the bot.
func New(publisher *outsheets.Publisher, lib *library.Library) *BotSheets {
	return &BotSheets{Publisher: publisher, Sheets: publisher.Sheets, Library: lib,
		CanUndo: func(string) bool { return false }}
}

// found is what the model says of a sheet.
type found struct {
	course, number string
	due            *string
}

func squeeze(s string) string { return strings.Join(pytext.Split(s), " ") }

func token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o600)
}

// Take is the owner's private message to the bot: Sheet (a new sheet), Known (a sheet already), Inbox (an item of
// the library's inbox) or "" (nothing to keep). A *library.Error when the library refuses the file.
func (b *BotSheets) Take(ctx context.Context, message feed.BotMessage, download Download) (string, error) {
	inboxed := func(download Download) (string, error) {
		id, err := feed.FromBot(ctx, b.Library, message, download)
		if err != nil || id == nil {
			return "", err
		}
		return Inbox, nil
	}
	file := feed.MaterialFile(message.Files)
	if file == nil || file["kind"] != "pdf" || files.Size(file) > files.UploadMaxSize || !b.Sheets.LLM.Available() {
		return inboxed(download)
	}
	directory := b.Sheets.Files.Directory
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	part := filepath.Join(directory, "bot_"+token(6)+".part")
	defer os.Remove(part)
	ok, err := download(ctx, part)
	if err != nil || !ok {
		return "", err
	}
	data, err := os.ReadFile(part)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	known, err := b.Sheets.Known(hex.EncodeToString(sum[:]))
	if err != nil {
		return "", err
	}
	if known {
		return Known, nil
	}
	name := feed.FileName(file, message.MsgID)
	caption := pytext.Cut(squeeze(message.Text), CaptionChars)
	sheet, err := b.read(ctx, part, name, caption, message.Forward)
	if err != nil {
		return "", err
	}
	if sheet != nil {
		added, err := b.add(ctx, data, name, caption, sheet)
		if err != nil {
			return "", err
		}
		if added {
			return Sheet, nil
		}
	}
	// into the inbox from the file downloaded already
	return inboxed(func(_ context.Context, path string) (bool, error) { return true, copyFile(part, path) })
}

// read is what the model says of the PDF: its course, number and due date when it is a sheet; else nil.
func (b *BotSheets) read(ctx context.Context, path, name, caption string, forward map[string]any) (*found, error) {
	extract, err := pdf.ExtractText(path, 1)
	if err != nil {
		var p *pdf.PdfError
		if errors.As(err, &p) {
			slog.Info("the PDF sent to the bot cannot be read", "name", name, "err", err)
			return nil, nil
		}
		return nil, err
	}
	today := clock.LocalDay(clock.NowUTC())
	known, err := b.Sheets.Subjects.All()
	if err != nil {
		return nil, err
	}
	courses := make([]string, len(known))
	for i, n := range known {
		courses[i] = n.Name
	}
	slices.Sort(courses)
	list := "(none known)"
	if len(courses) > 0 {
		list = strings.Join(courses, "; ")
	}
	lines := []string{"Today: " + today.Format("Mon 2006-01-02"), "Courses: " + list, "",
		fmt.Sprintf("PDF \"%s\", %d pages", name, extract.Pages)}
	if from, _ := forward["from"].(string); from != "" {
		lines = append(lines, "Forwarded from: "+from)
	}
	if caption != "" {
		lines = append(lines, "The student's caption: "+caption)
	} else {
		lines = append(lines, "No caption.")
	}
	var documents []llm.Document
	if pdf.Scanned(extract.First()) {
		head, err := pdf.Head(path, 1)
		if err != nil {
			return nil, err
		}
		documents = []llm.Document{{Data: head, Pages: 1}}
		lines = append(lines, "It is a scan: its first page is attached.")
	} else {
		lines = append(lines, "First page:")
		for _, line := range batches.SplitLines(pytext.Cut(extract.First(), batches.FirstPageLimit)) {
			lines = append(lines, "| "+line)
		}
	}
	own := int64(files.OwnChat)
	raw, err := b.Sheets.LLM.JSON(ctx, llm.Request{Task: "pdf", System: System, Prompt: strings.Join(lines, "\n"),
		Schema: Schema, MaxTokens: MaxTokens, ChatID: &own, Documents: documents})
	if err != nil {
		var e *llm.Error
		switch {
		case llm.IsBudget(err):
			slog.Info("the PDF sent to the bot goes to the inbox unread", "name", name, "err", err)
			return nil, nil
		case errors.As(err, &e):
			slog.Warn("reading the PDF sent to the bot failed", "name", name, "err", err)
			return nil, nil
		}
		return nil, err
	}
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil {
		slog.Warn("reading the PDF sent to the bot failed", "name", name, "err", err)
		return nil, nil
	}
	if is, _ := answer["sheet"].(bool); !is {
		return nil, nil
	}
	course, _ := answer["course"].(string)
	number, _ := answer["number"].(string)
	return &found{course: squeeze(course), number: pytext.Cut(squeeze(number), NumberChars),
		due: sheets.DueDate(answer["due"])}, nil
}

// add keeps the PDF and makes an own sheet of it, published; false when the file cannot be kept (then the inbox).
func (b *BotSheets) add(ctx context.Context, data []byte, name, caption string, sheet *found) (bool, error) {
	now := clock.NowUTC()
	file, err := b.Sheets.Files.Keep(data, name, now)
	if err != nil {
		var refused *i18n.Error
		if errors.As(err, &refused) {
			slog.Info("the sheet sent to the bot is not kept", "name", name, "err", err)
			return false, nil
		}
		return false, err
	}
	sheetID, err := b.Sheets.AddOwn(file, sheet.course, sheet.number, sheet.due, now, caption)
	if err != nil {
		return false, err
	}
	if b.Journal != nil {
		var undo []Undo
		if b.CanUndo("_remove_own_sheet") {
			undo = []Undo{{Tool: "_remove_own_sheet", Arguments: map[string]any{"sheet_id": sheetID}}}
		}
		b.Journal("add_sheet", map[string]any{"sheet_id": sheetID}, "Added the sheet «"+name+"» sent to the bot",
			undo)
	}
	if err := b.Publisher.Publish(ctx); err != nil { // published at the next run
		slog.Error("the sheet sent to the bot is not posted yet", "sheet", sheetID, "err", err)
	}
	return true, nil
}
