package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/kata"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/spf13/cobra"
)

type sweepRow struct {
	label string
	res   harness.Result
}

func newSweepCmd() *cobra.Command {
	var (
		weak, control string
		maxTurns      int
		maxCost       float64
	)

	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Run the same kata on a weak and a control model and compare",
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := os.Getenv("OPENROUTER_API_KEY")
			if key == "" {
				return fmt.Errorf("OPENROUTER_API_KEY is not set")
			}

			if weak == "" || control == "" {
				return fmt.Errorf("--model and --control-model are both required")
			}

			client := llm.NewClient(key)
			run := func(ctx context.Context, model string) (harness.Result, error) {
				dir, err := os.MkdirTemp("", "kata-")
				if err != nil {
					return harness.Result{}, err
				}

				if err := kata.Copy(dir); err != nil {
					return harness.Result{}, err
				}

				fmt.Fprintf(cmd.OutOrStdout(), "\n--- running %s in %s ---\n", model, dir) //nolint:errcheck

				return runSpike(ctx, client, runOpts{
					model: model, taskDir: dir,
					task:     "Make the failing test pass by editing the code, then run the tests.",
					maxTurns: maxTurns, maxCost: maxCost,
				})
			}

			rows, err := sweepDispatch(cmd.Context(), []string{weak, control}, run)
			if err != nil {
				return err
			}

			renderComparison(cmd.OutOrStdout(), rows)

			return nil
		},
	}
	cmd.Flags().StringVar(&weak, "model", "", "weak model slug (required)")
	cmd.Flags().StringVar(&control, "control-model", "", "control model slug (required)")
	cmd.Flags().IntVar(&maxTurns, "max-turns", 30, "maximum model turns per run")
	cmd.Flags().Float64Var(&maxCost, "max-cost", 0.50, "maximum USD spend per run")

	return cmd
}

func sweepDispatch(ctx context.Context, models []string, run func(context.Context, string) (harness.Result, error)) ([]sweepRow, error) {
	rows := make([]sweepRow, 0, len(models))
	for _, m := range models {
		res, err := run(ctx, m)
		if err != nil {
			return rows, fmt.Errorf("run %s: %w", m, err)
		}

		rows = append(rows, sweepRow{label: m, res: res})
	}

	return rows, nil
}

func renderComparison(w io.Writer, rows []sweepRow) {
	fmt.Fprintf(w, "\n=== sweep comparison ===\n")            //nolint:errcheck
	fmt.Fprintf(w, "%-40s %-9s %-7s %-6s %-11s %-8s %-10s\n", //nolint:errcheck
		"model", "completed", "turns", "calls", "tool_fails", "repairs", "cost_usd")

	for _, r := range rows {
		fmt.Fprintf(w, "%-40s %-9v %-7d %-6d %-11d %-8d %-10.5f\n", //nolint:errcheck
			r.label, r.res.Completed, r.res.Turns, r.res.ToolCallCount,
			r.res.ToolCallFailures, r.res.RepairCount, r.res.TotalCostUSD)
	}
}
