package connector_test

// The MCP connector (§8.5): OAuth for one owner, the JSON-RPC endpoint, the journal with its undo (the protocol,
// OAuth and journal parts of tests/test_connector.py). The catalog's tools are not here: a few stub tools stand in
// for them.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/connector"
	"github.com/IvanIkra/infomaster/go/internal/connector/changes"
	"github.com/IvanIkra/infomaster/go/internal/connector/mcp"
	"github.com/IvanIkra/infomaster/go/internal/connector/oauth"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	publicURL = "https://infomaster.example.ts.net"
	callback  = "https://claude.ai/api/mcp/auth_callback"
)

var legacyHeaders = map[string]string{"MCP-Protocol-Version": "2025-06-18",
	"Accept": "application/json, text/event-stream"}

// app is the stand-in of the public port's app: the own deadlines the stub tools change.
type app struct {
	mu        sync.Mutex
	next      int
	deadlines map[string]string // uid -> title
	assumed   []any
}

type seconds struct{ value float64 }

func (s *seconds) now() float64 { return s.value }

type fixture struct {
	t         *testing.T
	conn      *connector.Connector
	app       *app
	server    *httptest.Server
	client    *http.Client
	seconds   *seconds
	sent      []string
	sentMu    sync.Mutex
	failSends bool
}

func stubTools(t *testing.T, r *tools.Registry) {
	t.Helper()
	appOf := func(c *tools.Context) *app { return c.App.(*app) }
	r.MustAdd(tools.Tool{Name: "get_status", Title: "Status of the sources",
		Description: "What works and what needs the owner.", Schema: tools.Schema(nil),
		Run: func(c *tools.Context, _ map[string]any) (tools.Output, error) {
			return tools.Result(httpx.Object{{Key: "untrusted", Value: tools.Untrusted},
				{Key: "needs_attention", Value: []string{}}, {Key: "limit", Value: pyjson.Float(0.5)}}), nil
		}})
	r.MustAdd(tools.Tool{Name: "list_deadlines", Title: "List the deadlines", Description: "The merged list.",
		Schema: tools.Schema(map[string]any{"days": map[string]any{"type": "integer", "minimum": 1, "maximum": 60}}),
		Run: func(c *tools.Context, _ map[string]any) (tools.Output, error) {
			a := appOf(c)
			a.mu.Lock()
			defer a.mu.Unlock()
			items := []any{}
			for uid, title := range a.deadlines {
				items = append(items, map[string]any{"uid": uid, "title": title})
			}
			return tools.Result(map[string]any{"items": items}), nil
		}})
	r.MustAdd(tools.Tool{Name: "add_deadline", Title: "Add a deadline", Description: "An own deadline.",
		Schema: tools.Schema(map[string]any{
			"title":    map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
			"due":      map[string]any{"type": "string"},
			"category": map[string]any{"type": "string", "enum": []any{"study", "personal"}},
		}, "title", "due"),
		Write: true, NonDestructive: true,
		Run: func(c *tools.Context, args map[string]any) (tools.Output, error) {
			a := appOf(c)
			a.mu.Lock()
			defer a.mu.Unlock()
			a.next++
			uid := fmt.Sprintf("ha-%d", a.next)
			title := args["title"].(string)
			a.deadlines[uid] = title
			return tools.Output{Data: httpx.Object{{Key: "uid", Value: uid}, {Key: "title", Value: title}},
				Summary: fmt.Sprintf("Added the deadline «%s», due %s", title, args["due"]),
				Undo:    []tools.Step{{Tool: "delete_deadline", Arguments: map[string]any{"uid": uid}}}}, nil
		}})
	r.MustAdd(tools.Tool{Name: "delete_deadline", Title: "Delete an own deadline", Description: "Gone for good.",
		Schema: tools.Schema(map[string]any{"uid": map[string]any{"type": "string"}}, "uid"), Write: true,
		Run: func(c *tools.Context, args map[string]any) (tools.Output, error) {
			a := appOf(c)
			a.mu.Lock()
			defer a.mu.Unlock()
			uid := args["uid"].(string)
			title, ok := a.deadlines[uid]
			if !ok {
				return tools.Output{}, tools.Errorf("no such deadline: %s", uid)
			}
			delete(a.deadlines, uid)
			return tools.Output{Data: "deleted", Summary: "Deleted «" + title + "»", Note: "it is gone for good"}, nil
		}})
	r.MustAdd(tools.Tool{Name: "hide_deadline", Title: "Hide a deadline", Description: "Hidden from the list.",
		Schema: tools.Schema(map[string]any{"uid": map[string]any{"type": "string"}}, "uid"), Write: true,
		Run: func(c *tools.Context, args map[string]any) (tools.Output, error) {
			return tools.Output{Data: map[string]any{"hidden": args["uid"]}, Summary: "Hid it",
				Undo: []tools.Step{{Tool: "_assume_again", Arguments: map[string]any{"deadline_id": int64(3),
					"sheet_id": nil, "due_at": "2026-10-25", "share": pyjson.Float(8)}}}}, nil
		}})
	r.MustAdd(tools.Tool{Name: "broken", Title: "Broken", Description: "Fails.", Schema: tools.Schema(nil),
		Run: func(*tools.Context, map[string]any) (tools.Output, error) { panic("boom") }})
	r.Op("_assume_again", func(c *tools.Context, args map[string]any) (tools.Output, error) {
		a := appOf(c)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.assumed = append(a.assumed, args)
		return tools.Output{}, nil
	})
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &fixture{t: t, app: &app{deadlines: map[string]string{}}, seconds: &seconds{1_800_000_000}}
	send := func(_ context.Context, text string) error {
		f.sentMu.Lock()
		defer f.sentMu.Unlock()
		if f.failSends {
			return errors.New("blocked")
		}
		f.sent = append(f.sent, text)
		return nil
	}
	now := func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ) }
	env := config.Env{APIToken: "tok", PublicURL: publicURL}
	conn, err := connector.New(env, s, nil, send, nil, now, f.seconds.now)
	if err != nil {
		t.Fatal(err)
	}
	stubTools(t, conn.Registry)
	mux := http.NewServeMux()
	conn.Attach(mux, f.app)
	f.conn = conn
	f.server = httptest.NewServer(httpx.Router(mux))
	t.Cleanup(f.server.Close)
	f.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f
}

type response struct {
	status int
	header http.Header
	body   string
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("%v: %q", err, r.body)
	}
	return out
}

func (f *fixture) do(method, path string, body io.Reader, headers map[string]string) response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Header, string(data)}
}

func (f *fixture) get(path string, query url.Values) response {
	if query != nil {
		path += "?" + query.Encode()
	}
	return f.do("GET", path, nil, nil)
}

func (f *fixture) postForm(path string, form url.Values) response {
	return f.do("POST", path, strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func (f *fixture) postJSON(path string, body any, headers map[string]string) response {
	data, _ := json.Marshal(body)
	all := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		all[k] = v
	}
	return f.do("POST", path, strings.NewReader(string(data)), all)
}

func pkce() (string, string) {
	verifier := oauth.TokenURLSafe(48)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (f *fixture) register(redirect string) string {
	f.t.Helper()
	r := f.postJSON("/oauth/register", map[string]any{"client_name": "Claude", "redirect_uris": []string{redirect},
		"grant_types": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_method": "none"}, nil)
	if r.status != 201 {
		f.t.Fatal(r)
	}
	return r.json(f.t)["client_id"].(string)
}

func pageID(t *testing.T, text string) string {
	t.Helper()
	_, rest, ok := strings.Cut(text, `name="page" value="`)
	if !ok {
		t.Fatal(text)
	}
	id, _, _ := strings.Cut(rest, `"`)
	return id
}

func (f *fixture) lastSent() string {
	f.sentMu.Lock()
	defer f.sentMu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

// authorize is the owner's way through the authorize page; the code of the redirect.
func (f *fixture) authorize(clientID, challenge, redirect string) string {
	t := f.t
	t.Helper()
	page := f.get("/oauth/authorize", url.Values{"response_type": {"code"}, "client_id": {clientID},
		"redirect_uri": {redirect}, "state": {"xyz"}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "resource": {publicURL + "/mcp"}})
	if page.status != 200 || !strings.Contains(page.body, "Claude") ||
		(redirect == callback && !strings.Contains(page.body, "<b>claude.ai</b>")) ||
		page.header.Get("X-Frame-Options") != "DENY" ||
		page.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal(page)
	}
	id := pageID(t, page.body)
	sent := f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"send"}})
	if sent.status != 200 || !strings.Contains(sent.body, `name="code"`) {
		t.Fatal(sent)
	}
	message := f.lastSent()
	code := strings.Fields(message)[0]
	if len(code) != 6 || !strings.Contains(message, "Claude") {
		t.Fatal(message)
	}
	wrong := f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"allow"}, "code": {"000000x"}})
	if wrong.status != 400 || !strings.Contains(wrong.body, "Wrong code") {
		t.Fatal(wrong)
	}
	done := f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"allow"}, "code": {" " + code[:3] + " " + code[3:]}})
	if done.status != 303 {
		t.Fatal(done)
	}
	location, err := url.Parse(done.header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Scheme+"://"+location.Host+location.Path != strings.Split(redirect, "?")[0] {
		t.Fatal(location)
	}
	query := location.Query()
	if query.Get("state") != "xyz" || query.Get("iss") != publicURL {
		t.Fatal(query)
	}
	if !strings.HasSuffix(done.header.Get("Location"), "&state=xyz&iss=https%3A%2F%2Finfomaster.example.ts.net") {
		t.Fatal(done.header.Get("Location")) // Python's urlencode: in order, quote_plus
	}
	return query.Get("code")
}

type tokens struct {
	Access, Refresh, ClientID string
}

func (f *fixture) connect() tokens {
	f.t.Helper()
	clientID := f.register(callback)
	verifier, challenge := pkce()
	code := f.authorize(clientID, challenge, callback)
	r := f.postForm("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {callback}, "client_id": {clientID}, "code_verifier": {verifier},
		"resource": {publicURL + "/mcp"}})
	if r.status != 200 || r.header.Get("Cache-Control") != "no-store" {
		f.t.Fatal(r)
	}
	body := r.json(f.t)
	return tokens{body["access_token"].(string), body["refresh_token"].(string), clientID}
}

func (f *fixture) rpc(token, method string, params any, id any, headers map[string]string) response {
	f.t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		body["id"] = id
	}
	if params != nil {
		body["params"] = params
	}
	all := map[string]string{"Authorization": "Bearer " + token}
	if headers == nil {
		headers = legacyHeaders
	}
	for k, v := range headers {
		all[k] = v
	}
	return f.postJSON("/mcp", body, all)
}

func (f *fixture) call(token, name string, arguments map[string]any) map[string]any {
	f.t.Helper()
	if arguments == nil {
		arguments = map[string]any{}
	}
	r := f.rpc(token, "tools/call", map[string]any{"name": name, "arguments": arguments}, 1, nil)
	if r.status != 200 {
		f.t.Fatal(r)
	}
	body := r.json(f.t)
	if _, failed := body["error"]; failed {
		f.t.Fatal(body)
	}
	return body["result"].(map[string]any)
}

func text(result map[string]any) string {
	return result["content"].([]any)[0].(map[string]any)["text"].(string)
}

func data(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	if result["isError"] != false {
		t.Fatal(text(result))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text(result)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// --- OAuth ---------------------------------------------------------------------------------------

func TestMetadataAndNoToken(t *testing.T) {
	f := setup(t)
	resource := f.get("/.well-known/oauth-protected-resource/mcp", nil)
	got := resource.json(t)
	if got["resource"] != publicURL+"/mcp" || fmt.Sprint(got["authorization_servers"]) != "["+publicURL+"]" {
		t.Fatal(got)
	}
	if f.get("/.well-known/oauth-protected-resource", nil).body != resource.body {
		t.Fatal("the root path differs")
	}
	server := f.get("/.well-known/oauth-authorization-server", nil).json(t)
	if server["issuer"] != publicURL || fmt.Sprint(server["code_challenge_methods_supported"]) != "[S256]" ||
		!strings.HasSuffix(server["registration_endpoint"].(string), "/oauth/register") ||
		server["authorization_response_iss_parameter_supported"] != true ||
		fmt.Sprint(server["token_endpoint_auth_methods_supported"]) != "[none]" {
		t.Fatal(server)
	}
	for _, method := range []string{"POST", "GET", "DELETE"} {
		r := f.do(method, "/mcp", strings.NewReader(`{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}`), nil)
		if r.status != 401 || r.header.Get("WWW-Authenticate") !=
			`Bearer resource_metadata="https://infomaster.example.ts.net/.well-known/oauth-protected-resource/mcp"` ||
			r.body != `{"error": "unauthorized"}` {
			t.Fatal(method, r)
		}
	}
	r := f.rpc("made-up", "tools/list", nil, 1, nil)
	if r.status != 401 || !strings.Contains(r.header.Get("WWW-Authenticate"), `error="invalid_token"`) ||
		r.json(t)["error"] != "invalid_token" {
		t.Fatal(r)
	}
}

func TestRegistration(t *testing.T) {
	f := setup(t)
	for _, redirect := range []string{"https://evil.example/cb", "http://claude.ai/api/mcp/auth_callback",
		"javascript:alert(1)", "https://claude.ai/chat/x", "https://claude.com/api/mcp/auth_callback/../x",
		"https://claude.ai:443/api/mcp/auth_callback", "https://user@claude.ai/api/mcp/auth_callback",
		"https://claude.ai/api/mcp/auth_callback#x", "http://localhost:99999/cb"} {
		r := f.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{redirect}}, nil)
		if r.status != 400 || r.json(t)["error"] != "invalid_redirect_uri" {
			t.Fatal(redirect, r)
		}
	}
	if r := f.do("POST", "/oauth/register", strings.NewReader("not json"), nil); r.status != 400 ||
		r.json(t)["error"] != "invalid_client_metadata" || r.header.Get("Pragma") != "no-cache" {
		t.Fatal(r)
	}
	for _, bad := range []map[string]any{{"redirect_uris": []string{callback}, "grant_types": []string{"implicit"}},
		{"redirect_uris": []string{callback}, "response_types": []string{"token"}}, {"redirect_uris": []any{}}} {
		if r := f.postJSON("/oauth/register", bad, nil); r.status != 400 {
			t.Fatal(bad, r)
		}
	}
	if f.register("http://localhost:3118/callback") == "" {
		t.Fatal("no loopback client")
	}
	// The name: collapsed and cut, or the host of the first address.
	r := f.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{"http://[::1]:5000/cb"}}, nil)
	if r.status != 201 || r.json(t)["client_name"] != "::1" || r.json(t)["token_endpoint_auth_method"] != "none" {
		t.Fatal(r)
	}
	r = f.postJSON("/oauth/register", map[string]any{"redirect_uris": []string{callback},
		"client_name": "  Claude \x1f  Code " + strings.Repeat("я", 100)}, nil)
	if name := r.json(t)["client_name"].(string); len([]rune(name)) != 80 || !strings.HasPrefix(name, "Claude Code яя") {
		t.Fatal(name)
	}
	// The body is read whole only up to 2 MiB.
	if r := f.do("POST", "/oauth/register", strings.NewReader(strings.Repeat(" ", httpx.BodyMax+1)), nil); r.status != 413 {
		t.Fatal(r.status)
	}
}

func TestAuthorizeRefuses(t *testing.T) {
	f := setup(t)
	clientID := f.register(callback)
	_, challenge := pkce()
	base := func(change map[string]string) url.Values {
		q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {callback},
			"state": {"s"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
		for k, v := range change {
			q.Set(k, v)
		}
		return q
	}
	// An unknown client or another redirect: an error page, never a redirect.
	for _, wrong := range []map[string]string{{"client_id": "nope"}, {"redirect_uri": "https://claude.ai/other"}} {
		if r := f.get("/oauth/authorize", base(wrong)); r.status != 400 || r.header.Get("Location") != "" {
			t.Fatal(wrong, r)
		}
	}
	// The rest goes back to the client with the error.
	for _, c := range []struct {
		wrong map[string]string
		error string
	}{{map[string]string{"code_challenge_method": "plain"}, "invalid_request"},
		{map[string]string{"response_type": "token"}, "unsupported_response_type"},
		{map[string]string{"resource": "https://other.example/mcp"}, "invalid_target"},
		{map[string]string{"code_challenge": "short"}, "invalid_request"}} {
		r := f.get("/oauth/authorize", base(c.wrong))
		if r.status != 302 || r.body != "302: Found" {
			t.Fatal(c, r)
		}
		location, _ := url.Parse(r.header.Get("Location"))
		if location.Query().Get("error") != c.error || location.Query().Get("state") != "s" {
			t.Fatal(c, location)
		}
	}
	// The resource may name the server, any case, a trailing slash.
	if r := f.get("/oauth/authorize", base(map[string]string{"resource": "HTTPS://infomaster.example.ts.net/"})); r.status != 200 {
		t.Fatal(r)
	}
	page := f.get("/oauth/authorize", base(nil))
	id := pageID(t, page.body)
	early := f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"allow"}, "code": {"123456"}})
	if early.status != 400 || !strings.Contains(early.body, "Ask for a code first") {
		t.Fatal(early)
	}
	denied := f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"deny"}})
	location, _ := url.Parse(denied.header.Get("Location"))
	if denied.status != 303 || location.Query().Get("error") != "access_denied" {
		t.Fatal(denied)
	}
	if r := f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"send"}}); r.status != 400 {
		t.Fatal(r)
	}
	if f.lastSent() != "" {
		t.Fatal("a code was sent")
	}
}

func TestCodeAttempts(t *testing.T) {
	f := setup(t)
	clientID := f.register(callback)
	_, challenge := pkce()
	page := f.get("/oauth/authorize", url.Values{"response_type": {"code"}, "client_id": {clientID},
		"redirect_uri": {callback}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}})
	id := pageID(t, page.body)
	f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"send"}})
	code := strings.Fields(f.lastSent())[0]
	// A second code within 30 seconds is refused.
	if r := f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"send"}}); !strings.Contains(r.body, "a moment ago") {
		t.Fatal(r)
	}
	var r response
	for i := 0; i < oauth.Attempts; i++ {
		r = f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"allow"}, "code": {"x"}})
	}
	if !strings.Contains(strings.ToLower(r.body), "too many") {
		t.Fatal(r)
	}
	late := f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"allow"}, "code": {code}})
	if late.status != 400 { // that code is spent: a new one is needed
		t.Fatal(late)
	}
}

func TestCodeLimitsAndTheBot(t *testing.T) {
	f := setup(t)
	clientID := f.register(callback)
	_, challenge := pkce()
	open := func() string {
		page := f.get("/oauth/authorize", url.Values{"response_type": {"code"}, "client_id": {clientID},
			"redirect_uri": {callback}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}})
		return pageID(t, page.body)
	}
	f.failSends = true
	if r := f.postForm("/oauth/authorize", url.Values{"page": {open()}, "action": {"send"}}); !strings.Contains(r.body, "could not write to you") {
		t.Fatal(r)
	}
	f.failSends = false
	for i := 1; i < oauth.CodesPerHour; i++ {
		f.postForm("/oauth/authorize", url.Values{"page": {open()}, "action": {"send"}})
	}
	if r := f.postForm("/oauth/authorize", url.Values{"page": {open()}, "action": {"send"}}); !strings.Contains(r.body, "Too many codes") {
		t.Fatal(r)
	}
	// Pages expire after 10 minutes.
	id := open()
	f.seconds.value += oauth.PageTTL + 1
	if r := f.postForm("/oauth/authorize", url.Values{"page": {id}, "action": {"send"}}); r.status != 400 ||
		!strings.Contains(r.body, "This page has expired") {
		t.Fatal(r)
	}
}

func TestTokenFlow(t *testing.T) {
	f := setup(t)
	tk := f.connect()
	if r := f.rpc(tk.Access, "ping", nil, 1, nil); r.status != 200 {
		t.Fatal(r)
	}
	// The refresh token rotates; the one just used still works a moment (two refreshes in flight), then not.
	refreshed := f.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {tk.ClientID},
		"refresh_token": {tk.Refresh}}).json(t)
	if refreshed["refresh_token"] == tk.Refresh || refreshed["token_type"] != "Bearer" ||
		refreshed["expires_in"] != float64(oauth.AccessTTL) {
		t.Fatal(refreshed)
	}
	again := f.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tk.Refresh}})
	if again.status != 200 {
		t.Fatal(again)
	}
	f.seconds.value += oauth.RefreshGrace + 1
	late := f.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tk.Refresh}})
	if late.status != 400 || late.json(t)["error"] != "invalid_grant" {
		t.Fatal(late)
	}
	// An access token lives an hour.
	f.seconds.value += oauth.AccessTTL
	if r := f.rpc(refreshed["access_token"].(string), "ping", nil, 1, nil); r.status != 401 {
		t.Fatal(r)
	}
	newest := f.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"},
		"refresh_token": {again.json(t)["refresh_token"].(string)}}).json(t)
	if r := f.rpc(newest["access_token"].(string), "ping", nil, 1, nil); r.status != 200 {
		t.Fatal(r)
	}
	// Revoking the refresh token ends the connection.
	if r := f.postForm("/oauth/revoke", url.Values{"token": {newest["refresh_token"].(string)}}); r.status != 200 ||
		r.body != "" || r.header.Get("Cache-Control") != "no-store" {
		t.Fatal(r)
	}
	if r := f.rpc(newest["access_token"].(string), "ping", nil, 1, nil); r.status != 401 {
		t.Fatal(r)
	}
}

func TestTokenRefuses(t *testing.T) {
	f := setup(t)
	clientID := f.register(callback)
	verifier, challenge := pkce()
	code := f.authorize(clientID, challenge, callback)
	form := func(change map[string]string) url.Values {
		v := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback},
			"client_id": {clientID}, "code_verifier": {verifier}}
		for k, x := range change {
			v.Set(k, x)
		}
		return v
	}
	other, _ := pkce()
	for _, wrong := range []map[string]string{{"code_verifier": other}, {"client_id": "other"},
		{"redirect_uri": callback + "x"}, {"resource": "https://other.example"}} {
		if r := f.postForm("/oauth/token", form(wrong)); r.status != 400 || r.header.Get("Cache-Control") != "no-store" {
			t.Fatal(wrong, r)
		}
	}
	// A code is spent by its first try, right or wrong.
	if r := f.postForm("/oauth/token", form(nil)); r.status != 400 {
		t.Fatal(r)
	}
	if r := f.postForm("/oauth/token", form(map[string]string{"grant_type": "password"})); r.status != 400 ||
		r.json(t)["error"] != "unsupported_grant_type" {
		t.Fatal(r)
	}
	if r := f.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"nope"}}); r.status != 400 {
		t.Fatal(r)
	}
}

func TestBasicClientID(t *testing.T) {
	f := setup(t)
	clientID := f.register(callback)
	verifier, challenge := pkce()
	code := f.authorize(clientID, challenge, callback)
	basic := base64.StdEncoding.EncodeToString([]byte(clientID + ":"))
	r := f.do("POST", "/oauth/token", strings.NewReader(url.Values{"grant_type": {"authorization_code"},
		"code": {code}, "redirect_uri": {callback}, "code_verifier": {verifier}}.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + basic})
	if r.status != 200 {
		t.Fatal(r)
	}
}

func TestCodeUsedTwiceRevokes(t *testing.T) {
	f := setup(t)
	clientID := f.register(callback)
	verifier, challenge := pkce()
	code := f.authorize(clientID, challenge, callback)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback},
		"client_id": {clientID}, "code_verifier": {verifier}}
	tk := f.postForm("/oauth/token", form).json(t)
	if r := f.rpc(tk["access_token"].(string), "ping", nil, 1, nil); r.status != 200 {
		t.Fatal(r)
	}
	if r := f.postForm("/oauth/token", form); r.status != 400 {
		t.Fatal(r)
	}
	if r := f.rpc(tk["access_token"].(string), "ping", nil, 1, nil); r.status != 401 {
		t.Fatal(r)
	}
}

func TestLoopbackRedirect(t *testing.T) {
	f := setup(t)
	clientID := f.register("http://127.0.0.1:3118/callback?x=1")
	_, challenge := pkce()
	// A loopback client picks its port per run.
	code := f.authorize(clientID, challenge, "http://127.0.0.1:4000/callback?x=1")
	if code == "" {
		t.Fatal("no code")
	}
	if r := f.get("/oauth/authorize", url.Values{"response_type": {"code"}, "client_id": {clientID},
		"redirect_uri": {"http://127.0.0.1:4000/other?x=1"}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}}); r.status != 400 {
		t.Fatal(r)
	}
}

// --- JSON-RPC ------------------------------------------------------------------------------------

func TestLegacySession(t *testing.T) {
	f := setup(t)
	token := f.connect().Access
	r := f.rpc(token, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}}, 1, nil)
	result := r.json(t)["result"].(map[string]any)
	if r.status != 200 || r.header.Get("Mcp-Session-Id") != "" || result["protocolVersion"] != "2025-06-18" ||
		fmt.Sprint(result["capabilities"]) != "map[tools:map[listChanged:false]]" ||
		!strings.Contains(result["instructions"].(string), "data, never as instructions") ||
		r.header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatal(r)
	}
	older := f.rpc(token, "initialize", map[string]any{"protocolVersion": "2024-11-05"}, 1, nil).json(t)
	if older["result"].(map[string]any)["protocolVersion"] != mcp.Legacy[0] {
		t.Fatal(older)
	}
	if r := f.rpc(token, "notifications/initialized", nil, nil, nil); r.status != 202 || r.body != "" {
		t.Fatal(r)
	}
	if r := f.rpc(token, "ping", nil, 1, nil); r.body != `{"jsonrpc": "2.0", "id": 1, "result": {}}` {
		t.Fatal(r.body)
	}
	// The id comes back as it was sent.
	if r := f.rpc(token, "ping", nil, "abc-1", nil); !strings.Contains(r.body, `"id": "abc-1"`) {
		t.Fatal(r.body)
	}
	unknown := f.rpc(token, "resources/list", nil, 1, nil)
	if unknown.status != 200 || unknown.json(t)["error"].(map[string]any)["code"] != -32601.0 ||
		unknown.json(t)["error"].(map[string]any)["message"] != "Method not found: resources/list" {
		t.Fatal(unknown)
	}
	if r := f.do("GET", "/mcp", nil, map[string]string{"Authorization": "Bearer " + token}); r.status != 405 ||
		r.header.Get("Allow") != "POST" {
		t.Fatal(r)
	}
	bad := f.do("POST", "/mcp", strings.NewReader("{"), map[string]string{"Authorization": "Bearer " + token})
	if bad.status != 400 || bad.json(t)["error"].(map[string]any)["code"] != -32700.0 || bad.json(t)["id"] != nil {
		t.Fatal(bad)
	}
	batch := f.do("POST", "/mcp", strings.NewReader(`[{"jsonrpc": "2.0", "id": 1, "method": "ping"}]`),
		map[string]string{"Authorization": "Bearer " + token})
	if batch.status != 400 || batch.json(t)["error"].(map[string]any)["code"] != -32600.0 {
		t.Fatal(batch)
	}
	if r := f.do("POST", "/mcp", strings.NewReader(`{"jsonrpc": "2.0", "id": 1, "result": {}}`),
		map[string]string{"Authorization": "Bearer " + token}); r.status != 202 {
		t.Fatal(r) // a response: nothing to answer
	}
	old := f.rpc(token, "ping", nil, 1, map[string]string{"MCP-Protocol-Version": "2024-01-01"})
	if old.status != 400 || old.json(t)["error"].(map[string]any)["data"].(map[string]any)["supported"].([]any)[0] != mcp.Modern {
		t.Fatal(old)
	}
	foreign := f.rpc(token, "ping", nil, 1, map[string]string{"MCP-Protocol-Version": "2025-06-18",
		"Origin": "https://evil.example"})
	if foreign.status != 403 {
		t.Fatal(foreign)
	}
	for _, origin := range []string{"https://claude.ai", publicURL} {
		if r := f.rpc(token, "ping", nil, 1, map[string]string{"Origin": origin}); r.status != 200 {
			t.Fatal(origin, r)
		}
	}
	listed := f.rpc(token, "tools/list", nil, 1, nil).json(t)["result"].(map[string]any)["tools"].([]any)
	names := map[string]map[string]any{}
	var order []string
	for _, item := range listed {
		tool := item.(map[string]any)
		names[tool["name"].(string)] = tool
		order = append(order, tool["name"].(string))
		if tool["inputSchema"].(map[string]any)["type"] != "object" || tool["description"] == "" {
			t.Fatal(tool)
		}
	}
	if strings.Join(order[:3], ",") != "get_status,list_deadlines,add_deadline" {
		t.Fatal(order) // in the order they were registered
	}
	if names["list_deadlines"]["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatal(names["list_deadlines"])
	}
	annotations, _ := json.Marshal(names["hide_deadline"]["annotations"])
	if string(annotations) != `{"destructiveHint":true,"idempotentHint":false,"openWorldHint":false,`+
		`"readOnlyHint":false,"title":"Hide a deadline"}` {
		t.Fatal(string(annotations))
	}
	if names["add_deadline"]["annotations"].(map[string]any)["destructiveHint"] != false {
		t.Fatal(names["add_deadline"])
	}
}

func TestModernRequests(t *testing.T) {
	f := setup(t)
	token := f.connect().Access
	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": mcp.Modern,
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	headers := func(extra map[string]string) map[string]string {
		h := map[string]string{"MCP-Protocol-Version": mcp.Modern, "Mcp-Method": "server/discover"}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	found := f.rpc(token, "server/discover", map[string]any{"_meta": meta}, 1, headers(nil))
	if !strings.Contains(found.body, `"result": {"resultType": "complete", "supportedVersions": ["2026-07-28", `) {
		t.Fatal(found.body)
	}
	listed := f.rpc(token, "tools/list", map[string]any{"_meta": meta}, 1, headers(map[string]string{"Mcp-Method": "tools/list"}))
	if len(listed.json(t)["result"].(map[string]any)["tools"].([]any)) == 0 {
		t.Fatal(listed)
	}
	name := base64.StdEncoding.EncodeToString([]byte("get_status"))
	called := f.rpc(token, "tools/call", map[string]any{"_meta": meta, "name": "get_status", "arguments": map[string]any{}},
		1, headers(map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "=?base64?" + name + "?="}))
	if called.json(t)["result"].(map[string]any)["isError"] != false {
		t.Fatal(called)
	}
	for _, c := range []struct {
		wrong map[string]string
		code  float64
	}{{map[string]string{"Mcp-Method": "ping"}, -32020}, {map[string]string{"MCP-Protocol-Version": "2025-06-18"}, -32020}} {
		extra := map[string]string{"Mcp-Method": "tools/list"}
		for k, v := range c.wrong {
			extra[k] = v
		}
		r := f.rpc(token, "tools/list", map[string]any{"_meta": meta}, 1, headers(extra))
		if r.status != 400 || r.json(t)["error"].(map[string]any)["code"] != c.code {
			t.Fatal(c, r)
		}
	}
	mismatch := f.rpc(token, "tools/list", map[string]any{"_meta": meta}, 1,
		headers(map[string]string{"Mcp-Method": "tools/list", "MCP-Protocol-Version": "2025-06-18"}))
	if msg := mismatch.json(t)["error"].(map[string]any)["message"]; msg !=
		"MCP-Protocol-Version '2025-06-18' does not match the body's '2026-07-28'" {
		t.Fatal(msg)
	}
	missing := f.rpc(token, "tools/call", map[string]any{"_meta": meta, "name": "get_status"}, 1,
		headers(map[string]string{"Mcp-Method": "tools/call"}))
	if missing.status != 400 || missing.json(t)["error"].(map[string]any)["code"] != -32020.0 {
		t.Fatal(missing)
	}
	future := map[string]any{"io.modelcontextprotocol/protocolVersion": "2099-01-01"}
	r := f.rpc(token, "ping", map[string]any{"_meta": future}, 1,
		map[string]string{"MCP-Protocol-Version": "2099-01-01", "Mcp-Method": "ping"})
	if r.status != 400 || r.json(t)["error"].(map[string]any)["code"] != -32022.0 {
		t.Fatal(r)
	}
	unknown := f.rpc(token, "initialize", map[string]any{"_meta": meta}, 1, headers(map[string]string{"Mcp-Method": "initialize"}))
	if unknown.status != 404 || unknown.json(t)["error"].(map[string]any)["code"] != -32601.0 {
		t.Fatal(unknown)
	}
	// A legacy request with the modern header and no _meta.
	if r := f.rpc(token, "ping", nil, 1, map[string]string{"MCP-Protocol-Version": mcp.Modern}); r.status != 400 ||
		r.json(t)["error"].(map[string]any)["code"] != -32020.0 {
		t.Fatal(r)
	}
	// A notification answers 202 only after the header checks.
	if r := f.rpc(token, "notifications/initialized", map[string]any{"_meta": meta}, nil,
		headers(map[string]string{"Mcp-Method": "ping"})); r.status != 400 {
		t.Fatal(r)
	}
}

// --- tools ---------------------------------------------------------------------------------------

func TestCalls(t *testing.T) {
	f := setup(t)
	token := f.connect().Access
	status := f.call(token, "get_status", nil)
	if text(status) != `{"untrusted": "`+tools.Untrusted+`", "needs_attention": [], "limit": 0.5}` ||
		fmt.Sprint(status["structuredContent"].(map[string]any)["limit"]) != "0.5" {
		t.Fatal(status)
	}
	bad := f.call(token, "list_deadlines", map[string]any{"days": 0})
	if bad["isError"] != true || text(bad) != "arguments.days: at least 1" {
		t.Fatal(bad)
	}
	raw := f.do("POST", "/mcp", strings.NewReader(`{"jsonrpc": "2.0", "id": 1, "method": "tools/call", `+
		`"params": {"name": "list_deadlines", "arguments": {"days": 1.0, "x": 1}}}`),
		map[string]string{"Authorization": "Bearer " + token})
	if got := text(raw.json(t)["result"].(map[string]any)); got != "arguments.x: unknown; arguments.days: expected integer" {
		t.Fatal(got) // 1.0 is no integer, as in Python
	}
	if r := f.call(token, "broken", nil); r["isError"] != true || text(r) != "internal error: see the service's log" {
		t.Fatal(r)
	}
	unknown := f.rpc(token, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}}, 1, nil)
	if unknown.status != 200 || unknown.json(t)["error"].(map[string]any)["code"] != -32602.0 ||
		unknown.json(t)["error"].(map[string]any)["message"] != "Unknown tool: nope" {
		t.Fatal(unknown)
	}
	nameless := f.rpc(token, "tools/call", map[string]any{}, 1, nil)
	if nameless.json(t)["error"].(map[string]any)["message"] != "Unknown tool: None" {
		t.Fatal(nameless)
	}
	notObject := f.rpc(token, "tools/call", map[string]any{"name": "get_status", "arguments": []any{1}}, 1, nil)
	if text(notObject.json(t)["result"].(map[string]any)) != "arguments: expected object" {
		t.Fatal(notObject)
	}
}

func TestWriteJournalUndo(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	token := f.connect().Access
	result := f.call(token, "add_deadline", map[string]any{"title": "Essay", "due": "2026-10-25"})
	if !strings.HasPrefix(text(result), `{"change": {"id": 1, "summary": "Added the deadline «Essay», due 2026-10-25", `+
		`"undo": "the owner can undo it in the web app"}, "uid": "ha-1", "title": "Essay"}`) {
		t.Fatal(text(result))
	}
	if f.app.deadlines["ha-1"] != "Essay" {
		t.Fatal(f.app.deadlines)
	}
	refused := f.call(token, "add_deadline", map[string]any{"title": "", "due": "2026-10-25"})
	if refused["isError"] != true || !strings.Contains(text(refused), "title") {
		t.Fatal(refused)
	}
	enum := f.call(token, "add_deadline", map[string]any{"title": "x", "due": "d", "category": "work"})
	if text(enum) != `arguments.category: one of "study", "personal"` {
		t.Fatal(text(enum))
	}

	screen, err := f.conn.State() // what «Machine → Claude connector» shows
	if err != nil {
		t.Fatal(err)
	}
	if screen.Get("url") != publicURL+"/mcp" || screen.Get("tools").(httpx.Object).Get("write") != 3 {
		t.Fatal(screen)
	}
	connections := screen.Get("connections").([]httpx.Object)
	if len(connections) != 1 || connections[0].Get("name") != "Claude" ||
		*connections[0].Get("redirect").(*string) != "claude.ai" ||
		*connections[0].Get("created_at").(*string) != clock.LocalISO(time.Unix(1_800_000_000, 0)) {
		t.Fatal(connections)
	}
	journal := screen.Get("changes").([]httpx.Object)
	if len(journal) != 1 || journal[0].Get("tool") != "add_deadline" || journal[0].Get("undo") != true ||
		journal[0].Get("undone_at").(*string) != nil {
		t.Fatal(journal)
	}
	change, _ := f.conn.Changes.Get(1)
	if change.Via != "claude" || *change.GrantID != connections[0].Get("id").(int64) ||
		change.At != "2026-10-20T12:00:00" {
		t.Fatal(change)
	}

	if _, err := f.conn.Undo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.app.deadlines) != 0 {
		t.Fatal(f.app.deadlines)
	}
	if again, _ := f.conn.Changes.Get(1); again.UndoneAt == nil {
		t.Fatal(again)
	}
	if _, err := f.conn.Undo(ctx, 1); err == nil || !strings.Contains(err.Error(), "undone already") {
		t.Fatal(err)
	}
	if _, err := f.conn.Undo(ctx, 99); err == nil || err.Error() != "no such change" {
		t.Fatal(err)
	}

	// A change that cannot be undone says why.
	f.call(token, "add_deadline", map[string]any{"title": "Gym", "due": "2026-10-26"})
	deleted := f.call(token, "delete_deadline", map[string]any{"uid": "ha-2"})
	if !strings.HasPrefix(text(deleted), `{"change": {"id": 3, "summary": "Deleted «Gym»", "undo": "cannot be undone: `+
		`it is gone for good"}, "result": "deleted"}`) {
		t.Fatal(text(deleted))
	}
	if _, err := f.conn.Undo(ctx, 3); err == nil || err.Error() != "it is gone for good" {
		t.Fatal(err)
	}

	// An undo op no client sees, its arguments as the tool stored them.
	f.call(token, "hide_deadline", map[string]any{"uid": "tg-1"})
	stored, _ := f.conn.Changes.Get(4)
	var undo string
	f.conn.Store.DB.QueryRow("select undo from connector_changes where id = 4").Scan(&undo)
	if undo != `[{"tool": "_assume_again", "arguments": {"deadline_id": 3, "due_at": "2026-10-25", "share": 8.0, `+
		`"sheet_id": null}}]` {
		t.Fatal(undo)
	}
	if _, err := f.conn.Undo(ctx, stored.ID); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.app.assumed) != "[map[deadline_id:3 due_at:2026-10-25 share:8 sheet_id:<nil>]]" {
		t.Fatal(f.app.assumed)
	}

	grantID := connections[0].Get("id").(int64)
	if done, err := f.conn.OAuth.RevokeGrant(grantID); !done || err != nil {
		t.Fatal(done, err)
	}
	if screen, _ := f.conn.State(); len(screen.Get("connections").([]httpx.Object)) != 0 {
		t.Fatal(screen)
	}
	if r := f.rpc(token, "ping", nil, 1, nil); r.status != 401 {
		t.Fatal(r)
	}
}

func TestUndoFailsWhenThingsMoved(t *testing.T) {
	f := setup(t)
	token := f.connect().Access
	data(t, f.call(token, "add_deadline", map[string]any{"title": "Essay", "due": "2026-10-25"}))
	recent, _ := f.conn.Changes.Recent(1, "")
	changeID := recent[0].ID
	delete(f.app.deadlines, "ha-1")
	_, err := f.conn.Undo(context.Background(), changeID)
	if err == nil || !strings.Contains(err.Error(), "no such deadline") {
		t.Fatal(err)
	}
	change, _ := f.conn.Changes.Get(changeID)
	if change.UndoneAt != nil || !strings.Contains(*change.UndoError, "no such deadline") {
		t.Fatal(change)
	}
}

func TestActAndJournal(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id, output, err := f.conn.Act(ctx, "add_deadline", map[string]any{"title": "Lab", "due": "2026-10-27"}, changes.ViaWeb)
	if err != nil || output.Summary == "" {
		t.Fatal(err)
	}
	change, _ := f.conn.Changes.Get(id)
	if change.Via != "web" || change.GrantID != nil {
		t.Fatal(change)
	}
	if web, _ := f.conn.State(); len(web.Get("changes").([]httpx.Object)) != 0 {
		t.Fatal("a web change in Claude's journal")
	}
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{{"get_status", nil, "no such change: get_status"}, {"nope", nil, "no such change: nope"},
		{"add_deadline", map[string]any{"due": "x"}, "arguments.title: required"}} {
		if _, _, err := f.conn.Act(ctx, c.tool, c.args, changes.ViaWeb); err == nil || err.Error() != c.want {
			t.Fatal(c, err)
		}
	}
	// A change made without a tool, undone by an op; an unknown step fails.
	journaled, err := f.conn.Journal("put_aside", map[string]any{"id": int64(7)}, "Put aside",
		[]tools.Step{{Tool: "_assume_again", Arguments: map[string]any{"deadline_id": int64(7)}}}, "", changes.ViaWeb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.Undo(ctx, journaled); err != nil || len(f.app.assumed) != 1 {
		t.Fatal(err, f.app.assumed)
	}
	unknown, _ := f.conn.Journal("put_aside", map[string]any{}, "x", []tools.Step{{Tool: "_gone"}}, "", changes.ViaWeb)
	if _, err := f.conn.Undo(ctx, unknown); err == nil || err.Error() != "unknown undo step _gone" {
		t.Fatal(err)
	}
	// A tool's step is checked against its schema; an op's is not.
	checked, _ := f.conn.Journal("x", map[string]any{}, "x", []tools.Step{{Tool: "delete_deadline",
		Arguments: map[string]any{"uid": int64(5)}}}, "", changes.ViaWeb)
	if _, err := f.conn.Undo(ctx, checked); err == nil || err.Error() != "arguments.uid: expected string" {
		t.Fatal(err)
	}
	// An empty undo is an undo of nothing, not "cannot be undone".
	empty, _ := f.conn.Journal("x", map[string]any{}, "x", []tools.Step{}, "", changes.ViaWeb)
	if _, err := f.conn.Undo(ctx, empty); err != nil {
		t.Fatal(err)
	}
}

func TestJournalKeepsTheNewest(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	journal, err := changes.New(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < changes.Keep+5; n++ {
		if _, err := journal.Record(nil, "add_deadline", map[string]any{"n": int64(n)}, fmt.Sprintf("change %d", n),
			nil, "", changes.ViaClaude); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := journal.Recent(changes.Keep+10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != changes.Keep || listed[0].Arguments.(map[string]any)["n"] != int64(changes.Keep+4) {
		t.Fatal(len(listed), listed[0].Arguments)
	}
	// Arguments past 4000 code points are cut, and read back as {"cut": text}.
	id, _ := journal.Record(nil, "x", map[string]any{"text": strings.Repeat("я", 5000)}, "", nil, "", changes.ViaWeb)
	change, _ := journal.Get(id)
	cut := change.Arguments.(map[string]any)["cut"].(string)
	if len([]rune(cut)) != changes.ArgumentsChars || !strings.HasPrefix(cut, `{"text": "яя`) || change.Undo != nil {
		t.Fatal(len([]rune(cut)))
	}
	// A second start keeps the table and its via column.
	if _, err := changes.New(s, nil); err != nil {
		t.Fatal(err)
	}
}

func TestViaColumnAdded(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cut := strings.Index(changes.Schema, "    undo_error text,")
	if cut < 0 || !strings.Contains(changes.Schema, "via text") {
		t.Fatal("the schema has no via")
	}
	old := changes.Schema[:cut] + "    undo_error text\n);\n"
	if old == changes.Schema {
		t.Fatal("the schema has no via")
	}
	if _, err := s.DB.Exec(old); err != nil {
		t.Fatal(err)
	}
	s.DB.Exec("insert into connector_changes(at, tool, arguments, summary) values('2026-10-20T12:00:00', 'x', '{}', 's')")
	journal, err := changes.New(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if change, _ := journal.Get(1); change.Via != "claude" {
		t.Fatal(change)
	}
}
