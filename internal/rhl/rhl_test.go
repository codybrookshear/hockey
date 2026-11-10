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
	"regexp"
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
func savedGameSheet(t *testing.T) *countingSource { return savedGameSheetWith(t, nil) }

// winterGameSheet is GameSheet on 2026-11-10, in the week between seasons:
// Fall 2026 (as saved on 10-04) not quite over, and Winter 2026/27 (16530)
// started, with standings, rosters and no games played.
func winterGameSheet(t *testing.T) *countingSource {
	return savedGameSheetWith(t, map[string]string{
		"/api/leagues/620287/seasons":    "league-seasons-620287-winter.json",
		"/api/standings/16530":           "standings-16530.json",
		"/api/unified-games/16530":       `{"data":[],"meta":{"total":0}}`,
		"/api/players/standings/16530":   `{"status":"success","data":[]}`,
		"/api/goalies/standings/16530":   `{"status":"success","data":[]}`,
		"/api/teams/16530/587702/roster": "roster-16530-587702.json",
		"/api/teams/16530/591206/roster": "", // not saved: unavailable
		"/api/teams/15331/554799/roster": "roster-15331-554799.json",
		"/api/unified-games/10561":       `{"data":[],"meta":{"total":0}}`,
		"/api/standings/10561":           `{"status":"success","data":[]}`,
		"/api/players/standings/10561":   `{"status":"success","data":[]}`,
		"/api/goalies/standings/10561":   `{"status":"success","data":[]}`,
	})
}

// savedGameSheetWith serves the saved responses, with more: a path's saved
// file, or a body ("{..."), or "" for a 404.
func savedGameSheetWith(t *testing.T, more map[string]string) *countingSource {
	t.Helper()
	files := map[string]string{
		"/api/season-info/15331":         "season-info-15331.json",
		"/api/leagues/620287/seasons":    "league-seasons-620287.json",
		"/api/standings/15331":           "standings-15331.json",
		"/api/unified-games/15331":       "unified-games-15331.json",
		"/api/goalies/standings/15331":   "goalies-15331.json",
		"/api/teams/15331/554799/roster": "roster-15331-554799.json",
	}
	for k, v := range more {
		files[k] = v
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := files[r.URL.Path]
		if r.URL.Path == "/api/players/standings/15331" {
			name, ok = "players-15331-"+r.URL.Query().Get("offset")+".json", true
		}
		if !ok || name == "" {
			http.NotFound(w, r)
			return
		}
		b := []byte(name)
		if !strings.HasPrefix(name, "{") {
			var err error
			if b, err = os.ReadFile("../gamesheet/testdata/" + name); err != nil {
				t.Error(err)
			}
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

func (c *countingSource) Roster(ctx context.Context, season, team int) (gamesheet.Roster, error) {
	if err := c.note("roster"); err != nil {
		return gamesheet.Roster{}, err
	}
	return c.Source.Roster(ctx, season, team)
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
		// The toggle, on the first division by default.
		`<a href="/?division=82591" aria-current="page">A/B Division</a>`,
		`<a href="/?division=82590">B/C Division</a>`,
		"<h2>Standings</h2>",
		`<a href="/team/554805">Pirates</a>`,
		`<img class="logo" src="/logo/554805"`,
		`<span class="logo initial" aria-hidden="true">R</span>`, // Rogue has no logo
		"<h2>Results</h2>", "<h2>Upcoming</h2>", "<h2>Live</h2>",
		"<h2>Scoring leaders</h2>", "<h2>Goalies</h2>",
		"Earlier results", "Later games",
		`<td class="who">Derek Dusome <a class="abbr" href="/team/554805">PIR</a></td>`, // from "DEREK DUSOME"
		`<td class="pts">17</td>`,
		`<td class="pts">.902</td>`,    // the division's best save percentage first
		`<span class="tbd">TBD</span>`, // playoff slots
		`<span class="kind">Playoff</span>`,
		"https://gamesheetstats.com/seasons/15331",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	// Nothing from the other division.
	for _, other := range []string{"Ice Dogs", "sky guardians", ".935"} {
		if strings.Contains(body, other) {
			t.Errorf("A/B page shows %q, from B/C", other)
		}
	}
	if c := w.Header().Get("Set-Cookie"); c != "" {
		t.Errorf("set a cookie without a choice: %q", c)
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
	// Standings twice: Spring's too, looking for a logo for Rogue.
	get(t, h, "/")
	get(t, h, "/team/554802")
	for what, want := range map[string]int{"league seasons": 1, "season": 0, "standings": 2, "games": 1, "skaters": 1, "goalies": 1, "roster": 1} {
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
	// The league's seasons too, for the menu.
	if src.count("season") != 1 || src.count("league seasons") != 1 {
		t.Errorf("calls: %v", src.calls)
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
		!strings.Contains(body, "<h2>Results</h2>") || strings.Contains(body, "<h2>Standings</h2>") ||
		!strings.Contains(body, `href="/?division=82590"`) { // divisions from the games
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
		{"ROBERT", "ARNOLD IV"}:     "Robert Arnold IV",
		{"IVAN", "IVES"}:            "Ivan Ives",
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

	everyone := func(int) bool { return true }
	got := scorers(all, everyone, links{})
	// STAR and SUB tie on points; STAR has more goals. Then 14 tied at 1:
	// cut at scorersMax.
	if got[0].Name != "A Star" || got[1].Name != "A Sub" || got[1].Rank != 1 || got[1].Team != "ONE/TWO" ||
		got[2].Rank != 3 || len(got) != scorersMax {
		t.Errorf("scorers: %+v", got[:3])
	}
	// A division of team 2 only: SUB counts their games for TWO.
	if two := scorers(all, func(id int) bool { return id == 2 }, links{season: 7}); two[0].TeamHref != "/team/2?season=7" || two[0].Name != "A Sub" || two[0].PTS != 4 || two[0].Team != "TWO" {
		t.Errorf("division scorers: %+v", two[0])
	}
	one := teamSkaters(nil, all, 1)
	// No roster: who's played. SUB's stats for this team only; OLD (no games) left out.
	if len(one) != 2 || one[0].Name != "A Star" || one[1].PTS != 2 || one[1].Team != "" {
		t.Errorf("players: %+v", one)
	}
	// The roster: OLD and a new player, yet to play, by name; SUB (not on
	// it) still there.
	newbie := gamesheet.Skater{Player: gamesheet.Player{ID: 99, First: "A", Last: "NEW"}, Teams: []gamesheet.SkaterTeam{team(1, "ONE", line(0, 0, 0))}}
	one = teamSkaters([]gamesheet.Skater{all[2], newbie, all[1]}, all, 1)
	var names []string
	for _, s := range one {
		names = append(names, s.Name)
	}
	if fmt.Sprint(names) != "[A Star A Sub A New A Old]" {
		t.Errorf("roster: %v", names)
	}

	gl := []gamesheet.Goalie{
		{Player: gamesheet.Player{ID: 1, First: "a", Last: "busy"}, GoalieLine: gamesheet.GoalieLine{GP: 4, SVPct: .9},
			Teams: []gamesheet.GoalieTeam{{Team: gamesheet.Team{ID: 1, Abbr: "ONE"}, GoalieLine: gamesheet.GoalieLine{GP: 2, SVPct: .95}},
				{Team: gamesheet.Team{ID: 2, Abbr: "TWO"}, GoalieLine: gamesheet.GoalieLine{GP: 2, SVPct: .85}}}},
		{Player: gamesheet.Player{ID: 2, First: "b", Last: "best"}, GoalieLine: gamesheet.GoalieLine{GP: 1, SVPct: .93},
			Teams: []gamesheet.GoalieTeam{{Team: gamesheet.Team{ID: 2, Abbr: "TWO"}, GoalieLine: gamesheet.GoalieLine{GP: 1, SVPct: .93}}}},
		{Player: gamesheet.Player{ID: 3, First: "c", Last: "bench"},
			Teams: []gamesheet.GoalieTeam{{Team: gamesheet.Team{ID: 1, Abbr: "ONE"}}}},
	}
	// A row per goalie and team they played for; none for the benchwarmer.
	if g := goalies(gl, everyone, &links{}); len(g) != 3 || g[0].SVPct != .95 || g[0].Team != "ONE" || g[1].Name != "B Best" || g[2].Team != "TWO" {
		t.Errorf("league goalies: %+v", g)
	}
	if g := goalies(gl, func(id int) bool { return id == 1 }, nil); len(g) != 1 || g[0].SVPct != .95 || g[0].Team != "" {
		t.Errorf("team goalies (that team's line, no team label): %+v", g)
	}
	// With a roster: the benchwarmer too, after those who've played.
	if g := teamGoalies(gl[2:], gl, 1); len(g) != 2 || g[0].Name != "A Busy" || g[1].Name != "C Bench" || g[1].GP != 0 {
		t.Errorf("team goalies with a roster: %+v", g)
	}
}

func TestDivisionToggle(t *testing.T) {
	src := savedGameSheet(t)
	h := newServer(t, Config{Source: src, Season: 15331})
	bc := func(body string) bool {
		return strings.Contains(body, `<a href="/?division=82590" aria-current="page">B/C Division</a>`) &&
			strings.Contains(body, `<a href="/team/554802">Ice Dogs</a>`) && !strings.Contains(body, "Pirates") &&
			strings.Contains(body, `<td class="pts">.935</td>`) && // Dorman, B/C's best
			strings.Contains(body, "Final · Tie") // Bestmed 2, Ice Dogs 2
	}

	// Picking B/C shows it and remembers it.
	w := get(t, h, "/?division=82590")
	if !bc(w.Body.String()) {
		t.Errorf("?division=82590 doesn't show B/C only:\n%s", w.Body)
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != "division" || c[0].Value != "82590" || !c[0].HttpOnly || c[0].Secure ||
		c[0].SameSite != http.SameSiteLaxMode || c[0].Path != "/" || c[0].MaxAge < 300*24*60*60 {
		t.Fatalf("cookie: %+v", c)
	}

	withCookie := func(target, value string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.AddCookie(&http.Cookie{Name: "division", Value: value})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// Remembered: plain / shows B/C, without setting the cookie again.
	if w := withCookie("/", "82590"); !bc(w.Body.String()) || w.Header().Get("Set-Cookie") != "" {
		t.Error("the remembered division isn't used")
	}
	// An unknown division is ignored, in the link or the cookie.
	if w := withCookie("/?division=999", "82590"); !bc(w.Body.String()) || w.Header().Get("Set-Cookie") != "" {
		t.Error("?division=999 overrode the cookie, or was remembered")
	}
	if w := withCookie("/", "garbage"); !strings.Contains(w.Body.String(), `aria-current="page">A/B Division`) {
		t.Error("a bad cookie didn't fall back to the first division")
	}
	// Team pages aren't filtered.
	if w := withCookie("/team/554805", "82590"); !strings.Contains(w.Body.String(), "<h1>Pirates</h1>") {
		t.Error("team page filtered by division")
	}

	// In production the cookie is Secure.
	h = newServer(t, Config{Source: savedGameSheet(t), Season: 15331, SecureCookies: true})
	if c := get(t, h, "/?division=82590").Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Errorf("production cookie: %+v", c)
	}
}

// The week between seasons: Winter has started, Fall hasn't finished.
var nov10 = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, la) }

func TestSeasonMenu(t *testing.T) {
	src := winterGameSheet(t)
	h := newServer(t, Config{Source: src, League: 620287, Now: nov10})

	// The current season: Winter, standings at 0-0 before anyone plays.
	w := get(t, h, "/?division=90095")
	b := w.Body.String()
	for _, want := range []string{
		"<title>RHL · Winter 2026/27</title>",
		`<summary><span class="vh">Season: </span>Winter 2026/27</summary>`,
		// Latest first; the current one at plain /.
		`<li><a href="/" aria-current="page">Winter 2026/27</a></li>`,
		`<li><a href="/?season=15331">Fall 2026</a></li>`,
		`<li><a href="/?season=10561">Spring 2026</a></li>`,
		"<h2>Standings</h2>",
		`<td class="rank">–</td>`, // no rank yet
		`<a href="/team/591206">Renegades</a>`,
		`<a href="/?division=90096">Apollo</a>`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("winter lacks %q", want)
		}
	}
	if strings.Index(b, "Winter 2026/27</a>") > strings.Index(b, "Fall 2026</a>") || strings.Contains(b, `class="past"`) {
		t.Error("menu out of order, or the current season marked as earlier")
	}
	if strings.Contains(b, "class=\"problem\"") {
		t.Errorf("problems on a season with no games:\n%s", b)
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].Value != "90095" {
		t.Errorf("cookie: %+v", c)
	}

	// Fall, picked from the menu: every link keeps it; the division isn't
	// remembered (next season's are different).
	w = get(t, h, "/?season=15331&division=82590")
	b = w.Body.String()
	for _, want := range []string{
		"<title>RHL · Fall 2026</title>",
		`<li><a href="/?season=15331" aria-current="page">Fall 2026</a></li>`,
		`<p class="past">An earlier season. <a href="/">Back to Winter 2026/27 ›</a></p>`,
		`<a href="/?division=82590&amp;season=15331" aria-current="page">B/C Division</a>`,
		`<a href="/team/554802?season=15331">Ice Dogs</a>`,
		"https://gamesheetstats.com/seasons/15331",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("fall lacks %q", want)
		}
	}
	if w.Code != http.StatusOK || w.Header().Get("Set-Cookie") != "" {
		t.Errorf("fall: %d, cookie %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	// Plain links keep to the current season.
	if b := get(t, h, "/team/554802").Body.String(); !strings.Contains(b, "no such team this season") {
		t.Error("a fall team on the current season's page")
	}
	// Only the league's seasons.
	for _, p := range []string{"/?season=99", "/team/554802?season=99"} {
		if w := get(t, h, p); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no such RHL season") {
			t.Errorf("GET %s: %d", p, w.Code)
		}
	}
	// Winter's and Fall's (and Winter's again, for Fall's Rogue's logo); nothing for season 99.
	if src.count("standings") != 3 || src.count("league seasons") != 1 {
		t.Errorf("calls: %v", src.calls)
	}
	// Junk is the current season.
	if b := get(t, h, "/?season=abc").Body.String(); !strings.Contains(b, "<title>RHL · Winter 2026/27</title>") {
		t.Error("?season=abc")
	}
}

func TestRosters(t *testing.T) {
	src := winterGameSheet(t)
	h := newServer(t, Config{Source: src, League: 620287, Now: nov10})

	// A new season's team: its roster, before anyone has played; ranked by hand.
	b := get(t, h, "/team/587702").Body.String()
	for _, want := range []string{
		"<h1>White Lightning</h1>", "1st in the Apollo", "<dd>0-0-0</dd>", "<h2>Players</h2>",
		`<td class="who">Robert Arnold IV</td>`, `<td class="who">Jeremy Young</td>`,
		`<p class="back"><a href="/?division=90096">‹ Standings</a></p>`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("winter team lacks %q", want)
		}
	}
	if strings.Index(b, "Arnold IV") > strings.Index(b, "Fedderly") || strings.Contains(b, `class="problem"`) {
		t.Error("roster not by name, or a problem shown")
	}
	// Unranked: no "th in the". Roster unavailable: say so.
	b = get(t, h, "/team/591206").Body.String()
	if !strings.Contains(b, `<p class="sub">Bravo</p>`) || !strings.Contains(b, "get the roster") {
		t.Errorf("unranked team without a roster:\n%s", b)
	}

	// Last season's team, with its goalie.
	b = get(t, h, "/team/554799?season=15331").Body.String()
	for _, want := range []string{
		"<h1>Oakshire</h1>", `<td class="who">Jonathan Gunther</td>`, "<h2>Goalies</h2>", `<td class="who">Paul Wanke</td>`,
		`<p class="back"><a href="/?division=82590&amp;season=15331">‹ Standings</a></p>`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("fall team lacks %q", want)
		}
	}
	// Opponents' links keep the season.
	if all, kept := strings.Count(b, `href="/team/`), regexp.MustCompile(`href="/team/\d+\?season=15331"`).FindAllString(b, -1); all == 0 || len(kept) != all {
		t.Errorf("%d team links, %d keep the season", all, len(kept))
	}
	get(t, h, "/team/587702")
	if src.count("roster") != 3 {
		t.Errorf("roster fetched %d times, want 3 (cached)", src.count("roster"))
	}
}

// Fall's Rogue has no logo on GameSheet; Winter's Rogue has one.
func TestBorrowedLogo(t *testing.T) {
	src := winterGameSheet(t)
	logos := &logoLog{}
	h := newServer(t, Config{Source: src, League: 620287, Now: nov10, Logo: logos.fetch})

	for range 3 {
		b := get(t, h, "/?season=15331&division=82591").Body.String()
		if !strings.Contains(b, `<img class="logo" src="/logo/554807"`) {
			t.Fatal("Fall's Rogue has no logo")
		}
	}
	if w := get(t, h, "/logo/554807"); w.Code != http.StatusOK ||
		len(logos.urls) != 1 || !strings.Contains(logos.urls[0], "f08e63df-d273-473b-a78f-bca21e49a200") {
		t.Errorf("logo: %d, fetched %v (want Winter's Rogue's)", w.Code, logos.urls)
	}
	// Winter's standings once for the page (Fall's), once for logos; Spring's
	// not needed.
	if n := src.count("standings"); n != 2 {
		t.Errorf("standings fetched %d times, want 2", n)
	}
	// Teams that have their own aren't touched.
	if b := get(t, h, "/?season=15331&division=82591").Body.String(); !strings.Contains(b, `<img class="logo" src="/logo/554805"`) {
		t.Error("Pirates lost their logo")
	}
}

func TestTeamKey(t *testing.T) {
	for in, want := range map[string]string{"Nova Cains": "novacains", "Novacains": "novacains", "Black Berry Benders": "blackberrybenders", "Kelly's Heroes": "kellysheroes", "  ": ""} {
		if got := teamKey(in); got != want {
			t.Errorf("teamKey(%q) = %q, want %q", in, got, want)
		}
	}
}
