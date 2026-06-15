package orchestrator

import (
	"fmt"
	"sync"
)

// BudgetExceededError marks the cumulative-cost park. The worker maps it to
// exit-nonzero plus a failed callback after pushing WIP; inside the
// orchestrator it stops the phase loop without entering a further phase.
type BudgetExceededError struct {
	Spent, Max float64
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("budget exceeded: spent $%.4f of $%.4f ceiling", e.Spent, e.Max)
}

// Ledger enforces the cumulative per-card USD ceiling. It is seeded with the
// card's already-reported total so a resumed run never resets the meter: the
// ceiling spans every model-bearing step the card has ever taken, not just the
// current process.
//
// Spend is called from concurrent sub-agent fan-out, so all access is
// mutex-guarded.
//
// The type and its methods are exported for the phase implementations and
// tests; the Ledger itself is held only by the unexported run struct.
type Ledger struct {
	mu    sync.Mutex
	max   float64
	spent float64
}

// NewLedger creates a ledger with ceiling maxUSD, pre-loaded with
// alreadyReported (the card's reported cost at resume time). maxUSD == 0
// disables the ceiling entirely.
func NewLedger(maxUSD, alreadyReported float64) *Ledger {
	return &Ledger{max: maxUSD, spent: alreadyReported}
}

// Spend adds usd to the running total.
func (l *Ledger) Spend(usd float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.spent += usd
}

// Check returns a *BudgetExceededError when the ceiling is enabled (max > 0)
// and the accumulated spend has reached or exceeded it. It is the single point
// where a breach is detected; callers run it before every model-bearing step.
func (l *Ledger) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.max > 0 && l.spent >= l.max {
		return &BudgetExceededError{Spent: l.spent, Max: l.max}
	}

	return nil
}

// Spent reports the current accumulated total.
func (l *Ledger) Spent() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.spent
}
