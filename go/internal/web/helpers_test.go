package web

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/connector"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	hsedl "github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/news"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	testOrigin = "https://infomaster.example.ts.net"
	testRPID   = "infomaster.example.ts.net"
	iphone     = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) " +
		"Version/18.1 Mobile/15E148 Safari/604.1"
	mac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/131.0.0.0 Safari/537.36"
	testChat = int64(-1001)
)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testEnv() config.Env { return config.Env{PublicURL: testOrigin, APIToken: "tok"} }

// seconds is a settable unix clock.
type seconds struct {
	mu    sync.Mutex
	value float64
}

func (s *seconds) now() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}

func (s *seconds) add(d float64) {
	s.mu.Lock()
	s.value += d
	s.mu.Unlock()
}

// moment is a settable clock of the timetable: Tuesday 20.10.2026, noon in Moscow.
type moment struct {
	mu sync.Mutex
	at time.Time
}

func (m *moment) now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.at
}

func (m *moment) set(hour, minute int) {
	m.mu.Lock()
	m.at = time.Date(2026, 10, 20, hour, minute, 0, 0, clock.TZ).UTC()
	m.mu.Unlock()
}

type options struct {
	lang      string
	screens   bool // the timetable of the fixture, Data
	connector bool
	sendCode  func(ctx context.Context, text string) error
	noBot     bool
	data      func(h *harness, d *Data)
	model     llm.Model
}

type harness struct {
	t         *testing.T
	store     *store.Store
	app       *WebApp
	mux       *http.ServeMux
	handler   http.Handler
	secs      *seconds
	clock     *moment
	sch       *schedule.Schedule
	merged    *merge.DeadlineList
	news      *news.News
	connector *connector.Connector
	token     string

	mu   sync.Mutex
	sent []string
}

func (h *harness) texts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent...)
}

func newHarness(t *testing.T, o options) *harness {
	t.Helper()
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	h := &harness{t: t, store: s, secs: &seconds{value: 1_800_000_000}, clock: &moment{}}
	h.clock.set(12, 0)
	if o.lang == "" {
		o.lang = "en"
	}
	lang := func() string { return o.lang }
	send := o.sendCode
	if send == nil && !o.noBot {
		send = func(_ context.Context, text string) error {
			h.mu.Lock()
			h.sent = append(h.sent, text)
			h.mu.Unlock()
			return nil
		}
	}
	var data *Data
	if o.screens {
		r := ruz.New("", s)
		r.Now = h.clock.now
		sub := subjects.New(s, &llm.Fake{})
		h.sch = schedule.New(s, r, events.Open(s), deadlines.Open(s), sub, inbox.Open(s))
		h.sch.Now = h.clock.now
		hse := hsedl.New(s, sub, &llm.Fake{})
		hse.Now = h.clock.now
		h.merged = merge.New(s, h.sch.Deadlines, hse, sub)
		h.merged.Now = h.clock.now
		must(r.Sync(must(ruz.Parse(must(os.ReadFile("testdata/ruz.ics")))), false))
		data = NewData(s, h.sch, h.merged)
		data.Lang = lang
		h.news = news.Open(s)
		if o.data != nil {
			o.data(h, data)
		}
	}
	h.app = must(New(testEnv(), s, send, lang, h.secs.now, data, o.model, ""))
	if o.noBot {
		h.app.Auth.SendCode = nil
	}
	if o.connector {
		h.connector = must(connector.New(testEnv(), s, h.news, nil, lang, h.clock.now, h.secs.now))
		h.app.Connector = h.connector
	}
	h.mux = http.NewServeMux()
	return h
}

// serve attaches the app (after the tools are registered).
func (h *harness) serve() *harness {
	h.app.Attach(h.mux)
	h.handler = h.app.Wrap(httpx.Router(h.mux))
	return h
}

type req struct {
	token  string
	origin *string
	agent  string
}

func noOrigin() *string { return new(string) }

// call is a request of the app: its origin, the session's cookie by hand.
func (h *harness) call(method, path string, body any, r req) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		noErr(h.t, err)
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, "/web/api"+path, reader)
	if r.agent == "" {
		r.agent = iphone
	}
	request.Header.Set("User-Agent", r.agent)
	if r.origin == nil {
		request.Header.Set("Origin", testOrigin)
	} else if *r.origin != "" {
		request.Header.Set("Origin", *r.origin)
	}
	if r.token != "" {
		request.Header.Set("Cookie", Cookie+"="+r.token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	return recorder
}

func (h *harness) get(path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
	return recorder
}

var codePattern = regexp.MustCompile(`^(\d{6}) `)

func (h *harness) code() string {
	texts := h.texts()
	return codePattern.FindStringSubmatch(texts[len(texts)-1])[1]
}

func tokenOf(r *httptest.ResponseRecorder) string {
	for _, cookie := range r.Result().Cookies() {
		if cookie.Name == Cookie {
			return cookie.Value
		}
	}
	return ""
}

func (h *harness) logIn(agent string) string {
	h.t.Helper()
	h.secs.add(ResendAfter)
	if r := h.call("POST", "/login/code", map[string]any{}, req{}); r.Code != 200 {
		h.t.Fatalf("code: %d %s", r.Code, r.Body)
	}
	r := h.call("POST", "/login/code/check", map[string]any{"code": h.code()}, req{agent: agent})
	if r.Code != 200 {
		h.t.Fatalf("check: %d %s", r.Code, r.Body)
	}
	return tokenOf(r)
}

// in logs a device in and keeps its token.
func (h *harness) in() *harness {
	h.token = h.logIn(iphone)
	return h
}

func decode(t *testing.T, r *httptest.ResponseRecorder) any {
	t.Helper()
	value, err := httpx.Decode(r.Body.Bytes())
	if err != nil {
		t.Fatalf("not JSON (%d): %s", r.Code, r.Body)
	}
	return value
}

// ask is a request with the session; it must answer status.
func (h *harness) ask(method, path string, body any, status int) map[string]any {
	h.t.Helper()
	r := h.call(method, path, body, req{token: h.token})
	if r.Code != status {
		h.t.Fatalf("%s %s: %d, want %d: %s", method, path, r.Code, status, r.Body)
	}
	value, _ := decode(h.t, r).(map[string]any)
	return value
}

func (h *harness) read(path string) map[string]any { return h.ask("GET", path, nil, 200) }

// addRow is a chat's deadline of the fixture's chat (test_http.add_row).
func (h *harness) addRow(label, due, source, course string) int64 {
	if source == "" {
		source = "tg"
	}
	return must(h.sch.Deadlines.Add(deadlines.New{Source: source, ChatID: ptr(testChat), MsgIDs: []int64{1},
		Course: course, Label: label, Due: due}, h.clock.now()))
}

func ptr[T any](v T) *T { return &v }

func list(value any) []any {
	l, _ := value.([]any)
	return l
}

func obj(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

// stub registers a write tool whose run is given; its schema takes any of the names given.
func stub(t *testing.T, c *connector.Connector, name string, args []string, run tools.Run) {
	t.Helper()
	properties := map[string]any{}
	for _, arg := range args {
		properties[arg] = map[string]any{}
	}
	noErr(t, c.Registry.Add(tools.Tool{Name: name, Title: name, Schema: tools.Schema(properties), Run: run, Write: true}))
}

// calls records what the stub tools were called with.
type calls struct {
	mu   sync.Mutex
	list []map[string]any
}

func (c *calls) add(tool string, args map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copied := map[string]any{"_tool": tool}
	for k, v := range args {
		copied[k] = v
	}
	c.list = append(c.list, copied)
}

func (c *calls) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.list) == 0 {
		return nil
	}
	return c.list[len(c.list)-1]
}

// recorder is a stub that records its call and answers a change with an undo of op.
func (c *calls) recorder(tool string, undo bool) tools.Run {
	return func(_ *tools.Context, args map[string]any) (tools.Output, error) {
		c.add(tool, args)
		out := tools.Output{Summary: "did " + tool}
		if undo {
			out.Undo = []tools.Step{{Tool: "_noop", Arguments: map[string]any{}}}
		}
		return out, nil
	}
}

func refuse(text string) tools.Run {
	return func(*tools.Context, map[string]any) (tools.Output, error) {
		return tools.Output{}, &tools.ToolError{Msg: text}
	}
}

var errTest = errors.New("test")

func contains(s, part string) bool { return strings.Contains(s, part) }

func inboxFields(sender, text string, at time.Time) inbox.Fields {
	return inbox.Fields{SenderID: ptr(int64(5)), SenderName: &sender, At: at, Text: text}
}

func ecdhCurve() elliptic.Curve { return elliptic.P256() }

func errorsAs(err error, target any) bool { return errors.As(err, target) }

func errorOf(body string) string {
	var answer struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &answer)
	return answer.Error
}
