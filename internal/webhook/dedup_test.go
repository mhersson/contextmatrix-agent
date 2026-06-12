package webhook

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDedup_SeenDetectsDuplicate(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	assert.False(t, d.Seen("proj", "PROJ-001", "m1"))
	assert.True(t, d.Seen("proj", "PROJ-001", "m1"))
}

func TestDedup_EmptyMessageIDNeverDedups(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	// Empty message_id is never deduped, no matter how many times it is seen.
	assert.False(t, d.Seen("proj", "PROJ-001", ""))
	assert.False(t, d.Seen("proj", "PROJ-001", ""))
	assert.False(t, d.Seen("proj", "PROJ-001", ""))
}

func TestDedup_KeyedByAllThreeFields(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	assert.False(t, d.Seen("proj", "PROJ-001", "m1"))
	// Same message_id under a different card or project is a distinct entry.
	assert.False(t, d.Seen("proj", "PROJ-002", "m1"))
	assert.False(t, d.Seen("other", "PROJ-001", "m1"))
}

func TestDedup_CapacityEvictsOldest(t *testing.T) {
	d := NewDedupCache(time.Hour, 2)

	require.False(t, d.Seen("p", "c", "a"))
	require.False(t, d.Seen("p", "c", "b"))
	require.False(t, d.Seen("p", "c", "x")) // evicts "a"

	// "a" was evicted, so it is no longer remembered.
	assert.False(t, d.Seen("p", "c", "a"))
}

func TestDedup_TTLExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	d := NewDedupCache(time.Minute, 16, withDedupClock(func() time.Time { return now }))

	require.False(t, d.Seen("p", "c", "m1"))
	require.True(t, d.Seen("p", "c", "m1"))

	now = now.Add(2 * time.Minute)

	assert.False(t, d.Seen("p", "c", "m1"))
}
