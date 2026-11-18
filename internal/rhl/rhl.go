// Package rhl serves rhl.brookshear.party: the RHL (The Rink Exchange's adult
// league) standings, results, upcoming games, scoring leaders, goalies and
// team pages, and a page per game, from GameSheet, one division at a time:
// the current season, or an earlier one picked from the season menu. Public:
// no login, no scripts; one cookie, remembering the division picked.
//
// Visitors don't wait for GameSheet: Run keeps the current season's stats
// fresh in the background, and anything else is served from the last copy
// while a newer one is fetched (cache.Serve).
package rhl

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"hockey/internal/cache"
	"hockey/internal/gamesheet"
	"hockey/internal/serve"
)

//go:embed templates static
var files embed.FS

// Source is GameSheet; *gamesheet.Client in production.
type Source interface {
	Season(ctx context.Context, id int) (gamesheet.Season, error)
	LeagueSeasons(ctx context.Context, league int) ([]gamesheet.Season, error)
	Standings(ctx context.Context, season int) ([]gamesheet.Division, error)
	Games(ctx context.Context, season int) ([]gamesheet.Game, error)
	Skaters(ctx context.Context, season int) ([]gamesheet.Skater, error)
	Goalies(ctx context.Context, season int) ([]gamesheet.Goalie, error)
	Roster(ctx context.Context, season, team int) (gamesheet.Roster, error)
	BoxScore(ctx context.Context, season, game int) (gamesheet.BoxScore, error)
}

const (
	seasonFreshFor  = 6 * time.Hour    // the league's seasons, and which is current
	statsFreshFor   = 3 * time.Minute  // standings and games: live scores move
	playersFreshFor = 10 * time.Minute // player stats and rosters: a few pages each, and change only after games
	liveBoxFreshFor = time.Minute      // a live game's scoresheet
	boxFreshFor     = 30 * time.Minute // a final's: corrections are rare
	logoFreshFor    = 24 * time.Hour
	retryAfter      = time.Minute
	perMinute       = 20 // fetches a minute, to GameSheet and, separately, to its image CDN

	// Run refreshes the current season a little sooner than it stops being
	// fresh, so a visitor's request never has to: standings and games each
	// time, player stats (a few pages) every third.
	warmEvery        = statsFreshFor - 15*time.Second
	warmPlayersEvery = 3
	// With no visitors for a while, less often: the first one back gets
	// stats at most this old, and a refresh starts behind them.
	warmBoxesFor = 8 * 24 * time.Hour
	warmBoxesMax = 12
	idleAfter    = 30 * time.Minute
	idleEvery    = 15 * time.Minute
)

type Config struct {
	Source Source
	Season int // the GameSheet season to show by default; 0 = League's current one
	League int // the GameSheet league: its seasons are the ones visitors can pick
	Logo   func(ctx context.Context, url string) (Logo, error)
	// SecureCookies marks the division cookie Secure: in production, where
	// visitors arrive over HTTPS (through Cloudflare), not on a dev box's
	// plain HTTP.
	SecureCookies bool
	Location      *time.Location
	Log           *slog.Logger
	Now           func() time.Time // nil = time.Now
}

type Server struct {
	cfg       Config
	season    *cache.Cache[gamesheet.Season] // Config.Season's
	seasons   *cache.Cache[[]gamesheet.Season]
	standings *cache.Cache[[]gamesheet.Division]
	games     *cache.Cache[[]gamesheet.Game]
	skaters   *cache.Cache[[]gamesheet.Skater]
	goalies   *cache.Cache[[]gamesheet.Goalie]
	rosters   *cache.Cache[gamesheet.Roster]
	boxes     *cache.Cache[gamesheet.BoxScore] // finals', by season/game
	liveBoxes *cache.Cache[gamesheet.BoxScore]
	// Other seasons' logos, by teamKey: for teams with none this season.
	seasonLogos *cache.Cache[map[string]string]
	logos       *cache.Cache[Logo]
	pages       map[string]*template.Template
	static      *serve.Static

	lastVisit atomic.Int64 // Unix seconds; 0: no visitor yet

	mu       sync.Mutex
	logoURLs map[int]string // team ID → logo URL, from GameSheet's data: the only logos /logo/ will fetch
}

func New(cfg Config) (*Server, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Season == 0 && cfg.League == 0 {
		return nil, errors.New("rhl: set Season or League")
	}
	gs := cache.NewLimiter(perMinute, cfg.Now)
	s := &Server{
		cfg:         cfg,
		season:      cache.New[gamesheet.Season](seasonFreshFor, 5*time.Minute, gs, cfg.Now),
		seasons:     cache.New[[]gamesheet.Season](seasonFreshFor, 5*time.Minute, gs, cfg.Now),
		standings:   cache.New[[]gamesheet.Division](statsFreshFor, retryAfter, gs, cfg.Now),
		games:       cache.New[[]gamesheet.Game](statsFreshFor, retryAfter, gs, cfg.Now),
		skaters:     cache.New[[]gamesheet.Skater](playersFreshFor, retryAfter, gs, cfg.Now),
		goalies:     cache.New[[]gamesheet.Goalie](playersFreshFor, retryAfter, gs, cfg.Now),
		rosters:     cache.New[gamesheet.Roster](playersFreshFor, retryAfter, gs, cfg.Now),
		boxes:       cache.New[gamesheet.BoxScore](boxFreshFor, 10*time.Minute, gs, cfg.Now),
		liveBoxes:   cache.New[gamesheet.BoxScore](liveBoxFreshFor, retryAfter, gs, cfg.Now),
		seasonLogos: cache.New[map[string]string](logoFreshFor, 10*time.Minute, gs, cfg.Now),
		logos:       cache.New[Logo](logoFreshFor, 10*time.Minute, cache.NewLimiter(perMinute, cfg.Now), cfg.Now),
		logoURLs:    map[int]string{},
	}
	staticFS, err := fs.Sub(files, "static")
	if err != nil {
		return nil, err
	}
	if s.static, err = serve.NewStatic(staticFS); err != nil {
		return nil, err
	}
	s.pages = map[string]*template.Template{}
	for _, page := range []string{"home", "team", "game"} {
		t, err := template.New("layout.html").Funcs(s.funcs()).
			ParseFS(files, "templates/layout.html", "templates/games.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("rhl: template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	return s, nil
}

// Handler returns the site with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// GET patterns also match HEAD; other methods get 405.
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /team/{id}", s.team)
	mux.HandleFunc("GET /game/{id}", s.gamePage)
	mux.HandleFunc("GET /logo/{id}", s.logo)
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /robots.txt", serve.RobotsTxt)
	return serve.Wrap(mux, csp, s.cfg.Log)
}

const csp = "default-src 'none'; style-src 'self'; img-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// leagueSeasons is the league's public seasons, latest first: the ones a
// visitor can pick, which bounds the seasons anything is fetched for.
func (s *Server) leagueSeasons(ctx context.Context) cache.Result[[]gamesheet.Season] {
	return s.seasons.Serve(ctx, "league", func(ctx context.Context) ([]gamesheet.Season, error) {
		all, err := s.cfg.Source.LeagueSeasons(ctx, s.cfg.League)
		if err != nil {
			return nil, err
		}
		var out []gamesheet.Season
		for _, se := range all {
			if se.LeagueID == s.cfg.League && se.Public {
				out = append(out, se)
			}
		}
		slices.SortStableFunc(out, func(a, b gamesheet.Season) int { return cmp.Compare(b.Start, a.Start) })
		return out, nil
	})
}

// currentSeason is the season shown by default: the configured one, or the
// league's current one, from its seasons (list).
func (s *Server) currentSeason(ctx context.Context, list cache.Result[[]gamesheet.Season]) (gamesheet.Season, error) {
	if s.cfg.Season != 0 {
		r := s.season.Serve(ctx, "configured", func(ctx context.Context) (gamesheet.Season, error) {
			return s.cfg.Source.Season(ctx, s.cfg.Season)
		})
		return r.Value, r.Err
	}
	if list.Err != nil {
		return gamesheet.Season{}, list.Err
	}
	season, ok := gamesheet.Current(list.Value, s.cfg.League, gamesheet.Today(s.cfg.Now(), s.cfg.Location))
	if !ok {
		return gamesheet.Season{}, fmt.Errorf("league %d has no public seasons", s.cfg.League)
	}
	return season, nil
}

// stats is everything a page shows, with any problems getting it.
type stats struct {
	Season    gamesheet.Season
	Current   gamesheet.Season   // the default season; zero if unknown
	Seasons   []gamesheet.Season // for the menu, latest first; nil if unknown
	Divisions []gamesheet.Division
	Games     []gamesheet.Game
	Skaters   []gamesheet.Skater
	Goalies   []gamesheet.Goalie
	Fetched   time.Time // the oldest of the parts
	Stale     bool
	Problems  []string // for visitors; details go to the log
	Fatal     bool     // no season: nothing to show
	NoSeason  bool     // ...because the one asked for isn't one of the league's
}

// seasonParam is the season a request asks for (?season=<id>): 0, the
// default one, when it doesn't.
func seasonParam(r *http.Request) int {
	id, err := strconv.Atoi(r.URL.Query().Get("season"))
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// visit loads the season a visitor's request asks for.
func (s *Server) visit(r *http.Request) stats {
	s.lastVisit.Store(s.cfg.Now().Unix())
	return s.load(r.Context(), seasonParam(r), held)
}

// How load gets a season's stats: for a visitor, the copies held, however
// old, with newer ones fetched behind them; for Run, from GameSheet now.
type loading int

const (
	held         loading = iota
	refreshGames         // standings and games now; player stats if no longer fresh
	refreshAll
)

// load gets season want (0: the current one), if it's one of the league's.
func (s *Server) load(ctx context.Context, want int, how loading) stats {
	var st stats
	list := cache.Result[[]gamesheet.Season]{Err: errors.New("no league configured")}
	if s.cfg.League != 0 {
		if list = s.leagueSeasons(ctx); list.Err != nil {
			s.cfg.Log.Warn("league seasons unavailable", "err", list.Err.Error())
		}
	}
	cur, curErr := s.currentSeason(ctx, list)
	if curErr != nil {
		s.cfg.Log.Warn("season unavailable", "err", curErr.Error())
	}
	st.Current = cur
	if list.Err == nil {
		st.Seasons = list.Value
		if cur.ID != 0 && !slices.ContainsFunc(st.Seasons, func(se gamesheet.Season) bool { return se.ID == cur.ID }) {
			st.Seasons = append([]gamesheet.Season{cur}, st.Seasons...) // a configured season from another league
		}
	}
	switch i := slices.IndexFunc(st.Seasons, func(se gamesheet.Season) bool { return se.ID == want }); {
	case want == 0 || want == cur.ID:
		if curErr != nil {
			st.Problems, st.Fatal = append(st.Problems, "Couldn't find the current season on GameSheet right now."), true
			return st
		}
		st.Season = cur
	case i >= 0:
		st.Season = st.Seasons[i]
	case list.Err != nil:
		st.Problems, st.Fatal = append(st.Problems, "Couldn't get the RHL's seasons from GameSheet right now."), true
		return st
	default:
		st.Problems, st.Fatal, st.NoSeason = append(st.Problems, "There's no such RHL season."), true, true
		return st
	}
	id := st.Season.ID
	key := strconv.Itoa(id)
	var (
		dr cache.Result[[]gamesheet.Division]
		gr cache.Result[[]gamesheet.Game]
		sk cache.Result[[]gamesheet.Skater]
		gl cache.Result[[]gamesheet.Goalie]
	)
	standings := func(ctx context.Context) ([]gamesheet.Division, error) { return s.cfg.Source.Standings(ctx, id) }
	games := func(ctx context.Context) ([]gamesheet.Game, error) { return s.cfg.Source.Games(ctx, id) }
	skaters := func(ctx context.Context) ([]gamesheet.Skater, error) { return s.cfg.Source.Skaters(ctx, id) }
	goalies := func(ctx context.Context) ([]gamesheet.Goalie, error) { return s.cfg.Source.Goalies(ctx, id) }
	switch how {
	case refreshAll:
		dr, gr = s.standings.Refresh(ctx, key, standings), s.games.Refresh(ctx, key, games)
		sk, gl = s.skaters.Refresh(ctx, key, skaters), s.goalies.Refresh(ctx, key, goalies)
	case refreshGames:
		dr, gr = s.standings.Refresh(ctx, key, standings), s.games.Refresh(ctx, key, games)
		sk, gl = s.skaters.Get(ctx, key, skaters), s.goalies.Get(ctx, key, goalies)
	default:
		dr, gr = s.standings.Serve(ctx, key, standings), s.games.Serve(ctx, key, games)
		sk, gl = s.skaters.Serve(ctx, key, skaters), s.goalies.Serve(ctx, key, goalies)
	}
	st.Divisions, st.Games, st.Skaters, st.Goalies = dr.Value, gr.Value, sk.Value, gl.Value
	for _, part := range []struct {
		what  string
		err   error
		at    time.Time
		stale bool
	}{
		{"standings", dr.Err, dr.Fetched, dr.Stale},
		{"games", gr.Err, gr.Fetched, gr.Stale},
		{"player stats", sk.Err, sk.Fetched, sk.Stale},
		{"goalie stats", gl.Err, gl.Fetched, gl.Stale},
	} {
		if part.err != nil {
			s.cfg.Log.Warn(part.what+" unavailable", "season", id, "err", part.err.Error())
			st.Problems = append(st.Problems, "Couldn't get the "+part.what+" from GameSheet right now.")
			continue
		}
		if st.Fetched.IsZero() || part.at.Before(st.Fetched) {
			st.Fetched = part.at
		}
		st.Stale = st.Stale || part.stale
	}
	s.rememberLogos(st)
	s.borrowLogos(ctx, st)
	return st
}

// Run keeps the current season's stats and logos in memory and fresh until
// ctx is done, so that pages are ready before anyone asks: a fixed number of
// requests to GameSheet, however many visitors (or none) there are.
func (s *Server) Run(ctx context.Context) {
	tick := time.NewTicker(warmEvery)
	defer tick.Stop()
	var last time.Time
	for n := 0; ; {
		now := s.cfg.Now()
		visited := time.Unix(s.lastVisit.Load(), 0)
		if last.IsZero() || now.Sub(visited) < idleAfter || now.Sub(last) >= idleEvery {
			s.warm(ctx, n%warmPlayersEvery == 0)
			last = now
			n++
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// warm fetches the current season's standings and games (and, with players,
// its player stats), then what isn't held yet of its teams' logos and its
// latest games' box scores.
func (s *Server) warm(ctx context.Context, players bool) {
	how := refreshGames
	if players {
		how = refreshAll
	}
	st := s.load(ctx, 0, how)
	if st.Fatal {
		return
	}
	seen := map[int]bool{}
	logo := func(t gamesheet.Team) {
		if t.ID == 0 || seen[t.ID] || ctx.Err() != nil {
			return
		}
		seen[t.ID] = true
		s.mu.Lock()
		u, ok := s.logoURLs[t.ID]
		s.mu.Unlock()
		if ok {
			s.logos.Get(ctx, u, func(ctx context.Context) (Logo, error) { return s.cfg.Logo(ctx, u) })
		}
	}
	for _, d := range st.Divisions {
		for _, t := range d.Teams {
			logo(t.Team)
		}
	}
	for _, g := range st.Games {
		logo(g.Home.Team)
		logo(g.Visitor.Team)
	}
	// The box scores of the games people open: the ones on now, and the
	// last week's results, latest first.
	now, recent := s.cfg.Now(), 0
	for _, g := range slices.Backward(st.Games) {
		if ctx.Err() != nil {
			return
		}
		if g.Live() || (g.Final() && now.Sub(g.Start) < warmBoxesFor && recent < warmBoxesMax) {
			if g.Final() {
				recent++
			}
			s.boxScore(ctx, st.Season.ID, g, true)
		}
	}
}

func (s *Server) render(w http.ResponseWriter, page string, status int, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout.html", data); err != nil {
		s.cfg.Log.Error("render", "page", page, "err", err.Error())
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status == http.StatusOK {
		// Public stats: the browser may keep the page for going back to it
		// (from a game to the list), and asks again otherwise.
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// divisionCookie remembers the division a visitor picked: its GameSheet ID,
// nothing else. The site has no scripts, so a cookie is how it remembers.
const divisionCookie = "division"

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	st := s.visit(r)
	status := http.StatusOK
	switch {
	case st.NoSeason:
		status = http.StatusNotFound
	case st.Fatal || (st.Divisions == nil && st.Games == nil):
		status = http.StatusBadGateway
	}

	// The toggle links to /?division=<id>: show it, and remember it (a
	// division of the current season; others' are gone by next season).
	// Otherwise the remembered one, or the first.
	divs := divisionsOf(st)
	div := 0
	if id, err := strconv.Atoi(r.URL.Query().Get("division")); err == nil && pickDivision(divs, id) == id {
		div = id
		if st.Season.ID == st.Current.ID {
			http.SetCookie(w, &http.Cookie{
				Name: divisionCookie, Value: strconv.Itoa(id), Path: "/",
				MaxAge: 400 * 24 * 60 * 60, HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
			})
		}
	} else if c, err := r.Cookie(divisionCookie); err == nil {
		div, _ = strconv.Atoi(c.Value)
	}
	s.render(w, "home", status, s.homeData(st, pickDivision(divs, div)))
}

func (s *Server) team(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	st := s.visit(r)
	d, ok := s.teamData(st, id)
	switch {
	case st.NoSeason:
		s.render(w, "team", http.StatusNotFound, d)
	case st.Fatal || (st.Divisions == nil && st.Games == nil):
		s.render(w, "team", http.StatusBadGateway, d)
	case !ok:
		d.Problems = append(d.Problems, "There's no such team this season.")
		s.render(w, "team", http.StatusNotFound, d)
	default:
		// The roster: only for a team in the season's data, which bounds
		// what's fetched. Without it, the players who've played.
		season := st.Season.ID
		rr := s.rosters.Serve(r.Context(), fmt.Sprint(season, "/", id), func(ctx context.Context) (gamesheet.Roster, error) {
			return s.cfg.Source.Roster(ctx, season, id)
		})
		if rr.Err != nil {
			s.cfg.Log.Warn("roster unavailable", "season", season, "team", id, "err", rr.Err.Error())
			d.Problems = append(d.Problems, "Couldn't get the roster from GameSheet right now.")
		} else {
			d.Skaters = teamSkaters(rr.Value.Skaters, st.Skaters, id)
			d.Goalies = teamGoalies(rr.Value.Goalies, st.Goalies, id)
		}
		s.render(w, "team", http.StatusOK, d)
	}
}
