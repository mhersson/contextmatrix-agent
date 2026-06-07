package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
	"github.com/spf13/cobra"
)

type runOpts struct {
	model      string
	taskDir    string
	task       string
	maxTurns   int
	maxCost    float64
	transcript string
	human      io.Writer // defaults to os.Stdout when nil
}

func newRunCmd() *cobra.Command {
	var o runOpts
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the harness on one model against a workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			key := os.Getenv("OPENROUTER_API_KEY")
			if key == "" {
				return fmt.Errorf("OPENROUTER_API_KEY is not set")
			}
			if o.model == "" {
				return fmt.Errorf("--model is required")
			}
			client := llm.NewClient(key)
			res, err := runSpike(cmd.Context(), client, o)
			if err != nil {
				return err
			}
			printResult(cmd.OutOrStdout(), o.model, res)
			return nil
		},
	}
	cmd.Flags().StringVar(&o.model, "model", "", "OpenRouter model slug (required)")
	cmd.Flags().StringVar(&o.taskDir, "task-dir", "", "workspace directory the agent operates in (required)")
	cmd.Flags().StringVar(&o.task, "task", "Make the failing test pass by editing the code, then run the tests.", "task instruction")
	cmd.Flags().IntVar(&o.maxTurns, "max-turns", 30, "maximum model turns")
	cmd.Flags().Float64Var(&o.maxCost, "max-cost", 0.50, "maximum USD spend (0 disables)")
	cmd.Flags().StringVar(&o.transcript, "transcript", "", "path to write the JSON event transcript")
	_ = cmd.MarkFlagRequired("task-dir")
	return cmd
}

// runSpike wires deps and runs the harness once. Exposed for tests with a fake LLM.
func runSpike(ctx context.Context, client llm.LLM, o runOpts) (harness.Result, error) {
	if o.taskDir == "" {
		return harness.Result{}, fmt.Errorf("task-dir is required")
	}
	reg := tools.NewRegistry(
		tools.NewReadTool(o.taskDir),
		tools.NewEditTool(o.taskDir),
		tools.NewGrepTool(o.taskDir),
		tools.NewBashTool(o.taskDir),
	)

	human := o.human
	if human == nil {
		human = os.Stdout
	}
	var transcriptW io.Writer
	if o.transcript != "" {
		f, err := os.Create(o.transcript)
		if err != nil {
			return harness.Result{}, err
		}
		defer f.Close() //nolint:errcheck
		transcriptW = f
	}
	emit := events.NewEmitter(human, transcriptW)

	cfg := harness.Config{
		Model:        o.model,
		SystemPrompt: systemPrompt,
		MaxTurns:     o.maxTurns,
		MaxCostUSD:   o.maxCost,
	}
	return harness.Run(ctx, client, reg, emit, o.task, cfg)
}

const systemPrompt = "You are a coding agent working inside a Go project. Use the provided tools (read, edit, grep, bash) to inspect and modify files. To finish, run the tests with the bash tool and, once they pass, reply with a short confirmation and no tool call."

func printResult(w io.Writer, model string, r harness.Result) {
	fmt.Fprintf(w, "\n=== result: %s ===\n", model)                                                                           //nolint:errcheck
	fmt.Fprintf(w, "completed=%v reason=%s turns=%d tool_calls=%d tool_failures=%d repairs=%d cost_usd=%.5f model_used=%s\n", //nolint:errcheck
		r.Completed, r.Reason, r.Turns, r.ToolCallCount, r.ToolCallFailures, r.RepairCount, r.TotalCostUSD, r.ModelUsed)
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
