package orchestrator

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBudgetLedger(t *testing.T) {
	t.Run("accumulates spend", func(t *testing.T) {
		l := NewLedger(5.0, 0)
		l.Spend(1.0)
		l.Spend(2.5)
		assert.InDelta(t, 3.5, l.Spent(), 1e-9)
	})

	t.Run("seeds from already-reported total", func(t *testing.T) {
		l := NewLedger(5.0, 2.0)
		assert.InDelta(t, 2.0, l.Spent(), 1e-9)
		l.Spend(0.5)
		assert.InDelta(t, 2.5, l.Spent(), 1e-9)
	})

	t.Run("Check errs once reported+run spend reaches max", func(t *testing.T) {
		l := NewLedger(5.0, 4.0)
		require.NoError(t, l.Check())
		l.Spend(0.99)
		require.NoError(t, l.Check())
		l.Spend(0.01) // total now 5.0 == max
		err := l.Check()

		var be *BudgetExceededError
		require.ErrorAs(t, err, &be)
		assert.InDelta(t, 5.0, be.Spent, 1e-9)
		assert.InDelta(t, 5.0, be.Max, 1e-9)
	})

	t.Run("max == 0 disables the ceiling", func(t *testing.T) {
		l := NewLedger(0, 0)
		l.Spend(1000.0)
		require.NoError(t, l.Check())
	})

	t.Run("Spend is concurrency safe", func(t *testing.T) {
		l := NewLedger(0, 0)

		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() {
				l.Spend(0.01)
			})
		}

		wg.Wait()
		assert.InDelta(t, 1.0, l.Spent(), 1e-9)
	})
}

// TestLedgerServerFloor pins the two-lower-bounds model: effective spend is the
// max of the local scalar and the summed per-card server totals, and a card's
// server total is max-monotonic (a stale response never lowers the floor).
func TestLedgerServerFloor(t *testing.T) {
	l := NewLedger(10, 1.0)

	l.SyncServerTotal("CARD-1", 4.0)
	assert.InDelta(t, 4.0, l.Spent(), 1e-9, "server floor wins when above the local scalar")

	l.Spend(5.0) // local now 6.0
	assert.InDelta(t, 6.0, l.Spent(), 1e-9, "local wins when above the floor")

	l.SyncServerTotal("SUB-1", 3.5)
	assert.InDelta(t, 7.5, l.Spent(), 1e-9, "per-card server totals sum")

	l.SyncServerTotal("CARD-1", 2.0)
	assert.InDelta(t, 7.5, l.Spent(), 1e-9, "a stale lower total is ignored")
}

// TestLedgerCheckUsesServerFloor pins the CTXAGENT-017 incident shape: local
// charges of $0 (cost-less gateway) with a server-priced floor over the ceiling
// must trip Check.
func TestLedgerCheckUsesServerFloor(t *testing.T) {
	l := NewLedger(8.75, 0)

	require.NoError(t, l.Check())

	l.SyncServerTotal("CARD-1", 41.07)

	err := l.Check()

	var bee *BudgetExceededError

	require.ErrorAs(t, err, &bee)
	assert.InDelta(t, 41.07, bee.Spent, 1e-9)
	assert.InDelta(t, 8.75, bee.Max, 1e-9)
}

// TestLedgerServerFloorConcurrency exercises Spend and SyncServerTotal racing
// from fan-out goroutines; run under -race via make test-race.
func TestLedgerServerFloorConcurrency(t *testing.T) {
	l := NewLedger(0, 0)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			l.Spend(0.01)
			l.SyncServerTotal("CARD-1", float64(i))
		})
	}

	wg.Wait()

	assert.InDelta(t, 49.0, l.Spent(), 1e-9, "highest synced total wins over the 0.50 local sum")
}
