// Package links makes the links to the library's files (D58): a file goes in and out by a signed link, never inside a
// tool's call.
//
// Claude Code on the owner's machine moves a lecture's recording or a built PDF with `curl`; the web app opens a PDF
// the same way. A link names one file and one direction and expires; it is signed with a key kept in the database, so
// the service's restart keeps the links it gave. On the public port, next to the web app:
//   - GET /library/<token>/<name>: a material's file, a version's source or PDF, or a sheet's PDF the sheets keep for
//     14 days (D69: it opens outside the app's session, as an iPhone's Home Screen app opens a link in Safari);
//   - PUT /library/<token>: the bytes of a draft material or of a draft version's source or PDF (POST too).
package links

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// SecretKey is the kv key of the signing key.
const SecretKey = "library_link_secret"

// GetTTL and PutTTL are the seconds a download and an upload link work.
const (
	GetTTL = 24 * 3600
	PutTTL = 6 * 3600
)

// Inline are the types a browser opens without running anything on the app's origin; the rest is downloaded (a chat
// member chooses a file's type: text/html or image/svg+xml served inline would run a script next to the web app's
// session).
var Inline = []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/heic", "image/avif", "audio/",
	"video/", "application/pdf"}

// Sandbox is the CSP of every served file but a PDF.
const Sandbox = "sandbox; default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'"

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func unb64(text string) ([]byte, error) {
	return base64.URLEncoding.DecodeString(text + strings.Repeat("=", (4-len(text)%4)%4))
}

// Links signs and checks the links.
type Links struct {
	Library   *library.Library
	PublicURL string
	Seconds   func() float64
	key       []byte
	// Kept is a sheet's PDF by its hash, while the sheets keep it; "" when it is gone.
	Kept func(sha256 string) string
}

// New loads the key from the store, making one the first time.
func New(s *store.Store, lib *library.Library, publicURL string) (*Links, error) {
	var key string
	found, err := s.GetJSON(SecretKey, &key)
	if err != nil {
		return nil, err
	}
	if !found {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		key = hex.EncodeToString(b[:])
		if err := s.PutJSON(SecretKey, key); err != nil {
			return nil, err
		}
	}
	raw, err := hex.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", SecretKey, err)
	}
	return &Links{Library: lib, PublicURL: strings.TrimRight(publicURL, "/"), key: raw,
		Seconds: func() float64 { return float64(time.Now().UnixNano()) / 1e9 }}, nil
}

// The payloads, their keys in the order Python writes them.
type materialPayload struct {
	A string `json:"a"`
	M int64  `json:"m"`
	E int64  `json:"e"`
}

type versionPayload struct {
	A string `json:"a"`
	V int64  `json:"v"`
	P string `json:"p"`
	E int64  `json:"e"`
}

type keptPayload struct {
	A string `json:"a"`
	K string `json:"k"`
	E int64  `json:"e"`
}

// Sign makes a token of a payload (a struct whose fields are in the order of the JSON keys).
func (l *Links) Sign(payload any) string {
	text, err := pyjson.CompactASCII(payload)
	if err != nil {
		panic(err)
	}
	body := b64([]byte(text))
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte(body))
	return body + "." + b64(mac.Sum(nil)[:18])
}

// Check is the payload of a valid, unexpired link; nil otherwise. Numbers are float64, as JSON reads them.
func (l *Links) Check(token string) map[string]any {
	body, mac, _ := strings.Cut(token, ".")
	h := hmac.New(sha256.New, l.key)
	h.Write([]byte(body))
	expected := h.Sum(nil)[:18]
	got, err := unb64(mac)
	if err != nil || !hmac.Equal(got, expected) {
		return nil
	}
	raw, err := unb64(body)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return nil
	}
	expires, _ := payload["e"].(float64)
	if expires <= l.Seconds() {
		return nil
	}
	return payload
}

func (l *Links) expires(ttl int64) int64 { return int64(math.Floor(l.Seconds() + float64(ttl))) }

// --- making links -------------------------------------------------------------------------

// Material is the download link of a material's file.
func (l *Links) Material(row *library.Material, ttl int64) string {
	token := l.Sign(materialPayload{"get", row.ID, l.expires(ttl)})
	name := "file"
	if row.Name != nil && *row.Name != "" {
		name = *row.Name
	}
	return l.PublicURL + "/library/" + token + "/" + pytext.Quote(name, "/")
}

// MaterialUpload is the upload link of a draft material.
func (l *Links) MaterialUpload(itemID, ttl int64) string {
	return l.PublicURL + "/library/" + l.Sign(materialPayload{"put", itemID, l.expires(ttl)})
}

// Version is the download link of a version's source or PDF.
func (l *Links) Version(v *library.Version, part string, ttl int64) string {
	token := l.Sign(versionPayload{"get", v.ID, part, l.expires(ttl)})
	return l.PublicURL + "/library/" + token + "/" + pytext.Quote(FileName(v, part), "/")
}

// KeptPDF is the download link of a sheet's PDF the sheets keep; "" when it is gone.
func (l *Links) KeptPDF(sha, name string, ttl int64) string {
	if l.Kept == nil || l.Kept(sha) == "" {
		return ""
	}
	token := l.Sign(keptPayload{"get", sha, l.expires(ttl)})
	if name == "" {
		name = "sheet.pdf"
	}
	return l.PublicURL + "/library/" + token + "/" + pytext.Quote(name, "/")
}

// VersionUpload is the upload link of a draft version's part.
func (l *Links) VersionUpload(versionID int64, part string, ttl int64) string {
	return l.PublicURL + "/library/" + l.Sign(versionPayload{"put", versionID, part, l.expires(ttl)})
}

// FileName is a version's file as it is served: «<title> v<n>.pdf».
func FileName(v *library.Version, part string) string {
	ext := ".tex"
	if part == "pdf" {
		ext = ".pdf"
	} else if v.Kind == "cheatsheet" {
		ext = ".md"
	}
	title := v.Title
	if title == "" {
		title = "document"
	}
	var b strings.Builder
	for _, r := range title {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune(" -_.,()", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	number := ""
	if v.Version != nil && *v.Version != 0 {
		number = fmt.Sprintf(" v%d", *v.Version)
	}
	return pytext.Cut(pytext.Strip(b.String()), 100) + number + ext
}

// ServedType is the type a file is served with: a safe one as it is, any text as plain text, the rest as bytes to
// download.
func ServedType(mediaType string) string {
	kind, _, _ := strings.Cut(mediaType, ";")
	kind = strings.ToLower(strings.TrimSpace(kind))
	for _, prefix := range Inline {
		if strings.HasPrefix(kind, prefix) {
			return kind
		}
	}
	if strings.HasPrefix(kind, "text/") {
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// Disposition is Content-Disposition: inline for the safe types and plain text, with an ASCII name and the UTF-8 one.
func Disposition(name, mediaType string) string {
	kind := "attachment"
	for _, prefix := range append(Inline, "text/plain") {
		if strings.HasPrefix(mediaType, prefix) {
			kind = "inline"
		}
	}
	var ascii strings.Builder
	for _, r := range name {
		if r >= 0x80 || r == '"' || r == '?' {
			ascii.WriteByte('_')
		} else {
			ascii.WriteRune(r)
		}
	}
	return fmt.Sprintf("%s; filename=\"%s\"; filename*=UTF-8''%s", kind, ascii.String(), pytext.Quote(name, "/"))
}

// --- the routes ---------------------------------------------------------------------------

func notFound(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(text))
}

func number(payload map[string]any, key string) (int64, bool) {
	value, ok := payload[key].(float64)
	if !ok || value != math.Trunc(value) {
		return 0, false
	}
	return int64(value), true
}

// Download serves GET /library/{token}[/{name}].
func (l *Links) Download(w http.ResponseWriter, r *http.Request) {
	payload := l.Check(r.PathValue("token"))
	if payload == nil || payload["a"] != "get" {
		notFound(w, "the link is wrong or expired")
		return
	}
	var path, name, mediaType string
	_, material := payload["m"]
	_, kept := payload["k"]
	switch {
	case material:
		id, _ := number(payload, "m")
		row, err := l.Library.Get(id, false, false)
		if err != nil {
			http.Error(w, "500: Internal Server Error", http.StatusInternalServerError)
			return
		}
		if path = l.Library.Path(row); path == "" {
			notFound(w, "the file is gone")
			return
		}
		name = "file"
		if row.Name != nil && *row.Name != "" {
			name = *row.Name
		}
		stored := ""
		if row.MediaType != nil {
			stored = *row.MediaType
		}
		mediaType = ServedType(stored)
	case kept:
		if l.Kept != nil {
			path = l.Kept(fmt.Sprint(payload["k"]))
		}
		if path == "" {
			notFound(w, "the file is gone")
			return
		}
		name, mediaType = r.PathValue("name"), "application/pdf"
		if name == "" {
			name = "sheet.pdf"
		}
	default:
		id, _ := number(payload, "v")
		part, _ := payload["p"].(string)
		v, err := l.Library.GetVersion(id)
		if err != nil {
			http.Error(w, "500: Internal Server Error", http.StatusInternalServerError)
			return
		}
		if path = l.Library.PartPath(v, part); path == "" {
			notFound(w, "the file is gone")
			return
		}
		name = FileName(v, part)
		mediaType = "text/plain; charset=utf-8"
		if part == "pdf" {
			mediaType = "application/pdf"
		}
	}
	file, err := os.Open(path)
	if err != nil {
		notFound(w, "the file is gone")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		notFound(w, "the file is gone")
		return
	}
	kind, _, _ := strings.Cut(mediaType, ";")
	header := w.Header()
	header.Set("Content-Type", mediaType)
	header.Set("Content-Disposition", Disposition(name, kind))
	header.Set("Cache-Control", "private, max-age=3600")
	header.Set("X-Content-Type-Options", "nosniff")
	if mediaType != "application/pdf" { // a PDF viewer refuses a sandbox; a PDF runs nothing on the origin
		header.Set("Content-Security-Policy", Sandbox)
	}
	http.ServeContent(w, r, "", info.ModTime(), file)
}

// Upload serves PUT and POST /library/{token}.
func (l *Links) Upload(w http.ResponseWriter, r *http.Request) {
	payload := l.Check(r.PathValue("token"))
	if payload == nil || payload["a"] != "put" {
		notFound(w, "the link is wrong or expired")
		return
	}
	var result any
	if _, material := payload["m"]; material {
		id, _ := number(payload, "m")
		row, err := l.Library.Fill(id, r.Body)
		if err != nil {
			l.failed(w, err)
			return
		}
		result = struct {
			Material int64   `json:"material"`
			Size     *int64  `json:"size"`
			Sha256   *string `json:"sha256"`
		}{row.ID, row.Size, row.Sha256}
	} else {
		id, _ := number(payload, "v")
		part, _ := payload["p"].(string)
		v, err := l.Library.Put(id, part, r.Body)
		if err != nil {
			l.failed(w, err)
			return
		}
		result = struct {
			VersionDraft int64  `json:"version_draft"`
			Part         string `json:"part"`
			Uploaded     bool   `json:"uploaded"`
		}{v.ID, part, true}
	}
	text, err := pyjson.DumpsASCII(result)
	if err != nil {
		http.Error(w, "500: Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write([]byte(text))
}

func (l *Links) failed(w http.ResponseWriter, err error) {
	var refused *library.Error
	if errors.As(err, &refused) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(refused.Msg))
		return
	}
	http.Error(w, "500: Internal Server Error", http.StatusInternalServerError)
}

// Routes adds the links' routes to a mux.
func (l *Links) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /library/{token}/{name}", l.Download)
	mux.HandleFunc("GET /library/{token}", l.Download)
	mux.HandleFunc("PUT /library/{token}", l.Upload)
	mux.HandleFunc("POST /library/{token}", l.Upload)
}
