package schedule

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hockey/internal/frontline"
)

var la = func() *time.Location {
	l, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return l
}()

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeSource serves canned events for every day and counts requests.
type fakeSource struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeSource) Day(_ context.Context, d time.Time) ([]frontline.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	at := func(h, m int) time.Time { return time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, la) }
	return []frontline.Event{
		{Start: at(7, 15), End: at(8, 45), Surface: "Big Sheet",
			Home: frontline.Side{Name: "16U LAHA", Locker: "2"}, Away: frontline.Side{Name: "Tacoma", Locker: "6"}},
		{Start: at(11, 0), End: at(12, 0), Surface: "Big Sheet",
			Home: frontline.Side{Name: "Public Skate"}, Away: frontline.Side{Name: "Public Skate"}},
		{Start: at(9, 45), End: at(10, 45), Surface: "Mini Sheet",
			Home: frontline.Side{Name: "Yth Stick-Time", Locker: "9"}, Away: frontline.Side{Name: "Yth Stick-Time"}},
		{Start: at(20, 0), End: at(21, 15), Surface: "Big Sheet",
			Home: frontline.Side{Name: "Nova Cains & Co", Locker: "2"}, Away: frontline.Side{Name: "<b>Kelly</b>", Locker: "6"}},
	}, nil
}

func (f *fakeSource) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }
func newClock(t time.Time) *clock    { return &clock{t: t} }
func at1130(d int) time.Time         { return time.Date(2026, 10, d, 11, 30, 0, 0, la) }
func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func newTestServer(t *testing.T, src Source, c *clock) http.Handler {
	t.Helper()
	srv, err := New(Config{Source: src, SourceURL: frontline.DefaultURL, Location: la, Log: quiet, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

func TestSchedulePage(t *testing.T) {
	src := &fakeSource{}
	h := newTestServer(t, src, newClock(at1130(4)))

	w := get(t, h, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Sunday, October 4", ">Today<",
		`href="/?date=2026-10-03"`, `href="/?date=2026-10-05"`,
		"<h2>Big Sheet</h2>", "<h2>Mini Sheet</h2>",
		`<td colspan="2" class="event">Yth Stick-Time<span class="locker">Locker 9</span></td>`,
		"16U LAHA", "Locker 2", "Tacoma",
		`<td colspan="2" class="event">Public Skate</td>`, // one cell for both sides
		"<span>7:15</span><span>–8:45 AM</span>",
		"<span>11:00 AM</span><span>–12:00 PM</span>",     // crosses noon
		"Nova Cains &amp; Co", "&lt;b&gt;Kelly&lt;/b&gt;", // escaped
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Big Sheet before Mini Sheet.
	if strings.Index(body, "Big Sheet") > strings.Index(body, "Mini Sheet") {
		t.Error("sheets out of order")
	}
	// At 11:30: 7:15 game over, public skate on now, evening game ahead.
	if !strings.Contains(body, `<tr class="past">`) || !strings.Contains(body, `<tr class="now">`) {
		t.Error("past/now rows not marked")
	}

	hdr := w.Header()
	for k, want := range map[string]string{
		"Content-Security-Policy": csp,
		"X-Robots-Tag":            "noindex, nofollow",
		"X-Content-Type-Options":  "nosniff",
		"Content-Type":            "text/html; charset=utf-8",
	} {
		if got := hdr.Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
	if strings.Contains(body, "<script") {
		t.Error("page has a script")
	}

	// Another day: no Today badge, no now/past marks.
	w = get(t, h, "/?date=2026-10-09")
	if b := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(b, "Friday, October 9") ||
		strings.Contains(b, `class="now"`) || strings.Contains(b, `class="past"`) || !strings.Contains(b, `<a href="/">Today</a>`) {
		t.Errorf("other day: %d", w.Code)
	}
}

func TestDateValidation(t *testing.T) {
	src := &fakeSource{}
	h := newTestServer(t, src, newClock(at1130(4)))
	for target, want := range map[string]int{
		"/?date=nope":       http.StatusBadRequest,
		"/?date=2026-13-01": http.StatusBadRequest,
		"/?date=2026-09-26": http.StatusNotFound, // 8 days back
		"/?date=2027-02-02": http.StatusNotFound, // 121 days ahead
		"/?date=2026-09-27": http.StatusOK,       // 7 days back
		"/?date=2027-02-01": http.StatusOK,       // 120 days ahead
	} {
		if w := get(t, h, target); w.Code != want {
			t.Errorf("GET %s: %d, want %d", target, w.Code, want)
		}
	}
	if src.count() != 2 {
		t.Errorf("source asked %d times, want 2 (only the valid dates)", src.count())
	}
	// No prev link at the window's start, no next at its end.
	if b := get(t, h, "/?date=2026-09-27").Body.String(); strings.Contains(b, `rel="prev"`) {
		t.Error("prev link before the window")
	}
	if b := get(t, h, "/?date=2027-02-01").Body.String(); strings.Contains(b, `rel="next"`) {
		t.Error("next link past the window")
	}
}

func TestCachingAndFailures(t *testing.T) {
	src := &fakeSource{}
	c := newClock(at1130(4))
	h := newTestServer(t, src, c)

	get(t, h, "/")
	get(t, h, "/?date=2026-10-04")
	if src.count() != 1 {
		t.Fatalf("source asked %d times, want 1 (cached)", src.count())
	}
	c.add(freshFor)
	get(t, h, "/")
	if src.count() != 2 {
		t.Fatalf("source asked %d times after expiry, want 2", src.count())
	}

	// The rink's site goes down: serve the last copy, marked stale.
	src.err = errors.New("connection refused")
	c.add(freshFor)
	w := get(t, h, "/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Couldn't reach the rink's site") ||
		!strings.Contains(w.Body.String(), "16U LAHA") {
		t.Errorf("stale copy: %d", w.Code)
	}
	// ...and don't ask again until retryAfter.
	n := src.count()
	get(t, h, "/")
	if src.count() != n {
		t.Error("retried right after a failure")
	}
	// A day never fetched: an error page.
	w = get(t, h, "/?date=2026-10-20")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "get the schedule from the rink") {
		t.Errorf("no copy: %d", w.Code)
	}
}

func TestFetchLimit(t *testing.T) {
	src := &fakeSource{}
	c := newClock(at1130(4))
	h := newTestServer(t, src, c)
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, la)
	for i := range fetchesPerMinute + 5 {
		get(t, h, "/?date="+start.AddDate(0, 0, i).Format(time.DateOnly))
	}
	if src.count() != fetchesPerMinute {
		t.Fatalf("source asked %d times in a minute, want %d", src.count(), fetchesPerMinute)
	}
	w := get(t, h, "/?date=2026-12-30")
	if w.Code != http.StatusBadGateway {
		t.Errorf("over the limit: %d, want 502", w.Code)
	}
	c.add(time.Minute)
	if w := get(t, h, "/?date=2026-12-30"); w.Code != http.StatusOK {
		t.Errorf("a minute later: %d, want 200", w.Code)
	}
}

func TestOtherRoutes(t *testing.T) {
	h := newTestServer(t, &fakeSource{}, newClock(at1130(4)))

	w := get(t, h, "/robots.txt")
	if w.Code != http.StatusOK || w.Body.String() != "User-agent: *\nDisallow: /\n" {
		t.Errorf("robots.txt: %d %q", w.Code, w.Body.String())
	}
	w = get(t, h, "/static/hockey.css")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Errorf("css: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	for _, p := range []string{"/static/", "/static/nope.css", "/static/hockey.go", "/nope", "/index.html"} {
		if w := get(t, h, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(m, "/", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /: %d, want 405", m, w.Code)
		}
	}
	// Every response, errors included, carries the security headers.
	if w := get(t, h, "/nope"); w.Header().Get("Content-Security-Policy") != csp {
		t.Error("no CSP on a 404")
	}
}

func TestConcurrentRequestsShareOneFetch(t *testing.T) {
	src := &fakeSource{}
	h := newTestServer(t, src, newClock(at1130(4)))
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if w := get(t, h, "/"); w.Code != http.StatusOK {
				panic(fmt.Sprint("status ", w.Code))
			}
		})
	}
	wg.Wait()
	if src.count() != 1 {
		t.Errorf("source asked %d times, want 1", src.count())
	}
}

type emptySource struct{}

func (emptySource) Day(context.Context, time.Time) ([]frontline.Event, error) {
	return []frontline.Event{}, nil
}

func TestEmptyDayShowsBothSheets(t *testing.T) {
	h := newTestServer(t, emptySource{}, newClock(at1130(4)))
	b := get(t, h, "/").Body.String()
	if strings.Count(b, "Nothing scheduled.") != 2 || !strings.Contains(b, "Big Sheet") || !strings.Contains(b, "Mini Sheet") {
		t.Errorf("empty day:\n%s", b)
	}
}

func TestGroupKeepsUnknownSurfaces(t *testing.T) {
	d := time.Date(2026, 10, 4, 18, 0, 0, 0, la)
	got := group([]frontline.Event{
		{Start: d, End: d.Add(time.Hour), Surface: "Studio", Home: frontline.Side{Name: "Yoga"}, Away: frontline.Side{Name: "Yoga"}},
		{Start: d, End: d.Add(time.Hour), Surface: "Mini Sheet", Home: frontline.Side{Name: "A", Locker: "9"}, Away: frontline.Side{Name: "A", Locker: "10"}},
	}, d, false)
	if len(got) != 3 || got[0].Name != "Big Sheet" || got[1].Name != "Mini Sheet" || got[2].Name != "Studio" ||
		len(got[0].Rows) != 0 || len(got[2].Rows) != 1 {
		t.Fatalf("sheets: %+v", got)
	}
	if r := got[1].Rows[0]; !r.Same || r.Home.Locker != "9, 10" {
		t.Errorf("both lockers on a single event: %+v", r)
	}
}
