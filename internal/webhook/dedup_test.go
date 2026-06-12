package webhook

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDedup_RecordThenContains(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	// Not recorded yet → absent. Recording makes a later Contains report true.
	assert.False(t, d.Contains("proj", "PROJ-001", "m1"))
	d.Record("proj", "PROJ-001", "m1")
	assert.True(t, d.Contains("proj", "PROJ-001", "m1"))
}

func TestDedup_ContainsIsPureRead(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	// Contains must never record: repeated checks stay false until Record runs.
	assert.False(t, d.Contains("proj", "PROJ-001", "m1"))
	assert.False(t, d.Contains("proj", "PROJ-001", "m1"))
	assert.False(t, d.Contains("proj", "PROJ-001", "m1"))
}

func TestDedup_EmptyMessageIDNeverDedups(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	// Empty message_id is never deduped: Record is a no-op, Contains is false.
	d.Record("proj", "PROJ-001", "")
	assert.False(t, d.Contains("proj", "PROJ-001", ""))
	assert.False(t, d.Contains("proj", "PROJ-001", ""))
}

func TestDedup_KeyedByAllThreeFields(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	d.Record("proj", "PROJ-001", "m1")
	// Same message_id under a different card or project is a distinct entry.
	assert.True(t, d.Contains("proj", "PROJ-001", "m1"))
	assert.False(t, d.Contains("proj", "PROJ-002", "m1"))
	assert.False(t, d.Contains("other", "PROJ-001", "m1"))
}

func TestDedup_CapacityEvictsOldest(t *testing.T) {
	d := NewDedupCache(time.Hour, 2)

	d.Record("p", "c", "a")
	d.Record("p", "c", "b")
	d.Record("p", "c", "x") // evicts "a"

	// "a" was evicted, so it is no longer remembered.
	assert.False(t, d.Contains("p", "c", "a"))
	assert.True(t, d.Contains("p", "c", "b"))
	assert.True(t, d.Contains("p", "c", "x"))
}

func TestDedup_TTLExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	d := NewDedupCache(time.Minute, 16, withDedupClock(func() time.Time { return now }))

	d.Record("p", "c", "m1")
	require.True(t, d.Contains("p", "c", "m1"))

	now = now.Add(2 * time.Minute)

	// Expired: Contains is false and reaps the stale entry.
	assert.False(t, d.Contains("p", "c", "m1"))
}
