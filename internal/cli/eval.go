package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

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

	measured := eval.Score(mr.Outcomes, 1.96)

	// Merge over any existing file, then write.
	final := measured

	if f, err := os.Open(p.out); err == nil {
		prior, perr := registry.LoadCapabilities(f)
		f.Close() //nolint:errcheck

		if perr == nil {
			final = registry.MergeCapabilities(prior, measured)
		}
	}

	if err := writeCapabilitiesFile(p.out, final); err != nil {
		return err
	}

	eval.RenderScores(w, mr, measured)

	if mr.Aborted {
		fmt.Fprintf(w, "WARNING: budget $%.2f exceeded; wrote partial scores\n", p.maxTotalCost) //nolint:errcheck
	}

	if p.check != "" {
		return runCheck(w, p.check, measured)
	}

	return nil
}

func runCheck(w io.Writer, baselinePath string, measured map[string]map[registry.Role]float64) error {
	f, err := os.Open(baselinePath)
	if err != nil {
		return fmt.Errorf("open baseline: %w", err)
	}
	defer f.Close() //nolint:errcheck

	base, err := registry.LoadCapabilities(f)
	if err != nil {
		return err
	}

	var regressions []string

	for m, roles := range base {
		for r, bscore := range roles {
			mscore := measured[m][r]
			if eval.TierRank(mscore) < eval.TierRank(bscore) {
				regressions = append(regressions, fmt.Sprintf("%s/%s: %.2f (tier %d) < baseline %.2f (tier %d)",
					m, r, mscore, eval.TierRank(mscore), bscore, eval.TierRank(bscore)))
			}
		}
	}

	if len(regressions) > 0 {
		for _, line := range regressions {
			fmt.Fprintf(w, "REGRESSION: %s\n", line) //nolint:errcheck
		}

		return fmt.Errorf("%d capability regression(s)", len(regressions))
	}

	fmt.Fprintln(w, "check: no regressions") //nolint:errcheck

	return nil
}

func writeCapabilitiesFile(path string, caps map[string]map[registry.Role]float64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	return eval.WriteCapabilities(f, caps)
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
