// Package oauth is OAuth 2.1 of the connector (§8.5): infomaster is its own authorization server, for one person.
// The port of connector/oauth.py.
//
// What a remote MCP server needs (MCP authorization, revision 2026-07-28, and claude.ai's client):
//   - protected resource metadata (RFC 9728) at /.well-known/oauth-protected-resource/mcp and at the root path,
//     named by `resource_metadata` in the 401 of /mcp;
//   - authorization server metadata (RFC 8414) at /.well-known/oauth-authorization-server, the issuer being
//     PUBLIC_URL;
//   - Dynamic Client Registration (RFC 7591) at /oauth/register: claude.ai registers itself when it connects. Only the
//     redirect URIs of claude.ai and claude.com and loopback ones (Claude Code) are accepted. Client ID Metadata
//     Documents are not advertised, so claude.ai falls back to registration;
//   - /oauth/authorize with PKCE S256 only, `iss` in the answer (RFC 9207), the `resource` (RFC 8707) checked;
//   - /oauth/token: the code, then refresh tokens, which rotate. An access token lives an hour;
//   - /oauth/revoke (RFC 7009).
//
// There is no password: the authorize page asks the bot to send a one-time code to the owner's private chat
// (TG_OWNER_ID), and the owner types it in. Tokens and codes are kept as SHA-256 hashes; codes and the open authorize
// pages live in memory only (a restart asks to sign in again, nothing more).
package oauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Schema is the tables, verbatim from the Python service.
const Schema = `
create table if not exists connector_clients (  -- clients registered for the MCP connector (RFC 7591)
    id text primary key,        -- client_id
    name text not null,
    redirect_uris text not null,    -- JSON
    created_at integer not null     -- unix time
);
create table if not exists connector_grants (   -- connections the owner allowed: one refresh token each
    id integer primary key,
    client_id text not null,
    created_at integer not null,
    used_at integer not null,
    refresh_hash text,          -- SHA-256 of the refresh token
    refresh_until integer,
    previous_hash text,         -- the one it replaced, still good for REFRESH_GRACE
    previous_until integer,
    revoked_at integer
);
create table if not exists connector_tokens (   -- access tokens
    hash text primary key,      -- SHA-256
    grant_id integer not null,
    expires_at integer not null
);
`

const (
	AccessTTL     = 3600                     // seconds of an access token
	RefreshTTL    = 30 * 86400               // a refresh token unused this long is gone
	RefreshGrace  = 120                      // a rotated refresh token still works this long: two refreshes in flight
	CodeTTL       = 300                      // an authorization code
	PageTTL       = 600                      // an authorize page and the code the bot sent for it
	Attempts      = 3                        // wrong codes on one page
	ResendAfter   = 30                       // seconds between codes of one page
	CodesPerHour  = 6                        // codes the bot sends an hour, all pages together
	CodesPerDay   = 12                       // and a day: 36 guesses of a million, each code a message to the owner
	ClientsMax    = 50                       //
	PagesMax      = 20                       // open authorize pages kept, the oldest go first
	NameChars     = 80                       //
	CallbackPath  = "/api/mcp/auth_callback" // the one path of the trusted hosts: no other page of theirs gets a code
	resourceName  = "infomaster"
	defaultClient = "client"
)

// TrustedHosts are where the browser may be sent back with a code: claude.ai's callback (the web, desktop and phone
// apps); Loopback any port of a loopback address (Claude Code).
var (
	TrustedHosts = []string{"claude.ai", "claude.com"}
	Loopback     = []string{"localhost", "127.0.0.1", "::1"}
)

// NoStore are the headers of every token answer.
var NoStore = map[string]string{"Cache-Control": "no-store", "Pragma": "no-cache"}

// Error is an OAuth error: its code, its description and the HTTP status (302 for one sent back to the client, at
// Location).
type Error struct {
	Code, Description string
	Status            int
	Location          string
}

func (e *Error) Error() string { return e.Description }

func oauthError(code, description string) *Error {
	return &Error{Code: code, Description: description, Status: 400}
}

// Body is the JSON answer of the error: {error, error_description}.
func (e *Error) Body() httpx.Object {
	return httpx.Object{{Key: "error", Value: e.Code}, {Key: "error_description", Value: e.Description}}
}

// Digest is the SHA-256 hex of a token, as it is stored.
func Digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// S256 is the PKCE challenge of a verifier.
func S256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// TokenURLSafe is secrets.token_urlsafe(n).
func TokenURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func contains(list []string, item string) bool {
	for _, x := range list {
		if x == item {
			return true
		}
	}
	return false
}

// RedirectAllowed is an address a client may register.
func RedirectAllowed(uri string) bool {
	parts := urlsplit(uri)
	if parts.Invalid {
		return false
	}
	port, err := parts.Port()
	if err != nil {
		return false
	}
	if parts.Fragment != "" || parts.Username() != "" || parts.Password() != "" {
		return false
	}
	if parts.Scheme == "https" && contains(TrustedHosts, parts.Hostname()) && port < 0 {
		return parts.Path == CallbackPath
	}
	return parts.Scheme == "http" && contains(Loopback, parts.Hostname())
}

// SameRedirect is exact, except the port of a loopback address, which a native client picks per run (RFC 8252,
// 7.3).
func SameRedirect(registered, given string) bool {
	if registered == given {
		return true
	}
	a, b := urlsplit(registered), urlsplit(given)
	if a.Invalid || b.Invalid {
		return false
	}
	return a.Scheme == "http" && b.Scheme == "http" && contains(Loopback, a.Hostname()) &&
		a.Hostname() == b.Hostname() && a.Path == b.Path && a.Query == b.Query
}

// Page is an open authorize page: the request it answers and the code the bot sent for it.
type Page struct {
	ClientID    string
	ClientName  string
	RedirectURI string
	State       *string
	Challenge   string
	Resource    *string
	Opened      float64
	CodeHash    *string
	SentAt      float64
	Attempts    int
}

// Code is an authorization code, by its hash.
type Code struct {
	ClientID    string
	RedirectURI string
	Challenge   string
	Resource    *string
	Expires     float64
}

type spentCode struct {
	grantID int64
	until   float64
}

// Client is a registered client.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    int64
}

// OAuth is the authorization server.
type OAuth struct {
	db     *sql.DB
	Issuer string // PUBLIC_URL
	// SendCode sends a text to the owner's private chat with the bot; nil while the bot is not running.
	SendCode func(ctx context.Context, text string) error
	Lang     func() string  // the app's language
	Now      func() float64 // unix seconds

	mu    sync.Mutex
	pages map[string]*Page
	order []string // page ids, the oldest first
	codes map[string]*Code
	spent map[string]spentCode // a used code's hash -> its grant, until
	sent  []float64            // when codes went out, the last day
}

// New makes the tables. lang and now may be nil: English and the clock.
func New(s *store.Store, issuer string, sendCode func(context.Context, string) error, lang func() string,
	now func() float64) (*OAuth, error) {
	if _, err := s.DB.Exec(Schema); err != nil {
		return nil, err
	}
	if lang == nil {
		lang = func() string { return "en" }
	}
	if now == nil {
		now = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	}
	return &OAuth{db: s.DB, Issuer: issuer, SendCode: sendCode, Lang: lang, Now: now, pages: map[string]*Page{},
		codes: map[string]*Code{}, spent: map[string]spentCode{}}, nil
}

// Resource is the protected resource: PUBLIC_URL/mcp.
func (o *OAuth) Resource() string { return o.Issuer + "/mcp" }

// ResourceMetadata is the address of the protected resource metadata, named in the 401 of /mcp.
func (o *OAuth) ResourceMetadata() string {
	return o.Issuer + "/.well-known/oauth-protected-resource/mcp"
}

// --- metadata ---------------------------------------------------------------------------------

// ProtectedResource is RFC 9728's metadata.
func (o *OAuth) ProtectedResource() httpx.Object {
	return httpx.Object{{Key: "resource", Value: o.Resource()}, {Key: "authorization_servers", Value: []string{o.Issuer}},
		{Key: "bearer_methods_supported", Value: []string{"header"}}, {Key: "resource_name", Value: resourceName}}
}

// AuthorizationServer is RFC 8414's metadata.
func (o *OAuth) AuthorizationServer() httpx.Object {
	return httpx.Object{{Key: "issuer", Value: o.Issuer},
		{Key: "authorization_endpoint", Value: o.Issuer + "/oauth/authorize"},
		{Key: "token_endpoint", Value: o.Issuer + "/oauth/token"},
		{Key: "registration_endpoint", Value: o.Issuer + "/oauth/register"},
		{Key: "revocation_endpoint", Value: o.Issuer + "/oauth/revoke"},
		{Key: "response_types_supported", Value: []string{"code"}},
		{Key: "grant_types_supported", Value: []string{"authorization_code", "refresh_token"}},
		{Key: "code_challenge_methods_supported", Value: []string{"S256"}},
		{Key: "token_endpoint_auth_methods_supported", Value: []string{"none"}},
		{Key: "revocation_endpoint_auth_methods_supported", Value: []string{"none"}},
		{Key: "authorization_response_iss_parameter_supported", Value: true}}
}

// ResourceOK reports that the `resource` of a request names this server, or that none is given (nil).
func (o *OAuth) ResourceOK(resource *string) bool {
	if resource == nil {
		return true
	}
	canonical := strings.ToLower(strings.TrimRight(*resource, "/"))
	return canonical == strings.ToLower(o.Resource()) || canonical == strings.ToLower(o.Issuer)
}

// --- registration -----------------------------------------------------------------------------

// Register is RFC 7591: meta is the decoded JSON body (httpx.Decode).
func (o *OAuth) Register(meta any) (httpx.Object, error) {
	fields, ok := meta.(map[string]any)
	if !ok {
		return nil, oauthError("invalid_client_metadata", "expected a JSON object")
	}
	list, ok := fields["redirect_uris"].([]any)
	uris := make([]string, 0, len(list))
	for _, item := range list {
		if uri, isText := item.(string); isText {
			uris = append(uris, uri)
		} else {
			ok = false
		}
	}
	if !ok || len(uris) == 0 {
		return nil, oauthError("invalid_redirect_uri", "redirect_uris: a list of addresses")
	}
	for _, uri := range uris {
		if !RedirectAllowed(uri) {
			return nil, oauthError("invalid_redirect_uri",
				"only claude.ai's callback and loopback addresses are accepted")
		}
	}
	if grants, present := fields["grant_types"]; present {
		list, ok := grants.([]any)
		found := false
		for _, item := range list {
			if item == "authorization_code" {
				found = true
			}
		}
		if !ok || !found {
			return nil, oauthError("invalid_client_metadata", "grant_types must include authorization_code")
		}
	}
	if types, present := fields["response_types"]; present {
		list, ok := types.([]any)
		if !ok || len(list) != 1 || list[0] != "code" {
			return nil, oauthError("invalid_client_metadata", "response_types: only code")
		}
	}
	name, _ := fields["client_name"].(string)
	name = pytext.Cut(strings.Join(pytext.Split(name), " "), NameChars)
	if name == "" {
		name = urlsplit(uris[0]).Hostname()
	}
	if name == "" {
		name = defaultClient
	}
	now := int64(o.Now())
	clientID := TokenURLSafe(16)
	stored, err := pyjson.DumpsASCII(uris)
	if err != nil {
		return nil, err
	}
	tx, err := o.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Registrations that never led to a connection go after a day; the oldest beyond the cap too.
	if _, err := tx.Exec("delete from connector_clients where created_at < ? and id not in "+
		"(select client_id from connector_grants where revoked_at is null)", now-86400); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("delete from connector_clients where id in (select id from connector_clients where id "+
		"not in (select client_id from connector_grants where revoked_at is null) "+
		"order by created_at desc limit -1 offset ?)", ClientsMax-1); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("insert into connector_clients(id, name, redirect_uris, created_at) values(?, ?, ?, ?)",
		clientID, name, stored, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "client_id", Value: clientID}, {Key: "client_id_issued_at", Value: now},
		{Key: "client_name", Value: name}, {Key: "redirect_uris", Value: uris},
		{Key: "grant_types", Value: []string{"authorization_code", "refresh_token"}},
		{Key: "response_types", Value: []string{"code"}}, {Key: "token_endpoint_auth_method", Value: "none"}}, nil
}

// Client is a registered client, nil for none.
func (o *OAuth) Client(clientID string) (*Client, error) {
	var c Client
	var uris string
	err := o.db.QueryRow("select id, name, redirect_uris, created_at from connector_clients where id = ?",
		clientID).Scan(&c.ID, &c.Name, &uris, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decoded, err := httpx.Decode([]byte(uris))
	if err != nil {
		return nil, err
	}
	list, _ := decoded.([]any)
	for _, item := range list {
		if uri, ok := item.(string); ok {
			c.RedirectURIs = append(c.RedirectURIs, uri)
		}
	}
	return &c, nil
}

// --- the authorize page -----------------------------------------------------------------------

// Params are form or query fields as Python's dict reads them: a missing field is absent, not "".
type Params map[string]string

func (p Params) get(name string) *string {
	if value, ok := p[name]; ok {
		return &value
	}
	return nil
}

// First is the fields of a query or form, the first value of each (MultiDict.get).
func First(values url.Values) Params {
	out := Params{}
	for key, list := range values {
		if len(list) > 0 {
			out[key] = list[0]
		}
	}
	return out
}

// Last is the fields, the last value of each ({key: value for key, value in form.items()}).
func Last(values url.Values) Params {
	out := Params{}
	for key, list := range values {
		if len(list) > 0 {
			out[key] = list[len(list)-1]
		}
	}
	return out
}

// OpenPage is a page for a valid authorization request. An *Error with status 400 when the client or its redirect
// cannot be trusted (nothing is sent back then); with status 302 for an error to send back to the client.
func (o *OAuth) OpenPage(query Params) (string, *Page, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expire()
	var client *Client
	if id := query.get("client_id"); id != nil {
		var err error
		if client, err = o.Client(*id); err != nil {
			return "", nil, err
		}
	}
	if client == nil {
		return "", nil, oauthError("invalid_client", "unknown client: connect again from the client")
	}
	redirect := query.get("redirect_uri")
	matched := false
	if redirect != nil {
		for _, uri := range client.RedirectURIs {
			if SameRedirect(uri, *redirect) {
				matched = true
				break
			}
		}
	}
	if !matched {
		return "", nil, oauthError("invalid_request", "the redirect_uri is not the client's")
	}
	page := &Page{ClientID: client.ID, ClientName: client.Name, RedirectURI: *redirect, State: query.get("state"),
		Resource: query.get("resource"), Opened: o.Now()}
	if rt := query.get("response_type"); rt == nil || *rt != "code" {
		return "", nil, o.back(page, "unsupported_response_type", "only the code flow")
	}
	challenge := query["code_challenge"]
	method := query.get("code_challenge_method")
	length := utf8.RuneCountInString(challenge)
	if method == nil || *method != "S256" || length < 43 || length > 128 {
		return "", nil, o.back(page, "invalid_request", "PKCE with S256 is required")
	}
	if !o.ResourceOK(page.Resource) {
		return "", nil, o.back(page, "invalid_target", "the resource is not this server")
	}
	page.Challenge = challenge
	pageID := TokenURLSafe(24)
	o.pages[pageID] = page
	o.order = append(o.order, pageID)
	for len(o.pages) > PagesMax { // the page is public: anyone may open it, memory is not theirs
		o.drop(o.order[0])
	}
	return pageID, page, nil
}

func (o *OAuth) drop(pageID string) {
	delete(o.pages, pageID)
	for i, id := range o.order {
		if id == pageID {
			o.order = append(o.order[:i:i], o.order[i+1:]...)
			break
		}
	}
}

func (o *OAuth) back(page *Page, code, description string) *Error {
	return &Error{Code: code, Description: description, Status: 302,
		Location: o.Redirect(page, [][2]string{{"error", code}, {"error_description", description}})}
}

// Redirect is the page's redirect_uri with the params, the state and the issuer, in that order.
func (o *OAuth) Redirect(page *Page, params [][2]string) string {
	if page.State != nil {
		params = append(params, [2]string{"state", *page.State})
	}
	params = append(params, [2]string{"iss", o.Issuer})
	var b strings.Builder
	for i, param := range params {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(param[0]))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(param[1]))
	}
	separator := "?"
	if urlsplit(page.RedirectURI).Query != "" {
		separator = "&"
	}
	return page.RedirectURI + separator + b.String()
}

// Page is an open page, nil when it is gone.
func (o *OAuth) Page(pageID string) *Page {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expire()
	return o.pages[pageID]
}

// Send has the bot send a code for the page; the reason it did not, or "".
func (o *OAuth) Send(ctx context.Context, pageID string) string {
	lang := o.Lang()
	o.mu.Lock()
	page, now := o.pages[pageID], o.Now()
	if page == nil {
		o.mu.Unlock()
		return i18n.Tr(lang, "This page has expired: connect again from Claude.",
			"Страница устарела: подключитесь заново из Claude.")
	}
	if now-page.SentAt < ResendAfter {
		o.mu.Unlock()
		return i18n.Tr(lang, "A code was sent a moment ago: check the chat with the bot.",
			"Код только что отправлен: посмотрите чат с ботом.")
	}
	kept, hour := o.sent[:0], 0
	for _, at := range o.sent {
		if now-at < 86400 {
			kept = append(kept, at)
			if now-at < 3600 {
				hour++
			}
		}
	}
	o.sent = kept
	if hour >= CodesPerHour || len(o.sent) >= CodesPerDay {
		o.mu.Unlock()
		return i18n.Tr(lang, "Too many codes for now: try again later.", "Слишком много кодов: попробуйте позже.")
	}
	send := o.SendCode
	if send == nil {
		o.mu.Unlock()
		return i18n.Tr(lang, "The bot is not running.", "Бот не запущен.")
	}
	// Counted before the bot is awaited: requests at the same moment cannot all pass the limits.
	page.CodeHash, page.SentAt = nil, now
	o.sent = append(o.sent, now)
	clientName := page.ClientName
	o.mu.Unlock()

	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	code := fmt.Sprintf("%06d", n.Int64())
	text := i18n.Tr(lang, code+" is the code to connect «"+clientName+"» to infomaster. It reads and changes "+
		"your deadlines, timetable, grades, chats and settings. If you did not ask for it, ignore this message.",
		code+" — код, чтобы подключить «"+clientName+"» к infomaster. Он читает и меняет дедлайны, "+
			"расписание, оценки, чаты и настройки. Если вы его не запрашивали, не обращайте внимания.")
	if err := send(ctx, text); err != nil {
		log.Printf("the bot could not send the connector's code: %v", err)
		return i18n.Tr(lang, "The bot could not write to you: open the chat with the bot, press Start and try again.",
			"Бот не смог вам написать: откройте чат с ботом, нажмите «Старт» и попробуйте снова.")
	}
	o.mu.Lock()
	hash := Digest(code)
	page.CodeHash, page.Attempts = &hash, 0
	o.mu.Unlock()
	return ""
}

// Allow is the redirect with an authorization code for the right one-time code; an *Error (status 400) else. code is
// nil when the form has none.
func (o *OAuth) Allow(pageID string, code *string) (string, error) {
	lang := o.Lang()
	o.mu.Lock()
	defer o.mu.Unlock()
	page := o.pages[pageID]
	if page == nil {
		return "", oauthError("access_denied", i18n.Tr(lang, "This page has expired: connect again from Claude.",
			"Страница устарела: подключитесь заново из Claude."))
	}
	if page.CodeHash == nil {
		return "", oauthError("access_denied", i18n.Tr(lang, "Ask for a code first.", "Сначала запросите код."))
	}
	page.Attempts++
	typed := ""
	if code != nil {
		typed = strings.Join(pytext.Split(*code), "")
	}
	if !hmac.Equal([]byte(Digest(typed)), []byte(*page.CodeHash)) {
		if page.Attempts >= Attempts {
			page.CodeHash = nil
			return "", oauthError("access_denied", i18n.Tr(lang, "Wrong code too many times: ask for a new one.",
				"Слишком много неверных кодов: запросите новый."))
		}
		return "", oauthError("access_denied", i18n.Tr(lang, "Wrong code.", "Неверный код."))
	}
	o.drop(pageID)
	authCode := TokenURLSafe(32)
	o.codes[Digest(authCode)] = &Code{ClientID: page.ClientID, RedirectURI: page.RedirectURI,
		Challenge: page.Challenge, Resource: page.Resource, Expires: o.Now() + CodeTTL}
	log.Printf("the owner allowed the connector client %s", page.ClientName)
	return o.Redirect(page, [][2]string{{"code", authCode}}), nil
}

// Deny is the redirect that tells the client the owner said no; "" when the page is gone.
func (o *OAuth) Deny(pageID string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	page := o.pages[pageID]
	if page == nil {
		return ""
	}
	o.drop(pageID)
	return o.Redirect(page, [][2]string{{"error", "access_denied"}, {"error_description", "the owner said no"}})
}

func (o *OAuth) expire() {
	now := o.Now()
	for _, id := range append([]string(nil), o.order...) {
		if now-o.pages[id].Opened > PageTTL {
			o.drop(id)
		}
	}
	for key, code := range o.codes {
		if code.Expires < now {
			delete(o.codes, key)
		}
	}
	for key, spent := range o.spent {
		if spent.until < now {
			delete(o.spent, key)
		}
	}
}

// --- tokens -----------------------------------------------------------------------------------

// Token is POST /oauth/token: the form's fields as Python's dict has them.
func (o *OAuth) Token(form Params) (httpx.Object, error) {
	switch grant := form.get("grant_type"); {
	case grant != nil && *grant == "authorization_code":
		return o.exchange(form)
	case grant != nil && *grant == "refresh_token":
		return o.refresh(form)
	}
	return nil, oauthError("unsupported_grant_type", "authorization_code or refresh_token")
}

func truthy(s *string) bool { return s != nil && *s != "" }

func (o *OAuth) exchange(form Params) (httpx.Object, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expire()
	codeHash := Digest(form["code"])
	code := o.codes[codeHash]
	delete(o.codes, codeHash)
	if code == nil {
		if spent, ok := o.spent[codeHash]; ok { // a code used twice: the tokens of its first use are not to be trusted
			delete(o.spent, codeHash)
			if _, err := o.RevokeGrant(spent.grantID); err != nil {
				return nil, err
			}
		}
		return nil, oauthError("invalid_grant", "the code is unknown, used or expired")
	}
	if id := form.get("client_id"); id == nil || *id != code.ClientID {
		return nil, oauthError("invalid_grant", "the code was issued to another client")
	}
	if uri := form.get("redirect_uri"); uri == nil || *uri != code.RedirectURI {
		return nil, oauthError("invalid_grant", "redirect_uri differs from the authorization request")
	}
	verifier := form["code_verifier"]
	length := utf8.RuneCountInString(verifier)
	if length < 43 || length > 128 || !hmac.Equal([]byte(S256(verifier)), []byte(code.Challenge)) {
		return nil, oauthError("invalid_grant", "the code_verifier does not match")
	}
	resource := form.get("resource")
	if !o.ResourceOK(resource) || (truthy(code.Resource) && truthy(resource) &&
		strings.TrimRight(*code.Resource, "/") != strings.TrimRight(*resource, "/")) {
		return nil, oauthError("invalid_target", "the resource is not this server")
	}
	now := int64(o.Now())
	result, err := o.db.Exec("insert into connector_grants(client_id, created_at, used_at) values(?, ?, ?)",
		code.ClientID, now, now)
	if err != nil {
		return nil, err
	}
	grantID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	o.spent[codeHash] = spentCode{grantID, o.Now() + CodeTTL}
	return o.issue(grantID, nil)
}

func (o *OAuth) refresh(form Params) (httpx.Object, error) {
	tokenHash, now := Digest(form["refresh_token"]), int64(o.Now())
	var grantID int64
	var clientID string
	var refreshHash sql.NullString
	err := o.db.QueryRow("select id, client_id, refresh_hash from connector_grants where revoked_at is null and "+
		"((refresh_hash = ? and refresh_until > ?) or (previous_hash = ? and previous_until > ?))",
		tokenHash, now, tokenHash, now).Scan(&grantID, &clientID, &refreshHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, oauthError("invalid_grant", "the refresh token is unknown, revoked or expired")
	}
	if err != nil {
		return nil, err
	}
	if id := form.get("client_id"); id != nil && *id != clientID {
		return nil, oauthError("invalid_grant", "the refresh token was issued to another client")
	}
	if !o.ResourceOK(form.get("resource")) {
		return nil, oauthError("invalid_target", "the resource is not this server")
	}
	var previous *string
	if refreshHash.Valid && refreshHash.String == tokenHash {
		previous = &refreshHash.String
	}
	return o.issue(grantID, previous)
}

// issue is a new access token and a new refresh token; the refresh token used stays good for RefreshGrace.
func (o *OAuth) issue(grantID int64, previous *string) (httpx.Object, error) {
	access, refresh, now := TokenURLSafe(32), TokenURLSafe(32), int64(o.Now())
	tx, err := o.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("delete from connector_tokens where expires_at < ?", now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("insert into connector_tokens(hash, grant_id, expires_at) values(?, ?, ?)",
		Digest(access), grantID, now+AccessTTL); err != nil {
		return nil, err
	}
	if previous != nil {
		if _, err := tx.Exec("update connector_grants set previous_hash = ?, previous_until = ? where id = ?",
			*previous, now+RefreshGrace, grantID); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec("update connector_grants set refresh_hash = ?, refresh_until = ?, used_at = ? "+
		"where id = ?", Digest(refresh), now+RefreshTTL, now, grantID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "access_token", Value: access}, {Key: "token_type", Value: "Bearer"},
		{Key: "expires_in", Value: AccessTTL}, {Key: "refresh_token", Value: refresh}}, nil
}

// Check is the connection of a valid access token; ok false for none.
func (o *OAuth) Check(token string) (int64, bool, error) {
	now := int64(o.Now())
	var grantID, usedAt int64
	err := o.db.QueryRow("select g.id, g.used_at from connector_tokens t join connector_grants g on g.id = t.grant_id "+
		"where t.hash = ? and t.expires_at > ? and g.revoked_at is null", Digest(token), now).Scan(&grantID, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if now-usedAt > 60 {
		if _, err := o.db.Exec("update connector_grants set used_at = ? where id = ?", now, grantID); err != nil {
			return 0, false, err
		}
	}
	return grantID, true, nil
}

// Revoke is RFC 7009: an access token goes alone, a refresh token takes its connection with it.
func (o *OAuth) Revoke(token string) error {
	tokenHash := Digest(token)
	var grantID int64
	err := o.db.QueryRow("select id from connector_grants where refresh_hash = ? or previous_hash = ?",
		tokenHash, tokenHash).Scan(&grantID)
	if err == nil {
		if _, err := o.RevokeGrant(grantID); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = o.db.Exec("delete from connector_tokens where hash = ?", tokenHash)
	return err
}

// RevokeGrant ends a connection; false when it was not in force.
func (o *OAuth) RevokeGrant(grantID int64) (bool, error) {
	tx, err := o.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec("update connector_grants set revoked_at = ?, refresh_hash = null, "+
		"previous_hash = null where id = ? and revoked_at is null", int64(o.Now()), grantID)
	if err != nil {
		return false, err
	}
	done, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec("delete from connector_tokens where grant_id = ?", grantID); err != nil {
		return false, err
	}
	return done > 0, tx.Commit()
}

// Connection is a connection in force, for the web app.
type Connection struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	CreatedAt int64   `json:"created_at"`
	UsedAt    int64   `json:"used_at"`
	Redirect  *string `json:"redirect"` // the host of its first redirect address
}

// Connections are the connections in force, the newest used first.
func (o *OAuth) Connections() ([]Connection, error) {
	rows, err := o.db.Query("select g.id, g.created_at, g.used_at, c.name, c.redirect_uris from connector_grants g "+
		"left join connector_clients c on c.id = g.client_id where g.revoked_at is null and g.refresh_until > ? "+
		"order by g.used_at desc", int64(o.Now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		var c Connection
		var name, uris sql.NullString
		if err := rows.Scan(&c.ID, &c.CreatedAt, &c.UsedAt, &name, &uris); err != nil {
			return nil, err
		}
		c.Name = name.String
		if c.Name == "" {
			c.Name = "?"
		}
		text := uris.String
		if text == "" {
			text = `[""]`
		}
		first := ""
		if decoded, err := httpx.Decode([]byte(text)); err == nil {
			if list, ok := decoded.([]any); ok && len(list) > 0 {
				first, _ = list[0].(string)
			}
		}
		if host := urlsplit(first).Hostname(); host != "" {
			c.Redirect = &host
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
