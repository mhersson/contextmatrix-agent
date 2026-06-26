// Package webhook is the agent backend's HTTP surface: the HMAC-verified
// lifecycle endpoints (trigger, kill, stop-all, message, promote, end-session),
// the SSE /logs stream, and the health/readiness/containers
// probes ContextMatrix drives the backend through. It implements the
// contextmatrix-protocol wire contract.
package webhook

import (
	"container/list"
	"sync"
	"time"
)

// ReplayCache is a TTL- and capacity-bounded set of accepted
// (timestamp, signature) pairs. It implements protocol.ReplayCache so it can be
// passed directly to protocol.VerifySignatureWithTimestamp: CheckAndInsert
// returns true when the pair was already seen inside the window (reject as a
// replay) and records it otherwise.
//
// Eviction is twofold: capacity evicts the oldest entry by insertion order
// (front of the list), and TTL expires entries either lazily on CheckAndInsert
// or eagerly via the StartJanitor sweep. All methods are safe for concurrent
// use.
type ReplayCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	now      func() time.Time
	interval time.Duration

	// entries is an insertion-ordered list, oldest at the front. index maps the
	// composite key to its list element for O(1) lookup.
	entries *list.List
	index   map[string]*list.Element

	stopOnce sync.Once
	stopCh   chan struct{}
}

type replayEntry struct {
	key  string
	seen time.Time
}

// replayCacheOption configures a ReplayCache.
type replayCacheOption func(*ReplayCache)

// withReplayClock injects a deterministic clock for tests.
func withReplayClock(now func() time.Time) replayCacheOption {
	return func(c *ReplayCache) {
		if now != nil {
			c.now = now
		}
	}
}

// withReplaySweepInterval overrides the janitor tick. Tests shrink it; the
// production default is derived from the TTL.
func withReplaySweepInterval(d time.Duration) replayCacheOption {
	return func(c *ReplayCache) {
		if d > 0 {
			c.interval = d
		}
	}
}

// NewReplayCache builds a replay cache with the given TTL and capacity. A TTL
// <= 0 disables time-based expiry (entries live until evicted by capacity); a
// capacity <= 0 disables the hard cap.
func NewReplayCache(ttl time.Duration, capacity int, opts ...replayCacheOption) *ReplayCache {
	c := &ReplayCache{
		ttl:      ttl,
		capacity: capacity,
		now:      time.Now,
		interval: replaySweepInterval(ttl),
		entries:  list.New(),
		index:    make(map[string]*list.Element),
		stopCh:   make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// replaySweepInterval picks min(ttl/2, 60s) so a small skew window is swept
// proportionally faster than a flat tick would allow. A non-positive TTL still
// returns the ceiling so the janitor has a meaningful interval to honour stop
// against.
func replaySweepInterval(ttl time.Duration) time.Duration {
	const maxInterval = 60 * time.Second
	if ttl <= 0 {
		return maxInterval
	}

	if half := ttl / 2; half < maxInterval {
		return half
	}

	return maxInterval
}

// CheckAndInsert reports whether (timestamp, signature) was already recorded
// inside the TTL window and, if not, records it. It returns true for a
// duplicate (the caller rejects the request) and false for a fresh pair.
// Binding both the timestamp and the signature mirrors the value
// protocol.VerifySignatureWithTimestamp signs, so two distinct requests issued
// in the same Unix second never collide.
func (c *ReplayCache) CheckAndInsert(timestamp, signature string) bool {
	key := timestamp + "." + signature

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	if el, ok := c.index[key]; ok {
		entry := el.Value.(*replayEntry)
		if c.ttl <= 0 || now.Sub(entry.seen) <= c.ttl {
			return true
		}
		// Expired: drop and treat the incoming pair as fresh.
		c.entries.Remove(el)
		delete(c.index, key)
	}

	el := c.entries.PushBack(&replayEntry{key: key, seen: now})
	c.index[key] = el

	if c.capacity > 0 {
		for c.entries.Len() > c.capacity {
			oldest := c.entries.Front()
			if oldest == nil {
				break
			}

			c.entries.Remove(oldest)
			delete(c.index, oldest.Value.(*replayEntry).key)
		}
	}

	return false
}

// StartJanitor launches a background goroutine that sweeps expired entries on
// the cache's interval. It returns a stop function that is safe to call more
// than once. With a non-positive TTL the goroutine still runs but does no work.
func (c *ReplayCache) StartJanitor() (stop func()) {
	go func() {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()

		for {
			select {
			case <-c.stopCh:
				return
			case <-ticker.C:
				c.sweep()
			}
		}
	}()

	return func() {
		c.stopOnce.Do(func() { close(c.stopCh) })
	}
}

// sweep removes every entry older than the TTL.
func (c *ReplayCache) sweep() {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := c.now().Add(-c.ttl)

	for {
		front := c.entries.Front()
		if front == nil {
			return
		}

		entry := front.Value.(*replayEntry)
		if entry.seen.After(cutoff) {
			return
		}

		c.entries.Remove(front)
		delete(c.index, entry.key)
	}
}

// len returns the current entry count. Test-only.
func (c *ReplayCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.entries.Len()
}
