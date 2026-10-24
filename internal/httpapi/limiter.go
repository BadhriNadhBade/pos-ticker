package httpapi

import (
	"sync"
	"time"
)

const (
	limiterMaxKeys    = 4096 // distinct IP keys tracked per counter
	limiterMaxPerKey  = 1024 // timestamps kept per key
	imageAttemptsMin  = 10   // image-bearing /contact requests per IP per minute
	imageConcurrency  = 2    // concurrent PrepareImage calls on the public path
	contactCountCache = 5 * time.Second
)

// slidingCounter is a small in-memory per-key event counter over a sliding
// window. It is deliberately simple and bounded: it is only a cheap early
// filter in front of the authoritative, persistent Store.InsertMessage check.
type slidingCounter struct {
	mu     sync.Mutex
	window time.Duration
	m      map[string][]time.Time
}

func newSlidingCounter(window time.Duration) *slidingCounter {
	return &slidingCounter{window: window, m: map[string][]time.Time{}}
}

// prune drops expired timestamps for key and returns what is left (callers
// hold c.mu).
func (c *slidingCounter) prune(key string, now time.Time) []time.Time {
	ts := c.m[key]
	cut := now.Add(-c.window)
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	if i > 0 {
		ts = ts[i:]
		if len(ts) == 0 {
			delete(c.m, key)
			return nil
		}
		c.m[key] = ts
	}
	return ts
}

// count returns the number of events for key inside the window (read-only
// apart from dropping expired entries).
func (c *slidingCounter) count(key string, now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.prune(key, now))
}

// add records one event for key.
func (c *slidingCounter) add(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts := c.prune(key, now)
	if ts == nil && len(c.m) >= limiterMaxKeys {
		c.evictLocked(now)
	}
	if len(ts) >= limiterMaxPerKey {
		ts = ts[1:]
	}
	c.m[key] = append(ts, now)
}

// evictLocked makes room: drop every fully expired key, and if the map is
// still full drop an eighth of the keys so the O(n) sweep is amortised.
func (c *slidingCounter) evictLocked(now time.Time) {
	for k := range c.m {
		c.prune(k, now)
	}
	if len(c.m) < limiterMaxKeys {
		return
	}
	drop := limiterMaxKeys / 8
	for k := range c.m {
		delete(c.m, k)
		if drop--; drop <= 0 {
			break
		}
	}
}

func (c *slidingCounter) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// countCache memoises the /contact/count value for a short TTL so an
// unauthenticated flood cannot turn every request into a COUNT(*).
type countCache struct {
	mu  sync.Mutex
	val int
	exp time.Time
}

// get returns the cached value or computes it (serialised, so concurrent
// callers share a single query). Errors are not cached.
func (c *countCache) get(now time.Time, load func() (int, error)) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Before(c.exp) {
		return c.val, nil
	}
	n, err := load()
	if err != nil {
		return 0, err
	}
	c.val, c.exp = n, now.Add(contactCountCache)
	return n, nil
}
