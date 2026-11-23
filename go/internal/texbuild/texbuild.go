// Package texbuild builds a knowledge base document's PDF from its LaTeX source on the server (D84), so a client
// without a shell (claude.ai, a phone) can publish a lecture, a seminar or a book.
//
// The builder runs in a container of its own (`texbuild` in compose.yaml): no data, no secrets, no way out of its
// internal network, a read-only root, one CPU and a memory limit. That container is the boundary; TeX's own limits
// (no shell escape, reading and writing only under the build's directory) come second. One build at a time: another
// one is refused as busy, so the small host never runs two. The source is one .tex file and nothing else: no
// pictures, no bibliography.
//
// The service reaches it by Client (TEX_URL); the builder serves Handler (cmd/texbuild).
package texbuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	MaxSource = 2 << 20  // bytes of a source, as start_document_version takes it inline
	MaxPDF    = 64 << 20 // bytes of a PDF, as the library keeps it
	MaxLog    = 6000     // characters of a failed build's log given back
	Timeout   = 50 * time.Second
)

// Compiler runs TeX over one source.
type Compiler struct {
	Command []string      // the compiler and its first arguments; xelatex under nice by default
	Timeout time.Duration // of the whole build, every pass
	WorkDir string        // where each build gets a directory of its own; the system's temp by default
	Env     []string      // more of TeX's environment (a TEXMFHOME of one's own), after the builder's
}

// DefaultCommand is XeLaTeX at a low priority.
var DefaultCommand = []string{"nice", "-n", "10", "xelatex"}

// Failure is a build TeX refused: the text is the log's errors.
type Failure struct{ Log string }

func (f *Failure) Error() string { return "the build failed:\n" + f.Log }

// rerun says a pass left references to a second one.
func rerun(log string) bool {
	return strings.Contains(log, "Rerun to get") || strings.Contains(log, "Label(s) may have changed")
}

// Build is the source's PDF; a *Failure with the log's errors when TeX refuses it.
func (c Compiler) Build(ctx context.Context, source []byte) ([]byte, error) {
	if len(source) == 0 {
		return nil, &Failure{"the source is empty"}
	}
	if len(source) > MaxSource {
		return nil, &Failure{fmt.Sprintf("the source is over %d bytes", MaxSource)}
	}
	command := c.Command
	if len(command) == 0 {
		command = DefaultCommand
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dir, err := os.MkdirTemp(c.WorkDir, "build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "main.tex"), source, 0o600); err != nil {
		return nil, err
	}
	for pass := 1; pass <= 2; pass++ {
		args := append(append([]string{}, command[1:]...), "-no-shell-escape", "-interaction=nonstopmode",
			"-halt-on-error", "main.tex")
		cmd := exec.CommandContext(ctx, command[0], args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir,
			"openin_any=p", "openout_any=p", "shell_escape=f", "TEXMFVAR=" + filepath.Join(dir, ".texmf-var")}
		cmd.Env = append(cmd.Env, c.Env...)
		cmd.Stdin = nil
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		runErr := cmd.Run()
		log, _ := os.ReadFile(filepath.Join(dir, "main.log"))
		if ctx.Err() != nil {
			return nil, &Failure{fmt.Sprintf("the build took over %s and was stopped", timeout)}
		}
		if runErr != nil {
			text := string(log)
			if text == "" {
				text = out.String()
			}
			return nil, &Failure{Excerpt(text)}
		}
		if !rerun(string(log)) {
			break
		}
	}
	f, err := os.Open(filepath.Join(dir, "main.pdf"))
	if err != nil {
		return nil, &Failure{"TeX made no PDF"}
	}
	defer f.Close()
	pdf, err := io.ReadAll(io.LimitReader(f, MaxPDF+1))
	if err != nil {
		return nil, err
	}
	if len(pdf) > MaxPDF {
		return nil, &Failure{fmt.Sprintf("the PDF is over %d bytes", MaxPDF)}
	}
	return pdf, nil
}

// Excerpt is what of a log tells why a build failed: each error («! …») with the lines after it up to the source
// line it names (l.<n>), else the log's end; at most MaxLog characters.
func Excerpt(log string) string {
	lines := strings.Split(strings.ReplaceAll(log, "\r\n", "\n"), "\n")
	var picked []string
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "!") {
			continue
		}
		end := min(i+8, len(lines))
		for j := i + 1; j < end; j++ {
			if strings.HasPrefix(lines[j], "l.") {
				end = j + 2
				break
			}
		}
		picked = append(picked, lines[i:min(end, len(lines))]...)
		picked = append(picked, "")
		i = end - 1
	}
	if len(picked) == 0 {
		picked = lines[max(0, len(lines)-30):]
	}
	text := strings.TrimSpace(strings.Join(picked, "\n"))
	if r := []rune(text); len(r) > MaxLog {
		text = string(r[:MaxLog]) + "\n…"
	}
	return text
}

// Handler serves POST /build (the source in the body: 200 and the PDF, 422 and the log's errors, 503 while
// another build runs) and GET /health.
func Handler(c Compiler) http.Handler {
	busy := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("POST /build", func(w http.ResponseWriter, r *http.Request) {
		select {
		case busy <- struct{}{}:
			defer func() { <-busy }()
		default:
			http.Error(w, "another build is running", http.StatusServiceUnavailable)
			return
		}
		source, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxSource))
		if err != nil {
			http.Error(w, fmt.Sprintf("the source is over %d bytes", MaxSource), http.StatusRequestEntityTooLarge)
			return
		}
		pdf, err := c.Build(r.Context(), source)
		var failed *Failure
		switch {
		case errors.As(err, &failed):
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, failed.Log)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(pdf)
		}
	})
	return mux
}

// Client is the service's way to the builder.
type Client struct {
	URL  string // TEX_URL, no trailing slash
	HTTP *http.Client
}

// NewClient is a client of the builder at url; nil for an empty url (no builder).
func NewClient(url string) *Client {
	if url == "" {
		return nil
	}
	return &Client{URL: strings.TrimRight(url, "/"), HTTP: &http.Client{Timeout: Timeout + 10*time.Second}}
}

// Build is the source's PDF from the builder; a *Failure with the log's errors when TeX refuses it.
func (c *Client) Build(ctx context.Context, source []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL+"/build", bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-tex")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the TeX builder is not reachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxPDF+1))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if len(body) > MaxPDF {
			return nil, &Failure{fmt.Sprintf("the PDF is over %d bytes", MaxPDF)}
		}
		return body, nil
	case http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge:
		return nil, &Failure{string(body)}
	case http.StatusServiceUnavailable:
		return nil, errors.New("the TeX builder is busy with another build: try again in a minute")
	}
	return nil, fmt.Errorf("the TeX builder answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
