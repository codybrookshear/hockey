// Package cache keeps recent answers from the sites these apps read (the
// rink's schedule, GameSheet), and limits how often each is asked. Both apps
// are public, and every miss is a request to someone else's site, so the
// requests must stay bounded however much traffic arrives.
package cache

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBusy means the Limiter's budget for the minute is spent.
var ErrBusy = errors.New("too many requests to the source; try again in a minute")

// Limiter is one upstream site's budget: one request at a time, at most
// perMinute a minute. Share one between the caches that read the same site.
type Limiter struct {
	perMinute int
	now       func() time.Time
	slot      chan struct{} // holds a token while a request runs

	mu     sync.Mutex
	recent []time.Time // when recent requests started
}

func NewLimiter(perMinute int, now func() time.Time) *Limiter {
	return &Limiter{perMinute: perMinute, now: now, slot: make(chan struct{}, 1)}
}

func (l *Limiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	keep := l.recent[:0]
	for _, t := range l.recent {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	l.recent = keep
	if len(l.recent) >= l.perMinute {
		return false
	}
	l.recent = append(l.recent, now)
	return true
}

// Cache holds values by key. A value is served for FreshFor, then fetched
// again; after a failed fetch, the key isn't tried again for RetryAfter, and
// the last good value (if any) is served, marked stale.
type Cache[V any] struct {
	freshFor, retryAfter time.Duration
	limiter              *Limiter
	now                  func() time.Time

	mu      sync.Mutex
	entries map[string]*entry[V]
}

type entry[V any] struct {
	value   V
	fetched time.Time // last success; zero if none
	err     error     // last failure, if newer than fetched
	failed  time.Time
}

// Result is what Get found.
type Result[V any] struct {
	Value   V
	Fetched time.Time
	Stale   bool  // Value is older than FreshFor: the latest fetch failed
	Err     error // set when there's no value to show
}

func New[V any](freshFor, retryAfter time.Duration, l *Limiter, now func() time.Time) *Cache[V] {
	return &Cache[V]{freshFor: freshFor, retryAfter: retryAfter, limiter: l, now: now, entries: map[string]*entry[V]{}}
}

// Get returns key's value, calling fetch when there's no fresh one. Callers
// bound the set of keys (e.g. dates within a window).
func (c *Cache[V]) Get(ctx context.Context, key string, fetch func(context.Context) (V, error)) Result[V] {
	if r, ok := c.cached(key, false); ok {
		return r
	}
	select {
	case c.limiter.slot <- struct{}{}:
		defer func() { <-c.limiter.slot }()
	case <-ctx.Done():
		return Result[V]{Err: ctx.Err()}
	}
	// Another request may have fetched it, or failed to, while this one waited.
	if r, ok := c.cached(key, true); ok {
		return r
	}
	if !c.limiter.allow() {
		return c.lastGood(key, ErrBusy)
	}

	v, err := fetch(ctx)
	if err != nil && ctx.Err() != nil {
		// The visitor left: not the source failing.
		return c.lastGood(key, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	e := c.entries[key]
	if e == nil {
		c.prune(now)
		e = &entry[V]{}
		c.entries[key] = e
	}
	if err != nil {
		e.err, e.failed = err, now
	} else {
		e.value, e.fetched, e.err = v, now, nil
	}
	return c.result(e, now)
}

// cached returns a fresh value, or (afterFailure) whatever there is when a
// fetch failed too recently to try again.
func (c *Cache[V]) cached(key string, afterFailure bool) (Result[V], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		return Result[V]{}, false
	}
	now := c.now()
	if !e.fetched.IsZero() && now.Sub(e.fetched) < c.freshFor {
		return c.result(e, now), true
	}
	if afterFailure && e.err != nil && now.Sub(e.failed) < c.retryAfter {
		return c.result(e, now), true
	}
	return Result[V]{}, false
}

func (c *Cache[V]) lastGood(key string, err error) Result[V] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil && !e.fetched.IsZero() {
		return c.result(e, c.now())
	}
	return Result[V]{Err: err}
}

// prune forgets keys untouched for a day (or two FreshFor periods, if
// longer): mostly dates that have slid out of a window. Called with c.mu held.
func (c *Cache[V]) prune(now time.Time) {
	idle := max(24*time.Hour, 2*c.freshFor)
	for k, e := range c.entries {
		if now.Sub(e.fetched) > idle && now.Sub(e.failed) > idle {
			delete(c.entries, k)
		}
	}
}

func (c *Cache[V]) result(e *entry[V], now time.Time) Result[V] {
	if e.fetched.IsZero() {
		return Result[V]{Err: e.err}
	}
	return Result[V]{Value: e.value, Fetched: e.fetched, Stale: now.Sub(e.fetched) >= c.freshFor}
}

// Len is the number of keys held (for tests).
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
