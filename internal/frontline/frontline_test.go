package frontline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

var la = func() *time.Location {
	l, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return l
}()

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, la) }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Saved from the real site on 2026-10-04.
func TestParseDay(t *testing.T) {
	events, err := parse(fixture(t, "2026-10-04.html"), day(2026, 10, 4), la)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 15 {
		t.Fatalf("got %d events, want 15", len(events))
	}
	count := map[string]int{}
	for _, e := range events {
		count[e.Surface]++
	}
	if count["Big Sheet"] != 9 || count["Mini Sheet"] != 6 {
		t.Errorf("by surface: %v, want 9 Big Sheet, 6 Mini Sheet", count)
	}

	first := events[0]
	want := Event{
		Start:   time.Date(2026, 10, 4, 7, 15, 0, 0, la),
		End:     time.Date(2026, 10, 4, 8, 45, 0, 0, la),
		Surface: "Big Sheet",
		Home:    Side{Name: "16U LAHA", Locker: "2"},
		Away:    Side{Name: "Tacoma", Locker: "6"},
	}
	if first != want {
		t.Errorf("first event:\n got %+v\nwant %+v", first, want)
	}
	// Public skate: same name both sides, no lockers. 12:00 P is noon.
	skate := events[5]
	if skate.Home != (Side{Name: "Public Skate"}) || skate.Away != skate.Home ||
		skate.Start.Hour() != 12 || skate.End.Hour() != 13 || skate.End.Minute() != 30 {
		t.Errorf("public skate: %+v", skate)
	}
	// Away side named, no locker.
	if e := events[2]; e.Home != (Side{Name: "Yth Stick-Time", Locker: "9"}) || e.Away != (Side{Name: "Yth Stick-Time"}) {
		t.Errorf("stick time: %+v", e)
	}
	last := events[len(events)-1]
	if last.Start.Hour() != 20 || last.End.Hour() != 21 || last.End.Minute() != 15 {
		t.Errorf("last event times: %v – %v", last.Start, last.End)
	}
}

func TestParseEmptyDay(t *testing.T) {
	events, err := parse(fixture(t, "2027-06-15-empty.html"), day(2027, 6, 15), la)
	if err != nil {
		t.Fatal(err)
	}
	if events == nil || len(events) != 0 {
		t.Fatalf("got %#v, want an empty, non-nil list", events)
	}
}

func TestParseRefusesWrongOrBrokenPages(t *testing.T) {
	good := fixture(t, "2026-10-04.html")
	for name, c := range map[string]struct {
		page []byte
		date time.Time
	}{
		// The site ignoring the date shows today: not what was asked for.
		"other day":      {good, day(2026, 10, 5)},
		"error page":     {fixture(t, "error-page.html"), day(2026, 10, 4)},
		"no table":       {[]byte(`<h4>Daily Events for Sunday Oct 04, 2026</h4>`), day(2026, 10, 4)},
		"renamed column": {[]byte(strings.Replace(string(good), "Away or Event", "Visitor", 1)), day(2026, 10, 4)},
		"bad time":       {[]byte(strings.Replace(string(good), "07:15 A - 08:45 A", "TBD", 1)), day(2026, 10, 4)},
		"extra cell":     {[]byte(strings.Replace(string(good), "<td class=\"text-center\">Big Sheet</td>", "<td>Big Sheet</td><td>x</td>", 1)), day(2026, 10, 4)},
	} {
		if events, err := parse(c.page, c.date, la); err == nil {
			t.Errorf("%s: accepted, %d events", name, len(events))
		}
	}
}

func TestTimes(t *testing.T) {
	d := day(2026, 10, 4)
	for in, want := range map[string][2]string{
		"07:15 A - 08:45 A": {"07:15", "08:45"},
		"11:00 A - 12:00 P": {"11:00", "12:00"},
		"12:15 A - 01:00 A": {"00:15", "01:00"},
		"9:00A - 10:15A":    {"09:00", "10:15"},
		"11:00 P - 12:15 A": {"23:00", "00:15+1"}, // past midnight
	} {
		s, e, err := times(in, d, la)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		got := [2]string{s.Format("15:04"), e.Format("15:04")}
		if e.Day() != s.Day() {
			got[1] += "+1"
		}
		if got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "TBD", "13:00 P - 02:00 P", "07:60 A - 08:00 A", "07:15 - 08:45"} {
		if _, _, err := times(bad, d, la); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestClientDay(t *testing.T) {
	page := fixture(t, "2026-10-04.html")
	var gotDate, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Query().Get("fac") != "laneice" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		gotDate, gotUA = r.PostFormValue("SelectedDate"), r.UserAgent()
		w.Header().Set("Content-Type", "text/html;charset=UTF-8")
		w.Write(page)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), URL: srv.URL + "/dailysched.cfm?fac=laneice&facid=1", Location: la, UserAgent: "test-agent"}
	events, err := c.Day(context.Background(), day(2026, 10, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 15 || gotDate != "10/04/2026" || gotUA != "test-agent" {
		t.Errorf("events=%d date=%q ua=%q", len(events), gotDate, gotUA)
	}
}

func TestClientRefusesBadResponses(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"500": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusInternalServerError) },
		"json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		},
		"huge": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(strings.Repeat("<p>x</p>", maxPage/8+1)))
		},
	} {
		srv := httptest.NewServer(h)
		c := &Client{HTTP: srv.Client(), URL: srv.URL, Location: la}
		if _, err := c.Day(context.Background(), day(2026, 10, 4)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		srv.Close()
	}
}
