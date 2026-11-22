package sheets_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/wiki"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/library/feed"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/batches"
	"github.com/IvanIkra/infomaster/go/internal/processing/botsheets"
)

// --- a PDF sent to the bot (D73) -----------------------------------------------------------------------

const owner = 4242

// toBot is the owner's private message to the bot with a PDF, and its download.
func toBot(msgID int64, data []byte, name, body string) (feed.BotMessage, botsheets.Download, *[]string) {
	message := feed.BotMessage{ChatID: owner, MsgID: msgID, At: clock.NowUTC(), Text: body,
		Files: []inbox.File{{"kind": "pdf", "name": name, "mime": "application/pdf", "size": len(data)}}}
	var downloads []string
	download := func(ctx context.Context, path string) (bool, error) {
		downloads = append(downloads, path)
		return true, os.WriteFile(path, data, 0o600)
	}
	return message, download, &downloads
}

type journaled struct {
	tool      string
	arguments map[string]any
	undo      []botsheets.Undo
}

func botSetup(t *testing.T) (*fixture, *botsheets.BotSheets, *[]journaled) {
	f := setup(t)
	f.sheets.Library = newLibrary(f, filepath.Join(f.dir, "library")) // as the app links them
	taken := botsheets.New(f.topic, f.sheets.Library)
	var log []journaled
	taken.Journal = func(tool string, arguments map[string]any, summary string, undo []botsheets.Undo) {
		log = append(log, journaled{tool, arguments, undo})
	}
	taken.CanUndo = func(op string) bool { return op == "_remove_own_sheet" }
	return f, taken, &log
}

func TestASheetSentToTheBotIsASheetWithItsCaption(t *testing.T) {
	f, bot, log := botSetup(t)
	caption := "ДЗ 5 по матану, сдать до пятницы"
	message, download, downloads := toBot(1, sheet3, "hw5.pdf", caption)
	f.llm.readings = []any{map[string]any{"sheet": true, "course": "Математический анализ", "number": "5",
		"due": "2026-10-30"}}
	expect(t, must(bot.Take(ctx, message, download)) == botsheets.Sheet, "a sheet")
	expect(t, len(f.llm.calls) == 1, "calls %d", len(f.llm.calls))
	call := f.llm.calls[0]
	expect(t, call.Task == "pdf" && len(call.Documents) == 0 && *call.ChatID == -1, "call %+v", call)
	contains(t, call.Prompt, caption)
	contains(t, call.Prompt, "Calculus. Sheet 3")
	listing := must(f.sheets.Listing())
	expect(t, len(listing) == 1, "listing %+v", listing)
	l := listing[0]
	expect(t, l.Own && text(l.Course) == "Математический анализ" && l.Number == "5" && text(l.Due) == "2026-10-30" &&
		text(l.Caption) == caption && l.Status == "posted", "listed %+v", l)
	contains(t, strings.Join(must(f.sheets.Context(f.get(l.ID))), "\n"), "The owner's caption: "+caption)
	expect(t, reflect.DeepEqual(*log, []journaled{{"add_sheet", map[string]any{"sheet_id": l.ID},
		[]botsheets.Undo{{Tool: "_remove_own_sheet", Arguments: map[string]any{"sheet_id": l.ID}}}}}), "log %+v", *log)
	_, err := os.Stat((*downloads)[0])
	expect(t, len(*downloads) == 1 && os.IsNotExist(err), "downloaded once, the part removed")
	// In the library as a sheet, not as an item of the inbox too.
	expect(t, len(must(bot.Library.Items(library.Query{Inbox: true}))) == 0 &&
		len(must(bot.Library.Items(library.Query{Kind: "sheet"}))) == 1, "library")

	again, download, _ := toBot(2, sheet3, "copy.pdf", "")
	expect(t, must(bot.Take(ctx, again, download)) == botsheets.Known && len(f.llm.calls) == 1 &&
		len(must(f.sheets.Listing())) == 1, "known")
}

func TestAPDFThatIsNoSheetGoesToTheInbox(t *testing.T) {
	f, bot, log := botSetup(t)
	lecture := makePDF("Lecture 4. Series")
	slides, download, downloads := toBot(1, lecture, "lec4.pdf", "slides")
	f.llm.readings = []any{map[string]any{"sheet": false, "course": "", "number": "", "due": ""}}
	expect(t, must(bot.Take(ctx, slides, download)) == botsheets.Inbox, "inbox")
	items := must(bot.Library.Items(library.Query{Inbox: true}))
	expect(t, len(items) == 1 && text(items[0].Name) == "lec4.pdf" &&
		string(must(os.ReadFile(bot.Library.Path(items[0])))) == string(lecture), "item %+v", items)
	expect(t, len(*downloads) == 1 && len(must(f.sheets.Listing())) == 0 && len(*log) == 0, "once, no sheet")

	// No answer from the model: the inbox as before; a scan goes to it as its first page.
	scanned, download, _ := toBot(2, scan, "scan.pdf", "")
	f.llm.readings = []any{&llm.BudgetExceeded{Msg: "the limit"}}
	expect(t, must(bot.Take(ctx, scanned, download)) == botsheets.Inbox, "inbox")
	expect(t, len(f.llm.last().Documents) == 1 && len(must(bot.Library.Items(library.Query{Inbox: true}))) == 2,
		"scan")

	// Not a PDF: no model at all.
	note := feed.BotMessage{ChatID: owner, MsgID: 3, At: clock.NowUTC(), Text: "a note"}
	expect(t, must(bot.Take(ctx, note, download)) == botsheets.Inbox && len(f.llm.calls) == 2, "note")
}

// --- course wikis (D60) ---------------------------------------------------------------------------------

func drive(id string) string { return "https://drive.google.com/file/d/" + id + "/view?usp=sharing" }

const wikitext = `== Seminar sheets ==
* [S1 Sheet 1]
* [S2 Sheet 2]

== Homework ==
{| class="wikitable"
|-
! Task !! colspan="3" | Due in group
|-
| || 101 || 102 || 103
|-
| [H1 HW 1a ] ||  18.09 ||  || 24.09
|-
| [H2 HW 1b] || 04.10 ||  || 08.10
|}
`

func TestANoticeBecomesASheet(t *testing.T) {
	f := setup(t)
	page := "https://wiki.example.org/Discrete_maths_26/27"
	must(f.sheets.Settings.Set("wiki_pages", []any{page}))
	w := wiki.New(f.store, f.sheets.Settings, f.schedule.Inbox, f.clock.now)
	body := strings.NewReplacer("S1", drive("1Sheet1AAAAA"), "S2", drive("1Sheet2AAAAA"), "H1", drive("1Hw1aAAAAAAA"),
		"H2", drive("1Hw1bAAAAAAA")).Replace(wikitext)
	w.Reader = func(ctx context.Context, api, title string) (int64, string, string, error) {
		return 1, title, body, nil
	}
	f.links[drive("1Hw1bAAAAAAA")] = sheet3
	must(w.Poll(ctx))
	f.llm.answers = []any{answer(sheetItem(-2, "1b", "2026-10-29", "Математический анализ"))}
	noErr(f.batches.ParseChat(ctx, inbox.Mailbox, &batches.Changes{}))
	prompt := f.llm.calls[0].Prompt
	contains(t, prompt, `Letters from "Course wiki"`)
	contains(t, prompt, "downloaded from the link")
	waiting := f.waiting()
	expect(t, len(waiting) == 1 && waiting[0].ChatID == inbox.Mailbox && waiting[0].MsgID == -2 &&
		waiting[0].Number == "1b", "waiting %+v", waiting)
	expect(t, len(must(f.schedule.Inbox.Pending(inbox.Mailbox, "mail"))) == 0, "both notices parsed")
}
