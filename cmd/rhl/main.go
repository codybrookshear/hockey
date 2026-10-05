// Command rhl serves rhl.brookshear.party: the RHL's standings, results,
// upcoming games, goal leaders and team pages, from GameSheet. Public: no
// login, no database, no secrets.
//
// It listens on a Unix socket (RHL_SOCKET), which cloudflared on the host
// connects to, so the container publishes no port. RHL_ADDR (TCP, default
// 127.0.0.1:8082) is for running it outside Docker.
//
//	HOCKEY_TZ         the league's time zone (default America/Los_Angeles)
//	RHL_SEASON        a GameSheet season ID to show; default: the current one,
//	                  from the seasons the rink's standings page links to
//	RHL_SEASONS_PAGE  that page (default: the rink's)
//	RHL_LEAGUE        the GameSheet league ID (default: the RHL's)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
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
	userAgent    = "hockey-rhl/1 (+https://rhl.brookshear.party)"
	seasonsPage  = "https://www.therinkexchange.com/standings--stats.html"
	rhlLeague    = 620287 // "RHL - Adult Hockey League" on GameSheet
	maxPageBytes = 2 << 20
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
	page := serve.EnvOr("RHL_SEASONS_PAGE", seasonsPage)
	if u, err := url.Parse(page); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("RHL_SEASONS_PAGE should be an https URL, got %q", page)
	}

	// Every site it reads answers directly; a redirect means something
	// changed, and for logos could point anywhere.
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("unexpected redirect") },
	}
	srv, err := rhl.New(rhl.Config{
		Source:      &gamesheet.Client{HTTP: client, BaseURL: gamesheet.DefaultBaseURL, UserAgent: userAgent},
		SeasonsPage: func(ctx context.Context) ([]byte, error) { return fetchPage(ctx, client, page) },
		Season:      season,
		League:      league,
		Logo:        rhl.FetchLogo(client, userAgent),
		Location:    loc,
		Log:         log,
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

// fetchPage gets an HTML page (the rink's standings page).
func fetchPage(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt != "text/html" {
		return nil, fmt.Errorf("unexpected content type %q", mt)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxPageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPageBytes {
		return nil, errors.New("page is unexpectedly large")
	}
	return b, nil
}
