package ruz

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/clock"
)

// A small RFC 5545 reader: what the RUZ feed needs, read the way Python's icalendar reads it.

// property is one content line: NAME;PARAM=VALUE:value.
type property struct {
	name   string
	params map[string]string
	value  string
}

// component is a BEGIN/END block with its properties (the first of each name) and children.
type component struct {
	name     string
	props    map[string]property
	children []*component
}

func (c *component) get(name string) (property, bool) {
	p, ok := c.props[name]
	return p, ok
}

// text is a TEXT value unescaped: \n and \N a newline, \, \; and \\ the character.
func (c *component) text(name string) string {
	p, ok := c.props[name]
	if !ok {
		return ""
	}
	return unescape(p.value)
}

func (c *component) walk(name string, visit func(*component)) {
	if c.name == name {
		visit(c)
	}
	for _, child := range c.children {
		child.walk(name, visit)
	}
}

var folded = regexp.MustCompile(`\r?\n[ \t]`)

// readCalendar parses a VCALENDAR; an error for text that is no calendar.
func readCalendar(data []byte) (*component, error) {
	if !utf8.Valid(data) {
		data = bytes.ToValidUTF8(data, []byte("�"))
	}
	text := folded.ReplaceAllString(string(data), "")
	var root *component
	var stack []*component
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		p, err := contentLine(line)
		if err != nil {
			return nil, err
		}
		switch p.name {
		case "BEGIN":
			c := &component{name: strings.ToUpper(p.value), props: map[string]property{}}
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				top.children = append(top.children, c)
			} else if root == nil {
				root = c
			} else {
				return nil, errors.New("more than one calendar")
			}
			stack = append(stack, c)
		case "END":
			if len(stack) == 0 || stack[len(stack)-1].name != strings.ToUpper(p.value) {
				return nil, fmt.Errorf("END:%s without its BEGIN", p.value)
			}
			stack = stack[:len(stack)-1]
		default:
			if len(stack) == 0 {
				return nil, fmt.Errorf("content line outside a component: %q", line)
			}
			top := stack[len(stack)-1]
			if _, seen := top.props[p.name]; !seen {
				top.props[p.name] = p
			}
		}
	}
	if root == nil || root.name != "VCALENDAR" {
		return nil, errors.New("no VCALENDAR in the feed")
	}
	if len(stack) > 0 {
		return nil, fmt.Errorf("%s is not closed", stack[len(stack)-1].name)
	}
	return root, nil
}

// contentLine splits NAME;PARAM=VALUE;PARAM="VA:LUE":value.
func contentLine(line string) (property, error) {
	p := property{params: map[string]string{}}
	i, quoted := 0, false
	nameEnd, valueStart := -1, -1
	for ; i < len(line); i++ {
		c := line[i]
		if c == '"' {
			quoted = !quoted
		} else if !quoted && (c == ';' || c == ':') && nameEnd < 0 {
			nameEnd = i
		}
		if !quoted && c == ':' {
			valueStart = i + 1
			break
		}
	}
	if nameEnd <= 0 || valueStart < 0 {
		return p, fmt.Errorf("not a content line: %q", line)
	}
	p.name = strings.ToUpper(line[:nameEnd])
	p.value = line[valueStart:]
	for _, param := range splitParams(line[nameEnd : valueStart-1]) {
		key, value, _ := strings.Cut(param, "=")
		p.params[strings.ToUpper(key)] = strings.Trim(value, `"`)
	}
	return p, nil
}

func splitParams(text string) []string {
	var out []string
	quoted, start := false, -1
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '"':
			quoted = !quoted
		case text[i] == ';' && !quoted:
			if start >= 0 {
				out = append(out, text[start:i])
			}
			start = i + 1
		}
	}
	if start >= 0 && start <= len(text) {
		out = append(out, text[start:])
	}
	return out
}

func unescape(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			switch value[i+1] {
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			case ',', ';', '\\':
				b.WriteByte(value[i+1])
				i++
				continue
			}
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

// moment reads a DATE or DATE-TIME as Moscow local time: a date from its midnight, UTC (Z) and TZID converted, a
// floating time taken as it is.
func moment(p property) (time.Time, error) {
	value := strings.TrimSpace(p.value)
	if len(value) == 8 {
		return time.ParseInLocation("20060102", value, clock.TZ)
	}
	if strings.HasSuffix(value, "Z") {
		t, err := time.Parse("20060102T150405Z", value)
		if err != nil {
			return time.Time{}, err
		}
		return t.In(clock.TZ), nil
	}
	zone := clock.TZ
	if tzid := p.params["TZID"]; tzid != "" {
		if loc, err := time.LoadLocation(tzid); err == nil {
			zone = loc
		}
	}
	t, err := time.ParseInLocation("20060102T150405", value, zone)
	if err != nil {
		return time.Time{}, err
	}
	return t.In(clock.TZ), nil
}
