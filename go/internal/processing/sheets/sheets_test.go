package sheets_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	outdeadlines "github.com/IvanIkra/infomaster/go/internal/outputs/deadlines"
	outsheets "github.com/IvanIkra/infomaster/go/internal/outputs/sheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/batches"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/news"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/sheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const chat = int64(-1001234567890)

var ctx = context.Background()

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

func expect(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

func contains(t *testing.T, text, part string) {
	t.Helper()
	if !strings.Contains(text, part) {
		t.Fatalf("%q not in:\n%s", part, text)
	}
}

func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// makePDF is a PDF with one page per text (ASCII, one line each, Helvetica); "" makes a page without text, like a
// scan.
func makePDF(pages ...string) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var kids []string
	for _, page := range pages {
		var stream strings.Builder
		if page != "" {
			for i, line := range strings.Split(page, "\n") {
				line = strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(line)
				fmt.Fprintf(&stream, "BT /F1 12 Tf 50 %d Td (%s) Tj ET\n", 750-16*i, line)
			}
		}
		objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", stream.Len(), stream.String()))
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << "+
			"/Font << /F1 3 0 R >> >> /Contents %d 0 R >>", len(objects)))
		kids = append(kids, fmt.Sprintf("%d 0 R", len(objects)))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))
	out := []byte("%PDF-1.4\n")
	var offsets []int
	for n, body := range objects {
		offsets = append(offsets, len(out))
		out = append(out, fmt.Sprintf("%d 0 obj\n%s\nendobj\n", n+1, body)...)
	}
	xref := len(out)
	out = append(out, fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)...)
	for _, offset := range offsets {
		out = append(out, fmt.Sprintf("%010d 00000 n \n", offset)...)
	}
	out = append(out, fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1,
		xref)...)
	return out
}

var (
	sheet3      = makePDF("Calculus. Sheet 3. Series\nHand in by 29.10 23:59 in the LMS", "Task 1. Prove that the series")
	sheet3Fixed = makePDF("Calculus. Sheet 3. Series\nHand in by 30.10 23:59 in the LMS",
		"Task 1. Prove that the series converges")
	scan = makePDF("", "")
)

var names = map[string][2]string{
	"Математический анализ": {"Calculus", "Calc"}, "Алгебра": {"Algebra", "Alg"}, "Algebra": {"Algebra", "Alg"},
	"Calculus": {"Calculus", "Calc"},
}

func namer() *llm.Fake {
	return &llm.Fake{Respond: func(req llm.Request) (any, error) {
		items := []map[string]any{}
		for _, line := range strings.Split(req.Prompt, "\n") {
			if rest, ok := strings.CutPrefix(line, "- id "); ok {
				index, name, _ := strings.Cut(rest, ": ")
				if named, ok := names[strings.Trim(name, `"`)]; ok {
					items = append(items, map[string]any{"id": index, "title": named[0], "abbr": named[1]})
				}
			}
		}
		return map[string]any{"items": items}, nil
	}}
}

// fakeLLM answers the batches in order; the readings of sheets ("pdf") too, from their own list.
type fakeLLM struct {
	mu       sync.Mutex
	answers  []any
	readings []any
	calls    []llm.Request
}

func (f *fakeLLM) Available() bool { return true }

func (f *fakeLLM) JSON(ctx context.Context, req llm.Request) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
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

func (f *fakeLLM) last() llm.Request { return f.calls[len(f.calls)-1] }

type fakeClock struct{ moment time.Time }

func (c *fakeClock) now() time.Time { return c.moment }

type fixture struct {
	store     *store.Store
	clock     *fakeClock
	schedule  *schedule.Schedule
	files     *files.Files
	remote    map[[2]int64][]byte
	links     map[string][]byte
	asked     []string
	downloads []int64
	llm       *fakeLLM
	sheets    *sheets.Sheets
	batches   *batches.Batches
	topic     *outsheets.Publisher
	dir       string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	c := &fakeClock{moment: time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}
	r := ruz.New("", s)
	r.Now = c.now
	sch := schedule.New(s, r, events.Open(s), deadlines.Open(s), subjects.New(s, namer()), inbox.Open(s))
	sch.Now = c.now
	f := &fixture{store: s, clock: c, schedule: sch, remote: map[[2]int64][]byte{}, links: map[string][]byte{},
		llm: &fakeLLM{}, dir: t.TempDir()}
	f.files = files.New(s, f.dir)
	f.files.Download = func(ctx context.Context, chatID, msgID int64, path string) (bool, error) {
		f.downloads = append(f.downloads, msgID)
		data, ok := f.remote[[2]int64{chatID, msgID}]
		if !ok {
			return false, nil
		}
		return true, os.WriteFile(path, data, 0o600)
	}
	f.files.FetchURL = func(ctx context.Context, url, path string, limit int64) (string, error) {
		f.asked = append(f.asked, url)
		data, ok := f.links[url]
		if !ok {
			return "", nil
		}
		return "Sheet-3.pdf", os.WriteFile(path, data, 0o600)
	}
	chats := settings.NewChats(s)
	must(sch.Inbox.PutChat(chat, "Матан", "supergroup", nil, 0))
	must(chats.Update(chat, map[string]any{"enabled": true, "study": true}))
	st := must(settings.New(s))
	f.sheets = sheets.New(s, st, chats, sch.Inbox, f.files, sch.Deadlines, sch.Subjects, f.llm, c.now)
	f.batches = batches.New(s, st, chats, sch.Inbox, news.Open(s), sch.Deadlines, sch, f.llm, c.now)
	f.batches.Files, f.batches.Sheets = f.files, f.sheets
	f.topic = outsheets.New(f.sheets)
	return f
}

// send is a message of the chat, with a PDF the twin can download.
func (f *fixture) send(msgID int64, data []byte, name, body string, ago time.Duration) {
	fields := inbox.Fields{SenderID: ptr(int64(7)), SenderName: ptr("Ассистент"), At: f.clock.now().Add(-ago),
		Text: body}
	if data != nil {
		f.remote[[2]int64{chat, msgID}] = data
		fields.Files = []inbox.File{{"kind": "pdf", "name": name, "mime": "application/pdf", "size": len(data)}}
	}
	must(f.schedule.Inbox.Add(chat, msgID, fields, "tg", time.Time{}))
}

func sheetItem(msgID int64, number, due, course string) map[string]any {
	return map[string]any{"msg_ids": []int64{msgID}, "news": map[string]any{"full": "Calc sheet " + number + " posted",
		"short": "Calc sheet " + number, "importance": 5, "replaces": ""}, "deadlines": []any{}, "events": []any{},
		"sheets": []any{map[string]any{"msg_id": msgID, "course": course, "number": number, "due": due}}}
}

func calc(msgID int64, number, due string) map[string]any {
	return sheetItem(msgID, number, due, "Математический анализ")
}

func answer(items ...map[string]any) map[string]any { return map[string]any{"items": items} }

func (f *fixture) parse() batches.Changes {
	changes := batches.Changes{}
	noErr(f.batches.ParseChat(ctx, chat, &changes))
	return changes
}

func (f *fixture) publish() { noErr(f.topic.Publish(ctx)) }

func (f *fixture) get(id int64) *sheets.Sheet { return must(f.sheets.Get(id)) }

// found: a sheet found by a batch of its message.
func (f *fixture) found(msgID int64, data []byte, name string, item map[string]any) {
	f.send(msgID, data, name, "", 30*time.Minute)
	f.llm.answers = append(f.llm.answers, answer(item))
	f.parse()
}

// posted: a sheet found by a whole batch run: the run ends by syncing the deadlines that follow the timetable.
func (f *fixture) posted(msgID int64, data []byte, name string, item map[string]any) {
	f.send(msgID, data, name, "", 30*time.Minute)
	f.llm.answers = append(f.llm.answers, answer(item))
	must(f.batches.RunChats(ctx, []int64{chat}, nil))
}

func (f *fixture) waiting() []*sheets.Sheet { return must(f.sheets.Waiting()) }

// --- helpers -------------------------------------------------------------------------------------

func TestKeys(t *testing.T) {
	expect(t, sheets.NumberKey("№ 3") == "3" && sheets.NumberKey("ДЗ-2.1") == "дз-2.1", "keys")
}

func TestATitleInsteadOfANumber(t *testing.T) {
	expect(t, sheets.SheetLabel("Calc", "3") == "Calc sheet 3" && sheets.SheetLabel("Calc", "") == "Calc sheet", "1")
	expect(t, sheets.SheetLabel("Alg", "Test 1 problems") == "Alg: Test 1 problems", "2")
	expect(t, sheets.SheetLabel("", "Test 1 problems") == "Test 1 problems", "3")
	expect(t, sheets.SheetTitle("№ 4b") == "Sheet № 4b" && sheets.SheetTitle("Exam variant") == "Exam variant", "4")
	expect(t, sheets.SheetTitle("") == "Sheet", "5")
}

// D86: a kept text is read again from the kept file; without the file it stays.
func TestRereadKeptSheets(t *testing.T) {
	f := setup(t)
	f.found(1, sheet3, "list3.pdf", calc(1, "3", "2026-10-29"))
	id := f.waiting()[0].ID
	set := func() {
		must(f.store.DB.Exec("update sheets set text = 'garbled' where id = ?", id))
	}
	set()
	expect(t, must(f.sheets.Reread()) == 1, "read again")
	contains(t, text(f.get(id).Text), "Calculus. Sheet 3. Series")
	expect(t, must(f.sheets.Reread()) == 0, "no change")
	set()
	noErr(os.Remove(f.files.ByHash(f.get(id).Sha256)))
	expect(t, must(f.sheets.Reread()) == 0 && text(f.get(id).Text) == "garbled", "the file is gone")
}

// --- the batch -------------------------------------------------------------------------------------

func TestBatchReadsTheFirstPageAndFindsTheSheet(t *testing.T) {
	f := setup(t)
	f.send(1, sheet3, "list3.pdf", "Новый листок", 30*time.Minute)
	f.send(2, nil, "", "а когда сдавать?", 30*time.Minute)
	noNews := calc(2, "3", "2026-10-29")
	noNews["news"] = nil
	f.llm.answers = []any{answer(calc(1, "3", "2026-10-29"), noNews)} // 2 has no PDF: no sheet
	changes := f.parse()

	contains(t, f.llm.calls[0].Prompt, "  PDF \"list3.pdf\", 2 pages, first page:\n  | Calculus. Sheet 3. Series\n"+
		"  | Hand in by 29.10")
	expect(t, len(f.llm.calls[0].Documents) == 0, "no documents")
	waiting := f.waiting()
	expect(t, len(waiting) == 1 && reflect.DeepEqual(changes.Sheets, []int64{waiting[0].ID}), "waiting")
	s := waiting[0]
	expect(t, s.MsgID == 1 && s.Name == "list3.pdf" && text(s.Course) == "Математический анализ" && s.Number == "3" &&
		text(s.Due) == "2026-10-29", "sheet %+v", s)

	// The same file again (forwarded, posted twice) is no new sheet.
	f.send(3, sheet3, "list3 (1).pdf", "", 30*time.Minute)
	f.llm.answers = []any{answer(calc(3, "3", "2026-10-29"))}
	expect(t, len(f.parse().Sheets) == 0 && len(f.waiting()) == 1, "no new sheet")
}

func TestBatchAttachesAScan(t *testing.T) {
	f := setup(t)
	f.send(1, scan, "scan.pdf", "", 30*time.Minute)
	f.llm.answers = []any{answer(calc(1, "", "2026-10-29"))}
	f.parse()
	contains(t, f.llm.calls[0].Prompt, "  PDF \"scan.pdf\", 2 pages: a scan, its first page is attached as document 1")
	docs := f.llm.calls[0].Documents
	expect(t, len(docs) == 1 && docs[0].Pages == 1 && strings.HasPrefix(string(docs[0].Data), "%PDF"), "document")
}

func TestCheckKeepsSheetsOfReadPDFs(t *testing.T) {
	var a map[string]any
	d := json.NewDecoder(strings.NewReader(`{"items": [{"msg_ids": [1], "news": null, "deadlines": [], "events": [],
		"sheets": [{"msg_id": 1, "course": " Алгебра ", "number": " 2 ", "due": "2026-13-01"},
		{"msg_id": 1, "course": "", "number": "2", "due": ""}]}]}`))
	d.UseNumber()
	noErr(d.Decode(&a))
	expect(t, len(batches.Check(a, map[int64]bool{1: true}, nil, false)) == 0, "no PDF read: nothing")
	items := batches.Check(a, map[int64]bool{1: true}, map[int64]bool{1: true}, false)
	expect(t, len(items) == 1 && reflect.DeepEqual(items[0].Sheets, []sheets.Found{{MsgID: 1, Number: "2"}}),
		"one per message: %+v", items)
}

// --- publishing ----------------------------------------------------------------------------------

func TestPublishedAndVersions(t *testing.T) {
	f := setup(t)
	must(f.schedule.Subjects.Resolve(ctx, []string{"Математический анализ"}))
	f.found(1, sheet3, "list3.pdf", calc(1, "3", "2026-10-29"))
	f.publish()
	first := f.get(1)
	expect(t, first.Status == "posted" && first.Version == 1 && first.Previous == nil, "first %+v", first)
	expect(t, strings.HasPrefix(text(first.Text), "Calculus. Sheet 3") && len(f.llm.readings) == 0,
		"pdf_content is off: no reading")

	// The same number again: a newer version.
	f.found(2, sheet3Fixed, "list3_fixed.pdf", calc(2, "3", "2026-10-30"))
	f.publish()
	second := f.get(2)
	expect(t, second.Status == "posted" && *second.Previous == 1 && second.Version == 2, "second %+v", second)

	// Another number is another sheet.
	f.found(3, makePDF("Sheet 4"), "list3.pdf", calc(3, "4", "2026-10-29"))
	f.publish()
	expect(t, f.get(3).Previous == nil, "another sheet")

	f.found(4, makePDF("Sheet 3 again"), "list3_v3.pdf", calc(4, "3", "2026-10-29"))
	f.publish()
	expect(t, *f.get(4).Previous == 2 && f.get(4).Version == 3, "third version")
	var versions []int
	for _, s := range must(f.sheets.Listing()) {
		versions = append(versions, s.Versions)
	}
	expect(t, reflect.DeepEqual(versions, []int{1, 3}), "versions %v", versions)
}

func TestSameMessageNewFileIsAVersion(t *testing.T) {
	f := setup(t)
	f.found(1, sheet3, "list3.pdf", calc(1, "", "2026-10-29"))
	f.publish()
	f.remote[[2]int64{chat, 1}] = sheet3Fixed
	must(f.schedule.Inbox.Edit(chat, 1, "", []inbox.File{{"kind": "pdf", "name": "totally_other.pdf",
		"size": len(sheet3Fixed)}}, f.clock.now().Add(time.Minute)))
	f.llm.answers = append(f.llm.answers, answer(sheetItem(1, "", "2026-10-29", "")))
	f.parse()
	f.publish()
	expect(t, *f.get(2).Previous == 1, "a version")
}

func TestReadingTheWholeSheet(t *testing.T) {
	f := setup(t)
	must(f.sheets.ChatSettings.Update(chat, map[string]any{"pdf_content": true}))
	must(f.sheets.Settings.Set("pdf_pages", 1))
	must(f.sheets.Settings.Set("language", "ru")) // the summary is written in the app's language (D41)
	must(f.schedule.Subjects.Resolve(ctx, []string{"Математический анализ"}))
	f.llm.readings = []any{map[string]any{"summary": "Ряды: 6 задач, сдать в LMS.", "deadlines": []any{
		map[string]any{"label": "Calc sheet 3", "due": "2026-10-29"},
		map[string]any{"label": "Calc defence", "due": "2026-10-31"},
		map[string]any{"label": "a label far too long for Tablo", "due": "2026-10-31"},
		map[string]any{"label": "Calc", "due": "31.10"}}}}
	f.found(1, sheet3, "list3.pdf", calc(1, "3", "2026-10-29"))
	f.publish()
	reading := f.llm.last()
	expect(t, reading.Task == "pdf" && len(reading.Documents) == 0, "reading")
	contains(t, reading.System, "- summary: in Russian")
	contains(t, reading.System, `no "в этом листке"`)
	contains(t, reading.Prompt, "Course: Математический анализ, abbreviation Calc")
	contains(t, reading.Prompt, "Text of the sheet (1 of 2 pages):\n--- page 1 ---\nCalculus. Sheet 3")
	expect(t, !strings.Contains(reading.Prompt, "Task 1"), "past the page limit")
	expect(t, text(f.get(1).Summary) == "Ряды: 6 задач, сдать в LMS.", "summary")
	saved := map[string]*deadlines.Deadline{}
	for _, d := range must(f.schedule.Deadlines.Upcoming(clock.LocalDay(f.clock.now()), deadlines.Lookback)) {
		saved[d.Label] = d
	}
	expect(t, len(saved) == 2 && saved["Calc sheet 3"] != nil && saved["Calc defence"] != nil, "saved %v", saved)
	expect(t, *saved["Calc defence"].SheetID == 1 && reflect.DeepEqual(saved["Calc defence"].MsgIDs, []int64{1}), "of")
	expect(t, len(must(f.schedule.Deadlines.MadeOf("tg", chat, 1))) == 0, "an edit of the text keeps them")

	// A newer version: the model sees only the changed lines.
	f.llm.readings = []any{map[string]any{"changes": "Срок сдвинут на 30.10, в задаче 1 уточнено условие."}}
	f.found(2, sheet3Fixed, "list3_new.pdf", calc(2, "3", "2026-10-29"))
	f.publish()
	describing := f.llm.last()
	expect(t, describing.Task == "pdf", "task")
	contains(t, describing.Prompt, "Changed lines:\n-Hand in by 29.10 23:59 in the LMS")
	contains(t, describing.System, "Output changes: in Russian")
	contains(t, describing.System, "Изменилось только оформление.")
	contains(t, describing.Prompt, "+Task 1. Prove that the series converges")
	expect(t, text(f.get(2).Changes) == "Срок сдвинут на 30.10, в задаче 1 уточнено условие.", "changes")
	expect(t, text(f.get(1).Summary) == "Ряды: 6 задач, сдать в LMS.", "the first version's summary stays")
}

func TestAScanIsReadAsPagesAndTheLimitIsKept(t *testing.T) {
	f := setup(t)
	must(f.sheets.ChatSettings.Update(chat, map[string]any{"pdf_content": true}))
	must(f.sheets.Settings.Set("pdf_tokens", 4000)) // one page of a scan
	f.llm.readings = []any{&llm.BudgetExceeded{Msg: "optional LLM tasks are off above 80% of the daily limit"}}
	f.found(1, scan, "scan.pdf", calc(1, "3", "2026-10-29"))
	f.publish()
	reading := f.llm.last()
	expect(t, len(reading.Documents) == 1 && reading.Documents[0].Pages == 1, "documents")
	contains(t, reading.Prompt, "first 1 of 2 pages are attached")
	expect(t, f.get(1).Status == "posted" && f.get(1).Summary == nil, "published all the same")
}

func TestAFailureWaitsAndALostFileFails(t *testing.T) {
	f := setup(t)
	f.found(1, sheet3, "list3.pdf", calc(1, "3", "2026-10-29"))
	working := f.topic.Prepare
	f.topic.Prepare = func(ctx context.Context, id int64) (*sheets.Sheet, error) {
		return nil, fmt.Errorf("the twin is down")
	}
	f.publish()
	status := must(f.sheets.Status())
	expect(t, f.get(1).Status == "new" && status.Waiting == 1 && status.Error == nil, "waits")
	f.topic.Prepare = working
	f.publish()
	expect(t, f.get(1).Status == "posted", "posted")

	f.found(2, makePDF("Sheet 9"), "list3.pdf", calc(2, "9", "2026-10-29"))
	noErr(os.Remove(f.files.ByHash(f.get(2).Sha256)))
	f.publish()
	expect(t, f.get(2).Status == "failed" && text(f.get(2).Error) == "the file is deleted already", "failed")
}

func TestOwnerJoinsASheetToAnOlderOne(t *testing.T) {
	f := setup(t)
	f.found(1, sheet3, "list3.pdf", calc(1, "3", "2026-10-29"))
	f.found(2, sheet3Fixed, "ряды.pdf", calc(2, "3а", "2026-10-29")) // not recognised: published as new
	f.publish()
	expect(t, f.get(2).Previous == nil, "new")

	err := f.topic.Join(ctx, 1, 1)
	expect(t, err != nil && strings.Contains(err.Error(), "the same sheet"), "err %v", err)
	noErr(os.Remove(f.files.ByHash(f.get(2).Sha256))) // its file is gone: it is joined all the same
	noErr(f.topic.Join(ctx, 2, 1))
	joined := f.get(2)
	expect(t, *joined.Previous == 1 && joined.Version == 2 && joined.Status == "posted", "joined %+v", joined)
	err = f.topic.Join(ctx, 1, 2)
	expect(t, err != nil && strings.Contains(err.Error(), "has versions already"), "err %v", err)
	listing := must(f.sheets.Listing())
	expect(t, len(listing) == 1 && listing[0].ID == 2 && listing[0].First == 1 && listing[0].Versions == 2,
		"listing %+v", listing)
}

func TestADeletedMessageWithdrawsTheDeadlinesOfItsSheet(t *testing.T) {
	f := setup(t)
	must(f.sheets.ChatSettings.Update(chat, map[string]any{"pdf_content": true}))
	f.llm.readings = []any{map[string]any{"summary": "", "deadlines": []any{
		map[string]any{"label": "Calc defence", "due": "2026-10-31"}}}}
	f.found(1, sheet3, "list3.pdf", calc(1, "3", "2026-10-29"))
	f.publish()
	must(f.schedule.Inbox.Delete([]int64{1}, ptr(chat), f.clock.now()))
	changes := f.parse()
	expect(t, len(changes.Gone) == 1 && *must(f.schedule.Deadlines.Get(changes.Gone[0])).SheetID == 1,
		"gone %v", changes.Gone)
}

func TestOwnerChangesASheet(t *testing.T) {
	// The owner changes a sheet's fields (the web app's checks are the web layer's).
	f := setup(t)
	f.found(1, sheet3, "list3.pdf", calc(1, "3", "2026-10-29"))
	f.publish()
	s := must(f.topic.Edit(1, map[string]*string{"course": ptr("Линейная алгебра"), "number": ptr("3b"),
		"due": ptr("2026-11-02"), "summary": ptr("Series, 6 tasks.")}))
	expect(t, s.Number == "3b" && text(s.Due) == "2026-11-02" && text(s.Summary) == "Series, 6 tasks.", "%+v", s)
	s = must(f.topic.Edit(1, map[string]*string{"due": nil, "summary": nil}))
	expect(t, s.Due == nil && s.Summary == nil, "%+v", s)
	expect(t, must(f.topic.Edit(99, map[string]*string{"number": ptr("1")})) == nil, "no such sheet")
}

// --- the assumed due date (§6.3) ---------------------------------------------------------------------

// lesson is a class of the fictional RUZ feed: `start` is "MM-DD HH:MM" in 2026, 80 minutes long.
func lesson(n int, subject, start, kind string, cancelled bool) ruz.Class {
	begin := must(time.ParseInLocation("2006-01-02 15:04", "2026-"+start, clock.TZ))
	return ruz.Class{UID: fmt.Sprintf("30000%d@ruz.example", n), Subject: subject, Start: begin,
		End: begin.Add(80 * time.Minute), Kind: kind, Room: "R101", Cancelled: cancelled}
}

var lessons = []ruz.Class{
	lesson(1, "Математический анализ", "10-21 13:00", "Лекция", false),
	lesson(2, "Calculus", "10-22 09:30", "Seminar", false),
	lesson(3, "Математический анализ", "10-29 09:30", "Проведение семинаров (студенты)", false),
	lesson(4, "Algebra", "10-21 11:10", "Ongoing interim assessment", false), // never a sheet's class
	lesson(5, "Algebra", "10-23 13:00", "Лекция", false),
}

func timetableWith(classes ...ruz.Class) []ruz.Class {
	out := slices.Clone(lessons)
	for _, c := range classes {
		i := slices.IndexFunc(out, func(l ruz.Class) bool { return l.UID == c.UID })
		if i >= 0 {
			out[i] = c
		} else {
			out = append(out, c)
		}
	}
	return out
}

// timetable: RUZ classes of Calculus (lectures and seminars) and Algebra (no seminars); the sheets follow them.
func timetable(t *testing.T) *fixture {
	f := setup(t)
	f.sheets.Schedule = f.schedule
	f.schedule.Followers = append(f.schedule.Followers, f.sheets.Follow)
	must(f.schedule.Ruz.Sync(lessons, false))
	must(f.schedule.Subjects.Resolve(ctx, []string{"Математический анализ", "Calculus", "Алгебра", "Algebra"}))
	return f
}

func (f *fixture) assumedRows() []*deadlines.Deadline {
	var out []*deadlines.Deadline
	for _, d := range must(f.schedule.Deadlines.Upcoming(clock.LocalDay(f.clock.now()), deadlines.Lookback)) {
		if d.Assumed {
			out = append(out, d)
		}
	}
	return out
}

func newMerged(f *fixture) *merge.DeadlineList {
	sub := f.schedule.Subjects
	merged := merge.New(f.store, f.schedule.Deadlines, hseapp.New(f.store, sub, &llm.Fake{}), sub)
	merged.Now = f.clock.now
	return merged
}

func TestASheetWithoutADateIsDueAtTheNextSeminar(t *testing.T) {
	f := timetable(t)
	merged := newMerged(f)
	watch := outdeadlines.New(f.store, merged)
	watch.Now = f.clock.now
	var told []string
	watch.Notify = func(d todo.Deadline, was string) {
		told = append(told, fmt.Sprintf("%s|%s|%v|%s", d.Title, todo.DueText(d.Due), d.Assumed, was))
	}
	noErr(watch.Refresh())                             // the first look is quiet
	f.posted(1, sheet3, "list3.pdf", calc(1, "3", "")) // posted Tue 11:30: the lecture of Wed is no seminar
	s := f.get(1)
	expect(t, text(s.Due) == "2026-10-22" && text(s.DueAt) == "2026-10-22T09:30:00" && s.DueAssumed == 1, "%+v", s)
	rows := f.assumedRows()
	expect(t, len(rows) == 1, "rows %v", rows)
	row := rows[0]
	expect(t, row.Label == "Calc sheet 3" && row.Due == "2026-10-22" && *row.SheetID == 1 &&
		reflect.DeepEqual(row.MsgIDs, []int64{1}) && row.Source == "tg", "row %+v", row)
	var calcItems []merge.Item
	for _, item := range must(merged.Items()) {
		if strings.HasPrefix(item.Title, "Calc") {
			calcItems = append(calcItems, item)
		}
	}
	expect(t, len(calcItems) == 1 && calcItems[0].Title == "Calc sheet 3" && calcItems[0].Due == "2026-10-22" &&
		calcItems[0].Assumed, "items %+v", calcItems)

	f.publish()
	noErr(watch.Refresh())
	watch.Settle()
	expect(t, slices.Contains(told, "Calc sheet 3|2026-10-22|true|"), "pushed as new, the date marked assumed: %v",
		told)

	// The batch model sees the date as assumed.
	f.send(2, nil, "", "ок", 30*time.Minute)
	f.llm.answers = append(f.llm.answers, answer())
	must(f.batches.RunChats(ctx, []int64{chat}, nil))
	contains(t, f.llm.last().Prompt, fmt.Sprintf(`- d%d: "Calc sheet 3" 2026-10-22 assumed`, row.ID))
}

func TestACancelledSeminarIsSkipped(t *testing.T) {
	f := timetable(t)
	must(f.schedule.Ruz.Sync(timetableWith(lesson(2, "Calculus", "10-22 09:30", "Seminar", true)), false))
	f.posted(1, sheet3, "list3.pdf", calc(1, "3", ""))
	expect(t, text(f.get(1).Due) == "2026-10-29" && f.assumedRows()[0].Due == "2026-10-29", "next one")

	// A chat cancels that one too: nothing within 21 days, no date (and no deadline) until a class comes.
	uid := lessons[2].UID
	item := must(f.schedule.Ruz.ByUID(uid))
	must(f.schedule.Events.Add(events.New{UID: uid, ClassUID: uid, Source: "tg", ChatID: ptr(chat), MsgIDs: []int64{5},
		Course: item.Subject, Kind: item.Kind, Start: item.Start, End: item.End, Room: item.Room, Required: true,
		Cancelled: true}, f.clock.now()))
	changes := must(f.schedule.SyncDeadlines())
	s := f.get(1)
	expect(t, s.Due == nil && s.DueAssumed == 1 && len(f.assumedRows()) == 0 && len(changes.Gone) > 0, "%+v", s)
	must(f.schedule.Ruz.Sync(timetableWith(lesson(2, "Calculus", "10-22 09:30", "Seminar", true),
		lesson(6, "Calculus", "11-05 09:30", "Seminar", false)), false))
	must(f.schedule.SyncDeadlines())
	expect(t, text(f.get(1).Due) == "2026-11-05" && f.assumedRows()[0].Due == "2026-11-05", "a class came")
}

func TestACourseWithoutSeminarsTakesItsNextClass(t *testing.T) {
	f := timetable(t)
	f.posted(1, sheet3, "alg1.pdf", sheetItem(1, "1", "", "Алгебра"))
	s := f.get(1)
	expect(t, text(s.Due) == "2026-10-23" && text(s.DueAt) == "2026-10-23T13:00:00", "not the interim assessment")
	rows := f.assumedRows()
	expect(t, len(rows) == 1 && rows[0].Label == "Alg sheet 1", "rows %+v", rows)

	f.posted(2, makePDF("Sheet 1"), "h.pdf", sheetItem(2, "1", "", "История"))
	expect(t, f.get(2).Due == nil && f.get(2).DueAssumed == 1, "no classes at all: no date")
}

func deadlineItem(msgID int64, label, due, moves string) map[string]any {
	return map[string]any{"msg_ids": []int64{msgID}, "news": nil, "events": []any{}, "sheets": []any{},
		"deadlines": []any{map[string]any{"course": "Математический анализ", "label": label, "due": due,
			"moves": moves}}}
}

func TestARealDateWins(t *testing.T) {
	f := timetable(t)
	f.posted(1, sheet3, "list3.pdf", calc(1, "3", "2026-10-29")) // the first page gives the date: nothing assumed
	expect(t, text(f.get(1).Due) == "2026-10-29" && f.get(1).DueAssumed == 0 && len(f.assumedRows()) == 0, "real")

	// A later message moves the assumed deadline: the sheet takes the date, the mark goes.
	f.posted(2, makePDF("Sheet 4"), "list4.pdf", calc(2, "4", ""))
	row := f.assumedRows()[0]
	f.send(3, nil, "", "Листок 4 сдать до пятницы", 30*time.Minute)
	f.llm.answers = append(f.llm.answers, answer(deadlineItem(3, "Calc sheet 4", "2026-10-23",
		fmt.Sprintf("d%d", row.ID))))
	must(f.batches.RunChats(ctx, []int64{chat}, nil))
	s := f.get(2)
	expect(t, text(s.Due) == "2026-10-23" && s.DueAssumed == 0 && s.DueAt == nil, "sheet %+v", s)
	newer := must(f.schedule.Deadlines.Latest(row.ID))
	expect(t, newer.Due == "2026-10-23" && !newer.Assumed && len(f.assumedRows()) == 0, "newer %+v", newer)

	// The same work on the assumed date, told without a move: it is real now.
	f.posted(4, makePDF("Sheet 5"), "list5.pdf", calc(4, "5", ""))
	row = f.assumedRows()[0]
	f.send(5, nil, "", "Листок 5 к семинару в четверг", 30*time.Minute)
	f.llm.answers = append(f.llm.answers, answer(deadlineItem(5, "Calc sheet 5", row.Due, "")))
	must(f.batches.RunChats(ctx, []int64{chat}, nil))
	expect(t, !must(f.schedule.Deadlines.Get(row.ID)).Assumed && f.get(3).DueAssumed == 0, "real now")

	// The sheet's own pages give a date: it replaces the assumed one.
	f.publish()
	must(f.sheets.ChatSettings.Update(chat, map[string]any{"pdf_content": true}))
	f.llm.readings = []any{map[string]any{"summary": "", "deadlines": []any{
		map[string]any{"label": "Calc sheet 6", "due": "2026-10-30"}}}}
	f.posted(6, makePDF("Sheet 6"), "list6.pdf", calc(6, "6", ""))
	var sixth *sheets.Sheet
	for _, w := range f.waiting() {
		if w.MsgID == 6 {
			sixth = w
		}
	}
	expect(t, sixth.DueAssumed == 1 && f.assumedRows()[0].Due == "2026-10-22", "assumed first")
	f.publish()
	sixth = f.get(sixth.ID)
	expect(t, text(sixth.Due) == "2026-10-30" && sixth.DueAssumed == 0 && len(f.assumedRows()) == 0,
		"sheet %+v", sixth)
}

func TestTheOwnerClearsTheMark(t *testing.T) {
	// The sheet's half of the Python test; the owner's change of the deadline itself is the actions' (web layer).
	f := timetable(t)
	merged := newMerged(f)
	var changed []map[string]bool
	f.sheets.OnChange = func(refs map[string]bool) { changed = append(changed, refs) } // the owner's: no news
	f.posted(1, sheet3, "list3.pdf", calc(1, "3", ""))
	f.posted(2, makePDF("Sheet 4"), "list4.pdf", calc(2, "4", ""))
	f.publish()
	listed := map[int64]sheets.Listed{}
	for _, s := range must(f.sheets.Listing()) {
		listed[s.ID] = s
	}
	expect(t, listed[1].DueAssumed && text(listed[1].DueAt) == "2026-10-22T09:30:00", "listed %+v", listed[1])
	var calcs []string
	for _, d := range must(merged.Items()) {
		if strings.HasPrefix(d.Title, "Calc") {
			calcs = append(calcs, fmt.Sprintf("%s|%v", d.Title, d.Assumed))
		}
	}
	expect(t, reflect.DeepEqual(calcs, []string{"Calc sheet 3|true", "Calc sheet 4|true"}), "items %v", calcs)

	// The description alone keeps the date assumed; a date makes it real.
	must(f.topic.Edit(1, map[string]*string{"summary": ptr("Series.")}))
	expect(t, f.get(1).DueAssumed == 1, "still assumed")
	must(f.topic.Edit(1, map[string]*string{"due": ptr("2026-10-25")}))
	s := f.get(1)
	expect(t, text(s.Due) == "2026-10-25" && s.DueAssumed == 0, "real %+v", s)
	var real *deadlines.Deadline
	for _, d := range must(f.schedule.Deadlines.Upcoming(clock.LocalDay(f.clock.now()), deadlines.Lookback)) {
		if d.SheetID != nil && *d.SheetID == 1 {
			real = d
		}
	}
	expect(t, real != nil && real.Due == "2026-10-25" && !real.Assumed, "real %+v", real)
	expect(t, len(changed) > 0 && changed[len(changed)-1][fmt.Sprintf("dl:%d", real.ID)], "changed %v", changed)
}

func TestTheAssumedDateFollowsTheTimetable(t *testing.T) {
	f := timetable(t)
	f.posted(1, sheet3, "list3.pdf", calc(1, "3", ""))
	f.publish()
	first := f.assumedRows()[0]

	// RUZ moves the seminar to Friday: the date follows.
	diff := must(f.schedule.Ruz.Sync(timetableWith(lesson(2, "Calculus", "10-23 11:10", "Seminar", false)), false))
	changes := must(f.schedule.AfterPoll(ctx, *diff))
	row := f.assumedRows()[0]
	expect(t, row.Due == "2026-10-23" && *must(f.schedule.Deadlines.Get(first.ID)).MovedBy == row.ID, "row %+v", row)
	expect(t, reflect.DeepEqual(changes.Moved, [][2]int64{{first.ID, row.ID}}) &&
		text(f.get(1).DueAt) == "2026-10-23T11:10:00", "changes %+v", changes)

	// The owner moves the class in the web app: the date follows the feeds as they are.
	must(f.schedule.CorrectClass(ctx, lessons[1].UID, "2026-10-24T10:00:00", "2026-10-24T11:20:00", "R101", false, nil,
		nil, true))
	expect(t, f.assumedRows()[0].Due == "2026-10-24", "follows")

	// Once the class has started, the date stays.
	f.clock.moment = time.Date(2026, 10, 24, 10, 30, 0, 0, clock.TZ).UTC()
	must(f.schedule.CorrectClass(ctx, lessons[1].UID, "2026-10-25T10:00:00", "2026-10-25T11:20:00", "R101", false, nil,
		nil, true))
	expect(t, text(f.get(1).Due) == "2026-10-24", "stays")
}

func TestANewVersionTakesTheAssumedDateOver(t *testing.T) {
	f := timetable(t)
	f.posted(1, sheet3, "list3.pdf", calc(1, "3", ""))
	f.publish()
	first := f.assumedRows()[0]
	f.clock.moment = time.Date(2026, 10, 22, 12, 0, 0, 0, clock.TZ).UTC() // after Thursday's seminar
	f.posted(2, sheet3Fixed, "list3_fixed.pdf", calc(2, "3", ""))
	f.publish()
	expect(t, *f.get(2).Previous == 1, "a version")
	rows := f.assumedRows()
	expect(t, len(rows) == 1 && *rows[0].SheetID == 2 && rows[0].Due == "2026-10-29", "rows %+v", rows)
	expect(t, must(f.schedule.Deadlines.Latest(first.ID)).ID == rows[0].ID, "one deadline, moved")

	// Its message deleted withdraws the deadline: the sheet has no date any more and nothing comes back.
	must(f.schedule.Inbox.Delete([]int64{2}, ptr(chat), f.clock.now()))
	must(f.batches.RunChats(ctx, []int64{chat}, nil))
	expect(t, len(f.assumedRows()) == 0 && f.get(2).Due == nil && f.get(2).DueAssumed == 0, "no date")
}

// --- the library (D58) -------------------------------------------------------------------------------

func newLibrary(f *fixture, dir string) *library.Library {
	lib := must(library.New(f.store, dir))
	lib.Now = f.clock.now
	lib.SameCourse = func(a, b string) bool { return must(f.schedule.SameCourse(a, b)) }
	return lib
}

func TestSheetsGoToTheLibrary(t *testing.T) {
	f := setup(t)
	must(f.schedule.Ruz.Sync([]ruz.Class{lesson(9, "Математический анализ", "10-20 10:00", "Семинар", false)}, false))
	lib := newLibrary(f, filepath.Join(f.dir, "lib"))
	f.sheets.Library = lib

	f.found(1, sheet3, "list3.pdf", calc(1, "3", "2026-10-29")) // the sheet is sent at 11:30
	f.publish()
	items := must(lib.Items(library.Query{Kind: "sheet"}))
	expect(t, len(items) == 1, "items %+v", items)
	item := items[0]
	expect(t, item.Source == "sheet" && *item.ChatID == chat && *item.MsgID == 1 && item.Title == "Sheet 3" &&
		item.Author == "Ассистент", "item %+v", item)
	expect(t, text(item.Course) == "Математический анализ" && text(item.ClassUID) == "300009@ruz.example" &&
		text(item.Name) == "list3.pdf" && text(item.Day) == "2026-10-20", "item %+v", item)
	expect(t, string(must(os.ReadFile(lib.Path(item)))) == string(sheet3) && text(item.MediaType) == "application/pdf",
		"file")
	must(f.sheets.Prepare(ctx, 1)) // prepared again (a post that failed): taken once
	expect(t, len(must(lib.Items(library.Query{Kind: "sheet"}))) == 1, "once")

	// The same message with a new file is a new version: its file is taken too. A sheet of no course and no number
	// waits in the inbox.
	f.remote[[2]int64{chat, 1}] = sheet3Fixed
	must(f.schedule.Inbox.Edit(chat, 1, "", []inbox.File{{"kind": "pdf", "name": "list3_fixed.pdf",
		"size": len(sheet3Fixed)}}, f.clock.now().Add(time.Minute)))
	f.llm.answers = append(f.llm.answers, answer(sheetItem(1, "", "2026-10-29", "")))
	f.parse()
	f.publish()
	var newer *library.Material
	for _, i := range must(lib.Items(library.Query{Kind: "sheet"})) {
		if i.ID != item.ID {
			newer = i
		}
	}
	expect(t, newer != nil && newer.Title == "Sheet" && newer.Course == nil && newer.ClassUID == nil, "newer %+v",
		newer)
	expect(t, string(must(os.ReadFile(lib.Path(newer)))) == string(sheet3Fixed) && must(lib.InboxCount()) == 1,
		"inbox")
}

// --- sheets that come as no PDF of a chat (D59) ------------------------------------------------------

const dropbox = "https://www.dropbox.com/scl/fi/abc123/Sheet-3.pdf?rlkey=k9&dl=0"

func (f *fixture) linkMessage(msgID int64, body string) {
	must(f.schedule.Inbox.Add(chat, msgID, inbox.Fields{SenderID: ptr(int64(7)), SenderName: ptr("Ассистент"),
		At: f.clock.now().Add(-30 * time.Minute), Text: body}, "tg", time.Time{}))
}

func TestASheetBehindALink(t *testing.T) {
	f := setup(t)
	f.links[dropbox] = sheet3
	f.linkMessage(1, "Новый листок: "+dropbox)
	f.llm.answers = []any{answer(calc(1, "3", "2026-10-29"))}
	changes := f.parse()
	contains(t, f.llm.calls[0].Prompt, "  PDF \"Sheet-3.pdf\", 2 pages, downloaded from the link "+dropbox+
		", first page:")
	waiting := f.waiting()
	expect(t, len(waiting) == 1 && reflect.DeepEqual(changes.Sheets, []int64{waiting[0].ID}) &&
		waiting[0].Name == "Sheet-3.pdf", "waiting %+v", waiting)
	row := must(f.files.FetchLink(ctx, must(f.schedule.Inbox.Message(chat, 1, "tg"))))
	expect(t, text(row.URL) == dropbox && reflect.DeepEqual(f.asked, []string{dropbox}), "kept: not downloaded again")
}

func TestLinksOnlyInStudyChats(t *testing.T) {
	f := setup(t)
	must(f.sheets.ChatSettings.Update(chat, map[string]any{"study": false}))
	f.links[dropbox] = sheet3
	f.linkMessage(1, "смотрите "+dropbox)
	f.llm.answers = []any{answer()}
	f.parse()
	expect(t, len(f.asked) == 0 && !strings.Contains(f.llm.calls[0].Prompt, "downloaded from the link"), "no link")
}

func TestAnOldPDFOfAnotherChatIsRead(t *testing.T) {
	f := setup(t)
	must(f.sheets.ChatSettings.Update(chat, map[string]any{"study": false}))
	f.send(1, sheet3, "list3.pdf", "", 3*24*time.Hour) // after a pause of the twin
	f.send(2, nil, "", "старые новости", 3*24*time.Hour)
	f.llm.answers = []any{answer(calc(1, "3", "2026-10-29"))}
	f.parse()
	contains(t, f.llm.calls[0].Prompt, `"list3.pdf"`)
	expect(t, !strings.Contains(f.llm.calls[0].Prompt, "старые новости") && len(f.waiting()) == 1, "read")
}

func TestASheetInALetter(t *testing.T) {
	// The letter as Mail.Poll keeps it: its attachment first, then the letter (the IMAP part is collectors/mail's).
	f := setup(t)
	at := f.clock.now().Add(-time.Hour)
	row := must(f.files.KeepAttachment(7, at, "list3.pdf", sheet3Fixed))
	expect(t, text(row.Sha256) != "" && strings.HasPrefix(text(row.FirstPage), "Calculus. Sheet 3"), "kept")
	must(f.schedule.Inbox.AddLetter(7, inbox.Fields{SenderName: ptr(`Петров Иван <petrov@hse.ru>`), At: at,
		Text: "Добрый день! Новый листок во вложении.", Subject: "Листок 3",
		Files: []inbox.File{{"kind": "pdf", "name": "list3.pdf", "mime": "application/octet-stream",
			"size": len(sheet3Fixed)}}}))
	message := must(f.schedule.Inbox.Message(inbox.Mailbox, 7, "mail"))
	fetched := must(f.files.Fetch(ctx, message))
	expect(t, fetched != nil && text(fetched.Sha256) != "" && len(f.downloads) == 0, "no download")

	f.llm.answers = []any{answer(calc(7, "3", "2026-10-29"))}
	changes := batches.Changes{}
	noErr(f.batches.ParseChat(ctx, inbox.Mailbox, &changes))
	contains(t, f.llm.calls[0].Prompt, "  PDF \"list3.pdf\", 2 pages, first page:")
	waiting := f.waiting()
	expect(t, len(waiting) == 1 && sheets.Origin(waiting[0]) == "mail" && waiting[0].ChatID == inbox.Mailbox &&
		reflect.DeepEqual(changes.Sheets, []int64{waiting[0].ID}), "waiting %+v", waiting)
	expect(t, text(must(f.sheets.Listing())[0].Chat) == "✉ Листок 3", "chat")
}
