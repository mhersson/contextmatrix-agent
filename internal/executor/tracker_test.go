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

	got, ok := tr.Get(RunKey{Project: "p", CardID: "A"})
	require.True(t, ok)
	assert.Equal(t, "cid", got.ContainerID)

	_, ok = tr.Get(RunKey{Project: "p", CardID: "missing"})
	assert.False(t, ok)
}

func TestTracker_Remove_Idempotent(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	tr.Remove(RunKey{Project: "p", CardID: "A"})
	assert.Equal(t, 0, tr.Count())

	// Removing again is a no-op, not a panic.
	tr.Remove(RunKey{Project: "p", CardID: "A"})
	tr.Remove(RunKey{Project: "p", CardID: "never-existed"})
	assert.Equal(t, 0, tr.Count())
}

func TestTracker_RemoveClearsAuxiliaryState(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	tr.SetAwaiting(RunKey{Project: "p", CardID: "A"}, true)
	tr.Touch(RunKey{Project: "p", CardID: "A"})
	tr.Remove(RunKey{Project: "p", CardID: "A"})

	// After removal the auxiliary maps are cleared too.
	assert.False(t, tr.Awaiting(RunKey{Project: "p", CardID: "A"}))
	assert.True(t, tr.LastActivity(RunKey{Project: "p", CardID: "A"}).IsZero())

	// And a fresh add reuses the slot without stale awaiting state.
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))
	assert.False(t, tr.Awaiting(RunKey{Project: "p", CardID: "A"}))
}

func TestTracker_Awaiting_SetAndClear(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	assert.False(t, tr.Awaiting(RunKey{Project: "p", CardID: "A"}))

	tr.SetAwaiting(RunKey{Project: "p", CardID: "A"}, true)
	assert.True(t, tr.Awaiting(RunKey{Project: "p", CardID: "A"}))

	tr.SetAwaiting(RunKey{Project: "p", CardID: "A"}, false)
	assert.False(t, tr.Awaiting(RunKey{Project: "p", CardID: "A"}))

	// Awaiting on an untracked key is false.
	assert.False(t, tr.Awaiting(RunKey{Project: "p", CardID: "missing"}))
}

func TestTracker_Touch_UpdatesLastActivity(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	assert.True(t, tr.LastActivity(RunKey{Project: "p", CardID: "A"}).IsZero())

	before := time.Now()

	tr.Touch(RunKey{Project: "p", CardID: "A"})
	got := tr.LastActivity(RunKey{Project: "p", CardID: "A"})

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

// TestTracker_Touch_UntrackedRunIsNoop covers the trailing output the pump
// flushes after waitAndCleanup has already removed the run: the write must not
// resurrect a lastActivity entry that Remove will never run again to clear.
func TestTracker_Touch_UntrackedRunIsNoop(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	tr.Remove(RunKey{Project: "p", CardID: "A"})
	tr.Touch(RunKey{Project: "p", CardID: "A"})
	tr.Touch(RunKey{Project: "p", CardID: "never-existed"})

	assert.True(t, tr.LastActivity(RunKey{Project: "p", CardID: "A"}).IsZero())
	assert.True(t, tr.LastActivity(RunKey{Project: "p", CardID: "never-existed"}).IsZero())
}

// TestTracker_SetAwaitingIgnoresAnUntrackedRun pins the guard that keeps the
// awaiting map bounded. The exit path removes the run before the last entries
// it publishes reach the bridge, and the bridge clears awaiting on every one of
// them, so an unguarded write would leave an entry behind for every completed
// run and nothing would ever reclaim it.
func TestTracker_SetAwaitingIgnoresAnUntrackedRun(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "A"}))

	tr.Remove(RunKey{Project: "p", CardID: "A"})
	tr.SetAwaiting(RunKey{Project: "p", CardID: "A"}, false)
	tr.SetAwaiting(RunKey{Project: "p", CardID: "never-existed"}, true)

	assert.False(t, tr.Awaiting(RunKey{Project: "p", CardID: "A"}))
	assert.False(t, tr.Awaiting(RunKey{Project: "p", CardID: "never-existed"}))
	assert.Empty(t, tr.awaiting, "a removed run leaves nothing behind in the awaiting map")
}

func TestRunKeyName(t *testing.T) {
	assert.Equal(t, "ABC-12", RunKey{Project: "p", CardID: "ABC-12"}.Name())
	assert.Equal(t, "ABC-12.check", RunKey{Project: "p", CardID: "ABC-12", Kind: "check"}.Name())
	assert.Equal(t, "guidance_pr", RunKey{Project: "alpha", Kind: "guidance_pr"}.Name())
	assert.Equal(t, "ALPHA-001.check", RunKey{Project: "alpha", CardID: "ALPHA-001", Kind: "check"}.Name())
}

func TestTrackerKeepsRunKindsApart(t *testing.T) {
	tr := NewTracker(3)
	card := &Run{Project: "p", CardID: "ABC-12"}
	check := &Run{Project: "p", CardID: "ABC-12", Kind: "check"}

	assert.True(t, tr.AddIfUnderLimit(card))
	assert.True(t, tr.AddIfUnderLimit(check))
	assert.False(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "ABC-12", Kind: "check"}), "one run per kind")

	got, _ := tr.Get(RunKey{Project: "p", CardID: "ABC-12"})
	assert.Same(t, card, got)

	assert.True(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "ABC-12", Kind: "investigate"}))
	assert.False(t, tr.AddIfUnderLimit(&Run{Project: "p", CardID: "ABC-13"}), "max_concurrent is shared")

	tr.SetReason(check.Key(), "killed")
	assert.Equal(t, "killed", tr.Reason(check.Key()))
	assert.Empty(t, tr.Reason(card.Key()))

	tr.Remove(check.Key())

	_, ok := tr.Get(card.Key())
	assert.True(t, ok, "removing a run kind leaves the card run")
}
