package web

// Web Push (tests/test_web_push.py and the notifier's part of test_web_machine.py): the encryption and VAPID,
// subscriptions per device, the notifier.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const endpoint = "https://fcm.googleapis.com/fcm/send/device-1"

// browser is a browser's side of a subscription: its key pair and auth secret; it reads what the server sent.
type browser struct {
	key          *ecdh.PrivateKey
	auth         []byte
	subscription map[string]any
}

func newBrowser(endpoint any) *browser {
	key := must(ecdh.P256().GenerateKey(rand.Reader))
	auth := []byte("0123456789abcdef")
	return &browser{key: key, auth: auth, subscription: map[string]any{"endpoint": endpoint,
		"keys": map[string]any{"p256dh": b64(key.PublicKey().Bytes()), "auth": b64(auth)}}}
}

// read is RFC 8291 from the receiving end.
func (b *browser) read(t *testing.T, body []byte) map[string]any {
	t.Helper()
	salt, size, length := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	serverRaw, sealed := body[21:21+length], body[21+length:]
	if size != 4096 || length != 65 {
		t.Fatal(size, length)
	}
	server := must(ecdh.P256().NewPublicKey(serverRaw))
	secret := must(b.key.ECDH(server))
	own := b.key.PublicKey().Bytes()
	ikm := must(pushHKDF(b.auth, append(append([]byte("WebPush: info\x00"), own...), serverRaw...), 32, secret))
	key := must(pushHKDF(salt, []byte("Content-Encoding: aes128gcm\x00"), 16, ikm))
	nonce := must(pushHKDF(salt, []byte("Content-Encoding: nonce\x00"), 12, ikm))
	gcm := must(cipher.NewGCM(must(aes.NewCipher(key))))
	plain := must(gcm.Open(nil, nonce, sealed, nil))
	if plain[len(plain)-1] != 0x02 {
		t.Fatal("not the last record")
	}
	var message map[string]any
	noErr(t, json.Unmarshal(plain[:len(plain)-1], &message))
	return message
}

type sentPush struct {
	url     string
	body    []byte
	headers map[string]string
}

// service is a push service: what it was sent, and the status it answers.
type service struct {
	mu     sync.Mutex
	sent   []sentPush
	status int
}

func (s *service) post(_ context.Context, url string, body []byte, headers map[string]string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sentPush{url, body, headers})
	return s.status, nil
}

func (s *service) take() []sentPush {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sent
	s.sent = nil
	return out
}

func TestRFC8291Example(t *testing.T) {
	server := must(ecdh.P256().NewPrivateKey(must(unb64("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))))
	body := must(Encrypt([]byte("When I grow up, I want to be a watermelon"),
		"BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		"BTBZMqHH6r4Tts7J_aSIgg", must(unb64("DGv6ra1nlYgDCS1FRnbzlw")), server))
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if b64(body) != want {
		t.Fatal(b64(body))
	}
}

func verifyJWT(t *testing.T, token, public string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if string(must(unb64(parts[0]))) != `{"typ":"JWT","alg":"ES256"}` {
		t.Fatal(parts[0])
	}
	raw := must(unb64(parts[2]))
	point := must(unb64(public))
	key := must(ecdsa.ParseUncompressedPublicKey(ecdhCurve(), point))
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:])) {
		t.Fatal("bad signature")
	}
	var claims map[string]any
	noErr(t, json.Unmarshal(must(unb64(parts[1])), &claims))
	return claims
}

func TestVapid(t *testing.T) {
	s := must(store.Open(":memory:"))
	defer s.Close()
	push := must(NewPush(s, testOrigin, (&seconds{value: 1_800_000_000}).now))
	if must(NewPush(s, testOrigin, nil)).Vapid.Public != push.Vapid.Public {
		t.Fatal("kept in kv")
	}
	header := must(push.Vapid.Authorization(endpoint, 1_800_000_000))
	scheme, rest, _ := strings.Cut(header, " ")
	tokenPart, keyPart, _ := strings.Cut(rest, ", ")
	token, key := strings.TrimPrefix(tokenPart, "t="), strings.TrimPrefix(keyPart, "k=")
	if scheme != "vapid" || key != push.Vapid.Public || len(must(unb64(key))) != 65 {
		t.Fatal(header)
	}
	claims := verifyJWT(t, token, key)
	if claims["aud"] != "https://fcm.googleapis.com" || claims["exp"] != float64(1_800_000_000+12*3600) ||
		claims["sub"] != testOrigin {
		t.Fatal(claims)
	}
	var stored struct {
		Private string `json:"private"`
	}
	must(s.GetJSON(VapidKey, &stored))
	if !strings.HasPrefix(stored.Private, "-----BEGIN PRIVATE KEY-----\n") {
		t.Fatal(stored.Private)
	}
}

// The key Python made loads, gives Python's public key, and a token whose signed part is Python's bytes.
func TestVapidKeyOfPython(t *testing.T) {
	var vectors struct {
		Vapid struct{ Private, Public, Token string } `json:"vapid"`
	}
	noErr(t, json.Unmarshal(must(os.ReadFile("testdata/python_vectors.json")), &vectors))
	s := must(store.Open(":memory:"))
	defer s.Close()
	noErr(t, s.PutJSON(VapidKey, map[string]string{"private": vectors.Vapid.Private}))
	push := must(NewPush(s, testOrigin, nil))
	if push.Vapid.Public != vectors.Vapid.Public {
		t.Fatal(push.Vapid.Public, vectors.Vapid.Public)
	}
	token := must(push.Vapid.Token(endpoint, 1_800_000_000))
	signed := func(token string) string { return token[:strings.LastIndex(token, ".")] }
	if signed(token) != signed(vectors.Vapid.Token) {
		t.Fatal(signed(token), signed(vectors.Vapid.Token))
	}
	verifyJWT(t, vectors.Vapid.Token, push.Vapid.Public) // Python's signature verifies with the key as Go reads it
}

func device(t *testing.T) (*harness, *service) {
	h := newHarness(t, options{lang: "ru"}).serve().in()
	svc := &service{status: 201}
	h.app.Push.Post = svc.post
	return h, svc
}

func TestSubscribeAndSend(t *testing.T) {
	h, svc := device(t)
	state := h.ask("GET", "/push", nil, 200)
	if state["on"] != false || state["devices"] != int64(0) || len(must(unb64(state["key"].(string)))) != 65 {
		t.Fatal(state)
	}
	if h.ask("GET", "/me", nil, 200)["push"] != false {
		t.Fatal("me")
	}
	b := newBrowser(endpoint)
	state = h.ask("POST", "/push", map[string]any{"subscription": b.subscription}, 200)
	if state["on"] != true || state["devices"] != int64(1) || h.ask("GET", "/me", nil, 200)["push"] != true {
		t.Fatal(state)
	}
	if sent := h.ask("POST", "/push/test", map[string]any{}, 200); sent["sent"] != int64(1) {
		t.Fatal(sent)
	}
	got := svc.take()
	if len(got) != 1 || got[0].url != endpoint || got[0].headers["Content-Encoding"] != "aes128gcm" ||
		got[0].headers["TTL"] != "86400" || !strings.HasPrefix(got[0].headers["Authorization"], "vapid t=") ||
		got[0].headers["Urgency"] != "normal" {
		t.Fatal(got)
	}
	message := b.read(t, got[0].body)
	if message["title"] != "infomaster" || message["body"] != "Уведомления на этом устройстве работают." ||
		message["url"] != "/web/#/" || message["tag"] != "test" || len(message) != 4 {
		t.Fatal(message)
	}
	// A new subscription of the same device replaces the old one.
	other := newBrowser("https://web.push.apple.com/QGh-device")
	h.ask("POST", "/push", map[string]any{"subscription": other.subscription}, 200)
	devices := must(h.app.Push.Devices())
	if len(devices) != 1 || devices[0].Endpoint != "https://web.push.apple.com/QGh-device" {
		t.Fatal(devices)
	}
	state = h.ask("DELETE", "/push", nil, 200)
	if state["on"] != false || state["devices"] != int64(0) {
		t.Fatal(state)
	}
	svc.status = 500
	h.ask("POST", "/push", map[string]any{"subscription": b.subscription}, 200)
	if r := h.ask("POST", "/push/test", map[string]any{}, 502); r["error"] != "Служба уведомлений его не приняла." {
		t.Fatal(r)
	}
}

func TestRefusedSubscriptions(t *testing.T) {
	h, _ := device(t)
	for _, e := range []any{"http://fcm.googleapis.com/x", "https://example.com/push", "https://fcm.googleapis.com.evil/x",
		nil} {
		bad := newBrowser(e).subscription
		if r := h.ask("POST", "/push", map[string]any{"subscription": bad}, 400); !strings.Contains(r["error"].(string),
			"push service") {
			t.Fatal(r)
		}
	}
	bad := newBrowser(endpoint).subscription
	bad["keys"].(map[string]any)["auth"] = b64([]byte("short"))
	if r := h.ask("POST", "/push", map[string]any{"subscription": bad}, 400); !strings.Contains(r["error"].(string), "16-byte") {
		t.Fatal(r)
	}
	h.ask("POST", "/push", map[string]any{"subscription": "nothing"}, 400)
	var refused *PushError
	err := h.app.Push.Subscribe(1, map[string]any{"endpoint": endpoint,
		"keys": map[string]any{"p256dh": "AAAA", "auth": b64([]byte(strings.Repeat("x", 16)))}})
	if err == nil || !errorsAs(err, &refused) {
		t.Fatal(err)
	}
	r := h.call("POST", "/push", map[string]any{"subscription": newBrowser(endpoint).subscription},
		req{token: h.token, origin: ptr("https://elsewhere.example")})
	if r.Code != 403 {
		t.Fatal(r.Code)
	}
}

func TestGoneFailingAndEnded(t *testing.T) {
	h, svc := device(t)
	push := h.app.Push
	ctx := context.Background()
	message := Message{Title: "x", Body: "y"}
	h.ask("POST", "/push", map[string]any{"subscription": newBrowser(endpoint).subscription}, 200)
	svc.status = 410 // the browser dropped it
	if n := must(push.Send(ctx, message, nil, "", 0)); n != 0 || len(must(push.Devices())) != 0 {
		t.Fatal(n)
	}
	h.ask("POST", "/push", map[string]any{"subscription": newBrowser(endpoint).subscription}, 200)
	svc.status = 503
	for range MaxFailures - 1 {
		must(push.Send(ctx, message, nil, "", 0))
	}
	if devices := must(push.Devices()); devices[0].Failures != MaxFailures-1 {
		t.Fatal(devices)
	}
	svc.status = 201
	must(push.Send(ctx, message, nil, "", 0))
	if devices := must(push.Devices()); devices[0].Failures != 0 || devices[0].SentAt == nil {
		t.Fatal("a success starts the count again")
	}
	svc.status = 500
	for range MaxFailures {
		must(push.Send(ctx, message, nil, "", 0))
	}
	if len(must(push.Devices())) != 0 {
		t.Fatal("failed too often")
	}
	h.ask("POST", "/push", map[string]any{"subscription": newBrowser(endpoint).subscription}, 200)
	h.ask("POST", "/logout", map[string]any{}, 200)
	if len(must(push.Devices())) != 0 {
		t.Fatal("the session's notifications end with it")
	}
	if h.call("GET", "/push", nil, req{token: h.token}).Code != 401 {
		t.Fatal("logged out")
	}
}

func TestThePayloadIsCut(t *testing.T) {
	long := strings.Repeat("я", 2000) // 4000 bytes
	payload := must(Message{Title: "t", Body: long, URL: "/web/#/"}.Payload())
	var message map[string]any
	noErr(t, json.Unmarshal(payload, &message))
	if body := message["body"].(string); []rune(body)[750] != '…' || len([]rune(body)) != 751 {
		t.Fatal(len([]rune(body)))
	}
}

func notifierHarness(t *testing.T, lang *string) (*Notifier, *service, *browser) {
	s := must(store.Open(":memory:"))
	t.Cleanup(func() { s.Close() })
	svc := &service{status: 201}
	push := must(NewPush(s, testOrigin, (&seconds{value: 1_800_000_000}).now))
	push.Post = svc.post
	_, err := s.DB.Exec("create table if not exists web_sessions (id integer primary key)")
	noErr(t, err)
	_, err = s.DB.Exec("insert into web_sessions(id) values(1)")
	noErr(t, err)
	b := newBrowser(endpoint)
	noErr(t, push.Subscribe(1, b.subscription))
	return NewNotifier(push, func() string { return *lang }), svc, b
}

func (n *Notifier) told(t *testing.T, svc *service, b *browser) []map[string]any {
	n.Wait()
	out := []map[string]any{}
	for _, sent := range svc.take() {
		out = append(out, b.read(t, sent.body))
	}
	return out
}

func deadline(title, due string, assumed bool, name string) todo.Deadline {
	d := todo.Deadline{Title: title, Due: must(todo.ParseDate(due)), Category: "study", Source: "tg",
		Course: ptr("Calc"), Assumed: assumed}
	if name != "" {
		d.Name = &name
	}
	return d
}

func TestNotifier(t *testing.T) {
	lang := "ru"
	n, svc, b := notifierHarness(t, &lang)
	d := deadline("Calc HW 5", "2026-10-22", false, "Матанализ")
	n.Deadline(d, "")
	n.Wait()
	n.Deadline(d, "2026-10-24")
	got := n.told(t, svc, b)
	moved := got[1]
	fresh := got[0]
	if fresh["title"] != "Новый срок: Calc HW 5" || fresh["body"] != "чт 22.10 · Матанализ" {
		t.Fatal(fresh)
	}
	uid := strings.SplitN(fresh["url"].(string), "=", 2)[1]
	if !strings.HasPrefix(fresh["url"].(string), "/web/#/d?uid=tg-") || fresh["tag"] != "deadline-"+uid {
		t.Fatal(fresh)
	}
	if moved["title"] != "Срок перенесён: Calc HW 5" || moved["body"] != "сб 24.10 → чт 22.10 · Матанализ" ||
		moved["tag"] != fresh["tag"] {
		t.Fatal(moved)
	}

	guessed := deadline("Calc sheet 3", "2026-10-22", true, "")
	n.Deadline(guessed, "")
	n.Wait()
	n.Reminder("eve", []todo.Deadline{d, guessed})
	n.Reminder("day", nil)
	got = n.told(t, svc, b)
	if got[0]["body"] != "чт 22.10 ≈ предположительно" {
		t.Fatal(got[0])
	}
	reminder := got[1]
	if reminder["title"] != "Завтра срок" || reminder["body"] != "Calc HW 5 · Матанализ\nCalc sheet 3" ||
		reminder["url"] != "/web/#/" || reminder["tag"] != "reminder-eve-2026-10-22" || len(got) != 2 {
		t.Fatal(got)
	}

	n.System(i18n.P("infomaster is up.", "infomaster запущен."), "") // no failure: no push
	n.System(i18n.AlertP("RUZ is unavailable.", "РУЗ недоступен."), "")
	n.Wait()
	n.System(i18n.AlertP("A login is needed.", "Нужен вход."), "hseapp")
	n.Wait()
	n.Budget(llm.Notice{Level: llm.Soft, Spent: 1.6, Limit: 2.0})
	n.Budget(llm.Notice{Level: llm.Hard, Spent: 2.01, Limit: 2.0})
	n.Wait()
	lang = "en"
	n.Reminder("day", []todo.Deadline{d})
	n.Wait()
	sent := svc.take()
	urgencies := []string{}
	messages := []map[string]any{}
	for _, s := range sent {
		urgencies = append(urgencies, s.headers["Urgency"])
		messages = append(messages, b.read(t, s.body))
	}
	if len(messages) != 4 {
		t.Fatal(messages)
	}
	ruz, login, budget, today := messages[0], messages[1], messages[2], messages[3]
	if ruz["title"] != "infomaster" || ruz["body"] != "РУЗ недоступен." || ruz["url"] != "/web/#/machine" || len(ruz) != 3 {
		t.Fatal(ruz)
	}
	if login["tag"] != "login-hseapp" || budget["title"] != "Лимит LLM исчерпан" ||
		!strings.HasPrefix(budget["body"].(string), "$2.01 из $2.00") || today["title"] != "Due today" {
		t.Fatal(login, budget, today)
	}
	if strings.Join(urgencies, ",") != "high,high,normal,high" {
		t.Fatal(urgencies)
	}
}

func TestQuietHours(t *testing.T) {
	at := func(hm string) time.Time {
		return must(time.ParseInLocation("2006-01-02T15:04", "2026-10-20T"+hm, clock.TZ))
	}
	if Quiet("", at("03:00")) || !Quiet("23:00-08:00", at("23:00")) || !Quiet("23:00-08:00", at("02:30")) ||
		Quiet("23:00-08:00", at("08:00")) || Quiet("23:00-08:00", at("12:00")) || !Quiet("13:00-14:00", at("13:30")) ||
		Quiet("13:00-14:00", at("14:00")) {
		t.Fatal("quiet")
	}
}

func TestNotifierKindsAndQuiet(t *testing.T) {
	lang := "en"
	n, svc, b := notifierHarness(t, &lang)
	n.Settings = must(settings.New(n.Push.Store))
	must(n.Settings.Set("push_kinds", []any{"deadlines", "failures"}))
	must(n.Settings.Set("push_quiet", "23:00-07:00"))
	n.Now = func() time.Time { return time.Date(2026, 10, 20, 23, 30, 0, 0, clock.TZ) }
	d := deadline("Calc HW 5", "2026-10-22", false, "")
	n.Deadline(d, "")
	n.Wait()
	n.Reminder("eve", []todo.Deadline{d}) // reminders are off
	n.System(i18n.AlertP("RUZ is unavailable.", "РУЗ недоступен."), "")
	got := n.told(t, svc, b)
	if len(got) != 2 || got[0]["title"] != "New deadline: Calc HW 5" || got[0]["silent"] != true ||
		got[1]["url"] != "/web/#/machine" || got[1]["silent"] != true {
		t.Fatal(got)
	}
	n.Now = func() time.Time { return time.Date(2026, 10, 21, 9, 0, 0, 0, clock.TZ) }
	n.Deadline(d, "2026-10-24")
	got = n.told(t, svc, b)
	if _, silent := got[0]["silent"]; silent {
		t.Fatal(got)
	}
}
