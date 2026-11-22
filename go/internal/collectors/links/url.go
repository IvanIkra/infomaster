package links

import (
	"strconv"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// URL is urllib.parse.urlsplit's result: the netloc keeps the port and the user, the path stays as written.
type URL struct {
	Scheme, Netloc, Path, Query, Fragment string
}

// SplitURL is urllib.parse.urlsplit.
func SplitURL(text string) URL {
	text = strings.TrimLeftFunc(text, func(r rune) bool { return r <= ' ' })
	text = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, text)
	var u URL
	if i := strings.Index(text, ":"); i > 0 && isASCIILetter(text[0]) {
		ok := true
		for _, c := range []byte(text[:i]) {
			if !isASCIILetter(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
				ok = false
				break
			}
		}
		if ok {
			u.Scheme, text = strings.ToLower(text[:i]), text[i+1:]
		}
	}
	if strings.HasPrefix(text, "//") {
		end := strings.IndexAny(text[2:], "/?#")
		if end < 0 {
			end = len(text) - 2
		}
		u.Netloc, text = text[2:2+end], text[2+end:]
	}
	if i := strings.Index(text, "#"); i >= 0 {
		text, u.Fragment = text[:i], text[i+1:]
	}
	if i := strings.Index(text, "?"); i >= 0 {
		text, u.Query = text[:i], text[i+1:]
	}
	u.Path = text
	return u
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// String is urllib.parse.urlunsplit.
func (u URL) String() string {
	text := u.Path
	if u.Netloc != "" {
		if text != "" && !strings.HasPrefix(text, "/") {
			text = "/" + text
		}
		text = "//" + u.Netloc + text
	} else if strings.HasPrefix(text, "//") {
		text = "//" + text
	}
	if u.Scheme != "" {
		text = u.Scheme + ":" + text
	}
	if u.Query != "" {
		text += "?" + u.Query
	}
	if u.Fragment != "" {
		text += "#" + u.Fragment
	}
	return text
}

// Host is the netloc in lower case without a leading "www.": what this service tells links apart by.
func (u URL) Host() string { return strings.TrimPrefix(strings.ToLower(u.Netloc), "www.") }

// Pair is one name and value of a query.
type Pair struct{ Key, Value string }

// ParseQSL is urllib.parse.parse_qsl: pairs in order, "+" and %XX decoded.
func ParseQSL(query string, keepBlank bool) []Pair {
	var out []Pair
	for _, field := range strings.Split(query, "&") {
		if field == "" {
			continue
		}
		key, value, found := strings.Cut(field, "=")
		if !found && !keepBlank {
			continue
		}
		if value == "" && !keepBlank {
			continue
		}
		out = append(out, Pair{Unquote(strings.ReplaceAll(key, "+", " ")), Unquote(strings.ReplaceAll(value, "+", " "))})
	}
	return out
}

// URLEncode is urllib.parse.urlencode of pairs: quote_plus of each name and value.
func URLEncode(pairs []Pair) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = quotePlus(p.Key) + "=" + quotePlus(p.Value)
	}
	return strings.Join(parts, "&")
}

func quotePlus(s string) string {
	return strings.ReplaceAll(pytext.Quote(s, " "), " ", "+")
}

// Unquote is urllib.parse.unquote: %XX decoded as UTF-8 (bad bytes replaced), anything else kept.
func Unquote(text string) string {
	if !strings.Contains(text, "%") {
		return text
	}
	var out []byte
	for i := 0; i < len(text); i++ {
		if text[i] == '%' && i+2 < len(text) {
			if b, err := strconv.ParseUint(text[i+1:i+3], 16, 8); err == nil {
				out = append(out, byte(b))
				i += 2
				continue
			}
		}
		out = append(out, text[i])
	}
	return strings.ToValidUTF8(string(out), "�")
}
