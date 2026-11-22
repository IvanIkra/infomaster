package links

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const dropbox = "https://www.dropbox.com/scl/fi/abc123/Sheet-3.pdf?rlkey=k9&dl=0"

var sheet3 = []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")

func TestDocumentLinks(t *testing.T) {
	text := "Слайды и ДЗ: " + dropbox + ". Задание в Classroom: https://classroom.google.com/c/XYZ/a/1 и " +
		"https://accounts.google.com/AccountChooser?continue=x, папка https://disk.yandex.ru/d/AbCdEf, " +
		"https://drive.google.com/file/d/1AbCdEfGhIjK/view?usp=sharing, (https://example.org/hw/list3.pdf), " +
		"https://example.org/page"
	want := []string{dropbox, "https://disk.yandex.ru/d/AbCdEf",
		"https://drive.google.com/file/d/1AbCdEfGhIjK/view?usp=sharing"} // three at most
	if got := DocumentLinks(text); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if got := DocumentLinks("https://example.org/hw/list3.pdf») и всё"); !reflect.DeepEqual(got,
		[]string{"https://example.org/hw/list3.pdf"}) {
		t.Fatal(got)
	}
	if got := DocumentLinks("https://edu.hse.ru/mod/forum/discuss.php?d=1"); len(got) != 0 {
		t.Fatal(got)
	}
	for url, want := range map[string]string{
		dropbox: "https://dl.dropboxusercontent.com/scl/fi/abc123/Sheet-3.pdf?rlkey=k9&dl=1",
		"https://drive.google.com/file/d/1AbCdEfGhIjK/view":    "https://drive.google.com/uc?export=download&id=1AbCdEfGhIjK",
		"https://drive.google.com/open?id=1Ab%2FCd&x=1":        "https://drive.google.com/uc?export=download&id=1Ab/Cd",
		"https://docs.google.com/document/d/1AbCdEfGhIjK/edit": "https://docs.google.com/document/d/1AbCdEfGhIjK/export?format=pdf",
		"https://www.dropbox.com/s/q7/A%20b.pdf?x=a+b&dl=0":    "https://dl.dropboxusercontent.com/s/q7/A%20b.pdf?x=a+b&dl=1",
	} {
		if got := Direct(url); got != want {
			t.Errorf("%s: %s", url, got)
		}
	}
}

func TestSaveTakesOnlyAPDF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing.pdf":
			http.NotFound(w, r)
			return
		case "/broken.pdf":
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''%D0%9B%D0%B8%D1%81%D1%82%D0%BE%D0%BA%203.pdf")
		if r.URL.Path == "/page.pdf" {
			w.Write([]byte("<html>sign in</html>"))
			return
		}
		w.Write(sheet3)
	}))
	defer server.Close()
	ctx, client := context.Background(), server.Client()
	path := filepath.Join(t.TempDir(), "x.part")
	if name, err := save(ctx, client, server.URL+"/sheet.pdf", path, 1e6); err != nil || name != "Листок 3.pdf" {
		t.Fatal(name, err)
	}
	if data, _ := os.ReadFile(path); !bytes.Equal(data, sheet3) {
		t.Fatal("the file differs")
	}
	for _, url := range []string{"/page.pdf", "/missing.pdf"} {
		if name, err := save(ctx, client, server.URL+url, path, 1e6); err != nil || name != "" {
			t.Fatal(url, name, err)
		}
	}
	if name, err := save(ctx, client, server.URL+"/sheet.pdf", path, 20); err != nil || name != "" { // too big
		t.Fatal(name, err)
	}
	var linkErr *LinkError
	if _, err := save(ctx, client, server.URL+"/broken.pdf", path, 1e6); !errors.As(err, &linkErr) {
		t.Fatal(err)
	}
}

func TestFileName(t *testing.T) {
	for _, c := range [][3]string{
		{`attachment; filename="hw 1.PDF"`, "https://x.org/a", "hw 1.PDF"},
		{"", "https://x.org/files/%D0%94%D0%973", "ДЗ3.pdf"},
		{"", "https://x.org/", "document.pdf"},
	} {
		if got := FileName(c[0], c[1]); got != c[2] {
			t.Errorf("%q: %q", c, got)
		}
	}
}

// D79: a link into the service's own network is no sheet, and is not tried again.
func TestALocalServerIsNeverReached(t *testing.T) {
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Write([]byte("%PDF-1.4 secret"))
	}))
	defer server.Close()
	name, err := Fetch(context.Background(), server.URL+"/notes.pdf", filepath.Join(t.TempDir(), "f.pdf"), 1e6)
	if err != nil || name != "" || len(asked) != 0 {
		t.Fatal(name, err, asked)
	}
}

func TestURLHelpers(t *testing.T) {
	u := SplitURL("HTTPS://User@WWW.Example.org:8080/a%20b/c?x=1&y=#frag")
	if u.Scheme != "https" || u.Netloc != "User@WWW.Example.org:8080" || u.Path != "/a%20b/c" ||
		u.Query != "x=1&y=" || u.Fragment != "frag" || u.Host() != "user@www.example.org:8080" {
		t.Fatalf("%+v", u)
	}
	if got := ParseQSL("a=1&&b&c=&d=x+y%21", true); !reflect.DeepEqual(got,
		[]Pair{{"a", "1"}, {"b", ""}, {"c", ""}, {"d", "x y!"}}) {
		t.Fatal(got)
	}
	if got := ParseQSL("a=1&b&c=", false); !reflect.DeepEqual(got, []Pair{{"a", "1"}}) {
		t.Fatal(got)
	}
	if got := URLEncode([]Pair{{"k", "a b/ё&"}}); got != "k=a+b%2F%D1%91%26" {
		t.Fatal(got)
	}
}
