package gamesheet

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The box score here is not a saved response: this was written where
// GameSheet's API couldn't be reached, so it's put together from other
// people's notes on the endpoint's shape, for a game in the saved games
// (2986401: Pirates 9 at Orange 7). Replace it with a saved one:
//
//	curl -H 'Accept: application/json' \
//	  https://gamesheetstats.com/api/useBoxScore/getGameStats/<season>/games/<game>
func reconstructedBoxScore(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/boxscore-2986401-reconstructed.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBoxScore(t *testing.T) {
	const path = "/api/useBoxScore/getGameStats/15331/games/2986401"
	c, paths := fakeGameSheet(t, map[string]string{path: reconstructedBoxScore(t)})
	b, err := c.BoxScore(context.Background(), 15331, 2986401)
	if err != nil {
		t.Fatal(err)
	}
	if len(*paths) != 1 || (*paths)[0] != path {
		t.Errorf("requested %v", *paths)
	}
	if b.HomeID != 554812 || b.VisitorID != 554805 {
		t.Errorf("teams %d at %d", b.VisitorID, b.HomeID)
	}
	if len(b.Goals) != 16 || len(b.Penalties) != 3 {
		t.Fatalf("%d goals, %d penalties", len(b.Goals), len(b.Penalties))
	}
	// The visitors' first: Dusome from Funk and Katz, 03:03 left in the 1st.
	var first BoxGoal
	for _, g := range b.Goals {
		if !g.Home && g.Period == "1" && g.Clock == "03:03" {
			first = g
		}
	}
	if first.Scorer.Name != "DEREK DUSOME" || first.Scorer.ID != 8471640 || first.Scorer.Count != 1 ||
		len(first.Assist) != 2 || first.Assist[0].Name != "ROBERT FUNK" || first.Assist[1].Name != "LEO KATZ" {
		t.Errorf("first goal: %+v", first)
	}
	var pp, sh, en, gwg, unassisted int
	for _, g := range b.Goals {
		for flag, n := range map[bool]*int{g.PP: &pp, g.SH: &sh, g.EN: &en, g.GWG: &gwg} {
			if flag {
				*n++
			}
		}
		if len(g.Assist) == 0 {
			unassisted++
		}
	}
	if pp != 1 || sh != 1 || en != 1 || gwg != 1 || unassisted != 14 {
		t.Errorf("PP %d, SH %d, EN %d, GWG %d, unassisted %d", pp, sh, en, gwg, unassisted)
	}
	if p := b.Penalties[0]; !p.Home || p.Period != "1" || p.Clock != "06:12" || p.Player.Name != "JUSTIN BRISTOL" ||
		p.Infraction != "Tripping - Minor" || p.Minutes != 2 {
		t.Errorf("first penalty: %+v", p)
	}
	if p := b.Penalties[2]; p.Home || p.Player.Known() { // a bench minor
		t.Errorf("bench penalty: %+v", p)
	}
	if len(b.Shots) != 3 || b.Shots[0] != (PeriodShots{Period: "1", Home: 12, Visitor: 11}) {
		t.Errorf("shots: %+v", b.Shots)
	}
	// Who dressed: the scratch is left out; the goalie has shots and goals against.
	if len(b.Visitor) != 4 || len(b.Home) != 6 {
		t.Fatalf("%d visitors, %d home", len(b.Visitor), len(b.Home))
	}
	var goalie, dusome BoxPlayer
	for _, p := range b.Visitor {
		switch {
		case p.Goalie:
			goalie = p
		case p.ID == 8471640:
			dusome = p
		}
	}
	if !goalie.GoalieCounts || goalie.Faced != 41 || goalie.GA != 7 || goalie.Number != "1" {
		t.Errorf("goalie: %+v", goalie)
	}
	if dusome.G != 5 || dusome.PTS != 5 || dusome.Last != "DUSOME" {
		t.Errorf("Dusome: %+v", dusome)
	}
}

func TestBoxScoreShapes(t *testing.T) {
	// Wrapped in "data", shots listed home first, numbers as strings.
	wrapped := `{"status":"success","data":{"home":{"id":"2"},"visitor":{"id":1},"tables":{
		"goalsByPeriod":[{"period":"OT1 Period","periodEvents":[{"eventType":"goal","time":"01:00","homeTeam":true,
			"goalScorer":{"id":7,"title":"","number":9}}]}],
		"shots":{"title":[{"id":2},{"id":1}],"shotsByPeriod":[{"period":"1ST Period","data":["5",3]}]}}}}`
	b, err := parseBoxScore([]byte(wrapped))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Goals) != 1 || !b.Goals[0].Home || b.Goals[0].Period != "OT1" || b.Goals[0].Scorer.Number != "9" || len(b.Goals[0].Assist) != 0 {
		t.Errorf("goal: %+v", b.Goals)
	}
	if len(b.Shots) != 1 || b.Shots[0] != (PeriodShots{Period: "1", Home: 5, Visitor: 3}) {
		t.Errorf("shots: %+v", b.Shots)
	}
	// A game not started: teams, empty tables.
	if b, err := parseBoxScore([]byte(`{"home":{"id":2},"visitor":{"id":1},"tables":{}}`)); err != nil || len(b.Goals) != 0 {
		t.Errorf("empty: %+v, %v", b, err)
	}
	for name, body := range map[string]string{
		"no tables":            `{"home":{"id":2},"visitor":{"id":1}}`,
		"no team IDs":          `{"home":{},"visitor":{},"tables":{}}`,
		"another shape":        `{"status":"success","data":[]}`,
		"a goal by a stranger": `{"home":{"id":2},"visitor":{"id":1},"tables":{"goalsByPeriod":[{"period":"1","periodEvents":[{"eventType":"goal","time":"01:00","team":{"id":3}}]}]}}`,
		"a goal without time":  `{"home":{"id":2},"visitor":{"id":1},"tables":{"goalsByPeriod":[{"period":"1","periodEvents":[{"eventType":"goal","homeTeam":true}]}]}}`,
		"shots for one team":   `{"home":{"id":2},"visitor":{"id":1},"tables":{"shots":{"shotsByPeriod":[{"period":"1","data":[4]}]}}}`,
	} {
		if _, err := parseBoxScore([]byte(body)); err == nil || (name != "another shape" && !strings.Contains(err.Error(), "changed shape")) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestGameSides(t *testing.T) {
	c, _ := fakeGameSheet(t, nil)
	games, err := c.Games(context.Background(), 15331)
	if err != nil {
		t.Fatal(err)
	}
	g := games[0] // Pirates 9 at Orange 7
	if g.ID != 2986401 || g.Visitor.Shots != 38 || g.Home.Shots != 41 {
		t.Fatalf("game %d: shots %d–%d", g.ID, g.Visitor.Shots, g.Home.Shots)
	}
	want := []PeriodGoals{{"1", 2}, {"2", 3}, {"3", 4}}
	if len(g.Visitor.Periods) != 3 || g.Visitor.Periods[0] != want[0] || g.Visitor.Periods[1] != want[1] || g.Visitor.Periods[2] != want[2] {
		t.Errorf("periods: %+v", g.Visitor.Periods)
	}
	for _, g := range games {
		if g.Status == "scheduled" && len(g.Home.Periods) != 0 {
			t.Errorf("scheduled game %d has periods", g.ID)
		}
	}
	if comparePeriods("3", "OT") >= 0 || comparePeriods("OT", "SO") >= 0 || comparePeriods("2", "10") >= 0 {
		t.Error("periods out of order")
	}
}
