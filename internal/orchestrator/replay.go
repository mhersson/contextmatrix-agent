package orchestrator

import (
	"context"
	"sync"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
)

// replayMinContextWindow keeps replay to models whose window absorbs the
// reasoning carried on every later request; narrower or uncatalogued models
// run without it.
const replayMinContextWindow = 400_000

// replays reports whether a harness run on model sends back each reply's
// reasoning with its later requests.
func (o *run) replays(model string) bool {
	return o.d.Cfg.ReplayReasoning && o.d.Registry.ContextWindow(model) >= replayMinContextWindow
}

// spawnSubagents fans specs out, splitting them by replay decision because the
// harness takes one replay setting per fan-out. Results keep spec order.
func (o *run) spawnSubagents(ctx context.Context, client llm.LLM, root string, specs []harness.SubagentSpec, opts harness.SubagentOpts) ([]harness.SubagentResult, error) {
	var on, off []int

	for i, s := range specs {
		model := s.Model
		if model == "" {
			model = opts.DefaultModel
		}

		if o.replays(model) {
			on = append(on, i)
		} else {
			off = append(off, i)
		}
	}

	if len(on) == 0 || len(off) == 0 {
		opts.ReplayReasoning = len(on) > 0

		return harness.SpawnSubagents(ctx, client, root, o.d.Emit, specs, opts)
	}

	results := make([]harness.SubagentResult, len(specs))
	errs := make([]error, 2)

	var wg sync.WaitGroup

	for n, idx := range [][]int{on, off} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			sub := make([]harness.SubagentSpec, len(idx))
			for j, i := range idx {
				sub[j] = specs[i]
			}

			side := opts
			side.ReplayReasoning = n == 0

			res, err := harness.SpawnSubagents(ctx, client, root, o.d.Emit, sub, side)
			errs[n] = err

			for j, i := range idx {
				if j < len(res) {
					results[i] = res[j]
				}
			}
		}()
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	return results, nil
}

// withoutReasoning copies msgs with every ReasoningDetails dropped, leaving
// msgs untouched.
func withoutReasoning(msgs []llm.Message) []llm.Message {
	if len(msgs) == 0 {
		return nil
	}

	out := make([]llm.Message, len(msgs))
	copy(out, msgs)

	for i := range out {
		out[i].ReasoningDetails = nil
	}

	return out
}
