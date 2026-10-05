// Command rhl serves rhl.brookshear.party: the RHL's standings, results,
// upcoming games, scoring leaders, goalies and team pages, from GameSheet.
// Public: no login, no database, no secrets.
//
// It listens on a Unix socket (RHL_SOCKET), which cloudflared on the host
// connects to, so the container publishes no port. RHL_ADDR (TCP, default
// 127.0.0.1:8082) is for running it outside Docker.
//
//	HOCKEY_TZ    the league's time zone (default America/Los_Angeles)
//	RHL_LEAGUE   the GameSheet league ID (default: the RHL's)
//	RHL_SEASON   a GameSheet season ID to show; default: the league's current one
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"hockey/internal/gamesheet"
	"hockey/internal/rhl"
	"hockey/internal/serve"
)

const (
	// Sent with every request, so the sites' operators can see what it is
	// and where it lives.
	userAgent = "hockey-rhl/1 (+https://rhl.brookshear.party)"
	rhlLeague = 620287 // "RHL - Adult Hockey League" on GameSheet
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("rhl failed", "err", err.Error())
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
	season, err := envInt("RHL_SEASON", 0)
	if err != nil {
		return err
	}
	league, err := envInt("RHL_LEAGUE", rhlLeague)
	if err != nil {
		return err
	}
	// Every site it reads answers directly; a redirect means something
	// changed, and for logos could point anywhere.
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("unexpected redirect") },
	}
	srv, err := rhl.New(rhl.Config{
		Source:   &gamesheet.Client{HTTP: client, BaseURL: gamesheet.DefaultBaseURL, UserAgent: userAgent},
		Season:   season,
		League:   league,
		Logo:     rhl.FetchLogo(client, userAgent),
		Location: loc,
		Log:      log,
	})
	if err != nil {
		return err
	}
	ln, where, err := serve.Listen(os.Getenv("RHL_SOCKET"), serve.EnvOr("RHL_ADDR", "127.0.0.1:8082"))
	if err != nil {
		return err
	}
	return serve.Run(ctx, ln, where, srv.Handler(), log)
}

func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s should be a positive number, got %q", name, v)
	}
	return n, nil
}
