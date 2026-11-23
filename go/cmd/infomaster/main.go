// Command infomaster runs the service: infomaster [--data data]; logs the twin in once from a terminal: infomaster
// login; copies the own and hidden deadlines and the manual dictionary entries from Tablo once: infomaster
// import-tablo --tablo-db <tablo.db> (internal/tablo). The port of infomaster/__main__.py.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	_ "time/tzdata" // Moscow's zone in any image

	"github.com/IvanIkra/infomaster/go/internal/app"
	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/telegram"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/store"
	"github.com/IvanIkra/infomaster/go/internal/tablo"
	"github.com/IvanIkra/infomaster/go/internal/web"
)

// StaticDir is the web client's directory relative to the working directory: the repository's, or the image's.
const StaticDir = "web/static"

var commands = []string{"run", "login", "import-tablo"}

type arguments struct {
	command  string
	data     string
	logLevel string
	tabloDB  string
	static   string
	shadow   bool
	qr       bool
}

func main() {
	telegram.LoginHint = "docker compose run --rm infomaster infomaster login"
	args, err := parse(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "infomaster: %v\n", err)
		os.Exit(2)
	}
	if err := run(args); err != nil {
		fmt.Fprintf(os.Stderr, "infomaster: %v\n", err)
		os.Exit(1)
	}
}

// parse reads the command and the flags in any order, as argparse does: `infomaster login --data data` and
// `infomaster --data data login` are the same.
func parse(argv []string, output io.Writer) (arguments, error) {
	args := arguments{command: "run"}
	fs := flag.NewFlagSet("infomaster", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&args.data, "data", "data", "directory for the database and sessions")
	fs.StringVar(&args.logLevel, "log-level", "INFO", "DEBUG, INFO, WARNING or ERROR")
	fs.StringVar(&args.tabloDB, "tablo-db", "", "import-tablo: a copy of Tablo's tablo.db")
	fs.BoolVar(&args.shadow, "shadow", false,
		"serve both ports only: no Telegram, no loops, no push, no LLM (a copy of the database beside the live one)")
	fs.BoolVar(&args.qr, "qr", false, "login: by a QR code the twin's app scans, with no login code")
	fs.StringVar(&args.static, "static", "", "the web client's directory (WEB_STATIC; default "+StaticDir+")")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: infomaster [%s] [flags]\n", strings.Join(commands, "|"))
		fs.PrintDefaults()
	}
	given := false
	for {
		if err := fs.Parse(argv); err != nil {
			return args, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return args, nil
		}
		if given {
			return args, fmt.Errorf("unrecognized arguments: %s", strings.Join(rest, " "))
		}
		if !contains(commands, rest[0]) {
			return args, fmt.Errorf("argument command: invalid choice: '%s' (choose from %s)", rest[0],
				strings.Join(commands, ", "))
		}
		args.command, given, argv = rest[0], true, rest[1:]
	}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func run(args arguments) error {
	if args.command == "import-tablo" {
		return importTablo(args)
	}
	if err := config.LoadEnv(".env"); err != nil {
		return err
	}
	level, err := logLevel(args.logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	env, err := config.FromEnviron(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if args.command == "login" {
		if err := dataDir(args.data); err != nil {
			return err
		}
		if args.qr {
			return telegram.LoginQR(ctx, env, telegram.SessionPath(args.data), os.Stdin, os.Stdout)
		}
		return telegram.Login(ctx, env, telegram.SessionPath(args.data), os.Stdin, os.Stdout, telegram.DialGotd)
	}
	if args.shadow { // the model is unavailable: nothing is read or answered on the copy's behalf
		os.Unsetenv("ANTHROPIC_API_KEY")
		os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	}
	service, err := app.New(env, args.data, app.Options{Static: staticDir(args.static), Shadow: args.shadow})
	if err != nil {
		return err
	}
	return service.Run(ctx)
}

// logLevel is a level by Python's logging names.
func logLevel(name string) (slog.Level, error) {
	switch strings.ToUpper(name) {
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARNING", "WARN":
		return slog.LevelWarn, nil
	case "ERROR", "CRITICAL":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown level: '%s'", name)
}

// staticDir is the web client's directory: the flag, else WEB_STATIC, else web/static under the
// working directory, else the repository's (looked for upwards).
func staticDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if dir := strings.TrimSpace(os.Getenv("WEB_STATIC")); dir != "" {
		return dir
	}
	if info, err := os.Stat(filepath.Join(StaticDir, "index.html")); err == nil && !info.IsDir() {
		return StaticDir
	}
	return web.FindStatic()
}

// dataDir makes the data directory for the owner only: sessions and tokens live there (§10).
func dataDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// importTablo needs no secrets: only the two databases. Run it with the service stopped.
func importTablo(args arguments) error {
	if args.tabloDB == "" {
		return errors.New("import-tablo needs --tablo-db <tablo.db>")
	}
	source, err := tablo.Open(args.tabloDB)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := dataDir(args.data); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(args.data, "infomaster.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	counts, err := tablo.Import(st, source, clock.LocalDay(clock.NowUTC()))
	if err != nil {
		return err
	}
	fmt.Printf("own deadlines: %d added, %d skipped\nhidden deadlines: %d added, %d skipped\n"+
		"dictionary entries set by hand: %d added, %d skipped\n", counts.Own, counts.OwnSkipped, counts.Hidden,
		counts.HiddenSkipped, counts.Subjects, counts.SubjectsSkipped)
	return nil
}
