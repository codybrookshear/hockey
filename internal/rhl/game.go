package rhl

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"hockey/internal/cache"
	"hockey/internal/gamesheet"
)

// gameData is a game's own page: the score, then everything the scoresheet
// has. The score, goals by period, shots and power plays come with the
// season's games; assists, penalties, shots by period and the players'
// lines from the game's box score, when GameSheet has one.
type gameData struct {
	page
	Found    bool
	Game     game
	Status   string // "Final", "Final · OT", "Live", or the start time
	When     string // "Sun, Oct 4 · 9:00 AM"
	Where    string
	Number   string
	Back     string // the home page, on the game's division
	Sheet    string // the game on GameSheet's own site
	Refresh  int    // seconds between reloads, while live; 0: none
	Columns  []string
	Goals    []lineRow // goals by period: visitor, home
	Shots    []lineRow // shots by period, likewise; empty if not recorded
	Compare  []compareRow
	Scoring  []scoringPeriod
	Scorers  bool // Scoring has only who scored and when (no box score)
	Penalty  []penaltyPeriod
	NoCalls  bool // a box score, and no penalties in it
	Lineups  []lineup
	HasLines bool
}

// lineRow is a team's row of a by-period table.
type lineRow struct {
	side
	Cells []int
	Total int
}

// compareRow is one stat, side by side. Share is the visitor's part of the
// two (0–100) when it's a count worth drawing, else -1.
type compareRow struct {
	Label         string
	Visitor, Home string
	Share         int
}

type scoringPeriod struct {
	Label string // "1st period"
	Goals []goalRow
}

type goalRow struct {
	Clock   string
	Team    side
	Name    string
	Count   int // the scorer's goals this season; 0: unknown
	Assists string
	Tags    []string // "PP", "SH", "EN", "GWG"
	Score   string   // after it: "2–1", visitor first
}

type penaltyPeriod struct {
	Label     string
	Penalties []penaltyRow
}

type penaltyRow struct {
	Clock      string
	Team       side
	Name       string
	Infraction string
	Minutes    int
}

type lineup struct {
	Team    side
	Skaters []boxLine
	Goalies []boxLine
}

type boxLine struct {
	Number        string
	Name          string
	G, A, PTS     int
	PIM           int
	Faced, Saves  int
	SVPct         float64
	GoalieCounts  bool
	sortName      string
	sortNumber    int
	sortNumberSet bool
}

func (s *Server) gamePage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	st := s.visit(r)
	d := gameData{page: s.pageData(st), Back: linksFor(st).home(0)}
	i := slices.IndexFunc(st.Games, func(g gamesheet.Game) bool { return g.ID == id })
	switch {
	case st.NoSeason:
		s.render(w, "game", http.StatusNotFound, d)
		return
	case st.Fatal || st.Games == nil:
		s.render(w, "game", http.StatusBadGateway, d)
		return
	case i < 0:
		d.Problems = append(d.Problems, "There's no such game this season.")
		s.render(w, "game", http.StatusNotFound, d)
		return
	}
	g := st.Games[i]
	// The box score: only for a game in the season's data that has started,
	// which bounds what's fetched.
	var box *gamesheet.BoxScore
	if g.Final() || g.Live() {
		season := st.Season.ID
		br := s.boxScore(r.Context(), season, g, false)
		switch {
		case br.Err == nil:
			box = &br.Value
		case br.Err == cache.ErrBusy:
			d.Problems = append(d.Problems, "Too many people are looking up games right now. The full scoresheet will be back in a minute.")
		default:
			s.cfg.Log.Warn("box score unavailable", "season", season, "game", id, "err", br.Err.Error())
			d.Problems = append(d.Problems, "Couldn't get this game's full scoresheet from GameSheet right now. Here's what the schedule has.")
		}
	}
	s.fillGame(&d, st, g, box)
	s.render(w, "game", http.StatusOK, d)
}

// boxScore gets a started game's box score: a live game's is kept a minute,
// a final's longer. now: fetched if it's no longer fresh (Run's), where a
// visitor gets the copy held while that happens.
func (s *Server) boxScore(ctx context.Context, season int, g gamesheet.Game, now bool) cache.Result[gamesheet.BoxScore] {
	boxes := s.boxes
	if g.Live() {
		boxes = s.liveBoxes
	}
	get := boxes.Serve
	if now {
		get = boxes.Get
	}
	return get(ctx, fmt.Sprint(season, "/", g.ID), func(ctx context.Context) (gamesheet.BoxScore, error) {
		b, err := s.cfg.Source.BoxScore(ctx, season, g.ID)
		if err == nil && (b.HomeID != g.Home.ID || b.VisitorID != g.Visitor.ID) {
			err = fmt.Errorf("box score %d is for teams %d and %d, not this game's", g.ID, b.VisitorID, b.HomeID)
		}
		return b, err
	})
}

func (s *Server) fillGame(d *gameData, st stats, g gamesheet.Game, box *gamesheet.BoxScore) {
	l := linksFor(st)
	d.Found = true
	d.Game = s.game(g, l)
	d.Back = l.home(g.DivisionID)
	d.When = d.Game.Day + " · " + d.Game.Time
	d.Where, d.Number = g.Location, g.Number
	d.Sheet = fmt.Sprintf("%s/seasons/%d/games/%d", gamesheet.DefaultBaseURL, st.Season.ID, g.ID)
	visitor, home := d.Game.Visitor, d.Game.Home
	switch {
	case g.Live():
		d.Status = "Live"
		d.Refresh = int(liveBoxFreshFor.Seconds())
	case g.Final():
		d.Status = "Final"
		// Decided after the third: say how.
		if n := len(g.Home.Periods); n > 3 {
			d.Status += " · " + periodShort(g.Home.Periods[n-1].Period)
		}
	default:
		d.Status = d.Game.Time
		return // not started: nothing to break down
	}

	// Goals by period, and shots if the scoresheet has them by period.
	if len(g.Home.Periods) > 0 && len(g.Home.Periods) == len(g.Visitor.Periods) {
		vr, hr := lineRow{side: visitor, Total: g.Visitor.Goals}, lineRow{side: home, Total: g.Home.Goals}
		for i, p := range g.Home.Periods {
			d.Columns = append(d.Columns, periodShort(p.Period))
			vr.Cells, hr.Cells = append(vr.Cells, g.Visitor.Periods[i].Goals), append(hr.Cells, p.Goals)
		}
		d.Goals = []lineRow{vr, hr}
		if box != nil && len(box.Shots) == len(d.Columns) {
			vs, hs := lineRow{side: visitor}, lineRow{side: home}
			for _, p := range box.Shots {
				vs.Cells, hs.Cells = append(vs.Cells, p.Visitor), append(hs.Cells, p.Home)
				vs.Total, hs.Total = vs.Total+p.Visitor, hs.Total+p.Home
			}
			if vs.Total+hs.Total > 0 {
				d.Shots = []lineRow{vs, hs}
			}
		}
	}

	// Side by side. Shots: the game's totals, or the periods' if it has none.
	vShots, hShots := g.Visitor.Shots, g.Home.Shots
	if vShots+hShots == 0 && len(d.Shots) == 2 {
		vShots, hShots = d.Shots[0].Total, d.Shots[1].Total
	}
	count := func(label string, v, h int) {
		row := compareRow{Label: label, Visitor: strconv.Itoa(v), Home: strconv.Itoa(h), Share: -1}
		if v+h > 0 {
			row.Share = (v*100 + (v+h)/2) / (v + h)
		}
		d.Compare = append(d.Compare, row)
	}
	text := func(label, v, h string) {
		d.Compare = append(d.Compare, compareRow{Label: label, Visitor: v, Home: h, Share: -1})
	}
	if vShots+hShots > 0 {
		count("Shots on goal", vShots, hShots)
		text("Shooting %", percent(g.Visitor.Goals, vShots), percent(g.Home.Goals, hShots))
		// A goalie's saves: the other team's shots that didn't go in.
		vSaves, hSaves := max(hShots-g.Home.Goals, 0), max(vShots-g.Visitor.Goals, 0)
		count("Saves", vSaves, hSaves)
		text("Save %", savePct(vSaves, hShots), savePct(hSaves, vShots))
	}
	if g.Visitor.PPChances+g.Home.PPChances > 0 {
		text("Power play", fmt.Sprint(g.Visitor.PPGoals, " for ", g.Visitor.PPChances), fmt.Sprint(g.Home.PPGoals, " for ", g.Home.PPChances))
	}

	sideOf := func(isHome bool) side {
		if isHome {
			return home
		}
		return visitor
	}
	if box == nil || len(box.Goals) < g.Home.Goals+g.Visitor.Goals {
		// No box score (or it's behind the score): who scored, from the games list.
		d.Scorers = true
		var goals []gamesheet.BoxGoal
		for _, sd := range []struct {
			gamesheet.Side
			home bool
		}{{g.Visitor, false}, {g.Home, true}} {
			for _, gl := range sd.Scorers {
				goals = append(goals, gamesheet.BoxGoal{
					Period: gl.Period, Clock: gl.Clock, Home: sd.home,
					Scorer: gamesheet.BoxRef{ID: gl.Player.ID, Name: gl.Player.First + " " + gl.Player.Last},
				})
			}
		}
		d.Scoring = scoring(goals, sideOf)
	} else {
		d.Scoring = scoring(box.Goals, sideOf)
	}
	if box == nil {
		return
	}

	pens := slices.Clone(box.Penalties)
	slices.SortStableFunc(pens, func(a, b gamesheet.BoxPenalty) int { return compareWhen(a.Period, a.Clock, b.Period, b.Clock) })
	vPIM, hPIM := 0, 0
	for _, p := range pens {
		row := penaltyRow{Clock: p.Clock, Team: sideOf(p.Home), Name: refName(p.Player, "Bench"), Infraction: p.Infraction, Minutes: p.Minutes}
		if p.Home {
			hPIM += p.Minutes
		} else {
			vPIM += p.Minutes
		}
		label := periodLong(p.Period)
		if n := len(d.Penalty); n == 0 || d.Penalty[n-1].Label != label {
			d.Penalty = append(d.Penalty, penaltyPeriod{Label: label})
		}
		d.Penalty[len(d.Penalty)-1].Penalties = append(d.Penalty[len(d.Penalty)-1].Penalties, row)
	}
	d.NoCalls = len(pens) == 0
	count("Penalties", countSide(pens, false), countSide(pens, true))
	count("Penalty minutes", vPIM, hPIM)

	for _, t := range []struct {
		side    side
		players []gamesheet.BoxPlayer
		faced   int // shots by the other team
	}{{visitor, box.Visitor, hShots}, {home, box.Home, vShots}} {
		lu := lineup{Team: t.side}
		for _, p := range t.players {
			row := boxLine{
				Number: p.Number, Name: playerName(p.Player), G: p.G, A: p.A, PTS: p.PTS, PIM: p.PIM,
				sortName: strings.ToLower(p.Last + " " + p.First),
			}
			if n, err := strconv.Atoi(p.Number); err == nil {
				row.sortNumber, row.sortNumberSet = n, true
			}
			if row.Name == "" {
				row.Name = "#" + p.Number
			}
			if !p.Goalie {
				lu.Skaters = append(lu.Skaters, row)
				continue
			}
			if p.GoalieCounts && p.Faced > 0 {
				row.GoalieCounts, row.Faced, row.Saves = true, p.Faced, max(p.Faced-p.GA, 0)
				row.SVPct = float64(row.Saves) / float64(p.Faced)
			}
			lu.Goalies = append(lu.Goalies, row)
		}
		// Points first, then goals; the rest by number.
		slices.SortStableFunc(lu.Skaters, func(a, b boxLine) int {
			if c := cmp.Compare(b.PTS, a.PTS); c != 0 {
				return c
			}
			if c := cmp.Compare(b.G, a.G); c != 0 {
				return c
			}
			if a.sortNumberSet != b.sortNumberSet {
				if a.sortNumberSet {
					return -1
				}
				return 1
			}
			if c := cmp.Compare(a.sortNumber, b.sortNumber); c != 0 {
				return c
			}
			return cmp.Compare(a.sortName, b.sortName)
		})
		if len(lu.Skaters)+len(lu.Goalies) > 0 {
			d.Lineups = append(d.Lineups, lu)
			d.HasLines = true
		}
	}
}

// scoring groups goals by period, in order, with the score after each.
func scoring(goals []gamesheet.BoxGoal, sideOf func(home bool) side) []scoringPeriod {
	goals = slices.Clone(goals)
	slices.SortStableFunc(goals, func(a, b gamesheet.BoxGoal) int { return compareWhen(a.Period, a.Clock, b.Period, b.Clock) })
	var out []scoringPeriod
	v, h := 0, 0
	for _, g := range goals {
		if g.Home {
			h++
		} else {
			v++
		}
		row := goalRow{
			Clock: g.Clock, Team: sideOf(g.Home), Name: refName(g.Scorer, "Unknown"), Count: g.Scorer.Count,
			Score: fmt.Sprint(v, "–", h),
		}
		var assists []string
		for _, a := range g.Assist {
			assists = append(assists, refName(a, ""))
		}
		row.Assists = strings.Join(assists, ", ")
		for _, tag := range []struct {
			on   bool
			name string
		}{{g.PP, "PP"}, {g.SH, "SH"}, {g.EN, "EN"}, {g.GWG, "GWG"}} {
			if tag.on {
				row.Tags = append(row.Tags, tag.name)
			}
		}
		label := periodLong(g.Period)
		if n := len(out); n == 0 || out[n-1].Label != label {
			out = append(out, scoringPeriod{Label: label})
		}
		out[len(out)-1].Goals = append(out[len(out)-1].Goals, row)
	}
	return out
}

func countSide(pens []gamesheet.BoxPenalty, home bool) int {
	n := 0
	for _, p := range pens {
		if p.Home == home {
			n++
		}
	}
	return n
}

// refName is a player as named in a goal or penalty: their name, else
// their number, else none.
func refName(r gamesheet.BoxRef, none string) string {
	if name := playerName(gamesheet.Player{First: r.Name}); name != "" {
		return name
	}
	if r.Number != "" {
		return "#" + r.Number
	}
	return none
}

func percent(n, of int) string {
	if of <= 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f%%", float64(n)*100/float64(of))
}

// savePct is saves of shots faced, the way it's written: ".917".
func savePct(saves, faced int) string {
	if faced <= 0 {
		return "–"
	}
	return fmtSVPct(float64(saves) / float64(faced))
}

func fmtSVPct(f float64) string { return strings.TrimPrefix(fmt.Sprintf("%.3f", f), "0") }

// periodRank orders periods: 1, 2, 3…, overtimes, then the shootout.
func periodRank(p string) int {
	p = strings.ToUpper(p)
	if n, err := strconv.Atoi(p); err == nil {
		return n
	}
	switch {
	case p == "SO":
		return 1000
	case strings.HasPrefix(p, "OT"):
		n, _ := strconv.Atoi(p[2:])
		return 100 + n
	}
	return 500
}

// compareWhen orders events: by period, then by time left on the clock
// ("mm:ss", zero-padded), most first.
func compareWhen(periodA, clockA, periodB, clockB string) int {
	if c := cmp.Compare(periodRank(periodA), periodRank(periodB)); c != 0 {
		return c
	}
	return cmp.Compare(clockB, clockA)
}

// periodShort heads a column: "1", "2", "3", "OT", "2OT", "SO".
func periodShort(p string) string {
	p = strings.ToUpper(p)
	switch {
	case p == "4" || p == "OT" || p == "OT1":
		return "OT"
	case strings.HasPrefix(p, "OT"):
		return p[2:] + "OT"
	}
	return p
}

// periodLong heads a list: "1st period", "Overtime", "Shootout".
func periodLong(p string) string {
	switch short := periodShort(p); short {
	case "1":
		return "1st period"
	case "2":
		return "2nd period"
	case "3":
		return "3rd period"
	case "OT":
		return "Overtime"
	case "SO":
		return "Shootout"
	default:
		if n, ok := strings.CutSuffix(short, "OT"); ok {
			return "Overtime " + n
		}
		return short
	}
}
