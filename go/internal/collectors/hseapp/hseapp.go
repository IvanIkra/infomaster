// Package hseapp is HSE App X deadlines (§5.2, §8.1, §8.4, D7): the port of Tablo's sources/hseapp.py (§5.1,
// research/hseapp).
//
// The app's own Keycloak client (public, no secret) gives a 3-hour access token and a refresh token whose 21-day
// window slides with every refresh. Both live in kv "hseapp.auth" and go nowhere else: not into the status, the log,
// the notices or the web app. The access token is refreshed when under 10 minutes are left, and at once on a 401;
// the refresh token rotates, so every answer is saved at once. A refresh answered 400 or 401 (invalid_grant: three
// weeks without a refresh, or a logout) erases the tokens: a login is needed. Two clients refreshing one token chain
// throw each other out, so Tablo's own HSE App X must be off before this one runs (`hseapp_enabled` here).
//
// The login is PKCE in a browser, started from the web app. The client accepts any http(s) return address, so the
// browser comes back to our public port behind Funnel, /hseapp/callback. That route has no initData: the PKCE
// `state` is its protection, and only the last 5 login links work (kept in memory: a restart asks for a new link).
// The fallback is the app's own ru.hse.hseappx:// address, with the return address pasted by hand in
// «Machine → HSE App X».
//
// Every 15 minutes, when `hseapp_enabled` and logged in: POST /v2/deadlines with is_completed false, then true, up
// to 10 pages each, 60 s per request. ACTIVE ones come from the first, COMPLETED* from the second; the discipline is
// cut at the first " (", expires_at becomes a Moscow date. The collector never calls the LLM (§4): OnPoll hands both
// lists to the processing (processing/hseapp.py), which labels them in English and keeps them for the merge. A
// failed poll keeps what the last good one gave.
//
// The owner hears once per change of state: a login is needed (it leads to «Machine → HSE App X») and the login is
// back; and once when the API has failed for 3 hours, and when it works again.
package hseapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	Keycloak       = "https://saml.hse.ru/realms/hse/protocol/openid-connect"
	TokenURL       = Keycloak + "/token"
	AuthURL        = Keycloak + "/auth"
	DeadlinesURL   = "https://api.hseapp.ru/v2/deadlines"
	ClientID       = "app-x-android"
	AppRedirectURI = "ru.hse.hseappx://saml.hse.ru/authorize_callback" // the app's own: the return address is pasted
	Scope          = "profile email"
	CallbackPath   = "/hseapp/callback" // on the public port: the browser comes back here after the login

	AuthKey = "hseapp.auth"       // the tokens, a secret: unix seconds for the times, as in Tablo (§9)
	ToldKey = "hseapp.login_told" // the owner has heard that a login is needed

	RefreshMargin   = 10 * 60    // seconds: the access token is refreshed this long before it expires
	RefreshLifetime = 21 * 86400 // seconds, when the answer does not say
	PollEvery       = 15 * time.Minute
	Timeout         = 60 * time.Second // each request of a poll
	LoginTimeout    = 30 * time.Second // the exchange of the code
	MaxPages        = 10
	PendingLogins   = 5                // login links that still work
	StaleAfter      = 3 * time.Hour    // failing longer than this is told to the owner (§5.1)
	LoginError      = "login needed: " // the status error when Keycloak ended the session
)

// LoginNeededError is Python's LoginNeeded: there are no tokens, or Keycloak ended the session.
type LoginNeededError struct{ Reason string }

func (e *LoginNeededError) Error() string { return e.Reason }

// HTTPError is aiohttp's ClientResponseError: an answer with a failing status.
type HTTPError struct {
	Status  int
	Message string // the reason phrase: "Service Unavailable"
}

func (e *HTTPError) Error() string { return fmt.Sprintf("%d, message=%q", e.Status, e.Message) }

// pyError is an error with the name of the Python exception it stands for, for describe() and the login reasons.
type pyError struct {
	name, text string
	value      bool // a ValueError (or a subclass): the token answer could not be read
}

func (e *pyError) Error() string { return e.text }

func valueError(name, text string) error { return &pyError{name: name, text: text, value: true} }

// transport turns a failed request into the aiohttp exception it would have been.
func transport(err error) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &pyError{name: "TimeoutError"}
	}
	return &pyError{name: "ClientConnectorError", text: err.Error()}
}

func pyName(err error) string {
	var p *pyError
	var h *HTTPError
	switch {
	case errors.As(err, &h):
		return "ClientResponseError"
	case errors.As(err, &p):
		return p.name
	}
	return "Exception"
}

// Describe is an error for the status and the notices. HTTP errors by their status: aiohttp's text has the URL.
func Describe(err error) string {
	var h *HTTPError
	if errors.As(err, &h) {
		return strings.TrimSpace(fmt.Sprintf("HTTP %d %s", h.Status, h.Message))
	}
	name, text := pyName(err), err.Error()
	if text == "" {
		return name
	}
	return name + ": " + text
}

func httpError(response *http.Response) error {
	message := strings.TrimSpace(strings.TrimPrefix(response.Status, strconv.Itoa(response.StatusCode)))
	return &HTTPError{Status: response.StatusCode, Message: message}
}

// --- the login ----------------------------------------------------------------------

type pending struct{ state, verifier, redirectURI string }

// record is kv "hseapp.auth", in Python's key order with float unix seconds.
type record struct {
	AccessToken      any          `json:"access_token"`
	ExpiresAt        pyjson.Float `json:"expires_at"`
	RefreshToken     any          `json:"refresh_token"`
	RefreshExpiresAt pyjson.Float `json:"refresh_expires_at"`
	RefreshedAt      pyjson.Float `json:"refreshed_at"`
}

// Auth keeps the tokens in kv; the refresh token rotates, so every answer is saved at once.
type Auth struct {
	store    *store.Store
	now      func() time.Time
	TokenURL string // replaced in tests
	AuthURL  string

	pendingMu sync.Mutex
	pending   []pending // oldest first
	// One token request at a time: a refresh and a login, or two refreshes, must not overwrite each other.
	lock sync.Mutex
}

// NewAuth makes the token keeper; now is read at every use, so a test may change what it returns.
func NewAuth(s *store.Store, now func() time.Time) *Auth {
	if now == nil {
		now = clock.NowUTC
	}
	return &Auth{store: s, now: now, TokenURL: TokenURL, AuthURL: AuthURL}
}

// seconds is datetime.timestamp(): microseconds, as Python keeps them.
func (a *Auth) seconds() float64 { return float64(a.now().UnixMicro()) / 1e6 }

// Data is the stored tokens, {} when none.
func (a *Auth) Data() map[string]any {
	var data map[string]any
	if _, err := a.store.GetJSON(AuthKey, &data); err != nil {
		slog.Error("HSE App X: cannot read the tokens", "err", err)
	}
	if data == nil {
		data = map[string]any{}
	}
	return data
}

func number(data map[string]any, key string) float64 {
	if f, ok := data[key].(float64); ok {
		return f
	}
	return 0
}

// LoggedIn: a refresh token whose window has not closed.
func (a *Auth) LoggedIn() bool {
	data := a.Data()
	return truthy(data["refresh_token"]) && number(data, "refresh_expires_at") > a.seconds()
}

// Ever: tokens were saved once (erased ones too): a login needed now means the session ended.
func (a *Auth) Ever() bool {
	var value any
	found, err := a.store.GetJSON(AuthKey, &value)
	return err == nil && found && value != nil
}

// Save keeps a token answer; a ValueError-like error when it has no tokens.
func (a *Auth) Save(answer any) error {
	m, ok := answer.(map[string]any)
	if !ok || !truthy(m["access_token"]) || !truthy(m["refresh_token"]) {
		return valueError("ValueError", "token response without tokens")
	}
	expiresIn, err := pyInt(get(m, "expires_in", json.Number("300")))
	if err != nil {
		return err
	}
	refreshIn := any(json.Number(strconv.Itoa(RefreshLifetime)))
	if v := m["refresh_expires_in"]; truthy(v) {
		refreshIn = v
	}
	refreshExpiresIn, err := pyInt(refreshIn)
	if err != nil {
		return err
	}
	now := a.seconds()
	return a.store.PutJSON(AuthKey, record{
		AccessToken:      m["access_token"],
		ExpiresAt:        pyjson.Float(now + float64(expiresIn)),
		RefreshToken:     m["refresh_token"],
		RefreshExpiresAt: pyjson.Float(now + float64(refreshExpiresIn)),
		RefreshedAt:      pyjson.Float(now),
	})
}

// Forget erases the tokens, keeping the key: present but empty means the session ended.
func (a *Auth) Forget() error { return a.store.PutJSON(AuthKey, map[string]any{}) }

// AccessToken is a valid access token, refreshed when under 10 minutes are left or when forced.
func (a *Auth) AccessToken(ctx context.Context, client *http.Client, force bool) (string, error) {
	a.lock.Lock()
	defer a.lock.Unlock()
	data := a.Data()
	if !force && truthy(data["access_token"]) && number(data, "expires_at")-RefreshMargin > a.seconds() {
		return pyStr(data["access_token"]), nil
	}
	if !truthy(data["refresh_token"]) {
		return "", &LoginNeededError{Reason: "not logged in"}
	}
	err := a.tokenRequest(ctx, client, [][2]string{{"grant_type", "refresh_token"},
		{"refresh_token", pyStr(data["refresh_token"])}})
	if err != nil {
		return "", err
	}
	return pyStr(a.Data()["access_token"]), nil
}

// tokenRequest is called under the lock. The body is never logged: it holds the tokens.
func (a *Auth) tokenRequest(ctx context.Context, client *http.Client, fields [][2]string) error {
	form := url.Values{"client_id": {ClientID}}
	for _, f := range fields {
		form.Set(f[0], f[1])
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return transport(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return transport(err)
	}
	if response.StatusCode == 400 || response.StatusCode == 401 {
		// invalid_grant: the session ended (21 days without a refresh, or a logout).
		reason := jsonField(body, "error")
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", response.StatusCode)
		}
		if form.Get("grant_type") == "refresh_token" {
			if err := a.Forget(); err != nil {
				return err
			}
		}
		return &LoginNeededError{Reason: reason}
	}
	if response.StatusCode >= 400 {
		return httpError(response)
	}
	answer, err := decode(body)
	if err != nil {
		return err
	}
	return a.Save(answer)
}

// LoginURL is the HSE login page for a return address; the link's state is kept with its PKCE verifier.
func (a *Auth) LoginURL(redirectURI string) string {
	verifier, state := tokenURLSafe(48), tokenURLSafe(16)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	a.pendingMu.Lock()
	if len(a.pending) > PendingLogins-1 {
		a.pending = a.pending[len(a.pending)-(PendingLogins-1):]
	}
	a.pending = append(append([]pending(nil), a.pending...), pending{state, verifier, redirectURI})
	a.pendingMu.Unlock()
	// urlencode in Python's order: url.Values would sort the keys.
	parts := [][2]string{{"client_id", ClientID}, {"redirect_uri", redirectURI}, {"response_type", "code"},
		{"scope", Scope}, {"state", state}, {"code_challenge", challenge}, {"code_challenge_method", "S256"}}
	query := make([]string, len(parts))
	for i, p := range parts {
		query[i] = url.QueryEscape(p[0]) + "=" + url.QueryEscape(p[1])
	}
	return a.AuthURL + "?" + strings.Join(query, "&")
}

func (a *Auth) takePending(state string) (pending, bool) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	for i, p := range a.pending {
		if p.state == state {
			a.pending = append(append([]pending(nil), a.pending[:i]...), a.pending[i+1:]...)
			return p, true
		}
	}
	return pending{}, false
}

var oauthError = regexp.MustCompile(`^[a-z_]{1,40}$`)

// FinishLogin takes the return address with ?state=…&code=…, from the callback or pasted by hand. The reasons the
// owner reads are *i18n.Error; a failed request comes back as it is (Login words it).
func (a *Auth) FinishLogin(ctx context.Context, client *http.Client, returned string) error {
	query := parseQuery(pytext.Strip(returned))
	code, state := query["code"], query["state"]
	if code == "" {
		if reason := query["error"]; reason != "" {
			// D79: the callback is anyone's link: only a short OAuth error code is shown, never their text.
			why := "an error"
			if oauthError.MatchString(reason) {
				why = reason
			}
			return i18n.Err("HSE did not let you in: "+why, "ВШЭ не пустила: "+why)
		}
		return i18n.Err("the address has no code: copy the whole address from the address bar",
			"в адресе нет кода: скопируйте адрес из адресной строки целиком")
	}
	var p pending
	var ok bool
	if state != "" {
		p, ok = a.takePending(state)
	}
	if !ok {
		return i18n.Err("this login link is out of date: open a new one in Machine → HSE App X",
			"эта ссылка входа устарела: откройте новую в «Машина → HSE App X»")
	}
	a.lock.Lock()
	err := a.tokenRequest(ctx, client, [][2]string{{"grant_type", "authorization_code"}, {"code", code},
		{"redirect_uri", p.redirectURI}, {"code_verifier", p.verifier}})
	a.lock.Unlock()
	var login *LoginNeededError
	var py *pyError
	switch {
	case errors.As(err, &login):
		return i18n.Err(fmt.Sprintf("HSE did not take the code (%s): log in again", login.Reason),
			fmt.Sprintf("ВШЭ не приняла код (%s): войдите ещё раз", login.Reason))
	case errors.As(err, &py) && py.value:
		return i18n.Err("the HSE login server gave an unclear answer: log in again",
			"сервер входа ВШЭ ответил непонятно: войдите ещё раз")
	}
	return err
}

// parseQuery is parse_qs(urlsplit(url).query) with the first value of each name; blank values are dropped.
func parseQuery(address string) map[string]string {
	if i := strings.IndexByte(address, '#'); i >= 0 {
		address = address[:i]
	}
	out := map[string]string{}
	i := strings.IndexByte(address, '?')
	if i < 0 {
		return out
	}
	for _, pair := range strings.Split(address[i+1:], "&") {
		name, value, found := strings.Cut(pair, "=")
		if !found || value == "" {
			continue
		}
		name, value = unquotePlus(name), unquotePlus(value)
		if _, seen := out[name]; !seen && value != "" {
			out[name] = value
		}
	}
	return out
}

func unquotePlus(s string) string {
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return strings.ReplaceAll(s, "+", " ")
}

func jsonField(body []byte, name string) string {
	value, err := decode(body)
	if err != nil {
		return ""
	}
	m, ok := value.(map[string]any)
	if !ok || !truthy(m[name]) {
		return ""
	}
	return pytext.Cut(pyStr(m[name]), 100)
}

func tokenURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// --- the deadlines -----------------------------------------------------------------

// Raw is one deadline as HSE App X gives it.
type Raw struct {
	ID         string
	Title      string    // Russian, whitespace collapsed
	Discipline string    // cut at the first " (": the course as HSE App X names it; "" is None
	Due        time.Time // the Moscow date: midnight in clock.TZ
}

// TrimDiscipline: 'Дискретная математика (2026/2027 модули: 1,2,3,4) (Сидоров С.С.)' -> 'Дискретная математика'.
// "" is None.
func TrimDiscipline(name any) string {
	if !truthy(name) {
		return ""
	}
	before, _, _ := strings.Cut(pyStr(name), " (")
	return pytext.Strip(before)
}

var isoLayouts = func() []string {
	var out []string
	for _, sep := range []string{"T", " "} {
		for _, t := range []string{"15", "15:04", "15:04:05", "15:04:05.999999999"} {
			for _, zone := range []string{"", "-07:00", "-0700", "-07", "-07:00:05"} {
				out = append(out, "2006-01-02"+sep+t+zone)
			}
		}
	}
	return append(out, "2006-01-02")
}()

// DueDate reads an ISO UTC time as the Moscow date: 20:59Z is 23:59 of the same day in Moscow. A time without a
// zone is UTC, as the API gives it.
func DueDate(expiresAt string) (time.Time, error) {
	text := strings.ReplaceAll(expiresAt, "Z", "+00:00")
	for _, layout := range isoLayouts {
		if moment, err := time.Parse(layout, text); err == nil {
			return clock.LocalDay(moment), nil
		}
	}
	return time.Time{}, fmt.Errorf("Invalid isoformat string: %q", expiresAt)
}

// Parse keeps the ACTIVE deadlines, or with completed COMPLETED and COMPLETED_LATE instead. Items are decoded JSON
// (numbers as json.Number).
func Parse(items []any, completed bool) []Raw {
	result := []Raw{}
	for _, entry := range items {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		status := strings.ToUpper(pyStr(get(item, "completion_status", "ACTIVE")))
		if strings.HasPrefix(status, "COMPLETED") != completed || (!completed && status != "ACTIVE") {
			continue
		}
		expires, ok := item["expires_at"].(string)
		var due time.Time
		var err error
		if ok {
			due, err = DueDate(expires)
		}
		if !ok || err != nil {
			slog.Warn("HSE App X deadline without a valid expires_at: " + pytext.Cut(pyStr(item["title"]), 80))
			continue
		}
		title := ""
		if truthy(item["title"]) {
			title = strings.Join(pytext.Split(pyStr(item["title"])), " ")
		}
		if title == "" {
			continue
		}
		id := title
		if truthy(item["id"]) {
			id = pyStr(item["id"])
		}
		result = append(result, Raw{ID: id, Title: title, Discipline: TrimDiscipline(item["discipline"]), Due: due})
	}
	return result
}

// --- the source ---------------------------------------------------------------------

// Status is status(): never the tokens. The processing adds items, done and labels_error.
type Status struct {
	Enabled     bool    `json:"enabled"`
	LoggedIn    bool    `json:"logged_in"`
	OKAt        *string `json:"ok_at"`
	Error       *string `json:"error"`
	LoginNeeded bool    `json:"login_needed"`
}

// HseApp is the source.
type HseApp struct {
	store       *store.Store
	settings    *settings.Settings
	CallbackURL string // PUBLIC_URL + CallbackPath
	Now         func() time.Time
	Auth        *Auth
	Client      *http.Client
	Deadlines   string // the deadlines URL, replaced in tests

	// OnPoll gets both lists of a good poll: active, done.
	OnPoll func(ctx context.Context, active, done []Raw) error
	// Notify is a line for the owner (Web Push); login true: a login is needed, «Machine → HSE App X».
	Notify func(text i18n.Phrase, login bool)

	mu           sync.Mutex // the state below and the told flag in kv
	okAt         string
	err          string
	failingSince time.Time
	toldDown     bool
	wake         chan struct{}
	nap          func(ctx context.Context, d time.Duration) error
}

// New makes the source; now nil is clock.NowUTC.
func New(s *store.Store, st *settings.Settings, callbackURL string, now func() time.Time) *HseApp {
	h := &HseApp{store: s, settings: st, CallbackURL: callbackURL, Client: &http.Client{},
		Deadlines: DeadlinesURL, wake: make(chan struct{}, 1)}
	h.Now = now
	if h.Now == nil {
		h.Now = clock.NowUTC
	}
	h.Auth = NewAuth(s, func() time.Time { return h.Now() })
	h.nap = h.defaultNap
	return h
}

// Enabled is the setting hseapp_enabled.
func (h *HseApp) Enabled() bool {
	enabled, err := h.settings.Bool("hseapp_enabled")
	if err != nil {
		slog.Error("HSE App X: cannot read hseapp_enabled", "err", err)
	}
	return enabled
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Status never holds the tokens.
func (h *HseApp) Status() Status {
	enabled, loggedIn := h.Enabled(), h.Auth.LoggedIn()
	h.mu.Lock()
	defer h.mu.Unlock()
	return Status{Enabled: enabled, LoggedIn: loggedIn, OKAt: optional(h.okAt), Error: optional(h.err),
		LoginNeeded: enabled && !loggedIn}
}

// --- the owner's side (the web app) ---------------------------------------------------

// LoginLink is the HSE login page; the browser comes back to our callback, or with app to the app's own address,
// whose return address is pasted by hand.
func (h *HseApp) LoginLink(app bool) string {
	if app {
		return h.Auth.LoginURL(AppRedirectURI)
	}
	return h.Auth.LoginURL(h.CallbackURL)
}

// Login finishes a login with the return address; the reason the owner reads is an *i18n.Error.
func (h *HseApp) Login(ctx context.Context, returned string) error {
	ctx, cancel := context.WithTimeout(ctx, LoginTimeout)
	defer cancel()
	err := h.Auth.FinishLogin(ctx, h.Client, returned)
	var phrase *i18n.Error
	var py *pyError
	var status *HTTPError
	switch {
	case err == nil:
	case errors.As(err, &phrase):
		return err
	case errors.As(err, &status) || (errors.As(err, &py) && !py.value):
		what := pyName(err)
		return i18n.Err(fmt.Sprintf("the HSE login server is unavailable (%s): try again", what),
			fmt.Sprintf("сервер входа ВШЭ недоступен (%s): попробуйте ещё раз", what))
	default:
		return err
	}
	h.mu.Lock()
	h.err = ""
	h.mu.Unlock()
	slog.Info("HSE App X: logged in")
	h.tellLogin()
	h.Wake() // the deadlines now, not in 15 minutes
	return nil
}

// SetEnabled turns the source on or off.
func (h *HseApp) SetEnabled(enabled bool) error {
	if _, err := h.settings.Set("hseapp_enabled", enabled); err != nil {
		return err
	}
	h.tellLogin()
	h.Wake()
	return nil
}

// Wake makes the loop poll now.
func (h *HseApp) Wake() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// --- polling --------------------------------------------------------------------------

// Run polls every 15 minutes until ctx is done.
func (h *HseApp) Run(ctx context.Context) error {
	for {
		if h.Enabled() && h.Auth.LoggedIn() {
			h.Poll(ctx)
		}
		h.tellLogin()
		if err := h.nap(ctx, PollEvery); err != nil {
			return err
		}
	}
}

// defaultNap waits until the next poll, or less after a login. A wake during a poll stays in the channel: one
// more poll.
func (h *HseApp) defaultNap(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.wake:
	case <-timer.C:
	}
	return nil
}

// Poll is one fetch of both lists; true when it went through and the processing got them.
func (h *HseApp) Poll(ctx context.Context) bool {
	active, done, err := h.fetchBoth(ctx)
	var login *LoginNeededError
	if errors.As(err, &login) {
		h.mu.Lock()
		h.err = LoginError + login.Reason
		h.mu.Unlock()
		slog.Warn(fmt.Sprintf("HSE App X: login needed (%s); log in from the web app", login.Reason))
		h.tellLogin()
		return false
	}
	if err != nil {
		h.failed(Describe(err))
		return false
	}
	h.recovered()
	slog.Info(fmt.Sprintf("HSE App X: %d deadlines, %d completed", len(active), len(done)))
	if h.OnPoll != nil {
		if err := h.OnPoll(ctx, active, done); err != nil {
			slog.Error("processing the HSE App X poll failed", "err", err)
		}
	}
	return true
}

func (h *HseApp) fetchBoth(ctx context.Context) ([]Raw, []Raw, error) {
	items, err := h.fetch(ctx, false)
	if err != nil {
		return nil, nil, err
	}
	active := Parse(items, false)
	items, err = h.fetch(ctx, true)
	if err != nil {
		return nil, nil, err
	}
	return active, Parse(items, true), nil
}

func (h *HseApp) fetch(ctx context.Context, completed bool) ([]any, error) {
	token, err := h.tokenWithin(ctx, false)
	if err != nil {
		return nil, err
	}
	items, cursor := []any{}, any(nil)
	for range MaxPages {
		var page any
		for attempt := range 2 {
			status, body, err := h.post(ctx, token, cursor, completed)
			if err != nil {
				return nil, err
			}
			if status.StatusCode != 401 || attempt == 1 {
				if status.StatusCode >= 400 {
					return nil, httpError(status)
				}
				if page, err = decode(body); err != nil {
					return nil, err
				}
				break
			}
			if token, err = h.tokenWithin(ctx, true); err != nil { // revoked early: once more
				return nil, err
			}
		}
		m, ok := page.(map[string]any)
		var list []any
		if ok && truthy(m["items"]) {
			list, ok = m["items"].([]any)
		}
		if !ok {
			return nil, valueError("ValueError", "unexpected answer of /v2/deadlines")
		}
		items = append(items, list...)
		cursor = m["next_cursor"]
		if !truthy(cursor) {
			break
		}
	}
	return items, nil
}

func (h *HseApp) tokenWithin(ctx context.Context, force bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	return h.Auth.AccessToken(ctx, h.Client, force)
}

func (h *HseApp) post(ctx context.Context, token string, cursor any, completed bool) (*http.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	address := h.Deadlines
	if truthy(cursor) {
		address += "?" + url.Values{"cursor": {pyStr(cursor)}}.Encode()
	}
	payload := `{"is_completed": false}`
	if completed {
		payload = `{"is_completed": true}`
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, strings.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept-Language", "ru")
	response, err := h.Client.Do(request)
	if err != nil {
		return nil, nil, transport(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, transport(err)
	}
	return response, body, nil
}

// --- the notices -------------------------------------------------------------------------

// tellLogin says one line per change of state: a login is needed (with the button), the login is back. Kept in kv,
// so a restart does not tell it again; turning the source off forgets it silently.
func (h *HseApp) tellLogin() {
	enabled, loggedIn := h.Enabled(), h.Auth.LoggedIn()
	h.mu.Lock()
	var told bool
	if _, err := h.store.GetJSON(ToldKey, &told); err != nil {
		slog.Error("HSE App X: cannot read the told flag", "err", err)
	}
	var text *i18n.Phrase
	login := false
	if enabled && !loggedIn && !told {
		h.put(ToldKey, true)
		var p i18n.Phrase
		if h.Auth.Ever() {
			why := ""
			if strings.HasPrefix(h.err, LoginError) {
				if reason := strings.TrimPrefix(h.err, LoginError); reason != "" {
					why = " (" + reason + ")"
				}
			}
			p = i18n.AlertP(fmt.Sprintf("The HSE App X login is no longer valid%s: its deadlines are not updated.\n"+
				"Log in again in the web app, Machine → HSE App X.", why),
				fmt.Sprintf("Вход в HSE App X больше не действует%s: дедлайны оттуда не обновляются.\n"+
					"Войти заново — в приложении, Машина → HSE App X.", why))
		} else {
			p = i18n.AlertP("An HSE App X login is needed: without it no deadlines come from there.\n"+
				"Log in in the web app, Machine → HSE App X.",
				"Нужен вход в HSE App X: без него дедлайны оттуда не приходят.\n"+
					"Войти — в приложении, Машина → HSE App X.")
		}
		text, login = &p, true
	} else if told && (loggedIn || !enabled) {
		h.put(ToldKey, false)
		if loggedIn {
			p := i18n.P("Logged in to HSE App X: the deadlines are updated.",
				"Вход в HSE App X выполнен: дедлайны обновляются.")
			text = &p
		}
	}
	h.mu.Unlock()
	if text != nil {
		h.notify(*text, login)
	}
}

func (h *HseApp) put(key string, value any) {
	if err := h.store.PutJSON(key, value); err != nil {
		slog.Error("HSE App X: cannot save "+key, "err", err)
	}
}

func (h *HseApp) failed(reason string) {
	slog.Warn("HSE App X poll failed: " + reason)
	now := h.Now()
	h.mu.Lock()
	h.err = reason
	if h.failingSince.IsZero() {
		h.failingSince = now
	}
	var text *i18n.Phrase
	if !h.toldDown && now.Sub(h.failingSince) >= StaleAfter {
		h.toldDown = true
		since := h.failingSince.In(clock.TZ).Format("02.01 15:04")
		p := i18n.AlertP(fmt.Sprintf("HSE App X is unavailable since %s: %s.\n"+
			"The deadlines have the last list received.", since, reason),
			fmt.Sprintf("HSE App X недоступен с %s: %s.\n"+
				"В дедлайнах — последний полученный список.", since, reason))
		text = &p
	}
	h.mu.Unlock()
	if text != nil {
		h.notify(*text, false)
	}
}

func (h *HseApp) recovered() {
	h.mu.Lock()
	wasDown := h.toldDown
	h.okAt, h.err = clock.LocalISO(h.Now()), ""
	h.failingSince, h.toldDown = time.Time{}, false
	h.mu.Unlock()
	if wasDown {
		h.notify(i18n.P("HSE App X is available again.", "HSE App X снова доступен."), false)
	}
}

func (h *HseApp) notify(text i18n.Phrase, login bool) {
	if h.Notify != nil {
		h.Notify(text, login)
	}
}

// --- the callback (public port) -----------------------------------------------------------

// Callback is GET CallbackPath: the browser after the HSE login (public, no session: only the browser that opened
// the link knows its state). The address carries the code: it is not logged.
func (h *HseApp) Callback(w http.ResponseWriter, r *http.Request) {
	lang, err := h.settings.String("language")
	if err != nil {
		lang = "en"
	}
	if err := h.Login(r.Context(), r.URL.RequestURI()); err != nil {
		why := strings.TrimRight(i18n.Reason(err, lang), ".")
		if first, size := utf8.DecodeRuneInString(why); size > 0 {
			why = strings.ToUpper(string(first)) + why[size:]
		}
		page(w, lang, i18n.Tr(lang, "The login failed", "Вход не удался"), why+".",
			i18n.Tr(lang, "Go back to infomaster and log in again: Machine → HSE App X.",
				"Вернитесь в infomaster и войдите ещё раз: Машина → HSE App X."), http.StatusBadRequest)
		return
	}
	page(w, lang, i18n.Tr(lang, "Logged in to HSE App X", "Вход в HSE App X выполнен"),
		i18n.Tr(lang, "The deadlines will be updated within a minute.", "Дедлайны обновятся в течение минуты."),
		i18n.Tr(lang, "This window can be closed: go back to infomaster.",
			"Это окно можно закрыть и вернуться в infomaster."), http.StatusOK)
}

// Page is the callback's page (actions.PAGE): {lang}, {title}, {text}, {hint}.
const Page = `<!doctype html>
<html lang="{lang}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HSE App X</title>
<style>
  body { margin: 48px auto; max-width: 480px; padding: 0 16px; font: 17px/1.5 -apple-system, system-ui, sans-serif;
         color: #111; background: #fff; }
  @media (prefers-color-scheme: dark) { body { color: #eee; background: #111; } }
  h1 { font-size: 22px; }
</style>
</head>
<body>
<h1>{title}</h1>
<p>{text}</p>
<p>{hint}</p>
</body>
</html>
`

// escape is Python's html.escape(quote=True).
var escape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace

func page(w http.ResponseWriter, lang, title, text, hint string, status int) {
	body := strings.NewReplacer("{lang}", lang, "{title}", escape(title), "{text}", escape(text),
		"{hint}", escape(hint)).Replace(Page)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// --- Python semantics of decoded JSON -------------------------------------------------------

// decode is json.loads with numbers kept as written; an empty body is None, as aiohttp's response.json() gives.
func decode(body []byte) (any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, valueError("JSONDecodeError", err.Error())
	}
	if d.More() {
		return nil, valueError("JSONDecodeError", "Extra data")
	}
	return value, nil
}

func get(m map[string]any, key string, fallback any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return fallback
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr is str() of a decoded JSON value.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return string(x)
	case float64:
		return pyjson.FormatFloat(x)
	}
	return fmt.Sprint(v)
}

// pyInt is int() of a decoded JSON value.
func pyInt(v any) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, nil
		}
		if f, err := x.Float64(); err == nil {
			return int64(f), nil
		}
	case float64:
		return int64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		if n, err := strconv.ParseInt(strings.ReplaceAll(pytext.Strip(x), "_", ""), 10, 64); err == nil {
			return n, nil
		}
		return 0, valueError("ValueError", fmt.Sprintf("invalid literal for int() with base 10: '%s'", x))
	}
	return 0, valueError("TypeError", "int() argument must be a string, a bytes-like object or a real number")
}
