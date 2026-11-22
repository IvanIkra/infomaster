package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve is a made-up service: its answers by path, and the token it wants.
func serve(t *testing.T, answers map[string]string, headers map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "401: Unauthorized", http.StatusUnauthorized)
			return
		}
		body, ok := answers[r.URL.Path]
		if !ok {
			http.Error(w, "404: Not Found", http.StatusNotFound)
			return
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

const feed = "BEGIN:VCALENDAR\r\nDTSTAMP:20261123T100000Z\r\nBEGIN:VEVENT\r\nSUMMARY:Algebra\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestSameAnswersAgree(t *testing.T) {
	a := serve(t, map[string]string{
		"/api/status":                             `{"telegram": {"ok_at": "2026-11-23T10:00:00", "error": null}, "ruz": {"ok_at": null, "error": null}}`,
		"/api/deadlines":                          `{"items": [{"uid": "tg-1", "title": "HW 1", "due": "2026-11-30"}]}`,
		"/api/news":                               `{"items": [], "ttl_hours": 24}`,
		"/api/grades":                             `{"items": [{"value": 8.0}], "window_days": 7}`,
		"/calendar/internal-secret/main.ics":      feed,
		"/calendar/internal-secret/optional.ics":  feed,
		"/calendar/internal-secret/tablo.ics":     feed,
		"/.well-known/oauth-authorization-server": `{"issuer": "https://example.test"}`,
		"/.well-known/oauth-protected-resource":   `{"resource": "https://example.test/mcp"}`,
		"/web/":                                   "<!doctype html>",
	}, map[string]string{"Content-Security-Policy": "default-src 'self'"})
	b := serve(t, map[string]string{
		// another time of the last poll, keys in another order, 8 for 8.0, another stamp: all the same
		"/api/status":                             `{"ruz": {"error": null, "ok_at": null}, "telegram": {"ok_at": "2026-11-23T10:05:00", "error": null}}`,
		"/api/deadlines":                          `{"items": [{"due": "2026-11-30", "title": "HW 1", "uid": "tg-1"}]}`,
		"/api/news":                               `{"ttl_hours": 24, "items": []}`,
		"/api/grades":                             `{"items": [{"value": 8}], "window_days": 7}`,
		"/calendar/internal-secret/main.ics":      strings.Replace(feed, "100000Z", "100500Z", 1),
		"/calendar/internal-secret/optional.ics":  feed,
		"/calendar/internal-secret/tablo.ics":     feed,
		"/.well-known/oauth-authorization-server": `{"issuer": "https://example.test"}`,
		"/.well-known/oauth-protected-resource":   `{"resource": "https://example.test/mcp"}`,
		"/web/":                                   "<!doctype html>",
	}, map[string]string{"Content-Security-Policy": "default-src 'self'"})
	var out strings.Builder
	bases := map[string][2]string{"internal": {a.URL, b.URL}, "public": {a.URL, b.URL}}
	if !Run(&out, a.Client(), bases, Checks("internal-secret", ""), "secret-token") {
		t.Fatalf("the same answers differ:\n%s", out.String())
	}
	if strings.Contains(out.String(), "internal-secret") || strings.Contains(out.String(), "secret-token") {
		t.Fatalf("a secret is printed:\n%s", out.String())
	}
}

func TestDifferencesAreTold(t *testing.T) {
	a := serve(t, map[string]string{
		"/api/status":    `{"telegram": {"error": null}, "mail": {"error": null}}`,
		"/api/deadlines": `{"items": [{"uid": "tg-1", "due": "2026-11-30"}]}`,
		"/api/news":      `{"items": []}`,
		"/web/":          "<!doctype html>",
	}, map[string]string{"Content-Security-Policy": "default-src 'self'"})
	b := serve(t, map[string]string{
		"/api/status":    `{"telegram": {"error": "login needed"}}`,
		"/api/deadlines": `{"items": [{"uid": "tg-1", "due": "2026-12-01"}, {"uid": "tg-2", "due": "2026-12-02"}]}`,
		"/web/":          "<!doctype html>",
	}, map[string]string{"Content-Security-Policy": "default-src 'none'"})
	var out strings.Builder
	bases := map[string][2]string{"internal": {a.URL, b.URL}, "public": {a.URL, b.URL}}
	if Run(&out, a.Client(), bases, Checks("", ""), "secret-token") {
		t.Fatalf("no difference found:\n%s", out.String())
	}
	for _, want := range []string{
		"mail: only in a",
		`telegram.error: null != "login needed"`,
		"$.items: 1 items != 2",
		`$.items[0].due: "2026-11-30" != "2026-12-01"`,
		"status 200 != 404",
		"CSP",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q is not told:\n%s", want, out.String())
		}
	}
}

func TestFeedsDropOnlyTheirStamps(t *testing.T) {
	folded := "BEGIN:VCALENDAR\r\nDTSTAMP:20261123T100000Z\r\n X-FOLDED\r\nSUMMARY:A\r\nEND:VCALENDAR\r\n"
	if got := string(withoutStamps([]byte(folded))); got != "BEGIN:VCALENDAR\r\nSUMMARY:A\r\nEND:VCALENDAR\r\n" {
		t.Fatalf("%q", got)
	}
	diffs := Compare("ical", Answer{Status: 200, Body: []byte(feed)},
		Answer{Status: 200, Body: []byte(strings.Replace(feed, "Algebra", "Algebra 2", 1))})
	if len(diffs) != 1 || !strings.Contains(diffs[0], `"SUMMARY:Algebra" != "SUMMARY:Algebra 2"`) {
		t.Fatalf("%v", diffs)
	}
}

func TestPublicChecksWaitForTheirAddresses(t *testing.T) {
	a := serve(t, map[string]string{"/api/status": `{}`, "/api/deadlines": `{}`, "/api/news": `{}`,
		"/api/grades": `{}`}, nil)
	var out strings.Builder
	if !Run(&out, a.Client(), map[string][2]string{"internal": {a.URL, a.URL}}, Checks("", ""), "secret-token") {
		t.Fatalf("%s", out.String())
	}
	if !strings.Contains(out.String(), "skip  public /web/ (no public addresses)") {
		t.Fatalf("%s", out.String())
	}
}
