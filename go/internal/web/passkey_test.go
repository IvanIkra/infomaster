package web

// Passkeys (tests/test_web.py), and a passkey that py_webauthn verified and stored: it keeps working.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/web/passkey"
)

// --- a little CBOR, enough for an authenticator ----------------------------------------------------------------

type pair struct {
	key, value any
}

func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 256:
		return []byte{major<<5 | 24, byte(n)}
	case n < 65536:
		return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
	}
	out := []byte{major<<5 | 26, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(n))
	return out
}

func cbor(value any) []byte {
	switch v := value.(type) {
	case int:
		if v >= 0 {
			return cborHead(0, uint64(v))
		}
		return cborHead(1, uint64(-1-v))
	case []byte:
		return append(cborHead(2, uint64(len(v))), v...)
	case string:
		return append(cborHead(3, uint64(len(v))), v...)
	case []pair:
		out := cborHead(5, uint64(len(v)))
		for _, p := range v {
			out = append(out, cbor(p.key)...)
			out = append(out, cbor(p.value)...)
		}
		return out
	}
	panic("cbor")
}

// authenticator is a platform authenticator in software: an EC P-256 key, «none» attestation, user presence and
// verification.
type authenticator struct {
	key    *ecdsa.PrivateKey
	id     []byte
	count  uint32
	rpID   string
	origin string
}

func newAuthenticator() *authenticator {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &authenticator{key: must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader)), id: id, rpID: testRPID,
		origin: testOrigin}
}

func (a *authenticator) cose() []byte {
	x, y := make([]byte, 32), make([]byte, 32)
	a.key.X.FillBytes(x)
	a.key.Y.FillBytes(y)
	return cbor([]pair{{1, 2}, {3, -7}, {-1, 1}, {-2, x}, {-3, y}})
}

func (a *authenticator) clientData(kind, challenge string) []byte {
	return must(json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": a.origin,
		"crossOrigin": false}))
}

func (a *authenticator) authData(flags byte, attested bool) []byte {
	hash := sha256.Sum256([]byte(a.rpID))
	data := append(hash[:], flags)
	data = binary.BigEndian.AppendUint32(data, a.count)
	if attested {
		data = append(data, make([]byte, 16)...)
		data = binary.BigEndian.AppendUint16(data, uint16(len(a.id)))
		data = append(append(data, a.id...), a.cose()...)
	}
	return data
}

func (a *authenticator) create(options map[string]any) map[string]any {
	clientData := a.clientData("webauthn.create", options["challenge"].(string))
	authData := a.authData(0x45, true)
	attestation := cbor([]pair{{"fmt", "none"}, {"attStmt", []pair{}}, {"authData", authData}})
	// as PublicKeyCredential.toJSON() of today's Safari and Chrome gives it
	return map[string]any{"id": b64(a.id), "rawId": b64(a.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64(clientData), "attestationObject": b64(attestation),
			"transports": []any{"internal", "hybrid"}, "authenticatorData": b64(authData),
			"publicKey": b64(must(x509.MarshalPKIXPublicKey(&a.key.PublicKey))), "publicKeyAlgorithm": -7},
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{"credProps": map[string]any{"rk": true}}}
}

func (a *authenticator) get(options map[string]any) map[string]any {
	a.count++
	clientData := a.clientData("webauthn.get", options["challenge"].(string))
	authData := a.authData(0x05, false)
	hash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), hash[:]...))
	signature := must(ecdsa.SignASN1(rand.Reader, a.key, digest[:]))
	return map[string]any{"id": b64(a.id), "rawId": b64(a.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64(clientData), "authenticatorData": b64(authData),
			"signature": b64(signature), "userHandle": b64(UserID)}, "authenticatorAttachment": "platform",
		"clientExtensionResults": map[string]any{}}
}

func passkeys(t *testing.T, lang string) *harness {
	h := newHarness(t, options{lang: lang})
	h.app.Auth.Verifier = passkey.Verifier{}
	return h.serve()
}

func (h *harness) addPasskey(token string, a *authenticator) map[string]any {
	options := obj(decode(h.t, h.call("POST", "/passkeys/options", map[string]any{}, req{token: token})))
	r := h.call("POST", "/passkeys", map[string]any{"credential": a.create(options)}, req{token: token})
	if r.Code != 200 {
		h.t.Fatalf("%d %s", r.Code, r.Body)
	}
	return obj(decode(h.t, r))
}

func (h *harness) passkeyLogin(a *authenticator) *httpResult {
	options := obj(decode(h.t, h.call("POST", "/login/passkey/options", map[string]any{}, req{})))
	r := h.call("POST", "/login/passkey", map[string]any{"credential": a.get(options)}, req{agent: mac})
	return &httpResult{r.Code, r.Body.String(), tokenOf(r)}
}

type httpResult struct {
	code  int
	body  string
	token string
}

func TestPasskey(t *testing.T) {
	h := passkeys(t, "ru")
	token := h.logIn(iphone)
	phone := newAuthenticator()
	devices := h.addPasskey(token, phone)
	listed := list(devices["passkeys"])
	if len(listed) != 1 || obj(listed[0])["id"] != int64(1) || obj(listed[0])["device"] != "iPhone · Safari" ||
		obj(listed[0])["used_at"] != nil {
		t.Fatal(listed)
	}
	if texts := h.texts(); !strings.Contains(texts[len(texts)-1], "ключ доступа на устройстве iPhone · Safari") {
		t.Fatal(texts) // D79: the owner hears of a new key
	}
	options := obj(decode(t, h.call("POST", "/passkeys/options", map[string]any{}, req{token: token})))
	if obj(list(options["excludeCredentials"])[0])["id"] != b64(phone.id) {
		t.Fatal(options)
	}
	h.secs.add(100)
	login := h.passkeyLogin(phone)
	if login.code != 200 {
		t.Fatal(login.body)
	}
	me := obj(decode(t, h.call("GET", "/me", nil, req{token: login.token})))
	if me["device"] != "Mac · Chrome" || me["passkeys"] != int64(1) {
		t.Fatal(me)
	}
	again := obj(decode(t, h.call("GET", "/devices", nil, req{token: login.token})))
	if obj(list(again["passkeys"])[0])["used_at"] != int64(h.secs.now()) {
		t.Fatal(again)
	}
	if h.passkeyLogin(phone).code != 200 {
		t.Fatal("the count grew again")
	}
	removed := obj(decode(t, h.call("DELETE", "/passkeys/1", nil, req{token: login.token})))
	if len(list(removed["passkeys"])) != 0 {
		t.Fatal(removed)
	}
	gone := h.passkeyLogin(phone)
	if gone.code != 400 || !strings.Contains(errorOf(gone.body), "неизвестен") {
		t.Fatal(gone.body) // «неизвестен»
	}
}

func TestPasskeyRefused(t *testing.T) {
	h := passkeys(t, "ru")
	token := h.logIn(iphone)
	phone := newAuthenticator()
	h.addPasskey(token, phone)

	// an answer to a challenge the service did not give, or gave already
	options := obj(decode(t, h.call("POST", "/login/passkey/options", map[string]any{}, req{})))
	credential := phone.get(options)
	if h.call("POST", "/login/passkey", map[string]any{"credential": credential}, req{}).Code != 200 {
		t.Fatal("first")
	}
	if h.call("POST", "/login/passkey", map[string]any{"credential": credential}, req{}).Code != 400 {
		t.Fatal("again")
	}
	forged := phone.get(map[string]any{"challenge": b64(make([]byte, 32))})
	if h.call("POST", "/login/passkey", map[string]any{"credential": forged}, req{}).Code != 400 {
		t.Fatal("forged")
	}
	// a challenge too old
	options = obj(decode(t, h.call("POST", "/login/passkey/options", map[string]any{}, req{})))
	h.secs.add(301)
	if h.call("POST", "/login/passkey", map[string]any{"credential": phone.get(options)}, req{}).Code != 400 {
		t.Fatal("old")
	}
	// another site's page
	phishing := &authenticator{key: phone.key, id: phone.id, count: phone.count, rpID: testRPID,
		origin: "https://infomaster.example.com"}
	r := h.passkeyLogin(phishing)
	if r.code != 400 || errorOf(r.body) != "Ключ не принят." {
		t.Fatal(r.body)
	}
	// a registration challenge used to log in, and a passkey added without a session
	options = obj(decode(t, h.call("POST", "/passkeys/options", map[string]any{}, req{token: token})))
	asLogin := newAuthenticator().create(options)
	if h.call("POST", "/login/passkey", map[string]any{"credential": asLogin}, req{}).Code != 400 {
		t.Fatal("as login")
	}
	if h.call("POST", "/passkeys", map[string]any{"credential": asLogin}, req{}).Code != 401 {
		t.Fatal("no session")
	}
	if h.call("POST", "/login/passkey", map[string]any{"credential": "nonsense"}, req{}).Code != 400 {
		t.Fatal("nonsense")
	}
}

// A passkey made, verified and stored by py_webauthn, and its assertion: the stored row is what Go stores, and the
// assertion logs in with the row as Python kept it.
func TestAPasskeyOfPyWebauthnKeepsWorking(t *testing.T) {
	var vectors struct {
		Register struct {
			Challenge  string         `json:"challenge"`
			Credential map[string]any `json:"credential"`
			Stored     struct {
				CredentialID string `json:"credential_id"`
				PublicKeyHex string `json:"public_key_hex"`
				SignCount    int64  `json:"sign_count"`
			} `json:"stored"`
		} `json:"register"`
		Login struct {
			Challenge    string         `json:"challenge"`
			Credential   map[string]any `json:"credential"`
			NewSignCount int64          `json:"new_sign_count"`
		} `json:"login"`
	}
	noErr(t, json.Unmarshal(must(os.ReadFile("testdata/python_vectors.json")), &vectors))
	h := passkeys(t, "en")
	token := h.logIn(iphone)
	session := must(h.app.Auth.Session(token))
	auth := h.app.Auth

	// Go verifies Python's registration and stores what py_webauthn stored.
	auth.ceremony[vectors.Register.Challenge] = ceremony{"register", &session.ID, h.secs.now() + 100}
	credential := must(httpx.Decode(must(json.Marshal(vectors.Register.Credential))))
	must(auth.AddPasskey(session, credential))
	var id, transports string
	var key []byte
	var count int64
	noErr(t, h.store.DB.QueryRow("select credential_id, public_key, sign_count, transports from web_passkeys").
		Scan(&id, &key, &count, &transports))
	if id != vectors.Register.Stored.CredentialID || hex.EncodeToString(key) != vectors.Register.Stored.PublicKeyHex ||
		count != vectors.Register.Stored.SignCount || transports != `["internal"]` {
		t.Fatal(id, hex.EncodeToString(key), count, transports)
	}

	// The row as Python wrote it (its bytes, not Go's) logs in with Python's assertion.
	_, err := h.store.DB.Exec("delete from web_passkeys")
	noErr(t, err)
	_, err = h.store.DB.Exec("insert into web_passkeys(credential_id, public_key, sign_count, transports, device, "+
		"created_at) values(?, ?, ?, ?, ?, ?)", vectors.Register.Stored.CredentialID,
		must(hex.DecodeString(vectors.Register.Stored.PublicKeyHex)), vectors.Register.Stored.SignCount, `["internal"]`,
		"iPhone · Safari", 1)
	noErr(t, err)
	auth.ceremony[vectors.Login.Challenge] = ceremony{"login", nil, h.secs.now() + 100}
	login := must(httpx.Decode(must(json.Marshal(vectors.Login.Credential))))
	if _, err := auth.PasskeyLogin(login, "Mac · Chrome"); err != nil {
		t.Fatal(err)
	}
	noErr(t, h.store.DB.QueryRow("select sign_count from web_passkeys").Scan(&count))
	if count != vectors.Login.NewSignCount {
		t.Fatal(count)
	}
	// the same assertion again: its count did not grow
	auth.ceremony[vectors.Login.Challenge] = ceremony{"login", nil, h.secs.now() + 100}
	if _, err := auth.PasskeyLogin(login, "Mac · Chrome"); err == nil {
		t.Fatal("a replayed count was accepted")
	}
}
