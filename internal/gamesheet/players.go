package gamesheet

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// Skater is a player's season: overall, and for each team they've played
// for. Subs show up under more than one team; teams from other seasons can
// appear too, with no games.
type Skater struct {
	Player
	Jersey string
	SkaterLine
	Teams []SkaterTeam
}

type SkaterTeam struct {
	Team
	SkaterLine
}

type SkaterLine struct {
	GP, G, A, PTS, PIM int
	PPG, SHG, GWG      int
}

// Goalie is a goalie's season, like Skater.
type Goalie struct {
	Player
	Jersey string
	GoalieLine
	Teams []GoalieTeam
}

type GoalieTeam struct {
	Team
	GoalieLine
}

type GoalieLine struct {
	GP, W, L, T, OTL, SO int
	GA, SA               int
	GAA, SVPct           float64 // 0 with no games
}

const (
	playerPageSize = 100 // a season has a few hundred players
	maxPlayerPages = 20
)

// Skaters returns every skater in a season, as GameSheet sorts them (points).
func (c *Client) Skaters(ctx context.Context, season int) ([]Skater, error) {
	var out []Skater
	err := c.pages(ctx, "players/standings/"+strconv.Itoa(season), func(page []byte) (int, error) {
		var rows []rawSkater
		if err := decodeData(page, &rows); err != nil {
			return 0, err
		}
		for _, r := range rows {
			s, err := r.skater()
			if err != nil {
				return 0, err
			}
			out = append(out, s)
		}
		return len(rows), nil
	})
	return out, err
}

// Goalies returns every goalie in a season, games played or not.
func (c *Client) Goalies(ctx context.Context, season int) ([]Goalie, error) {
	var out []Goalie
	err := c.pages(ctx, "goalies/standings/"+strconv.Itoa(season), func(page []byte) (int, error) {
		var rows []rawGoalie
		if err := decodeData(page, &rows); err != nil {
			return 0, err
		}
		for _, r := range rows {
			g, err := r.goalie()
			if err != nil {
				return 0, err
			}
			out = append(out, g)
		}
		return len(rows), nil
	})
	return out, err
}

// Roster is a team's players and goalies, as signed up for the season:
// before its first game too, when the season's stats list nobody for it.
// Each one's Teams has their line for this team.
type Roster struct {
	Skaters []Skater
	Goalies []Goalie
}

// Roster returns a team's roster in a season. Not in the stats site's
// network traffic (its team pages are rendered server-side); found in its
// JavaScript.
func (c *Client) Roster(ctx context.Context, season, team int) (Roster, error) {
	var body struct {
		Players struct {
			Status string      `json:"status"`
			Data   []rawSkater `json:"data"`
		} `json:"players"`
		Goalies struct {
			Status string      `json:"status"`
			Data   []rawGoalie `json:"data"`
		} `json:"goalies"`
	}
	path := "teams/" + strconv.Itoa(season) + "/" + strconv.Itoa(team) + "/roster"
	if err := c.get(ctx, path, nil, &body); err != nil {
		return Roster{}, err
	}
	if body.Players.Status != "success" || body.Goalies.Status != "success" {
		return Roster{}, fmt.Errorf("roster: status %q/%q", body.Players.Status, body.Goalies.Status)
	}
	var out Roster
	for _, r := range body.Players.Data {
		s, err := r.skater()
		if err != nil {
			return Roster{}, err
		}
		out.Skaters = append(out.Skaters, s)
	}
	for _, r := range body.Goalies.Data {
		g, err := r.goalie()
		if err != nil {
			return Roster{}, err
		}
		out.Goalies = append(out.Goalies, g)
	}
	return out, nil
}

type rawSkaterLine struct {
	GP, G, A, PTS, PIM *int
	PPG, SHG, GWG      int
}

type rawSkater struct {
	ID        int           `json:"id"`
	FirstName string        `json:"firstName"`
	LastName  string        `json:"lastName"`
	Jersey    string        `json:"jersey"`
	Stats     rawSkaterLine `json:"stats"`
	Teams     []struct {
		ID    int           `json:"id"`
		Title string        `json:"title"`
		Abbr  string        `json:"abbreviation"`
		Logo  string        `json:"logo"`
		Stats rawSkaterLine `json:"stats"`
	} `json:"teams"`
}

func (l rawSkaterLine) line(who string) (SkaterLine, error) {
	for _, p := range []*int{l.GP, l.G, l.A, l.PTS, l.PIM} {
		if p == nil {
			return SkaterLine{}, fmt.Errorf("skaters: %s is missing a stat (the response changed shape)", who)
		}
	}
	return SkaterLine{GP: *l.GP, G: *l.G, A: *l.A, PTS: *l.PTS, PIM: *l.PIM, PPG: l.PPG, SHG: l.SHG, GWG: l.GWG}, nil
}

func (r rawSkater) skater() (Skater, error) {
	if r.ID == 0 {
		return Skater{}, errors.New("skaters: a player without an ID (the response changed shape)")
	}
	s := Skater{Player: Player{ID: r.ID, First: r.FirstName, Last: r.LastName}, Jersey: r.Jersey}
	var err error
	if s.SkaterLine, err = r.Stats.line(r.LastName); err != nil {
		return Skater{}, err
	}
	for _, t := range r.Teams {
		st := SkaterTeam{Team: Team{ID: t.ID, Title: t.Title, Abbr: t.Abbr, Logo: t.Logo}}
		if st.SkaterLine, err = t.Stats.line(r.LastName); err != nil {
			return Skater{}, err
		}
		s.Teams = append(s.Teams, st)
	}
	return s, nil
}

type rawGoalieLine struct {
	GP    *int     `json:"gp"`
	W     *int     `json:"wins"`
	L     *int     `json:"losses"`
	T     *int     `json:"ties"`
	OTL   int      `json:"otl"`
	SO    int      `json:"so"`
	GA    *int     `json:"ga"`
	SA    int      `json:"sa"`
	GAA   *float64 `json:"gaa"` // null with no games
	SVPct float64  `json:"svpct"`
}

type rawGoalie struct {
	ID        int           `json:"id"`
	FirstName string        `json:"firstName"`
	LastName  string        `json:"lastName"`
	Jersey    string        `json:"jersey"`
	Stats     rawGoalieLine `json:"stats"`
	Teams     []struct {
		ID    int           `json:"id"`
		Title string        `json:"title"`
		Abbr  string        `json:"abbreviation"`
		Logo  string        `json:"logo"`
		Stats rawGoalieLine `json:"stats"`
	} `json:"teams"`
}

func (l rawGoalieLine) line(who string) (GoalieLine, error) {
	for _, p := range []*int{l.GP, l.W, l.L, l.T, l.GA} {
		if p == nil {
			return GoalieLine{}, fmt.Errorf("goalies: %s is missing a stat (the response changed shape)", who)
		}
	}
	g := GoalieLine{GP: *l.GP, W: *l.W, L: *l.L, T: *l.T, OTL: l.OTL, SO: l.SO, GA: *l.GA, SA: l.SA, SVPct: l.SVPct}
	if l.GAA != nil {
		g.GAA = *l.GAA
	}
	return g, nil
}

func (r rawGoalie) goalie() (Goalie, error) {
	if r.ID == 0 {
		return Goalie{}, errors.New("goalies: a goalie without an ID (the response changed shape)")
	}
	g := Goalie{Player: Player{ID: r.ID, First: r.FirstName, Last: r.LastName}, Jersey: r.Jersey}
	var err error
	if g.GoalieLine, err = r.Stats.line(r.LastName); err != nil {
		return Goalie{}, err
	}
	for _, t := range r.Teams {
		gt := GoalieTeam{Team: Team{ID: t.ID, Title: t.Title, Abbr: t.Abbr, Logo: t.Logo}}
		if gt.GoalieLine, err = t.Stats.line(r.LastName); err != nil {
			return Goalie{}, err
		}
		g.Teams = append(g.Teams, gt)
	}
	return g, nil
}

// LeagueSeasons returns a league's seasons.
func (c *Client) LeagueSeasons(ctx context.Context, league int) ([]Season, error) {
	var body struct {
		Status string      `json:"status"`
		Data   []rawSeason `json:"data"`
	}
	if err := c.get(ctx, "leagues/"+strconv.Itoa(league)+"/seasons", nil, &body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("league seasons: status %q", body.Status)
	}
	var out []Season
	for _, r := range body.Data {
		if r.ID == 0 || r.Title == "" || r.LeagueID != league {
			return nil, errors.New("league seasons: unexpected season entry (the response changed shape)")
		}
		out = append(out, r.season())
	}
	return out, nil
}

// pages fetches a paged list (limit/offset; no total given) until a short
// page. each decodes one page and returns how many rows it held.
func (c *Client) pages(ctx context.Context, path string, each func(page []byte) (int, error)) error {
	offset := 0
	for range maxPlayerPages {
		var page rawBody
		q := url.Values{"limit": {strconv.Itoa(playerPageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := c.get(ctx, path, q, &page); err != nil {
			return err
		}
		n, err := each(page)
		if err != nil {
			return err
		}
		if n < playerPageSize {
			return nil
		}
		offset += n
	}
	return fmt.Errorf("%s: more than %d pages", path, maxPlayerPages)
}
