package gamesheet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeGameSheet serves the saved responses (testdata/, from 2026-10-04) at
// GameSheet's paths.
func fakeGameSheet(t *testing.T, override map[string]string) (*Client, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.Header.Get("User-Agent") != "test-agent" || r.Method != http.MethodGet {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		name := map[string]string{
			"/api/season-info/15331":   "season-info-15331.json",
			"/api/season-info/10562":   "season-info-10562.json",
			"/api/season-info/10561":   "season-info-10561.json",
			"/api/standings/15331":     "standings-15331.json",
			"/api/unified-games/15331": "unified-games-15331.json",
		}[r.URL.Path]
		if body, ok := override[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Write([]byte(body))
			return
		}
		if name == "" {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), BaseURL: srv.URL, UserAgent: "test-agent"}, &paths
}

func TestSeason(t *testing.T) {
	c, _ := fakeGameSheet(t, nil)
	s, err := c.Season(context.Background(), 15331)
	if err != nil {
		t.Fatal(err)
	}
	want := Season{
		ID: 15331, Title: "RHL - Adult Hockey League - Fall 2026", Name: "Fall 2026",
		LeagueID: 620287, League: "RHL - Adult Hockey League",
		Start: "2026-09-13", End: "2026-11-30", Active: true, Public: true,
	}
	if s != want {
		t.Errorf("got  %+v\nwant %+v", s, want)
	}
	if w, _ := c.Season(context.Background(), 10562); w.Name != "Winter 2025/26" || w.Active {
		t.Errorf("winter (archived): %+v", w)
	}
	if _, err := c.Season(context.Background(), 99); err == nil {
		t.Error("unknown season: no error")
	}
}

func TestStandings(t *testing.T) {
	c, _ := fakeGameSheet(t, nil)
	divs, err := c.Standings(context.Background(), 15331)
	if err != nil {
		t.Fatal(err)
	}
	if len(divs) != 2 {
		t.Fatalf("%d divisions, want 2", len(divs))
	}
	byTitle := map[string]Division{}
	for _, d := range divs {
		byTitle[d.Title] = d
		for i, s := range d.Teams {
			if s.Rank != i+1 {
				t.Errorf("%s: position %d has rank %d (not sorted)", d.Title, i+1, s.Rank)
			}
		}
	}
	ab := byTitle["A/B Division"]
	if len(ab.Teams) != 8 || ab.Teams[0].Team.Title != "Yellow Jackets" || ab.Teams[2].Team.Title != "Renegades" {
		t.Errorf("A/B: %+v", ab.Teams)
	}
	ice := byTitle["B/C Division"].Teams[0]
	if ice.Team.Title != "Ice Dogs" || ice.GP != 3 || ice.W != 2 || ice.T != 1 || ice.PTS != 5 ||
		ice.GF != 14 || ice.GA != 8 || ice.Diff != 6 || ice.Streak != "Won 1" || ice.Reason != "Points" ||
		!strings.HasPrefix(ice.Team.Logo, "https://imagedelivery.net/") {
		t.Errorf("Ice Dogs: %+v", ice)
	}
}

func TestStandingsRefusesChangedShape(t *testing.T) {
	for name, body := range map[string]string{
		"error status":  `{"status":"error","data":[]}`,
		"missing stat":  `{"status":"success","data":[{"divisionId":1,"standings":[{"division":{"id":1,"title":"A"},"rank":1,"stats":{"GP":1},"team":{"id":2,"title":"X"}}]}]}`,
		"no team":       `{"status":"success","data":[{"divisionId":1,"standings":[{"division":{"id":1,"title":"A"},"rank":1,"stats":{"GP":1,"W":1,"L":0,"T":0,"OTL":0,"PTS":2,"GF":1,"GA":0,"DIFF":1},"team":{}}]}]}`,
		"not json":      `<html>`,
		"wrong mistake": `{"status":"success","data":[{"divisionId":1,"standings":[{"division":{"id":9,"title":"A"},"rank":1,"stats":{"GP":1,"W":1,"L":0,"T":0,"OTL":0,"PTS":2,"GF":1,"GA":0,"DIFF":1},"team":{"id":2,"title":"X"}}]}]}`,
	} {
		c, _ := fakeGameSheet(t, map[string]string{"/api/standings/15331": body})
		if _, err := c.Standings(context.Background(), 15331); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGames(t *testing.T) {
	c, paths := fakeGameSheet(t, nil)
	games, err := c.Games(context.Background(), 15331)
	if err != nil {
		t.Fatal(err)
	}
	if len(games) != 56 {
		t.Fatalf("%d games, want 56", len(games))
	}
	if len(*paths) != 1 || !strings.Contains((*paths)[0], "limit=500") || !strings.Contains((*paths)[0], "offset=0") {
		t.Errorf("requests: %v", *paths)
	}
	g := games[0]
	if g.ID != 2986401 || !g.Final() || g.Visitor.Title != "Pirates" || g.Visitor.Goals != 9 || g.Visitor.Result != "W" ||
		g.Home.Title != "Orange" || g.Home.Goals != 7 || g.Division != "A/B Division" ||
		!g.Start.Equal(time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("first game: %+v", g)
	}
	if len(g.Visitor.Scorers) != 9 || g.Visitor.Scorers[0].Player.Last != "DUSOME" || g.Visitor.Scorers[0].Period != "1" {
		t.Errorf("scorers: %+v", g.Visitor.Scorers)
	}
	var live, tbd, finals int
	for _, g := range games {
		if g.Live() {
			live++
		}
		if g.Home.TBD() && g.Visitor.TBD() {
			tbd++
		}
		if g.Final() {
			finals++
			// Every goal on the scoresheet has a scorer.
			if len(g.Home.Scorers) != g.Home.Goals || len(g.Visitor.Scorers) != g.Visitor.Goals {
				t.Errorf("game %d: goals and scorers differ", g.ID)
			}
		}
	}
	if live != 1 || tbd != 21 || finals != 22 {
		t.Errorf("live=%d tbd=%d finals=%d", live, tbd, finals)
	}
}

func TestGamesPages(t *testing.T) {
	var offsets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		off := r.URL.Query().Get("offset")
		offsets = append(offsets, off)
		w.Header().Set("Content-Type", "application/json")
		game := `{"gameId":%d,"status":"final","gameType":"regular_season","timeStampZulu":"2026-09-13T16:00:00Z","home":{"id":1,"title":"A","goals":1},"visitor":{"id":2,"title":"B","goals":0}}`
		switch off {
		case "0":
			w.Write([]byte(`{"data":[` + strings.ReplaceAll(game, "%d", "1") + `,` + strings.ReplaceAll(game, "%d", "2") + `],"meta":{"total":3}}`))
		default:
			w.Write([]byte(`{"data":[` + strings.ReplaceAll(game, "%d", "3") + `],"meta":{"total":3}}`))
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	games, err := c.Games(context.Background(), 1)
	if err != nil || len(games) != 3 || strings.Join(offsets, ",") != "0,2" {
		t.Errorf("games=%d err=%v offsets=%v", len(games), err, offsets)
	}
}

func TestGamesRefusesChangedShape(t *testing.T) {
	for name, body := range map[string]string{
		"no meta":   `{"data":[]}`,
		"no goals":  `{"data":[{"gameId":1,"status":"final","timeStampZulu":"2026-09-13T16:00:00Z","home":{"id":1},"visitor":{"id":2}}],"meta":{"total":1}}`,
		"bad start": `{"data":[{"gameId":1,"status":"final","timeStampZulu":"Sep 13","home":{"id":1,"goals":0},"visitor":{"id":2,"goals":0}}],"meta":{"total":1}}`,
	} {
		c, _ := fakeGameSheet(t, map[string]string{"/api/unified-games/15331": body})
		if _, err := c.Games(context.Background(), 15331); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestClientRefusesBadResponses(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"403 challenge": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "Just a moment...", http.StatusForbidden) },
		"html": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<!DOCTYPE html>`))
		},
		"huge": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"success","data":"` + strings.Repeat("x", maxResponse) + `"}`))
		},
	} {
		srv := httptest.NewServer(h)
		c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
		if _, err := c.Season(context.Background(), 1); err == nil {
			t.Errorf("%s: accepted", name)
		}
		srv.Close()
	}
}

func TestSeasonLinks(t *testing.T) {
	page, err := os.ReadFile("testdata/rink-standings-page.html")
	if err != nil {
		t.Fatal(err)
	}
	got := SeasonLinks(page)
	if len(got) != 3 || got[0] != 15331 || got[1] != 10562 || got[2] != 10561 {
		t.Errorf("got %v, want [15331 10562 10561]", got)
	}
	if got := SeasonLinks([]byte(`<a href="https://gamesheetstats.com/seasons/7/standings">x</a> https://evil.example/seasons/8`)); len(got) != 1 || got[0] != 7 {
		t.Errorf("other domains: %v", got)
	}
}

func TestCurrent(t *testing.T) {
	const rhl = 620287
	fall := Season{ID: 15331, LeagueID: rhl, Public: true, Start: "2026-09-13", End: "2026-11-30", Active: true}
	// As entered in GameSheet: ends before it starts.
	winter := Season{ID: 10562, LeagueID: rhl, Public: true, Start: "2025-11-02", End: "2025-03-22"}
	spring := Season{ID: 10561, LeagueID: rhl, Public: true, Start: "2026-04-12", End: "2026-05-31"}
	nextWinter := Season{ID: 20000, LeagueID: rhl, Public: true, Start: "2027-01-03", End: "2027-03-20"}
	other := Season{ID: 1, LeagueID: 5, Public: true, Start: "2026-10-01", Active: true}
	private := Season{ID: 2, LeagueID: rhl, Start: "2026-10-01", Active: true}
	all := []Season{winter, spring, other, private, fall, nextWinter}

	for date, want := range map[string]int{
		"2026-10-04": 15331, // active
		"2026-06-15": 10561, // fall is flagged active but hasn't begun: spring, the latest started
		"2026-12-15": 15331, // between seasons: fall's final standings
		"2027-01-10": 15331, // fall still flagged active: it wins until GameSheet clears the flag
		"2025-01-01": 10562, // nothing started: the earliest upcoming
	} {
		if got, ok := Current(all, rhl, date); !ok || got.ID != want {
			t.Errorf("%s: got %d, want %d", date, got.ID, want)
		}
	}
	fall.Active = false
	if got, _ := Current([]Season{winter, spring, fall, nextWinter}, rhl, "2027-01-10"); got.ID != 20000 {
		t.Errorf("after fall's flag clears: got %d, want 20000", got.ID)
	}
	if _, ok := Current([]Season{other}, rhl, "2026-10-04"); ok {
		t.Error("picked another league's season")
	}
}
