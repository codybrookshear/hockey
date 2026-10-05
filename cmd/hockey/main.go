// Command hockey serves hockey.brookshear.party: The Rink Exchange's daily
// schedule, one table per sheet. Public: no login, no database, no secrets.
//
// It listens on a Unix socket (HOCKEY_SOCKET), which cloudflared on the host
// connects to, so the container publishes no port. HOCKEY_ADDR (TCP,
// default 127.0.0.1:8081) is for running it outside Docker.
//
//	HOCKEY_TZ            the rink's time zone (default America/Los_Angeles)
//	HOCKEY_SCHEDULE_URL  the rink's daily schedule page (default: frontline.DefaultURL)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hockey/internal/frontline"
	"hockey/internal/site"
)

// Sent with every request to the rink's site, so its operators can see what
// it is and where it lives.
const userAgent = "hockey-schedule/1 (+https://hockey.brookshear.party)"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("hockey failed", "err", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tz := envOr("HOCKEY_TZ", "America/Los_Angeles")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("HOCKEY_TZ: %w", err)
	}
	schedURL := envOr("HOCKEY_SCHEDULE_URL", frontline.DefaultURL)
	if u, err := url.Parse(schedURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("HOCKEY_SCHEDULE_URL should be an https URL, got %q", schedURL)
	}

	client := &frontline.Client{
		HTTP: &http.Client{
			Timeout: 15 * time.Second,
			// The page answers directly; a redirect means something changed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("unexpected redirect") },
		},
		URL:       schedURL,
		Location:  loc,
		UserAgent: userAgent,
	}
	srv, err := site.New(site.Config{Source: client, SourceURL: schedURL, Location: loc, Log: log})
	if err != nil {
		return err
	}

	ln, where, err := listen()
	if err != nil {
		return err
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	log.Info("listening", "on", where)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// listen opens HOCKEY_SOCKET if set, else HOCKEY_ADDR.
func listen() (net.Listener, string, error) {
	sock := os.Getenv("HOCKEY_SOCKET")
	if sock == "" {
		addr := envOr("HOCKEY_ADDR", "127.0.0.1:8081")
		ln, err := net.Listen("tcp", addr)
		return ln, addr, err
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, "", err
	}
	// Who may connect is decided by the directory the socket lives in (on the
	// host): the socket itself is open to anyone who can reach it there.
	if err := os.Chmod(sock, 0o666); err != nil {
		ln.Close()
		return nil, "", err
	}
	return ln, "unix:" + sock, nil
}
