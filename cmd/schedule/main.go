// Command schedule serves hockey.brookshear.party: The Rink Exchange's daily
// schedule, one table per sheet. Public: no login, no database, no secrets.
//
// It listens on a Unix socket (SCHEDULE_SOCKET), which cloudflared on the host
// connects to, so the container publishes no port. SCHEDULE_ADDR (TCP,
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
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hockey/internal/frontline"
	"hockey/internal/schedule"
	"hockey/internal/serve"
)

// Sent with every request to the rink's site, so its operators can see what
// it is and where it lives.
const userAgent = "hockey-schedule/1 (+https://hockey.brookshear.party)"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("schedule failed", "err", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loc, err := time.LoadLocation(serve.EnvOr("HOCKEY_TZ", "America/Los_Angeles"))
	if err != nil {
		return fmt.Errorf("HOCKEY_TZ: %w", err)
	}
	schedURL := serve.EnvOr("HOCKEY_SCHEDULE_URL", frontline.DefaultURL)
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
	srv, err := schedule.New(schedule.Config{Source: client, SourceURL: schedURL, Location: loc, Log: log})
	if err != nil {
		return err
	}
	ln, where, err := serve.Listen(os.Getenv("SCHEDULE_SOCKET"), serve.EnvOr("SCHEDULE_ADDR", "127.0.0.1:8081"))
	if err != nil {
		return err
	}
	return serve.Run(ctx, ln, where, srv.Handler(), log)
}
