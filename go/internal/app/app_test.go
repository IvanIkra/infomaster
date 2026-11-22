package app

// The wiring in the service (tests/test_app.py): a RUZ or HSE App X poll reaches the dictionary, the merged
// deadlines and Web Push; the sources reach both ports; the watch follows the twin's logins.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/collectors/telegram"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/outputs/bot"
	"github.com/IvanIkra/infomaster/go/internal/processing/batches"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	hsedl "github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
)

var names = map[string][2]string{
	"Математический анализ": {"Calculus", "Calc"},
	"Алгебра":                                {"Algebra", "Alg"},
	"Algebra":                                {"Algebra", "Alg"},
	"Discrete Mathematics":                   {"Discrete Math", "DM"},
	"Introduction into Software Engineering": {"Intro to SE", "SE"},
	"Основы российской государственности": {"Rus Statehood", "RSF"},
	"Foundations of Russian Statehood":    {"Rus Statehood", "RSF"},
	"Введение в программирование":         {"Intro to Prog", "Prog"},
	"Calculus": {"Calculus", "Calc"},
}

// namer is Haiku for the dictionary: it names the courses it knows.
func namer() *llm.Fake {
	return &llm.Fake{Respond: func(req llm.Request) (any, error) {
		if req.Task != "subjects" {
			return nil, errors.New("unexpected task " + req.Task)
		}
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

func env() config.Env {
	return config.Env{APIID: 1, APIHash: "h", BotToken: "1:t", OwnerID: 42, APIToken: "tok",
		PublicURL: "https://x.ts.net"}
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// told is one push: kind deadline (title, was), reminder, budget (its level), or system (its text, login).
type told struct{ Kind, Title, Was string }

// recorder is the notifier, recorded.
type recorder struct {
	mu   sync.Mutex
	told []told
}

func (r *recorder) add(t told) {
	r.mu.Lock()
	r.told = append(r.told, t)
	r.mu.Unlock()
}

func (r *recorder) Deadline(d todo.Deadline, was string) { r.add(told{"deadline", d.Title, was}) }
func (r *recorder) Reminder(name string, due []todo.Deadline) {
	r.add(told{"reminder", name, ""})
}
func (r *recorder) Budget(notice llm.Notice)              { r.add(told{"budget", string(notice.Level), ""}) }
func (r *recorder) System(text i18n.Phrase, login string) { r.add(told{"system", text.En, login}) }

func (r *recorder) all() []told {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]told{}, r.told...)
}

// service is the service at Tue 2026-10-20 noon in Moscow, what goes to Web Push recorded; Telegram clients are
// made, never connected.
func service(t *testing.T, e config.Env) (*Service, *recorder) {
	t.Helper()
	s := must(New(e, t.TempDir(), Options{}))
	t.Cleanup(func() { s.Store.Close() })
	moment := time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()
	now := func() time.Time { return moment }
	s.Ruz.Now, s.Schedule.Now, s.Merged.Now, s.HseAppDeadlines.Now = now, now, now, now
	s.Watch.Now, s.Reminders.Now, s.Batches.Now = now, now, now
	s.Subjects.LLM = namer()
	r := &recorder{}
	s.Notifier = r
	return s, r
}

func compact(t *testing.T, value any) string {
	t.Helper()
	text, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

func titles(ds []todo.Deadline) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Title)
	}
	return out
}

func TestAfterPoll(t *testing.T) {
	ctx := context.Background()
	s, r := service(t, env())
	if err := s.Watch.Refresh(); err != nil { // the list was seen before RUZ came
		t.Fatal(err)
	}
	diff := must(s.Ruz.Sync(must(ruz.Parse(must(os.ReadFile("testdata/ruz.ics")))), false))
	if err := s.afterPoll(ctx, *diff); err != nil {
		t.Fatal(err)
	}
	if got := r.all(); len(got) != 0 { // the first poll is no news: its Midterms join quietly
		t.Fatalf("told %v", got)
	}
	if got := titles(must(s.Merged.Visible())); strings.Join(got, "|") != "RSF Midterm|Prog Midterm" {
		t.Fatalf("visible %v", got)
	}
	if got := compact(t, must(s.RuzStatus())); got !=
		`{"ok_at":null,"error":"RUZ_ICAL_URL is not set","classes":9,"subjects_error":null}` {
		t.Fatalf("ruz status %s", got)
	}
	if feed := must(s.Calendar.Render("main")); !strings.HasPrefix(string(feed), "BEGIN:VCALENDAR") {
		t.Fatalf("feed %q", feed[:20])
	}
	if got := s.Calendar.EditLink("2000001@ruz.hse.ru"); got != "https://x.ts.net/web/#/c?uid=2000001%40ruz.hse.ru" {
		t.Fatalf("edit link %s", got) // the card in the web app (D69)
	}
	sourcesReachThePorts(t, s)
	midtermFollowsItsCorrection(t, s, r)
	dictionaryRenamesTheMidtermsAtOnce(t, s, r)

	s.PublishSheets = func(context.Context) error { return errors.New("the sheets are down") }
	before := len(r.all())
	if err := s.Store.InTx(func(tx *sql.Tx) error {
		_, err := s.Deadlines.With(tx).Add(deadlines.New{Source: "tg", MsgIDs: []int64{9}, Label: "Alg HW 5",
			Due: "2026-10-30"}, s.Batches.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// the sheets failing does not stop the deadlines
	if err := s.publish(ctx, batches.Changes{}); err != nil {
		t.Fatal(err)
	}
	if got := r.all(); len(got) != before+1 || got[before] != (told{"deadline", "Alg HW 5", ""}) {
		t.Fatalf("told %v", got[before:])
	}
}

func get(t *testing.T, h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// sourcesReachThePorts: both ports get the service's sources: «Machine» and /api/status read them by name.
func sourcesReachThePorts(t *testing.T, s *Service) {
	listed := []string{}
	for _, source := range s.Sources {
		listed = append(listed, source.Name)
	}
	if got := strings.Join(listed, ","); got != "telegram,batches,ruz,hseapp,mail,sheets,grades,wiki,images" {
		t.Fatalf("sources %s", got)
	}
	if s.Actions.Sources == nil || s.Web.Parts.Sources == nil || s.API.Sources == nil {
		t.Fatal("a port has no sources")
	}
	var deadlines struct{ Items []struct{ Title string } }
	w := get(t, s.InternalHandler(), "/api/deadlines", "Authorization", "Bearer tok")
	if err := json.Unmarshal(w.Body.Bytes(), &deadlines); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if len(deadlines.Items) != 2 || deadlines.Items[0].Title != "RSF Midterm" ||
		deadlines.Items[1].Title != "Prog Midterm" {
		t.Fatalf("deadlines %s", w.Body.String())
	}
	w = get(t, s.InternalHandler(), "/api/status", "Authorization", "Bearer tok")
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err, w.Body.String())
	}
	for _, key := range []string{"telegram", "batches", "llm"} {
		if body[key] == nil {
			t.Fatalf("no %s in %s", key, w.Body.String())
		}
	}
	text := w.Body.String()
	for _, part := range []string{
		`"ruz": {"ok_at": null, "error": "RUZ_ICAL_URL is not set", "classes": 9, "subjects_error": null}`,
		`"hseapp": {"enabled": true, "logged_in": false, "ok_at": null, "error": null, "login_needed": true, ` +
			`"items": 0, "done": 0, "labels_error": null}`,
		`"mail": {"configured": false, "ok_at": null, "error": "MAIL_USER and MAIL_PASSWORD are not set", ` +
			`"last": null, "letters": 0}`,
		`"grades": {"configured": false, "sheets": {}, `,
		`"telegram": {"ok_at": null, "error": null, "login_needed": false, "account": null, "login": null}`,
	} {
		if !strings.Contains(text, part) {
			t.Fatalf("no %s in %s", part, text)
		}
	}
	var grades struct{ New int }
	if err := json.Unmarshal(body["grades"], &grades); err != nil || grades.New != 0 {
		t.Fatalf("grades %s", body["grades"])
	}
	if s.Batches.Mail { // without the box the schedule does not wait for letters
		t.Fatal("mail is not configured")
	}
}

// midtermFollowsItsCorrection: a chat moved the interim assessment: the batch run tells the Midterm's move, the
// owner's hiding moves it back.
func midtermFollowsItsCorrection(t *testing.T, s *Service, r *recorder) {
	ctx := context.Background()
	uid := "2000007@ruz.hse.ru"
	item := must(s.Ruz.ByUID(uid))
	var moved int64
	if err := s.Store.InTx(func(tx *sql.Tx) error {
		var err error
		moved, err = s.Schedule.Events.With(tx).Add(events.New{UID: uid, ClassUID: uid, Source: "tg",
			MsgIDs: []int64{1}, Course: item.Subject, Title: "Intro to Prog", Kind: item.Kind,
			Start: "2026-11-05T10:00:00", End: "2026-11-05T13:00:00", Room: "R401", Required: true}, s.Batches.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Batches.RunChats(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	last := func() told { all := r.all(); return all[len(all)-1] }
	if got := r.all(); len(got) == 0 || last() != (told{"deadline", "Prog Midterm", "2026-10-29"}) {
		t.Fatalf("told %v", got)
	}
	if _, err := s.Schedule.SetHidden(ctx, moved, true); err != nil {
		t.Fatal(err)
	}
	if got := last(); got != (told{"deadline", "Prog Midterm", "2026-11-05"}) {
		t.Fatalf("told %v", got)
	}
}

// dictionaryRenamesTheMidtermsAtOnce: a manual entry in the dictionary: the Midterm's label follows now, and a
// rename is no news.
func dictionaryRenamesTheMidtermsAtOnce(t *testing.T, s *Service, r *recorder) {
	before := len(r.all())
	if _, err := s.Subjects.SetManual("Foundations of Russian Statehood", "Statehood", "State"); err != nil {
		t.Fatal(err)
	}
	s.Settle()
	row := must(s.Deadlines.Get(*must(s.Ruz.ByUID("2000006@ruz.hse.ru")).DeadlineID))
	if row.Label != "State Midterm" {
		t.Fatalf("label %s", row.Label)
	}
	if got := r.all(); len(got) != before {
		t.Fatalf("told %v", got[before:])
	}
}

// TestDeadlinesWiring: the first HSE App X poll joins quietly, later changes are told; the owner's own changes are
// not.
func TestDeadlinesWiring(t *testing.T) {
	ctx := context.Background()
	s, r := service(t, env())
	if err := s.Watch.Refresh(); err != nil {
		t.Fatal(err)
	}
	lists := func(items ...[3]string) {
		rows := []hsedl.Row{}
		for _, item := range items {
			rows = append(rows, hsedl.Row{ID: item[0], Label: item[1], Due: item[2], Title: item[1]})
		}
		if err := s.Store.PutJSON(hsedl.ListsKey, hsedl.Lists{Items: rows, Done: []hsedl.Row{}}); err != nil {
			t.Fatal(err)
		}
	}
	lists([3]string{"h1", "DM HW 3", "2026-10-22"}, [3]string{"h2", "Calc HW 1", "2026-10-23"})
	if err := s.HseAppDeadlines.OnChange(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := r.all(); len(got) != 0 {
		t.Fatalf("told %v", got)
	}
	lists([3]string{"h1", "DM HW 3", "2026-10-22"}, [3]string{"h2", "Calc HW 1", "2026-10-23"},
		[3]string{"h3", "Eng essay", "2026-10-25"})
	if err := s.HseAppDeadlines.OnChange(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := r.all(); len(got) != 1 || got[0] != (told{"deadline", "Eng essay", ""}) {
		t.Fatalf("told %v", got)
	}

	if _, err := s.Merged.Add("Dentist", s.Merged.Today(), "personal", ""); err != nil { // the owner's: no news
		t.Fatal(err)
	}
	s.Watch.Settle()
	if got := r.all(); len(got) != 1 {
		t.Fatalf("told %v", got)
	}

	if err := s.Store.InTx(func(tx *sql.Tx) error { // what a batch run saved
		_, err := s.Deadlines.With(tx).Add(deadlines.New{Source: "tg", MsgIDs: []int64{1}, Label: "Alg HW 2",
			Due: "2026-10-24"}, s.Batches.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Batches.Publish(ctx, batches.Changes{}); err != nil {
		t.Fatal(err)
	}
	if got := r.all(); got[len(got)-1] != (told{"deadline", "Alg HW 2", ""}) {
		t.Fatalf("told %v", got)
	}
}

// TestNoticesGoToWebPush: D69: the sources' lines, the logins they need and the LLM limit reach the notifier, each
// with the login it leads to (the notifier itself pushes only failures, logins and the spent limit: internal/web).
func TestNoticesGoToWebPush(t *testing.T) {
	s, r := service(t, env())
	if s.HseApp.CallbackURL != "https://x.ts.net/hseapp/callback" {
		t.Fatalf("callback %s", s.HseApp.CallbackURL)
	}
	if s.Web.Notifier.Settings != s.Settings || s.Web.Auth.Verifier == nil {
		t.Fatal("the web app's notifier or passkeys are not wired")
	}
	s.Ruz.Notify(i18n.P("RUZ is available again.", "РУЗ снова доступен."))
	s.Mail.Notify(i18n.AlertP("Mail is unavailable.", "Почта недоступна."))
	s.HseApp.Notify(i18n.P("HSE App X needs a login.", "HSE App X нужен вход."), true)
	s.HseApp.Notify(i18n.P("HSE App X works again.", "HSE App X снова работает."), false)
	s.Twin.NotifyLogin(i18n.P("The twin needs a login.", "Твинку нужен вход."))
	s.Twin.Notify(i18n.P("The twin logged out.", "Твинк вышел."))
	for _, notify := range []func(i18n.Phrase){s.Batches.Notify, s.Wiki.Notify, s.GradeSheets.Notify} {
		notify(i18n.P("line", "строка"))
	}
	s.Budget.Notify(llm.Notice{Level: llm.Soft, Spent: 0.4, Limit: 0.5})
	s.Budget.Notify(llm.Notice{Level: llm.Hard, Spent: 0.5, Limit: 0.5})
	want := []told{{"system", "RUZ is available again.", ""}, {"system", "Mail is unavailable.", ""},
		{"system", "HSE App X needs a login.", "hseapp"}, {"system", "HSE App X works again.", ""},
		{"system", "The twin needs a login.", "twin"}, {"system", "The twin logged out.", ""},
		{"system", "line", ""}, {"system", "line", ""}, {"system", "line", ""},
		{"budget", "soft", ""}, {"budget", "hard", ""}}
	if got := r.all(); compact(t, got) != compact(t, want) {
		t.Fatalf("told %v", got)
	}
}

// --- the watch -----------------------------------------------------------------------------------------------

type fakeBot struct{ done chan struct{} }

func (b *fakeBot) Done() <-chan struct{} { return b.done }

type fakeTwin struct {
	mu      sync.Mutex
	client  chan struct{}
	changed chan struct{}
}

func newTwin() *fakeTwin { return &fakeTwin{changed: make(chan struct{}, 1)} }

func (f *fakeTwin) Done() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.client == nil {
		return nil
	}
	return f.client
}

func (f *fakeTwin) Changed() <-chan struct{} { return f.changed }

// set changes the client and tells, as a login or a logout does.
func (f *fakeTwin) set(client chan struct{}) {
	f.mu.Lock()
	f.client = client
	f.mu.Unlock()
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

func watching(ctx context.Context, b Doner, twin Switcher) chan error {
	result := make(chan error, 1)
	go func() { result <- Watch(ctx, b, twin) }()
	return result
}

func pause() { time.Sleep(10 * time.Millisecond) }

// TestWatchFollowsTheTwinsLoginAndLogout: a login in «Machine» is watched from then on, a logout is no loss, a lost
// connection ends the service.
func TestWatchFollowsTheTwinsLoginAndLogout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, twin := &fakeBot{done: make(chan struct{})}, newTwin()
	watch := watching(ctx, b, twin)
	pause()

	first := make(chan struct{})
	twin.set(first) // logged in
	pause()
	twin.set(nil) // logged out: its client disconnects too, and that is no loss
	close(first)
	pause()
	select {
	case err := <-watch:
		t.Fatalf("the watch ended: %v", err)
	default:
	}

	second := make(chan struct{})
	twin.set(second)
	pause()
	close(second) // lost for good
	select {
	case err := <-watch:
		if !errors.Is(err, ErrLost) || !strings.HasPrefix(err.Error(), "the twin ") {
			t.Fatalf("watch: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the watch goes on")
	}

	stopping, stop := context.WithCancel(context.Background())
	watch = watching(stopping, b, newTwin())
	stop()
	select {
	case err := <-watch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the watch goes on after the stop")
	}

	for range 20 { // a stop disconnects the clients at once: still a stop, not a loss
		stopping, stop := context.WithCancel(context.Background())
		ending := &fakeBot{done: make(chan struct{})}
		stop()
		close(ending.done)
		if err := Watch(stopping, ending, newTwin()); err != nil {
			t.Fatalf("a stop as a loss: %v", err)
		}
	}

	lost := &fakeBot{done: make(chan struct{})}
	watch = watching(ctx, lost, newTwin())
	close(lost.done)
	select {
	case err := <-watch:
		if !errors.Is(err, ErrLost) || !strings.HasPrefix(err.Error(), "the bot ") {
			t.Fatalf("watch: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the watch goes on without the bot")
	}
}

// --- both ports ----------------------------------------------------------------------------------------------

// botAPI is the bot on Telegram, faked: nothing connects.
type botAPI struct{ done chan struct{} }

func (f *botAPI) Send(context.Context, int64, string, bot.SendOptions) error { return nil }
func (f *botAPI) ResetMenu(context.Context, int64) error                     { return nil }
func (f *botAPI) ResetCommands(context.Context) error                        { return nil }
func (f *botAPI) Download(context.Context, *tg.Message, string) (bool, error) {
	return false, nil
}
func (f *botAPI) Done() <-chan struct{} { return f.done }
func (f *botAPI) Close() error          { return nil }

// TestPorts: Serve answers on both ports (status, a web page, the 404s, the connector's metadata, the API behind
// its Bearer token, the feeds outside it), and a stop closes everything.
func TestPorts(t *testing.T) {
	e := env()
	e.CalendarSecret, e.CalendarPublicSecret = "internal-secret-0123", "public-secret-012345"
	s, r := service(t, e)
	s.Bot.Connect = func(context.Context, string, func(context.Context, bot.Event)) (bot.API, error) {
		return &botAPI{done: make(chan struct{})}, nil
	}
	s.Twin.Connect = func(context.Context, string, telegram.Handler) (telegram.Client, error) {
		return nil, telegram.ErrNotAuthorized
	}
	if info := must(os.Stat(s.DataDir)); info.Mode().Perm() != 0o700 {
		t.Fatalf("the data directory is %v", info.Mode().Perm())
	}
	public, internal := must(net.Listen("tcp", "127.0.0.1:0")), must(net.Listen("tcp", "127.0.0.1:0"))
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, public, internal) }()

	client := &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fetch := func(listener net.Listener, path string, headers ...string) (int, http.Header, string) {
		t.Helper()
		req := must(http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+path, nil))
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp := must(client.Do(req))
		defer resp.Body.Close()
		return resp.StatusCode, resp.Header, string(must(io.ReadAll(resp.Body)))
	}
	expect := func(listener net.Listener, path string, status int, body string, headers ...string) string {
		t.Helper()
		code, _, text := fetch(listener, path, headers...)
		if code != status || !strings.Contains(text, body) {
			t.Fatalf("GET %s: %d %q", path, code, text)
		}
		return text
	}

	// the public port
	if code, header, _ := fetch(public, "/web"); code != http.StatusFound || header.Get("Location") != "/web/" {
		t.Fatalf("GET /web: %d %v", code, header)
	}
	if _, header, _ := fetch(public, "/web/"); header.Get("Content-Security-Policy") == "" {
		t.Fatal("the web app's headers are missing")
	}
	expect(public, "/web/", http.StatusOK, "<html")
	expect(public, "/nowhere", http.StatusNotFound, "404: Not Found")
	expect(public, "/.well-known/oauth-authorization-server", http.StatusOK, `"issuer": "https://x.ts.net"`)
	expect(public, "/.well-known/oauth-protected-resource/mcp", http.StatusOK, "https://x.ts.net/mcp")
	expect(public, "/calendar/public-secret-012345/main.ics", http.StatusOK, "BEGIN:VCALENDAR")
	expect(public, "/calendar/public-secret-012345/tablo.ics", http.StatusNotFound, "")
	expect(public, "/calendar/internal-secret-0123/main.ics", http.StatusNotFound, "")
	expect(public, "/hseapp/callback?state=x&code=y", http.StatusBadRequest, "<!doctype html>")
	if code, _, _ := fetch(public, "/web/api/feed"); code != http.StatusUnauthorized {
		t.Fatalf("the web app's API without a session: %d", code)
	}
	if code, _, _ := fetch(public, "/library/nothing"); code != http.StatusNotFound {
		t.Fatalf("a wrong library link: %d", code)
	}

	// the internal port
	expect(internal, "/api/status", http.StatusUnauthorized, "Bearer token required")
	expect(internal, "/api/status", http.StatusOK, `"telegram": {`, "Authorization", "Bearer tok")
	expect(internal, "/api/deadlines", http.StatusOK, `{"items": []}`, "Authorization", "Bearer tok")
	expect(internal, "/api/grades", http.StatusOK, `"items"`, "Authorization", "Bearer tok")
	expect(internal, "/api/news", http.StatusOK, `"ttl_hours": 24`, "Authorization", "Bearer tok")
	expect(internal, "/calendar/internal-secret-0123/tablo.ics", http.StatusOK, "BEGIN:VCALENDAR")
	expect(internal, "/nowhere", http.StatusNotFound, "404: Not Found")
	if code, _, _ := fetch(internal, "/mcp"); code != http.StatusNotFound {
		t.Fatalf("the connector on the internal port: %d", code)
	}

	// the twin has no session: the service runs on and asks for a login
	found := false
	for _, line := range r.all() {
		found = found || (line.Kind == "system" && line.Was == "twin")
	}
	if !found {
		t.Fatalf("no login notice of the twin: %v", r.all())
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the service does not stop")
	}
	if _, err := net.DialTimeout("tcp", public.Addr().String(), time.Second); err == nil {
		t.Fatal("the public port still listens")
	}
}

// TestShadow: the shadow mode serves both ports on the database as it is, and starts nothing else: neither the bot
// nor the twin connects, no loop runs, nothing is pushed, and a stop is clean.
func TestShadow(t *testing.T) {
	s := must(New(env(), t.TempDir(), Options{Shadow: true}))
	if _, ok := s.Notifier.(silent); !ok {
		t.Fatalf("the shadow's notifier is %T", s.Notifier)
	}
	r := &recorder{}
	s.Notifier = r // to see that nothing would be pushed
	s.Bot.Connect = func(context.Context, string, func(context.Context, bot.Event)) (bot.API, error) {
		t.Error("the bot connects in the shadow mode")
		return nil, errors.New("no")
	}
	s.Twin.Connect = func(context.Context, string, telegram.Handler) (telegram.Client, error) {
		t.Error("the twin connects in the shadow mode")
		return nil, errors.New("no")
	}
	s.PublishSheets = func(context.Context) error {
		t.Error("a loop runs in the shadow mode")
		return nil
	}
	public, internal := must(net.Listen("tcp", "127.0.0.1:0")), must(net.Listen("tcp", "127.0.0.1:0"))
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, public, internal) }()

	get := func(listener net.Listener, path string, auth bool) (int, string) {
		t.Helper()
		req := must(http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+path, nil))
		if auth {
			req.Header.Set("Authorization", "Bearer tok")
		}
		resp := must((&http.Client{Timeout: 10 * time.Second}).Do(req))
		defer resp.Body.Close()
		return resp.StatusCode, string(must(io.ReadAll(resp.Body)))
	}
	if code, body := get(public, "/web/", false); code != http.StatusOK || !strings.Contains(body, "<html") {
		t.Fatalf("GET /web/: %d", code)
	}
	if code, body := get(internal, "/api/status", true); code != http.StatusOK || !strings.Contains(body, `"telegram": {`) {
		t.Fatalf("GET /api/status: %d %s", code, body)
	}
	time.Sleep(200 * time.Millisecond) // a loop or a client would have started by now
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the shadow does not stop")
	}
	if told := r.all(); len(told) != 0 {
		t.Fatalf("pushed in the shadow mode: %v", told)
	}
	if _, err := os.Stat(filepath.Join(s.DataDir, "bot.gotd.json")); err == nil {
		t.Fatal("the bot made a session in the shadow mode")
	}
}
