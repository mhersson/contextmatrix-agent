package modelexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/mhersson/contextmatrix-protocol/selection"
	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
)

// ClaudeConfig configures the Claude Code executor.
type ClaudeConfig struct {
	OAuthToken string // required
	// Env is the base env of claude and its Bash children: the scrubbed bash env
	// plus the passthrough variables and the bash timeout mapping (see ClaudeEnv). A nil Env leaves the child
	// without PATH, so production always passes ClaudeEnv(...).
	Env    []string
	Effort string // the claude_code endpoint's effort; NewClaude passes it through claudecode.NormalizeEffort
	// Home is Spec.Home of every run; "" = os.UserHomeDir(). It is the worker
	// user's real HOME, not a temp dir: the commands the Bash tool runs inherit
	// it, and a temp HOME would hide git's global config (credential helper),
	// GOPATH/GOCACHE, the gh config and package caches. Panel children and
	// Best-of-N candidates run concurrently in it (vendorkit names its per-run
	// files uniquely), and resume needs the same Home and Dir. vendorkit never
	// prunes Home/.vendorkit/sessions: the container is disposed after the
	// card run, so its lifetime is the retention. The Home is never created
	// or removed here.
	Home string
}

// bashDefaultCeilingMS is Claude Code's default Bash timeout.
const bashDefaultCeilingMS = 120000

// claudeEnvPassthrough is the worker env claude itself needs to reach
// Anthropic, beyond the scrubbed bash env.
var claudeEnvPassthrough = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// ClaudeEnv returns the scrubbed bash env plus each set passthrough variable,
// plus NODE_EXTRA_CA_CERTS=<caFile> when the worker has a CA bundle, plus the
// bash timeout knob as Claude Code's Bash ceiling (BASH_MAX_TIMEOUT_MS) and,
// below its 2-minute default, its default (BASH_DEFAULT_TIMEOUT_MS). Every key
// appears once; a later value wins. vendorkit adds the loopback hosts to
// NO_PROXY when it builds the child env.
func ClaudeEnv(scrubbed []string, caFile string, bashTimeoutMaxSeconds int) []string {
	add := make([]string, 0, len(scrubbed)+len(claudeEnvPassthrough)+3)
	add = append(add, scrubbed...)

	for _, k := range claudeEnvPassthrough {
		if v, ok := os.LookupEnv(k); ok {
			add = append(add, k+"="+v)
		}
	}

	if caFile != "" {
		add = append(add, "NODE_EXTRA_CA_CERTS="+caFile)
	}

	if bashTimeoutMaxSeconds > 0 {
		ms := bashTimeoutMaxSeconds * 1000
		add = append(add, "BASH_MAX_TIMEOUT_MS="+strconv.Itoa(ms))

		// A ceiling below Claude Code's 2-minute default has no effect unless
		// the default is lowered with it.
		if ms < bashDefaultCeilingMS {
			add = append(add, "BASH_DEFAULT_TIMEOUT_MS="+strconv.Itoa(ms))
		}
	}

	idx := make(map[string]int, len(add))
	out := make([]string, 0, len(add))

	for _, e := range add {
		k, _, _ := strings.Cut(e, "=")
		if i, ok := idx[k]; ok {
			out[i] = e

			continue
		}

		idx[k] = len(out)
		out = append(out, e)
	}

	return out
}

// Claude runs model work through contextmatrix-vendorkit's claudecode.Run: the
// pinned `claude` binary on the operator's subscription.
type Claude struct {
	token  string
	env    []string
	effort string
	home   string
	run    func(context.Context, claudecode.Spec) (harness.Result, error)

	mu     sync.Mutex
	limits []cmclient.SubscriptionUsage
}

var _ orchestrator.Executor = (*Claude)(nil)

// NewClaude validates cfg and resolves Home once so every run, and any resume,
// uses the same one. It never creates or removes Home.
func NewClaude(cfg ClaudeConfig) (*Claude, error) {
	return newClaude(cfg, claudecode.Run)
}

func newClaude(cfg ClaudeConfig, run func(context.Context, claudecode.Spec) (harness.Result, error)) (*Claude, error) {
	if cfg.OAuthToken == "" {
		return nil, errors.New("claude executor: OAuthToken is required")
	}

	home := cfg.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("claude executor: resolve home: %w", err)
		}

		home = h
	}

	effort := claudecode.NormalizeEffort(cfg.Effort)
	if effort == "" && strings.TrimSpace(cfg.Effort) != "" {
		slog.Warn("claude executor: effort not accepted by Claude Code, using its default", "effort", cfg.Effort)
	}

	return &Claude{token: cfg.OAuthToken, env: cfg.Env, effort: effort, home: home, run: run}, nil
}

// Run maps req onto a claudecode.Spec. Repo grounding is already in the task
// prompts the orchestrator builds, so SystemPromptAppend carries only the
// seat's own system prompt.
func (c *Claude) Run(ctx context.Context, req orchestrator.RunRequest) (harness.Result, error) {
	model, ok := selection.ClaudeCodeModelID(req.Config.Model)
	if !ok {
		return harness.Result{}, fmt.Errorf("claude executor: model %q has no Claude Code id", req.Config.Model)
	}

	builtins, bridged := claudecode.SplitRegistry(req.Registry)

	bridged = wrapBridged(bridged, req.Config.RedactToolOutput, req.Config.ToolOutputMaxBytes)

	task := req.Task
	if len(req.Config.History) > 0 && req.Resume == "" {
		task = claudecode.RenderTranscript(req.Config.History) + "\n\n" + req.Task
	}

	res, err := c.run(ctx, claudecode.Spec{
		Dir:                req.Dir,
		Home:               c.home,
		OAuthToken:         c.token,
		Env:                c.env,
		Model:              model,
		Task:               task,
		TaskImages:         req.Config.TaskImages,
		SystemPromptAppend: req.Config.SystemPrompt,
		BuiltinTools:       builtins,
		Tools:              bridged,
		MaxTurns:           req.Config.MaxTurns,
		MaxBudgetUSD:       req.Config.MaxCostUSD,
		Effort:             c.effort,
		SessionID:          req.SessionID,
		Resume:             req.Resume,
		Emitter:            req.Emit,
		Redact:             req.Config.RedactToolOutput,
		OnDelta:            req.Config.OnDelta,
		OnRateLimit:        c.storeRateLimit,
	})
	if err != nil {
		if ule, ok := errors.AsType[*claudecode.UsageLimitError](err); ok {
			return res, &orchestrator.UsageLimitError{Window: ule.Window, ResetsAt: ule.ResetsAt}
		}

		return res, fmt.Errorf("claude executor: %w", err)
	}

	if res.Completed && strings.TrimSpace(res.Output) == "" && res.CompletionArgs == nil {
		return res, harness.ErrEmptyResponse
	}

	return res, nil
}

// wrapBridged redacts and caps the output of every bridged tool: the bridge
// hands results to claude whole, and the harness loop that does this in
// endpoint mode does not run.
func wrapBridged(reg *tools.Registry, redact func(string) string, maxBytes int) *tools.Registry {
	if reg == nil {
		return nil
	}

	if redact == nil {
		redact = func(s string) string { return s }
	}

	all := reg.All()
	wrapped := make([]tools.Tool, len(all))

	for i, t := range all {
		wrapped[i] = bridgedTool{Tool: t, redact: redact, maxBytes: maxBytes}
	}

	return tools.NewRegistry(wrapped...)
}

type bridgedTool struct {
	tools.Tool

	redact   func(string) string
	maxBytes int
}

func (b bridgedTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	res, err := b.Tool.Execute(ctx, args)
	res.Text = tools.HeadTail(b.redact(res.Text), b.maxBytes)

	if err != nil {
		// An unchanged error keeps its wrapping.
		if msg := tools.HeadTail(b.redact(err.Error()), b.maxBytes); msg != err.Error() {
			err = errors.New(msg)
		}
	}

	return res, err
}

// Terminal forwards the inner tool's, which the bridge reads to end the run.
func (b bridgedTool) Terminal() bool {
	term, ok := b.Tool.(tools.Terminal)

	return ok && term.Terminal()
}

// storeRateLimit only stores a copy: vendorkit calls it on the stream reader.
func (c *Claude) storeRateLimit(rl claudecode.RateLimit) {
	windows := make([]cmclient.SubscriptionUsage, 0, len(rl.Windows))

	for _, w := range rl.Windows {
		if !w.ResetsAt.IsZero() {
			windows = append(windows, w)
		}
	}

	c.mu.Lock()
	c.limits = windows
	c.mu.Unlock()
}

// SubscriptionUsage returns a copy of the last-seen limit windows; nil before any.
func (c *Claude) SubscriptionUsage() []cmclient.SubscriptionUsage {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.limits == nil {
		return nil
	}

	return append([]cmclient.SubscriptionUsage(nil), c.limits...)
}

// SpawnSubagents runs one read-only Claude Code run per spec, concurrently,
// and returns the results in spec order.
func (c *Claude) SpawnSubagents(ctx context.Context, req orchestrator.FanoutRequest) ([]harness.SubagentResult, error) {
	reg := tools.NewRegistry(append(tools.ReadOnlyTools(req.Root), req.Opts.ExtraReadOnlyTools...)...)

	req.Emit.Emit(events.StateChange, map[string]any{"spawn_subagents": len(req.Specs)})

	results := make([]harness.SubagentResult, len(req.Specs))

	var wg sync.WaitGroup

	for i, spec := range req.Specs {
		wg.Go(func() {
			model := spec.Model
			if model == "" {
				model = req.Opts.DefaultModel
			}

			res, err := c.Run(ctx, orchestrator.RunRequest{
				Registry: reg,
				Emit:     req.Emit,
				Task:     spec.Prompt,
				Dir:      req.Root,
				Config: harness.Config{
					Model:              model,
					MaxTurns:           spec.MaxTurns,
					RedactToolOutput:   req.Opts.RedactToolOutput,
					ToolOutputMaxBytes: req.Opts.ToolOutputMaxBytes,
					OnDelta:            req.OnDelta,
					MaxCostUSD:         req.BudgetUSD,
				},
			})
			results[i] = harness.SubagentResult{Role: spec.Role, Output: res.Output, Result: res, Err: err}
		})
	}

	wg.Wait()

	return results, nil
}

// CheckClaudeCode checks the `claude` on PATH. It fails only when no runnable
// binary is found; it logs the found version, as a warning when it differs
// from the tested one.
func CheckClaudeCode(ctx context.Context, logger *slog.Logger) error {
	return checkClaudeCode(ctx, logger, claudecode.CheckVersion)
}

func checkClaudeCode(ctx context.Context, logger *slog.Logger, check func(context.Context, string) (string, bool, error)) error {
	found, tested, err := check(ctx, "claude")
	if err != nil {
		return fmt.Errorf("worker image lacks a usable claude (tested version %s): %w", claudecode.Version, err)
	}

	if !tested {
		logger.Warn("claude version differs from the tested one", "found", found, "tested", claudecode.Version)

		return nil
	}

	logger.Info("claude found", "version", found)

	return nil
}
