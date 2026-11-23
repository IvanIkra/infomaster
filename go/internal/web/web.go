// Package web is the web app (§8.6, D61, stage 16): a home-screen app on the public port that took the place of the
// forum and the Mini App (D69). The port of infomaster/web.
//
// On the public port, next to the connector:
//   - /web/: the app's page; /web/static/*: its scripts, styles, fonts and icons; /web/sw.js: the service worker,
//     whose scope is /web/; /web/manifest.webmanifest;
//   - /web/api/*: JSON for the app, behind the session cookie (auth.go); what the screens read comes from views.go,
//     the «Check» queue from check.go. A request that changes something must come from the app's own origin: the
//     cookie is SameSite=Strict already, and the Origin header is checked too.
//
// The owner's changes (stage 16.3) are the connector's write tools (§8.5), called through Connector.Act: the same
// checks, and every change goes into the connector's journal (`via` web) with the steps that undo it.
//
// Notifications (stage 16.4): a device turns them on with its browser's push subscription (push.go); the notifier
// (notify.go) sends the deadlines' news, reminders and failures (D69).
//
// News (stage 16.7), shelves (16.8, shelves.go), corrections (16.9, fixes.go), «Machine» (16.6, machine.go) and «+»
// (16.5, quick.go) are under /web/api too.
//
// The client is plain ES modules, no build step, served from a directory (web/static) unchanged.
package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/connector"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// StaticPath is where the client lives in the Go module (go/ of the repository).
const StaticPath = "web/static"

var changing = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// WebApp is the web app on the public port.
type WebApp struct {
	Env     config.Env
	Store   *store.Store
	Origin  string // scheme://netloc of PUBLIC_URL
	Lang    func() string
	Seconds func() float64

	Data  *Data  // what the screens read; nil leaves the screens out (the login's tests)
	Check *Check // with Data
	Quick *Quick // with Data
	// Connector is the service's connector: the owner's changes are its tools, journaled with undo. Set it before
	// Attach.
	Connector *connector.Connector
	Auth      *Auth
	Push      *Push
	Notifier  *Notifier
	// Parts are the services «Machine» and the shelves read; set them before the port serves.
	Parts Parts

	Static string // the client's directory
	CSP    string // the Content-Security-Policy of every /web answer
}

// New makes the web app's tables and services. sendCode is the bot's text to the owner (nil while there is no bot);
// lang and seconds may be nil (English, the clock); data nil leaves the screens out; model reads «+»'s one line (nil:
// none). static is the client's directory, "" for web/static of the Go module.
func New(env config.Env, s *store.Store, sendCode func(context.Context, string) error, lang func() string,
	seconds func() float64, data *Data, model llm.Model, static string) (*WebApp, error) {
	if lang == nil {
		lang = func() string { return "en" }
	}
	if seconds == nil {
		seconds = unixNow
	}
	if static == "" {
		static = FindStatic()
	}
	parts, err := url.Parse(env.PublicURL)
	if err != nil {
		return nil, err
	}
	csp, err := ContentSecurityPolicy(filepath.Join(static, "index.html"))
	if err != nil {
		return nil, err
	}
	w := &WebApp{Env: env, Store: s, Origin: parts.Scheme + "://" + parts.Host, Lang: lang, Seconds: seconds,
		Data: data, Static: static, CSP: csp}
	if data != nil {
		if w.Check, err = NewCheck(data, s); err != nil {
			return nil, err
		}
		w.Quick = &Quick{Data: data, LLM: model}
	}
	if w.Auth, err = NewAuth(s, strings.ToLower(parts.Hostname()), w.Origin, sendCode, lang, seconds); err != nil {
		return nil, err
	}
	if w.Push, err = NewPush(s, env.PublicURL, seconds); err != nil {
		return nil, err
	}
	w.Notifier = NewNotifier(w.Push, lang)
	return w, nil
}

// FindStatic is web/static of the Go module: looked for from the working directory up (in it and in its go/), then
// next to this source file.
func FindStatic() string {
	if dir, err := os.Getwd(); err == nil {
		for {
			for _, candidate := range []string{filepath.Join(dir, filepath.FromSlash(StaticPath)),
				filepath.Join(dir, "go", filepath.FromSlash(StaticPath))} {
				if info, err := os.Stat(filepath.Join(candidate, "index.html")); err == nil && !info.IsDir() {
					return candidate
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Join(filepath.Dir(file), "..", "..", filepath.FromSlash(StaticPath))
	}
	return filepath.FromSlash(StaticPath)
}

var inlineScript = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)

// ContentSecurityPolicy is the app's CSP: its pages run only its own files and the inline scripts of index.html
// (the import map), by their hashes; a script slipped into a page (a quote, a link) has nowhere to load from, and no
// other site may frame the app. Styles stay inline-capable: Preact sets style attributes.
func ContentSecurityPolicy(index string) (string, error) {
	page, err := os.ReadFile(index)
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(page), "\r\n", "\n"), "\r", "\n") // read in text mode
	hashes := []string{}
	for _, found := range inlineScript.FindAllStringSubmatch(text, -1) {
		if found[1] == "" {
			continue
		}
		sum := sha256.Sum256([]byte(found[1]))
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return "default-src 'self'; script-src 'self' " + strings.Join(hashes, " ") + "; style-src 'self' " +
		"'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'; worker-src 'self'; " +
		"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'", nil
}

// --- headers -------------------------------------------------------------------------------------------------

type headed struct {
	http.ResponseWriter
	app  *WebApp
	path string
	done bool
}

func (h *headed) set() {
	if h.done {
		return
	}
	h.done = true
	header := h.Header()
	setDefault := func(name, value string) {
		if header.Get(name) == "" {
			header.Set(name, value)
		}
	}
	if strings.HasPrefix(h.path, "/web/static/") {
		setDefault("Cache-Control", "no-cache")
	}
	if !strings.HasPrefix(h.path, "/web") {
		return
	}
	setDefault("X-Content-Type-Options", "nosniff")
	setDefault("Referrer-Policy", "same-origin")
	setDefault("X-Frame-Options", "DENY")
	setDefault("Content-Security-Policy", h.app.CSP)
	if strings.HasPrefix(h.path, "/web/api/") {
		setDefault("Cache-Control", "no-store")
	}
}

func (h *headed) WriteHeader(status int) {
	h.set()
	h.ResponseWriter.WriteHeader(status)
}

func (h *headed) Write(b []byte) (int, error) {
	h.set()
	return h.ResponseWriter.Write(b)
}

func (h *headed) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// Wrap gives every answer whose path starts with /web the app's headers (set only if absent): no type sniffing, no
// framing, no referrer to other sites, the CSP, no-store under /web/api/, and no-cache for /web/static/* (its modules
// are asked for again on every load, a 304 while unchanged). Put it around the public port's whole handler, so the
// 404 and 405 answers get them too.
func (w *WebApp) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&headed{ResponseWriter: rw, app: w, path: r.URL.Path}, r)
	})
}

// --- pages ---------------------------------------------------------------------------------------------------

var staticTypes = map[string]string{".js": "text/javascript", ".mjs": "text/javascript", ".css": "text/css",
	".woff2": "font/woff2", ".woff": "font/woff", ".ttf": "font/ttf", ".png": "image/png", ".svg": "image/svg+xml",
	".webmanifest": "application/manifest+json", ".txt": "text/plain", ".html": "text/html",
	".json": "application/json", ".ico": "image/vnd.microsoft.icon", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".map": "application/json"}

// serveFile is aiohttp's FileResponse of a file under the static directory: its type, ETag and Last-Modified, the
// conditional and Range requests; 404 for none.
func (w *WebApp) serveFile(rw http.ResponseWriter, r *http.Request, name, contentType string, headers map[string]string) {
	root, err := os.OpenRoot(w.Static)
	if err != nil {
		httpx.Error(rw, http.StatusNotFound)
		return
	}
	defer root.Close()
	file, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		httpx.Error(rw, http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		httpx.Error(rw, http.StatusNotFound)
		return
	}
	if contentType == "" {
		contentType = staticTypes[strings.ToLower(path.Ext(name))]
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	for key, value := range headers {
		rw.Header().Set(key, value)
	}
	rw.Header().Set("Content-Type", contentType)
	rw.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.ModTime().UnixNano(), info.Size()))
	http.ServeContent(rw, r, "", info.ModTime(), file)
}

var noCache = map[string]string{"Cache-Control": "no-cache"}

func (w *WebApp) static(rw http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/web/static/")
	if name == "" || strings.HasSuffix(name, "/") {
		httpx.Error(rw, http.StatusNotFound)
		return
	}
	w.serveFile(rw, r, name, "", nil)
}

// --- the API's frame -----------------------------------------------------------------------------------------

// call is one request of /web/api.
type call struct {
	app     *WebApp
	w       http.ResponseWriter
	r       *http.Request
	session *Session // nil for the login's steps
}

type handler func(c *call) error

type route struct {
	method, path string
	public       bool // open without a session: the login's steps
	fn           handler
}

func (c *call) ctx() context.Context { return c.r.Context() }

func (c *call) json(value any) error {
	httpx.JSON(c.w, http.StatusOK, value)
	return nil
}

// jsonLang answers with every Phrase in the app's language (json_default(lang)).
func (c *call) jsonLang(value any) error {
	httpx.JSONLang(c.w, http.StatusOK, value, c.app.Lang())
	return nil
}

func (c *call) query(name string) (string, bool) {
	values, ok := c.r.URL.Query()[name]
	if !ok || len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// fail is an answer {"error": text} with a status.
func fail(w http.ResponseWriter, status int, text string) {
	httpx.JSON(w, status, httpx.Object{{Key: "error", Value: text}})
}

// failure answers an error of a handler: a refusal as {"error": text} with its status, anything else as a 500.
func failure(w http.ResponseWriter, err error) {
	var auth *AuthError
	var refused *httpx.HTTPError
	switch {
	case errors.As(err, &auth):
		fail(w, auth.Status, auth.Message)
	case errors.As(err, &refused) && refused.Status >= 400:
		text := refused.Text
		if text == "" || strings.HasPrefix(text, strconv.Itoa(refused.Status)+":") {
			text = http.StatusText(refused.Status)
		}
		fail(w, refused.Status, text)
	default:
		log.Printf("web: %v", err)
		httpx.Error(w, http.StatusInternalServerError)
	}
}

// guarded is the checks every /web/api handler goes through: the origin of a change, then the session unless the
// handler is one of the login steps. Errors come back as JSON {"error": text}.
func (w *WebApp) guarded(rt route) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if changing[r.Method] && r.Header.Get("Origin") != w.Origin {
			fail(rw, http.StatusForbidden, "wrong origin")
			return
		}
		c := &call{app: w, w: rw, r: r}
		if !rt.public {
			token := ""
			if cookie, err := r.Cookie(Cookie); err == nil {
				token = cookie.Value
			}
			session, err := w.Auth.Session(token)
			if err != nil {
				failure(rw, err)
				return
			}
			if session == nil {
				fail(rw, http.StatusUnauthorized, "log in")
				return
			}
			c.session = session
		}
		if err := rt.fn(c); err != nil {
			failure(rw, err)
		}
	}
}

// body is the request's JSON object.
func (c *call) body() (map[string]any, error) {
	value, err := httpx.ReadJSON(c.r, httpx.BodyMax)
	if errors.Is(err, httpx.ErrNotJSON) {
		return nil, httpx.BadRequest("JSON expected")
	}
	if err != nil {
		return nil, err
	}
	data, ok := value.(map[string]any)
	if !ok {
		return nil, httpx.BadRequest("a JSON object expected")
	}
	return data, nil
}

// id is an integer path parameter; a bare 404 for anything else.
func (c *call) id(name string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(c.r.PathValue(name)), 10, 64)
	if err != nil {
		return 0, httpx.NotFound("")
	}
	return n, nil
}

func (c *call) say(en, ru string) string {
	if c.app.Lang() == "ru" {
		return ru
	}
	return en
}

func (c *call) device() string { return DeviceName(c.r.Header.Get("User-Agent")) }

// --- Attach ----------------------------------------------------------------------------------------------------

// Attach adds the web app's routes to the public port's mux (Python's attach(app)); set Connector and Parts before
// it. Put Wrap around the port's handler.
func (w *WebApp) Attach(mux *http.ServeMux) {
	wrap := func(h http.HandlerFunc) http.Handler { return w.Wrap(h) }
	mux.Handle("GET /web", wrap(func(rw http.ResponseWriter, r *http.Request) {
		httpx.Redirect(rw, http.StatusFound, "/web/")
	}))
	mux.Handle("GET /web/{$}", wrap(func(rw http.ResponseWriter, r *http.Request) {
		w.serveFile(rw, r, "index.html", "text/html", noCache)
	}))
	mux.Handle("GET /web/sw.js", wrap(func(rw http.ResponseWriter, r *http.Request) {
		w.serveFile(rw, r, "sw.js", "text/javascript", noCache)
	}))
	mux.Handle("GET /web/manifest.webmanifest", wrap(func(rw http.ResponseWriter, r *http.Request) {
		w.serveFile(rw, r, "manifest.webmanifest", "application/manifest+json", noCache)
	}))
	mux.Handle("GET /web/static/", wrap(w.static))
	mux.Handle("POST /web/share", wrap(func(rw http.ResponseWriter, r *http.Request) {
		// Android's «Share» before the service worker took over: the files cannot be kept here (no session is
		// sure), so the app opens «+» and asks to share again.
		httpx.Redirect(rw, http.StatusSeeOther, "/web/#/add?shared=lost")
	}))
	routes := w.routes()
	routes = append(routes, prefixed("/machine", w.machineRoutes())...)
	routes = append(routes, prefixed("/shelf", w.shelfRoutes())...)
	routes = append(routes, prefixed("/fix", w.fixRoutes())...)
	for _, rt := range routes {
		mux.Handle(rt.method+" /web/api"+rt.path, wrap(w.guarded(rt)))
	}
	if w.Connector != nil && w.Check != nil {
		w.Connector.Registry.Op("_ask_again", func(_ *tools.Context, args map[string]any) (tools.Output, error) {
			id, _ := args["id"].(string)
			return tools.Output{}, w.Check.AskAgain(id)
		})
	}
}

func prefixed(prefix string, routes []route) []route {
	out := make([]route, len(routes))
	for i, rt := range routes {
		rt.path = prefix + rt.path
		out[i] = rt
	}
	return out
}

func (w *WebApp) routes() []route {
	return []route{
		{"GET", "/me", false, me},
		{"POST", "/login/code", true, sendCode},
		{"POST", "/login/code/check", true, checkCode},
		{"POST", "/login/passkey/options", true, loginOptions},
		{"POST", "/login/passkey", true, passkeyLogin},
		{"POST", "/logout", false, logout},
		{"GET", "/devices", false, devices},
		{"DELETE", "/sessions/{session_id}", false, endSession},
		{"POST", "/passkeys/options", false, passkeyOptions},
		{"POST", "/passkeys", false, addPasskey},
		{"DELETE", "/passkeys/{passkey_id}", false, removePasskey},
		{"GET", "/feed", false, feed},
		{"GET", "/week", false, week},
		{"GET", "/deadline", false, deadlineCard},
		{"GET", "/class", false, classCard},
		{"GET", "/course", false, course},
		{"POST", "/course/calc", false, courseCalc},
		{"GET", "/news", false, newsList},
		{"GET", "/news/{news_id}", false, newsCard},
		{"POST", "/news/{news_id}/hide", false, hideNews},
		{"GET", "/check", false, check},
		{"POST", "/check/put-aside", false, putAside},
		{"POST", "/deadlines/{action}", false, deadlineAction},
		{"POST", "/changes/{change_id}/undo", false, undo},
		{"GET", "/push", false, pushState},
		{"POST", "/push", false, subscribe},
		{"DELETE", "/push", false, unsubscribe},
		{"POST", "/push/test", false, pushTest},
		{"GET", "/add", false, addState},
		{"POST", "/add/line", false, addLine},
		{"POST", "/add/files", false, addFiles},
		{"POST", "/add/note", false, addNote},
		{"POST", "/add/{what}", false, addByHand},
	}
}

// --- the login and the devices ---------------------------------------------------------------------------------

func me(c *call) error {
	passkeys, err := c.app.Auth.Passkeys()
	if err != nil {
		return err
	}
	push, err := c.app.Push.Subscribed(c.session.ID)
	if err != nil {
		return err
	}
	return c.json(httpx.Object{{Key: "lang", Value: c.app.Lang()}, {Key: "session", Value: c.session.ID},
		{Key: "device", Value: c.session.Device}, {Key: "passkeys", Value: len(passkeys)}, {Key: "push", Value: push}})
}

func sendCode(c *call) error {
	if err := c.app.Auth.Send(c.ctx()); err != nil {
		return err
	}
	return c.json(httpx.Object{{Key: "ok", Value: true}})
}

// loggedIn answers with the session's cookie.
func (c *call) loggedIn(token string) error {
	http.SetCookie(c.w, &http.Cookie{Name: Cookie, Value: token, MaxAge: SessionIdle, Path: CookiePath, Secure: true,
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return c.json(httpx.Object{{Key: "ok", Value: true}})
}

func checkCode(c *call) error {
	data, err := c.body()
	if err != nil {
		return err
	}
	token, err := c.app.Auth.CheckCode(data["code"], c.device())
	if err != nil {
		return err
	}
	return c.loggedIn(token)
}

func jsonText(c *call, text string) error {
	c.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.w.WriteHeader(http.StatusOK)
	_, err := c.w.Write([]byte(text))
	return err
}

func loginOptions(c *call) error {
	text, err := c.app.Auth.LoginOptions()
	if err != nil {
		return err
	}
	return jsonText(c, text)
}

// truthy is Python's truth of a JSON value.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case int64:
		return v != 0
	case float64:
		return v != 0
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	}
	return true
}

func passkeyLogin(c *call) error {
	data, err := c.body()
	if err != nil {
		return err
	}
	credential := data["credential"]
	if !truthy(credential) {
		credential = map[string]any{}
	}
	token, err := c.app.Auth.PasskeyLogin(credential, c.device())
	if err != nil {
		return err
	}
	return c.loggedIn(token)
}

func logout(c *call) error {
	if _, err := c.app.Auth.End(c.session.ID); err != nil {
		return err
	}
	http.SetCookie(c.w, &http.Cookie{Name: Cookie, Value: "", Path: CookiePath, MaxAge: -1,
		Expires: time.Unix(0, 0).UTC()})
	return c.json(httpx.Object{{Key: "ok", Value: true}})
}

func (c *call) devices() (httpx.Object, error) {
	sessions, err := c.app.Auth.Sessions()
	if err != nil {
		return nil, err
	}
	passkeys, err := c.app.Auth.Passkeys()
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "current", Value: c.session.ID}, {Key: "sessions", Value: sessions},
		{Key: "passkeys", Value: passkeys}}, nil
}

func devices(c *call) error {
	listed, err := c.devices()
	if err != nil {
		return err
	}
	return c.json(listed)
}

func endSession(c *call) error {
	id, err := c.id("session_id")
	if err != nil {
		return err
	}
	ended, err := c.app.Auth.End(id)
	if err != nil {
		return err
	}
	if !ended {
		fail(c.w, http.StatusNotFound, "no such device")
		return nil
	}
	return devices(c)
}

func passkeyOptions(c *call) error {
	text, err := c.app.Auth.RegistrationOptions(c.session.ID)
	if err != nil {
		return err
	}
	return jsonText(c, text)
}

func addPasskey(c *call) error {
	data, err := c.body()
	if err != nil {
		return err
	}
	credential, ok := data["credential"].(map[string]any)
	if !ok {
		credential = map[string]any{}
	}
	if _, err := c.app.Auth.AddPasskey(c.session, credential); err != nil {
		return err
	}
	if send := c.app.Auth.SendCode; send != nil { // D79: a new key to the app is told where the owner sees it
		device := c.session.Device
		text := c.say("A passkey for the infomaster app was added on "+device+". If it was not you, remove it in "+
			"Login and devices.", "В приложении infomaster добавлен ключ доступа на устройстве "+device+". Если это "+
			"были не вы, удалите его в «Вход и устройства».")
		if err := send(c.ctx(), text); err != nil {
			log.Printf("the bot could not tell of a new passkey")
		}
	}
	return devices(c)
}

func removePasskey(c *call) error {
	id, err := c.id("passkey_id")
	if err != nil {
		return err
	}
	removed, err := c.app.Auth.RemovePasskey(id)
	if err != nil {
		return err
	}
	if !removed {
		fail(c.w, http.StatusNotFound, "no such passkey")
		return nil
	}
	return devices(c)
}

// --- the screens (views.go) ------------------------------------------------------------------------------------

func (c *call) data() (*Data, error) {
	if c.app.Data == nil {
		return nil, httpx.Fail(http.StatusServiceUnavailable, "the screens are not wired")
	}
	return c.app.Data, nil
}

// spaced is " ".join(text.split()).
func spaced(text string) string { return strings.Join(pytext.Split(text), " ") }

func isSpace(r rune) bool { return pytext.IsSpace(r) }

func (c *call) course() string {
	value, _ := c.query("course")
	return spaced(value)
}

func feed(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	course := c.course()
	answer, err := data.Feed(course)
	if err != nil {
		return err
	}
	questions, err := c.app.Check.Items(course)
	if err != nil {
		return err
	}
	problems, err := c.app.attention(c)
	if err != nil {
		return err
	}
	inbox := int64(0)
	if data.Library != nil {
		if inbox, err = data.Library.InboxCount(); err != nil {
			return err
		}
	}
	return c.json(append(answer, httpx.Field{Key: "check", Value: questions},
		httpx.Field{Key: "problems", Value: len(problems)}, httpx.Field{Key: "inbox", Value: inbox}))
}

func week(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	day := data.Now()
	if start, ok := c.query("start"); ok {
		if day, err = time.Parse("2006-01-02", start); err != nil {
			fail(c.w, http.StatusBadRequest, "start: a date like 2026-11-16")
			return nil
		}
	}
	answer, err := data.Week(day, c.course())
	if err != nil {
		return err
	}
	return c.json(answer)
}

func deadlineCard(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	uid, _ := c.query("uid")
	card, err := data.DeadlineCard(uid)
	if err != nil {
		return err
	}
	if card == nil {
		fail(c.w, http.StatusNotFound, "no such deadline")
		return nil
	}
	return c.json(card)
}

func classCard(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	uid, _ := c.query("uid")
	card, err := data.ClassCard(uid)
	if err != nil {
		return err
	}
	if card == nil {
		fail(c.w, http.StatusNotFound, "no such class")
		return nil
	}
	return c.json(card)
}

func course(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	key, _ := c.query("key")
	found, err := data.Course(spaced(key))
	if err != nil {
		return err
	}
	if found == nil {
		fail(c.w, http.StatusNotFound, "no such course")
		return nil
	}
	return c.json(found)
}

// courseCalc is POST {key, book, target?, values: {cell: number|null}, solve?: {cell, goal}}: a gradebook's
// calculator (17.6). Nothing is saved: the values are the owner's «what if».
func courseCalc(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	data, err := c.data()
	if err != nil {
		return err
	}
	given := map[string]*float64{}
	values := body["values"]
	if !truthy(values) {
		values = map[string]any{}
	}
	cells, ok := values.(map[string]any)
	if !ok {
		fail(c.w, http.StatusBadRequest, "values: {cell: number or null} expected")
		return nil
	}
	for cell, value := range cells {
		switch v := value.(type) {
		case nil:
			given[cell] = nil
		case int64:
			f := float64(v)
			given[cell] = &f
		case float64:
			if math.IsInf(v, 0) || math.IsNaN(v) {
				fail(c.w, http.StatusBadRequest, "values: {cell: number or null} expected")
				return nil
			}
			given[cell] = &v
		default:
			fail(c.w, http.StatusBadRequest, "values: {cell: number or null} expected")
			return nil
		}
	}
	target, _ := body["target"].(string)
	var solve *Solve
	if asked, ok := body["solve"].(map[string]any); ok {
		cell, _ := asked["cell"].(string)
		switch goal := asked["goal"].(type) {
		case int64:
			solve = &Solve{cell, float64(goal)}
		case float64:
			solve = &Solve{cell, goal}
		case bool:
			solve = &Solve{cell, map[bool]float64{true: 1, false: 0}[goal]}
		}
	}
	key := spaced(pyStr(body["key"]))
	found, err := data.Calc(key, pyStr(body["book"]), target, given, solve)
	if err != nil {
		return err
	}
	if found == nil {
		fail(c.w, http.StatusNotFound, "no such gradebook or target")
		return nil
	}
	return c.json(found)
}

// pyStr is str(value or "").
func pyStr(value any) string {
	if !truthy(value) {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return "True"
	}
	return fmt.Sprint(value)
}

// --- news (stage 16.7) -----------------------------------------------------------------------------------------

func newsList(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	answer, err := data.NewsList()
	if err != nil {
		return err
	}
	problems, err := c.app.attention(c)
	if err != nil {
		return err
	}
	return c.json(append(answer, httpx.Field{Key: "problems", Value: len(problems)}))
}

func newsCard(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	id, err := c.id("news_id")
	if err != nil {
		return err
	}
	card, err := data.NewsCard(id)
	if err != nil {
		return err
	}
	if card == nil {
		fail(c.w, http.StatusNotFound, "no such news item")
		return nil
	}
	return c.json(card)
}

func hideNews(c *call) error {
	connector, err := c.connector()
	if err != nil {
		return err
	}
	id, err := c.id("news_id")
	if err != nil {
		return err
	}
	return c.act(connector, "hide_news", map[string]any{"news_id": id})
}

// --- the owner's changes (stage 16.3) --------------------------------------------------------------------------

func (c *call) connector() (*connector.Connector, error) {
	if c.app.Connector == nil {
		return nil, httpx.Fail(http.StatusServiceUnavailable, "changes are not wired")
	}
	return c.app.Connector, nil
}

// change is the change answer's {id, summary, undo, note}.
func change(id int64, output tools.Output) httpx.Object {
	return httpx.Object{{Key: "id", Value: id}, {Key: "summary", Value: output.Summary},
		{Key: "undo", Value: output.Undo != nil}, {Key: "note", Value: output.Note}}
}

// act is a tool of the connector, journaled; a refusal is 400 {"error": text}.
func (c *call) act(connector *connector.Connector, tool string, arguments map[string]any) error {
	id, output, err := connector.Act(c.ctx(), tool, arguments, "web")
	if text, refused := tools.Refusal(err); refused {
		fail(c.w, http.StatusBadRequest, text)
		return nil
	}
	if err != nil {
		return err
	}
	return c.json(httpx.Object{{Key: "change", Value: change(id, output)}})
}

func check(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	items, err := c.app.Check.Items(c.course())
	if err != nil {
		return err
	}
	courses, err := data.Courses()
	if err != nil {
		return err
	}
	return c.json(httpx.Object{{Key: "items", Value: items}, {Key: "courses", Value: courses},
		{Key: "today", Value: dateOf(data.Now())}})
}

func text(data map[string]any, name string) (string, error) {
	value, ok := data[name].(string)
	if !ok || strings.TrimFunc(value, isSpace) == "" {
		return "", httpx.BadRequest(name + " expected")
	}
	return strings.TrimFunc(value, isSpace), nil
}

// deadlineAction is POST /deadlines/{action} {uid, ...}: done, not-mine, edit {title?, due?, category?}, confirm (an
// assumed date), merge {keep} (uid is the duplicate that goes), course {course: a key of the lenses}. An own
// deadline is deleted when it is done, not the owner's or a duplicate. The answer: the change, for its undo.
func deadlineAction(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	connector, err := c.connector()
	if err != nil {
		return err
	}
	action := c.r.PathValue("action")
	uid, err := text(body, "uid")
	if err != nil {
		return err
	}
	data, err := c.data()
	if err != nil {
		return err
	}
	d, err := data.find(uid)
	if err != nil {
		return err
	}
	if d == nil {
		fail(c.w, http.StatusNotFound, "this deadline is off the list already")
		return nil
	}
	own := d.Source == "ha"
	var tool string
	var arguments map[string]any
	switch action {
	case "done", "not-mine":
		tool, arguments = "hide_deadline", map[string]any{"uid": uid, "reason": strings.ReplaceAll(action, "-", "_")}
		if own {
			tool, arguments = "delete_deadline", map[string]any{"uid": uid}
		}
	case "merge":
		keep, err := text(body, "keep")
		if err != nil {
			return err
		}
		tool, arguments = "hide_deadline", map[string]any{"uid": uid, "reason": "duplicate", "kept": keep}
		if own {
			tool, arguments = "delete_deadline", map[string]any{"uid": uid}
			if keep == uid {
				fail(c.w, http.StatusBadRequest, "a deadline cannot be a duplicate of itself")
				return nil
			}
		}
	case "edit":
		tool, arguments = "edit_deadline", map[string]any{"uid": uid}
		for _, name := range []string{"title", "due", "category"} {
			if value, ok := body[name]; ok {
				arguments[name] = value
			}
		}
	case "confirm":
		tool, arguments = "confirm_deadline_date", map[string]any{"uid": uid}
	case "course":
		key, err := text(body, "course")
		if err != nil {
			return err
		}
		name, err := data.CourseName(key)
		if err != nil {
			return err
		}
		if name == "" {
			fail(c.w, http.StatusBadRequest, "no such course")
			return nil
		}
		tool, arguments = "set_deadline_course", map[string]any{"uid": uid, "course": name}
	default:
		fail(c.w, http.StatusNotFound, "no such action")
		return nil
	}
	return c.act(connector, tool, arguments)
}

var askedAside = map[string]string{"dup": "Marked two deadlines as different",
	"course": "Left a deadline outside the courses", "assumed": "Left an assumed date as it is"}

// putAside is {id}: a «Check» question answered «leave it»: not asked again, until it is undone.
func putAside(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	question, err := text(body, "id")
	if err != nil {
		return err
	}
	connector, err := c.connector()
	if err != nil {
		return err
	}
	if c.app.Check == nil {
		return httpx.Fail(http.StatusServiceUnavailable, "the screens are not wired")
	}
	put, err := c.app.Check.PutAway(question)
	if err != nil {
		return err
	}
	if !put {
		fail(c.w, http.StatusNotFound, "no such question")
		return nil
	}
	kind, _, _ := strings.Cut(question, ":")
	summary, ok := askedAside[kind]
	if !ok {
		summary = "Put a question aside"
	}
	undoSteps := []tools.Step{{Tool: "_ask_again", Arguments: map[string]any{"id": question}}}
	id, err := connector.Journal("put_aside", map[string]any{"id": question}, summary, undoSteps, "", "web")
	if err != nil {
		return err
	}
	return c.json(httpx.Object{{Key: "change", Value: httpx.Object{{Key: "id", Value: id},
		{Key: "summary", Value: summary}, {Key: "undo", Value: true}, {Key: "note", Value: ""}}}})
}

// undo undoes a change of the journal: the web app's own, or one Claude made through the connector.
func undo(c *call) error {
	connector, err := c.connector()
	if err != nil {
		return err
	}
	id, err := c.id("change_id")
	if err != nil {
		return err
	}
	done, err := connector.Undo(c.ctx(), id)
	if text, refused := tools.Refusal(err); refused {
		fail(c.w, http.StatusBadRequest, text)
		return nil
	}
	if err != nil {
		return err
	}
	return c.json(httpx.Object{{Key: "change", Value: httpx.Object{{Key: "id", Value: done.ID},
		{Key: "summary", Value: done.Summary}, {Key: "undone_at", Value: done.UndoneAt}}}})
}

// --- notifications (stage 16.4) --------------------------------------------------------------------------------

func (c *call) pushState() (httpx.Object, error) {
	push := c.app.Push
	on, err := push.Subscribed(c.session.ID)
	if err != nil {
		return nil, err
	}
	devices, err := push.Devices()
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "key", Value: push.Vapid.Public}, {Key: "on", Value: on},
		{Key: "devices", Value: len(devices)}}, nil
}

func pushState(c *call) error {
	state, err := c.pushState()
	if err != nil {
		return err
	}
	return c.json(state)
}

func subscribe(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	if err := c.app.Push.Subscribe(c.session.ID, body["subscription"]); err != nil {
		var refused *PushError
		if errors.As(err, &refused) {
			fail(c.w, http.StatusBadRequest, refused.Msg)
			return nil
		}
		return err
	}
	return pushState(c)
}

func unsubscribe(c *call) error {
	if _, err := c.app.Push.Unsubscribe(c.session.ID); err != nil {
		return err
	}
	return pushState(c)
}

// pushTest is a message to this device only, to see that notifications come.
func pushTest(c *call) error {
	message := Message{Title: "infomaster", Body: c.say("Notifications work on this device.",
		"Уведомления на этом устройстве работают."), URL: "/web/#/", Tag: "test"}
	sent, err := c.app.Push.Send(c.ctx(), message, &c.session.ID, "", 0)
	if err != nil {
		log.Printf("web: a test push: %v", err)
	}
	if sent == 0 {
		fail(c.w, http.StatusBadGateway, c.say("The push service did not take it.", "Служба уведомлений его не приняла."))
		return nil
	}
	return c.json(httpx.Object{{Key: "sent", Value: sent}})
}
