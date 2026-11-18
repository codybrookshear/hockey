package gamesheet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// BoxScore is one game's scoresheet: what the season's list of games leaves
// out. Assists and penalties, shots by period, and each player's line.
type BoxScore struct {
	HomeID, VisitorID int
	Goals             []BoxGoal    // in the order GameSheet lists them: by period, then time
	Penalties         []BoxPenalty // likewise
	Shots             []PeriodShots
	Home, Visitor     []BoxPlayer // who dressed
}

// BoxRef is a player named in a goal or penalty. Leagues that hide names
// leave only the number.
type BoxRef struct {
	ID     int
	Name   string // as entered; may be empty
	Number string
	Count  int // this season's goals (a scorer's) or assists, with this one; 0 if not given
}

func (r BoxRef) Known() bool { return r.ID != 0 || r.Name != "" || r.Number != "" }

type BoxGoal struct {
	Period string // "1", "2", "3", "OT1"…
	Clock  string // time left in the period
	Home   bool
	Scorer BoxRef
	Assist []BoxRef // none, one or two
	// Power play, short-handed, empty net, game winner.
	PP, SH, EN, GWG bool
}

type BoxPenalty struct {
	Period     string
	Clock      string
	Home       bool
	Player     BoxRef // who took it; unknown for a bench penalty
	Infraction string // "Tripping - Minor"
	Minutes    int
}

type PeriodShots struct {
	Period        string
	Home, Visitor int
}

// BoxPlayer is a player's line for the game.
type BoxPlayer struct {
	Player
	Number       string
	Goalie       bool
	G, A, PTS    int
	PIM          int
	Faced, GA    int  // a goalie's shots faced and goals against
	GoalieCounts bool // ...when the scoresheet has them
}

// BoxScore returns a game's scoresheet. A game that hasn't started has an
// empty one.
//
// Unlike the other endpoints, this one's shape comes from other people's
// notes on it, not from a saved response (see the test): scalars are read
// leniently (GameSheet writes some IDs and counts as strings), the structure
// strictly.
func (c *Client) BoxScore(ctx context.Context, season, game int) (BoxScore, error) {
	var raw json.RawMessage
	path := fmt.Sprintf("useBoxScore/getGameStats/%d/games/%d", season, game)
	if err := c.get(ctx, path, nil, &raw); err != nil {
		return BoxScore{}, err
	}
	b, err := parseBoxScore(raw)
	if err != nil {
		return BoxScore{}, fmt.Errorf("box score %d: %w", game, err)
	}
	return b, nil
}

// loose is a scalar GameSheet writes as a number, a string or null.
type loose string

func (l *loose) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || string(b) == "null":
		*l = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*l = loose(strings.TrimSpace(s))
	case b[0] == '{' || b[0] == '[':
		return errors.New("expected a number or a string")
	default: // a number, true or false
		*l = loose(b)
	}
	return nil
}

func (l loose) String() string { return string(l) }

// Int is the whole number it holds: ok is false for none ("", null, "-").
func (l loose) Int() (n int, ok bool) {
	n, err := strconv.Atoi(string(l))
	if err != nil {
		f, ferr := strconv.ParseFloat(string(l), 64)
		if ferr != nil || f != float64(int(f)) {
			return 0, false
		}
		n = int(f)
	}
	return n, true
}

func (l loose) int() int {
	n, _ := l.Int()
	return n
}

type rawBoxRef struct {
	ID     loose `json:"id"`
	Title  loose `json:"title"`
	Number loose `json:"number"`
	// The scorer's goals this season, or the assister's assists.
	Total loose `json:"totalGoalCount"`
}

func (r *rawBoxRef) ref() BoxRef {
	if r == nil {
		return BoxRef{}
	}
	return BoxRef{ID: r.ID.int(), Name: strings.Join(strings.Fields(r.Title.String()), " "), Number: r.Number.String(), Count: r.Total.int()}
}

type rawBoxEvents struct {
	Period string `json:"period"` // "1ST Period"
	Events []struct {
		Type     string `json:"eventType"` // "goal", "penalty"
		Time     loose  `json:"time"`
		Period   loose  `json:"period"`
		HomeTeam *bool  `json:"homeTeam"`
		Team     *struct {
			ID loose `json:"id"`
		} `json:"team"`

		Scorer  *rawBoxRef `json:"goalScorer"`
		Assist1 *rawBoxRef `json:"assist1By"`
		Assist2 *rawBoxRef `json:"assist2By"`
		PP      bool       `json:"powerPlay"`
		SH      bool       `json:"shg"`
		EN      bool       `json:"en"`
		GWG     bool       `json:"gameWinningGoal"`

		By      *rawBoxRef `json:"commitedBy"` // GameSheet's spelling
		Penalty *struct {
			Title    loose `json:"title"`
			Duration loose `json:"duration"`
		} `json:"penaltyType"`
	} `json:"periodEvents"`
}

type rawBoxPlayer struct {
	ID       loose  `json:"id"`
	First    string `json:"firstName"`
	Last     string `json:"lastName"`
	Number   loose  `json:"number"`
	Position string `json:"position"`
	Status   string `json:"status"` // "playing"; "scratched", "injured"…
	G        loose  `json:"g"`
	A        loose  `json:"a"`
	PTS      loose  `json:"pts"`
	PIM      loose  `json:"pim"`
	GA       loose  `json:"ga"`
	SA       loose  `json:"sa"`
}

type rawBoxSide struct {
	ID     loose `json:"id"`
	Roster struct {
		Players []rawBoxPlayer `json:"players"`
		Goalies []rawBoxPlayer `json:"goalies"`
	} `json:"roster"`
}

func parseBoxScore(b []byte) (BoxScore, error) {
	var body struct {
		// Some of GameSheet's endpoints wrap their answer in "data".
		Data    json.RawMessage `json:"data"`
		Home    *rawBoxSide     `json:"home"`
		Visitor *rawBoxSide     `json:"visitor"`
		Tables  *struct {
			Goals     []rawBoxEvents `json:"goalsByPeriod"`
			Penalties []rawBoxEvents `json:"penaltiesByPeriod"`
			Shots     *struct {
				Title []struct {
					ID loose `json:"id"`
				} `json:"title"`
				ByPeriod []struct {
					Period loose   `json:"period"`
					Data   []loose `json:"data"`
				} `json:"shotsByPeriod"`
			} `json:"shots"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return BoxScore{}, err
	}
	if body.Home == nil && len(body.Data) > 0 && body.Data[0] == '{' {
		return parseBoxScore(body.Data)
	}
	if body.Home == nil || body.Visitor == nil || body.Tables == nil {
		return BoxScore{}, errors.New("no teams or tables (the response changed shape)")
	}
	out := BoxScore{HomeID: body.Home.ID.int(), VisitorID: body.Visitor.ID.int()}
	if out.HomeID == 0 || out.VisitorID == 0 {
		return BoxScore{}, errors.New("a team has no ID (the response changed shape)")
	}
	out.Home, out.Visitor = body.Home.players(), body.Visitor.players()

	// Each event says whose it is: by team, or as "homeTeam".
	for _, kind := range []struct {
		name   string
		blocks []rawBoxEvents
	}{{"goal", body.Tables.Goals}, {"penalty", body.Tables.Penalties}} {
		for _, block := range kind.blocks {
			for _, ev := range block.Events {
				if ev.Type != kind.name {
					continue
				}
				period := boxPeriod(ev.Period.String())
				if period == "" {
					period = boxPeriod(block.Period)
				}
				var home bool
				switch id := 0; {
				case ev.Team != nil && ev.Team.ID.int() != 0:
					if id = ev.Team.ID.int(); id != out.HomeID && id != out.VisitorID {
						return BoxScore{}, fmt.Errorf("a %s by team %d, which isn't playing (the response changed shape)", kind.name, id)
					}
					home = id == out.HomeID
				case ev.HomeTeam != nil:
					home = *ev.HomeTeam
				default:
					return BoxScore{}, fmt.Errorf("a %s with no team (the response changed shape)", kind.name)
				}
				if period == "" || ev.Time == "" {
					return BoxScore{}, fmt.Errorf("a %s with no period or time (the response changed shape)", kind.name)
				}
				if kind.name == "goal" {
					g := BoxGoal{
						Period: period, Clock: ev.Time.String(), Home: home, Scorer: ev.Scorer.ref(),
						PP: ev.PP, SH: ev.SH, EN: ev.EN, GWG: ev.GWG,
					}
					for _, a := range []*rawBoxRef{ev.Assist1, ev.Assist2} {
						if ref := a.ref(); ref.Known() {
							g.Assist = append(g.Assist, ref)
						}
					}
					out.Goals = append(out.Goals, g)
					continue
				}
				p := BoxPenalty{Period: period, Clock: ev.Time.String(), Home: home, Player: ev.By.ref()}
				if ev.Penalty != nil {
					p.Infraction, p.Minutes = ev.Penalty.Title.String(), ev.Penalty.Duration.int()
				}
				out.Penalties = append(out.Penalties, p)
			}
		}
	}

	if sh := body.Tables.Shots; sh != nil && len(sh.ByPeriod) > 0 {
		// Two columns, in the order of "title": visitor first unless it says otherwise.
		v, h := 0, 1
		if len(sh.Title) == 2 && sh.Title[0].ID.int() == out.HomeID && sh.Title[1].ID.int() == out.VisitorID {
			v, h = 1, 0
		}
		for _, p := range sh.ByPeriod {
			period := boxPeriod(p.Period.String())
			if len(p.Data) != 2 || period == "" {
				return BoxScore{}, errors.New("shots by period aren't two teams' (the response changed shape)")
			}
			out.Shots = append(out.Shots, PeriodShots{Period: period, Home: p.Data[h].int(), Visitor: p.Data[v].int()})
		}
	}
	return out, nil
}

func (s *rawBoxSide) players() []BoxPlayer {
	var out []BoxPlayer
	seen := map[int]bool{}
	add := func(r rawBoxPlayer, goalie bool) {
		id := r.ID.int()
		if id == 0 || seen[id] || (r.Status != "" && r.Status != "playing") {
			return
		}
		seen[id] = true
		p := BoxPlayer{
			Player: Player{ID: id, First: r.First, Last: r.Last}, Number: r.Number.String(),
			G: r.G.int(), A: r.A.int(), PTS: r.PTS.int(), PIM: r.PIM.int(),
		}
		sa, hasSA := r.SA.Int()
		ga, hasGA := r.GA.Int()
		pos := strings.ToLower(r.Position)
		p.Goalie = goalie || pos == "g" || pos == "goalie" || pos == "goaltender" || hasSA
		if p.Goalie && hasSA && hasGA {
			p.Faced, p.GA, p.GoalieCounts = sa, ga, true
		}
		out = append(out, p)
	}
	for _, r := range s.Roster.Goalies {
		add(r, true)
	}
	for _, r := range s.Roster.Players {
		add(r, false)
	}
	return out
}

// boxPeriod names a period the way the games list does: "1" from "1" or
// "1ST Period", "OT1" from "OT1" or "OT1 Period", "SO" from "Shootout".
func boxPeriod(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimSpace(strings.TrimSuffix(s, "PERIOD"))
	switch {
	case s == "":
		return ""
	case strings.HasPrefix(s, "SO") || strings.HasPrefix(s, "SHOOTOUT"):
		return "SO"
	case strings.HasPrefix(s, "OVERTIME"):
		return "OT" + strings.TrimSpace(strings.TrimPrefix(s, "OVERTIME"))
	case strings.HasPrefix(s, "OT"):
		return s
	}
	// "1ST", "2ND", "3RD", "4TH" → the number.
	digits := strings.TrimRight(s, "STNDRH")
	if _, err := strconv.Atoi(digits); err == nil {
		return digits
	}
	return s
}
