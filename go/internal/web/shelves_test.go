package web

// The shelves (tests/test_web_shelves.py): the sheets with their PDFs and versions, the library's raw material by
// course and the sorting of its inbox. The sheets are a fake SheetTopic and the tools stubs that record their calls:
// what is tested is the screens' side (the sheets' own tests are processing/sheets').

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/library/links"
)

const calcName = "Математический анализ"

type fakeTopic struct {
	dir       string
	listed    []SheetListed
	versions  map[int64]*SheetVersion
	added     []string
	published int
}

func (f *fakeTopic) Listing() ([]SheetListed, error) { return f.listed, nil }
func (f *fakeTopic) Get(id int64) (*SheetVersion, error) {
	return f.versions[id], nil
}
func (f *fakeTopic) First(s *SheetVersion) (*SheetVersion, error) { return s, nil }
func (f *fakeTopic) Versions(first *SheetVersion) ([]*SheetVersion, error) {
	return []*SheetVersion{first}, nil
}
func (f *fakeTopic) ByHash(sha string) string {
	path := filepath.Join(f.dir, sha+".pdf")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}
func (f *fakeTopic) Keep(_ context.Context, data []byte, name string, _ time.Time) (KeptFile, error) {
	if !strings.HasPrefix(string(data), "%PDF") {
		return KeptFile{}, i18n.Err("not a PDF", "не PDF")
	}
	return KeptFile{Name: name, Row: data}, nil
}
func (f *fakeTopic) AddOwn(file KeptFile, course, number string, due *string, _ time.Time) (int64, error) {
	f.added = append(f.added, course+"|"+number+"|"+deref(due))
	return int64(100 + len(f.added)), nil
}
func (f *fakeTopic) Publish(context.Context) error { f.published++; return nil }
func (f *fakeTopic) CanRemoveOwn() bool            { return true }

type shelvesHarness struct {
	*harness
	lib   *library.Library
	topic *fakeTopic
	seen  *calls
}

func shelvesOf(t *testing.T, libraryOn bool) *shelvesHarness {
	sh := &shelvesHarness{topic: &fakeTopic{dir: t.TempDir(), versions: map[int64]*SheetVersion{}}, seen: &calls{}}
	h := newHarness(t, options{screens: true, connector: true, data: func(h *harness, d *Data) {
		must(h.sch.Subjects.SetManual(calcName, "Calculus", "Calc"))
		must(h.sch.Subjects.SetManual("Algebra", "Algebra", "Alg"))
		h.clock.set(15, 0)
		if libraryOn {
			sh.lib = must(library.New(h.store, t.TempDir()))
			sh.lib.Now = h.clock.now
			sh.lib.SameCourse = func(a, b string) bool { return must(h.sch.SameCourse(a, b)) }
			d.Library = sh.lib
		}
	}})
	sh.harness = h
	h.app.Parts.Sheets = sh.topic
	for _, name := range []string{"edit_sheet", "join_sheet", "delete_sheet", "sort_material", "update_material",
		"remove_material"} {
		stub(t, h.connector, name, []string{"sheet_id", "course", "number", "due", "summary", "previous", "id", "ids",
			"class_uid", "title", "note", "status"}, sh.seen.recorder(name, name != "join_sheet" && name != "delete_sheet"))
	}
	h.connector.Registry.Op("_noop", func(*tools.Context, map[string]any) (tools.Output, error) { return tools.Output{}, nil })
	return &shelvesHarness{harness: h.serve().in(), lib: sh.lib, topic: sh.topic, seen: sh.seen}
}

func (sh *shelvesHarness) shelf(method, path string, body any, status int) map[string]any {
	sh.t.Helper()
	return sh.ask(method, "/shelf"+path, body, status)
}

func (sh *shelvesHarness) twoSheets() {
	pdf := []byte("%PDF-1.4 sheet 3")
	noErr(sh.t, os.WriteFile(filepath.Join(sh.topic.dir, "aaa.pdf"), pdf, 0o600))
	sh.topic.versions[1] = &SheetVersion{ID: 1, ChatID: testChat, MsgID: 1, Name: "list3.pdf", Sha256: "aaa", Version: 1,
		Status: "posted", CreatedAt: "2026-10-20T10:00:00"}
	sh.topic.versions[2] = &SheetVersion{ID: 2, ChatID: testChat, MsgID: 2, Name: "list4.pdf", Sha256: "bbb", Version: 1,
		Status: "posted", CreatedAt: "2026-10-20T11:00:00"}
	sh.topic.listed = []SheetListed{
		{ID: 2, First: 2, Versions: 1, Status: "posted", Course: ptr(calcName), Number: "4", Name: "list4.pdf"},
		{ID: 1, First: 1, Versions: 1, Status: "posted", Course: ptr(calcName), Number: "3", Name: "list3.pdf", File: true}}
}

func TestHomeAndTheLibraryOff(t *testing.T) {
	sh := shelvesOf(t, true)
	sh.twoSheets()
	must(sh.lib.Add("text", library.Item{Source: "web", Text: ptr("a loose note"), Title: "Note"}))
	must(sh.lib.Add("text", library.Item{Source: "web", Text: ptr("Series"), Title: "Series", Course: calcName}))
	home := sh.shelf("GET", "", nil, 200)
	courses := list(home["courses"])
	if home["library"] != true || home["inbox"] != int64(1) || home["sheets"] != int64(2) || len(courses) != 1 ||
		obj(courses[0])["key"] != "Calc" || obj(courses[0])["title"] != "Calculus" {
		t.Fatal(home)
	}
	if sh.read("/feed")["inbox"] != int64(1) {
		t.Fatal("the feed marks the inbox")
	}
	bare := shelvesOf(t, false)
	home = bare.shelf("GET", "", nil, 200)
	if home["library"] != false || home["inbox"] != int64(0) || len(list(home["courses"])) != 0 {
		t.Fatal(home)
	}
	for _, path := range []string{"/inbox", "/raw?course=Calc", "/classes?course=Calc"} {
		if r := bare.shelf("GET", path, nil, 404); r["error"] != "the library is off" {
			t.Fatal(path, r)
		}
	}
	if list(bare.shelf("GET", "/sheets", nil, 200)["sheets"]) == nil {
		t.Fatal("the sheets do not need it")
	}
}

func TestSheetsAndACard(t *testing.T) {
	sh := shelvesOf(t, true)
	sh.twoSheets()
	listed := sh.shelf("GET", "/sheets", nil, 200)
	first := obj(list(listed["sheets"])[0])
	if first["id"] != int64(2) || first["course"] != "Calc" || first["course_title"] != "Calculus" || first["number"] != "4" {
		t.Fatal(first)
	}
	if len(list(sh.shelf("GET", "/sheets?course=Alg", nil, 200)["sheets"])) != 0 ||
		len(list(sh.shelf("GET", "/sheets?course=Calc", nil, 200)["sheets"])) != 2 {
		t.Fatal("lens")
	}
	card := sh.shelf("GET", "/sheets/1", nil, 200)
	version := obj(list(card["versions_list"])[0])
	if card["name"] != "list3.pdf" || version["file_url"] != "/web/api/shelf/sheets/1/file" ||
		len(list(card["candidates"])) != 1 || obj(list(card["candidates"])[0])["id"] != int64(2) {
		t.Fatal(card) // the kept file, behind the session
	}
	file := sh.call("GET", "/shelf/sheets/1/file", nil, req{token: sh.token})
	if file.Code != 200 || file.Body.String() != "%PDF-1.4 sheet 3" || file.Header().Get("Content-Type") != "application/pdf" ||
		file.Header().Get("Content-Disposition") != `inline; filename="list3.pdf"` ||
		file.Header().Get("Cache-Control") != "private, max-age=3600" {
		t.Fatal(file.Code, file.Header())
	}
	if sh.call("GET", "/shelf/sheets/1/file", nil, req{}).Code != 401 {
		t.Fatal("not without the session")
	}
	if obj(list(sh.shelf("GET", "/sheets/2", nil, 200)["versions_list"])[0])["file_url"] != nil {
		t.Fatal("gone")
	}
	sh.shelf("GET", "/sheets/2/file", nil, 404)
	sh.shelf("GET", "/sheets/99", nil, 404)
}

func TestSheetChanges(t *testing.T) {
	sh := shelvesOf(t, true)
	sh.twoSheets()
	changed := sh.shelf("PATCH", "/sheets/1", map[string]any{"course": "Alg", "number": "3b", "due": "2026-11-02"}, 200)
	got := sh.seen.last()
	if obj(changed["change"])["undo"] != true || changed["id"] != int64(1) || got["course"] != "Algebra" ||
		got["number"] != "3b" || got["sheet_id"] != int64(1) {
		t.Fatal(changed, got)
	}
	if r := sh.shelf("PATCH", "/sheets/1", map[string]any{"course": "Nope"}, 400); r["error"] != "no such course" {
		t.Fatal(r)
	}
	sh.shelf("PATCH", "/sheets/1", map[string]any{}, 400)
	sh.shelf("PATCH", "/sheets/1", map[string]any{"oops": 1}, 400)
	sh.shelf("PATCH", "/sheets/99", map[string]any{"number": "1"}, 404)
	sh.shelf("PATCH", "/sheets/1", map[string]any{"course": "", "due": nil}, 200)
	if got := sh.seen.last(); got["course"] != "" || got["due"] != nil {
		t.Fatal(got)
	}
	joined := sh.shelf("POST", "/sheets/2/join", map[string]any{"previous": 1}, 200)
	if obj(joined["change"])["undo"] != false || joined["id"] != int64(1) {
		t.Fatal(joined)
	}
	sh.shelf("POST", "/sheets/2/join", map[string]any{"previous": "1"}, 400)
	if obj(sh.shelf("DELETE", "/sheets/2", nil, 200)["change"])["undo"] != false {
		t.Fatal("delete")
	}
}

func TestAnOwnSheet(t *testing.T) {
	sh := shelvesOf(t, true)
	upload := func(fields map[string]string, data []byte, status int) map[string]any {
		parts := []formPart{}
		for name, value := range fields {
			parts = append(parts, formPart{name: name, data: []byte(value)})
		}
		if data != nil {
			parts = append(parts, formPart{name: "file", file: "mine.pdf", contentType: "application/pdf", data: data})
		}
		a := &adder{harness: sh.harness}
		return a.upload("/shelf/sheets", parts, status)
	}
	pdf := []byte("%PDF-1.4 mine")
	added := upload(map[string]string{"course": "Alg", "number": " 7 ", "due": "2026-11-05"}, pdf, 200)
	change := obj(added["change"])
	if added["id"] != int64(101) || change["undo"] != true || change["summary"] != "Added the own sheet «mine.pdf»" ||
		sh.topic.added[0] != "Algebra|7|2026-11-05" || sh.topic.published != 1 {
		t.Fatal(added, sh.topic.added)
	}
	if recent := must(sh.connector.Changes.Recent(1, "")); recent[0].Tool != "add_sheet" || recent[0].Via != "web" {
		t.Fatal(recent)
	}
	upload(map[string]string{"number": "1"}, nil, 400) // no file
	upload(map[string]string{"course": "Nope"}, pdf, 400)
	upload(map[string]string{"due": "2.11"}, pdf, 400)
	if r := upload(nil, []byte("not a pdf"), 400); r["error"] != "not a PDF" {
		t.Fatal(r)
	}
}

func TestSortingTheInboxAndTheRawShelf(t *testing.T) {
	sh := shelvesOf(t, true)
	note := must(sh.lib.Add("text", library.Item{Source: "web", Text: ptr("Series converge"), Title: "Series"}))
	photo := must(sh.lib.Add("photo", library.Item{Source: "web", Title: "Board"}))
	inbox := sh.shelf("GET", "/inbox", nil, 200)
	if len(list(inbox["items"])) != 2 {
		t.Fatal(inbox)
	}
	classes := list(sh.shelf("GET", "/classes?course=Calc", nil, 200)["classes"])
	if len(classes) == 0 || obj(classes[0])["uid"] == nil {
		t.Fatal(classes)
	}
	uid := obj(classes[0])["uid"].(string)
	sh.shelf("POST", "/sort", map[string]any{"ids": []any{note}, "course": "Calc"}, 200)
	if got := sh.seen.last(); got["course"] != calcName {
		t.Fatal(got)
	}
	sh.shelf("POST", "/sort", map[string]any{"ids": []any{photo}, "class_uid": uid}, 200)
	if got := sh.seen.last(); got["class_uid"] != uid {
		t.Fatal(got)
	}
	sh.shelf("POST", "/sort", map[string]any{"ids": []any{note}, "course": nil}, 200)
	if got, has := sh.seen.last()["course"]; !has || got != nil {
		t.Fatal(got, "back to the inbox")
	}
	// the raw shelf: an item placed by hand at a class
	must(sh.lib.Update(photo, map[string]any{"course": calcName, "class_uid": uid}))
	page := sh.shelf("GET", "/raw?course=Calc", nil, 200)
	if page["course"] != "Calc" || page["title"] != "Calculus" {
		t.Fatal(page)
	}
	sh.shelf("POST", "/sort", map[string]any{"ids": []any{note}, "course": "Nope"}, 400)
	sh.shelf("POST", "/sort", map[string]any{"ids": []any{}, "course": "Calc"}, 400)
	sh.shelf("POST", "/sort", map[string]any{"ids": []any{note}}, 400)
	sh.shelf("GET", "/raw?course=Nope", nil, 400)
	sh.shelf("GET", "/raw", nil, 400)
}

func TestAnItemCard(t *testing.T) {
	sh := shelvesOf(t, true)
	text := must(sh.lib.Add("text", library.Item{Source: "web", Text: ptr(strings.Repeat("Long ", 400)), Title: "Notes",
		Course: calcName}))
	whole := sh.shelf("GET", "/items/"+itoa(text), nil, 200)
	if len([]rune(whole["text"].(string))) != 2000 || whole["text_cut"] != false || whole["file_url"] != nil ||
		whole["course"] != "Calc" || whole["course_title"] != "Calculus" || len(list(whole["children"])) != 0 {
		t.Fatal(whole)
	}
	cut := obj(list(sh.shelf("GET", "/raw?course=Calc", nil, 200)["items"])[0])
	if len([]rune(cut["text"].(string))) != 600 || cut["text_cut"] != true {
		t.Fatal("the list shows a preview")
	}
	sh.shelf("GET", "/items/999", nil, 404)
	sh.shelf("PATCH", "/items/"+itoa(text), map[string]any{"title": "Slides 1", "note": "week 1"}, 200)
	if got := sh.seen.last(); got["_tool"] != "update_material" || got["id"] != text || got["title"] != "Slides 1" {
		t.Fatal(got)
	}
	sh.shelf("PATCH", "/items/"+itoa(text), map[string]any{"kind": "photo"}, 400)
	sh.shelf("PATCH", "/items/"+itoa(text), map[string]any{}, 400)
	sh.shelf("DELETE", "/items/"+itoa(text), nil, 200)
	sh.shelf("POST", "/remove", map[string]any{"ids": []any{text}}, 200)
	sh.shelf("POST", "/remove", map[string]any{"ids": []any{}}, 400)
}

func TestShelvesAreBehindTheSessionAndTheOrigin(t *testing.T) {
	sh := shelvesOf(t, true)
	for _, path := range []string{"/shelf", "/shelf/sheets", "/shelf/inbox"} {
		if sh.call("GET", path, nil, req{}).Code != 401 {
			t.Fatal(path)
		}
	}
	for _, path := range []string{"/shelf/sort", "/shelf/remove"} {
		if sh.call("POST", path, map[string]any{}, req{}).Code != 401 {
			t.Fatal(path)
		}
	}
	if sh.call("POST", "/shelf/sort", map[string]any{"ids": []any{1}, "course": "Calc"},
		req{token: sh.token, origin: ptr("https://evil.example")}).Code != 403 {
		t.Fatal("origin")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The lens links every version of a document (D69), each by a link of its own; «Machine» shows the library's disk.
func TestTheLensLinksEveryVersionOfADocument(t *testing.T) {
	sh := shelvesOf(t, true)
	lk := must(links.New(sh.store, sh.lib, testOrigin))
	sh.app.Data.Links = lk
	sh.app.Parts.Library = sh.lib
	lk.Routes(sh.mux)
	for _, n := range []string{"1", "2"} {
		draft := must(sh.lib.Draft(calcName, "book", "", "", "version "+n, nil))
		must(sh.lib.PutText(draft.ID, "source", "v"+n))
		must(sh.lib.Put(draft.ID, "pdf", strings.NewReader("%PDF-"+n)))
		must(sh.lib.Publish(draft.ID))
	}
	documents := list(sh.read("/course?key=Calc")["documents"])
	if len(documents) != 1 {
		t.Fatal(documents)
	}
	book := obj(documents[0])
	versions := list(book["versions"])
	if len(versions) != 2 || obj(versions[0])["version"] != int64(2) || obj(versions[1])["version"] != int64(1) ||
		obj(versions[0])["note"] != "version 2" || book["url"] != obj(versions[0])["url"] {
		t.Fatal(book)
	}
	for i, body := range []string{"%PDF-2", "%PDF-1"} {
		served := sh.get(strings.TrimPrefix(obj(versions[i])["url"].(string), testOrigin))
		if served.Code != 200 || served.Body.String() != body {
			t.Fatal(i, served.Code, served.Body)
		}
	}
	usage := obj(sh.ask("GET", "/machine", nil, 200)["library"])
	if usage["documents"] != int64(1) || usage["bytes"].(int64) <= 0 {
		t.Fatal(usage)
	}
	// an item's file opens by its signed link; a sheet's PDF by the library's copy first
	item := must(sh.lib.AddStream(strings.NewReader("%PDF slides"), "slides.pdf", "application/pdf", "",
		library.Item{Source: "web", Author: "owner", Course: calcName}))
	card := sh.shelf("GET", "/items/"+itoa(item), nil, 200)
	if served := sh.get(strings.TrimPrefix(card["file_url"].(string), testOrigin)); served.Body.String() != "%PDF slides" {
		t.Fatal(served.Code)
	}
	sh.twoSheets()
	sum := sha256.Sum256([]byte("%PDF-1.4 sheet 3"))
	sh.topic.versions[1].Sha256 = hex.EncodeToString(sum[:])
	noErr(t, os.Rename(filepath.Join(sh.topic.dir, "aaa.pdf"), filepath.Join(sh.topic.dir, sh.topic.versions[1].Sha256+".pdf")))
	copyID := must(sh.lib.AddStream(strings.NewReader("%PDF-1.4 sheet 3"), "list3.pdf", "application/pdf", "",
		library.Item{Source: "sheet", ChatID: ptr(testChat), MsgID: ptr(int64(1)), Course: calcName}))
	url := obj(list(sh.shelf("GET", "/sheets/1", nil, 200)["versions_list"])[0])["file_url"].(string)
	if !strings.HasPrefix(url, testOrigin+"/library/") {
		t.Fatal(url, copyID)
	}
	noErr(t, sh.lib.Remove(copyID))
	lk.Kept = sh.topic.ByHash // without the library's copy: the kept file by a signed link
	url = obj(list(sh.shelf("GET", "/sheets/1", nil, 200)["versions_list"])[0])["file_url"].(string)
	if served := sh.get(strings.TrimPrefix(url, testOrigin)); !strings.HasSuffix(url, "list3.pdf") ||
		served.Body.String() != "%PDF-1.4 sheet 3" {
		t.Fatal(url, served.Code)
	}
}

// D79: the public port reads at most BodyMax whole before anyone is known; an upload streams past it.
func TestABigPDFStreamsPastTheBodyLimitABigJSONDoesNot(t *testing.T) {
	sh := shelvesOf(t, true)
	big := append([]byte("%PDF-1.4 "), bytes.Repeat([]byte("0"), httpx.BodyMax+1<<20)...)
	a := &adder{harness: sh.harness}
	a.upload("/shelf/sheets", []formPart{{name: "course", data: []byte("Alg")},
		{name: "file", file: "big.pdf", contentType: "application/pdf", data: big}}, 200)
	huge := map[string]any{"text": strings.Repeat(" ", httpx.BodyMax+1)}
	if r := sh.call("POST", "/add/note", huge, req{token: sh.token}); r.Code != 413 || errorOf(r.Body.String()) == "" {
		t.Fatal(r.Code, r.Body.Len())
	}
}
