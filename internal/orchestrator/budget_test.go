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
