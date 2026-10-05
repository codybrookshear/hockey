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
	scorersTop = 10 // scoring leaders listed (more when tied for the last place)
	scorersMax = 15 // ...but never more than this
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
	Divisions []divisionTab // the toggle: empty with fewer than two divisions
	Standings []standing    // the selected division's
	Live      []game
	Upcoming  []day
	Later     []day // upcoming, after the first few days
	Results   []day
	Earlier   []day
	Scorers   []skater
	Goalies   []goalie
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
	Skaters  []skater
	Goalies  []goalie
}

type divisionTab struct {
	ID       int
	Title    string
	Selected bool
}

// divisionRef is a division in the season: from the standings, or the games
// when standings are unavailable.
type divisionRef struct {
	ID    int
	Title string
}

type standing struct {
	gamesheet.Standing
	ID      int // the team's; ID, Name, HasLogo and TBD as on a game's side, for the logo template
	Name    string
	HasLogo bool
	TBD     bool
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

// skater is a row of a skaters table: the league's leaders (Rank and Team
// set) or a team's players (Jersey set).
type skater struct {
	Rank          int // shared by ties
	Jersey        string
	Name          string
	TeamID        int
	Team          string // abbreviation(s): subs play for more than one
	GP, G, A, PTS int
	PIM           int
}

type goalie struct {
	Name            string
	TeamID          int
	Team            string
	GP, W, L, T, SO int
	GAA, SVPct      float64
}

func (s *Server) pageData(st stats) page {
	p := page{Season: st.Season, Fetched: st.Fetched.In(s.cfg.Location), Stale: st.Stale, Problems: st.Problems}
	if st.Season.ID != 0 {
		p.Source = fmt.Sprintf("%s/seasons/%d", gamesheet.DefaultBaseURL, st.Season.ID)
	}
	return p
}

// divisionsOf lists the season's divisions, by title.
func divisionsOf(st stats) []divisionRef {
	var out []divisionRef
	add := func(id int, title string) {
		if id != 0 && !slices.ContainsFunc(out, func(d divisionRef) bool { return d.ID == id }) {
			out = append(out, divisionRef{ID: id, Title: title})
		}
	}
	for _, d := range st.Divisions {
		add(d.ID, d.Title)
	}
	for _, g := range st.Games {
		add(g.DivisionID, g.Division)
	}
	slices.SortFunc(out, func(a, b divisionRef) int { return cmp.Compare(a.Title, b.Title) })
	return out
}

// pickDivision returns want if the season has it, else the first division
// (0 when there are none: nothing is filtered).
func pickDivision(divs []divisionRef, want int) int {
	if slices.ContainsFunc(divs, func(d divisionRef) bool { return d.ID == want }) {
		return want
	}
	if len(divs) > 0 {
		return divs[0].ID
	}
	return 0
}

// homeData is the home page for one division (0: all).
func (s *Server) homeData(st stats, div int) homeData {
	d := homeData{page: s.pageData(st)}
	if divs := divisionsOf(st); len(divs) > 1 {
		for _, ref := range divs {
			d.Divisions = append(d.Divisions, divisionTab{ID: ref.ID, Title: ref.Title, Selected: ref.ID == div})
		}
	}
	// The division's teams, to pick out their players.
	teams := map[int]bool{}
	for _, dv := range st.Divisions {
		if div != 0 && dv.ID != div {
			continue
		}
		for _, t := range dv.Teams {
			teams[t.Team.ID] = true
			d.Standings = append(d.Standings, standing{
				Standing: t, ID: t.Team.ID, Name: t.Team.Title, HasLogo: s.hasLogo(t.Team.ID),
			})
		}
	}

	var upcoming, finals []game
	for _, g := range st.Games {
		if div != 0 && g.DivisionID != div && g.Home.DivisionID != div && g.Visitor.DivisionID != div {
			continue
		}
		for _, sd := range []gamesheet.Side{g.Home, g.Visitor} {
			if sd.ID != 0 && (div == 0 || sd.DivisionID == div) {
				teams[sd.ID] = true
			}
		}
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
	inDivision := func(team int) bool { return div == 0 || teams[team] }
	d.Scorers = scorers(st.Skaters, inDivision)
	d.Goalies = goalies(st.Goalies, inDivision, true)
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
	d.Skaters = roster(st.Skaters, id)
	d.Goalies = goalies(st.Goalies, func(team int) bool { return team == id }, false)
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

// scorers is the scoring leaders among the teams include accepts (a
// division's): by points, then goals, then fewer games; ranks shared by ties
// on points. A sub counts only their games for those teams.
func scorers(all []gamesheet.Skater, include func(team int) bool) []skater {
	var out []skater
	for _, s := range all {
		row := skater{Name: playerName(s.Player)}
		var abbrs []string
		for _, t := range s.Teams {
			if t.GP == 0 || !include(t.ID) {
				continue
			}
			row.GP += t.GP
			row.G += t.G
			row.A += t.A
			row.PTS += t.PTS
			row.PIM += t.PIM
			if !slices.Contains(abbrs, t.Abbr) {
				abbrs = append(abbrs, t.Abbr)
			}
			if row.TeamID == 0 {
				row.TeamID = t.ID
			}
		}
		if row.PTS == 0 {
			continue
		}
		row.Team = strings.Join(abbrs, "/")
		out = append(out, row)
	}
	slices.SortStableFunc(out, func(a, b skater) int {
		if c := cmp.Compare(b.PTS, a.PTS); c != 0 {
			return c
		}
		if c := cmp.Compare(b.G, a.G); c != 0 {
			return c
		}
		return cmp.Compare(a.GP, b.GP)
	})
	for i := range out {
		out[i].Rank = i + 1
		if i > 0 && out[i].PTS == out[i-1].PTS {
			out[i].Rank = out[i-1].Rank
		}
	}
	if len(out) > scorersTop {
		cut := scorersTop
		for cut < len(out) && cut < scorersMax && out[cut].PTS == out[scorersTop-1].PTS {
			cut++
		}
		out = out[:cut]
	}
	return out
}

// roster is a team's skaters with games, by points: their stats for that
// team, which differ from their season's if they've subbed elsewhere.
func roster(all []gamesheet.Skater, team int) []skater {
	var out []skater
	for _, s := range all {
		for _, t := range s.Teams {
			if t.ID == team && t.GP > 0 {
				out = append(out, skater{
					Jersey: s.Jersey, Name: playerName(s.Player),
					GP: t.GP, G: t.G, A: t.A, PTS: t.PTS, PIM: t.PIM,
				})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b skater) int {
		if c := cmp.Compare(b.PTS, a.PTS); c != 0 {
			return c
		}
		if c := cmp.Compare(b.G, a.G); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out
}

// goalies is a row per goalie and team, for the teams include accepts, with
// games; best save percentage first. showTeam labels each row with its team.
func goalies(all []gamesheet.Goalie, include func(team int) bool, showTeam bool) []goalie {
	var out []goalie
	for _, g := range all {
		for _, t := range g.Teams {
			if t.GP == 0 || !include(t.ID) {
				continue
			}
			row := goalie{
				Name: playerName(g.Player),
				GP:   t.GP, W: t.W, L: t.L, T: t.T, SO: t.SO, GAA: t.GAA, SVPct: t.SVPct,
			}
			if showTeam {
				row.TeamID, row.Team = t.ID, t.Abbr
			}
			out = append(out, row)
		}
	}
	slices.SortStableFunc(out, func(a, b goalie) int {
		if c := cmp.Compare(b.SVPct, a.SVPct); c != 0 {
			return c
		}
		return cmp.Compare(a.GAA, b.GAA)
	})
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
// "DEREK DUSOME" → "Derek Dusome", "O'NEIL-SMITH" → "O'Neil-Smith",
// "MCMACKIN" → "McMackin". Mixed case is left as entered.
func playerName(p gamesheet.Player) string {
	name := strings.Join(strings.Fields(p.First+" "+p.Last), " ")
	if name != strings.ToUpper(name) && name != strings.ToLower(name) {
		return name
	}
	runes := []rune(strings.ToLower(name))
	start := true
	for i, r := range runes {
		// "Mc" names capitalize the letter after it too.
		mc := i >= 2 && runes[i-1] == 'c' && runes[i-2] == 'M' && (i == 2 || !unicode.IsLetter(runes[i-3]))
		if start || mc {
			runes[i] = unicode.ToUpper(r)
		}
		start = r == ' ' || r == '-' || r == '\''
	}
	return string(runes)
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
		// .901, the way save percentage is written
		"svpct": func(f float64) string { return strings.TrimPrefix(fmt.Sprintf("%.3f", f), "0") },
		"gaa":   func(f float64) string { return fmt.Sprintf("%.2f", f) },
		// A badge for teams without a logo: their initial.
		"initial": func(name string) string {
			for _, r := range name {
				return string(unicode.ToUpper(r))
			}
			return ""
		},
	}
}
