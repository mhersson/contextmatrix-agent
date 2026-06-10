package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

type MatrixOpts struct {
	Models        []string
	Tasks         []Task
	Samples       int
	TranscriptDir string // "" disables per-run transcripts
	MaxTurns      int
	MaxCostUSD    float64         // per-run cap
	MaxTotalCost  float64         // 0 = unlimited; abort before a run that would exceed this
	Provider      json.RawMessage // OpenRouter provider routing (sort/require_parameters/quantizations); nil = default routing
}

type MatrixResult struct {
	Outcomes  []Outcome
	TotalCost float64
	Aborted   bool
	Errors    int // runs skipped due to a per-run error (provider/stream/check); see RunMatrix
}

// RunMatrix drives every (model × task × sample) in-process, scoring each run via
// the task's Check. It aborts (Aborted=true) before starting a run once cumulative
// cost has reached MaxTotalCost, returning whatever completed. A per-run error
// (transient provider/stream failure, or a check error) does not abort the sweep:
// the run is skipped (not scored), counted in Errors, and any partial spend still
// accrues to TotalCost.
func RunMatrix(ctx context.Context, client llm.LLM, opts MatrixOpts) (MatrixResult, error) {
	samples := opts.Samples
	if samples <= 0 {
		samples = 1
	}

	var mr MatrixResult

	for _, model := range opts.Models {
		for _, task := range opts.Tasks {
			for s := 0; s < samples; s++ {
				if opts.MaxTotalCost > 0 && mr.TotalCost >= opts.MaxTotalCost {
					mr.Aborted = true

					return mr, nil
				}

				o, err := runOne(ctx, client, model, task, s, opts)

				mr.TotalCost += o.Cost // accrue any partial spend, even on error
				if err != nil {
					// A per-run error (transient provider/stream failure, or a check
					// error) must not abort the whole costly sweep. Skip the run — it
					// is not scored (no pass, no total in Score) — count it, continue.
					// The errored run's transcript (if enabled) records the failure.
					mr.Errors++

					continue
				}

				mr.Outcomes = append(mr.Outcomes, o)
			}
		}
	}

	return mr, nil
}

func runOne(ctx context.Context, client llm.LLM, model string, task Task, sample int, opts MatrixOpts) (Outcome, error) {
	dir, err := os.MkdirTemp("", "eval-")
	if err != nil {
		return Outcome{}, err
	}
	defer os.RemoveAll(dir) //nolint:errcheck

	if err := task.Provision(dir); err != nil {
		return Outcome{}, fmt.Errorf("provision %s: %w", task.Name(), err)
	}

	var reg *tools.Registry
	if task.Role() == registry.RoleReviewer {
		reg = tools.NewReadOnlyRegistry(dir)
	} else {
		reg = tools.NewRegistry(
			tools.NewReadTool(dir), tools.NewEditTool(dir), tools.NewWriteTool(dir),
			tools.NewGrepTool(dir), tools.NewGlobTool(dir), tools.NewGitTool(dir), tools.NewBashTool(dir),
		)
	}

	var tw io.Writer

	if opts.TranscriptDir != "" {
		if err := os.MkdirAll(opts.TranscriptDir, 0o755); err != nil {
			return Outcome{}, err
		}

		f, err := os.Create(filepath.Join(opts.TranscriptDir, transcriptName(model, task.Name(), sample)))
		if err != nil {
			return Outcome{}, err
		}
		defer f.Close() //nolint:errcheck

		tw = f
	}

	emit := events.NewEmitter(nil, tw)

	res, err := harness.Run(ctx, client, reg, emit, task.Prompt(), harness.Config{
		Model:      model,
		MaxTurns:   opts.MaxTurns,
		MaxCostUSD: opts.MaxCostUSD,
		Provider:   opts.Provider,
	})
	if err != nil {
		return Outcome{Model: model, Role: task.Role(), Task: task.Name(), Cost: res.TotalCostUSD}, fmt.Errorf("run %s on %s: %w", task.Name(), model, err)
	}

	v, err := task.Check(ctx, dir, res)
	if err != nil {
		return Outcome{Model: model, Role: task.Role(), Task: task.Name(), Cost: res.TotalCostUSD}, fmt.Errorf("check %s on %s: %w", task.Name(), model, err)
	}

	return Outcome{Model: model, Role: task.Role(), Task: task.Name(), Pass: v.OK, Cost: res.TotalCostUSD, Res: res}, nil
}

func transcriptName(model, task string, sample int) string {
	safe := strings.NewReplacer("/", "_", ":", "_").Replace(model)

	return fmt.Sprintf("%s-%s-%d.jsonl", safe, task, sample)
}
