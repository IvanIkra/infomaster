package grades

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	sheetURL = "https://docs.google.com/spreadsheets/d/abc123/edit#gid=0"
	// Dropbox share links (made up): a file since 2023, an older file link, the direct host.
	scl    = "https://www.dropbox.com/scl/fi/a1b2c3d4e5/DM%20grades%202026.xlsx?rlkey=k9x8y7&st=s1&dl=0"
	old    = "https://www.dropbox.com/s/q7w8e9r0/Calculus.xlsx?dl=0"
	direct = "https://dl.dropboxusercontent.com/scl/fi/z5y4x3/Algebra.xlsx?rlkey=r1"
)

// testClock is a settable now(); it starts at noon in Moscow.
type testClock struct{ moment time.Time }

func (c *testClock) now() time.Time { return c.moment }

func newClock() *testClock {
	return &testClock{time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}
}

func newSettings(t *testing.T, sheets ...map[string]any) *settings.Settings {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := settings.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("grades_student", "Иванов Иван"); err != nil {
		t.Fatal(err)
	}
	list := []any{}
	for _, sheet := range sheets {
		list = append(list, sheet)
	}
	if _, err := s.Set("grades_sheets", list); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDropboxLinks(t *testing.T) {
	for url, want := range map[string]string{
		scl:      "https://dl.dropboxusercontent.com/scl/fi/a1b2c3d4e5/DM%20grades%202026.xlsx?rlkey=k9x8y7&st=s1&dl=1",
		old:      "https://dl.dropboxusercontent.com/s/q7w8e9r0/Calculus.xlsx?dl=1",
		direct:   "https://dl.dropboxusercontent.com/scl/fi/z5y4x3/Algebra.xlsx?rlkey=r1&dl=1",
		sheetURL: strings.ReplaceAll(ExportURL, "{id}", "abc123"),
	} {
		if got, err := DownloadURL(url); err != nil || got != want {
			t.Errorf("%s: %s %v", url, got, err)
		}
	}
	var ids []string
	for _, url := range []string{scl, old, direct, sheetURL} {
		sid, _ := SheetID(url)
		ids = append(ids, sid)
	}
	if !reflect.DeepEqual(ids, []string{"dropbox-a1b2c3d4e5", "dropbox-q7w8e9r0", "dropbox-z5y4x3", "abc123"}) {
		t.Fatal(ids)
	}
	_, name, _, _ := settings.DropboxFile(scl)
	if FileTitle(name) != "DM grades 2026" || FileTitle("Матан.v2.xlsx") != "Матан.v2" || FileTitle(".xlsx") != ".xlsx" {
		t.Fatal(FileTitle(name))
	}
}

// google serves a sheet's export and its HTML view; data is the workbook, or an error code.
type google struct {
	mu     sync.Mutex
	data   []byte
	status int
	titles int
}

func (g *google) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/htmlview") {
			g.titles++
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><title>ДМ &quot;ПИ&quot; - Google Диск</title></html>`))
			return
		}
		if g.status != 0 {
			w.WriteHeader(g.status)
			return
		}
		w.Write(g.data)
	}))
	t.Cleanup(server.Close)
	export, title := ExportURL, TitleURL
	ExportURL, TitleURL = server.URL+"/d/{id}/export", server.URL+"/d/{id}/htmlview"
	t.Cleanup(func() { ExportURL, TitleURL = export, title })
	return server
}

// countingSheet is OnSheet of a processing that counts the reads: the first is the baseline.
func countingSheet(reads *int) func(context.Context, Sheet) (Read, error) {
	return func(_ context.Context, sheet Sheet) (Read, error) {
		*reads++
		items := 0
		for _, v := range sheet.Views {
			items += len(v.Mine)
		}
		return Read{First: *reads == 1, Items: items}, nil
	}
}

func TestPollFailures(t *testing.T) {
	g := &google{data: fixture(t, "book_default")}
	server := g.serve(t)
	now := newClock()
	source := New(newSettings(t, map[string]any{"url": sheetURL, "label": "DM"}))
	source.Now, source.Client = now.now, server.Client()
	reads := 0
	source.OnSheet = countingSheet(&reads)
	var system []string
	var alerts []bool
	source.Notify = func(line i18n.Phrase) { system = append(system, line.Ru); alerts = append(alerts, line.Alert) }
	ctx := context.Background()
	source.Poll(ctx)
	status := source.Status().Sheets["abc123"]
	if *status.Title != `ДМ "ПИ"` || status.Tabs != 2 || status.Error != nil || status.Unread == nil {
		t.Fatalf("%+v", status)
	}
	if !reflect.DeepEqual(system, []string{`Таблица оценок «ДМ "ПИ"» подключена: вкладок со своей строкой — 2, ` +
		`оценок — 9. Они в линзах курсов в приложении.`}) {
		t.Fatal(system)
	}

	g.status = 404
	source.Poll(ctx)
	if got := source.Status().Sheets["abc123"].Error; got != "HTTP 404 Not Found" {
		t.Fatal(got)
	}
	g.status, g.data = 0, []byte("<html>sign in</html>")
	source.Poll(ctx)
	if got := source.Status().Sheets["abc123"].Error.(string); !strings.Contains(got, "not a workbook") {
		t.Fatal(got)
	}
	g.data = fixture(t, "book_other_name") // the row is gone
	source.Poll(ctx)
	if got := source.Status().Sheets["abc123"].Error; got != NoRow {
		t.Fatal(got)
	}
	if len(system) != 1 { // only "подключена"
		t.Fatal(system)
	}

	now.moment = now.moment.Add(13 * time.Hour)
	source.Poll(ctx)
	source.Poll(ctx)
	if len(system) != 2 || !strings.HasPrefix(system[1], `Таблица оценок «ДМ "ПИ"» не читается с 20.10 12:00: своей строки нет`) {
		t.Fatal(system)
	}
	g.data = fixture(t, "book_default")
	source.Poll(ctx)
	if !reflect.DeepEqual(system[2:], []string{`Таблица оценок «ДМ "ПИ"» снова читается.`}) {
		t.Fatal(system)
	}
	if !reflect.DeepEqual(alerts, []bool{false, true, false}) { // only the failure notifies (D55)
		t.Fatal(alerts)
	}
	if st := source.Status().Sheets["abc123"]; st.Error != nil || *st.OkAt != "2026-10-21T01:00:00" || g.titles != 1 {
		t.Fatalf("%+v %d", st, g.titles)
	}
}

func TestPollReportsErrorsWithoutRaising(t *testing.T) {
	export := ExportURL
	ExportURL = "http://127.0.0.1:9/{id}"
	t.Cleanup(func() { ExportURL = export })
	s := newSettings(t, map[string]any{"url": sheetURL, "label": "DM"})
	source := New(s)
	source.Client = &http.Client{Timeout: 5 * time.Second}
	if err := source.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.Status().Sheets["abc123"].Error == nil {
		t.Fatal("no error")
	}
	s.Set("grades_sheets", []any{})
	source.Poll(context.Background())
	if st := source.Status(); st.Configured || len(st.Sheets) != 0 {
		t.Fatal(st)
	}
}

// The service's own client never reaches a sheet served from the private network (D79).
func TestPollRefusesAPrivateAddress(t *testing.T) {
	g := &google{data: fixture(t, "book_default")}
	g.serve(t)
	source := New(newSettings(t, map[string]any{"url": sheetURL, "label": "DM"}))
	source.Poll(context.Background())
	if got, _ := source.Status().Sheets["abc123"].Error.(string); !strings.HasPrefix(got, "PrivateAddress: 127.0.0.1") {
		t.Fatal(got)
	}
}

func TestDropboxSheet(t *testing.T) {
	var mu sync.Mutex
	data, disposition := fixture(t, "book_default"), ""
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/scl/fi/") { // a share link redirects to the file
			asked = append(asked, r.URL.RawQuery)
			http.Redirect(w, r, "/content/"+strings.Split(r.URL.Path, "/")[3], http.StatusFound)
			return
		}
		if disposition != "" {
			w.Header().Set("Content-Disposition", disposition)
		}
		w.Write(data)
	}))
	defer server.Close()
	real := downloadURL
	downloadURL = func(url string) (string, error) {
		got, err := real(url)
		return strings.Replace(got, "https://dl.dropboxusercontent.com", server.URL, 1), err
	}
	t.Cleanup(func() { downloadURL = real })
	now := newClock()
	source := New(newSettings(t, map[string]any{"url": scl, "label": ""}))
	source.Now, source.Client = now.now, server.Client()
	var courses []string
	source.OnSheet = func(_ context.Context, sheet Sheet) (Read, error) {
		courses = append(courses, sheet.Course)
		return Read{First: len(courses) == 1, Items: 2}, nil
	}
	ctx, sid := context.Background(), "dropbox-a1b2c3d4e5"
	source.Poll(ctx) // the baseline, as for a Google Sheet
	st := source.Status().Sheets[sid]
	if *st.Title != "DM grades 2026" || st.Error != nil || st.Tabs != 2 || courses[0] != "DM grade" {
		t.Fatalf("%+v %v", st, courses)
	}
	if !reflect.DeepEqual(asked, []string{"rlkey=k9x8y7&st=s1&dl=1"}) {
		t.Fatal(asked)
	}

	// The file was renamed: Content-Disposition names it.
	disposition = `attachment; filename="x.xlsx"; filename*=UTF-8''%D0%94%D0%9C%20final.xlsx`
	source.Poll(ctx)
	if got := *source.Status().Sheets[sid].Title; got != "ДМ final" {
		t.Fatal(got)
	}

	// A deleted or private link gives an HTML page: not a workbook.
	data, disposition = []byte("<html><title>Dropbox - Error</title></html>"), ""
	source.Poll(ctx)
	if got, _ := source.Status().Sheets[sid].Error.(string); !strings.Contains(got, "not a workbook") {
		t.Fatal(got)
	}

	// The link now gives a file of another kind.
	data, disposition = fixture(t, "book_default"), `attachment; filename="grades.xls"`
	source.Poll(ctx)
	got := source.Status().Sheets[sid].Error.(i18n.Phrase)
	if got.En != "the Dropbox file «grades.xls» is not an .xlsx workbook" || got.Ru != "файл Dropbox «grades.xls» — не книга .xlsx" {
		t.Fatal(got)
	}
}
