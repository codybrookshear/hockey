// Package schedule serves hockey.brookshear.party: the rink's daily
// schedule, one table per sheet, for a day picked by ?date=YYYY-MM-DD
// (default today). Public: no login, no cookies, no scripts.
package schedule

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"hockey/internal/cache"
	"hockey/internal/frontline"
	"hockey/internal/serve"
)

//go:embed templates static
var files embed.FS

// Sheets always shown, in this order, even when nothing's on them. Other
// surfaces, if the rink adds one, follow.
var sheets = []string{"Big Sheet", "Mini Sheet"}

// Days the site will show, relative to today: keeps the cache, and the
// requests it makes to the rink's site, bounded.
const (
	daysBack  = 7
	daysAhead = 120
)

// Source returns a day's events; *frontline.Client in production.
type Source interface {
	Day(ctx context.Context, date time.Time) ([]frontline.Event, error)
}

const (
	freshFor         = 10 * time.Minute // serve a fetched day this long before asking again
	retryAfter       = time.Minute      // after a failed fetch of a day, wait this long
	fetchesPerMinute = 20               // to the rink's site, across all days
)

type Config struct {
	Source    Source
	SourceURL string // the rink's page, linked in the footer
	Location  *time.Location
	Log       *slog.Logger
	Now       func() time.Time // nil = time.Now
}

type Server struct {
	cfg    Config
	days   *cache.Cache[[]frontline.Event] // by date
	page   *template.Template
	static *serve.Static
}

func New(cfg Config) (*Server, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	staticFS, err := fs.Sub(files, "static")
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:  cfg,
		days: cache.New[[]frontline.Event](freshFor, retryAfter, cache.NewLimiter(fetchesPerMinute, cfg.Now), cfg.Now),
	}
	if s.static, err = serve.NewStatic(staticFS); err != nil {
		return nil, err
	}
	s.page, err = template.New("schedule.html").Funcs(template.FuncMap{
		"asset": s.static.URL,
	}).ParseFS(files, "templates/schedule.html")
	if err != nil {
		return nil, fmt.Errorf("schedule: template: %w", err)
	}
	return s, nil
}

// Handler returns the site with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// GET patterns also match HEAD; other methods get 405.
	mux.HandleFunc("GET /{$}", s.schedule)
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /robots.txt", serve.RobotsTxt)
	return serve.Wrap(mux, csp, s.cfg.Log)
}

const csp = "default-src 'none'; style-src 'self'; img-src 'self'; " +
	"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

type pageData struct {
	Date       time.Time
	Today      bool
	Prev, Next string // ?date= values; "" past the window's ends
	Min, Max   string // the window, for the date picker
	Sheets     []sheet
	Fetched    time.Time
	Stale      bool
	Problem    string // instead of the schedule
	SourceURL  string
}

type sheet struct {
	Name string
	Rows []row
}

type row struct {
	Start, End string // "7:15", "8:45 AM"; Start has AM/PM only when End's differs
	Home, Away frontline.Side
	Same       bool   // one event in both columns, like public skate: shown as Home
	State      string // "past" or "now", on today's page
}

func (s *Server) schedule(w http.ResponseWriter, r *http.Request) {
	loc := s.cfg.Location
	now := s.cfg.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	first, last := today.AddDate(0, 0, -daysBack), today.AddDate(0, 0, daysAhead)
	d := pageData{
		Date:      today,
		Min:       first.Format(time.DateOnly),
		Max:       last.Format(time.DateOnly),
		SourceURL: s.cfg.SourceURL,
	}

	status := http.StatusOK
	if q := r.URL.Query().Get("date"); q != "" {
		t, err := time.ParseInLocation(time.DateOnly, q, loc)
		switch {
		case err != nil:
			d.Problem, status = "That's not a date.", http.StatusBadRequest
		case t.Before(first) || t.After(last):
			d.Problem, status = fmt.Sprintf("Pick a day from %s to %s.",
				first.Format("Jan 2"), last.Format("Jan 2, 2006")), http.StatusNotFound
		default:
			d.Date = t
		}
	}
	d.Today = d.Date.Equal(today)
	if p := d.Date.AddDate(0, 0, -1); !p.Before(first) {
		d.Prev = p.Format(time.DateOnly)
	}
	if n := d.Date.AddDate(0, 0, 1); !n.After(last) {
		d.Next = n.Format(time.DateOnly)
	}

	if d.Problem == "" {
		res := s.days.Get(r.Context(), d.Date.Format(time.DateOnly), func(ctx context.Context) ([]frontline.Event, error) {
			return s.cfg.Source.Day(ctx, d.Date)
		})
		if res.Err != nil {
			s.cfg.Log.Warn("schedule unavailable", "date", d.Date.Format(time.DateOnly), "err", res.Err.Error())
			d.Problem, status = "Couldn't get the schedule from the rink's site right now.", http.StatusBadGateway
		} else {
			d.Sheets, d.Fetched, d.Stale = group(res.Value, now, d.Today), res.Fetched.In(loc), res.Stale
			if res.Stale {
				s.cfg.Log.Warn("serving a stale schedule", "date", d.Date.Format(time.DateOnly))
			}
		}
	}

	var buf bytes.Buffer
	if err := s.page.Execute(&buf, d); err != nil {
		s.cfg.Log.Error("render", "err", err.Error())
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// group splits events into one table per sheet.
func group(events []frontline.Event, now time.Time, today bool) []sheet {
	out := make([]sheet, len(sheets))
	for i, name := range sheets {
		out[i].Name = name
	}
	for _, e := range events {
		i := slices.IndexFunc(out, func(s sheet) bool { return s.Name == e.Surface })
		if i < 0 {
			out = append(out, sheet{Name: e.Surface})
			i = len(out) - 1
		}
		r := row{Home: e.Home, Away: e.Away}
		if e.Home.Name == e.Away.Name { // one event, maybe with a locker on one side only
			r.Same = true
			if a := e.Away.Locker; a != "" && a != r.Home.Locker {
				if r.Home.Locker != "" {
					a = r.Home.Locker + ", " + a
				}
				r.Home.Locker = a
			}
		}
		r.Start, r.End = e.Start.Format("3:04"), e.End.Format("3:04 PM")
		if e.Start.Format("PM") != e.End.Format("PM") {
			r.Start = e.Start.Format("3:04 PM")
		}
		if today {
			switch {
			case !now.Before(e.End):
				r.State = "past"
			case !now.Before(e.Start):
				r.State = "now"
			}
		}
		out[i].Rows = append(out[i].Rows, r)
	}
	return out
}
