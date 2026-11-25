package catalog_test

// The library's tools (tests/test_connector_library.py): raw material, the inbox, the knowledge base, files by links.

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/library/links"
	sheetspub "github.com/IvanIkra/infomaster/go/internal/outputs/sheets"
)

var png = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func addClass(t *testing.T, f *fixture, uid, subject, start, kind string) {
	t.Helper()
	_, err := f.store.DB.Exec("insert into classes(uid, subject, start, end, kind, room, seen_at) "+
		"values(?, ?, ?, ?, ?, '', ?)", uid, subject, start, start[:11]+"23:00:00", kind, start)
	noErr(t, err)
}

func libraryFixture(t *testing.T) (*fixture, *library.Library) {
	return libraryFixtureWith(t, nil)
}

// libraryFixtureWith is libraryFixture with a change to the services before the tools are registered.
func libraryFixtureWith(t *testing.T, change func(*fixture)) (*fixture, *library.Library) {
	f := setup(t)
	addClass(t, f, "calc-1", "Calculus", "2026-10-19T09:30:00", "Лекция")
	addClass(t, f, "calc-2", "Calculus", "2026-10-20T11:10:00", "Семинар")
	addClass(t, f, "calc-3", "Calculus", "2026-10-27T09:30:00", "Лекция") // not yet
	addClass(t, f, "alg-1", "Algebra", "2026-10-16T13:00:00", "Lecture")
	lib := must(library.New(f.store, t.TempDir()))
	lib.Now = f.clock.now
	f.services.Library = lib
	ln := must(links.New(f.store, lib, publicURL))
	ln.Seconds = func() float64 { return 1_800_000_000 }
	f.services.Links = ln
	if change != nil {
		change(f)
	}
	return f.register(), lib
}

func (f *fixture) transfer(method, url string, body []byte) (int, []byte) {
	f.t.Helper()
	path := strings.TrimPrefix(url, publicURL)
	check(f.t, path != url, "not a public link: %s", url)
	req := must(http.NewRequest(method, f.server.URL+path, bytes.NewReader(body)))
	resp := must(http.DefaultClient.Do(req))
	defer resp.Body.Close()
	return resp.StatusCode, must(io.ReadAll(resp.Body))
}

func TestLibraryListing(t *testing.T) {
	f, _ := libraryFixture(t)
	byName := map[string]map[string]any{}
	listed := list(obj(f.rpc("tools/list", nil)["result"])["tools"])
	for _, tool := range listed {
		byName[obj(tool)["name"].(string)] = obj(tool)
	}
	names := []string{"list_library", "list_materials", "get_material", "list_classes", "list_documents",
		"get_document", "get_cheatsheet", "add_material", "sort_material", "update_material", "remove_material",
		"start_document_version", "publish_document_version"}
	for _, name := range names {
		check(t, obj(byName[name]["inputSchema"])["additionalProperties"] == false, "%s", name)
	}
	hint := func(name, key string) any { return obj(byName[name]["annotations"])[key] }
	equal(t, []any{hint("get_material", "readOnlyHint"), hint("add_material", "destructiveHint"),
		hint("sort_material", "destructiveHint"), hint("remove_material", "destructiveHint"),
		hint("publish_document_version", "destructiveHint")}, []any{true, false, true, true, false})
	check(t, strings.Contains(byName["start_document_version"]["description"].(string), "NOT solved"), "seminar")
}

func TestLibraryFlow(t *testing.T) {
	f, lib := libraryFixture(t)
	draft := f.data("add_material", map[string]any{"file_name": "lecture 3.m4a", "author": "owner"})
	equal(t, []any{draft["status"], draft["kind"], draft["course"]}, []any{"uploading", "audio", nil})
	equal(t, draft["curl"], "curl --fail-with-body -T 'lecture 3.m4a' "+draft["upload_url"].(string))
	check(t, strings.Contains(obj(draft["change"])["summary"].(string), "waiting for its upload"), "summary")
	equal(t, f.data("list_materials", map[string]any{"inbox": true})["items"], []any{})
	status, _ := f.transfer("PUT", draft["upload_url"].(string), []byte("m4a-bytes"))
	equal(t, status, 200)
	inbox := list(f.data("list_materials", map[string]any{"inbox": true})["items"])
	audio := obj(inbox[0])
	equal(t, []any{audio["id"], audio["kind"], obj(audio["file"])["media_type"], audio["transcripts"]},
		[]any{draft["id"], "audio", "audio/mp4", []any{}})
	equal(t, f.data("list_library", nil)["inbox"], 1.0)
	audioID := int64(audio["id"].(float64))

	sorted := f.data("sort_material", map[string]any{"ids": []any{audioID}, "class_uid": "calc-1"})
	equal(t, []any{sorted["course"], sorted["day"]}, []any{"Calculus", "2026-10-19"})
	equal(t, obj(sorted["change"])["summary"], "Sorted 1 item(s) into Calculus, lecture 2026-10-19")
	classes := f.data("list_classes", map[string]any{"course": "calculus"})
	listed := list(classes["classes"])
	equal(t, []any{obj(listed[0])["uid"], obj(listed[1])["uid"], len(listed)}, []any{"calc-2", "calc-1", 2})
	equal(t, []any{obj(listed[1])["items"], obj(listed[1])["kind"]}, []any{1.0, "lecture"})

	got := f.data("get_material", map[string]any{"id": audioID})
	_, hasText := got["text"]
	_, hasImage := got["image"]
	check(t, obj(got["class"])["kind"] == "lecture" && !hasText && !hasImage, "material %v", got)
	_, body := f.transfer("GET", got["download_url"].(string), nil)
	equal(t, string(body), "m4a-bytes")
	transcript := f.data("add_material", map[string]any{"text": strings.Repeat("Today: limits. ", 4000),
		"kind": "transcript", "parent": audioID, "class_uid": "calc-1"})
	transcriptID := int64(transcript["id"].(float64))
	got = f.data("get_material", map[string]any{"id": audioID})
	equal(t, []any{obj(list(got["children"])[0])["id"], got["transcripts"]},
		[]any{transcript["id"], []any{transcript["id"]}})
	long := f.data("get_material", map[string]any{"id": transcriptID, "max_chars": 1000})
	page := obj(long["text"])
	equal(t, []any{len(page["text"].(string)), page["next_offset"], page["total_chars"]}, []any{1000, 1000.0, 60000.0})
	check(t, strings.HasPrefix(long["preview"].(string), "Today: limits."), "preview")
	rest := obj(f.data("get_material", map[string]any{"id": transcriptID, "offset": 59500})["text"])
	equal(t, []any{len(rest["text"].(string)), rest["next_offset"]}, []any{500, nil})

	photo := f.data("add_material", map[string]any{"file_name": "board.png", "class_uid": "calc-1"})
	status, _ = f.transfer("PUT", photo["upload_url"].(string), png)
	equal(t, status, 200)
	photoID := int64(photo["id"].(float64))
	result := f.call("get_material", map[string]any{"id": photoID})
	image := obj(list(result["content"])[1])
	equal(t, []any{image["mimeType"], must(base64.StdEncoding.DecodeString(image["data"].(string)))},
		[]any{"image/png", png})
	equal(t, len(list(f.call("get_material", map[string]any{"id": photoID, "image": false})["content"])), 1)

	journaled := len(must(f.conn.Changes.Recent(1000, "")))
	started := f.data("start_document_version", map[string]any{"course": "Calculus", "kind": "lecture",
		"class_uid": "calc-1", "title": "Limits", "materials": []any{audioID, transcriptID, photoID},
		"source_text": `\section{Limits}`, "pdf_file": "build/limits.pdf"})
	missing := obj(started["missing"])
	_, hasSource := missing["source"]
	_, hasChange := started["change"]
	check(t, started["source_uploaded"] == true && !hasSource && !hasChange, "started %v", started)
	check(t, strings.HasPrefix(obj(missing["pdf"])["curl"].(string), "curl --fail-with-body -T build/limits.pdf "),
		"curl")
	equal(t, len(must(f.conn.Changes.Recent(1000, ""))), journaled)
	check(t, strings.Contains(f.refused("publish_document_version", map[string]any{"draft_id": started["draft_id"]}),
		"PDF is not uploaded"), "no PDF")
	status, _ = f.transfer("PUT", obj(missing["pdf"])["upload_url"].(string), []byte("%PDF-1.7 limits"))
	equal(t, status, 200)
	published := f.data("publish_document_version", map[string]any{"draft_id": started["draft_id"]})
	processed := []float64{}
	for _, id := range list(published["processed"]) {
		processed = append(processed, id.(float64))
	}
	sort.Float64s(processed)
	equal(t, []any{published["version"], processed}, []any{1.0, []float64{float64(audioID), float64(transcriptID),
		float64(photoID)}})
	equal(t, obj(published["change"])["summary"],
		"Published Calculus lecture 2026-10-19 v1: «Limits»; 3 item(s) marked processed")
	equal(t, must(lib.Get(audioID, false, false)).Status, "processed")
	_, body = f.transfer("GET", published["pdf_url"].(string), nil)
	equal(t, string(body), "%PDF-1.7 limits")

	documents := list(f.data("list_documents", map[string]any{"course": "Calculus"})["documents"])
	doc := obj(documents[0])
	equal(t, []any{len(documents), doc["kind"], doc["class_uid"], doc["version"]}, []any{1, "lecture", "calc-1", 1.0})
	listed = list(f.data("list_classes", map[string]any{"course": "Calculus"})["classes"])
	equal(t, []any{obj(list(obj(listed[1])["documents"])[0])["id"], obj(listed[1])["new"]}, []any{doc["id"], 0.0})
	document := f.data("get_document", map[string]any{"course": "Calculus", "kind": "lecture", "class_uid": "calc-1"})
	equal(t, obj(document["source"])["text"], `\section{Limits}`)
	chosen := obj(document["chosen"])
	check(t, chosen["pdf_url"] != nil, "pdf url")
	equal(t, chosen["materials"], []any{float64(audioID), float64(transcriptID), float64(photoID)})
	check(t, strings.Contains(f.refused("get_document", map[string]any{"doc_id": doc["id"], "version": 2}),
		"no version 2"), "version 2")

	f.undoLast()
	equal(t, len(must(lib.Documents(ptr("Calculus")))), 0)
	equal(t, must(lib.Get(audioID, false, false)).Status, "new")
	equal(t, f.data("list_documents", nil)["documents"], []any{})
}

func TestLibraryCheatsheet(t *testing.T) {
	f, _ := libraryFixture(t)
	empty := f.data("get_cheatsheet", map[string]any{"course": "Algebra"})
	check(t, empty["cheatsheet"] == nil && strings.Contains(empty["hint"].(string), "start_document_version"), "empty")
	check(t, strings.Contains(f.refused("start_document_version", map[string]any{"course": "Algebra",
		"kind": "cheatsheet", "source_text": "x", "pdf_file": "x.pdf"}), "no PDF"), "no PDF")
	started := f.data("start_document_version", map[string]any{"course": "Algebra", "kind": "cheatsheet",
		"source_text": "# Notation\n- $A^T$: transpose"})
	equal(t, []any{started["missing"], started["next"]},
		[]any{map[string]any{}, "call publish_document_version with draft_id"})
	f.data("publish_document_version", map[string]any{"draft_id": started["draft_id"]})
	sheet := f.data("get_cheatsheet", map[string]any{"course": "Algebra"})
	check(t, sheet["version"] == 1.0 && strings.HasPrefix(obj(sheet["cheatsheet"])["text"].(string), "# Notation"),
		"sheet %v", sheet)
	check(t, strings.Contains(f.refused("publish_document_version", map[string]any{"draft_id": started["draft_id"]}),
		"published already"), "again")
	lecture := f.data("start_document_version", map[string]any{"course": "Algebra", "kind": "lecture",
		"class_uid": "alg-1"})
	missing := obj(lecture["missing"])
	check(t, len(missing) == 2 && strings.Contains(obj(missing["source"])["curl"].(string), "-T main.tex "),
		"missing %v", missing)
}

// A problem sheet sends the model to its course's cheat sheet, once there is one; other items do not.
func TestLibraryBeforeSolving(t *testing.T) {
	f, _ := libraryFixture(t)
	add := func(kind string, course any) map[string]any {
		args := map[string]any{"text": "Problem 1 of " + kind, "kind": kind}
		if course != nil {
			args["course"] = course
		}
		return f.data("get_material", map[string]any{"id": f.data("add_material", args)["id"]})
	}
	equal(t, add("sheet", "Algebra")["before_solving"], nil) // no cheat sheet yet

	started := f.data("start_document_version", map[string]any{"course": "Algebra", "kind": "cheatsheet",
		"source_text": "# Notation"})
	equal(t, add("sheet", "Algebra")["before_solving"], nil) // a draft is not one
	f.data("publish_document_version", map[string]any{"draft_id": started["draft_id"]})

	hint, _ := add("sheet", "algebra")["before_solving"].(string)
	check(t, strings.Contains(hint, `get_cheatsheet with course "Algebra"`), "hint %q", hint)
	equal(t, add("notes", "Algebra")["before_solving"], nil)
	equal(t, add("sheet", "Calculus")["before_solving"], nil)
	equal(t, add("sheet", nil)["before_solving"], nil)
}

// A chat's or an own sheet read by get_sheet comes with the same hint.
func TestSheetBeforeSolving(t *testing.T) {
	var pub *sheetspub.Publisher
	f, _ := libraryFixtureWith(t, func(f *fixture) {
		pub = f.sheets()
		f.services.Sheets = pub
	})
	id := must(pub.Sheets.AddOwn(&files.Row{MsgID: 1, Sha256: ptr("abc")}, "algebra", "3", nil, f.clock.now(), ""))
	read := func() any { return f.data("get_sheet", map[string]any{"sheet_id": id})["before_solving"] }
	equal(t, read(), nil)
	started := f.data("start_document_version", map[string]any{"course": "Algebra", "kind": "cheatsheet",
		"source_text": "# Notation"})
	f.data("publish_document_version", map[string]any{"draft_id": started["draft_id"]})
	hint, _ := read().(string)
	check(t, strings.Contains(hint, `get_cheatsheet with course "Algebra"`), "hint %q", hint)
}

// The guide is one document over all courses: Markdown, no course, no PDF, and it comes with every cheat sheet.
func TestLibraryGuide(t *testing.T) {
	f, lib := libraryFixture(t)
	empty := f.data("get_guide", nil)
	check(t, empty["guide"] == nil && strings.Contains(empty["hint"].(string), "kind guide"), "empty %v", empty)
	equal(t, f.data("get_cheatsheet", map[string]any{"course": "Algebra"})["guide"], nil)
	check(t, strings.Contains(f.refused("start_document_version", map[string]any{"course": "Algebra",
		"kind": "guide", "source_text": "x"}), "no course"), "a course")
	check(t, strings.Contains(f.refused("start_document_version", map[string]any{"kind": "guide",
		"source_text": "x", "pdf_file": "x.pdf"}), "no PDF"), "no PDF")
	check(t, strings.Contains(f.refused("start_document_version", map[string]any{"kind": "book"}),
		"needs its course"), "a book needs a course")
	started := f.data("start_document_version", map[string]any{"kind": "guide", "source_text": "# Rules\n- Russian"})
	equal(t, []any{started["missing"], started["course"]}, []any{map[string]any{}, ""})
	f.data("publish_document_version", map[string]any{"draft_id": started["draft_id"]})

	guide := f.data("get_guide", nil)
	equal(t, []any{guide["version"], obj(guide["guide"])["text"]}, []any{1.0, "# Rules\n- Russian"})
	sheet := f.data("get_cheatsheet", map[string]any{"course": "Algebra"})
	check(t, sheet["cheatsheet"] == nil && obj(sheet["guide"])["text"] == "# Rules\n- Russian", "with %v", sheet)
	document := f.data("get_document", map[string]any{"kind": "guide"})
	equal(t, []any{document["kind"], document["title"]}, []any{"guide", "Knowledge base guide"})
	for _, course := range must(lib.Courses()) {
		check(t, course.Course != "", "the guide is no course")
	}

	again := f.data("start_document_version", map[string]any{"kind": "guide", "source_text": "# Rules v2"})
	f.data("publish_document_version", map[string]any{"draft_id": again["draft_id"]})
	equal(t, obj(f.data("get_guide", nil)["guide"])["text"], "# Rules v2")
	f.undoLast()
	equal(t, obj(f.data("get_guide", nil)["guide"])["text"], "# Rules\n- Russian")
}

// A draft's PDF is built by the TeX builder of TEX_URL: TeX's errors come back, a fixed source builds, the PDF is
// the draft's.
func TestLibraryBuildOnServer(t *testing.T) {
	builds := 0
	tex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		builds++
		source := string(must(io.ReadAll(r.Body)))
		if strings.Contains(source, "BROKEN") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, "! Undefined control sequence.\nl.3 BROKEN")
			return
		}
		io.WriteString(w, "%PDF-1.7 built from "+source)
	}))
	defer tex.Close()

	plain, _ := libraryFixture(t)
	draft := plain.data("start_document_version", map[string]any{"course": "Algebra", "kind": "book",
		"source_text": "x"})
	check(t, strings.Contains(plain.refused("build_document_version", map[string]any{"draft_id": draft["draft_id"]}),
		"no TeX builder"), "no builder")

	f, _ := libraryFixtureWith(t, func(f *fixture) { f.services.Env.TexURL = tex.URL })
	sheet := f.data("start_document_version", map[string]any{"course": "Algebra", "kind": "cheatsheet",
		"source_text": "# x"})
	check(t, strings.Contains(f.refused("build_document_version", map[string]any{"draft_id": sheet["draft_id"]}),
		"no PDF"), "a cheat sheet")
	empty := f.data("start_document_version", map[string]any{"course": "Algebra", "kind": "book"})
	check(t, strings.Contains(f.refused("build_document_version", map[string]any{"draft_id": empty["draft_id"]}),
		"not uploaded"), "no source")
	equal(t, builds, 0)

	failed := f.data("build_document_version", map[string]any{"draft_id": empty["draft_id"],
		"source_text": "BROKEN"})
	check(t, failed["built"] == false && strings.Contains(failed["errors"].(string), "Undefined control"),
		"failed %v", failed)
	check(t, strings.Contains(f.refused("publish_document_version", map[string]any{"draft_id": empty["draft_id"]}),
		"PDF"), "no PDF yet")
	built := f.data("build_document_version", map[string]any{"draft_id": empty["draft_id"],
		"source_text": `\section{Rings}`})
	equal(t, []any{built["built"], built["next"]}, []any{true, "call publish_document_version with draft_id"})
	published := f.data("publish_document_version", map[string]any{"draft_id": empty["draft_id"]})
	_, pdf := f.transfer("GET", published["pdf_url"].(string), nil)
	equal(t, string(pdf), "%PDF-1.7 built from \\section{Rings}")
	equal(t, builds, 2)
	check(t, strings.Contains(f.refused("build_document_version", map[string]any{"draft_id": empty["draft_id"]}),
		"published already"), "after publishing")
}

func TestLibraryUndoSortRemoveUpdate(t *testing.T) {
	f, lib := libraryFixture(t)
	id := func(data map[string]any) int64 { return int64(data["id"].(float64)) }
	get := func(n int64) *library.Material { return must(lib.Get(n, false, false)) }
	a := id(f.data("add_material", map[string]any{"text": "Notes on rings", "title": "Rings"}))
	b := id(f.data("add_material", map[string]any{"url": "https://example.org/board", "class_uid": "calc-2"}))
	equal(t, []any{get(b).Kind, *get(b).Course}, []any{"link", "Calculus"})

	f.data("sort_material", map[string]any{"ids": []any{a, b}, "course": "Algebra"})
	check(t, *get(a).Course == "Algebra" && *get(b).Course == "Algebra" && get(b).ClassUID == nil, "sorted")
	f.undoLast()
	check(t, get(a).Course == nil && *get(b).ClassUID == "calc-2", "back")

	f.data("sort_material", map[string]any{"ids": []any{b}, "course": nil})
	equal(t, must(lib.InboxCount()), int64(2))
	f.undoLast()
	equal(t, *get(b).ClassUID, "calc-2")

	refusal := f.refused("sort_material", map[string]any{"ids": []any{a, b}, "course": "Algebra",
		"class_uid": "calc-1"})
	check(t, strings.Contains(refusal, "is of Calculus") && get(a).Course == nil, "refused: %s", refusal)

	changed := f.data("update_material", map[string]any{"id": a, "title": "Ideals", "status": "processed"})
	equal(t, obj(changed["change"])["summary"], "Changed «Rings»: title → Ideals, status → processed")
	f.undoLast()
	equal(t, []any{get(a).Title, get(a).Status}, []any{"Rings", "new"})

	tr := id(f.data("add_material", map[string]any{"text": "words", "kind": "transcript", "parent": b}))
	removed := f.data("remove_material", map[string]any{"id": b})
	equal(t, removed["with"], []any{float64(tr)})
	check(t, get(b) == nil && get(tr) == nil, "removed")
	f.undoLast()
	check(t, get(b) != nil && get(tr) != nil, "back")

	many := list(f.data("remove_material", map[string]any{"ids": []any{a, b, tr}})["removed"])
	equal(t, []any{obj(many[0])["removed"], obj(many[1])["removed"], obj(many[1])["with"]},
		[]any{float64(a), float64(b), []any{float64(tr)}})
	check(t, get(a) == nil && get(b) == nil && get(tr) == nil, "all removed")
	f.undoLast()
	check(t, get(a) != nil && get(b) != nil && get(tr) != nil, "all back")

	draft := f.data("add_material", map[string]any{"file_name": "rec.mp3"})
	gone := f.data("remove_material", map[string]any{"id": draft["id"]})
	check(t, strings.HasPrefix(obj(gone["change"])["undo"].(string), "cannot be undone"), "no undo")
	status, _ := f.transfer("PUT", draft["upload_url"].(string), []byte("late"))
	equal(t, status, 400)
}

func TestLibraryRefusals(t *testing.T) {
	f, _ := libraryFixture(t)
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"add_material", map[string]any{"text": "x", "colour": "red"}, "unknown"},
		{"add_material", map[string]any{"title": "nothing"}, "give text"},
		{"add_material", map[string]any{"file_name": "a.pdf", "text": "x"}, "not a file with the others"},
		{"add_material", map[string]any{"text": "x", "kind": "poem"}, "one of"},
		{"add_material", map[string]any{"text": "x", "class_uid": "nope"}, "no class nope"},
		{"get_material", map[string]any{"id": "1"}, "expected integer"},
		{"get_material", map[string]any{"id": 99}, "no item 99"},
		{"sort_material", map[string]any{"ids": []any{}, "course": "Algebra"}, "ids: 1 to"},
		{"sort_material", map[string]any{"ids": []any{99}}, "give course"},
		{"remove_material", map[string]any{}, "give id"},
		{"remove_material", map[string]any{"id": 1, "ids": []any{1}}, "give id"},
		{"remove_material", map[string]any{"ids": []any{99}}, "no item 99"},
		{"start_document_version", map[string]any{"course": "Calculus", "kind": "lecture"}, "needs class_uid"},
		{"start_document_version", map[string]any{"course": "Calculus", "kind": "essay"}, "one of"},
		{"publish_document_version", map[string]any{}, "required"},
		{"get_document", map[string]any{"course": "Calculus"}, "give doc_id"},
		{"list_materials", map[string]any{"inbox": true, "course": "Algebra"}, "inbox or a course"},
	} {
		text := f.refused(c.tool, c.args)
		check(t, strings.Contains(text, c.want), "%s %v: %s", c.tool, c.args, text)
	}
}
