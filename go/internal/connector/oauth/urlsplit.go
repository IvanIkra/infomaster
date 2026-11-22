package oauth

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

// split is Python's urllib.parse.urlsplit, as far as the checks of redirect addresses read it: Go's url.Parse
// refuses or reads differently some addresses Python takes.
type split struct {
	Scheme, Netloc, Path, Query, Fragment string
	Invalid                               bool // Python raises ValueError: a bracket of an IPv6 host without its pair
}

func urlsplit(raw string) split {
	raw = strings.TrimLeft(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13"+
		"\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	var parts split
	if i := strings.IndexByte(raw, ':'); i > 0 && isAlpha(raw[0]) && validScheme(raw[:i]) {
		parts.Scheme, raw = strings.ToLower(raw[:i]), raw[i+1:]
	}
	if strings.HasPrefix(raw, "//") {
		rest := raw[2:]
		end := len(rest)
		for _, c := range "/?#" {
			if i := strings.IndexRune(rest, c); i >= 0 && i < end {
				end = i
			}
		}
		parts.Netloc, raw = rest[:end], rest[end:]
		parts.Invalid = strings.Contains(parts.Netloc, "[") != strings.Contains(parts.Netloc, "]")
		if !parts.Invalid && strings.Contains(parts.Netloc, "[") {
			host, _ := parts.hostPort() // Python 3.12 refuses a bracketed host that is no IPv6 address
			parts.Invalid = net.ParseIP(host) == nil || !strings.Contains(host, ":")
		}
	}
	if before, after, ok := strings.Cut(raw, "#"); ok {
		raw, parts.Fragment = before, after
	}
	if before, after, ok := strings.Cut(raw, "?"); ok {
		raw, parts.Query = before, after
	}
	parts.Path = raw
	return parts
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func validScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlpha(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func (s split) hostinfo() (userinfo string, hasUser bool, hostinfo string) {
	if i := strings.LastIndexByte(s.Netloc, '@'); i >= 0 {
		return s.Netloc[:i], true, s.Netloc[i+1:]
	}
	return "", false, s.Netloc
}

// Username and Password are "" when not given (Python's None and "" are both falsy where they are read).
func (s split) Username() string {
	userinfo, _, _ := s.hostinfo()
	name, _, _ := strings.Cut(userinfo, ":")
	return name
}

func (s split) Password() string {
	userinfo, _, _ := s.hostinfo()
	_, password, _ := strings.Cut(userinfo, ":")
	return password
}

func (s split) hostPort() (host, port string) {
	_, _, hostinfo := s.hostinfo()
	if _, bracketed, ok := strings.Cut(hostinfo, "["); ok {
		host, rest, _ := strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(rest, ":")
		return host, port
	}
	host, port, _ = strings.Cut(hostinfo, ":")
	return host, port
}

// Hostname is lowercased; "" for none (Python's None).
func (s split) Hostname() string {
	host, _ := s.hostPort()
	return strings.ToLower(host)
}

var errPort = errors.New("bad port")

// Port is -1 for none; errPort for one that is not a number of 0-65535 (Python's ValueError).
func (s split) Port() (int, error) {
	_, port := s.hostPort()
	if port == "" {
		return -1, nil
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return 0, errPort
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n > 65535 {
		return 0, errPort
	}
	return n, nil
}
