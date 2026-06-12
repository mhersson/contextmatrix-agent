package webhook

import (
	"container/list"
	"sync"
	"time"
)

// DedupCache remembers the (project, cardID, messageID) tuples whose /message
// request has already been processed, so a retry returns a cached ack instead
// of writing the user frame to the worker's stdin a second time. It is
// TTL- and capacity-bounded; an empty messageID NEVER dedups (the client opted
// out of at-most-once delivery). All methods are safe for concurrent use.
type DedupCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	now      func() time.Time

	entries *list.List
	index   map[string]*list.Element
}

type dedupEntry struct {
	key    string
	stored time.Time
}

// dedupCacheOption configures a DedupCache.
type dedupCacheOption func(*DedupCache)

// withDedupClock injects a deterministic clock for tests.
func withDedupClock(now func() time.Time) dedupCacheOption {
	return func(c *DedupCache) {
		if now != nil {
			c.now = now
		}
	}
}

// NewDedupCache builds a dedup cache with the given TTL and capacity. A TTL
// <= 0 disables time-based expiry; a capacity <= 0 disables the hard cap.
func NewDedupCache(ttl time.Duration, capacity int, opts ...dedupCacheOption) *DedupCache {
	c := &DedupCache{
		ttl:      ttl,
		capacity: capacity,
		now:      time.Now,
		entries:  list.New(),
		index:    make(map[string]*list.Element),
	}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// dedupKey builds the composite lookup key. A NUL delimiter cannot appear in a
// validated project name or card ID, so fields containing dashes or slashes
// never collide across the boundary.
func dedupKey(project, cardID, messageID string) string {
	return project + "\x00" + cardID + "\x00" + messageID
}

// Seen reports whether the (project, cardID, messageID) tuple has already been
// recorded inside the TTL window and, if not, records it. An empty messageID
// always returns false and records nothing: dedup requires the client to supply
// an idempotency key.
func (c *DedupCache) Seen(project, cardID, messageID string) bool {
	if messageID == "" {
		return false
	}

	key := dedupKey(project, cardID, messageID)

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	if el, ok := c.index[key]; ok {
		entry := el.Value.(*dedupEntry)
		if c.ttl <= 0 || now.Sub(entry.stored) <= c.ttl {
			return true
		}

		c.entries.Remove(el)
		delete(c.index, key)
	}

	el := c.entries.PushBack(&dedupEntry{key: key, stored: now})
	c.index[key] = el

	if c.capacity > 0 {
		for c.entries.Len() > c.capacity {
			oldest := c.entries.Front()
			if oldest == nil {
				break
			}

			c.entries.Remove(oldest)
			delete(c.index, oldest.Value.(*dedupEntry).key)
		}
	}

	return false
}
