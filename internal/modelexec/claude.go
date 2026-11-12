package modelexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
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
	// plus the passthrough variables (see ClaudeEnv). A nil Env leaves the child
	// without PATH, so production always passes ClaudeEnv(...).
	Env    []string
	Effort string // the worker's reasoning effort; NewClaude passes it through claudecode.NormalizeEffort
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

// claudeEnvPassthrough is the worker env claude itself needs to reach
// Anthropic, beyond the scrubbed bash env.
var claudeEnvPassthrough = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// claudeLoopback must bypass any proxy: claude's MCP bridge and its stop hook
// listen on loopback.
var claudeLoopback = []string{"127.0.0.1", "localhost", "::1"}

// ClaudeEnv returns the scrubbed bash env plus each set passthrough variable,
// plus NODE_EXTRA_CA_CERTS=<caFile> when the worker has a CA bundle. With a
// proxy set, NO_PROXY and no_proxy both gain the loopback hosts they lack; an
// unset one starts from the other's value. Every key appears once; a later
// value wins.
func ClaudeEnv(scrubbed []string, caFile string) []string {
	add := make([]string, 0, len(scrubbed)+len(claudeEnvPassthrough)+1)
	add = append(add, scrubbed...)

	for _, k := range claudeEnvPassthrough {
		if v, ok := os.LookupEnv(k); ok {
			add = append(add, k+"="+v)
		}
	}

	if caFile != "" {
		add = append(add, "NODE_EXTRA_CA_CERTS="+caFile)
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

	return withLoopbackNoProxy(out, idx)
}

// withLoopbackNoProxy extends env's NO_PROXY and no_proxy with claudeLoopback
// when a proxy is set. idx maps each key of env to its index.
func withLoopbackNoProxy(env []string, idx map[string]int) []string {
	value := func(k string) (string, bool) {
		i, ok := idx[k]
		if !ok {
			return "", false
		}

		_, v, _ := strings.Cut(env[i], "=")

		return v, true
	}

	proxied := false

	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if v, _ := value(k); v != "" {
			proxied = true
		}
	}

	if !proxied {
		return env
	}

	upper, hasUpper := value("NO_PROXY")
	lower, hasLower := value("no_proxy")

	if !hasUpper {
		upper = lower
	}

	if !hasLower {
		lower = upper
	}

	for _, kv := range [][2]string{{"NO_PROXY", upper}, {"no_proxy", lower}} {
		e := kv[0] + "=" + appendNoProxy(kv[1], claudeLoopback)
		if i, ok := idx[kv[0]]; ok {
			env[i] = e

			continue
		}

		env = append(env, e)
	}

	return env
}

// appendNoProxy appends each host list lacks, compared case-insensitively.
func appendNoProxy(list string, hosts []string) string {
	entries := make([]string, 0, len(hosts)+1)

	for e := range strings.SplitSeq(list, ",") {
		if e = strings.TrimSpace(e); e != "" {
			entries = append(entries, e)
		}
	}

	for _, h := range hosts {
		if !slices.ContainsFunc(entries, func(e string) bool { return strings.EqualFold(e, h) }) {
			entries = append(entries, h)
		}
	}

	return strings.Join(entries, ",")
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
		slog.Warn("claude executor: reasoning effort not accepted by Claude Code, using its default", "effort", cfg.Effort)
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
					Model:            model,
					MaxTurns:         spec.MaxTurns,
					RedactToolOutput: req.Opts.RedactToolOutput,
					OnDelta:          req.OnDelta,
					MaxCostUSD:       req.BudgetUSD,
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
