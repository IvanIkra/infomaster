package wiki

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const page = "https://wiki.example.org/Discrete_maths_26/27"

func drive(id string) string { return "https://drive.google.com/file/d/" + id + "/view?usp=sharing" }

const wikitext = `== About ==
Lecturer: [https://t.me/someone A. Lecturer]. [https://docs.google.com/spreadsheets/d/1AbCdEfGhIjK/edit Grades]

== Seminar sheets ==
* [S1 Sheet 1]
* [S2 Sheet 2]
<!--
* [S3 Sheet 3]
-->

== Homework ==
{| class="wikitable"
|-
! Task !! colspan="3" | Due in group
|-
| || 101 || 102 || 103
|-
| [H1 HW 1a ] ||  18.09 ||  || 24.09
|-
| [H2 HW 1b] || 04.10 || DUE || 08.10
|}

== Literature ==
* [https://example.org/books/logic.pdf A. Author. Logic.]
`

func text(due string, s3 bool) string {
	t := strings.NewReplacer("S1", drive("1Sheet1AAAAA"), "S2", drive("1Sheet2AAAAA"), "S3", drive("1Sheet3AAAAA"),
		"H1", drive("1Hw1aAAAAAAA"), "H2", drive("1Hw1bAAAAAAA"), "DUE", due).Replace(wikitext)
	if s3 {
		t = strings.NewReplacer("<!--\n", "", "-->\n", "").Replace(t)
	}
	return t
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func TestPageAPI(t *testing.T) {
	for url, want := range map[string][2]string{
		page: {"https://wiki.example.org/api.php", "Discrete maths 26/27"},
		"https://wiki.example.org/index.php?title=Calculus_101": {"https://wiki.example.org/api.php", "Calculus 101"},
		"https://wiki.example.org/w/index.php?title=X":          {"https://wiki.example.org/w/api.php", "X"},
		"http://wiki.example.org/wiki/%D0%90%D0%BB%D0%B3":       {"https://wiki.example.org/api.php", "Алг"},
	} {
		api, title, err := PageAPI(url)
		if err != nil || api != want[0] || title != want[1] {
			t.Errorf("%s: %s %s %v", url, api, title, err)
		}
	}
	if _, _, err := PageAPI("https://wiki.example.org/"); err == nil || err.Error() != "no page title in 'https://wiki.example.org/'" {
		t.Fatal(err)
	}
	if _, _, err := PageAPI("ftp://x/y"); err == nil {
		t.Fatal("ftp")
	}
}

func TestItems(t *testing.T) {
	found := Items(text("", false))
	var got [][2]string
	for _, i := range found {
		got = append(got, [2]string{i.Section, i.Name})
	}
	want := [][2]string{{"Seminar sheets", "Sheet 1"}, {"Seminar sheets", "Sheet 2"}, {"Homework", "HW 1a"},
		{"Homework", "HW 1b"}, {"Literature", "A. Author. Logic."}} // no Telegram, no spreadsheet, nothing commented out
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if found[2].Row != "101: 18.09 · 102: — · 103: 24.09" || found[0].Row != "" {
		t.Fatal(found[2].Row)
	}
}

func TestHeadingsAndTasks(t *testing.T) {
	for line, want := range map[string]string{"== A ==": "A", "====": "", "=== A ==": "= A", "======B======": "B",
		"== A ===": "A =", "==A==": "A"} {
		if got, ok := heading(line); !ok || got != want {
			t.Errorf("%q: %q %v", line, got, ok)
		}
	}
	for _, line := range []string{"= A =", "== A", "A =="} {
		if _, ok := heading(line); ok {
			t.Errorf("%q is a heading", line)
		}
	}
	for _, s := range []string{"ДЗ 3", "Мои дз", "HW 1a", "Листок 2", "контрольная"} {
		if !tasks.MatchString(s) {
			t.Errorf("%q is no task", s)
		}
	}
	for _, s := range []string{"дзюдо", "Литература", "show"} {
		if tasks.MatchString(s) {
			t.Errorf("%q is a task", s)
		}
	}
	if got := Clean("'''[https://x.example/a b]''' [[Page|Title]] <br/>  [[Other]]"); got != "b Title Other" {
		t.Fatal(got)
	}
}

type reader struct {
	revid int64
	text  string
	fail  error
}

func (r *reader) read(ctx context.Context, api, title string) (int64, string, string, error) {
	if r.fail != nil {
		return 0, "", "", r.fail
	}
	return r.revid, title, r.text, nil
}

type fixture struct {
	wiki   *Wiki
	reader *reader
	told   []i18n.Phrase
	now    time.Time
	store  *store.Store
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s := must(store.Open(filepath.Join(t.TempDir(), "x.db")))
	t.Cleanup(func() { s.Close() })
	general := must(settings.New(s))
	must(general.Set("wiki_pages", []any{page}))
	f := &fixture{reader: &reader{revid: 1, text: text("", false)}, store: s,
		now: time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}
	f.wiki = New(s, general, inbox.Open(s), func() time.Time { return f.now })
	f.wiki.Reader = f.reader.read
	f.wiki.Notify = func(p i18n.Phrase) { f.told = append(f.told, p) }
	return f
}

func (f *fixture) letters() map[int64]*inbox.Message {
	out := map[int64]*inbox.Message{}
	for _, m := range must(f.wiki.Inbox.Pending(inbox.Mailbox, "mail")) {
		out[m.MsgID] = m
	}
	return out
}

func TestNotices(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if n := must(f.wiki.Poll(ctx)); n != 2 { // the baseline: the current sheet and homework, not the literature
		t.Fatal(n)
	}
	first := f.letters()
	var next int64
	must(f.store.GetJSON(NextKey, &next))
	if len(first) != 2 || first[-1] == nil || first[-2] == nil || next != -3 {
		t.Fatal(first, next)
	}
	sheet, homework := first[-1], first[-2]
	if *sheet.SenderName != Sender || *sheet.Subject != "Wiki · Discrete maths 26/27: Sheet 2" ||
		!strings.Contains(sheet.Text, drive("1Sheet2AAAAA")) {
		t.Fatal(sheet)
	}
	if !strings.Contains(homework.Text, "Row of the table: 101: 04.10 · 102: — · 103: 08.10") {
		t.Fatal(homework.Text)
	}

	if n := must(f.wiki.Poll(ctx)); n != 0 { // the same revision: not read again
		t.Fatal(n)
	}
	f.reader.revid, f.reader.text = 2, text("29.11", true)
	if n := must(f.wiki.Poll(ctx)); n != 2 {
		t.Fatal(n)
	}
	news := map[string]*inbox.Message{}
	for id, m := range f.letters() {
		if id < -2 {
			news[*m.Subject] = m
		}
	}
	if !strings.HasPrefix(news["Wiki · Discrete maths 26/27: Sheet 3"].Text, "New on the course's wiki page") {
		t.Fatal(news)
	}
	changed := news["Wiki · Discrete maths 26/27: HW 1b"].Text
	if !strings.HasPrefix(changed, "Changed on") || !strings.Contains(changed, "102: 29.11") {
		t.Fatal(changed)
	}
	status := must(f.wiki.Status())
	got, _ := status.Pages.Get(page)
	if *got.Title != "Discrete maths 26/27" || got.Items != 6 || got.Error != nil || status.OKAt == nil {
		t.Fatalf("%+v", got)
	}
	var state string
	must(0, f.store.DB.QueryRow("select value from kv where key = ?", StateKey).Scan(&state))
	if !strings.HasPrefix(state, `{"`+page+`": {"revid": 2, "title": "Discrete maths 26/27", "items": {"`+
		drive("1Sheet1AAAAA")+`": "", `) || !strings.Contains(state, `": "101: 04.10 · 102: 29.11 · 103: 08.10"`) {
		t.Fatal(state)
	}
}

func TestAFailingPageIsToldAfterADay(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.reader.fail = context.DeadlineExceeded
	must(f.wiki.Poll(ctx))
	got, _ := must(f.wiki.Status()).Pages.Get(page)
	if len(f.told) != 0 || !strings.Contains(*got.Error, "TimeoutError") {
		t.Fatal(f.told, *got.Error)
	}
	f.now = f.now.Add(StaleAfter)
	must(f.wiki.Poll(ctx))
	if len(f.told) != 1 || !strings.Contains(f.told[0].En, "not read since 20.10 12:00") || !f.told[0].Alert {
		t.Fatal(f.told)
	}
	f.reader.fail = nil
	must(f.wiki.Poll(ctx))
	got, _ = must(f.wiki.Status()).Pages.Get(page)
	if len(f.told) != 2 || got.Error != nil || f.told[1].En != "The course wiki page "+page+" is read again." {
		t.Fatal(f.told)
	}
}

func TestTheWikisOwnFailureReadsPlainly(t *testing.T) {
	f := setup(t)
	f.reader.fail = &WikiDown{`Wikimedia\Rdbms\DBConnectionError`}
	if n := must(f.wiki.Poll(context.Background())); n != 0 {
		t.Fatal(n)
	}
	got, _ := must(f.wiki.Status()).Pages.Get(page)
	if *got.Error != `the wiki's server failed (Wikimedia\Rdbms\DBConnectionError)` {
		t.Fatal(*got.Error)
	}
}

func TestReadPage(t *testing.T) {
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RawQuery+" "+r.Header.Get("User-Agent"))
		switch r.URL.Query().Get("page") {
		case "Ok page":
			w.Write([]byte(`{"parse": {"title": "Ok page", "revid": 42, "wikitext": "== A ==\n[x y]"}}`))
		case "Down":
			w.Write([]byte(`{"error": {"code": "internal_api_error_Wikimedia\\Rdbms\\DBConnectionError", "info": "x"}}`))
		case "Missing":
			w.Write([]byte(`{"error": {"code": "missingtitle", "info": "The page you specified doesn't exist."}}`))
		default:
			http.Error(w, "no", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	client := NewClient
	NewClient = server.Client
	defer func() { NewClient = client }()
	ctx := context.Background()
	revid, title, text, err := ReadPage(ctx, server.URL+"/api.php", "Ok page")
	if err != nil || revid != 42 || title != "Ok page" || text != "== A ==\n[x y]" {
		t.Fatal(revid, title, text, err)
	}
	if asked[0] != "action=parse&page=Ok+page&prop=wikitext%7Crevid&format=json&formatversion=2&redirects=1 infomaster" {
		t.Fatal(asked[0])
	}
	var down *WikiDown
	if _, _, _, err := ReadPage(ctx, server.URL+"/api.php", "Down"); !errors.As(err, &down) ||
		down.Code != `Wikimedia\Rdbms\DBConnectionError` {
		t.Fatal(err)
	}
	if _, _, _, err := ReadPage(ctx, server.URL+"/api.php", "Missing"); err == nil || errorName(err) != "ValueError" ||
		err.Error() != "The page you specified doesn't exist." {
		t.Fatal(err)
	}
	if _, _, _, err := ReadPage(ctx, server.URL+"/api.php", "Other"); err == nil ||
		errorName(err) != "ClientResponseError" || !strings.HasPrefix(err.Error(), "503, message='Service Unavailable'") {
		t.Fatal(err)
	}
}
