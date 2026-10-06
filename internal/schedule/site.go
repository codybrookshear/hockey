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
	"strconv"
	"strings"
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
	Sides      []side // home then away, or just one for an event like public skate
	State      string // "past" or "now", on today's page
}

type side struct {
	Name    string
	Lockers []chip
}

// chip is one or more lockers on the same side of the rink: 1–4 are on one
// side (blue), 5 and up on the other (red). That, not home or away, is what
// tells a team where to go.
type chip struct {
	Label string // "2 & 3", after a locker icon
	Class string // "blue" or "red"; "" for a locker that isn't a number
}

// chips reads a locker cell ("2", "2 & 3", "8&5") as a chip per side of the
// rink, in the cell's order. Anything else is shown as given, uncolored.
func chips(lockers string) []chip {
	if lockers == "" {
		return nil
	}
	var out []chip
	for _, f := range strings.FieldsFunc(lockers, func(r rune) bool { return r == '&' || r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 {
			return []chip{{Label: lockers}}
		}
		class := "red"
		if n <= 4 {
			class = "blue"
		}
		if i := slices.IndexFunc(out, func(c chip) bool { return c.Class == class }); i >= 0 {
			out[i].Label += " & " + f
		} else {
			out = append(out, chip{Label: f, Class: class})
		}
	}
	return out
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
		var r row
		home, away := e.Home, e.Away
		sides := []frontline.Side{home, away}
		switch {
		case home.Name == away.Name: // one event, maybe with a locker on one side only
			if a := away.Locker; a != "" && a != home.Locker {
				if home.Locker != "" {
					a = home.Locker + " & " + a
				}
				home.Locker = a
			}
			sides = []frontline.Side{home}
		case home.Locker != "" && away.Locker == "": // away is the event's detail: "RBL Practice", "10/12U"
			if away.Name != "" {
				home.Name += " " + away.Name
			}
			sides = []frontline.Side{home}
		}
		for _, sd := range sides {
			if sd != (frontline.Side{}) { // not a blank cell
				r.Sides = append(r.Sides, side{Name: sd.Name, Lockers: chips(sd.Locker)})
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
	if today { // between two events on a sheet, the next one is shown as on now
		for _, s := range out {
			if k := slices.IndexFunc(s.Rows, func(r row) bool { return r.State != "past" }); k > 0 && s.Rows[k].State == "" {
				s.Rows[k].State = "now"
			}
		}
	}
	return out
}
