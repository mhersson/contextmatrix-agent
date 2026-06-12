package webhook

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplayCache_DuplicateDetected(t *testing.T) {
	c := NewReplayCache(time.Minute, 16)

	// First sight: new, not a duplicate.
	assert.False(t, c.CheckAndInsert("1000", "sig-a"))
	// Second sight of the SAME (timestamp, signature) pair: duplicate.
	assert.True(t, c.CheckAndInsert("1000", "sig-a"))
}

func TestReplayCache_DistinctPairsCoexist(t *testing.T) {
	c := NewReplayCache(time.Minute, 16)

	// Same timestamp, different signature is a distinct entry.
	assert.False(t, c.CheckAndInsert("1000", "sig-a"))
	assert.False(t, c.CheckAndInsert("1000", "sig-b"))
	// Same signature, different timestamp is a distinct entry.
	assert.False(t, c.CheckAndInsert("1001", "sig-a"))

	// All three are now remembered.
	assert.True(t, c.CheckAndInsert("1000", "sig-a"))
	assert.True(t, c.CheckAndInsert("1000", "sig-b"))
	assert.True(t, c.CheckAndInsert("1001", "sig-a"))
}

func TestReplayCache_CapacityEvictsOldest(t *testing.T) {
	c := NewReplayCache(time.Hour, 2)

	require.False(t, c.CheckAndInsert("1", "a")) // [a]
	require.False(t, c.CheckAndInsert("2", "b")) // [a b]
	require.False(t, c.CheckAndInsert("3", "c")) // [b c] — a evicted (oldest)

	// "a" was evicted, so it is admitted again (no longer a duplicate).
	assert.False(t, c.CheckAndInsert("1", "a"))
	// "b" and "c" still remembered until they too age out by capacity.
	// After admitting "a" the set is [c a] (b evicted as oldest).
	assert.False(t, c.CheckAndInsert("2", "b"))
}

func TestReplayCache_ExpiredReadmitted(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewReplayCache(time.Minute, 16, withReplayClock(func() time.Time { return now }))

	require.False(t, c.CheckAndInsert("1", "a"))
	require.True(t, c.CheckAndInsert("1", "a"))

	// Advance past the TTL: the entry expires and is readmitted as fresh.
	now = now.Add(2 * time.Minute)

	assert.False(t, c.CheckAndInsert("1", "a"))
}

func TestReplayCache_JanitorSweepsExpired(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewReplayCache(time.Minute, 16,
		withReplayClock(func() time.Time { return now }),
		withReplaySweepInterval(5*time.Millisecond),
	)

	require.False(t, c.CheckAndInsert("1", "a"))
	require.Equal(t, 1, c.len())

	stop := c.StartJanitor()
	defer stop()

	now = now.Add(2 * time.Minute)

	require.Eventually(t, func() bool {
		return c.len() == 0
	}, time.Second, 2*time.Millisecond, "janitor should sweep the expired entry")
}

func TestReplayCache_StopJanitorIdempotent(t *testing.T) {
	c := NewReplayCache(time.Minute, 16)
	stop := c.StartJanitor()
	stop()
	stop() // second stop must not panic or block
}
