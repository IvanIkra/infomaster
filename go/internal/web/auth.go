package web

// Who may open the web app (§8.6, D61): the one owner, with a session per device. The port of web/auth.py.
//
//   - The first login on a device is a one-time code the bot sends to the owner's private chat, as the connector's
//     sign-in does (§8.5). A logged-in device may then add a passkey (WebAuthn), and its later logins use that.
//   - A session is a random token in an HttpOnly, Secure, SameSite=Strict cookie on /web; the database keeps its
//     SHA-256 only. A session unused for SessionIdle is gone, and any one can be ended from another device.
//   - The code, its attempts and the WebAuthn challenges live in memory: a restart only asks for a new code.
//
// Passkeys are discoverable (the login asks for no name) and need user verification (Face ID, a fingerprint, a PIN).
// They are bound to the host of PUBLIC_URL: if the Funnel name changes, the code from the bot still works. The
// WebAuthn ceremonies' cryptography is a Verifier (internal/web/passkey); the challenges, the rows and the rules of
// the ceremonies are here.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"strings"
	"sync"

	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// AuthSchema is the tables of the sessions and the passkeys.
const AuthSchema = `
create table if not exists web_sessions (   -- devices logged in to the web app (D61)
    id integer primary key,
    token_hash text not null unique,        -- SHA-256 of the cookie's token
    device text not null,                   -- from the User-Agent, for the list of devices
    created_at integer not null,            -- unix seconds
    used_at integer not null
);
create table if not exists web_passkeys (   -- the owner's WebAuthn credentials (D61)
    id integer primary key,
    credential_id text not null unique,     -- base64url
    public_key blob not null,               -- COSE
    sign_count integer not null,
    transports text,                        -- JSON list, a hint for the browser
    device text not null,                   -- where it was added
    created_at integer not null,
    used_at integer
);
`

const (
	Cookie         = "im_session"
	CookiePath     = "/web"
	SessionIdle    = 90 * 86400  // a session unused this long is gone
	SessionMax     = 365 * 86400 // and any session this old: a stolen cookie does not live for ever (D79)
	TouchEvery     = 3600        // used_at is written at most this often
	CodeTTL        = 600         // the code from the bot
	Attempts       = 3           // wrong codes before a new one is needed
	ResendAfter    = 30          // seconds between two codes
	CodesPerHour   = 6
	CodesPerDay    = 12  // with Attempts: 36 guesses a day of a million, and each code is a message to the owner
	ChallengeTTL   = 300 // a WebAuthn ceremony
	ChallengesMax  = 20  // open ceremonies kept: the login's options are public, memory is not theirs
	challengeBytes = 64  // as py_webauthn makes them
)

// UserID is the one user, the same on every device: an authenticator keeps one passkey for it.
var UserID = []byte("infomaster-owner")

// AuthError is a refusal of the login with its HTTP status and its text in the app's language.
type AuthError struct {
	Status  int
	Message string
}

func (e *AuthError) Error() string { return e.Message }

// Digest is the SHA-256 hex of a token, as the database keeps it.
func Digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// DeviceName is 'iPhone · Safari' from a User-Agent: enough to tell one's own devices apart in the list.
func DeviceName(userAgent string) string {
	systems := [][2]string{{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"Macintosh", "Mac"},
		{"Windows", "Windows"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"}}
	browsers := [][2]string{{"YaBrowser", "Yandex"}, {"Edg/", "Edge"}, {"Firefox", "Firefox"}, {"FxiOS", "Firefox"},
		{"CriOS", "Chrome"}, {"Chrome", "Chrome"}, {"Safari", "Safari"}}
	first := func(pairs [][2]string) string {
		for _, pair := range pairs {
			if strings.Contains(userAgent, pair[0]) {
				return pair[1]
			}
		}
		return ""
	}
	parts := []string{}
	for _, part := range []string{first(systems), first(browsers)} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "?"
	}
	return strings.Join(parts, " · ")
}

// Verifier is the WebAuthn cryptography of the ceremonies (internal/web/passkey.Verifier): the credential is the
// browser's PublicKeyCredential as JSON, the challenge base64url, the origin scheme://host of PUBLIC_URL. User
// presence and verification are required. Login applies py_webauthn's rule for the sign count.
type Verifier interface {
	Register(credential []byte, challenge, rpID, origin string) (id, publicKey []byte, signCount int64, err error)
	Login(credential []byte, challenge, rpID, origin string, publicKey []byte, signCount int64) (int64, error)
}

// Session is a row of web_sessions.
type Session struct {
	ID        int64
	Device    string
	CreatedAt int64
	UsedAt    int64
}

type ceremony struct {
	kind      string
	sessionID *int64
	expires   float64
}

// Auth is the login and the devices.
type Auth struct {
	Store  *store.Store
	RPID   string // the host of PUBLIC_URL
	Origin string // scheme://host[:port] of PUBLIC_URL
	// SendCode is a text to the owner's private chat with the bot; nil while there is no bot.
	SendCode func(ctx context.Context, text string) error
	Lang     func() string
	Seconds  func() float64
	Verifier Verifier // nil: no passkey is accepted

	mu       sync.Mutex
	codeHash string // "" for none
	codeSent float64
	attempts int
	sent     []float64 // when codes went out, the last day
	order    []string  // the challenges, the oldest first
	ceremony map[string]ceremony
}

// NewAuth makes the tables.
func NewAuth(s *store.Store, rpID, origin string, sendCode func(context.Context, string) error, lang func() string,
	seconds func() float64) (*Auth, error) {
	if lang == nil {
		lang = func() string { return "en" }
	}
	if seconds == nil {
		seconds = unixNow
	}
	if _, err := s.DB.Exec(AuthSchema); err != nil {
		return nil, err
	}
	return &Auth{Store: s, RPID: rpID, Origin: origin, SendCode: sendCode, Lang: lang, Seconds: seconds,
		ceremony: map[string]ceremony{}}, nil
}

func (a *Auth) refuse(status int, en, ru string) *AuthError {
	return &AuthError{Status: status, Message: i18n.Tr(a.Lang(), en, ru)}
}

func (a *Auth) now() int64 { return int64(math.Floor(a.Seconds())) }

// --- the code from the bot -----------------------------------------------------------------------------------

func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// Send has the bot send a new code; an *AuthError with the reason it did not.
func (a *Auth) Send(ctx context.Context) error {
	a.mu.Lock()
	now := a.Seconds()
	if now-a.codeSent < ResendAfter {
		a.mu.Unlock()
		return a.refuse(429, "A code was sent a moment ago: check the chat with the bot.",
			"Код только что отправлен: посмотрите чат с ботом.")
	}
	day, hour := a.sent[:0:0], 0
	for _, at := range a.sent {
		if now-at < 86400 {
			day = append(day, at)
			if now-at < 3600 {
				hour++
			}
		}
	}
	a.sent = day
	if hour >= CodesPerHour || len(a.sent) >= CodesPerDay {
		a.mu.Unlock()
		return a.refuse(429, "Too many codes for now: try again later, or log in with a passkey.",
			"Слишком много кодов: попробуйте позже или войдите по ключу доступа.")
	}
	if a.SendCode == nil {
		a.mu.Unlock()
		return a.refuse(503, "The bot is not running.", "Бот не запущен.")
	}
	// Counted before the bot is waited for: requests at the same moment cannot all pass the limits.
	a.codeHash, a.codeSent = "", now
	a.sent = append(a.sent, now)
	a.mu.Unlock()
	code, err := randomCode()
	if err != nil {
		return err
	}
	text := i18n.Tr(a.Lang(), code+" is the code to log in to the infomaster app. If you did not ask for it, ignore "+
		"this message.", code+" — код для входа в приложение infomaster. Если вы его не запрашивали, не обращайте "+
		"внимания.")
	if err := a.SendCode(ctx, text); err != nil {
		log.Printf("the bot could not send the web app's code: %v", err)
		return a.refuse(502, "The bot could not write to you: open the chat with the bot, press Start and try "+
			"again.", "Бот не смог вам написать: откройте чат с ботом, нажмите «Старт» и попробуйте снова.")
	}
	a.mu.Lock()
	a.codeHash, a.attempts = Digest(code), 0
	a.mu.Unlock()
	return nil
}

// CheckCode is a new session's token for the right code; an *AuthError else. A code works once.
func (a *Auth) CheckCode(code any, device string) (string, error) {
	a.mu.Lock()
	if a.codeHash == "" || a.Seconds()-a.codeSent > CodeTTL {
		a.codeHash = ""
		a.mu.Unlock()
		return "", a.refuse(400, "Ask for a code first.", "Сначала запросите код.")
	}
	a.attempts++
	typed := ""
	if text, ok := code.(string); ok {
		typed = strings.Join(pytext.Split(text), "")
	}
	if !hmac.Equal([]byte(Digest(typed)), []byte(a.codeHash)) {
		if a.attempts >= Attempts {
			a.codeHash = ""
			a.mu.Unlock()
			return "", a.refuse(400, "Wrong code too many times: ask for a new one.",
				"Слишком много неверных кодов: запросите новый.")
		}
		a.mu.Unlock()
		return "", a.refuse(400, "Wrong code.", "Неверный код.")
	}
	a.codeHash = ""
	a.mu.Unlock()
	log.Printf("the web app: a login by the bot's code on %s", device)
	return a.Start(device)
}

// --- sessions -------------------------------------------------------------------------------------------------

// Token is secrets.token_urlsafe(32): 43 characters.
func Token() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return b64(data), nil
}

// Start makes a session for a device; its token.
func (a *Auth) Start(device string) (string, error) {
	token, err := Token()
	if err != nil {
		return "", err
	}
	now := a.now()
	_, err = a.Store.DB.Exec("insert into web_sessions(token_hash, device, created_at, used_at) values(?, ?, ?, ?)",
		Digest(token), device, now, now)
	return token, err
}

// Session is the session of a cookie's token, its use noted; nil for an unknown or idle one.
func (a *Auth) Session(token string) (*Session, error) {
	if token == "" {
		return nil, nil
	}
	now := a.now()
	if _, err := a.Store.DB.Exec("delete from web_sessions where used_at < ? or created_at < ?", now-SessionIdle,
		now-SessionMax); err != nil {
		return nil, err
	}
	var s Session
	err := a.Store.DB.QueryRow("select id, device, created_at, used_at from web_sessions where token_hash = ?",
		Digest(token)).Scan(&s.ID, &s.Device, &s.CreatedAt, &s.UsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if now-s.UsedAt >= TouchEvery {
		if _, err := a.Store.DB.Exec("update web_sessions set used_at = ? where id = ?", now, s.ID); err != nil {
			return nil, err
		}
	}
	return &s, nil
}

// End ends a session; whether there was one.
func (a *Auth) End(sessionID int64) (bool, error) {
	return a.deleted("delete from web_sessions where id = ?", sessionID)
}

func (a *Auth) deleted(query string, id int64) (bool, error) {
	result, err := a.Store.DB.Exec(query, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// DeviceRow is a session or a passkey in the list of devices.
type DeviceRow struct {
	ID        int64  `json:"id"`
	Device    string `json:"device"`
	CreatedAt int64  `json:"created_at"`
	UsedAt    *int64 `json:"used_at"`
}

func (a *Auth) rows(query string) ([]DeviceRow, error) {
	rows, err := a.Store.DB.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceRow{}
	for rows.Next() {
		var r DeviceRow
		if err := rows.Scan(&r.ID, &r.Device, &r.CreatedAt, &r.UsedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Sessions are the devices logged in, the last used first.
func (a *Auth) Sessions() ([]DeviceRow, error) {
	return a.rows("select id, device, created_at, used_at from web_sessions order by used_at desc")
}

// --- passkeys -----------------------------------------------------------------------------------------------

// Passkeys are the owner's passkeys, the oldest first.
func (a *Auth) Passkeys() ([]DeviceRow, error) {
	return a.rows("select id, device, created_at, used_at from web_passkeys order by created_at")
}

// RemovePasskey drops a passkey; whether there was one.
func (a *Auth) RemovePasskey(passkeyID int64) (bool, error) {
	return a.deleted("delete from web_passkeys where id = ?", passkeyID)
}

func (a *Auth) remember(kind string, sessionID *int64) (string, error) {
	data := make([]byte, challengeBytes)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	challenge := b64(data)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Seconds()
	kept := a.order[:0:0]
	for _, c := range a.order {
		if a.ceremony[c].expires > now {
			kept = append(kept, c)
		} else {
			delete(a.ceremony, c)
		}
	}
	a.order = append(kept, challenge)
	a.ceremony[challenge] = ceremony{kind, sessionID, now + ChallengeTTL}
	for len(a.order) > ChallengesMax { // the oldest go first
		delete(a.ceremony, a.order[0])
		a.order = a.order[1:]
	}
	return challenge, nil
}

// challengeOf is the challenge a credential answers, from its clientDataJSON; "" when it has none.
func challengeOf(credential map[string]any) string {
	response, _ := credential["response"].(map[string]any)
	data, _ := response["clientDataJSON"].(string)
	if response == nil || data == "" {
		return ""
	}
	raw, err := unb64(data)
	if err != nil {
		return ""
	}
	var client map[string]any
	if json.Unmarshal(raw, &client) != nil {
		return ""
	}
	challenge, _ := client["challenge"].(string)
	return challenge
}

// take is the challenge a credential answers, once, if this service gave it for this ceremony.
func (a *Auth) take(credential any, kind string, sessionID *int64) (string, error) {
	expired := a.refuse(400, "The passkey's request has expired: try again.", "Запрос ключа устарел: попробуйте снова.")
	found, ok := credential.(map[string]any)
	if !ok {
		return "", expired
	}
	challenge := challengeOf(found)
	if challenge == "" {
		return "", expired
	}
	a.mu.Lock()
	entry, known := a.ceremony[challenge]
	if known {
		delete(a.ceremony, challenge)
		for i, c := range a.order {
			if c == challenge {
				a.order = append(a.order[:i:i], a.order[i+1:]...)
				break
			}
		}
	}
	a.mu.Unlock()
	same := entry.sessionID == nil && sessionID == nil ||
		entry.sessionID != nil && sessionID != nil && *entry.sessionID == *sessionID
	if !known || entry.kind != kind || !same || entry.expires < a.Seconds() {
		return "", expired
	}
	return challenge, nil
}

// RegistrationOptions is the JSON for navigator.credentials.create() on a logged-in device, as py_webauthn's
// options_to_json writes it.
func (a *Auth) RegistrationOptions(sessionID int64) (string, error) {
	rows, err := a.Store.DB.Query("select credential_id from web_passkeys")
	if err != nil {
		return "", err
	}
	exclude := []any{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		raw, err := unb64(id)
		if err != nil {
			rows.Close()
			return "", err
		}
		exclude = append(exclude, httpx.Object{{Key: "id", Value: b64(raw)}, {Key: "type", Value: "public-key"}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	challenge, err := a.remember("register", &sessionID)
	if err != nil {
		return "", err
	}
	param := func(alg int) httpx.Object {
		return httpx.Object{{Key: "type", Value: "public-key"}, {Key: "alg", Value: alg}}
	}
	return pyjson.Dumps(httpx.Object{
		{Key: "rp", Value: httpx.Object{{Key: "name", Value: "infomaster"}, {Key: "id", Value: a.RPID}}},
		{Key: "user", Value: httpx.Object{{Key: "id", Value: b64(UserID)}, {Key: "name", Value: "infomaster"},
			{Key: "displayName", Value: "infomaster"}}},
		{Key: "challenge", Value: challenge},
		{Key: "pubKeyCredParams", Value: []any{param(-8), param(-7), param(-257)}},
		{Key: "timeout", Value: 60000},
		{Key: "excludeCredentials", Value: exclude},
		{Key: "authenticatorSelection", Value: httpx.Object{{Key: "residentKey", Value: "required"},
			{Key: "requireResidentKey", Value: true}, {Key: "userVerification", Value: "required"}}},
		{Key: "attestation", Value: "none"}})
}

func (a *Auth) notAccepted() *AuthError {
	return a.refuse(400, "The passkey was not accepted.", "Ключ не принят.")
}

// AddPasskey keeps the passkey a logged-in device made; an *AuthError when it is refused.
func (a *Auth) AddPasskey(session *Session, credential any) (int64, error) {
	challenge, err := a.take(credential, "register", &session.ID)
	if err != nil {
		return 0, err
	}
	if a.Verifier == nil {
		log.Printf("a passkey was not added: no verifier")
		return 0, a.notAccepted()
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		return 0, a.notAccepted()
	}
	id, publicKey, count, err := a.Verifier.Register(raw, challenge, a.RPID, a.Origin)
	if err != nil {
		log.Printf("a passkey was not added: %v", err)
		return 0, a.notAccepted()
	}
	var transports any
	if response, ok := credential.(map[string]any)["response"].(map[string]any); ok {
		if list, ok := response["transports"].([]any); ok {
			text, err := pyjson.DumpsASCII(httpx.PyValue(list))
			if err != nil {
				return 0, err
			}
			transports = text
		}
	}
	result, err := a.Store.DB.Exec("insert into web_passkeys(credential_id, public_key, sign_count, transports, "+
		"device, created_at) values(?, ?, ?, ?, ?, ?) on conflict(credential_id) do nothing", b64(id), publicKey,
		count, transports, session.Device, a.now())
	if err != nil {
		return 0, err
	}
	log.Printf("the web app: a passkey added on %s", session.Device)
	rowID, err := result.LastInsertId()
	return rowID, err
}

// LoginOptions is the JSON for navigator.credentials.get(): any of the owner's passkeys (they are discoverable).
func (a *Auth) LoginOptions() (string, error) {
	challenge, err := a.remember("login", nil)
	if err != nil {
		return "", err
	}
	return pyjson.Dumps(httpx.Object{{Key: "challenge", Value: challenge}, {Key: "timeout", Value: 60000},
		{Key: "rpId", Value: a.RPID}, {Key: "allowCredentials", Value: []any{}},
		{Key: "userVerification", Value: "required"}})
}

// PasskeyLogin is a new session's token for a valid assertion of a known passkey; an *AuthError else.
func (a *Auth) PasskeyLogin(credential any, device string) (string, error) {
	challenge, err := a.take(credential, "login", nil)
	if err != nil {
		return "", err
	}
	id, _ := credential.(map[string]any)["id"].(string)
	var rowID, count int64
	var publicKey []byte
	err = a.Store.DB.QueryRow("select id, public_key, sign_count from web_passkeys where credential_id = ?", id).
		Scan(&rowID, &publicKey, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return "", a.refuse(400, "This passkey is unknown here: log in with a code from the bot.",
			"Этот ключ здесь неизвестен: войдите по коду от бота.")
	}
	if err != nil {
		return "", err
	}
	if a.Verifier == nil {
		log.Printf("a passkey login failed: no verifier")
		return "", a.notAccepted()
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		return "", a.notAccepted()
	}
	newCount, err := a.Verifier.Login(raw, challenge, a.RPID, a.Origin, publicKey, count)
	if err != nil {
		log.Printf("a passkey login failed: %v", err)
		return "", a.notAccepted()
	}
	if _, err := a.Store.DB.Exec("update web_passkeys set sign_count = ?, used_at = ? where id = ?", newCount,
		a.now(), rowID); err != nil {
		return "", err
	}
	log.Printf("the web app: a login by passkey on %s", device)
	return a.Start(device)
}
