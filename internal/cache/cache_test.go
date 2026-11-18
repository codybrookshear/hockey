package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

// counter is a fetch func that counts calls and fails while err is set.
type counter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *counter) fetch(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "value", nil
}

func (f *counter) n() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func setup() (*clock, *Cache[string], *counter) {
	c := &clock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	return c, New[string](10*time.Minute, time.Minute, NewLimiter(5, c.now), c.now), &counter{}
}

func TestFreshThenRefetch(t *testing.T) {
	c, cache, f := setup()
	ctx := context.Background()
	if r := cache.Get(ctx, "k", f.fetch); r.Err != nil || r.Value != "value" || r.Stale {
		t.Fatalf("first: %+v", r)
	}
	cache.Get(ctx, "k", f.fetch)
	if f.n() != 1 {
		t.Fatalf("fetched %d times, want 1", f.n())
	}
	c.add(10 * time.Minute)
	cache.Get(ctx, "k", f.fetch)
	if f.n() != 2 {
		t.Fatalf("fetched %d times after expiry, want 2", f.n())
	}
}

func TestStaleOnFailureAndRetryAfter(t *testing.T) {
	c, cache, f := setup()
	ctx := context.Background()
	cache.Get(ctx, "k", f.fetch)
	f.err = errors.New("down")
	c.add(11 * time.Minute)
	if r := cache.Get(ctx, "k", f.fetch); r.Err != nil || !r.Stale || r.Value != "value" {
		t.Fatalf("want the stale value: %+v", r)
	}
	n := f.n()
	cache.Get(ctx, "k", f.fetch)
	if f.n() != n {
		t.Error("retried right after a failure")
	}
	c.add(time.Minute)
	cache.Get(ctx, "k", f.fetch)
	if f.n() != n+1 {
		t.Error("didn't retry after RetryAfter")
	}
	// A key never fetched has nothing to fall back on.
	if r := cache.Get(ctx, "other", f.fetch); r.Err == nil {
		t.Error("no error for a failed first fetch")
	}
}

func TestLimiterBudget(t *testing.T) {
	c, cache, f := setup()
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		cache.Get(ctx, k, f.fetch)
	}
	if f.n() != 5 {
		t.Fatalf("fetched %d times in a minute, want 5", f.n())
	}
	if r := cache.Get(ctx, "h", f.fetch); !errors.Is(r.Err, ErrBusy) {
		t.Errorf("over budget: %+v", r)
	}
	c.add(time.Minute)
	if r := cache.Get(ctx, "h", f.fetch); r.Err != nil {
		t.Errorf("a minute later: %v", r.Err)
	}
}

func TestSharedLimiter(t *testing.T) {
	c := &clock{t: time.Now()}
	l := NewLimiter(2, c.now)
	a := New[string](time.Minute, time.Minute, l, c.now)
	b := New[string](time.Minute, time.Minute, l, c.now)
	f := &counter{}
	a.Get(context.Background(), "1", f.fetch)
	b.Get(context.Background(), "1", f.fetch)
	if r := b.Get(context.Background(), "2", f.fetch); !errors.Is(r.Err, ErrBusy) {
		t.Errorf("third fetch across caches sharing a budget of 2: %+v", r)
	}
}

func TestConcurrentGetsShareOneFetch(t *testing.T) {
	_, cache, f := setup()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { cache.Get(context.Background(), "k", f.fetch) })
	}
	wg.Wait()
	if f.n() != 1 {
		t.Errorf("fetched %d times, want 1", f.n())
	}
}

func TestVisitorLeavingIsNotAFailure(t *testing.T) {
	_, cache, f := setup()
	ctx, cancel := context.WithCancel(context.Background())
	leaving := func(ctx context.Context) (string, error) {
		f.fetch(ctx)
		cancel()
		return "", ctx.Err()
	}
	if r := cache.Get(ctx, "k", leaving); r.Err == nil {
		t.Fatal("no error")
	}
	// The next visitor fetches right away rather than waiting out RetryAfter.
	if r := cache.Get(context.Background(), "k", f.fetch); r.Err != nil || f.n() != 2 {
		t.Errorf("next visitor: err=%v calls=%d", r.Err, f.n())
	}
}

func TestPruneForgetsIdleKeys(t *testing.T) {
	c, cache, f := setup()
	ctx := context.Background()
	cache.Get(ctx, "old", f.fetch)
	c.add(25 * time.Hour)
	cache.Get(ctx, "new", f.fetch)
	if cache.Len() != 1 {
		t.Errorf("%d keys, want 1", cache.Len())
	}
}

func TestServeReturnsTheOldValueAndRefetchesBehind(t *testing.T) {
	c, cache, f := setup()
	ctx := context.Background()
	// Nothing yet: the first visitor waits.
	if r := cache.Serve(ctx, "k", f.fetch); r.Err != nil || r.Value != "value" || f.n() != 1 {
		t.Fatalf("first: %+v after %d fetches", r, f.n())
	}
	cache.Serve(ctx, "k", f.fetch)
	cache.Wait()
	if f.n() != 1 {
		t.Fatalf("a fresh value was fetched again: %d", f.n())
	}
	// No longer fresh: served as it is (not a failure, so not Stale), and
	// fetched once behind the visitors.
	fetched := c.now()
	c.add(11 * time.Minute)
	for range 5 {
		if r := cache.Serve(ctx, "k", f.fetch); r.Err != nil || r.Stale || !r.Fetched.Equal(fetched) {
			t.Fatalf("old value: %+v", r)
		}
	}
	cache.Wait()
	if n := f.n(); n < 2 || n > 5 {
		t.Fatalf("fetched %d times", n)
	}
	if r := cache.Serve(ctx, "k", f.fetch); !r.Fetched.Equal(c.now()) {
		t.Fatalf("not refreshed: %+v", r)
	}
	// The source fails: the old value, marked stale once it's known, and
	// no new attempt until RetryAfter.
	c.add(11 * time.Minute)
	f.mu.Lock()
	f.err = errors.New("down")
	f.mu.Unlock()
	cache.Serve(ctx, "k", f.fetch)
	cache.Wait()
	n := f.n()
	if r := cache.Serve(ctx, "k", f.fetch); r.Err != nil || !r.Stale || r.Value != "value" {
		t.Fatalf("after a failure: %+v", r)
	}
	cache.Wait()
	if f.n() != n {
		t.Fatalf("retried within RetryAfter")
	}
}

func TestRefreshFetchesAFreshValue(t *testing.T) {
	_, cache, f := setup()
	ctx := context.Background()
	cache.Get(ctx, "k", f.fetch)
	if r := cache.Refresh(ctx, "k", f.fetch); r.Err != nil || f.n() != 2 {
		t.Fatalf("refresh: %+v after %d fetches", r, f.n())
	}
	cache.Get(ctx, "k", f.fetch)
	if f.n() != 2 {
		t.Fatalf("fetched %d times, want 2", f.n())
	}
}
