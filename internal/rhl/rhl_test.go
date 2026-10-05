package rhl

import (
	"context"
	"errors"
	"fmt"
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
		"/api/season-info/15331":       "season-info-15331.json",
		"/api/leagues/620287/seasons":  "league-seasons-620287.json",
		"/api/standings/15331":         "standings-15331.json",
		"/api/unified-games/15331":     "unified-games-15331.json",
		"/api/goalies/standings/15331": "goalies-15331.json",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := files[r.URL.Path]
		if r.URL.Path == "/api/players/standings/15331" {
			name, ok = "players-15331-"+r.URL.Query().Get("offset")+".json", true
		}
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

func (c *countingSource) LeagueSeasons(ctx context.Context, id int) ([]gamesheet.Season, error) {
	if err := c.note("league seasons"); err != nil {
		return nil, err
	}
	return c.Source.LeagueSeasons(ctx, id)
}

func (c *countingSource) Skaters(ctx context.Context, id int) ([]gamesheet.Skater, error) {
	if err := c.note("skaters"); err != nil {
		return nil, err
	}
	return c.Source.Skaters(ctx, id)
}

func (c *countingSource) Goalies(ctx context.Context, id int) ([]gamesheet.Goalie, error) {
	if err := c.note("goalies"); err != nil {
		return nil, err
	}
	return c.Source.Goalies(ctx, id)
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
	h := newServer(t, Config{Source: src, League: 620287})

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
		"<h2>Results</h2>", "<h2>Upcoming</h2>", "<h2>Live</h2>",
		"<h2>Scoring leaders</h2>", "<h2>Goalies</h2>",
		"Earlier results", "Later games",
		`<td class="who">Derek Dusome <a class="abbr" href="/team/554805">PIR</a></td>`, // from "DEREK DUSOME"
		`<td class="pts">17</td>`,
		`<td class="pts">.935</td>`,    // the best save percentage first
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

	// The season came from the league's list; everything is fetched once.
	get(t, h, "/")
	get(t, h, "/team/554802")
	for what, want := range map[string]int{"league seasons": 1, "season": 0, "standings": 1, "games": 1, "skaters": 1, "goalies": 1} {
		if got := src.count(what); got != want {
			t.Errorf("%s fetched %d times, want %d", what, got, want)
		}
	}
}

func TestConfiguredSeason(t *testing.T) {
	src := savedGameSheet(t)
	h := newServer(t, Config{Source: src, Season: 15331})
	if w := get(t, h, "/"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Fall 2026") {
		t.Errorf("GET /: %d", w.Code)
	}
	if src.count("season") != 1 || src.count("league seasons") != 0 {
		t.Errorf("calls: %v (want the configured season only)", src.calls)
	}
	if _, err := New(Config{Source: src, Location: la, Log: quiet}); err == nil {
		t.Error("New accepted neither Season nor League")
	}
}

func TestTeamPage(t *testing.T) {
	h := newServer(t, Config{Source: savedGameSheet(t), League: 620287})
	w := get(t, h, "/team/554812") // Orange
	if w.Code != http.StatusOK {
		t.Fatalf("GET /team/554812: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<title>Orange · RHL</title>", "<h1>Orange</h1>", "6th in the A/B Division",
		"<dd>1-3-0</dd>", "<h2>Results</h2>", "<h2>Upcoming</h2>", "<h2>Players</h2>", "<h2>Goalies</h2>",
		`<td class="who">Travis Johannes</td>`, // no team label: it's this team
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
	src.fail = map[string]bool{"league seasons": true}
	h := newServer(t, Config{Source: src, League: 620287})
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
	// Player stats down too: the rest still shows.
	src.fail["skaters"] = true
	h = newServer(t, Config{Source: src, Season: 15331})
	if b := get(t, h, "/").Body.String(); !strings.Contains(b, "get the player stats") || strings.Contains(b, "Scoring leaders") {
		t.Error("skaters down: no notice, or a leaders table anyway")
	}
	// Standings and games down: an error page.
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
		{"STEPHEN", "MCMACKIN"}:     "Stephen McMackin",
		{"mac", "mcd"}:              "Mac McD",
		{"EMMA", "MC"}:              "Emma Mc",
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

func TestScorersRosterGoalies(t *testing.T) {
	line := func(gp, g, a int) gamesheet.SkaterLine { return gamesheet.SkaterLine{GP: gp, G: g, A: a, PTS: g + a} }
	team := func(id int, abbr string, l gamesheet.SkaterLine) gamesheet.SkaterTeam {
		return gamesheet.SkaterTeam{Team: gamesheet.Team{ID: id, Abbr: abbr}, SkaterLine: l}
	}
	var all []gamesheet.Skater
	add := func(last string, total gamesheet.SkaterLine, teams ...gamesheet.SkaterTeam) {
		all = append(all, gamesheet.Skater{Player: gamesheet.Player{ID: len(all) + 1, First: "A", Last: last}, SkaterLine: total, Teams: teams})
	}
	add("SUB", line(4, 3, 3), team(1, "ONE", line(2, 1, 1)), team(2, "TWO", line(2, 2, 2)))
	add("STAR", line(3, 5, 1), team(1, "ONE", line(3, 5, 1)))
	add("OLD", line(0, 0, 0), team(9, "OLD", line(0, 0, 0)), team(1, "ONE", line(0, 0, 0)))
	for i := range 14 {
		add(fmt.Sprint("DEPTH", i), line(1, 1, 0), team(2, "TWO", line(1, 1, 0)))
	}

	got := scorers(all)
	// STAR and SUB tie on points; STAR has more goals. Then 14 tied at 1:
	// cut at scorersMax.
	if got[0].Name != "A Star" || got[1].Name != "A Sub" || got[1].Rank != 1 || got[1].Team != "ONE/TWO" ||
		got[2].Rank != 3 || len(got) != scorersMax {
		t.Errorf("scorers: %+v", got[:3])
	}
	one := roster(all, 1)
	// SUB's stats for this team only; OLD (no games) left out.
	if len(one) != 2 || one[0].Name != "A Star" || one[1].PTS != 2 || one[1].Team != "" {
		t.Errorf("roster: %+v", one)
	}

	gl := []gamesheet.Goalie{
		{Player: gamesheet.Player{First: "a", Last: "busy"}, GoalieLine: gamesheet.GoalieLine{GP: 4, SVPct: .9},
			Teams: []gamesheet.GoalieTeam{{Team: gamesheet.Team{ID: 1, Abbr: "ONE"}, GoalieLine: gamesheet.GoalieLine{GP: 2, SVPct: .95}},
				{Team: gamesheet.Team{ID: 2, Abbr: "TWO"}, GoalieLine: gamesheet.GoalieLine{GP: 2, SVPct: .85}}}},
		{Player: gamesheet.Player{First: "b", Last: "best"}, GoalieLine: gamesheet.GoalieLine{GP: 1, SVPct: .93},
			Teams: []gamesheet.GoalieTeam{{Team: gamesheet.Team{ID: 2, Abbr: "TWO"}, GoalieLine: gamesheet.GoalieLine{GP: 1, SVPct: .93}}}},
		{Player: gamesheet.Player{First: "c", Last: "bench"},
			Teams: []gamesheet.GoalieTeam{{Team: gamesheet.Team{ID: 1, Abbr: "ONE"}}}},
	}
	if g := goalies(gl, 0); len(g) != 2 || g[0].Name != "B Best" || g[1].SVPct != .9 || g[1].Team != "ONE" {
		t.Errorf("league goalies (season totals, once each, no benchwarmers): %+v", g)
	}
	if g := goalies(gl, 1); len(g) != 1 || g[0].SVPct != .95 || g[0].Team != "" {
		t.Errorf("team goalies (that team's line, no team label): %+v", g)
	}
}
