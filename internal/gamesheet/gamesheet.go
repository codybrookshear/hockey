// Package gamesheet reads a league's public stats from GameSheet
// (gamesheetstats.com), the scoresheet app the RHL uses.
//
// GameSheet has no public API. Its stats site is a web app that loads JSON
// from gamesheetstats.com/api/..., unauthenticated, and that's what this
// reads. It's undocumented and has changed before (the endpoints for player
// stats moved), so responses are checked strictly: a changed shape is an
// error, not a quietly wrong table.
package gamesheet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://gamesheetstats.com"

const (
	maxResponse = 8 << 20 // a season's games are ~70 KB; a long one, a few hundred
	pageSize    = 500
	maxPages    = 10
)

// Season is a league's season (e.g. Fall 2026).
type Season struct {
	ID       int
	Title    string // "RHL - Adult Hockey League - Fall 2026"
	Name     string // "Fall 2026": Title without the league's
	LeagueID int
	League   string
	Start    string // YYYY-MM-DD, as entered (an end can precede its start)
	End      string
	Active   bool // GameSheet's "active", and not archived
	Public   bool
}

// Division is one division's standings, best first.
type Division struct {
	ID    int
	Title string
	Teams []Standing
}

// Standing is a team's line in its division, as GameSheet ranks it.
type Standing struct {
	Rank                   int
	Team                   Team
	GP, W, L, T, OTL       int
	PTS, GF, GA, Diff      int
	PIM                    int
	Streak                 string // "Won 2"
	Reason                 string // the tiebreaker that decided the rank: "Points", "Goals Against"…
	HomeRecord, AwayRecord string // W-L-OTL-SOL
}

type Team struct {
	ID    int
	Title string
	Abbr  string
	Logo  string // an image on GameSheet's CDN (imagedelivery.net); may be empty
}

// Game is one game, scheduled, live or final.
type Game struct {
	ID       int
	Number   string
	Start    time.Time
	Status   string // "scheduled", "in_progress", "final"
	Type     string // "regular_season", "playoff", "exhibition"
	Location string
	Division string // the home team's
	Home     Side
	Visitor  Side
}

func (g Game) Final() bool { return g.Status == "final" }
func (g Game) Live() bool  { return g.Status == "in_progress" }

// Side is one team in a game.
type Side struct {
	Team
	Goals   int
	Result  string // "W", "L", or "" (tie, or not final)
	Record  string // overall W-L-OTL-SOL, as of now
	Scorers []Goal
}

// TBD reports a playoff slot not yet filled.
func (s Side) TBD() bool { return s.ID == 0 }

type Goal struct {
	Period string // "1", "2", "3", "OT"…
	Clock  string // time left in the period, "03:03"
	Player Player
}

type Player struct {
	ID          int
	First, Last string // as entered; often all capitals
}

// Client reads one GameSheet site.
type Client struct {
	HTTP      *http.Client
	BaseURL   string // DefaultBaseURL
	UserAgent string
}

func (c *Client) Season(ctx context.Context, id int) (Season, error) {
	var body struct {
		Status string `json:"status"`
		Data   []struct {
			ID       int    `json:"id"`
			Title    string `json:"title"`
			Start    string `json:"start"`
			End      string `json:"end"`
			Active   bool   `json:"is_active"`
			Archived bool   `json:"archived"`
			Public   bool   `json:"isPublic"`
			LeagueID int    `json:"leagueId"`
			League   struct {
				Title string `json:"title"`
			} `json:"league"`
		} `json:"data"`
	}
	if err := c.get(ctx, "season-info/"+strconv.Itoa(id), nil, &body); err != nil {
		return Season{}, err
	}
	if body.Status != "success" || len(body.Data) != 1 || body.Data[0].ID != id || body.Data[0].Title == "" {
		return Season{}, fmt.Errorf("season %d: unexpected season-info response", id)
	}
	d := body.Data[0]
	return Season{
		ID: d.ID, Title: d.Title, Name: seasonName(d.Title, d.League.Title),
		LeagueID: d.LeagueID, League: d.League.Title,
		Start: d.Start, End: d.End, Active: d.Active && !d.Archived, Public: d.Public,
	}, nil
}

// seasonName drops the league's name from a season's title:
// "RHL - Adult Hockey League - Fall 2026" → "Fall 2026".
func seasonName(title, league string) string {
	if league != "" {
		if rest, ok := strings.CutPrefix(title, league); ok {
			if rest = strings.TrimLeft(rest, " -–:"); rest != "" {
				return rest
			}
		}
	}
	return title
}

func (c *Client) Standings(ctx context.Context, season int) ([]Division, error) {
	var body struct {
		Status string `json:"status"`
		Data   []struct {
			DivisionID int `json:"divisionId"`
			Standings  []struct {
				Division struct {
					ID    int    `json:"id"`
					Title string `json:"title"`
				} `json:"division"`
				Rank   int    `json:"rank"`
				Reason string `json:"reason"`
				// Keys are GameSheet's abbreviations: GP, W, L, T, OTL, PTS…
				Stats struct {
					GP, W, L, T, OTL  *int
					PTS, GF, GA, DIFF *int
					PIM               int
					STK, HREC, VREC   string
				} `json:"stats"`
				Team struct {
					ID    int    `json:"id"`
					Title string `json:"title"`
					Abbr  string `json:"abbreviation"`
					Logo  string `json:"logoUrl"`
				} `json:"team"`
			} `json:"standings"`
		} `json:"data"`
	}
	if err := c.get(ctx, "standings/"+strconv.Itoa(season), nil, &body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("standings: status %q", body.Status)
	}
	divs := []Division{}
	for _, d := range body.Data {
		div := Division{ID: d.DivisionID}
		for _, s := range d.Standings {
			st := s.Stats
			for _, p := range []*int{st.GP, st.W, st.L, st.T, st.OTL, st.PTS, st.GF, st.GA, st.DIFF} {
				if p == nil {
					return nil, fmt.Errorf("standings: %q is missing a stat (the response changed shape)", s.Team.Title)
				}
			}
			if s.Team.ID == 0 || s.Team.Title == "" || s.Rank < 1 || s.Division.ID != d.DivisionID {
				return nil, errors.New("standings: unexpected team entry (the response changed shape)")
			}
			div.Title = s.Division.Title
			div.Teams = append(div.Teams, Standing{
				Rank: s.Rank,
				Team: Team{ID: s.Team.ID, Title: s.Team.Title, Abbr: s.Team.Abbr, Logo: s.Team.Logo},
				GP:   *st.GP, W: *st.W, L: *st.L, T: *st.T, OTL: *st.OTL,
				PTS: *st.PTS, GF: *st.GF, GA: *st.GA, Diff: *st.DIFF, PIM: st.PIM,
				Streak: st.STK, Reason: s.Reason, HomeRecord: st.HREC, AwayRecord: st.VREC,
			})
		}
		if len(div.Teams) == 0 {
			continue
		}
		// The response isn't always in rank order.
		slices.SortStableFunc(div.Teams, func(a, b Standing) int { return a.Rank - b.Rank })
		divs = append(divs, div)
	}
	return divs, nil
}

func (c *Client) Games(ctx context.Context, season int) ([]Game, error) {
	games := []Game{}
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("games: more than %d pages", maxPages)
		}
		var body struct {
			Data []rawGame `json:"data"`
			Meta struct {
				Total *int `json:"total"`
			} `json:"meta"`
		}
		q := url.Values{
			"order":  {"asc"},
			"limit":  {strconv.Itoa(pageSize)},
			"offset": {strconv.Itoa(len(games))},
		}
		if err := c.get(ctx, "unified-games/"+strconv.Itoa(season), q, &body); err != nil {
			return nil, err
		}
		if body.Meta.Total == nil {
			return nil, errors.New("games: no total (the response changed shape)")
		}
		for _, r := range body.Data {
			g, err := r.game()
			if err != nil {
				return nil, err
			}
			games = append(games, g)
		}
		if len(body.Data) == 0 || len(games) >= *body.Meta.Total {
			return games, nil
		}
	}
}

type rawGame struct {
	GameID   int    `json:"gameId"`
	Number   string `json:"number"`
	Status   string `json:"status"`
	GameType string `json:"gameType"`
	Location string `json:"location"`
	Start    string `json:"timeStampZulu"`
	Home     rawSide
	Visitor  rawSide
}

type rawSide struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	Abbr     string `json:"abbr"`
	Logo     string `json:"logo"`
	Goals    *int   `json:"goals"`
	Result   string `json:"result"`
	Record   string `json:"overallRecord"`
	Division struct {
		Title string `json:"title"`
	} `json:"division"`
	GoalDetails []struct {
		ID        int    `json:"id"`
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
		Period    string `json:"period"`
		Clock     string `json:"clockTime"`
	} `json:"goalDetails"`
}

func (r rawGame) game() (Game, error) {
	start, err := time.Parse(time.RFC3339, r.Start)
	if err != nil || r.GameID == 0 || r.Status == "" || r.Home.Goals == nil || r.Visitor.Goals == nil {
		return Game{}, fmt.Errorf("games: unexpected game entry %d (the response changed shape)", r.GameID)
	}
	return Game{
		ID: r.GameID, Number: r.Number, Start: start, Status: r.Status, Type: r.GameType,
		Location: r.Location, Division: r.Home.Division.Title,
		Home: r.Home.side(), Visitor: r.Visitor.side(),
	}, nil
}

func (r rawSide) side() Side {
	s := Side{
		Team:   Team{ID: r.ID, Title: r.Title, Abbr: r.Abbr, Logo: r.Logo},
		Goals:  *r.Goals,
		Result: r.Result,
		Record: r.Record,
	}
	for _, g := range r.GoalDetails {
		s.Scorers = append(s.Scorers, Goal{
			Period: g.Period, Clock: g.Clock,
			Player: Player{ID: g.ID, First: g.FirstName, Last: g.LastName},
		})
	}
	return s
}

func (c *Client) get(ctx context.Context, path string, q url.Values, v any) error {
	u := c.BaseURL + "/api/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("gamesheet %s: %w", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("gamesheet %s: HTTP %d", path, res.StatusCode)
	}
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt != "application/json" {
		return fmt.Errorf("gamesheet %s: unexpected content type %q", path, mt)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("gamesheet %s: %w", path, err)
	}
	if len(b) > maxResponse {
		return fmt.Errorf("gamesheet %s: response is unexpectedly large", path)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("gamesheet %s: %w", path, err)
	}
	return nil
}
