// Package net is outbound HTTP to addresses on the public internet only (D79).
//
// Links in chats and letters, wiki pages and grade sheets name hosts that other people choose. A request to such a
// host must not reach the service's neighbours: the docker network (Tablo, the internal port), the host, the tailnet,
// a cloud's metadata address. Client resolves every host itself and connects only to global addresses, on every
// redirect too, and by the address it checked, so a name that resolves anew (DNS rebinding) changes nothing.
package net

import (
	"context"
	"fmt"
	stdnet "net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// PrivateAddress is the error of a host that is not on the public internet. It reaches the caller through
// *url.Error and *net.OpError: errors.As finds it.
type PrivateAddress struct {
	Host string
}

func (e *PrivateAddress) Error() string { return e.Host + " is not a public address" }

// IsPublic reports whether an address (an IP, maybe with a %zone) is global and not multicast, as Python's
// ipaddress has it. It is a variable only so that tests may serve "public" sheets from 127.0.0.1, as the Python
// tests monkeypatch net.is_public; the service never changes it.
var IsPublic = isPublic

func mustPrefixes(texts ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(texts))
	for i, t := range texts {
		out[i] = netip.MustParsePrefix(t)
	}
	return out
}

// Not globally reachable blocks of the IANA special registries, as Python 3.12's ipaddress lists them.
var (
	private4 = mustPrefixes("0.0.0.0/8", "10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.0.170/31", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32")
	private4Except = mustPrefixes("192.0.0.9/32", "192.0.0.10/32")
	shared4        = netip.MustParsePrefix("100.64.0.0/10") // neither private nor global
	multicast4     = netip.MustParsePrefix("224.0.0.0/4")
	private6       = mustPrefixes("::1/128", "::/128", "::ffff:0:0/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10")
	private6Except = mustPrefixes("2001:1::1/128", "2001:1::2/128", "2001:3::/32", "2001:4:112::/48",
		"2001:20::/28", "2001:30::/28")
	multicast6 = netip.MustParsePrefix("ff00::/8")
)

func inAny(ip netip.Addr, list []netip.Prefix) bool {
	for _, p := range list {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func isPublic(address string) bool {
	address, _, _ = strings.Cut(address, "%")
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	if ip.Is6() {
		b := ip.As16()
		mapped := ip.Is4In6()
		if !mapped && b[0] == 0x20 && b[1] == 0x02 { // 6to4: 2002:<v4>::
			ip, mapped = netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), false
		} else if mapped {
			ip = ip.Unmap()
		}
	}
	if ip.Is4() {
		private := inAny(ip, private4) && !inAny(ip, private4Except)
		return !shared4.Contains(ip) && !private && !multicast4.Contains(ip)
	}
	private := inAny(ip, private6) && !inAny(ip, private6Except)
	return !private && !multicast6.Contains(ip)
}

// Resolver looks names up; a variable for tests.
var Resolver = stdnet.DefaultResolver

// DialContext connects to a public address of the host only: it resolves the name itself and dials the address it
// checked.
func DialContext(ctx context.Context, network, address string) (stdnet.Conn, error) {
	host, port, err := stdnet.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var found []string
	if _, err := netip.ParseAddr(strings.SplitN(host, "%", 2)[0]); err == nil {
		found = []string{host}
	} else {
		addrs, err := Resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			text := a.IP.String()
			if a.Zone != "" {
				text += "%" + a.Zone
			}
			found = append(found, text)
		}
	}
	var public []string
	for _, a := range found {
		if IsPublic(a) {
			public = append(public, a)
		}
	}
	if len(public) == 0 {
		return nil, &PrivateAddress{Host: host}
	}
	dialer := &stdnet.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	var first error
	for _, a := range public {
		conn, err := dialer.DialContext(ctx, network, stdnet.JoinHostPort(a, port))
		if err == nil {
			return conn, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, fmt.Errorf("cannot connect to %s: %w", host, first)
}

// Client is an http.Client that connects to public addresses only, with a total timeout (0: none). No proxy from
// the environment is used: it would connect for us, past the check.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport()}
}

// Transport is the http.Transport of Client.
func Transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
