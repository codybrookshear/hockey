// Package frontline reads a rink's public daily schedule from Frontline
// Connect (frontline-connect.com), the booking site The Rink Exchange uses.
//
// There's no API: the schedule page is server-rendered HTML (ColdFusion), so
// this fetches the page and parses its table. Picking a date is a form POST
// (SelectedDate=MM/DD/YYYY) to the same URL. One request returns every surface;
// the page's own surface filter isn't needed.
//
// If the page changes shape, Day returns an error rather than a partial or
// empty schedule.
package frontline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultURL is The Rink Exchange (Lane County Ice, Eugene) daily schedule.
const DefaultURL = "https://www.frontline-connect.com/dailysched.cfm?fac=laneice&facid=1"

const maxPage = 2 << 20 // the page is ~25 KB

// Event is one row of the daily schedule.
type Event struct {
	Start, End time.Time
	Surface    string // "Big Sheet", "Mini Sheet"
	Home, Away Side   // the page's "Home or Event" and "Away or Event" columns
}

// Side is one of an event's two columns: a team, or the event's name (public
// skate shows the same name in both).
type Side struct {
	Name   string
	Locker string // e.g. "2"; empty when none is assigned
}

// Client fetches one rink's schedule.
type Client struct {
	HTTP      *http.Client
	URL       string // the daily schedule page, e.g. DefaultURL
	Location  *time.Location
	UserAgent string
}

// Day returns the events on date (its year, month and day, in c.Location),
// in the page's order: by start time.
func (c *Client) Day(ctx context.Context, date time.Time) ([]Event, error) {
	form := url.Values{
		"SelectedDate": {date.Format("01/02/2006")},
		"sChange":      {"Get Date"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.UserAgent)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("schedule request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("schedule request: HTTP %d", res.StatusCode)
	}
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt != "text/html" {
		return nil, fmt.Errorf("schedule request: unexpected content type %q", mt)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxPage+1))
	if err != nil {
		return nil, fmt.Errorf("schedule request: %w", err)
	}
	if len(b) > maxPage {
		return nil, errors.New("schedule page is unexpectedly large")
	}
	return parse(b, date, c.Location)
}
