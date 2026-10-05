package rhl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hockey/internal/gamesheet"
)

// fourDivisions is a made-up season with four divisions (next season's
// plan) of three teams each: team d0, d1, d2 in division d.
type fourDivisions struct{}

var winter = gamesheet.Season{ID: 1, Title: "RHL - Adult Hockey League - Winter 2027", Name: "Winter 2027",
	LeagueID: 620287, Public: true, Active: true, Start: "2026-09-01"}

func (fourDivisions) Season(context.Context, int) (gamesheet.Season, error) { return winter, nil }
func (fourDivisions) LeagueSeasons(context.Context, int) ([]gamesheet.Season, error) {
	return []gamesheet.Season{winter}, nil
}

func team(div, i int) gamesheet.Team {
	return gamesheet.Team{ID: div*10 + i, Title: fmt.Sprintf("Team %c%d", 'A'+div-1, i), Abbr: fmt.Sprintf("%c%d", 'A'+div-1, i)}
}

func (fourDivisions) Standings(context.Context, int) ([]gamesheet.Division, error) {
	var out []gamesheet.Division
	for d := 1; d <= 4; d++ {
		div := gamesheet.Division{ID: d, Title: fmt.Sprintf("%c Division", 'A'+d-1)}
		for i := range 3 {
			div.Teams = append(div.Teams, gamesheet.Standing{Rank: i + 1, Team: team(d, i), GP: 1})
		}
		out = append(out, div)
	}
	return out, nil
}

func (fourDivisions) Games(context.Context, int) ([]gamesheet.Game, error) {
	var out []gamesheet.Game
	for d := 1; d <= 4; d++ {
		side := func(i, goals int) gamesheet.Side {
			return gamesheet.Side{Team: team(d, i), Goals: goals, DivisionID: d}
		}
		out = append(out,
			gamesheet.Game{ID: d, Status: "final", Type: "regular_season", Start: time.Date(2026, 9, 6, 17, 0, 0, 0, time.UTC),
				Division: fmt.Sprintf("%c Division", 'A'+d-1), DivisionID: d, Home: side(0, 3), Visitor: side(1, 2)},
			gamesheet.Game{ID: 10 + d, Status: "scheduled", Type: "regular_season", Start: time.Date(2026, 10, 11, 17, 0, 0, 0, time.UTC),
				Division: fmt.Sprintf("%c Division", 'A'+d-1), DivisionID: d, Home: side(2, 0), Visitor: side(0, 0)})
	}
	return out, nil
}

func (fourDivisions) Skaters(context.Context, int) ([]gamesheet.Skater, error) {
	var out []gamesheet.Skater
	for d := 1; d <= 4; d++ {
		t := team(d, 0)
		line := gamesheet.SkaterLine{GP: 1, G: d, PTS: d}
		out = append(out, gamesheet.Skater{
			Player:     gamesheet.Player{ID: d, First: "Player", Last: t.Abbr},
			SkaterLine: line, Teams: []gamesheet.SkaterTeam{{Team: t, SkaterLine: line}},
		})
	}
	return out, nil
}

func (fourDivisions) Goalies(context.Context, int) ([]gamesheet.Goalie, error) {
	var out []gamesheet.Goalie
	for d := 1; d <= 4; d++ {
		t := team(d, 1)
		line := gamesheet.GoalieLine{GP: 1, W: 1, SVPct: .9}
		out = append(out, gamesheet.Goalie{
			Player:     gamesheet.Player{ID: 100 + d, First: "Goalie", Last: t.Abbr},
			GoalieLine: line, Teams: []gamesheet.GoalieTeam{{Team: t, GoalieLine: line}},
		})
	}
	return out, nil
}

func TestFourDivisions(t *testing.T) {
	h := newServer(t, Config{Source: fourDivisions{}, League: 620287})
	b := get(t, h, "/").Body.String()
	for _, want := range []string{
		`<nav class="divisions many" aria-label="Division">`,
		`<a href="/?division=1" aria-current="page">A</a>`, // "A Division", shortened
		`<a href="/?division=2">B</a>`, `<a href="/?division=3">C</a>`, `<a href="/?division=4">D</a>`,
		">Team A0</a></td>", "Player A0", "Goalie A1",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	if strings.Contains(b, "Team C0") {
		t.Error("division A's page shows division C")
	}

	w := get(t, h, "/?division=3")
	b = w.Body.String()
	if !strings.Contains(b, `<a href="/?division=3" aria-current="page">C</a>`) ||
		!strings.Contains(b, ">Team C0</a></td>") || !strings.Contains(b, "Player C0") || !strings.Contains(b, "Goalie C1") ||
		strings.Contains(b, "Team A0") || strings.Contains(b, "Player A0") || strings.Contains(b, "Goalie A1") {
		t.Errorf("division C's page:\n%s", b)
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].Value != "3" {
		t.Errorf("cookie: %+v", c)
	}
	// Last season's division, remembered: falls back to the first.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptestRequest("/", "82590"))
	if !strings.Contains(w2.Body.String(), `aria-current="page">A</a>`) {
		t.Error("a stale division cookie didn't fall back to the first division")
	}
}

func httptestRequest(target, division string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, target, nil)
	r.AddCookie(&http.Cookie{Name: "division", Value: division})
	return r
}
