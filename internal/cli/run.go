package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/spf13/cobra"
)

type runOpts struct {
	model         string
	taskDir       string
	task          string
	systemPrompt  string
	maxTurns      int
	maxCost       float64
	contextWindow int
	toolOutputMax int
	models        []string
	provider      json.RawMessage
	reasoning     json.RawMessage
	transcript    string
	human         io.Writer // defaults to os.Stdout when nil
}

const defaultSystemPrompt = "You are a coding agent working in a project workspace. Use the provided tools (read, edit, write, grep, glob, git, bash) to inspect and modify files. When the task is complete, run any relevant checks with bash and reply with a short confirmation and no tool call."

func newRunCmd() *cobra.Command {
	var (
		taskDir, task, transcript, verify, configFile, systemPrompt string
		printConfig                                                 bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the harness on a workspace with a free-form task",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Flags(), configFile)
			if err != nil {
				return err
			}

			if printConfig {
				config.PrintRedacted(cmd.OutOrStdout(), cfg)

				return nil
			}

			if err := cfg.Validate(); err != nil {
				return err
			}

			key := os.Getenv("LLM_API_KEY")
			if key == "" {
				return fmt.Errorf("LLM_API_KEY is not set")
			}

			model := derefStr(cfg.Model)
			if model == "" {
				return fmt.Errorf("--model (or config 'model') is required")
			}

			if taskDir == "" {
				return fmt.Errorf("--workspace is required")
			}

			clientOpts := []llm.Option{llm.WithRetry(llm.DefaultRetryPolicy()), llm.WithDialect(dialectFromType(os.Getenv("LLM_TYPE")))}
			if bu := os.Getenv("LLM_BASE_URL"); bu != "" {
				clientOpts = append(clientOpts, llm.WithBaseURL(bu))
			}

			client := llm.NewClient(key, clientOpts...)

			toolOutputMax, _ := cmd.Flags().GetInt("tool-output-max-bytes")

			prov, reas := routingRaw(cfg)
			o := runOpts{
				model:         model,
				taskDir:       taskDir,
				task:          task,
				systemPrompt:  systemPrompt,
				maxTurns:      derefInt(cfg.MaxTurns),
				maxCost:       derefFloat(cfg.MaxCostUSD),
				contextWindow: resolveWindow(cmd.Context(), client, model),
				toolOutputMax: toolOutputMax,
				models:        cfg.Models,
				provider:      prov,
				reasoning:     reas,
				transcript:    transcript,
			}

			res, err := runSpike(cmd.Context(), client, o)
			if err != nil {
				return err
			}

			printResult(cmd.OutOrStdout(), model, res)

			if verify != "" {
				emit := events.NewEmitter(cmd.OutOrStdout(), nil)

				v, vErr := harness.Verify(cmd.Context(), emit, commandCheck(taskDir, verify))
				if vErr != nil {
					return vErr
				}

				fmt.Fprintf(cmd.OutOrStdout(), "verified=%v %s\n", v.OK, v.Detail) //nolint:errcheck

				if !v.OK {
					return fmt.Errorf("verification failed")
				}
			}

			return nil
		},
	}
	cmd.Flags().String("model", "", "model slug (or set 'model' in config)")
	cmd.Flags().Int("max-turns", 45, "maximum model turns")
	cmd.Flags().Float64("max-cost", 0.50, "maximum USD spend (0 disables)")
	cmd.Flags().Int("tool-output-max-bytes", 131072, "max bytes of a single tool result before head/tail truncation (0 disables)")
	cmd.Flags().StringVar(&taskDir, "workspace", "", "workspace directory the agent operates in (required)")
	cmd.Flags().StringVar(&task, "task", "Inspect the project and complete the requested change, then verify.", "task instruction")
	cmd.Flags().StringVar(&systemPrompt, "system-prompt", "", "override the default system prompt")
	cmd.Flags().StringVar(&transcript, "transcript", "", "path to write the JSON event transcript")
	cmd.Flags().StringVar(&verify, "verify", "", "shell command run after the loop; exit 0 = success")
	cmd.Flags().StringVar(&configFile, "config", "", "path to a YAML config file")
	cmd.Flags().BoolVar(&printConfig, "print-config", false, "print the effective config (secrets redacted) and exit")
	cmd.Flags().Bool("local", true, "run standalone without CM callbacks (always true)")

	return cmd
}

// runSpike wires concrete deps and runs the harness once. Exposed for tests with
// a fake LLM (TestRunSpikeDrivesKataGreen).
func runSpike(ctx context.Context, client llm.LLM, o runOpts) (harness.Result, error) {
	if o.taskDir == "" {
		return harness.Result{}, fmt.Errorf("workspace is required")
	}

	reg := tools.NewRegistry(
		tools.NewReadTool(o.taskDir),
		tools.NewEditTool(o.taskDir),
		tools.NewWriteTool(o.taskDir),
		tools.NewGrepTool(o.taskDir),
		tools.NewGlobTool(o.taskDir),
		tools.NewGitTool(o.taskDir),
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

	sysPrompt := o.systemPrompt
	if sysPrompt == "" {
		sysPrompt = defaultSystemPrompt
	}

	cfg := harness.Config{
		Model:              o.model,
		Models:             o.models,
		Provider:           o.provider,
		Reasoning:          o.reasoning,
		SystemPrompt:       sysPrompt,
		MaxTurns:           o.maxTurns,
		MaxCostUSD:         o.maxCost,
		ContextWindow:      o.contextWindow,
		ToolOutputMaxBytes: o.toolOutputMax,
	}

	return harness.Run(ctx, client, reg, emit, o.task, cfg)
}

// runVerifyTimeout bounds a standalone `run --verify` command. Generous — a slow
// local suite must complete, not be cut off — while still bounding a hang.
const runVerifyTimeout = time.Hour

// commandCheck builds a harness.Check that runs a shell command in root with the
// shared bash-pipefail semantics. It PROBES the command first: an unrunnable
// declared command (a missing tool) is a loud error, never a silent pass or
// skip. A command that runs and exits 0 is OK; a non-zero exit is not-OK with the
// output as detail.
func commandCheck(root, command string) harness.Check {
	return func(ctx context.Context) (harness.Verdict, error) {
		if err := verifyexec.ProbeShell(root, command); err != nil {
			return harness.Verdict{}, fmt.Errorf("verify command cannot run: %w", err)
		}

		out := verifyexec.Exec(ctx, root, verifyexec.ShellArgv(command), runVerifyTimeout, nil)
		if out.StartErr {
			return harness.Verdict{}, fmt.Errorf("verify command failed to start")
		}

		return harness.Verdict{
			OK:     out.ExitCode == 0 && !out.TimedOut,
			Detail: strings.TrimSpace(out.Output),
		}, nil
	}
}

// resolveWindow best-effort fetches the catalog and returns model's context
// window (0 if unavailable, which disables context-limit detection).
// run uses the pinned --model directly; no registry or complexity selection.
func resolveWindow(ctx context.Context, client *llm.Client, model string) int {
	cat, err := client.FetchCatalog(ctx)
	if err != nil {
		return 0
	}

	e, ok := cat.Find(model)
	if !ok {
		return 0
	}

	return e.ContextLength
}

func routingRaw(c config.Config) (json.RawMessage, json.RawMessage) {
	var prov, reas json.RawMessage

	if c.Provider != nil {
		if raw, err := (llm.Provider{
			RequireParameters: c.Provider.RequireParameters,
			Order:             c.Provider.Order,
			Sort:              c.Provider.Sort,
		}).Raw(); err == nil {
			prov = raw
		}
	}

	if c.Reasoning != nil {
		if raw, err := (llm.Reasoning{
			Effort:    c.Reasoning.Effort,
			MaxTokens: c.Reasoning.MaxTokens,
			Exclude:   c.Reasoning.Exclude,
		}).Raw(); err == nil {
			reas = raw
		}
	}

	return prov, reas
}

func printResult(w io.Writer, model string, r harness.Result) {
	fmt.Fprintf(w, "\n=== result: %s ===\n", model)                                                                           //nolint:errcheck
	fmt.Fprintf(w, "completed=%v reason=%s turns=%d tool_calls=%d tool_failures=%d repairs=%d cost_usd=%.5f model_used=%s\n", //nolint:errcheck
		r.Completed, r.Reason, r.Turns, r.ToolCallCount, r.ToolCallFailures, r.RepairCount, r.TotalCostUSD, r.ModelUsed)
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}

	return *p
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}

	return *p
}
