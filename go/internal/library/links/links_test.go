package links

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func ptr[T any](v T) *T { return &v }

func TestTokenVectors(t *testing.T) {
	l := &Links{key: make([]byte, 32), Seconds: func() float64 { return 1.7e9 }}
	if got := l.Sign(materialPayload{"get", 12, 1800000000}); got !=
		"eyJhIjoiZ2V0IiwibSI6MTIsImUiOjE4MDAwMDAwMDB9.Iqbg-WwHmA33BQCiaj0wyes4" {
		t.Fatal(got)
	}
	token := l.Sign(versionPayload{"get", 3, "pdf", 1800000000})
	if token != "eyJhIjoiZ2V0IiwidiI6MywicCI6InBkZiIsImUiOjE4MDAwMDAwMDB9.GxUts41Sx-t3OkqFZJzyzhAv" {
		t.Fatal(token)
	}
	payload := l.Check(token)
	if payload == nil || payload["v"] != 3.0 || payload["p"] != "pdf" {
		t.Fatal(payload)
	}
	if l.Check(token[:len(token)-1]+"A") != nil || l.Check("x"+token) != nil || l.Check("") != nil {
		t.Fatal("a wrong token")
	}
	l.Seconds = func() float64 { return 1.8e9 }
	if l.Check(token) != nil {
		t.Fatal("expired")
	}
}

func TestNamesAndTypes(t *testing.T) {
	if got := FileName(&library.Version{Title: "Calc: lecture/1", VersionRow: library.VersionRow{Version: ptr(int64(2))}},
		"pdf"); got != "Calc_ lecture_1 v2.pdf" {
		t.Fatal(got)
	}
	if got := FileName(&library.Version{Title: "Cheat", Kind: "cheatsheet"}, "source"); got != "Cheat.md" {
		t.Fatal(got)
	}
	if got := FileName(&library.Version{Title: "Лекция 1", Kind: "lecture"}, "source"); got != "Лекция 1.tex" {
		t.Fatal(got)
	}
	want := `inline; filename="________ _1_.pdf"; filename*=UTF-8''%D0%9A%D0%BE%D0%BD%D1%81%D0%BF%D0%B5%D0%BA%D1%82%20%221%22.pdf`
	if got := Disposition(`Конспект "1".pdf`, "application/pdf"); got != want {
		t.Fatal(got)
	}
	if ServedType("Text/HTML; charset=x") != "text/plain; charset=utf-8" || ServedType("image/svg+xml") !=
		"application/octet-stream" || ServedType("image/png") != "image/png" || ServedType("") != "application/octet-stream" {
		t.Fatal("served type")
	}
}

type fixture struct {
	lib    *library.Library
	links  *Links
	server *httptest.Server
}

func setup(t *testing.T) *fixture {
	dir := t.TempDir()
	s := must(store.Open(filepath.Join(dir, "x.db")))
	t.Cleanup(func() { s.Close() })
	lib := must(library.New(s, dir))
	lib.Now = func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, clock.TZ) }
	links := must(New(s, lib, "https://example.test/"))
	var stored string
	if found, _ := s.GetJSON(SecretKey, &stored); !found || len(stored) != 64 {
		t.Fatal(stored)
	}
	if again := must(New(s, lib, "https://example.test")); string(again.key) != string(links.key) {
		t.Fatal("the key is kept")
	}
	mux := http.NewServeMux()
	links.Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &fixture{lib, links, server}
}

func (f *fixture) do(method, link string, body string) *http.Response {
	request := must(http.NewRequest(method, f.server.URL+strings.TrimPrefix(link, "https://example.test"),
		strings.NewReader(body)))
	return must(http.DefaultClient.Do(request))
}

func read(r *http.Response) string {
	defer r.Body.Close()
	return string(must(io.ReadAll(r.Body)))
}

func TestLinks(t *testing.T) {
	f := setup(t)
	item := must(f.lib.AddStream(strings.NewReader("%PDF-slides"), "Слайды 1.pdf", "application/pdf", "",
		library.Item{Source: "bot"}))
	link := f.links.Material(must(f.lib.Get(item, false, false)), GetTTL)
	if !strings.HasSuffix(link, "/%D0%A1%D0%BB%D0%B0%D0%B9%D0%B4%D1%8B%201.pdf") {
		t.Fatal(link)
	}
	response := f.do("GET", link, "")
	if response.StatusCode != 200 || read(response) != "%PDF-slides" ||
		!strings.Contains(response.Header.Get("Content-Disposition"), "inline") {
		t.Fatal(response.Status)
	}
	if response := f.do("GET", strings.Replace(link, "/library/", "/library/x", 1), ""); response.StatusCode != 404 ||
		read(response) != "the link is wrong or expired" {
		t.Fatal(response.Status)
	}
	if f.do("PUT", link[:strings.LastIndex(link, "/")], "x").StatusCode != 404 { // a download link uploads nothing
		t.Fatal("upload by a download link")
	}
	ranged := must(http.NewRequest("GET", f.server.URL+strings.TrimPrefix(link, "https://example.test"), nil))
	ranged.Header.Set("Range", "bytes=0-3")
	if response := must(http.DefaultClient.Do(ranged)); response.StatusCode != 206 || read(response) != "%PDF" {
		t.Fatal(response.Status)
	}

	seconds := f.links.Seconds
	f.links.Seconds = func() float64 { return 1e10 } // later than every link
	if f.do("GET", link, "").StatusCode != 404 {
		t.Fatal("expired")
	}
	f.links.Seconds = seconds

	draft := must(f.lib.Add("audio", library.Item{Source: "connector", Status: "uploading",
		File: library.File{Name: "rec.m4a"}}))
	upload := f.links.MaterialUpload(draft, PutTTL)
	response = f.do("PUT", upload, "audio-bytes")
	var answer map[string]any
	body := read(response)
	if response.StatusCode != 200 || json.Unmarshal([]byte(body), &answer) != nil || answer["size"] != 11.0 {
		t.Fatal(response.Status, body)
	}
	if !strings.HasPrefix(body, `{"material": `) || response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatal(body)
	}
	if response := f.do("PUT", upload, "again"); response.StatusCode != 400 ||
		read(response) != "this upload is used already or gone" {
		t.Fatal(response.Status)
	}

	version := must(f.lib.Draft("Calculus", "book", "", "", "", nil))
	for _, part := range [][2]string{{"source", `\documentclass{book}`}, {"pdf", "%PDF-book"}} {
		response := f.do("PUT", f.links.VersionUpload(version.ID, part[0], PutTTL), part[1])
		if body := read(response); response.StatusCode != 200 ||
			body != `{"version_draft": `+fmt.Sprint(version.ID)+`, "part": "`+part[0]+`", "uploaded": true}` {
			t.Fatal(body)
		}
	}
	published := must(f.lib.Publish(version.ID))
	response = f.do("GET", f.links.Version(published, "pdf", GetTTL), "")
	if read(response) != "%PDF-book" || !strings.Contains(response.Header.Get("Content-Disposition"), "Calculus v1.pdf") {
		t.Fatal(response.Header)
	}
	if response := f.do("GET", f.links.Version(published, "source", GetTTL), ""); response.Header.Get("Content-Type") !=
		"text/plain; charset=utf-8" || read(response) != `\documentclass{book}` {
		t.Fatal(response.Header)
	}
}

func TestKeptPDF(t *testing.T) {
	f := setup(t)
	if f.links.KeptPDF("abc", "", GetTTL) != "" {
		t.Fatal("no sheets")
	}
	path := filepath.Join(t.TempDir(), "sheet.pdf")
	if err := os.WriteFile(path, []byte("%PDF-sheet"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.links.Kept = func(sha string) string {
		if sha == "abc" {
			return path
		}
		return ""
	}
	link := f.links.KeptPDF("abc", "", GetTTL)
	response := f.do("GET", link, "")
	if !strings.HasSuffix(link, "/sheet.pdf") || read(response) != "%PDF-sheet" ||
		response.Header.Get("Content-Security-Policy") != "" {
		t.Fatal(link, response.Header)
	}
}

func TestAChatMembersFileNeverRunsOnTheAppsOrigin(t *testing.T) {
	// A sender picks a file's type: svg is downloaded, any text (html too) is plain text; every file is sniff-free and
	// sandboxed (a PDF aside: a viewer refuses a sandbox).
	f := setup(t)
	cases := map[string][2]string{
		"text/html":             {"text/plain; charset=utf-8", "inline"},
		"image/svg+xml":         {"application/octet-stream", "attachment"},
		"application/xhtml+xml": {"application/octet-stream", "attachment"},
		"text/markdown":         {"text/plain; charset=utf-8", "inline"},
		"image/png":             {"image/png", "inline"},
		"application/pdf":       {"application/pdf", "inline"},
	}
	for sent, want := range cases {
		item := must(f.lib.AddStream(strings.NewReader("<script>x</script>"), "slides", sent, "",
			library.Item{Source: "chat"}))
		response := f.do("GET", f.links.Material(must(f.lib.Get(item, false, false)), GetTTL), "")
		read(response)
		if response.Header.Get("Content-Type") != want[0] ||
			!strings.HasPrefix(response.Header.Get("Content-Disposition"), want[1]) {
			t.Fatal(sent, response.Header)
		}
		if response.Header.Get("X-Content-Type-Options") != "nosniff" ||
			strings.Contains(response.Header.Get("Content-Security-Policy"), "sandbox") != (sent != "application/pdf") {
			t.Fatal(sent, response.Header)
		}
	}
}
