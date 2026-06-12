package executor

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTracker_AddIfUnderLimit_CapacityBoundary(t *testing.T) {
	tr := NewTracker(2)

	assert.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))
	assert.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "B"}))
	assert.Equal(t, 2, tr.Count())

	// Third distinct add is refused at capacity.
	assert.False(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "C"}))
	assert.Equal(t, 2, tr.Count())
}

func TestTracker_AddIfUnderLimit_DuplicateKeyRefused(t *testing.T) {
	tr := NewTracker(5)

	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))
	// Re-adding the same project/card key is refused (one container per card).
	assert.False(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))
	assert.Equal(t, 1, tr.Count())
}

func TestTracker_Get(t *testing.T) {
	tr := NewTracker(5)
	r := &Run{Project: "p", CardID: "A", ContainerID: "cid"}

	require.True(t, tr.AddIfUnderLimit(r))

	got, ok := tr.Get("p", "A")
	require.True(t, ok)
	assert.Equal(t, "cid", got.ContainerID)

	_, ok = tr.Get("p", "missing")
	assert.False(t, ok)
}

func TestTracker_Remove_Idempotent(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	tr.Remove("p", "A")
	assert.Equal(t, 0, tr.Count())

	// Removing again is a no-op, not a panic.
	tr.Remove("p", "A")
	tr.Remove("p", "never-existed")
	assert.Equal(t, 0, tr.Count())
}

func TestTracker_RemoveClearsAuxiliaryState(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	tr.SetAwaiting("p", "A", true)
	tr.Touch("p", "A")
	tr.Remove("p", "A")

	// After removal the auxiliary maps are cleared too.
	assert.False(t, tr.Awaiting("p", "A"))
	assert.True(t, tr.LastActivity("p", "A").IsZero())

	// And a fresh add reuses the slot without stale awaiting state.
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))
	assert.False(t, tr.Awaiting("p", "A"))
}

func TestTracker_Awaiting_SetAndClear(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	assert.False(t, tr.Awaiting("p", "A"))

	tr.SetAwaiting("p", "A", true)
	assert.True(t, tr.Awaiting("p", "A"))

	tr.SetAwaiting("p", "A", false)
	assert.False(t, tr.Awaiting("p", "A"))

	// Awaiting on an untracked key is false.
	assert.False(t, tr.Awaiting("p", "missing"))
}

func TestTracker_Touch_UpdatesLastActivity(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	assert.True(t, tr.LastActivity("p", "A").IsZero())

	before := time.Now()

	tr.Touch("p", "A")
	got := tr.LastActivity("p", "A")

	assert.False(t, got.IsZero())
	assert.False(t, got.Before(before))
}

func TestTracker_List_Snapshot(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "B"}))

	list := tr.List()
	assert.Len(t, list, 2)

	// Mutating the returned slice must not affect the tracker.
	list[0] = nil

	assert.Equal(t, 2, tr.Count())
	assert.Len(t, tr.List(), 2)
}

func TestTracker_ConcurrentAdds_RaceClean(t *testing.T) {
	const limit = 50

	tr := NewTracker(limit)

	var wg sync.WaitGroup

	var (
		mu       sync.Mutex
		accepted int
	)

	for i := range 200 {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()

			r := &Run{Project: "p", CardID: cardName(n)}
			if tr.AddIfUnderLimit(r) {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	assert.Equal(t, limit, accepted)
	assert.Equal(t, limit, tr.Count())
}

func cardName(n int) string {
	const digits = "0123456789"

	if n == 0 {
		return "card-0"
	}

	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}

	return "card-" + string(b)
}
