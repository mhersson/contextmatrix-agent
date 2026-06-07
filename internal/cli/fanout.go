package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/spf13/cobra"
)

type fanoutOpts struct {
	workspace string
	model     string
	maxDepth  int
	costCap   float64
	tasks     []harness.SubagentSpec
	human     io.Writer
}

func runFanout(ctx context.Context, client llm.LLM, o fanoutOpts) ([]harness.SubagentResult, error) {
	human := o.human
	if human == nil {
		human = os.Stdout
	}
	emit := events.NewEmitter(human, nil)
	return harness.SpawnSubagents(ctx, client, o.workspace, emit, o.tasks, harness.SubagentOpts{
		Depth: 0, MaxDepth: o.maxDepth, AggregateCostCap: o.costCap, DefaultModel: o.model,
	})
}

func parseTasks(raw []string) ([]harness.SubagentSpec, error) {
	specs := make([]harness.SubagentSpec, 0, len(raw))
	for _, t := range raw {
		role, prompt, ok := strings.Cut(t, "=")
		if !ok || role == "" || prompt == "" {
			return nil, fmt.Errorf("task %q must be in the form role=prompt", t)
		}
		specs = append(specs, harness.SubagentSpec{Role: role, Prompt: prompt})
	}
	return specs, nil
}

func newFanoutCmd() *cobra.Command {
	var workspace, configFile string
	var taskArgs []string
	var maxDepth int
	var maxCost float64
	cmd := &cobra.Command{
		Use:   "fanout",
		Short: "Fan out parallel read-only subagents over a workspace (SpawnSubagents demo)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Flags(), configFile)
			if err != nil {
				return err
			}
			key := os.Getenv("OPENROUTER_API_KEY")
			if key == "" {
				return fmt.Errorf("OPENROUTER_API_KEY is not set")
			}
			model := derefStr(cfg.Model)
			if model == "" {
				return fmt.Errorf("--model (or config 'model') is required")
			}
			if workspace == "" {
				return fmt.Errorf("--workspace is required")
			}
			specs, err := parseTasks(taskArgs)
			if err != nil {
				return err
			}
			client := llm.NewClient(key, llm.WithRetry(llm.DefaultRetryPolicy()))
			results, err := runFanout(cmd.Context(), client, fanoutOpts{
				workspace: workspace, model: model, maxDepth: maxDepth, costCap: maxCost,
				tasks: specs, human: cmd.OutOrStdout(),
			})
			if err != nil {
				return err
			}
			for _, r := range results {
				fmt.Fprintf(cmd.OutOrStdout(), "\n=== %s ===\n%s\n(completed=%v cost=%.5f err=%v)\n", //nolint:errcheck
					r.Role, r.Output, r.Result.Completed, r.Result.TotalCostUSD, r.Err)
			}
			return nil
		},
	}
	cmd.Flags().String("model", "", "OpenRouter model slug for children (or config 'model')")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace directory (required)")
	cmd.Flags().StringArrayVar(&taskArgs, "task", nil, "subagent task as role=prompt (repeatable)")
	cmd.Flags().IntVar(&maxDepth, "max-depth", 2, "maximum subagent recursion depth")
	cmd.Flags().Float64Var(&maxCost, "max-cost-total", 0.50, "aggregate USD cap across children")
	cmd.Flags().StringVar(&configFile, "config", "", "path to a YAML config file")
	return cmd
}
