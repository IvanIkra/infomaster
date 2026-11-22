package oauth

import (
	"context"
	"testing"

	"github.com/IvanIkra/infomaster/go/internal/store"
)

const callback = "https://claude.ai/api/mcp/auth_callback"

type seconds struct{ value float64 }

func (s *seconds) now() float64 { return s.value }

// A connection unused past the refresh token's life is no longer listed.
func TestConnectionsClock(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	clock := &seconds{1_800_000_000}
	auth, err := New(s, "https://infomaster.example.ts.net", nil, nil, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	client, err := auth.Register(map[string]any{"redirect_uris": []any{callback}, "client_name": "Claude"})
	if err != nil {
		t.Fatal(err)
	}
	clientID := client.Get("client_id").(string)
	result, err := s.DB.Exec("insert into connector_grants(client_id, created_at, used_at) values(?, ?, ?)",
		clientID, 1_800_000_000, 1_800_000_000)
	if err != nil {
		t.Fatal(err)
	}
	grant, _ := result.LastInsertId()
	if _, err := auth.issue(grant, nil); err != nil {
		t.Fatal(err)
	}
	listed, err := auth.Connections()
	if err != nil || len(listed) != 1 || listed[0].ID != grant || listed[0].Name != "Claude" ||
		*listed[0].Redirect != "claude.ai" {
		t.Fatal(listed, err)
	}
	clock.value += RefreshTTL + 1
	if listed, _ := auth.Connections(); len(listed) != 0 {
		t.Fatal(listed)
	}
}

func TestNoBot(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	auth, err := New(s, "https://infomaster.example.ts.net", nil, func() string { return "ru" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := auth.Register(map[string]any{"redirect_uris": []any{callback}})
	if client.Get("client_name") != "claude.ai" {
		t.Fatal(client)
	}
	pageID, _, err := auth.OpenPage(Params{"client_id": client.Get("client_id").(string), "redirect_uri": callback,
		"response_type": "code", "code_challenge": S256("a"), "code_challenge_method": "S256"})
	if err != nil {
		t.Fatal(err)
	}
	if problem := auth.Send(context.Background(), pageID); problem != "Бот не запущен." {
		t.Fatal(problem)
	}
	// The oldest pages go past PagesMax.
	for i := 0; i < PagesMax; i++ {
		auth.OpenPage(Params{"client_id": client.Get("client_id").(string), "redirect_uri": callback,
			"response_type": "code", "code_challenge": S256("a"), "code_challenge_method": "S256"})
	}
	if auth.Page(pageID) != nil || len(auth.pages) != PagesMax || len(auth.order) != PagesMax {
		t.Fatal(len(auth.pages))
	}
}

func TestS256(t *testing.T) {
	verifier := ""
	for i := 0; i < 43; i++ {
		verifier += "a"
	}
	if got := S256(verifier); got != "ZtNPunH49FD35FWYhT5Tv8I7vRKQJ8uxMaL0_9eHjNA" {
		t.Fatal(got)
	}
}

func TestRedirects(t *testing.T) {
	for uri, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback":      true,
		"https://CLAUDE.com/api/mcp/auth_callback":     true,
		"https://claude.ai:443/api/mcp/auth_callback":  false,
		"https://claude.ai/api/mcp/auth_callback?x=1":  true,
		"https://claude.ai/api/mcp/auth_callback#f":    false,
		"https://a:b@claude.ai/api/mcp/auth_callback":  false,
		"http://localhost/cb":                          true,
		"http://127.0.0.1:8080/x/y":                    true,
		"http://[::1]:1/cb":                            true,
		"http://[::1/cb":                               false,
		"http://localhost:abc/cb":                      false,
		"http://localhost:65536/cb":                    false,
		"http://localhost.evil.example/cb":             false,
		"javascript:alert(1)":                          false,
		"  https://claude.ai/api/mcp/auth_callback":    true,
		"https://claude.ai/api/mcp/auth_call\nback":    true, // Python drops tabs and newlines
		"https://claude.ai/api/mcp/auth_callback/../x": false,
	} {
		if RedirectAllowed(uri) != want {
			t.Errorf("%q: want %v", uri, want)
		}
	}
	if !SameRedirect("http://localhost:1/cb?a=1", "http://localhost:2/cb?a=1") ||
		SameRedirect("http://localhost:1/cb", "http://127.0.0.1:1/cb") ||
		SameRedirect("https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback/") {
		t.Fatal("same redirect")
	}
}
