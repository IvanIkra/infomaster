package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// StopLoops is how long a stop waits for the background loops to end.
	StopLoops = 10 * time.Second
	// StopServers is how long a stop waits for the requests under way.
	StopServers = 5 * time.Second
)

// ErrLost is the end of Run when the bot or the twin lost Telegram for good: Docker restarts the service.
var ErrLost = errors.New("lost Telegram for good")

// Run serves both ports of the environment until SIGINT or SIGTERM, or until Telegram is lost for good.
func (s *Service) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	public, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", s.Env.PublicPort))
	if err != nil {
		s.Store.Close()
		return err
	}
	internal, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", s.Env.InternalPort))
	if err != nil {
		public.Close()
		s.Store.Close()
		return err
	}
	return s.Serve(ctx, public, internal)
}

// Serve is Run on given listeners: the bot logs in, both ports serve, the twin starts, the loops run; everything
// stops when ctx is done or Telegram is lost (ErrLost), and the database is closed last.
func (s *Service) Serve(ctx context.Context, public, internal net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := s.Bot.Start(ctx); err != nil {
		public.Close()
		internal.Close()
		s.Bot.Stop()
		s.Store.Close()
		return fmt.Errorf("the bot: %w", err)
	}
	servers := []*http.Server{
		{Handler: s.public, ReadHeaderTimeout: 30 * time.Second},
		{Handler: s.internal, ReadHeaderTimeout: 30 * time.Second},
	}
	for i, listener := range []net.Listener{public, internal} {
		go func(server *http.Server, listener net.Listener) {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("a port stopped", "addr", listener.Addr().String(), "err", err)
			}
		}(servers[i], listener)
	}
	var loops sync.WaitGroup
	err := s.serve(ctx, &loops, public, internal)
	cancel()
	s.stop(&loops, servers)
	return err
}

func (s *Service) serve(ctx context.Context, loops *sync.WaitGroup, public, internal net.Listener) error {
	slog.Info(fmt.Sprintf("public port %d (the web app), internal port %d (API)", port(public), port(internal)))
	if s.Env.CalendarSecret == "" {
		slog.Info("CALENDAR_SECRET is not set: the iCal feeds for Tablo are off")
	}
	if s.Env.CalendarPublicSecret == "" {
		slog.Info("CALENDAR_PUBLIC_SECRET is not set: no iCal feeds behind Funnel")
	}
	if _, err := s.Twin.Start(ctx); err != nil { // without a login the service runs on, and the twin says so
		return fmt.Errorf("the twin: %w", err)
	}
	for _, loop := range []func(context.Context){
		s.prune,
		s.Batches.Run,
		func(ctx context.Context) { logged("RUZ", s.Ruz.Run(ctx)) },
		func(ctx context.Context) { logged("HSE App X", s.HseApp.Run(ctx)) },
		func(ctx context.Context) { logged("mail", s.Mail.Run(ctx)) },
		func(ctx context.Context) { logged("the wiki", s.Wiki.Run(ctx)) },
		s.Watch.Run,
		s.Reminders.Run,
		s.GradeSheets.Run,
	} {
		loops.Add(1)
		go func(loop func(context.Context)) {
			defer loops.Done()
			loop(ctx)
		}(loop)
	}
	return Watch(ctx, s.Bot, s.Twin)
}

func logged(name string, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error(name+" stopped", "err", err)
	}
}

// stop ends the loops, the ports and the clients, and closes the database last.
func (s *Service) stop(loops *sync.WaitGroup, servers []*http.Server) {
	if !wait(loops.Wait, StopLoops) {
		slog.Warn("some loops did not stop in time")
	}
	stopping, cancel := context.WithTimeout(context.Background(), StopServers)
	for _, server := range servers {
		if err := server.Shutdown(stopping); err != nil {
			server.Close()
		}
	}
	cancel()
	s.Twin.Stop()
	s.Bot.Stop()
	wait(func() {
		s.Settle()
		s.Watch.Settle()
		s.Feed.Settle()
		s.Web.Notifier.Wait()
	}, StopLoops)
	if err := s.Store.Close(); err != nil {
		slog.Error("closing the database failed", "err", err)
	}
}

// wait runs fn and reports whether it returned within the time.
func wait(fn func(), limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

func port(listener net.Listener) int {
	if addr, ok := listener.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// Doner is a Telegram client that tells when it lost Telegram: the bot.
type Doner interface {
	Done() <-chan struct{}
}

// Switcher is one whose client changes on a login or a logout: the twin.
type Switcher interface {
	Doner
	Changed() <-chan struct{}
}

// Watch waits until ctx is done (nil), or until the bot or the twin loses Telegram for good (ErrLost). A login or
// a logout in «Machine» changes the twin's client: the new one is watched, and a logout is no loss (the twin tells
// of the change before its old client disconnects).
func Watch(ctx context.Context, bot Doner, twin Switcher) error {
	for {
		drain(twin.Changed())
		botDone, twinDone := bot.Done(), twin.Done() // nil ones never come
		select {
		case <-ctx.Done():
			return nil
		case <-twin.Changed():
			continue
		case <-botDone:
		case <-twinDone:
		}
		if ctx.Err() != nil { // a stop ends the clients too: that is no loss
			return nil
		}
		select {
		case <-twin.Changed(): // the twin's client changed: its old one's end is no loss
			if !closed(botDone) {
				continue
			}
		default:
		}
		lost := []string{}
		if closed(botDone) {
			lost = append(lost, "the bot")
		}
		if closed(twinDone) && twin.Done() == twinDone {
			lost = append(lost, "the twin")
		}
		if len(lost) == 0 {
			continue
		}
		slog.Error(strings.Join(lost, " and ") + " lost Telegram for good, exiting so Docker restarts the service")
		return fmt.Errorf("%s %w", strings.Join(lost, " and "), ErrLost)
	}
}

func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func closed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
