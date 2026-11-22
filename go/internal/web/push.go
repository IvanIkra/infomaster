package web

// Web Push of the web app (§8.6, stage 16.4): subscriptions per device and the sending, by RFC 8030, 8291 and 8292.
// The port of web/push.py.
//
//   - A device that turns notifications on gives its browser's subscription (an endpoint of its push service and two
//     keys); it is kept per web session (`web_push`), so ending the session elsewhere, or 90 days without it, ends its
//     notifications too. The endpoint must be https on a known push service: Apple's, Google's, Mozilla's, Microsoft's.
//   - The server's VAPID key pair (P-256) is made at the first start and kept in `kv`: the browser binds a subscription
//     to its public key, so a new pair would make every device subscribe again.
//   - A message is JSON {title, body, url, tag}, encrypted for each device (aes128gcm, RFC 8291) and posted with a
//     VAPID token (ES256, 12 hours). A subscription the service answers 404 or 410 to is gone and deleted; one that
//     fails MaxFailures times in a row is deleted too.
//
// No third-party push library: this is a page of cryptography on the standard library.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// PushSchema is the table of the subscriptions.
const PushSchema = `
create table if not exists web_push (     -- devices that get the web app's notifications (§8.6)
    id integer primary key,
    session_id integer not null,          -- web_sessions.id: the device; its subscription goes with it
    endpoint text not null unique,        -- the push service's URL for this browser
    p256dh text not null,                 -- the browser's public key, base64url
    auth text not null,                   -- its auth secret, base64url
    created_at integer not null,          -- unix seconds
    sent_at integer,                      -- the last message the service took
    failures integer not null default 0   -- failed sends in a row
);
`

const (
	VapidKey     = "web.vapid"
	MaxFailures  = 10
	TokenLife    = 12 * 3600
	RecordSize   = 4096
	PushTTL      = 24 * 3600 // a message waits a day for a device that is off
	PayloadLimit = 3000      // bytes of JSON; the services take about 4 KB encrypted
)

// PushHosts are the push services a subscription may name (the host itself or a subdomain).
var PushHosts = []string{"push.apple.com", "fcm.googleapis.com", "push.services.mozilla.com", "notify.windows.com"}

// PushError is a subscription refused: the text says why.
type PushError struct{ Msg string }

func (e *PushError) Error() string { return e.Msg }

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

// unb64 is Python's urlsafe_b64decode of a text with its padding added: either alphabet, padding optional.
func unb64(text string) ([]byte, error) {
	text = strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(text, "="))
	return base64.RawURLEncoding.DecodeString(text)
}

func pushHKDF(salt, info []byte, length int, ikm []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, ikm, salt, string(info), length)
}

// Encrypt is the body of a push message for one browser (RFC 8291, aes128gcm, one record). salt and server (a new
// key pair each time) are given only by tests; nil makes them.
func Encrypt(payload []byte, p256dh, auth string, salt []byte, server *ecdh.PrivateKey) ([]byte, error) {
	browserRaw, err := unb64(p256dh)
	if err != nil {
		return nil, err
	}
	browser, err := ecdh.P256().NewPublicKey(browserRaw)
	if err != nil {
		return nil, err
	}
	secretAuth, err := unb64(auth)
	if err != nil {
		return nil, err
	}
	if server == nil {
		if server, err = ecdh.P256().GenerateKey(rand.Reader); err != nil {
			return nil, err
		}
	}
	serverRaw := server.PublicKey().Bytes()
	if salt == nil {
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
	}
	secret, err := server.ECDH(browser)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), browserRaw...), serverRaw...)
	ikm, err := pushHKDF(secretAuth, info, 32, secret)
	if err != nil {
		return nil, err
	}
	key, err := pushHKDF(salt, []byte("Content-Encoding: aes128gcm\x00"), 16, ikm)
	if err != nil {
		return nil, err
	}
	nonce, err := pushHKDF(salt, []byte("Content-Encoding: nonce\x00"), 12, ikm)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, append(append([]byte{}, payload...), 0x02), nil) // \x02: the last record
	out := bytes.NewBuffer(nil)
	out.Write(salt)
	_ = binary.Write(out, binary.BigEndian, uint32(RecordSize))
	out.WriteByte(byte(len(serverRaw)))
	out.Write(serverRaw)
	out.Write(sealed)
	return out.Bytes(), nil
}

// Vapid is the server's key pair for VAPID (RFC 8292), made once and kept.
type Vapid struct {
	Subject string // https://… of the service: the push services may write to its owner
	Key     *ecdsa.PrivateKey
	Public  string // the applicationServerKey of the browsers: base64url of the uncompressed point
}

type vapidSaved struct {
	Private string `json:"private"`
}

// NewVapid loads the key pair from kv, or makes and keeps one (PKCS#8 PEM, P-256).
func NewVapid(s *store.Store, subject string) (*Vapid, error) {
	var saved vapidSaved
	found, err := s.GetJSON(VapidKey, &saved)
	if err != nil {
		return nil, err
	}
	if !found || saved.Private == "" {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		saved.Private = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		if err := s.PutJSON(VapidKey, saved); err != nil {
			return nil, err
		}
	}
	block, _ := pem.Decode([]byte(saved.Private))
	if block == nil {
		return nil, errors.New("web.vapid: not a PEM key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("web.vapid: not a P-256 key")
	}
	public, err := key.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	return &Vapid{Subject: subject, Key: key, Public: b64(public.Bytes())}, nil
}

// endpointOrigin is scheme://netloc of an endpoint, as urlsplit gives them.
func endpointOrigin(endpoint string) string {
	scheme, rest, ok := strings.Cut(endpoint, "://")
	if !ok {
		return "://"
	}
	host := rest
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	return strings.ToLower(scheme) + "://" + host
}

// Token is the VAPID JWT for an endpoint at a moment (unix seconds).
func (v *Vapid) Token(endpoint string, now float64) (string, error) {
	header, err := pyjson.CompactASCII(httpx.Object{{Key: "typ", Value: "JWT"}, {Key: "alg", Value: "ES256"}})
	if err != nil {
		return "", err
	}
	claims, err := pyjson.CompactASCII(httpx.Object{{Key: "aud", Value: endpointOrigin(endpoint)},
		{Key: "exp", Value: int64(math.Floor(now)) + TokenLife}, {Key: "sub", Value: v.Subject}})
	if err != nil {
		return "", err
	}
	signed := b64([]byte(header)) + "." + b64([]byte(claims))
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, v.Key, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signed + "." + b64(signature), nil
}

// Authorization is the Authorization header of a post to an endpoint.
func (v *Vapid) Authorization(endpoint string, now float64) (string, error) {
	token, err := v.Token(endpoint, now)
	if err != nil {
		return "", err
	}
	return "vapid t=" + token + ", k=" + v.Public, nil
}

// Post sends a body to a push service: its HTTP status; an error (status 0) when the request failed on the way.
type Post func(ctx context.Context, url string, body []byte, headers map[string]string) (int, error)

var pushClient = &http.Client{Timeout: 20 * time.Second}

// HTTPPost is the default Post.
func HTTPPost(ctx context.Context, endpoint string, body []byte, headers map[string]string) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := pushClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		log.Printf("push service %s answered %d: %s", hostOf(endpoint), response.StatusCode,
			pytext.Cut(string(text), 200))
	}
	return response.StatusCode, nil
}

func hostOf(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// CheckEndpoint is the endpoint when it is https on a known push service; a *PushError else.
func CheckEndpoint(endpoint any) (string, error) {
	text, ok := endpoint.(string)
	refused := &PushError{"the endpoint is not a known push service"}
	if !ok {
		return "", refused
	}
	parsed, err := url.Parse(text)
	if err != nil || parsed.Scheme != "https" {
		return "", refused
	}
	host := strings.ToLower(parsed.Hostname())
	for _, known := range PushHosts {
		if host == known || strings.HasSuffix(host, "."+known) {
			return text, nil
		}
	}
	return "", refused
}

// Device is a row of web_push.
type Device struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"session_id"`
	Endpoint  string `json:"endpoint"`
	P256dh    string `json:"p256dh"`
	Auth      string `json:"auth"`
	CreatedAt int64  `json:"created_at"`
	SentAt    *int64 `json:"sent_at"`
	Failures  int64  `json:"failures"`
}

// Push is the subscriptions and the sending.
type Push struct {
	Store   *store.Store
	Seconds func() float64
	Post    Post
	Vapid   *Vapid
}

// NewPush makes the table and loads (or makes) the VAPID key; seconds nil is the clock.
func NewPush(s *store.Store, subject string, seconds func() float64) (*Push, error) {
	if seconds == nil {
		seconds = unixNow
	}
	if _, err := s.DB.Exec(PushSchema); err != nil {
		return nil, err
	}
	vapid, err := NewVapid(s, subject)
	if err != nil {
		return nil, err
	}
	return &Push{Store: s, Seconds: seconds, Post: HTTPPost, Vapid: vapid}, nil
}

func unixNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func (p *Push) now() int64 { return int64(math.Floor(p.Seconds())) }

// --- the devices -------------------------------------------------------------------------------------------

// Subscribe keeps a browser's PushSubscription ({endpoint, keys: {p256dh, auth}}) for a session; a device has one,
// a new one replaces it. A *PushError when it is refused.
func (p *Push) Subscribe(sessionID int64, subscription any) error {
	sub, ok := subscription.(map[string]any)
	if !ok {
		return &PushError{"a subscription {endpoint, keys} expected"}
	}
	keys, ok := sub["keys"].(map[string]any)
	if !ok {
		return &PushError{"a subscription {endpoint, keys} expected"}
	}
	endpoint, err := CheckEndpoint(sub["endpoint"])
	if err != nil {
		return err
	}
	bad := &PushError{"the subscription's keys are not P-256 and a 16-byte secret"}
	p256dh, ok1 := keys["p256dh"].(string)
	auth, ok2 := keys["auth"].(string)
	if !ok1 || !ok2 {
		return bad
	}
	raw, err := unb64(p256dh)
	if err != nil {
		return bad
	}
	if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
		return bad
	}
	if secret, err := unb64(auth); err != nil || len(secret) != 16 {
		return bad
	}
	return p.Store.InTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("delete from web_push where session_id = ? or endpoint = ?", sessionID,
			endpoint); err != nil {
			return err
		}
		_, err := tx.Exec("insert into web_push(session_id, endpoint, p256dh, auth, created_at) "+
			"values(?, ?, ?, ?, ?)", sessionID, endpoint, p256dh, auth, p.now())
		return err
	})
}

// Unsubscribe drops a session's subscription; whether there was one.
func (p *Push) Unsubscribe(sessionID int64) (bool, error) {
	result, err := p.Store.DB.Exec("delete from web_push where session_id = ?", sessionID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// Devices are the subscriptions of sessions still in force; the others go (a session ended or idle).
func (p *Push) Devices() ([]Device, error) {
	if _, err := p.Store.DB.Exec("delete from web_push where session_id not in (select id from web_sessions)"); err != nil {
		return nil, err
	}
	rows, err := p.Store.DB.Query("select id, session_id, endpoint, p256dh, auth, created_at, sent_at, failures " +
		"from web_push order by id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Endpoint, &d.P256dh, &d.Auth, &d.CreatedAt, &d.SentAt,
			&d.Failures); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Subscribed reports whether a session has a subscription.
func (p *Push) Subscribed(sessionID int64) (bool, error) {
	devices, err := p.Devices()
	if err != nil {
		return false, err
	}
	for _, d := range devices {
		if d.SessionID == sessionID {
			return true, nil
		}
	}
	return false, nil
}

// --- sending -------------------------------------------------------------------------------------------------

// Message is what a device shows: {title, body, url, tag?, silent?}.
type Message struct {
	Title  string
	Body   string
	URL    string // "" leaves it out
	Tag    string // "" leaves it out
	Silent bool
}

func (m Message) object(body string) httpx.Object {
	o := httpx.Object{{Key: "title", Value: m.Title}, {Key: "body", Value: body}}
	if m.URL != "" {
		o = append(o, httpx.Field{Key: "url", Value: m.URL})
	}
	if m.Tag != "" {
		o = append(o, httpx.Field{Key: "tag", Value: m.Tag})
	}
	if m.Silent {
		o = append(o, httpx.Field{Key: "silent", Value: true})
	}
	return o
}

// Payload is the JSON a message is sent as (json.dumps(ensure_ascii=False)); a body that makes it longer than
// PayloadLimit bytes is cut to PayloadLimit/4 characters and an ellipsis.
func (m Message) Payload() ([]byte, error) {
	text, err := pyjson.Dumps(m.object(m.Body))
	if err != nil {
		return nil, err
	}
	if len(text) > PayloadLimit {
		if text, err = pyjson.Dumps(m.object(pytext.Cut(m.Body, PayloadLimit/4) + "…")); err != nil {
			return nil, err
		}
	}
	return []byte(text), nil
}

// Send sends a message to every device, or to one session's (sessionID non-nil); how many services took it.
// urgency "" is normal, ttl 0 is PushTTL.
func (p *Push) Send(ctx context.Context, message Message, sessionID *int64, urgency string, ttl int) (int, error) {
	if urgency == "" {
		urgency = "normal"
	}
	if ttl == 0 {
		ttl = PushTTL
	}
	payload, err := message.Payload()
	if err != nil {
		return 0, err
	}
	devices, err := p.Devices()
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sent := 0
	var failure error
	for _, d := range devices {
		if sessionID != nil && d.SessionID != *sessionID {
			continue
		}
		wg.Add(1)
		go func(d Device) {
			defer wg.Done()
			ok, err := p.one(ctx, d, payload, urgency, ttl)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				sent++
			}
			if err != nil && failure == nil {
				failure = err
			}
		}(d)
	}
	wg.Wait()
	return sent, failure
}

func (p *Push) one(ctx context.Context, d Device, payload []byte, urgency string, ttl int) (bool, error) {
	authorization, err := p.Vapid.Authorization(d.Endpoint, p.Seconds())
	if err != nil {
		return false, err
	}
	headers := map[string]string{"Authorization": authorization, "Content-Encoding": "aes128gcm",
		"Content-Type": "application/octet-stream", "TTL": strconv.Itoa(ttl), "Urgency": urgency}
	body, err := Encrypt(payload, d.P256dh, d.Auth, nil, nil)
	if err != nil {
		return false, fmt.Errorf("push to %s: %w", hostOf(d.Endpoint), err)
	}
	status, err := p.Post(ctx, d.Endpoint, body, headers)
	if err != nil {
		log.Printf("push to %s failed: %v", hostOf(d.Endpoint), err)
		status = 0
	}
	if status >= 200 && status < 300 {
		_, err := p.Store.DB.Exec("update web_push set sent_at = ?, failures = 0 where id = ?", p.now(), d.ID)
		return err == nil, err
	}
	return false, p.Store.InTx(func(tx *sql.Tx) error {
		if status == 404 || status == 410 { // the browser dropped the subscription
			_, err := tx.Exec("delete from web_push where id = ?", d.ID)
			return err
		}
		if _, err := tx.Exec("update web_push set failures = failures + 1 where id = ?", d.ID); err != nil {
			return err
		}
		_, err := tx.Exec("delete from web_push where id = ? and failures >= ?", d.ID, MaxFailures)
		return err
	})
}
