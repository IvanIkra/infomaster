package hseapp

// HSE App X against a fake Keycloak and a fake API: never the real ones, never real tokens.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const callback = "https://infomaster.example.ts.net/hseapp/callback"

func item(title string, expiresAt any, discipline any, status any, id any) map[string]any {
	return map[string]any{"id": id, "title": title, "description": "", "discipline": discipline,
		"expires_at": expiresAt, "completion_status": status, "is_pinned": false, "groups": []any{},
		"emails": []any{}}
}

func active(title string, expiresAt any, id any) map[string]any {
	return item(title, expiresAt, nil, "ACTIVE", id)
}

var items = []any{
	item("Дедлайн ДЗ 3 по  Дискретной Математике", "2026-10-22T20:59:00.000Z",
		"Дискретная математика (2026/2027 модули: 1,2,3,4) (Иванов И.И.)", "ACTIVE", "8f1c2a0e"),
	active("Доп лист Матан", "2026-10-19T20:59:01.000Z", "91aa07d3"), // overdue: kept, the screen picks
	item("Done one", "2026-10-25T20:59:00.000Z", nil, "COMPLETED", "c0ffee01"),
}

var done = []any{
	item("ДЗ 2 по БЖД", "2026-10-13T20:59:00.000Z",
		"Безопасность жизнедеятельности (2026/2027 модуль: 1) (Петров П.П.)", "COMPLETED_LATE", "b2d70c44"),
	active("Active one slipped in", "2026-10-25T20:59:00.000Z", "ac71ve00"),
}

// jsonItems round-trips items through JSON with numbers as json.Number, as a page is decoded.
func jsonItems(t *testing.T, list []any) []any {
	t.Helper()
	data, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return value.([]any)
}

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, clock.TZ) }

// --- the fake Keycloak and API ------------------------------------------------------------

type fakeHSE struct {
	mu           sync.Mutex
	access       string
	refresh      string
	issued       int
	grants       []string
	redirects    []string
	codes        map[string]string
	rejectAccess map[string]bool
	pages        [][]any
	completed    []any
	status       int
	bodies       []body
	server       *httptest.Server
}

type body struct {
	completed bool
	cursor    string // "" is None
}

func newFake(t *testing.T) *fakeHSE {
	f := &fakeHSE{access: "access-secret-0", refresh: "refresh-secret-1", codes: map[string]string{},
		rejectAccess: map[string]bool{}, pages: [][]any{items}, completed: done, status: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("POST /deadlines", f.deadlines)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func answer(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func (f *fakeHSE) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != ClientID {
		http.Error(w, "bad client", 500)
		return
	}
	form := r.PostForm
	f.grants = append(f.grants, form.Get("grant_type"))
	if form.Get("grant_type") == "refresh_token" {
		if form.Get("refresh_token") != f.refresh {
			answer(w, 400, map[string]any{"error": "invalid_grant", "error_description": "Token is not active"})
			return
		}
	} else {
		if form.Get("grant_type") != "authorization_code" {
			http.Error(w, "bad grant", 500)
			return
		}
		f.redirects = append(f.redirects, form.Get("redirect_uri"))
		challenge, ok := f.codes[form.Get("code")]
		delete(f.codes, form.Get("code"))
		digest := sha256.Sum256([]byte(form.Get("code_verifier")))
		if !ok || challenge != base64.RawURLEncoding.EncodeToString(digest[:]) {
			answer(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
	}
	f.issued++
	f.access = fmt.Sprintf("access-secret-%d", f.issued)
	f.refresh = fmt.Sprintf("refresh-secret-%d", f.issued+1)
	answer(w, 200, map[string]any{"access_token": f.access, "expires_in": 10800, "refresh_token": f.refresh,
		"refresh_expires_in": 1814400})
}

func (f *fakeHSE) deadlines(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != 200 {
		w.WriteHeader(f.status)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.access || f.rejectAccess[f.access] {
		w.WriteHeader(401)
		return
	}
	if r.Header.Get("Accept-Language") != "ru" {
		http.Error(w, "no language", 500)
		return
	}
	var request map[string]any
	data, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(data, &request); err != nil || len(request) != 1 {
		http.Error(w, "bad body", 500)
		return
	}
	completed, ok := request["is_completed"].(bool)
	if !ok {
		http.Error(w, "bad body", 500)
		return
	}
	f.bodies = append(f.bodies, body{completed, r.URL.Query().Get("cursor")})
	if completed {
		answer(w, 200, map[string]any{"next_cursor": nil, "items": f.completed})
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	var following any
	if page+1 < len(f.pages) {
		following = strconv.Itoa(page + 1)
	}
	answer(w, 200, map[string]any{"next_cursor": following, "items": f.pages[page]})
}

// --- the source under test ---------------------------------------------------------------

type notice struct {
	ru    string
	login bool
}

type fixture struct {
	t        *testing.T
	store    *store.Store
	settings *settings.Settings
	moment   time.Time
}

func newFixture(t *testing.T) *fixture {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	st, err := settings.New(s)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, store: s, settings: st, moment: time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()}
}

func (f *fixture) now() time.Time { return f.moment }

type source struct {
	*HseApp
	notices []notice
	polls   [][2][]Raw
}

func (f *fixture) source(fake *fakeHSE) *source {
	src := &source{HseApp: New(f.store, f.settings, callback, f.now)}
	if fake != nil {
		src.Auth.TokenURL = fake.server.URL + "/token"
		src.Deadlines = fake.server.URL + "/deadlines"
	}
	src.Notify = func(text i18n.Phrase, login bool) { src.notices = append(src.notices, notice{text.Ru, login}) }
	src.OnPoll = func(_ context.Context, active, done []Raw) error {
		src.polls = append(src.polls, [2][]Raw{active, done})
		return nil
	}
	return src
}

// seed leaves tokens as a login left them; the access token runs out in accessLeft seconds.
func seed(t *testing.T, src *source, refresh, access string, accessLeft float64) {
	t.Helper()
	at := float64(src.Now().UnixMicro()) / 1e6
	if err := src.store.PutJSON(AuthKey, map[string]any{"access_token": access, "expires_at": at + accessLeft,
		"refresh_token": refresh, "refresh_expires_at": at + 21*86400, "refreshed_at": at}); err != nil {
		t.Fatal(err)
	}
}

func seedDefault(t *testing.T, src *source) { seed(t, src, "refresh-secret-1", "access-secret-0", 0) }

func query(t *testing.T, address string) map[string]string {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range u.Query() {
		out[k] = v[0]
	}
	return out
}

func ids(list []Raw) []string {
	out := []string{}
	for _, r := range list {
		out = append(out, r.ID)
	}
	return out
}

func storedAuth(t *testing.T, s *store.Store) map[string]any {
	t.Helper()
	var data map[string]any
	if _, err := s.GetJSON(AuthKey, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func wantErr(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("error %v, want %q", err, text)
	}
}

func woken(h *HseApp) bool { return len(h.wake) == 1 }

// --- the collector ---------------------------------------------------------------------

func TestParse(t *testing.T) {
	act := Parse(jsonItems(t, items), false)
	if len(act) != 2 || act[0].Title != "Дедлайн ДЗ 3 по Дискретной Математике" || act[1].Title != "Доп лист Матан" {
		t.Fatalf("active: %+v", act)
	}
	want := Raw{"8f1c2a0e", "Дедлайн ДЗ 3 по Дискретной Математике", "Дискретная математика", day(2026, 10, 22)}
	if !reflect.DeepEqual(act[0], want) { // 20:59Z is 23:59 in Moscow: the same day
		t.Fatalf("%+v", act[0])
	}
	if act[1].Discipline != "" || !act[1].Due.Equal(day(2026, 10, 19)) {
		t.Fatalf("%+v", act[1])
	}
	fin := Parse(jsonItems(t, done), true)
	if len(fin) != 1 || fin[0].Title != "ДЗ 2 по БЖД" || fin[0].Discipline != "Безопасность жизнедеятельности" {
		t.Fatalf("done: %+v", fin)
	}

	naive := active("Наивный срок", "2026-10-22T21:30:00", nil)
	naive["id"] = 7
	odd := []any{active("Без срока", nil, "1"), active("Кривой срок", "завтра", "2"),
		active("  ", "2026-10-22T20:59Z", nil), active("Без id", "2026-10-22T21:00:00Z", nil),
		item("Без статуса", "2026-10-22T20:59:00Z", nil, nil, nil), "not a deadline", naive}
	got := Parse(jsonItems(t, odd), false)
	if len(got) != 2 ||
		got[0].ID != "Без id" || got[0].Title != "Без id" || !got[0].Due.Equal(day(2026, 10, 23)) || // past midnight
		got[1].ID != "7" || got[1].Title != "Наивный срок" || !got[1].Due.Equal(day(2026, 10, 23)) { // no zone: UTC
		t.Fatalf("odd: %+v", got)
	}
	if TrimDiscipline("Алгебра (рус) (Иванов)") != "Алгебра" || TrimDiscipline("") != "" {
		t.Fatal("trim")
	}
}

func TestPollRefreshesRotatesAndHandsBothLists(t *testing.T) {
	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	seedDefault(t, src) // the access token has run out
	ctx := context.Background()
	if !src.Poll(ctx) {
		t.Fatal("poll failed")
	}
	if !reflect.DeepEqual(fake.grants, []string{"refresh_token"}) {
		t.Fatal(fake.grants)
	}
	if storedAuth(t, f.store)["refresh_token"] != "refresh-secret-2" { // rotated: saved at once
		t.Fatal("not rotated")
	}
	if len(src.polls) != 1 || !reflect.DeepEqual(ids(src.polls[0][0]), []string{"8f1c2a0e", "91aa07d3"}) ||
		!reflect.DeepEqual(ids(src.polls[0][1]), []string{"b2d70c44"}) {
		t.Fatalf("%+v", src.polls)
	}
	if !reflect.DeepEqual(fake.bodies, []body{{false, ""}, {true, ""}}) {
		t.Fatal(fake.bodies)
	}
	okAt := "2026-10-20T12:00:00"
	if !reflect.DeepEqual(src.Status(), Status{Enabled: true, LoggedIn: true, OKAt: &okAt}) {
		t.Fatalf("%+v", src.Status())
	}
	data, _ := json.Marshal(src.Status())
	if string(data) != `{"enabled":true,"logged_in":true,"ok_at":"2026-10-20T12:00:00","error":null,"login_needed":false}` {
		t.Fatal(string(data))
	}

	if !src.Poll(ctx) || len(fake.grants) != 1 { // the new access token lives 3 hours
		t.Fatal(fake.grants)
	}

	f.moment = f.moment.Add(2*time.Hour + 51*time.Minute) // under 10 minutes left
	src.Poll(ctx)
	if len(fake.grants) != 2 || src.Auth.Data()["refresh_token"] != "refresh-secret-3" {
		t.Fatal(fake.grants)
	}
}

func TestStoredTokensAsPythonWritesThem(t *testing.T) {
	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	seedDefault(t, src)
	src.Poll(context.Background())
	var text string
	if err := f.store.DB.QueryRow("select value from kv where key = ?", AuthKey).Scan(&text); err != nil {
		t.Fatal(err)
	}
	at := f.moment.Unix()
	want := fmt.Sprintf(`{"access_token": "access-secret-1", "expires_at": %d.0, "refresh_token": "refresh-secret-2", `+
		`"refresh_expires_at": %d.0, "refreshed_at": %d.0}`, at+10800, at+1814400, at)
	if text != want {
		t.Fatalf("%s\nwant %s", text, want)
	}
}

func TestA401RefreshesOnceAndRetries(t *testing.T) {
	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	seed(t, src, "refresh-secret-1", "access-secret-0", 3*3600) // valid by its time, but the server revoked it
	fake.rejectAccess["access-secret-0"] = true
	if !src.Poll(context.Background()) || len(fake.grants) != 1 {
		t.Fatal(fake.grants)
	}

	fake.rejectAccess["access-secret-1"] = true // refused after the refresh too: no loop
	fake.rejectAccess["access-secret-2"] = true
	if src.Poll(context.Background()) {
		t.Fatal("poll went through")
	}
	if src.err != "HTTP 401 Unauthorized" || len(fake.grants) != 2 {
		t.Fatal(src.err, fake.grants)
	}
}

func TestPagesAreJoinedUpToTen(t *testing.T) {
	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	seedDefault(t, src)
	fake.pages = [][]any{items[:1], items[1:]}
	src.Poll(context.Background())
	if len(src.polls[0][0]) != 2 || !reflect.DeepEqual(fake.bodies, []body{{false, ""}, {false, "1"}, {true, ""}}) {
		t.Fatal(src.polls, fake.bodies)
	}

	fake.pages = nil
	for n := range 12 {
		fake.pages = append(fake.pages, []any{active(fmt.Sprintf("ДЗ %d", n), "2026-10-30T20:59:00Z", strconv.Itoa(n))})
	}
	src.Poll(context.Background())
	var titles, want []string
	for _, r := range src.polls[1][0] {
		titles = append(titles, r.Title)
	}
	for n := range 10 {
		want = append(want, fmt.Sprintf("ДЗ %d", n))
	}
	if !reflect.DeepEqual(titles, want) {
		t.Fatal(titles)
	}
}

func TestDeadSessionMeansLoginNeededToldOnce(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	seed(t, src, "refresh-secret-old", "access-secret-0", 0)
	if src.Poll(context.Background()) {
		t.Fatal("poll went through")
	}
	if data := storedAuth(t, f.store); data == nil || len(data) != 0 { // erased: a dead token is never sent again
		t.Fatal(data)
	}
	status := src.Status()
	if !status.LoginNeeded || status.LoggedIn || status.Error == nil || *status.Error != "login needed: invalid_grant" {
		t.Fatalf("%+v", status)
	}
	if len(src.notices) != 1 || !src.notices[0].login ||
		!strings.HasPrefix(src.notices[0].ru, "Вход в HSE App X больше не действует (invalid_grant)") {
		t.Fatal(src.notices)
	}
	for range 3 { // the loop's next rounds: no poll without a login, no second notice
		src.tellLogin()
	}
	if len(src.notices) != 1 || len(src.polls) != 0 {
		t.Fatal(src.notices)
	}
	if strings.Contains(logs.String(), "refresh-secret") || strings.Contains(logs.String(), "access-secret") {
		t.Fatal(logs.String())
	}

	link := query(t, src.LoginLink(false))
	fake.codes["code-1"] = link["code_challenge"]
	if err := src.Login(context.Background(), callback+"?state="+link["state"]+"&session_state=s&code=code-1"); err != nil {
		t.Fatal(err)
	}
	if src.notices[len(src.notices)-1] != (notice{"Вход в HSE App X выполнен: дедлайны обновляются.", false}) {
		t.Fatal(src.notices)
	}
	if status := src.Status(); status.LoginNeeded || status.Error != nil {
		t.Fatalf("%+v", status)
	}
	if !woken(src.HseApp) { // the loop polls at once
		t.Fatal("not woken")
	}
	if !src.Poll(context.Background()) || len(src.notices) != 2 {
		t.Fatal(src.notices)
	}
	if strings.Contains(logs.String(), "secret") {
		t.Fatal(logs.String())
	}
}

func TestFirstStartAsksForALoginOnce(t *testing.T) {
	f := newFixture(t)
	src := f.source(nil)
	if !src.Status().LoginNeeded {
		t.Fatal("no login needed")
	}
	src.tellLogin()
	if len(src.notices) != 1 || !src.notices[0].login || !strings.HasPrefix(src.notices[0].ru, "Нужен вход в HSE App X") {
		t.Fatal(src.notices)
	}
	restarted := f.source(nil)
	restarted.tellLogin() // told already
	if len(restarted.notices) != 0 || len(src.notices) != 1 {
		t.Fatal(restarted.notices)
	}

	if err := src.SetEnabled(false); err != nil { // off: no login needed, and nothing to say
		t.Fatal(err)
	}
	if src.Status().LoginNeeded || len(src.notices) != 1 || !woken(src.HseApp) {
		t.Fatal(src.notices)
	}
	if err := src.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if len(src.notices) != 2 || !src.notices[1].login {
		t.Fatal(src.notices)
	}
}

func TestPKCELogin(t *testing.T) {
	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	ctx := context.Background()
	link := src.LoginLink(false)
	q := query(t, link)
	if !strings.HasPrefix(link, AuthURL+"?client_id=") || !strings.Contains(link, "&scope=profile+email&") {
		t.Fatal(link)
	}
	if got := [5]string{q["client_id"], q["redirect_uri"], q["response_type"], q["scope"], q["code_challenge_method"]}; got != [5]string{ClientID, callback, "code", "profile email", "S256"} {
		t.Fatal(got)
	}
	fake.codes["code-1"] = q["code_challenge"]

	wantErr(t, src.Login(ctx, callback+"?state=forged&code=code-1"), "out of date")
	wantErr(t, src.Login(ctx, "https://example.org/somewhere"), "has no code")
	err := src.Login(ctx, callback+"?error=access_denied&error_description=Login+was+cancelled&state="+q["state"])
	if err == nil || err.Error() != "HSE did not let you in: access_denied" { // D79: no text of the link
		t.Fatal(err)
	}
	if err := src.Login(ctx, "  "+callback+"?state="+q["state"]+"&session_state=s&code=code-1 "); err != nil {
		t.Fatal(err)
	}
	if !src.Auth.LoggedIn() || !reflect.DeepEqual(fake.redirects, []string{callback}) {
		t.Fatal(fake.redirects)
	}
	wantErr(t, src.Login(ctx, callback+"?state="+q["state"]+"&code=code-1"), "out of date") // a state works once

	// The fallback: the app's own address, pasted by hand; the code exchange names the same address.
	appQ := query(t, src.LoginLink(true))
	if appQ["redirect_uri"] != AppRedirectURI {
		t.Fatal(appQ)
	}
	fake.codes["code-2"] = appQ["code_challenge"]
	if err := src.Login(ctx, AppRedirectURI+"?state="+appQ["state"]+"&code=code-2"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.redirects, []string{callback, AppRedirectURI}) {
		t.Fatal(fake.redirects)
	}

	// a code Keycloak does not know
	err = src.Login(ctx, callback+"?state="+query(t, src.LoginLink(false))["state"]+"&code=unknown")
	wantErr(t, err, "HSE did not take the code")
	var phrase *i18n.Error
	if !errors.As(err, &phrase) || !strings.HasPrefix(phrase.Phrase.Ru, "ВШЭ не приняла код (invalid_grant)") {
		t.Fatal(err)
	}
}

func TestOnlyTheLastFiveLinksWork(t *testing.T) {
	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	var links []map[string]string
	for range 6 {
		links = append(links, query(t, src.LoginLink(false)))
	}
	wantErr(t, src.Login(context.Background(), callback+"?state="+links[0]["state"]+"&code=c"), "out of date")
	fake.codes["c"] = links[1]["code_challenge"]
	if err := src.Login(context.Background(), callback+"?state="+links[1]["state"]+"&code=c"); err != nil {
		t.Fatal(err)
	}
	if !src.Auth.LoggedIn() {
		t.Fatal("not logged in")
	}
}

func TestLoginServerDown(t *testing.T) {
	f := newFixture(t)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // nothing listens there
	src := f.source(nil)
	src.Auth.TokenURL = down.URL + "/token"
	state := query(t, src.LoginLink(false))["state"]
	wantErr(t, src.Login(context.Background(), callback+"?state="+state+"&code=c"),
		"the HSE login server is unavailable")
	if src.Auth.LoggedIn() {
		t.Fatal("logged in")
	}
}

func TestAPIFailureIsToldAfterThreeHours(t *testing.T) {
	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	seedDefault(t, src)
	ctx := context.Background()
	src.Poll(ctx)
	fake.status = 503
	if src.Poll(ctx) {
		t.Fatal("poll went through")
	}
	if src.err != "HTTP 503 Service Unavailable" || len(src.notices) != 0 || src.okAt != "2026-10-20T12:00:00" {
		t.Fatal(src.err, src.notices, src.okAt)
	}
	f.moment = f.moment.Add(3 * time.Hour)
	src.Poll(ctx)
	src.Poll(ctx)
	if len(src.notices) != 1 || src.notices[0].login ||
		!strings.HasPrefix(src.notices[0].ru, "HSE App X недоступен с 20.10 12:00: HTTP 503 Service Unavailable.") {
		t.Fatal(src.notices)
	}
	fake.status = 200
	if !src.Poll(ctx) {
		t.Fatal("poll failed")
	}
	if src.notices[len(src.notices)-1] != (notice{"HSE App X снова доступен.", false}) || src.err != "" {
		t.Fatal(src.notices)
	}
	if len(src.polls) != 2 { // failed polls hand nothing over: the lists stay
		t.Fatal(len(src.polls))
	}
}

func TestRunPollsOnlyWhenOnAndLoggedIn(t *testing.T) {
	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	var naps []time.Duration
	stop := errors.New("stop")
	src.nap = func(_ context.Context, d time.Duration) error {
		naps = append(naps, d)
		switch len(naps) {
		case 1:
			src.SetEnabled(false)
		case 2:
			seedDefault(t, src)
		case 3:
			src.SetEnabled(true)
		default:
			return stop
		}
		return nil
	}
	if err := src.Run(context.Background()); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	// on without a login: the notice, no poll; off; off and logged in: still no poll; on and logged in: a poll
	if !reflect.DeepEqual(naps, []time.Duration{PollEvery, PollEvery, PollEvery, PollEvery}) ||
		len(src.polls) != 1 || len(src.notices) != 1 {
		t.Fatal(naps, src.polls, src.notices)
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	f := newFixture(t)
	src := f.source(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := src.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// --- the callback ----------------------------------------------------------------------

func callPage(t *testing.T, h http.HandlerFunc, target string) (int, http.Header, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	h(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder.Code, recorder.Header(), recorder.Body.String()
}

func TestCallbackNeedsNoSession(t *testing.T) {
	f, fake := newFixture(t), newFake(t)
	src := f.source(fake)
	link := query(t, src.LoginLink(false))
	fake.codes["code-1"] = link["code_challenge"]

	status, headers, text := callPage(t, src.Callback, CallbackPath+"?state=forged&code=code-1")
	if status != 400 || headers.Get("Cache-Control") != "no-store" || headers.Get("Referrer-Policy") != "no-referrer" ||
		headers.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal(status, headers)
	}
	if !strings.Contains(text, `<html lang="en">`) || !strings.Contains(text, "The login failed") ||
		!strings.Contains(text, "<p>This login link is out of date: open a new one in Machine → HSE App X.</p>") {
		t.Fatal(text)
	}
	if _, err := f.settings.Set("language", "ru"); err != nil { // the page in the web app's language (D41)
		t.Fatal(err)
	}
	_, _, text = callPage(t, src.Callback, CallbackPath+"?state=forged&code=code-1")
	if !strings.Contains(text, "Вход не удался") ||
		!strings.Contains(text, "Эта ссылка входа устарела: откройте новую в «Машина → HSE App X».") {
		t.Fatal(text)
	}
	status, _, text = callPage(t, src.Callback, CallbackPath+
		"?error=access_denied&error_description=%3Cscript%3Ex%3C/script%3E&state="+link["state"])
	if status != 400 || strings.Contains(text, "script") || !strings.Contains(text, "access_denied") {
		t.Fatal(status, text)
	}
	if src.Auth.LoggedIn() {
		t.Fatal("logged in")
	}

	status, _, text = callPage(t, src.Callback, CallbackPath+"?state="+link["state"]+"&session_state=s&code=code-1")
	if status != 200 || !strings.Contains(text, "<h1>Вход в HSE App X выполнен</h1>") {
		t.Fatal(status, text)
	}
	if !src.Auth.LoggedIn() || !reflect.DeepEqual(fake.redirects, []string{callback}) {
		t.Fatal(fake.redirects)
	}
	if len(src.notices) != 0 || !woken(src.HseApp) { // no login was asked for: nothing to say
		t.Fatal(src.notices)
	}
}

func TestPageEscapesAsPython(t *testing.T) {
	recorder := httptest.NewRecorder()
	page(recorder, "en", `a "b" & 'c'`, "<x>", "-", 200)
	if !strings.Contains(recorder.Body.String(), "<h1>a &quot;b&quot; &amp; &#x27;c&#x27;</h1>\n<p>&lt;x&gt;</p>") {
		t.Fatal(recorder.Body.String())
	}
}

func TestStatusNeverHoldsTheTokens(t *testing.T) {
	f := newFixture(t)
	src := f.source(nil)
	seed(t, src, "refresh-secret-1", "access-secret-0", 3600)
	data, _ := json.Marshal(src.Status())
	if !src.Status().LoggedIn || strings.Contains(string(data), "secret") {
		t.Fatal(string(data))
	}
	f.moment = f.moment.Add(22 * 24 * time.Hour) // three weeks without a refresh
	if !src.Status().LoginNeeded || !src.Auth.Ever() {
		t.Fatal("still logged in")
	}
}

func TestDueIsAMoscowDate(t *testing.T) {
	for text, want := range map[string]time.Time{
		"2026-10-31T20:59:00Z":      day(2026, 10, 31),
		"2026-10-31T21:00:00+00:00": day(2026, 11, 1),
		"2026-10-31":                day(2026, 10, 31),
		"2026-10-31 20:59:00.5Z":    day(2026, 10, 31),
	} {
		got, err := DueDate(text)
		if err != nil || !got.Equal(want) {
			t.Fatal(text, got, err)
		}
	}
	if _, err := DueDate("завтра"); err == nil {
		t.Fatal("parsed")
	}
}

func TestDescribe(t *testing.T) {
	if got := Describe(valueError("ValueError", "unexpected answer of /v2/deadlines")); got != "ValueError: unexpected answer of /v2/deadlines" {
		t.Fatal(got)
	}
	if got := Describe(&pyError{name: "TimeoutError"}); got != "TimeoutError" {
		t.Fatal(got)
	}
}
