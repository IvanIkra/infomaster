package texbuild

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fake is a compiler script: a source with "BROKEN" fails with a TeX error, "SLOW" sleeps, "REF" asks for a second
// pass; anything else makes a PDF. Each pass is counted in passes.
func fake(t *testing.T) (Compiler, string) {
	t.Helper()
	dir := t.TempDir()
	passes := filepath.Join(dir, "passes")
	script := filepath.Join(dir, "tex")
	body := `#!/bin/sh
for last; do :; done
echo x >> ` + passes + `
case "$*" in *-no-shell-escape*) ;; *) echo "shell escape not off" > main.log; exit 1;; esac
[ "$openin_any" = p ] || { echo "openin_any not p" > main.log; exit 1; }
if grep -q BROKEN "$last"; then
  printf 'This is XeTeX\n! Undefined control sequence.\nl.3 \\brokn\n          {x}\nmore\n' > main.log; exit 1
fi
if grep -q SLOW "$last"; then sleep 5; fi
if grep -q REF "$last" && [ ! -f seen ]; then touch seen; echo "Rerun to get cross-references right" > main.log
else echo "Output written" > main.log; fi
printf '%%PDF-1.7 fake' > main.pdf
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return Compiler{Command: []string{script}, WorkDir: dir}, passes
}

func count(t *testing.T, path string) int {
	data, _ := os.ReadFile(path)
	n := strings.Count(string(data), "x")
	os.Remove(path)
	return n
}

func TestBuild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	c, passes := fake(t)
	pdf, err := c.Build(context.Background(), []byte(`\documentclass{article}`))
	if err != nil || string(pdf) != "%PDF-1.7 fake" || count(t, passes) != 1 {
		t.Fatalf("plain: %q %v", pdf, err)
	}
	if _, err := c.Build(context.Background(), []byte("REF")); err != nil || count(t, passes) != 2 {
		t.Fatalf("a second pass: %v", err)
	}
	_, err = c.Build(context.Background(), []byte("BROKEN"))
	var failed *Failure
	if !errors.As(err, &failed) || !strings.Contains(failed.Log, "! Undefined control sequence.") ||
		!strings.Contains(failed.Log, `l.3 \brokn`) || strings.Contains(failed.Log, "XeTeX") {
		t.Fatalf("broken: %v", err)
	}
	if _, err := c.Build(context.Background(), nil); !errors.As(err, &failed) {
		t.Fatalf("empty: %v", err)
	}
	c.Timeout = 300 * time.Millisecond
	if _, err := c.Build(context.Background(), []byte("SLOW")); !errors.As(err, &failed) ||
		!strings.Contains(failed.Log, "stopped") {
		t.Fatalf("slow: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(c.WorkDir, "build-*"))
	if len(left) != 0 {
		t.Fatalf("build directories left: %v", left)
	}
}

func TestExcerpt(t *testing.T) {
	long := strings.Repeat("line\n", 100) + "the end"
	if got := Excerpt(long); !strings.HasSuffix(got, "the end") || strings.Count(got, "\n") != 29 {
		t.Fatalf("tail: %q", got)
	}
	if got := Excerpt("! " + strings.Repeat("я", MaxLog*2)); len([]rune(got)) != MaxLog+2 {
		t.Fatalf("cut: %d", len([]rune(got)))
	}
}

func TestHandlerAndClient(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	c, _ := fake(t)
	server := httptest.NewServer(Handler(c))
	defer server.Close()
	client := NewClient(server.URL + "/")
	pdf, err := client.Build(context.Background(), []byte("ok"))
	if err != nil || !strings.HasPrefix(string(pdf), "%PDF-") {
		t.Fatalf("built: %q %v", pdf, err)
	}
	_, err = client.Build(context.Background(), []byte("BROKEN"))
	var failed *Failure
	if !errors.As(err, &failed) || !strings.Contains(failed.Log, "Undefined control sequence") {
		t.Fatalf("broken: %v", err)
	}
	_, err = client.Build(context.Background(), make([]byte, MaxSource+1))
	if !errors.As(err, &failed) || !strings.Contains(failed.Log, "over") {
		t.Fatalf("too big: %v", err)
	}
	if NewClient("") != nil {
		t.Fatal("no url, no client")
	}

	// one build at a time: the second is refused while the first runs
	done := make(chan error)
	go func() { _, err := client.Build(context.Background(), []byte("SLOW")); done <- err }()
	time.Sleep(500 * time.Millisecond)
	_, err = client.Build(context.Background(), []byte("ok"))
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("slow: %v", err)
	}
	resp, err := http.Get(server.URL + "/health")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v", err)
	}

	gone := NewClient("http://127.0.0.1:1")
	if _, err := gone.Build(context.Background(), []byte("x")); err == nil ||
		!strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("gone: %v", err)
	}
}

// With TeX installed, the guide's preamble builds for real.
func TestRealXeLaTeX(t *testing.T) {
	if _, err := exec.LookPath("xelatex"); err != nil || testing.Short() {
		t.Skip("no xelatex")
	}
	source, err := os.ReadFile(filepath.Join("testdata", "guide.tex"))
	if err != nil {
		t.Fatal(err)
	}
	// a TeX of one's own may keep packages under the user's TEXMFHOME, which the builder's HOME hides
	home, _ := exec.Command("kpsewhich", "-var-value", "TEXMFHOME").Output()
	env := []string{"TEXMFHOME=" + strings.TrimSpace(string(home))}
	pdf, err := Compiler{Command: []string{"xelatex"}, WorkDir: t.TempDir(), Env: env}.Build(context.Background(),
		source)
	if err != nil || !strings.HasPrefix(string(pdf), "%PDF-") {
		t.Fatalf("real: %v", err)
	}
	_, err = Compiler{Command: []string{"xelatex"}, WorkDir: t.TempDir(), Env: env}.Build(context.Background(),
		[]byte(`\documentclass{article}\begin{document}\input{/etc/passwd}\end{document}`))
	var failed *Failure
	if !errors.As(err, &failed) {
		t.Fatalf("reading outside the build was let through: %v", err)
	}
}
