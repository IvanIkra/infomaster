package library

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, clock.TZ)

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

func addClass(s *store.Store, uid, subject, start, kind string) {
	must(s.DB.Exec("insert into classes(uid, subject, start, end, kind, room, seen_at) values(?, ?, ?, ?, ?, '', ?)",
		uid, subject, start, start[:11]+"23:00:00", kind, start))
}

func setup(t *testing.T) (*Library, string) {
	t.Helper()
	dir := t.TempDir()
	s := must(store.Open(filepath.Join(dir, "x.db")))
	t.Cleanup(func() { s.Close() })
	addClass(s, "calc-1", "Calculus", "2026-11-18T09:30:00", "Лекция")
	addClass(s, "calc-2", "Calculus", "2026-11-19T11:10:00", "Семинар")
	addClass(s, "calc-3", "Calculus", "2026-11-25T09:30:00", "Лекция") // not yet
	addClass(s, "alg-1", "Algebra", "2026-11-20T11:10:00", "Lecture")
	lib := must(New(s, dir))
	lib.Now = func() time.Time { return now }
	lib.SameCourse = func(a, b string) bool {
		return a != "" && b != "" && (pytext.Casefold(a) == pytext.Casefold(b) ||
			(a == "Матан" && b == "Calculus") || (a == "Calculus" && b == "Матан"))
	}
	return lib, dir
}

func at(text string) time.Time { return must(clock.ParseLocal(text)) }

func TestHelpers(t *testing.T) {
	if GuessKind("image/jpeg", "") != "photo" || GuessKind("audio/mp4", "") != "audio" ||
		GuessKind("", "Lecture 3.PPTX") != "slides" || GuessKind("application/pdf", "") != "file" {
		t.Fatal("guess kind")
	}
	if SafeName("../a/b.pdf", "x") != "_a_b.pdf" || SafeName("  ", "x") != "x" {
		t.Fatal(SafeName("../a/b.pdf", "x"))
	}
	if long := SafeName(strings.Repeat("я", 130)+".pdf", "x"); long != strings.Repeat("я", 110)+".pdf" {
		t.Fatal(long)
	}
	if ClassKind("Лекция") != "lecture" || ClassKind("Практическое занятие") != "seminar" ||
		ClassKind("Рубежный контроль") != "" {
		t.Fatal("class kind")
	}
	for name, want := range map[string]string{"a.tar.GZ": ".GZ", ".bashrc": "", "a.": "", "x/y.jpg": ".jpg", "": ""} {
		if got := suffix(name); got != want {
			t.Errorf("%q: %q", name, got)
		}
	}
}

func TestItemsAndClasses(t *testing.T) {
	lib, _ := setup(t)
	// A photo of the board sent during the seminar belongs to it; the course's other name is the timetable's.
	photo := must(lib.AddStream(strings.NewReader("jpegbytes"), "board.jpg", "image/jpeg", "", Item{Course: "Матан",
		Source: "chat", ChatID: ptr(int64(-5)), MsgID: ptr(int64(7)), At: at("2026-11-19T12:00:00")}))
	row := must(lib.Get(photo, false, false))
	if *row.Course != "Calculus" || row.Kind != "photo" || *row.Size != 9 {
		t.Fatal(row)
	}
	if row.ClassUID != nil { // no class was asked for: FindClass is the caller's
		t.Fatal("class")
	}
	found := must(lib.FindClass("Матан", at("2026-11-19T12:00:00"), false))
	if found.UID != "calc-2" || found.Kind != "seminar" {
		t.Fatal(found)
	}
	if must(lib.FindClass("Calculus", at("2026-11-18T09:15:00"), false)).UID != "calc-1" {
		t.Fatal("calc-1")
	}
	if must(lib.FindClass("Calculus", at("2026-11-10T00:00:00"), false)) != nil ||
		must(lib.FindClass("History", now, false)) != nil {
		t.Fatal("none")
	}
	if data := must(os.ReadFile(lib.Path(row))); string(data) != "jpegbytes" {
		t.Fatal(string(data))
	}
	if must(lib.FromOrigin("chat", -5, 7, "")).ID != photo {
		t.Fatal("origin")
	}

	note := must(lib.Add("text", Item{Source: "miniapp", Text: ptr("Limits, ε-δ"), ClassUID: "calc-1"}))
	if got := must(lib.Get(note, false, false)); *got.Course != "Calculus" || *got.Day != "2026-11-18" {
		t.Fatal(got)
	}
	if _, err := lib.Add("text", Item{Source: "miniapp", Text: ptr("x"), Course: "Algebra", ClassUID: "calc-1"}); err == nil {
		t.Fatal("class of another course")
	}
	inbox := must(lib.Add("link", Item{Source: "bot", URL: ptr("https://example.org/rec")}))
	if must(lib.InboxCount()) != 1 || must(lib.Items(Query{Inbox: true}))[0].ID != inbox {
		t.Fatal("inbox")
	}

	classes := must(lib.Classes("Calculus", time.Time{}))
	if len(classes) != 2 || classes[0].UID != "calc-2" || classes[1].UID != "calc-1" { // started ones, the last first
		t.Fatal(classes)
	}
	if classes[1].Items != 1 || classes[1].Kind != "lecture" {
		t.Fatal(classes[1])
	}

	old := must(lib.Update(inbox, map[string]any{"course": "Algebra", "class_uid": "alg-1"}))
	if len(old) != 2 || old["course"] != nil || old["class_uid"] != nil {
		t.Fatal(old)
	}
	if must(lib.InboxCount()) != 0 || *must(lib.Get(inbox, false, false)).Day != "2026-11-20" {
		t.Fatal("sorted")
	}
	must(lib.Update(inbox, old))
	if must(lib.InboxCount()) != 1 {
		t.Fatal("back in the inbox")
	}
	courses := map[string]CourseInfo{}
	for _, c := range must(lib.Courses()) {
		courses[c.Course] = c
	}
	if courses["Calculus"].Items != 2 || courses["Algebra"].Items != 0 {
		t.Fatal(courses)
	}
	if _, err := lib.Update(inbox, map[string]any{"path": "x"}); err == nil || err.Error() != "cannot change path" {
		t.Fatal(err)
	}

	// The same bytes again are kept once on disk.
	again := must(lib.AddStream(strings.NewReader("jpegbytes"), "copy.JPG", "image/jpeg", "", Item{Source: "bot"}))
	if *must(lib.Get(again, false, false)).Path != *row.Path {
		t.Fatal("kept twice")
	}
}

func TestRemoveRestoreAndPrune(t *testing.T) {
	lib, dir := setup(t)
	audio := must(lib.AddStream(strings.NewReader("m4a"), "rec.m4a", "audio/mp4", "", Item{Source: "miniapp",
		ClassUID: "calc-1"}))
	transcript := must(lib.Add("transcript", Item{Source: "connector", Text: ptr("today: limits"), Parent: &audio}))
	if children := must(lib.Children(audio)); len(children) != 1 || children[0].ID != transcript {
		t.Fatal(children)
	}
	noErr(lib.Remove(audio))
	if must(lib.Get(audio, false, false)) != nil || must(lib.Get(transcript, false, false)) != nil {
		t.Fatal("removed")
	}
	noErr(lib.Restore(audio))
	if must(lib.Get(transcript, false, false)) == nil {
		t.Fatal("restored")
	}
	noErr(lib.Remove(audio))
	lib.Now = func() time.Time { return now.AddDate(0, 0, 31) }
	if n := must(lib.Prune()); n != 2 {
		t.Fatal(n)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "library", "raw", "*", "*.m4a"))
	if len(matches) != 0 {
		t.Fatal(matches)
	}
	if err := lib.Restore(audio); err == nil {
		t.Fatal("pruned")
	}
}

func TestDrafts(t *testing.T) {
	lib, _ := setup(t)
	draft := must(lib.Add("audio", Item{Source: "connector", ClassUID: "calc-1", Status: "uploading",
		File: File{Name: "rec.m4a", MediaType: "audio/mp4"}}))
	if must(lib.Get(draft, false, false)) != nil || len(must(lib.Items(Query{}))) != 0 {
		t.Fatal("a draft is listed")
	}
	row := must(lib.Fill(draft, strings.NewReader("abc")))
	if row.Status != "new" || *row.Size != 3 {
		t.Fatal(row)
	}
	if _, err := lib.Fill(draft, strings.NewReader("x")); err == nil {
		t.Fatal("filled twice")
	}
	stale := must(lib.Add("file", Item{Source: "connector", Status: "uploading"}))
	lib.Now = func() time.Time { return now.AddDate(0, 0, 2) }
	must(lib.Prune())
	if must(lib.Get(stale, true, false)) != nil || must(lib.Get(draft, false, false)) == nil {
		t.Fatal("prune")
	}
}

func TestKnowledge(t *testing.T) {
	lib, _ := setup(t)
	photo := must(lib.Add("text", Item{Source: "miniapp", Text: ptr("board"), ClassUID: "calc-1"}))
	for _, bad := range [][2]string{{"Calculus", ""}, {"Algebra", "calc-1"}} {
		if _, err := lib.Draft(bad[0], "lecture", bad[1], "", "", nil); err == nil {
			t.Fatal(bad)
		}
	}
	if _, err := lib.Draft("Calculus", "book", "calc-1", "", "", nil); err == nil {
		t.Fatal("a book of a class")
	}
	draft := must(lib.Draft("Матан", "lecture", "calc-1", "", "first", []int64{photo}))
	if draft.Version != nil || draft.Course != "Calculus" || draft.Materials != "[1]" {
		t.Fatal(draft)
	}
	if _, err := lib.Publish(draft.ID); err == nil {
		t.Fatal("no source")
	}
	must(lib.PutText(draft.ID, "source", `\section{Limits}`))
	if _, err := lib.Publish(draft.ID); err == nil {
		t.Fatal("no PDF yet")
	}
	if _, err := lib.PutText(draft.ID, "pdf", "not a pdf"); err == nil {
		t.Fatal("no PDF")
	}
	must(lib.Put(draft.ID, "pdf", bytes.NewReader([]byte("%PDF-1.7 ..."))))
	first := must(lib.Publish(draft.ID))
	if *first.Version != 1 || must(lib.Get(photo, false, false)).Status != "processed" {
		t.Fatal(first)
	}
	if *must(lib.ReadSource(first)) != `\section{Limits}` {
		t.Fatal("source")
	}

	second := must(lib.Draft("Calculus", "lecture", "calc-1", "Limits", "", nil))
	must(lib.PutText(second.ID, "source", "v2"))
	must(lib.Put(second.ID, "pdf", strings.NewReader("%PDF-2")))
	must(lib.Publish(second.ID))
	doc := must(lib.FindDocument("Calculus", "lecture", "calc-1"))
	if len(doc.Versions) != 2 || *doc.Versions[0].Version != 2 || *doc.Versions[1].Version != 1 || doc.Title != "Limits" {
		t.Fatal(doc)
	}
	if err := lib.Unpublish(first.ID); err == nil {
		t.Fatal("a newer one came after it")
	}
	noErr(lib.Unpublish(second.ID))
	if doc := must(lib.FindDocument("Calculus", "lecture", "calc-1")); len(doc.Versions) != 1 {
		t.Fatal(doc)
	}

	cheat := must(lib.Draft("Calculus", "cheatsheet", "", "", "", nil))
	must(lib.PutText(cheat.ID, "source", "# Notation\n- ε for epsilon"))
	if *must(lib.Publish(cheat.ID)).Version != 1 { // no PDF needed
		t.Fatal("cheat sheet")
	}
	if !strings.HasSuffix(lib.PartPath(must(lib.GetVersion(cheat.ID)), "source"), ".md") {
		t.Fatal("md")
	}
	var kinds []string
	for _, d := range must(lib.Documents(ptr("Calculus"))) {
		kinds = append(kinds, d.Kind)
	}
	if !slices.Equal(kinds, []string{"cheatsheet", "lecture"}) {
		t.Fatal(kinds)
	}
	if usage := must(lib.Usage()); usage.Documents != 2 || usage.FreeBytes == nil || usage.Bytes == 0 {
		t.Fatal(usage)
	}
}

func TestARenamedCourseIsOneCourse(t *testing.T) {
	// RUZ's feed went from Russian to English: the old classes keep the old name, and both are one course.
	lib, _ := setup(t)
	addClass(lib.Store, "calc-0", "Математический анализ", "2026-10-07T09:30:00", "Семинар")
	names := map[string]string{"Математический анализ": "Calculus", "Calculus": "Calculus", "Матан": "Calculus"}
	of := func(name string) string {
		if n, ok := names[name]; ok {
			return n
		}
		return name
	}
	lib.SameCourse = func(a, b string) bool { return a != "" && b != "" && of(a) == of(b) }
	if must(lib.Course("Математический анализ")) != "Calculus" || must(lib.Course("Матан")) != "Calculus" {
		t.Fatal("course")
	}
	all := must(lib.Names("Матан"))
	slices.Sort(all)
	if !slices.Equal(all, []string{"Calculus", "Математический анализ"}) {
		t.Fatal(all)
	}
	item := must(lib.Add("text", Item{Source: "miniapp", Text: ptr("old notes"), Course: "Математический анализ",
		ClassUID: "calc-0"}))
	if *must(lib.Get(item, false, false)).Course != "Calculus" {
		t.Fatal("course of the item")
	}
	found := false
	for _, c := range must(lib.Classes("Calculus", time.Time{})) {
		found = found || c.UID == "calc-0"
	}
	if !found {
		t.Fatal("calc-0")
	}
	count := 0
	for _, c := range must(lib.Courses()) {
		if c.Course == "Calculus" {
			count++
		}
		if c.Course == "Математический анализ" {
			t.Fatal("the old name")
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
	if must(lib.FindClass("Матан", at("2026-10-07T10:00:00"), false)).UID != "calc-0" {
		t.Fatal("find class")
	}
	if must(lib.Draft("Математический анализ", "seminar", "calc-0", "", "", nil)).Course != "Calculus" {
		t.Fatal("draft")
	}
}

func TestASheetFindsItsSeminar(t *testing.T) {
	// A sheet sent the night before its seminar belongs to that seminar, not to the lecture before it.
	lib, _ := setup(t)
	addClass(lib.Store, "calc-lec", "Calculus", "2026-11-17T14:40:00", "Лекция")
	night := at("2026-11-19T00:25:00")
	if must(lib.FindClass("Calculus", night, true)).UID != "calc-2" {
		t.Fatal("seminar")
	}
	if must(lib.FindClass("Calculus", night, false)).UID != "calc-1" { // the lecture before it
		t.Fatal("lecture")
	}
	if must(lib.FindClass("Algebra", night, true)) != nil { // no seminar, no class started yet
		t.Fatal("algebra")
	}
}
