package site

import (
	"context"
	"errors"
	"sync"
	"time"

	"hockey/internal/frontline"
)

// Source returns a day's events; *frontline.Client in production.
type Source interface {
	Day(ctx context.Context, date time.Time) ([]frontline.Event, error)
}

const (
	freshFor         = 10 * time.Minute // serve a fetched day this long before asking again
	retryAfter       = time.Minute      // after a failed fetch of a day, wait this long
	fetchesPerMinute = 20               // across all days
)

var errBusy = errors.New("too many schedule requests; try again in a minute")

// schedules caches days from the Source and limits how often it's asked: the
// site is public, and every miss is a request to the rink's site. One fetch
// runs at a time, at most fetchesPerMinute a minute. When a fetch fails, the
// last good copy (if any) is served, marked stale.
type schedules struct {
	src Source
	now func() time.Time

	mu      sync.Mutex
	days    map[string]*day // by date, YYYY-MM-DD
	fetches []time.Time     // when recent fetches started

	fetching chan struct{} // holds a token while a fetch runs
}

type day struct {
	events  []frontline.Event
	fetched time.Time // last success; zero if none
	err     error     // last failure, if it's newer than fetched
	failed  time.Time
}

type result struct {
	Events  []frontline.Event
	Fetched time.Time
	Stale   bool  // Events are older than freshFor: the latest fetch failed
	Err     error // set when there's nothing to show
}

func newSchedules(src Source, now func() time.Time) *schedules {
	return &schedules{src: src, now: now, days: map[string]*day{}, fetching: make(chan struct{}, 1)}
}

// get returns date's events. Callers keep date inside the site's window, which
// bounds the cache.
func (s *schedules) get(ctx context.Context, date time.Time) result {
	key := date.Format(time.DateOnly)
	if r, ok := s.cached(key, false); ok {
		return r
	}
	select {
	case s.fetching <- struct{}{}:
		defer func() { <-s.fetching }()
	case <-ctx.Done():
		return result{Err: ctx.Err()}
	}
	// Another request may have fetched it, or failed to, while this one waited.
	if r, ok := s.cached(key, true); ok {
		return r
	}
	if !s.allowFetch() {
		r, _ := s.cachedOr(key, errBusy)
		return r
	}

	events, err := s.src.Day(ctx, date)
	if err != nil && ctx.Err() != nil {
		// The visitor left: not the rink's site failing.
		r, _ := s.cachedOr(key, err)
		return r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	d := s.days[key]
	if d == nil {
		s.prune(now)
		d = &day{}
		s.days[key] = d
	}
	if err != nil {
		d.err, d.failed = err, now
	} else {
		d.events, d.fetched, d.err = events, now, nil
	}
	return d.result(now)
}

// prune forgets days not fetched (or tried) for a day: mostly days that have
// slid out of the site's window. Called with s.mu held.
func (s *schedules) prune(now time.Time) {
	for k, d := range s.days {
		if now.Sub(d.fetched) > 24*time.Hour && now.Sub(d.failed) > 24*time.Hour {
			delete(s.days, k)
		}
	}
}

// cached returns a fresh copy of key, or (afterFailure) whatever there is when
// a fetch failed too recently to try again.
func (s *schedules) cached(key string, afterFailure bool) (result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.days[key]
	if d == nil {
		return result{}, false
	}
	now := s.now()
	if !d.fetched.IsZero() && now.Sub(d.fetched) < freshFor {
		return d.result(now), true
	}
	if afterFailure && d.err != nil && now.Sub(d.failed) < retryAfter {
		return d.result(now), true
	}
	return result{}, false
}

func (s *schedules) cachedOr(key string, err error) (result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.days[key]; d != nil && !d.fetched.IsZero() {
		return d.result(s.now()), true
	}
	return result{Err: err}, false
}

func (s *schedules) allowFetch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	recent := s.fetches[:0]
	for _, t := range s.fetches {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	s.fetches = recent
	if len(s.fetches) >= fetchesPerMinute {
		return false
	}
	s.fetches = append(s.fetches, now)
	return true
}

func (d *day) result(now time.Time) result {
	if d.fetched.IsZero() {
		return result{Err: d.err}
	}
	return result{Events: d.events, Fetched: d.fetched, Stale: now.Sub(d.fetched) >= freshFor}
}
