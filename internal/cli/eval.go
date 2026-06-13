package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/eval"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/spf13/cobra"
)

type evalParams struct {
	role, out, transcriptDir, check string
	samples, maxTurns               int
	maxCost, maxTotalCost           float64
	dryRun                          bool
	providerSort                    string
	quantAllow                      string
	date                            string // baseline date stamp; defaults to today in RunE
	referenceModel                  string // known-capable model that must clear the floor
}

func newEvalCmd() *cobra.Command {
	var (
		p          evalParams
		modelsCSV  string
		freeAuto   bool
		minContext int
	)

	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Measure per-role capability scores across a model set",
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := os.Getenv("OPENROUTER_API_KEY")
			if key == "" {
				return fmt.Errorf("OPENROUTER_API_KEY is not set")
			}

			client := llm.NewClient(key, llm.WithRetry(llm.DefaultRetryPolicy()))

			cat, err := client.FetchCatalog(cmd.Context())
			if err != nil {
				return fmt.Errorf("fetch catalog: %w", err)
			}

			models, err := resolveModels(modelsCSV, freeAuto, cat, minContext)
			if err != nil {
				return err
			}

			tasks, err := eval.DefaultTasks(p.role)
			if err != nil {
				return err
			}

			// Default the baseline date here, not in business logic — runEval takes the
			// date as data so tests are deterministic (no time.Now in the merge path).
			if p.date == "" {
				p.date = time.Now().UTC().Format("2006-01-02")
			}

			return runEval(cmd.Context(), cmd.OutOrStdout(), client, cat, models, tasks, p)
		},
	}
	cmd.Flags().StringVar(&p.role, "role", "all", "coder | reviewer | all")
	cmd.Flags().StringVar(&modelsCSV, "models", "", "comma-separated model allowlist (default: curated candidates)")
	cmd.Flags().BoolVar(&freeAuto, "free-auto", false, "use the :free tool-capable subset from the live catalog")
	cmd.Flags().IntVar(&p.samples, "samples", 3, "samples per (model, task)")
	cmd.Flags().StringVar(&p.out, "out", "internal/registry/data/capabilities.json", "capabilities file to write/merge")
	cmd.Flags().StringVar(&p.transcriptDir, "transcript-dir", "", "directory for per-run JSONL transcripts")
	cmd.Flags().StringVar(&p.check, "check", "", "baseline capabilities.json; non-zero exit if any tier drops")
	cmd.Flags().Float64Var(&p.maxCost, "max-cost", 0.50, "per-run USD cap")
	cmd.Flags().Float64Var(&p.maxTotalCost, "max-total-cost", 0, "abort the sweep once cumulative cost exceeds this (0 = unlimited)")
	cmd.Flags().BoolVar(&p.dryRun, "dry-run", false, "print a rough cost estimate and exit")
	cmd.Flags().IntVar(&p.maxTurns, "max-turns", 30, "max model turns per run")
	cmd.Flags().IntVar(&minContext, "min-context", 16384, "min context window for --free-auto models")
	cmd.Flags().StringVar(&p.providerSort, "provider-sort", "throughput", "OpenRouter provider sort: throughput|price|latency (empty disables provider routing)")
	cmd.Flags().StringVar(&p.quantAllow, "quant-allow", "fp16,bf16,fp8,unknown", "allowed provider quantizations (comma-separated); gates out heavy quant like fp4/int4")
	cmd.Flags().StringVar(&p.date, "date", "", "baseline date stamp YYYY-MM-DD (default: today, UTC)")
	cmd.Flags().StringVar(&p.referenceModel, "reference-model", "deepseek/deepseek-v4-flash", "known-capable model that must clear the calibrated floor, else the battery is presumed broken")

	return cmd
}

func resolveModels(csv string, freeAuto bool, cat llm.Catalog, minContext int) ([]string, error) {
	switch {
	case csv != "":
		return splitCSV(csv), nil
	case freeAuto:
		m := eval.FreeToolModels(cat, minContext)
		if len(m) == 0 {
			return nil, fmt.Errorf("--free-auto found no :free tool-capable models with context >= %d", minContext)
		}

		return m, nil
	default:
		return eval.DefaultCandidates(), nil
	}
}

// runEval is the testable core: it takes an injected client + catalog + resolved
// models/tasks so unit tests need no network.
func runEval(ctx context.Context, w io.Writer, client llm.LLM, cat llm.Catalog, models []string, tasks []eval.Task, p evalParams) error {
	if p.dryRun {
		est := eval.EstimateCost(cat, models, len(tasks), p.samples, 8000, 1500)
		fmt.Fprintf(w, "dry-run: %d models × %d tasks × %d samples = %d runs; rough est ≈ $%.2f\n", //nolint:errcheck
			len(models), len(tasks), p.samples, len(models)*len(tasks)*p.samples, est)

		return nil
	}

	// Normalize samples once so the matrix, the calibrated floor, and the stamped meta
	// all agree on the battery size (RunMatrix defaults to 1 internally otherwise).
	if p.samples <= 0 {
		p.samples = 1
	}

	prov, err := providerRouting(p.providerSort, p.quantAllow)
	if err != nil {
		return err
	}

	if prov != nil {
		fmt.Fprintf(w, "provider routing: %s\n", string(prov)) //nolint:errcheck
	}

	mr, err := eval.RunMatrix(ctx, client, eval.MatrixOpts{
		Models: models, Tasks: tasks, Samples: p.samples,
		TranscriptDir: p.transcriptDir, MaxTurns: p.maxTurns, MaxCostUSD: p.maxCost, MaxTotalCost: p.maxTotalCost,
		Provider: prov,
	})
	if err != nil {
		return err
	}

	// Score ONLY the trustworthy cells: partial-coverage (budget-aborted) and
	// parse-artifact (all-zero-tool-call) cells are excluded and never overwrite the
	// prior baseline. Every dropped cell is logged with which cell and why — no
	// silent truncation.
	measured, dropped := eval.MeasuredComplete(mr)
	for _, d := range dropped {
		fmt.Fprintf(w, "EXCLUDED %s/%s from merge: %s\n", d.Cell.Model, d.Cell.Role, d.Reason) //nolint:errcheck
	}

	// Eval-time canary: the known-capable reference model must clear the calibrated
	// floor on every role it was measured on, else the battery itself is broken and the
	// whole measurement is suspect. The floor is calibrated against the per-cell trial
	// count (samples × tasks-of-role) of the smallest-battery role — NOT the raw sample
	// count — because that is the n at which the gated scores actually live. Using
	// p.samples here would size the floor to a 3-trial ceiling while the scores sit at a
	// 30-trial ceiling, making the canary near-useless. The floor and the cell scores
	// share eval.DefaultZ so they live at one confidence level.
	floor := eval.CalibratedFloor(eval.MinCellTrials(tasks, p.samples), eval.DefaultZ)
	if err := eval.AssertReferenceModel(measured, p.referenceModel, floor); err != nil {
		return err
	}

	// Merge the surviving measured cells over any existing baseline, then write with a
	// fully populated meta envelope. Merge intentionally overwrites across task-library
	// hashes (retain-prior for unmeasured cells, overwrite for measured ones) and does
	// NOT refuse on a hash mismatch — that lets a re-measure overwrite the pre-hardening
	// baseline. The hash guard lives in --check (comparison), not here (overwrite).
	final := measured

	if f, err := os.Open(p.out); err == nil {
		prior, perr := registry.LoadCapabilities(f)
		f.Close() //nolint:errcheck

		if perr == nil {
			final = registry.MergeCapabilities(prior, measured)
		}
	}

	meta := registry.CapabilitiesMeta{
		Date:            p.date,
		Samples:         p.samples,
		TaskLibraryHash: eval.TaskLibraryHash(),
		Routing:         routingLabel(prov),
		HarnessVersion:  eval.HarnessVersion,
		Floor:           floor,
	}

	if err := writeCapabilitiesFile(p.out, final, meta); err != nil {
		return err
	}

	eval.RenderScores(w, mr, measured)

	if mr.Aborted {
		fmt.Fprintf(w, "WARNING: budget $%.2f exceeded; wrote partial scores\n", p.maxTotalCost) //nolint:errcheck
	}

	if p.check != "" {
		return runCheck(w, p.check, measured, meta.TaskLibraryHash)
	}

	return nil
}

// routingLabel summarizes the provider routing block for the baseline meta: the
// sort key when routing is active, "default" when OpenRouter's default routing is
// used (prov nil).
func routingLabel(prov json.RawMessage) string {
	if prov == nil {
		return "default"
	}

	var block struct {
		Sort string `json:"sort"`
	}

	if err := json.Unmarshal(prov, &block); err == nil && block.Sort != "" {
		return block.Sort
	}

	return "custom"
}

// runCheck compares the measured scores against a baseline. It refuses to compare
// across task libraries: if both the baseline and the measured set carry a
// task_library_hash and they differ, the comparison is meaningless and the check
// errors. Unmeasured cells are treated as UNKNOWN, not regressions — a baseline cell
// the measured set lacks is skipped (we did not re-measure it this sweep), and a
// measured cell absent from the baseline is reported as new (no prior to compare).
func runCheck(w io.Writer, baselinePath string, measured map[string]map[registry.Role]float64, measuredHash string) error {
	f, err := os.Open(baselinePath)
	if err != nil {
		return fmt.Errorf("open baseline: %w", err)
	}
	defer f.Close() //nolint:errcheck

	base, meta, err := registry.LoadCapabilitiesWithMeta(f)
	if err != nil {
		return err
	}

	// Refuse a cross-library comparison. A legacy baseline with no recorded hash
	// (empty) is exempt — there is nothing to disagree with.
	if meta.TaskLibraryHash != "" && measuredHash != "" && meta.TaskLibraryHash != measuredHash {
		return fmt.Errorf("baseline task_library_hash %q != measured %q: different task libraries, refusing to compare",
			meta.TaskLibraryHash, measuredHash)
	}

	var regressions []string

	for m, roles := range base {
		for r, bscore := range roles {
			// A baseline cell we did not measure this sweep is UNKNOWN, not a
			// regression — skip it rather than treating absence as 0.
			mroles, ok := measured[m]
			if !ok {
				continue
			}

			mscore, ok := mroles[r]
			if !ok {
				continue
			}

			if eval.TierRank(mscore) < eval.TierRank(bscore) {
				regressions = append(regressions, fmt.Sprintf("%s/%s: %.2f (tier %d) < baseline %.2f (tier %d)",
					m, r, mscore, eval.TierRank(mscore), bscore, eval.TierRank(bscore)))
			}
		}
	}

	// Report measured cells absent from the baseline as new (informational, not a
	// failure) so a fresh model's first measurement is visible.
	reportNewCells(w, base, measured)

	if len(regressions) > 0 {
		sort.Strings(regressions)

		for _, line := range regressions {
			fmt.Fprintf(w, "REGRESSION: %s\n", line) //nolint:errcheck
		}

		return fmt.Errorf("%d capability regression(s)", len(regressions))
	}

	fmt.Fprintln(w, "check: no regressions") //nolint:errcheck

	return nil
}

// reportNewCells prints measured (model, role) cells that have no baseline entry, in
// sorted order, as informational "new" lines.
func reportNewCells(w io.Writer, base, measured map[string]map[registry.Role]float64) {
	var lines []string

	for m, roles := range measured {
		for r, mscore := range roles {
			if _, ok := base[m][r]; ok {
				continue
			}

			lines = append(lines, fmt.Sprintf("new: %s/%s = %.2f (tier %d), no baseline entry",
				m, r, mscore, eval.TierRank(mscore)))
		}
	}

	sort.Strings(lines)

	for _, line := range lines {
		fmt.Fprintln(w, line) //nolint:errcheck
	}
}

func writeCapabilitiesFile(path string, caps map[string]map[registry.Role]float64, meta registry.CapabilitiesMeta) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	return eval.WriteCapabilitiesWithMeta(f, caps, meta)
}

// providerRouting builds the OpenRouter provider routing block: sort by `sort`,
// require tool support, restricted to the allowed quantizations. Returns nil
// (OpenRouter's default routing) when sort is empty. "unknown" is kept in the
// allowlist on purpose — native/proprietary endpoints (Anthropic, OpenAI,
// DeepSeek-official) report their quantization as "unknown".
func providerRouting(sort, quantCSV string) (json.RawMessage, error) {
	if strings.TrimSpace(sort) == "" {
		return nil, nil
	}

	block := map[string]any{
		"sort":               sort,
		"require_parameters": true,
	}
	if q := splitCSV(quantCSV); len(q) > 0 {
		block["quantizations"] = q
	}

	b, err := json.Marshal(block)
	if err != nil {
		return nil, fmt.Errorf("marshal provider routing: %w", err)
	}

	return b, nil
}

func splitCSV(s string) []string {
	var out []string

	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}
