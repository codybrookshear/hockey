// Package rhl serves rhl.brookshear.party: the RHL (The Rink Exchange's adult
// league) standings, results, upcoming games, goal leaders and team pages,
// from GameSheet. Public: no login, no cookies, no scripts.
package rhl

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
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
	Standings(ctx context.Context, season int) ([]gamesheet.Division, error)
	Games(ctx context.Context, season int) ([]gamesheet.Game, error)
}

const (
	seasonFreshFor = 6 * time.Hour   // which season is current
	statsFreshFor  = 3 * time.Minute // standings and games: live scores move
	logoFreshFor   = 24 * time.Hour
	retryAfter     = time.Minute
	perMinute      = 20 // requests a minute, to GameSheet and, separately, to its image CDN
	maxSeasonLinks = 10
)

type Config struct {
	Source Source
	// SeasonsPage returns the rink's standings page, whose GameSheet widgets
	// link to the league's seasons. Unused when Season is set.
	SeasonsPage func(ctx context.Context) ([]byte, error)
	Season      int // the season to show; 0 = the current one, from SeasonsPage
	League      int // GameSheet league ID: seasons from other leagues are ignored
	Logo        func(ctx context.Context, url string) (Logo, error)
	Location    *time.Location
	Log         *slog.Logger
	Now         func() time.Time // nil = time.Now
}

type Server struct {
	cfg       Config
	season    *cache.Cache[gamesheet.Season]
	standings *cache.Cache[[]gamesheet.Division]
	games     *cache.Cache[[]gamesheet.Game]
	logos     *cache.Cache[Logo]
	pages     map[string]*template.Template
	static    *serve.Static

	mu       sync.Mutex
	logoURLs map[int]string // team ID → logo URL, from GameSheet's data: the only logos /logo/ will fetch
}

func New(cfg Config) (*Server, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Season == 0 && cfg.SeasonsPage == nil {
		return nil, errors.New("rhl: set Season or SeasonsPage")
	}
	gs := cache.NewLimiter(perMinute, cfg.Now)
	s := &Server{
		cfg:       cfg,
		season:    cache.New[gamesheet.Season](seasonFreshFor, 5*time.Minute, gs, cfg.Now),
		standings: cache.New[[]gamesheet.Division](statsFreshFor, retryAfter, gs, cfg.Now),
		games:     cache.New[[]gamesheet.Game](statsFreshFor, retryAfter, gs, cfg.Now),
		logos:     cache.New[Logo](logoFreshFor, 10*time.Minute, cache.NewLimiter(perMinute, cfg.Now), cfg.Now),
		logoURLs:  map[int]string{},
	}
	staticFS, err := fs.Sub(files, "static")
	if err != nil {
		return nil, err
	}
	if s.static, err = serve.NewStatic(staticFS); err != nil {
		return nil, err
	}
	s.pages = map[string]*template.Template{}
	for _, page := range []string{"home", "team"} {
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
	mux.HandleFunc("GET /logo/{id}", s.logo)
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /robots.txt", serve.RobotsTxt)
	return serve.Wrap(mux, csp, s.cfg.Log)
}

const csp = "default-src 'none'; style-src 'self'; img-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// currentSeason finds the season to show: the configured one, or the current
// one among those the rink's page links to.
func (s *Server) currentSeason(ctx context.Context) cache.Result[gamesheet.Season] {
	return s.season.Get(ctx, "current", func(ctx context.Context) (gamesheet.Season, error) {
		if s.cfg.Season != 0 {
			return s.cfg.Source.Season(ctx, s.cfg.Season)
		}
		page, err := s.cfg.SeasonsPage(ctx)
		if err != nil {
			return gamesheet.Season{}, fmt.Errorf("seasons page: %w", err)
		}
		ids := gamesheet.SeasonLinks(page)
		if len(ids) == 0 {
			return gamesheet.Season{}, errors.New("seasons page links to no GameSheet seasons")
		}
		var seasons []gamesheet.Season
		for _, id := range ids[:min(len(ids), maxSeasonLinks)] {
			season, err := s.cfg.Source.Season(ctx, id)
			if err != nil {
				return gamesheet.Season{}, err
			}
			seasons = append(seasons, season)
		}
		season, ok := gamesheet.Current(seasons, s.cfg.League, gamesheet.Today(s.cfg.Now(), s.cfg.Location))
		if !ok {
			return gamesheet.Season{}, fmt.Errorf("none of seasons %v is the league's", ids)
		}
		return season, nil
	})
}

// stats is everything a page shows, with any problems getting it.
type stats struct {
	Season    gamesheet.Season
	Divisions []gamesheet.Division
	Games     []gamesheet.Game
	Fetched   time.Time // the oldest of the parts
	Stale     bool
	Problems  []string // for visitors; details go to the log
	Fatal     bool     // no season: nothing to show
}

func (s *Server) load(ctx context.Context) stats {
	var st stats
	sr := s.currentSeason(ctx)
	if sr.Err != nil {
		s.cfg.Log.Warn("season unavailable", "err", sr.Err.Error())
		st.Problems, st.Fatal = append(st.Problems, "Couldn't find the current season on GameSheet right now."), true
		return st
	}
	st.Season = sr.Value
	key := strconv.Itoa(st.Season.ID)
	dr := s.standings.Get(ctx, key, func(ctx context.Context) ([]gamesheet.Division, error) {
		return s.cfg.Source.Standings(ctx, st.Season.ID)
	})
	gr := s.games.Get(ctx, key, func(ctx context.Context) ([]gamesheet.Game, error) {
		return s.cfg.Source.Games(ctx, st.Season.ID)
	})
	if dr.Err != nil {
		s.cfg.Log.Warn("standings unavailable", "season", st.Season.ID, "err", dr.Err.Error())
		st.Problems = append(st.Problems, "Couldn't get the standings from GameSheet right now.")
	}
	if gr.Err != nil {
		s.cfg.Log.Warn("games unavailable", "season", st.Season.ID, "err", gr.Err.Error())
		st.Problems = append(st.Problems, "Couldn't get the games from GameSheet right now.")
	}
	st.Divisions, st.Games = dr.Value, gr.Value
	for _, r := range []struct {
		t     time.Time
		stale bool
	}{{dr.Fetched, dr.Stale}, {gr.Fetched, gr.Stale}} {
		if !r.t.IsZero() && (st.Fetched.IsZero() || r.t.Before(st.Fetched)) {
			st.Fetched = r.t
		}
		st.Stale = st.Stale || r.stale
	}
	s.rememberLogos(st)
	return st
}

func (s *Server) render(w http.ResponseWriter, page string, status int, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout.html", data); err != nil {
		s.cfg.Log.Error("render", "page", page, "err", err.Error())
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	st := s.load(r.Context())
	status := http.StatusOK
	if st.Fatal || (st.Divisions == nil && st.Games == nil) {
		status = http.StatusBadGateway
	}
	s.render(w, "home", status, s.homeData(st))
}

func (s *Server) team(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	st := s.load(r.Context())
	d, ok := s.teamData(st, id)
	switch {
	case st.Fatal || (st.Divisions == nil && st.Games == nil):
		s.render(w, "team", http.StatusBadGateway, d)
	case !ok:
		d.Problems = append(d.Problems, "There's no such team this season.")
		s.render(w, "team", http.StatusNotFound, d)
	default:
		s.render(w, "team", http.StatusOK, d)
	}
}
