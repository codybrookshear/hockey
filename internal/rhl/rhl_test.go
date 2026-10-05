package rhl

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"hockey/internal/gamesheet"
)

var la = func() *time.Location {
	l, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return l
}()

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// The saved responses (gamesheet/testdata, from 2026-10-04 evening), served
// at GameSheet's paths to a real client.
func savedGameSheet(t *testing.T) *countingSource {
	t.Helper()
	files := map[string]string{
		"/api/season-info/15331":   "season-info-15331.json",
		"/api/season-info/10562":   "season-info-10562.json",
		"/api/season-info/10561":   "season-info-10561.json",
		"/api/standings/15331":     "standings-15331.json",
		"/api/unified-games/15331": "unified-games-15331.json",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile("../gamesheet/testdata/" + name)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return &countingSource{Source: &gamesheet.Client{HTTP: srv.Client(), BaseURL: srv.URL}, calls: map[string]int{}}
}

// countingSource counts calls, and can be made to fail.
type countingSource struct {
	Source
	mu    sync.Mutex
	calls map[string]int
	fail  map[string]bool
}

func (c *countingSource) note(what string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[what]++
	if c.fail[what] {
		return errors.New(what + " is down")
	}
	return nil
}

func (c *countingSource) count(what string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[what]
}

func (c *countingSource) Season(ctx context.Context, id int) (gamesheet.Season, error) {
	if err := c.note("season"); err != nil {
		return gamesheet.Season{}, err
	}
	return c.Source.Season(ctx, id)
}

func (c *countingSource) Standings(ctx context.Context, id int) ([]gamesheet.Division, error) {
	if err := c.note("standings"); err != nil {
		return nil, err
	}
	return c.Source.Standings(ctx, id)
}

func (c *countingSource) Games(ctx context.Context, id int) ([]gamesheet.Game, error) {
	if err := c.note("games"); err != nil {
		return nil, err
	}
	return c.Source.Games(ctx, id)
}

func seasonsPage(t *testing.T) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) {
		return os.ReadFile("../gamesheet/testdata/rink-standings-page.html")
	}
}

type logoLog struct {
	mu   sync.Mutex
	urls []string
}

func (l *logoLog) fetch(_ context.Context, u string) (Logo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.urls = append(l.urls, u)
	return Logo{Type: "image/png", Data: []byte("\x89PNG fake")}, nil
}

func newServer(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	if cfg.League == 0 {
		cfg.League = 620287
	}
	if cfg.Logo == nil {
		cfg.Logo = (&logoLog{}).fetch
	}
	cfg.Location, cfg.Log = la, quiet
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Date(2026, 10, 4, 21, 0, 0, 0, la) }
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestHome(t *testing.T) {
	src := savedGameSheet(t)
	h := newServer(t, Config{Source: src, SeasonsPage: seasonsPage(t)})

	w := get(t, h, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /: %d\n%s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<title>RHL · Fall 2026</title>",
		"<h2>A/B Division</h2>", "<h2>B/C Division</h2>",
		`<a href="/team/554802">Ice Dogs</a>`,
		`<img class="logo" src="/logo/554802"`,
		"<h2>Results</h2>", "<h2>Upcoming</h2>", "<h2>Goal leaders</h2>", "<h2>Live</h2>",
		"Earlier results", "Later games",
		"Derek Dusome",                 // from "DEREK DUSOME"
		`<span class="tbd">TBD</span>`, // playoff slots
		`<span class="kind">Playoff</span>`,
		"Final · Tie",
		"https://gamesheetstats.com/seasons/15331",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	if strings.Index(body, "A/B Division") > strings.Index(body, "B/C Division") {
		t.Error("divisions out of order")
	}
	// Ranked: Renegades (3rd) before Pirates (4th), though the response lists them the other way round.
	if strings.Index(body, ">Renegades</a></td>") > strings.Index(body, ">Pirates</a></td>") {
		t.Error("standings not in rank order")
	}
	if strings.Contains(body, "<script") {
		t.Error("page has a script")
	}
	if got := w.Header().Get("Content-Security-Policy"); got != csp {
		t.Errorf("CSP %q", got)
	}

	// The season came from the rink's page: three seasons looked up, once.
	get(t, h, "/")
	get(t, h, "/team/554802")
	if src.count("season") != 3 || src.count("standings") != 1 || src.count("games") != 1 {
		t.Errorf("calls: %v (want 3 season, 1 standings, 1 games: the rest cached)", src.calls)
	}
}

func TestConfiguredSeason(t *testing.T) {
	src := savedGameSheet(t)
	h := newServer(t, Config{Source: src, Season: 15331})
	if w := get(t, h, "/"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Fall 2026") {
		t.Errorf("GET /: %d", w.Code)
	}
	if src.count("season") != 1 {
		t.Errorf("season looked up %d times, want 1", src.count("season"))
	}
	if _, err := New(Config{Source: src, Location: la, Log: quiet}); err == nil {
		t.Error("New accepted neither Season nor SeasonsPage")
	}
}

func TestTeamPage(t *testing.T) {
	h := newServer(t, Config{Source: savedGameSheet(t), SeasonsPage: seasonsPage(t)})
	w := get(t, h, "/team/554812") // Orange
	if w.Code != http.StatusOK {
		t.Fatalf("GET /team/554812: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<title>Orange · RHL</title>", "<h1>Orange</h1>", "6th in the A/B Division",
		"<dd>1-3-0</dd>", "<h2>Results</h2>", "<h2>Upcoming</h2>", "<h2>Goal scorers</h2>",
		"Travis Johannes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("team page lacks %q", want)
		}
	}
	if n := strings.Count(body, "<span>Final"); n != 4 {
		t.Errorf("%d results, want 4", n)
	}
	for _, p := range []string{"/team/999", "/team/abc", "/team/0", "/team/-1"} {
		if w := get(t, h, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
}

func TestLogos(t *testing.T) {
	logos := &logoLog{}
	h := newServer(t, Config{Source: savedGameSheet(t), Season: 15331, Logo: logos.fetch})

	// Unknown until GameSheet's data names it.
	if w := get(t, h, "/logo/554802"); w.Code != http.StatusNotFound {
		t.Errorf("logo before any data: %d", w.Code)
	}
	get(t, h, "/")
	w := get(t, h, "/logo/554802")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" ||
		w.Header().Get("Cache-Control") != "public, max-age=86400" || w.Body.String() != "\x89PNG fake" {
		t.Errorf("logo: %d %v", w.Code, w.Header())
	}
	get(t, h, "/logo/554802")
	if len(logos.urls) != 1 || !strings.HasPrefix(logos.urls[0], "https://imagedelivery.net/") || !strings.HasSuffix(logos.urls[0], "/256") {
		t.Errorf("fetched %v (want one /256 URL, then cached)", logos.urls)
	}
	for _, p := range []string{"/logo/1", "/logo/x", "/logo/"} {
		if w := get(t, h, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
}

func TestLogoURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://imagedelivery.net/acct/img/256": "https://imagedelivery.net/acct/img/256",
		"https://imagedelivery.net/acct/img":     "https://imagedelivery.net/acct/img/256",
		"http://imagedelivery.net/acct/img/256":  "",
		"https://evil.example/acct/img/256":      "",
		"https://imagedelivery.net.evil/a/b/c":   "",
		"https://u:p@imagedelivery.net/a/b/c":    "",
		"https://imagedelivery.net/a/b/c?x=1":    "",
		"https://imagedelivery.net/a":            "",
		"https://imagedelivery.net/a/b/c/d":      "",
		"":                                       "",
	} {
		got, ok := logoURL(in)
		if (want == "") == ok || got != want {
			t.Errorf("logoURL(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestGameSheetDown(t *testing.T) {
	src := savedGameSheet(t)
	src.fail = map[string]bool{"season": true}
	h := newServer(t, Config{Source: src, SeasonsPage: seasonsPage(t)})
	w := get(t, h, "/")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "find the current season") {
		t.Errorf("no season: %d", w.Code)
	}

	// Standings down, games fine: show the games, say what's missing.
	src = savedGameSheet(t)
	src.fail = map[string]bool{"standings": true}
	h = newServer(t, Config{Source: src, Season: 15331})
	w = get(t, h, "/")
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "get the standings") ||
		!strings.Contains(body, "<h2>Results</h2>") || strings.Contains(body, "<h2>A/B Division</h2>") {
		t.Errorf("standings down: %d", w.Code)
	}
	// Both down: an error page.
	src.fail["games"] = true
	h = newServer(t, Config{Source: src, Season: 15331})
	if w := get(t, h, "/"); w.Code != http.StatusBadGateway {
		t.Errorf("both down: %d", w.Code)
	}
}

func TestOtherRoutes(t *testing.T) {
	h := newServer(t, Config{Source: savedGameSheet(t), Season: 15331})
	if w := get(t, h, "/robots.txt"); w.Body.String() != "User-agent: *\nDisallow: /\n" {
		t.Errorf("robots.txt: %q", w.Body.String())
	}
	if w := get(t, h, "/static/rhl.css"); w.Code != http.StatusOK {
		t.Errorf("css: %d", w.Code)
	}
	for _, p := range []string{"/nope", "/static/rhl.go", "/team"} {
		if w := get(t, h, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d", w.Code)
	}
}

func TestPlayerName(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"DEREK", "DUSOME"}:         "Derek Dusome",
		{"ryan", "o'neil-smith"}:    "Ryan O'Neil-Smith",
		{"Gallen", "Pierce-Lackey"}: "Gallen Pierce-Lackey",
		{"JJ", "McDonald"}:          "JJ McDonald",
		{" ANNA ", " LEE "}:         "Anna Lee",
	} {
		if got := playerName(gamesheet.Player{First: in[0], Last: in[1]}); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestLeaders(t *testing.T) {
	g := func(start int, typ string, home gamesheet.Side) gamesheet.Game {
		return gamesheet.Game{Start: time.Unix(int64(start), 0), Status: "final", Type: typ, Home: home}
	}
	scored := func(team int, title string, players ...int) gamesheet.Side {
		s := gamesheet.Side{Team: gamesheet.Team{ID: team, Title: title}}
		for _, p := range players {
			s.Scorers = append(s.Scorers, gamesheet.Goal{Player: gamesheet.Player{ID: p, First: "P", Last: string(rune('A' + p))}})
		}
		return s
	}
	games := []gamesheet.Game{
		g(1, "regular_season", scored(10, "Ten", 1, 1, 2)),
		g(2, "playoff", scored(20, "Twenty", 1, 3)),    // player 1 subs for Twenty
		g(3, "exhibition", scored(10, "Ten", 2, 2, 2)), // not counted
	}
	got := leaders(games, 0, 2)
	// P B: 3 goals, latest for Twenty. Then P C and P D tie at 1: both kept.
	if len(got) != 3 || got[0].Name != "P B" || got[0].Goals != 3 || got[0].Team != "Twenty" ||
		got[1].Rank != 2 || got[2].Rank != 2 {
		t.Errorf("leaders: %+v", got)
	}
	if got := leaders(games, 10, 0); len(got) != 2 || got[0].Goals != 2 {
		t.Errorf("team 10: %+v", got)
	}
}
