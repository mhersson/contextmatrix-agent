package harness

import (
	"context"
	"fmt"
	"sync"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

// SubagentSpec describes one child agent.
type SubagentSpec struct {
	Role       string
	Prompt     string
	Model      string  // optional per-spec override; falls back to opts.DefaultModel
	MaxTurns   int     // 0 -> harness default
	MaxCostUSD float64 // 0 -> per-child share of the aggregate cap
}

// SubagentResult is one child's structured result.
type SubagentResult struct {
	Role   string
	Output string
	Result Result
	Err    error
}

// SubagentOpts bounds a fan-out.
type SubagentOpts struct {
	Depth            int     // current recursion depth (0 at the top level)
	MaxDepth         int     // refuse to spawn at/after this depth
	AggregateCostCap float64 // total USD budget across children (0 = uncapped)
	DefaultModel     string  // model for specs that don't override
}

// SpawnSubagents runs specs as parallel, READ-ONLY child Harness.Run calls over
// the shared workspace root. Read-only is structural (tools.NewReadOnlyRegistry),
// so parallel fan-out cannot corrupt the workspace. Depth and aggregate-cost
// caps bound recursion and spend. Results preserve spec order. Children share
// emit (thread-safe), so their activity interleaves into the same transcript.
func SpawnSubagents(ctx context.Context, client llm.LLM, root string, emit *events.Emitter, specs []SubagentSpec, opts SubagentOpts) ([]SubagentResult, error) {
	if opts.MaxDepth > 0 && opts.Depth >= opts.MaxDepth {
		return nil, fmt.Errorf("subagent depth %d reached the cap of %d", opts.Depth, opts.MaxDepth)
	}

	if len(specs) == 0 {
		return nil, nil
	}

	reg := tools.NewReadOnlyRegistry(root)

	perChild := 0.0
	if opts.AggregateCostCap > 0 {
		perChild = opts.AggregateCostCap / float64(len(specs))
	}

	emit.Emit(events.StateChange, map[string]any{"spawn_subagents": len(specs), "depth": opts.Depth})

	results := make([]SubagentResult, len(specs))

	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)

		go func(i int, spec SubagentSpec) {
			defer wg.Done()

			model := spec.Model
			if model == "" {
				model = opts.DefaultModel
			}

			cost := spec.MaxCostUSD
			if cost == 0 {
				cost = perChild
			}

			res, err := Run(ctx, client, reg, emit, spec.Prompt,
				Config{Model: model, MaxTurns: spec.MaxTurns, MaxCostUSD: cost})
			results[i] = SubagentResult{Role: spec.Role, Output: res.Output, Result: res, Err: err}
		}(i, spec)
	}

	wg.Wait()

	return results, nil
}
