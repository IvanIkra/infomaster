package mail

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"

	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// A lenient reader of MIME letters, after Python's email package with policy.default: a letter is never refused.
// Headers run until a blank line or the first line that is no header; a multipart without its boundary is a part
// without parts; a body that cannot be decoded is taken as it is.

type header struct{ name, value string }

type part struct {
	headers  []header
	body     []byte // the raw body of a leaf (still in its transfer encoding)
	parts    []*part
	multi    bool // a multipart whose boundary was found
	ctype    string
	params   map[string]string
	parentMD bool // the parent is multipart/digest: the default type is message/rfc822
}

// headerLine is feedparser's headerRE: a field name of printable characters but ':' (or none), then ':'.
var headerLine = regexp.MustCompile(`^[\x21-\x39\x3b-\x7e]*:`)

// lines splits data after each line ending, keeping the endings.
func lines(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		i := bytes.IndexAny(data, "\r\n")
		if i < 0 {
			out = append(out, data)
			break
		}
		end := i + 1
		if data[i] == '\r' && end < len(data) && data[end] == '\n' {
			end++
		}
		out = append(out, data[:end])
		data = data[end:]
	}
	return out
}

func trimEOL(line []byte) []byte { return bytes.TrimRight(line, "\r\n") }

func parseMessage(raw []byte, parentDigest bool) *part {
	p := &part{parentMD: parentDigest}
	all := lines(raw)
	i := 0
	if len(all) > 0 && bytes.HasPrefix(all[0], []byte("From ")) { // a unix "From " line
		i = 1
	}
	for ; i < len(all); i++ {
		line := trimEOL(all[i])
		if len(line) == 0 {
			i++
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if n := len(p.headers); n > 0 {
				p.headers[n-1].value += string(line)
				continue
			}
		}
		if !headerLine.Match(line) {
			break // no header: the body starts here
		}
		name, value, _ := strings.Cut(string(line), ":")
		p.headers = append(p.headers, header{name, strings.TrimLeft(value, " \t")})
	}
	body := bytes.Join(all[i:], nil)
	p.ctype, p.params = p.contentType()
	if strings.HasPrefix(p.ctype, "multipart/") {
		if boundary := p.params["boundary"]; boundary != "" {
			p.parts, p.multi = splitParts(body, boundary, p.ctype == "multipart/digest")
		}
	}
	if !p.multi {
		p.body = body
	}
	return p
}

func splitParts(body []byte, boundary string, digest bool) ([]*part, bool) {
	separator := []byte("--" + boundary)
	var parts []*part
	var current [][]byte
	started := false
	flush := func() {
		if len(current) > 0 { // the line ending before a boundary belongs to the boundary
			last := len(current) - 1
			current[last] = trimEOL(current[last])
		}
		parts = append(parts, parseMessage(bytes.Join(current, nil), digest))
		current = nil
	}
	for _, line := range lines(body) {
		rest, isBoundary := bytes.CutPrefix(trimEOL(line), separator)
		closing := false
		if isBoundary {
			if after, ok := bytes.CutPrefix(rest, []byte("--")); ok {
				rest, closing = after, true
			}
			isBoundary = len(bytes.Trim(rest, " \t")) == 0
		}
		if !isBoundary {
			if started {
				current = append(current, line)
			}
			continue
		}
		if started {
			flush()
		}
		if closing {
			return parts, started
		}
		started = true
	}
	if started {
		flush()
	}
	return parts, started
}

func (p *part) get(name string) (string, bool) {
	for _, h := range p.headers {
		if strings.EqualFold(strings.TrimSpace(h.name), name) {
			return h.value, true
		}
	}
	return "", false
}

// contentType is get_content_type() and the type's parameters: text/plain by default or when it is no type.
func (p *part) contentType() (string, map[string]string) {
	fallback := "text/plain"
	if p.parentMD {
		fallback = "message/rfc822"
	}
	value, ok := p.get("Content-Type")
	if !ok {
		return fallback, map[string]string{}
	}
	ctype, params := parseParams(value)
	if strings.Count(ctype, "/") != 1 {
		return "text/plain", params
	}
	return ctype, params
}

// parseParams is a media type and its parameters (RFC 2231 included); a value mime refuses is read by hand.
func parseParams(value string) (string, map[string]string) {
	if mediaType, params, err := mime.ParseMediaType(value); err == nil {
		return mediaType, params
	}
	pieces := strings.Split(value, ";")
	params := map[string]string{}
	for _, piece := range pieces[1:] {
		key, val, ok := strings.Cut(piece, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		key = strings.TrimSuffix(key, "*")
		if _, seen := params[key]; !seen {
			params[key] = val
		}
	}
	return strings.ToLower(strings.TrimSpace(pieces[0])), params
}

func (p *part) maintype() string {
	main, _, _ := strings.Cut(p.ctype, "/")
	return main
}

func (p *part) subtype() string {
	_, sub, _ := strings.Cut(p.ctype, "/")
	return sub
}

// disposition is get_content_disposition(): lowercased, "" for none.
func (p *part) disposition() (string, map[string]string) {
	value, ok := p.get("Content-Disposition")
	if !ok {
		return "", map[string]string{}
	}
	d, params := parseParams(value)
	return strings.ToLower(d), params
}

func (p *part) isAttachment() bool {
	d, _ := p.disposition()
	return d == "attachment"
}

// filename is get_filename(): the disposition's filename, else the type's name; RFC 2231 and 2047 decoded.
func (p *part) filename() string {
	_, params := p.disposition()
	name, ok := params["filename"]
	if !ok {
		if name, ok = p.params["name"]; !ok {
			return ""
		}
	}
	return pytext.Strip(decodeHeader(name))
}

// findBody is _find_body: the parts of the preferred subtypes, depth first, without attachments; of a
// multipart/related only its start part.
func (p *part) findBody(subtype string, found func(*part) bool) bool {
	if p.isAttachment() {
		return false
	}
	switch p.maintype() {
	case "text":
		return p.subtype() == subtype && found(p)
	case "multipart":
	default:
		return false
	}
	if !p.multi {
		return false
	}
	if p.subtype() != "related" {
		for _, sub := range p.parts {
			if sub.findBody(subtype, found) {
				return true
			}
		}
		return false
	}
	var candidate *part
	if start := p.params["start"]; start != "" {
		for _, sub := range p.parts {
			if id, _ := sub.get("Content-ID"); id == start {
				candidate = sub
				break
			}
		}
	}
	if candidate == nil && len(p.parts) > 0 {
		candidate = p.parts[0]
	}
	return candidate != nil && candidate.findBody(subtype, found)
}

// body is get_body(preferencelist=(subtype,)); nil for none.
func (p *part) bodyOf(subtype string) *part {
	var body *part
	p.findBody(subtype, func(found *part) bool { body = found; return true })
	return body
}

var bodyTypes = map[string]bool{"text/plain": true, "text/html": true, "multipart/related": true,
	"multipart/alternative": true}

// attachments is iter_attachments(): the top-level parts that are not the letter's body.
func (p *part) attachments() []*part {
	if p.maintype() != "multipart" || p.subtype() == "alternative" || !p.multi {
		return nil
	}
	if p.subtype() == "related" {
		if start := p.params["start"]; start != "" {
			var rest []*part
			found := false
			for _, sub := range p.parts {
				if id, _ := sub.get("Content-ID"); id == start {
					found = true
				} else {
					rest = append(rest, sub)
				}
			}
			if found {
				return rest
			}
		}
		if len(p.parts) == 0 {
			return nil
		}
		return p.parts[1:]
	}
	var out []*part
	seen := map[string]bool{}
	for _, sub := range p.parts {
		if bodyTypes[sub.ctype] && !sub.isAttachment() && !seen[sub.subtype()] {
			seen[sub.subtype()] = true
			continue
		}
		out = append(out, sub)
	}
	return out
}

// payload is get_payload(decode=True): the body without its transfer encoding.
func (p *part) payload() []byte {
	cte, _ := p.get("Content-Transfer-Encoding")
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		return decodeBase64(p.body)
	case "quoted-printable":
		return decodeQP(p.body)
	}
	return p.body
}

func decodeBase64(data []byte) []byte {
	clean := make([]byte, 0, len(data))
	for _, c := range data {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' {
			clean = append(clean, c)
		}
	}
	if len(clean)%4 == 1 { // a stray character
		clean = clean[:len(clean)-1]
	}
	out, err := base64.RawStdEncoding.DecodeString(string(clean))
	if err != nil {
		return data
	}
	return out
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// decodeQP is quoted-printable as Python decodes it: a broken escape stays as it is.
func decodeQP(data []byte) []byte {
	var out []byte
	for _, line := range lines(data) {
		eol := line[len(trimEOL(line)):]
		body := bytes.TrimRight(trimEOL(line), " \t")
		soft := false
		if bytes.HasSuffix(body, []byte("=")) {
			body, soft = body[:len(body)-1], true
		}
		for i := 0; i < len(body); i++ {
			if body[i] == '=' && i+2 < len(body) {
				hi, ok1 := unhex(body[i+1])
				lo, ok2 := unhex(body[i+2])
				if ok1 && ok2 {
					out = append(out, hi<<4|lo)
					i += 2
					continue
				}
			}
			out = append(out, body[i])
		}
		if !soft && len(eol) > 0 {
			out = append(out, '\n')
		}
	}
	return out
}

// decoder is the decoding of a charset: nil for one that is not known.
func decoder(charset string) *encoding.Decoder {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "iso-8859-1", "iso8859-1", "latin-1", "latin1", "l1":
		return charmap.ISO8859_1.NewDecoder()
	}
	enc, err := htmlindex.Get(charset)
	if err != nil || enc == nil {
		return nil
	}
	return enc.NewDecoder()
}

// utf8Replace decodes UTF-8, each invalid byte as U+FFFD (errors="replace").
func utf8Replace(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var out strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		out.WriteRune(r)
		data = data[size:]
	}
	return out.String()
}

// decodeText is bytes.decode(charset, errors="replace"); an unknown charset reads as UTF-8 (_part_text).
func decodeText(data []byte, charset string) string {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "ascii", "us-ascii", "us_ascii", "646":
		var out strings.Builder
		for _, c := range data {
			if c < 0x80 {
				out.WriteByte(c)
			} else {
				out.WriteRune(utf8.RuneError)
			}
		}
		return out.String()
	case "utf-8", "utf8", "utf_8":
		return utf8Replace(data)
	}
	d := decoder(charset)
	if d == nil {
		return utf8Replace(data)
	}
	out, err := d.Bytes(data)
	if err != nil {
		return utf8Replace(data)
	}
	return string(out)
}

// text is _part_text: a text part's content, decoded.
func (p *part) text() string {
	charset, ok := p.params["charset"]
	if !ok {
		charset = "ascii"
	}
	return decodeText(p.payload(), charset)
}

var wordDecoder = &mime.WordDecoder{CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(decodeText(data, charset)), nil
}}

// decodeHeader is a header's value with its RFC 2047 words decoded; raw 8-bit text reads as UTF-8.
func decodeHeader(value string) string {
	value = utf8Replace([]byte(value))
	if decoded, err := wordDecoder.DecodeHeader(value); err == nil {
		return decoded
	}
	return value
}
