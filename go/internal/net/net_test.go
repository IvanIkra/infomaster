package net

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// D79: the service fetches what other people link to from public addresses only.

func TestPublicAddresses(t *testing.T) {
	for _, address := range []string{"8.8.8.8", "2a00:1450:4010:c05::64", "1.1.1.1"} {
		if !IsPublic(address) {
			t.Error(address)
		}
	}
	for _, address := range []string{"127.0.0.1", "10.0.0.5", "172.30.77.10", "192.168.1.1", "169.254.169.254",
		"100.100.100.100", "::1", "fe80::1", "fd7a:115c:a1e0::1", "::ffff:127.0.0.1", "2002:7f00:1::", "0.0.0.0",
		"224.0.0.1", "not an address", "fe80::1%eth0", "ff02::1"} {
		if IsPublic(address) {
			t.Error(address)
		}
	}
}

func TestALocalServerIsNeverReached(t *testing.T) {
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Write([]byte("%PDF-1.4 secret"))
	}))
	defer server.Close()
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	for _, host := range []string{"127.0.0.1", "localhost"} {
		_, err := Client(0).Get(fmt.Sprintf("http://%s:%s/x.pdf", host, port))
		var private *PrivateAddress
		if !errors.As(err, &private) {
			t.Fatalf("%s: %v", host, err)
		}
	}
	if len(asked) != 0 {
		t.Fatal(asked)
	}
}
