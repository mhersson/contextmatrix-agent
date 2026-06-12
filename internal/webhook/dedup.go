package webhook

import (
	"container/list"
	"sync"
	"time"
)

// DedupCache remembers the (project, cardID, messageID) tuples whose /message
// request has already been DELIVERED, so a retry returns a cached ack instead
// of writing the user frame to the worker's stdin a second time. The check
// (Contains) and the record (Record) are deliberately split so a caller records
// only after a successful delivery — a failed write or an untracked run must not
// poison the cache, or ContextMatrix's retry would get a false duplicate ack and
// silently drop the human's message. It is TTL- and capacity-bounded; an empty
// messageID NEVER dedups (the client opted out of at-most-once delivery). All
// methods are safe for concurrent use.
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

// Contains reports whether the (project, cardID, messageID) tuple has already
// been recorded inside the TTL window. It is a pure read — nothing is recorded.
// An empty messageID always returns false: dedup requires the client to supply
// an idempotency key. A TTL-expired entry is reaped here so it does not linger.
func (c *DedupCache) Contains(project, cardID, messageID string) bool {
	if messageID == "" {
		return false
	}

	key := dedupKey(project, cardID, messageID)

	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.index[key]
	if !ok {
		return false
	}

	entry := el.Value.(*dedupEntry)
	if c.ttl <= 0 || c.now().Sub(entry.stored) <= c.ttl {
		return true
	}

	// Expired: reap it so a later Record reuses a fresh slot.
	c.entries.Remove(el)
	delete(c.index, key)

	return false
}

// Record marks the (project, cardID, messageID) tuple as processed so a
// subsequent Contains returns true within the TTL window. It is called only
// AFTER the message has been delivered, so a failed delivery never poisons the
// cache. An empty messageID records nothing (the client opted out of
// at-most-once delivery). Recording an already-present key refreshes its
// stored time.
func (c *DedupCache) Record(project, cardID, messageID string) {
	if messageID == "" {
		return
	}

	key := dedupKey(project, cardID, messageID)

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	if el, ok := c.index[key]; ok {
		el.Value.(*dedupEntry).stored = now

		return
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
}
