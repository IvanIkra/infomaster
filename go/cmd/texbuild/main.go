// Command texbuild is the TeX builder of the knowledge base (D84): it serves internal/texbuild's Handler on the
// builder's own internal network. TEX_COMMAND replaces the compiler (space-separated), for tests.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/texbuild"
)

func main() {
	listen := flag.String("listen", ":8090", "address to serve on")
	flag.Parse()
	c := texbuild.Compiler{WorkDir: os.Getenv("TMPDIR")}
	if command := strings.Fields(os.Getenv("TEX_COMMAND")); len(command) > 0 {
		c.Command = command
	}
	server := &http.Server{Addr: *listen, Handler: texbuild.Handler(c), ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout: texbuild.Timeout + 10*time.Second}
	slog.Info("texbuild: serving", "addr", *listen)
	if err := server.ListenAndServe(); err != nil {
		slog.Error("texbuild", "error", err)
		os.Exit(1)
	}
}
