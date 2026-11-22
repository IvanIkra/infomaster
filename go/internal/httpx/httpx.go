// Package httpx is what every HTTP layer of the service needs to answer as the Python service (aiohttp) did:
// JSON bodies written as json.dumps writes them, aiohttp's plain-text error bodies ("404: Not Found"), its 404 and
// 405 for the whole mux, body size limits and the app's language.
//
// The JSON helpers write Python's separators (", " and ": ") and floats as Python does when the value holds
// pyjson.Float; values decoded with Decode keep integers apart from floats (int64 and float64) and PyValue turns the
// float64 back into pyjson.Float before they are written again.
package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

// BodyMax is the public port's client_max_size: the bytes of a body read whole before anyone is known (a JSON
// request, a form). Streamed bodies (/mcp, uploads) have limits of their own.
const BodyMax = 2 << 20

const (
	jsonType = "application/json; charset=utf-8"
	textType = "text/plain; charset=utf-8"
	htmlType = "text/html; charset=utf-8"
)

// --- answers ------------------------------------------------------------------------------------

// JSON is web.json_response(value, status=status): json.dumps(value) with every non-ASCII character escaped. A
// Phrase in it is written in English (its MarshalText).
func JSON(w http.ResponseWriter, status int, value any) {
	text, err := pyjson.DumpsASCII(PyValue(value))
	write(w, status, text, err, nil)
}

// JSONLang is actions.respond: JSON with every Phrase in it written in the language (json_default(lang)), also a
// Phrase held in a struct field.
func JSONLang(w http.ResponseWriter, status int, value any, lang string) {
	JSON(w, status, Localize(value, lang))
}

// JSONUnicode is a JSON answer written with json.dumps(ensure_ascii=False) (the MCP endpoint).
func JSONUnicode(w http.ResponseWriter, status int, value any) {
	text, err := pyjson.Dumps(PyValue(value))
	write(w, status, text, err, nil)
}

// JSONHeaders is JSON with extra headers (no-store).
func JSONHeaders(w http.ResponseWriter, status int, value any, headers map[string]string) {
	text, err := pyjson.DumpsASCII(PyValue(value))
	write(w, status, text, err, headers)
}

func write(w http.ResponseWriter, status int, text string, err error, headers map[string]string) {
	if err != nil {
		log.Printf("httpx: a JSON answer failed: %v", err)
		Error(w, http.StatusInternalServerError)
		return
	}
	for name, value := range headers {
		w.Header().Set(name, value)
	}
	w.Header().Set("Content-Type", jsonType)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

// Text is web.Response(text=..., status=...) and HTTPException(text=...): text/plain; charset=utf-8.
func Text(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", textType)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

// HTML is web.Response(text=..., content_type="text/html").
func HTML(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", htmlType)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

// Reason is aiohttp's default text of an HTTPException without text: "404: Not Found".
func Reason(status int) string { return fmt.Sprintf("%d: %s", status, http.StatusText(status)) }

// Error is an aiohttp HTTPException raised without a text: web.HTTPNotFound() gives 404 "404: Not Found".
func Error(w http.ResponseWriter, status int) { Text(w, status, Reason(status)) }

// Redirect is web.HTTPFound(location) (302) or HTTPSeeOther (303): the Location header and aiohttp's body.
func Redirect(w http.ResponseWriter, status int, location string) {
	w.Header().Set("Location", location)
	Error(w, status)
}

// HTTPError is an aiohttp HTTPException as an error: a handler returns it and Handle writes it. Text "" is the
// default text ("400: Bad Request").
type HTTPError struct {
	Status int
	Text   string
	Header http.Header // extra headers, may be nil
}

func (e *HTTPError) Error() string {
	if e.Text == "" {
		return Reason(e.Status)
	}
	return e.Text
}

// Write writes the error as aiohttp does.
func (e *HTTPError) Write(w http.ResponseWriter) {
	for name, values := range e.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	Text(w, e.Status, e.Error())
}

// Fail is an HTTPError with a text; "" for the default text.
func Fail(status int, text string) *HTTPError { return &HTTPError{Status: status, Text: text} }

// BadRequest is web.HTTPBadRequest(text=...).
func BadRequest(text string) *HTTPError { return Fail(http.StatusBadRequest, text) }

// NotFound is web.HTTPNotFound(text=...); "" for "404: Not Found".
func NotFound(text string) *HTTPError { return Fail(http.StatusNotFound, text) }

// WriteError writes an *HTTPError as it is and anything else as a 500, logged.
func WriteError(w http.ResponseWriter, err error) {
	var failure *HTTPError
	if errors.As(err, &failure) {
		failure.Write(w)
		return
	}
	log.Printf("httpx: %v", err)
	Error(w, http.StatusInternalServerError)
}

// Handle adapts a handler that returns an error (an *HTTPError for a refusal) to http.HandlerFunc.
func Handle(fn func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			WriteError(w, err)
		}
	}
}

// --- the mux ------------------------------------------------------------------------------------

// Router serves the mux with aiohttp's answers for what no route takes: 404 "404: Not Found" and 405 "405: Method
// Not Allowed" with the Allow header (Go's mux finds both; only its bodies differ). Do not register a "/" catch-all
// on the mux: it would take the 404s and the 405s.
func Router(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &probe{header: http.Header{}}
		handler.ServeHTTP(probe, r)
		switch probe.status {
		case http.StatusMethodNotAllowed:
			if allow := probe.header.Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			Error(w, http.StatusMethodNotAllowed)
		case http.StatusNotFound, 0:
			Error(w, http.StatusNotFound)
		default: // a redirect of the mux (a cleaned path): as the mux gives it
			for name, values := range probe.header {
				w.Header()[name] = values
			}
			w.WriteHeader(probe.status)
			_, _ = w.Write(probe.body.Bytes())
		}
	})
}

// NotFoundHandler is aiohttp's 404 as a handler.
func NotFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { Error(w, http.StatusNotFound) })
}

type probe struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (p *probe) Header() http.Header { return p.header }
func (p *probe) WriteHeader(status int) {
	if p.status == 0 {
		p.status = status
	}
}
func (p *probe) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return p.body.Write(b)
}

// --- request bodies -----------------------------------------------------------------------------

// ErrNotJSON is a body that is not JSON (Python's ValueError of request.json()).
var ErrNotJSON = errors.New("the body is not JSON")

// TooLarge is aiohttp's 413 of a body read whole past client_max_size.
func TooLarge(max int64) *HTTPError {
	return Fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("Maximum request body size %d exceeded.", max))
}

// ReadBody reads the whole body, at most max bytes: TooLarge past it.
func ReadBody(r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	if r.ContentLength > max {
		return nil, TooLarge(max)
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, TooLarge(max)
	}
	return data, nil
}

// ReadJSON is await request.json() under client_max_size: the value decoded by Decode; ErrNotJSON for a body that
// is not JSON, TooLarge for one past max.
func ReadJSON(r *http.Request, max int64) (any, error) {
	data, err := ReadBody(r, max)
	if err != nil {
		return nil, err
	}
	value, err := Decode(data)
	if err != nil {
		return nil, ErrNotJSON
	}
	return value, nil
}

// ReadForm is await request.post() of an urlencoded form under client_max_size (a multipart form is read too): the
// fields of the body only, never of the query.
func ReadForm(r *http.Request, max int64) (url.Values, error) {
	if r.Body == nil {
		return url.Values{}, nil
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(nil, r.Body, max)
		if err := r.ParseMultipartForm(max); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				return nil, TooLarge(max)
			}
			return url.Values{}, nil
		}
		return r.MultipartForm.Value, nil
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return url.Values{}, nil // aiohttp gives an empty form for any other body
	}
	data, err := ReadBody(r, max)
	if err != nil {
		return nil, err
	}
	values, err := url.ParseQuery(string(data))
	if err != nil {
		return url.Values{}, nil
	}
	return values, nil
}

// --- JSON values --------------------------------------------------------------------------------

// Decode is json.loads: objects are map[string]any, arrays []any, integers int64 and other numbers float64, as
// Python tells int from float (an integer past int64 becomes a float64).
func Decode(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("extra data after the JSON value")
	}
	return Numbers(value), nil
}

// Numbers turns the json.Number of a value decoded with UseNumber into int64 (an integer) or float64.
func Numbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		text := string(v)
		if !strings.ContainsAny(text, ".eE") {
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				return n
			}
		}
		f, _ := strconv.ParseFloat(text, 64)
		return f
	case map[string]any:
		for key, item := range v {
			v[key] = Numbers(item)
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = Numbers(item)
		}
		return v
	default:
		return value
	}
}

// PyValue makes a value decoded by Decode write as Python wrote it: every float64 in maps and slices becomes a
// pyjson.Float (8.0, not 8). Other values are returned as they are.
func PyValue(value any) any {
	switch v := value.(type) {
	case float64:
		return pyjson.Float(v)
	case float32:
		return pyjson.Float(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = PyValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = PyValue(item)
		}
		return out
	case Object:
		out := make(Object, len(v))
		for i, field := range v {
			out[i] = Field{field.Key, PyValue(field.Value)}
		}
		return out
	default:
		return value
	}
}

// Field is one key of an Object.
type Field struct {
	Key   string
	Value any
}

// Object is a JSON object that keeps its keys in order, as a Python dict does.
type Object []Field

// Get is the value of a key, nil when it is not there.
func (o Object) Get(key string) any {
	for _, field := range o {
		if field.Key == key {
			return field.Value
		}
	}
	return nil
}

// MarshalJSON writes the keys in their order (no HTML escapes; pyjson puts the separators).
func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, field := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := pyjson.Compact(field.Key)
		if err != nil {
			return nil, err
		}
		value, err := pyjson.Compact(field.Value)
		if err != nil {
			return nil, err
		}
		b.WriteString(key)
		b.WriteByte(':')
		b.WriteString(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// --- the language -------------------------------------------------------------------------------

// Lang is actions.language: the app's language (D41), English where the app has no settings or cannot read them.
func Lang(s *settings.Settings) string {
	if s == nil {
		return "en"
	}
	lang, err := s.String("language")
	if err != nil || lang == "" {
		return "en"
	}
	return lang
}

// DecodeOrdered is Decode with every object an Object in the order of its keys, as Python's json.loads keeps them
// (a key given twice keeps its first place and its last value).
func DecodeOrdered(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := ordered(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("extra data after the JSON value")
	}
	return value, nil
}

func ordered(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch t := token.(type) {
	case json.Delim:
		switch t {
		case '{':
			out := Object{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				value, err := ordered(decoder)
				if err != nil {
					return nil, err
				}
				name, _ := key.(string)
				replaced := false
				for i := range out {
					if out[i].Key == name {
						out[i].Value, replaced = value, true
					}
				}
				if !replaced {
					out = append(out, Field{name, value})
				}
			}
			_, err := decoder.Token() // }
			return out, err
		case '[':
			out := []any{}
			for decoder.More() {
				value, err := ordered(decoder)
				if err != nil {
					return nil, err
				}
				out = append(out, value)
			}
			_, err := decoder.Token() // ]
			return out, err
		}
		return nil, errors.New("unexpected delimiter")
	default:
		return Numbers(token), nil
	}
}
