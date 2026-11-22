package mail

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// --- letter to text -------------------------------------------------------------

// ws is Python's \s for text: Go's \s plus \v and the Unicode spaces.
const ws = `\s\x0b\x1c-\x1f\x85\xa0\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// ReplyHeader is a line that starts the quoted letter in a reply: everything from it down is cut.
var ReplyHeader = regexp.MustCompile(`(?i)^[` + ws + `]*(` +
	`On .{5,200} wrote:` +
	`|.{5,200} (пишет|написал|написала|написал\(а\)):` +
	`|.{0,120}<[^<>` + ws + `]+@[^<>` + ws + `]+>:` + // Yandex: 18.10.2026, 12:00, "Name" <a@b.ru>:
	`|-{2,}[` + ws + `]*(Original Message|Исходное сообщение)[` + ws + `]*-{2,}` +
	`|_{10,}` + // Outlook puts a line of underscores above "От: … Отправлено: …"
	`)[` + ws + `]*$`)

// invisible is preheader padding in mailings.
var invisible = regexp.MustCompile(`[\x{ad}\x{34f}\x{200b}-\x{200f}\x{2060}-\x{2064}\x{feff}]`)

var manyBreaks = regexp.MustCompile(`\n{3,}`)

var (
	blockTags = map[string]bool{"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true,
		"h3": true, "h4": true, "h5": true, "h6": true, "table": true, "ul": true, "ol": true, "hr": true}
	skipTags = map[string]bool{"script": true, "style": true, "head": true, "title": true}
)

// HTMLToText is the visible text with line breaks at block tags; quoted replies (blockquote, gmail_quote) are left
// out. A tokenizer, as Python's HTMLParser: no tree is built.
func HTMLToText(text string) string {
	z := html.NewTokenizer(strings.NewReader(text))
	var parts []string
	var skip []string // open tags whose content is dropped
	for {
		kind := z.Next()
		switch kind {
		case html.ErrorToken:
			return strings.Join(parts, "")
		case html.TextToken:
			if len(skip) == 0 {
				parts = append(parts, string(z.Text()))
			}
		case html.StartTagToken:
			token := z.Token()
			tag := token.Data
			if len(skip) > 0 {
				if tag == skip[len(skip)-1] {
					skip = append(skip, tag)
				}
				continue
			}
			classes := ""
			for _, a := range token.Attr {
				if a.Key == "class" {
					classes = a.Val
				}
			}
			if skipTags[tag] || tag == "blockquote" || strings.Contains(classes, "gmail_quote") ||
				strings.Contains(classes, "yandex_quote") {
				if tag != "br" && tag != "hr" {
					skip = append(skip, tag)
				}
				continue
			}
			if blockTags[tag] {
				parts = append(parts, "\n")
			}
		case html.SelfClosingTagToken:
			name, _ := z.TagName()
			if tag := string(name); len(skip) == 0 && (tag == "br" || tag == "hr") {
				parts = append(parts, "\n")
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if len(skip) > 0 {
				if tag == skip[len(skip)-1] {
					skip = skip[:len(skip)-1]
				}
				continue
			}
			if blockTags[tag] {
				parts = append(parts, "\n")
			}
		}
	}
}

// StripQuotes drops '>' lines, the quoted letter below a reply header and the signature below '-- '.
func StripQuotes(text string) string {
	var kept []string
	for _, line := range splitlines(text) {
		if ReplyHeader.MatchString(line) || strings.TrimRightFunc(line, pytext.IsSpace) == "--" {
			break
		}
		if !strings.HasPrefix(strings.TrimLeftFunc(line, pytext.IsSpace), ">") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// Tidy squeezes the spaces of every line and the blank lines between them.
func Tidy(text string) string {
	text = invisible.ReplaceAllString(text, "")
	all := splitlines(text)
	for i, line := range all {
		all[i] = strings.Join(pytext.Split(strings.ReplaceAll(line, "\u00a0", " ")), " ")
	}
	return pytext.Strip(manyBreaks.ReplaceAllString(strings.Join(all, "\n"), "\n\n"))
}

// bodyText is the plain text of the letter: text/plain if it says something, else the HTML part as text.
func bodyText(msg *part) string {
	text := ""
	if plain := msg.bodyOf("plain"); plain != nil {
		text = Tidy(StripQuotes(plain.text()))
	}
	if pytext.Len(text) < 20 {
		if htmlPart := msg.bodyOf("html"); htmlPart != nil {
			text = Tidy(StripQuotes(HTMLToText(htmlPart.text())))
		}
	}
	return text
}

// splitlines is str.splitlines(): lines split at \n, \r, \r\n, \v, \f, \x1c-\x1e, \x85, U+2028 and U+2029.
func splitlines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			start = i + size
		case '\r':
			out = append(out, s[start:i])
			if i+1 < len(s) && s[i+1] == '\n' {
				size = 2
			}
			start = i + size
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
