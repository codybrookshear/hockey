package rhl

import (
	"cmp"
	"fmt"
	"html/template"
	"slices"
	"strings"
	"time"
	"unicode"

	"hockey/internal/gamesheet"
)

const (
	daysShown  = 2  // game days shown before "earlier results" / "later games"
	leadersTop = 10 // goal leaders listed
)

type page struct {
	Season   gamesheet.Season
	Fetched  time.Time
	Stale    bool
	Problems []string
	Source   string // the season on GameSheet's own site
}

type homeData struct {
	page
	Live      []game
	Divisions []division
	Upcoming  []day
	Later     []day // upcoming, after the first few days
	Results   []day
	Earlier   []day
	Leaders   []leader
}

type teamData struct {
	page
	Team     gamesheet.Team
	HasLogo  bool
	Standing *gamesheet.Standing
	Division string
	Live     []game
	Upcoming []game
	Results  []game
	Scorers  []leader
}

type division struct {
	Title string
	Teams []standing
}

type standing struct {
	gamesheet.Standing
	HasLogo bool
}

type day struct {
	Label string // "Sun, Oct 4"
	Games []game
}

type game struct {
	ID       int
	Day      string // "Sun, Oct 4"
	Time     string // "9:00 AM"
	Visitor  side
	Home     side
	Final    bool
	Live     bool
	Tie      bool
	Kind     string // "Playoff", "Exhibition", or ""
	Division string
	Goals    []goal
	ShowDay  bool // on lists not grouped by day
}

type side struct {
	ID      int
	Name    string
	Abbr    string
	Goals   int
	Won     bool
	TBD     bool
	HasLogo bool
}

type goal struct {
	Period string // "1st", "OT"
	Clock  string // time left in the period
	Name   string
	Team   string // abbreviation
}

type leader struct {
	Rank   int // shared by ties
	Name   string
	TeamID int
	Team   string
	Goals  int
}

func (s *Server) pageData(st stats) page {
	p := page{Season: st.Season, Fetched: st.Fetched.In(s.cfg.Location), Stale: st.Stale, Problems: st.Problems}
	if st.Season.ID != 0 {
		p.Source = fmt.Sprintf("%s/seasons/%d", gamesheet.DefaultBaseURL, st.Season.ID)
	}
	return p
}

func (s *Server) homeData(st stats) homeData {
	d := homeData{page: s.pageData(st)}
	for _, div := range st.Divisions {
		out := division{Title: div.Title}
		for _, t := range div.Teams {
			out.Teams = append(out.Teams, standing{Standing: t, HasLogo: s.hasLogo(t.Team.ID)})
		}
		d.Divisions = append(d.Divisions, out)
	}
	slices.SortFunc(d.Divisions, func(a, b division) int { return cmp.Compare(a.Title, b.Title) })

	var upcoming, finals []game
	for _, g := range st.Games {
		switch row := s.game(g); {
		case g.Live():
			d.Live = append(d.Live, row)
		case g.Final():
			finals = append(finals, row)
		case g.Status == "scheduled":
			upcoming = append(upcoming, row)
		}
	}
	slices.Reverse(finals) // games come oldest first
	d.Upcoming, d.Later = split(s.days(upcoming))
	d.Results, d.Earlier = split(s.days(finals))
	d.Leaders = leaders(st.Games, 0, leadersTop)
	return d
}

func (s *Server) teamData(st stats, id int) (teamData, bool) {
	d := teamData{page: s.pageData(st), HasLogo: s.hasLogo(id)}
	found := false
	for _, div := range st.Divisions {
		for _, t := range div.Teams {
			if t.Team.ID == id {
				d.Team, d.Standing, d.Division, found = t.Team, &t, div.Title, true
			}
		}
	}
	for _, g := range st.Games {
		var us gamesheet.Side
		switch id {
		case g.Home.ID:
			us = g.Home
		case g.Visitor.ID:
			us = g.Visitor
		default:
			continue
		}
		if !found {
			d.Team, d.Division, found = us.Team, g.Division, true
		}
		row := s.game(g)
		row.ShowDay = true
		switch {
		case g.Live():
			d.Live = append(d.Live, row)
		case g.Final():
			d.Results = append(d.Results, row)
		case g.Status == "scheduled":
			d.Upcoming = append(d.Upcoming, row)
		}
	}
	slices.Reverse(d.Results)
	d.Scorers = leaders(st.Games, id, 0)
	for i := range d.Scorers {
		d.Scorers[i].Team = "" // all the same team
	}
	return d, found
}

func (s *Server) game(g gamesheet.Game) game {
	start := g.Start.In(s.cfg.Location)
	row := game{
		ID:       g.ID,
		Day:      start.Format("Mon, Jan 2"),
		Time:     start.Format("3:04 PM"),
		Visitor:  s.side(g.Visitor),
		Home:     s.side(g.Home),
		Final:    g.Final(),
		Live:     g.Live(),
		Division: g.Division,
	}
	switch g.Type {
	case "playoff":
		row.Kind = "Playoff"
	case "exhibition":
		row.Kind = "Exhibition"
	}
	if row.Final {
		row.Visitor.Won = g.Visitor.Goals > g.Home.Goals
		row.Home.Won = g.Home.Goals > g.Visitor.Goals
		row.Tie = g.Home.Goals == g.Visitor.Goals
	}
	for _, sd := range []gamesheet.Side{g.Visitor, g.Home} {
		for _, gl := range sd.Scorers {
			row.Goals = append(row.Goals, goal{
				Period: period(gl.Period), Clock: gl.Clock, Name: playerName(gl.Player), Team: sd.Abbr,
			})
		}
	}
	// In order: by period, then by time left on the clock, most first.
	slices.SortStableFunc(row.Goals, func(a, b goal) int {
		if c := cmp.Compare(periodOrder(a.Period), periodOrder(b.Period)); c != 0 {
			return c
		}
		return cmp.Compare(b.Clock, a.Clock) // "mm:ss", zero-padded
	})
	return row
}

func (s *Server) side(sd gamesheet.Side) side {
	out := side{ID: sd.ID, Name: sd.Title, Abbr: sd.Abbr, Goals: sd.Goals, TBD: sd.TBD()}
	if out.TBD {
		out.Name = "TBD"
	} else {
		out.HasLogo = s.hasLogo(sd.ID)
	}
	return out
}

func (s *Server) days(games []game) []day {
	var out []day
	for _, g := range games {
		if n := len(out); n > 0 && out[n-1].Label == g.Day {
			out[n-1].Games = append(out[n-1].Games, g)
			continue
		}
		out = append(out, day{Label: g.Day, Games: []game{g}})
	}
	return out
}

func split(days []day) (first, rest []day) {
	if len(days) <= daysShown {
		return days, nil
	}
	return days[:daysShown], days[daysShown:]
}

// leaders counts goals in final games (not exhibitions), for one team (or
// all, team 0), most first, top n (0: all). A player shows under the team
// of their latest goal; subs score for more than one.
func leaders(games []gamesheet.Game, team, n int) []leader {
	type tally struct {
		leader
		latest time.Time
	}
	byPlayer := map[int]*tally{}
	for _, g := range games {
		if !g.Final() || g.Type == "exhibition" {
			continue
		}
		for _, sd := range []gamesheet.Side{g.Home, g.Visitor} {
			if team != 0 && sd.ID != team {
				continue
			}
			for _, gl := range sd.Scorers {
				t := byPlayer[gl.Player.ID]
				if t == nil {
					t = &tally{leader: leader{Name: playerName(gl.Player)}}
					byPlayer[gl.Player.ID] = t
				}
				t.Goals++
				if !g.Start.Before(t.latest) {
					t.latest, t.TeamID, t.Team = g.Start, sd.ID, sd.Title
				}
			}
		}
	}
	var out []leader
	for _, t := range byPlayer {
		out = append(out, t.leader)
	}
	slices.SortFunc(out, func(a, b leader) int {
		if c := cmp.Compare(b.Goals, a.Goals); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	for i := range out {
		out[i].Rank = i + 1
		if i > 0 && out[i].Goals == out[i-1].Goals {
			out[i].Rank = out[i-1].Rank
		}
	}
	if n > 0 && len(out) > n {
		// Keep everyone tied with the last one shown.
		cut := n
		for cut < len(out) && out[cut].Goals == out[n-1].Goals {
			cut++
		}
		out = out[:cut]
	}
	return out
}

func period(p string) string {
	switch p {
	case "1":
		return "1st"
	case "2":
		return "2nd"
	case "3":
		return "3rd"
	case "4", "OT", "ot":
		return "OT"
	case "SO", "so":
		return "SO"
	}
	return p
}

func periodOrder(p string) int {
	switch p {
	case "1st":
		return 1
	case "2nd":
		return 2
	case "3rd":
		return 3
	case "OT":
		return 4
	case "SO":
		return 5
	}
	return 6
}

// playerName tidies names entered in all capitals (or all lower case):
// "DEREK DUSOME" → "Derek Dusome", "O'NEIL-SMITH" → "O'Neil-Smith". Mixed
// case is left as entered.
func playerName(p gamesheet.Player) string {
	name := strings.Join(strings.Fields(p.First+" "+p.Last), " ")
	if name != strings.ToUpper(name) && name != strings.ToLower(name) {
		return name
	}
	var b strings.Builder
	start := true
	for _, r := range strings.ToLower(name) {
		if start {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(r)
		}
		start = r == ' ' || r == '-' || r == '\''
	}
	return b.String()
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"asset": s.static.URL,
		"ordinal": func(n int) string {
			suffix := "th"
			if n%100 < 11 || n%100 > 13 {
				switch n % 10 {
				case 1:
					suffix = "st"
				case 2:
					suffix = "nd"
				case 3:
					suffix = "rd"
				}
			}
			return fmt.Sprint(n, suffix)
		},
		"signed": func(n int) string {
			if n > 0 {
				return fmt.Sprint("+", n)
			}
			return fmt.Sprint(n)
		},
		"stamp": func(t time.Time) string { return t.Format("3:04 PM") },
		"list":  func(xs ...any) []any { return xs },
	}
}
