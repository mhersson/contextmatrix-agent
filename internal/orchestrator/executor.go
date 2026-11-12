package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// Executor runs a card run's model work. The worker picks the harness or the
// Claude Code executor per run; every model run in this package goes through it.
type Executor interface {
	Run(ctx context.Context, req RunRequest) (harness.Result, error)
	SpawnSubagents(ctx context.Context, req FanoutRequest) ([]harness.SubagentResult, error)
}

// RunRequest is one harness.Run call plus what only the Claude Code executor reads.
type RunRequest struct {
	Client    llm.LLM // the harness executor's client; Claude Code ignores it
	Registry  *tools.Registry
	Emit      *events.Emitter
	Task      string
	Config    harness.Config
	Dir       string // the registry's root: Claude Code's working directory
	SessionID string // Claude Code: start a persisted session with this UUID
	Resume    string // Claude Code: continue this session; at most one of the two
}

// FanoutRequest is one harness.SpawnSubagents call plus what only Claude Code reads.
type FanoutRequest struct {
	Client    llm.LLM
	Root      string
	Emit      *events.Emitter
	Specs     []harness.SubagentSpec
	Opts      harness.SubagentOpts
	OnDelta   func(llm.Delta) // Claude Code: liveness; the harness streams through Client
	BudgetUSD float64         // Claude Code: each child's spend cap; 0 = none
}

// runScope is a run's tools root and the ledger it is charged to.
type runScope struct {
	dir    string
	ledger *Ledger
}

func (o *run) mainScope() runScope {
	return runScope{dir: o.d.Cfg.Workspace, ledger: o.ledger}
}

// UsageLimitError marks a run the subscription's usage limit stopped. The
// worker parks the card; nothing retries.
type UsageLimitError struct {
	Window   string    // "" when none was named
	ResetsAt time.Time // zero when no reset time was reported
}

func (e *UsageLimitError) Error() string {
	msg := "subscription usage limit reached"
	if e.Window != "" {
		msg += " (" + e.Window + ")"
	}

	if !e.ResetsAt.IsZero() {
		msg += ", resets at " + e.ResetsAt.UTC().Format(time.RFC3339)
	}

	return msg
}

// noteUsageLimit records the first usage limit err carries; later ones are dropped.
func (o *run) noteUsageLimit(err error) {
	if ule, ok := errors.AsType[*UsageLimitError](err); ok {
		o.usageLimit.CompareAndSwap(nil, ule)
	}
}

// usageLimitHit returns the recorded usage limit, nil until one was.
func (o *run) usageLimitHit() *UsageLimitError {
	return o.usageLimit.Load()
}

// execRun refuses to start once a usage limit is recorded (both modes). In
// Claude Code mode it also refuses on an exhausted ledger and caps the run at
// the remaining budget: vendorkit reads a 0 cap as "no cap".
func (o *run) execRun(ctx context.Context, ledger *Ledger, req RunRequest) (harness.Result, error) {
	if ule := o.usageLimitHit(); ule != nil {
		return harness.Result{}, ule
	}

	if o.d.Cfg.ClaudeCode {
		if ledger == nil {
			ledger = o.ledger
		}

		if err := ledger.Check(); err != nil {
			return harness.Result{}, err
		}

		if req.Config.MaxCostUSD <= 0 {
			req.Config.MaxCostUSD, _ = ledger.Remaining()
		}

		if req.Config.OnDelta == nil && o.progress != nil {
			req.Config.OnDelta = o.progress.onDelta
		}
	}

	res, err := o.d.Executor.Run(ctx, req)
	o.noteUsageLimit(err)

	return res, err
}

// execFanout refuses like execRun. Claude Code children run concurrently, so
// each is capped at an equal share of the remaining budget.
func (o *run) execFanout(ctx context.Context, req FanoutRequest) ([]harness.SubagentResult, error) {
	if ule := o.usageLimitHit(); ule != nil {
		return nil, ule
	}

	if o.d.Cfg.ClaudeCode {
		if err := o.ledger.Check(); err != nil {
			return nil, err
		}

		req.BudgetUSD = 0

		if remaining, capped := o.ledger.Remaining(); capped {
			req.BudgetUSD = remaining / float64(max(len(req.Specs), 1))
			if req.BudgetUSD <= 0 {
				return nil, &BudgetExceededError{Spent: o.ledger.Spent(), Max: o.ledger.Spent()}
			}
		}

		if req.OnDelta == nil && o.progress != nil {
			req.OnDelta = o.progress.onDelta
		}
	}

	res, err := o.d.Executor.SpawnSubagents(ctx, req)
	o.noteUsageLimit(err)

	for _, r := range res {
		o.noteUsageLimit(r.Err)
	}

	return res, err
}
