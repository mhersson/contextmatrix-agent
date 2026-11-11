package orchestrator

import (
	"context"

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

func (o *run) execRun(ctx context.Context, _ *Ledger, req RunRequest) (harness.Result, error) {
	return o.d.Executor.Run(ctx, req)
}

func (o *run) execFanout(ctx context.Context, req FanoutRequest) ([]harness.SubagentResult, error) {
	return o.d.Executor.SpawnSubagents(ctx, req)
}
