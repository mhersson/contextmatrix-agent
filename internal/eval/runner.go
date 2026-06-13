package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// CellKey identifies one (model, role) capability cell in the matrix. Coverage and
// scoring aggregate at this granularity (a model's coder cell spans all coder tasks
// × samples).
type CellKey struct {
	Model string
	Role  registry.Role
}

type MatrixResult struct {
	Outcomes  []Outcome
	TotalCost float64
	Aborted   bool
	Errors    int // runs skipped due to a per-run error (provider/stream/check); see RunMatrix
	// Coverage counts the outcomes actually recorded per (model, role) cell. Errored
	// and budget-aborted runs are NOT recorded, so a cell with Coverage < Expected is
	// incomplete and must be excluded from the merged baseline.
	Coverage map[CellKey]int
	// Expected is the full battery size per (model, role) cell: samples × the number of
	// tasks with that role. It is computed up front from the requested matrix, so an
	// aborted cell still reports the count it WOULD have had — that gap is what marks
	// the cell partial.
	Expected map[CellKey]int
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

	mr := MatrixResult{
		Coverage: map[CellKey]int{},
		Expected: expectedCoverage(opts.Models, opts.Tasks, samples),
	}

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
					// is not scored (no pass, no total in Score), does NOT count as
					// coverage — count it, continue. The errored run's transcript (if
					// enabled) records the failure.
					mr.Errors++

					continue
				}

				mr.Outcomes = append(mr.Outcomes, o)
				mr.Coverage[CellKey{Model: model, Role: task.Role()}]++
			}
		}
	}

	return mr, nil
}

// MinCellTrials returns the per-cell trial count of the SMALLEST-battery role in the
// requested matrix: samples × min over roles present of |tasks(role)|. This is the n
// at which the floor must be calibrated — the achievable Wilson-LB ceiling depends on
// the trial count per (model, role) cell (samples × tasks-of-role), NOT the raw
// sample count. The stored meta Floor is a single scalar applied uniformly across
// roles by the registry, so we calibrate against the most-conservative (smallest)
// battery: a floor sized to the smaller-battery role can never exceed what any role's
// cell can reach, so it never over-gates. Returns 0 when no tasks are requested.
func MinCellTrials(tasks []Task, samples int) int {
	perRole := tasksPerRole(tasks)
	if len(perRole) == 0 {
		return 0
	}

	minTasks := math.MaxInt
	for _, n := range perRole {
		if n < minTasks {
			minTasks = n
		}
	}

	return minTasks * samples
}

// tasksPerRole counts the requested tasks by role.
func tasksPerRole(tasks []Task) map[registry.Role]int {
	perRole := map[registry.Role]int{}
	for _, task := range tasks {
		perRole[task.Role()]++
	}

	return perRole
}

// expectedCoverage computes the full battery size per (model, role) cell:
// samples × the number of requested tasks with that role. Every model runs the same
// task list, so the per-role count is identical across models even when a model is
// added mid-sweep.
func expectedCoverage(models []string, tasks []Task, samples int) map[CellKey]int {
	perRole := tasksPerRole(tasks)

	expected := map[CellKey]int{}

	for _, model := range models {
		for role, n := range perRole {
			expected[CellKey{Model: model, Role: role}] = n * samples
		}
	}

	return expected
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

	// A coder Check fails a run that tampered with protected fixture files, marking
	// the verdict with tamperedDetailPrefix. Surface that on the Outcome (Pass stays
	// false either way) so the merge step can distinguish it from a wrong answer.
	return Outcome{
		Model: model, Role: task.Role(), Task: task.Name(),
		Pass: v.OK, Cost: res.TotalCostUSD, Res: res,
		Tampered: !v.OK && strings.HasPrefix(v.Detail, tamperedDetailPrefix),
	}, nil
}

func transcriptName(model, task string, sample int) string {
	safe := strings.NewReplacer("/", "_", ":", "_").Replace(model)

	return fmt.Sprintf("%s-%s-%d.jsonl", safe, task, sample)
}
