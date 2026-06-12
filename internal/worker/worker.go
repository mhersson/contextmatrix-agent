package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/redact"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

// RunSpec is the container-side contract: populated from CM_* env by the
// work command.
type RunSpec struct {
	CardID        string // CM_CARD_ID (required)
	Project       string // CM_PROJECT (required)
	RepoURL       string // CM_REPO_URL (required)
	BaseBranch    string // CM_BASE_BRANCH (optional)
	Interactive   bool   // CM_INTERACTIVE ("true")
	Model         string // CM_MODEL (optional; honored if catalog-resolvable)
	MCPURL        string // CM_MCP_URL (required)
	MCPAPIKey     string // CM_MCP_API_KEY (required)
	CorrelationID string // CM_CORRELATION_ID (optional)

	OpenRouterKey string // from /run/cm-secrets/env via the secrets source
	GitToken      string // from /run/cm-secrets/env via the secrets source

	BashTimeoutMax        int     // CMX_BASH_TIMEOUT_MAX_SECONDS; default 600
	ToolOutputMax         int     // CMX_TOOL_OUTPUT_MAX_BYTES; default 30000
	MaxTurns              int     // CMX_MAX_TURNS
	MaxCostUSD            float64 // CMX_MAX_COST_USD
	MaxCardCost           float64 // CMX_MAX_CARD_COST; 0 disables
	SelectorPriceHeadroom float64 // CMX_SELECTOR_PRICE_HEADROOM; 0 uses worker default
	DefaultModel          string  // CMX_DEFAULT_MODEL; fallback when Model is absent/unresolvable
	Workspace             string  // CMX_WORKSPACE; parent dir for the clone (default /home/user/workspace)
}

// CardOps is the slice of cmclient the worker needs (interface here, where
// it's consumed, so tests fake it without MCP).
type CardOps interface {
	ClaimCard(ctx context.Context, cardID string) error
	GetTaskContext(ctx context.Context, cardID string) (cmclient.TaskContext, error)
	Heartbeat(ctx context.Context, cardID string) error
	ReportUsage(ctx context.Context, cardID, model string, promptTokens, completionTokens int64, actualCostUSD float64) error
	ReportPush(ctx context.Context, cardID, branch, prURL string) error
	CompleteTask(ctx context.Context, cardID, summary string) error
	ReleaseCard(ctx context.Context, cardID string) error
}

// catalogFetcher is the catalog-lookup seam, satisfied by *llm.Client. It is
// defined here, where it is consumed, so model resolution stays best-effort:
// a client that cannot fetch the catalog simply skips the lookup and the run
// falls back to the default model.
type catalogFetcher interface {
	FetchCatalog(ctx context.Context) (llm.Catalog, error)
}

// Result is the worker's outcome. Reason distinguishes a graceful finish from
// an end-session park and a hard error.
type Result struct {
	Reason string // completed | end_session | error
}

// heartbeatInterval is a var so tests can shrink it.
var heartbeatInterval = 5 * time.Minute

const (
	defaultBashTimeoutMax = 600
	defaultToolOutputMax  = 30000
	defaultWorkspace      = "/home/user/workspace"
	summaryMaxLen         = 200
)

const taskTemplate = `You are an automated coding agent working on a task from a kanban card.

Card %s: %s

%s

Work in the current directory (the repository is already cloned on a work
branch). Make the change, verify it builds and its tests pass, then stop.
Do not commit or push — that happens automatically after you finish.`

const interactiveSuffix = "\n\nA human teammate may send you messages mid-run; treat them as corrections or added requirements."

// Run executes the linear, card-scoped sequence for one container: clone the
// repo on a work branch, claim the card, fetch its context, drive the harness
// loop, then finalize (commit/push/report/complete or release). It builds the
// Inbox and the run-scoped context internally so the inbox liveness contract
// holds: an end_session frame cancels runCtx, waking any parked Wait.
func Run(ctx context.Context, spec RunSpec, ops CardOps, client llm.LLM, emit *events.Emitter, stdin io.Reader) (Result, error) {
	spec = withDefaults(spec)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	var endSession atomic.Bool

	inbox := NewInbox(
		spec.Interactive,
		func() {}, // promote: the inbox closes itself; no extra action here
		func() {
			// Uniform across modes: the host holds the container's stdin
			// attach open for the container's whole life, so end_session or
			// EOF always means "session over" — finalize without completing.
			// Canceling runCtx wakes a parked Wait (the liveness contract)
			// and aborts an in-flight autonomous turn alike.
			endSession.Store(true)
			cancelRun()
		},
	)

	go inbox.Pump(stdin)

	branchName := "cm/" + strings.ToLower(spec.CardID)

	red := redact.New([]string{spec.OpenRouterKey, spec.MCPAPIKey, spec.GitToken})

	// 1-2: workspace + clone + branch.
	ws := filepath.Join(spec.Workspace, strings.ToLower(spec.CardID))

	git := NewGit(ws, spec.GitToken)

	if err := prepareWorkspace(ctx, git, spec, branchName); err != nil {
		return releaseWithError(ctx, ops, spec.CardID, err)
	}

	// 3: claim + context.
	if err := ops.ClaimCard(ctx, spec.CardID); err != nil {
		return releaseWithError(ctx, ops, spec.CardID, fmt.Errorf("claim card: %w", err))
	}

	tcx, err := ops.GetTaskContext(ctx, spec.CardID)
	if err != nil {
		return releaseWithError(ctx, ops, spec.CardID, fmt.Errorf("get task context: %w", err))
	}

	// 4: heartbeat goroutine for the whole run, including human waits.
	stopHeartbeat := startHeartbeat(runCtx, ops, spec.CardID)
	defer stopHeartbeat()

	// 5: model resolution.
	model := resolveModel(ctx, client, emit, spec)

	// 6: registry rooted at ws.
	reg := tools.NewRegistry(
		tools.NewReadTool(ws),
		tools.NewEditTool(ws),
		tools.NewWriteTool(ws),
		tools.NewGrepTool(ws),
		tools.NewGlobTool(ws),
		tools.NewGitTool(ws),
		tools.NewBashTool(ws).WithMaxTimeout(spec.BashTimeoutMax),
	)

	// 7: task prompt.
	task := fmt.Sprintf(taskTemplate, spec.CardID, tcx.Title, tcx.Description)
	if spec.Interactive {
		task += interactiveSuffix
	}

	// 8: harness loop.
	res, runErr := harness.Run(runCtx, client, reg, emit, task, harness.Config{
		Model:              model,
		MaxTurns:           spec.MaxTurns,
		MaxCostUSD:         spec.MaxCostUSD,
		Inbox:              inbox,
		RedactToolOutput:   red.Apply,
		ToolOutputMaxBytes: spec.ToolOutputMax,
	})

	// 9: finalize on the PARENT ctx — runCtx may be canceled by end_session.
	return finalize(ctx, finalizeArgs{
		ops:        ops,
		git:        git,
		spec:       spec,
		tcx:        tcx,
		branch:     branchName,
		model:      model,
		res:        res,
		runErr:     runErr,
		endSession: endSession.Load(),
	})
}

// prepareWorkspace creates the clone parent, clones the repo, and cuts the work
// branch. Clone requires the parent dir to exist and the workspace itself to
// not exist yet, so the per-card workspace path stays fresh.
func prepareWorkspace(ctx context.Context, git *Git, spec RunSpec, branch string) error {
	if err := os.MkdirAll(spec.Workspace, 0o755); err != nil {
		return fmt.Errorf("create workspace parent: %w", err)
	}

	if err := git.Clone(ctx, spec.RepoURL, spec.BaseBranch); err != nil {
		return fmt.Errorf("clone %s: %w", spec.RepoURL, err)
	}

	if err := git.CreateBranch(ctx, branch); err != nil {
		return fmt.Errorf("create branch %s: %w", branch, err)
	}

	// Lock the push policy now: the run owns exactly this branch. baseBranch
	// is the spec's base or the clone's default; remoteDefault is best-effort
	// from origin/HEAD. All three are protected against force-push.
	remoteDefault := git.RemoteDefaultBranch(ctx)

	baseBranch := spec.BaseBranch
	if baseBranch == "" {
		baseBranch = remoteDefault
	}

	git.SetBranchPolicy(branch, baseBranch, remoteDefault)

	return nil
}

// startHeartbeat ticks ops.Heartbeat on heartbeatInterval until the returned
// stop func is called. Heartbeat failures are logged, not fatal: a transient
// MCP hiccup must not abort an otherwise healthy run.
func startHeartbeat(ctx context.Context, ops CardOps, cardID string) func() {
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := ops.Heartbeat(ctx, cardID); err != nil {
					slog.Warn("heartbeat failed", "card", cardID, "error", err)
				}
			}
		}
	}()

	var once atomic.Bool

	return func() {
		if once.CompareAndSwap(false, true) {
			close(done)
		}
	}
}

// resolveModel picks the harness model. An explicit, catalog-resolvable slug
// wins; an unresolvable slug or a catalog-fetch error falls back to the default
// with a warning state-change event (a catalog hiccup must not fail the run).
// An empty spec.Model uses the default silently.
func resolveModel(ctx context.Context, client llm.LLM, emit *events.Emitter, spec RunSpec) string {
	if spec.Model == "" {
		return spec.DefaultModel
	}

	fetcher, ok := client.(catalogFetcher)
	if !ok {
		emitModelFallback(emit, spec)

		return spec.DefaultModel
	}

	cat, err := fetcher.FetchCatalog(ctx)
	if err != nil {
		emitModelFallback(emit, spec)

		return spec.DefaultModel
	}

	if _, found := cat.Find(spec.Model); !found {
		emitModelFallback(emit, spec)

		return spec.DefaultModel
	}

	return spec.Model
}

func emitModelFallback(emit *events.Emitter, spec RunSpec) {
	emit.Emit(events.StateChange, map[string]any{
		"warning":   "model not in catalog, using default",
		"requested": spec.Model,
		"using":     spec.DefaultModel,
	})
}

type finalizeArgs struct {
	ops        CardOps
	git        *Git
	spec       RunSpec
	tcx        cmclient.TaskContext
	branch     string
	model      string
	res        harness.Result
	runErr     error
	endSession bool
}

// finalize runs the end-of-run git + reporting sequence on the parent ctx. It
// commits if the tree is dirty, pushes and reports the push, always reports
// usage, then decides the card's fate: release on end-session (graceful),
// complete on a finished run, or release + error otherwise.
func finalize(ctx context.Context, a finalizeArgs) (Result, error) {
	finishedClean := a.runErr == nil && a.res.Completed
	shouldCommit := a.endSession || finishedClean

	if shouldCommit {
		if err := commitPushReport(ctx, a); err != nil {
			slog.Warn("commit/push/report failed", "card", a.spec.CardID, "error", err)
		}
	}

	reportUsage(ctx, a)

	switch {
	case a.endSession:
		if err := a.ops.ReleaseCard(ctx, a.spec.CardID); err != nil {
			slog.Warn("release card failed", "card", a.spec.CardID, "error", err)
		}

		return Result{Reason: "end_session"}, nil

	case finishedClean:
		summary := summaryFrom(a.res, a.tcx)
		if err := a.ops.CompleteTask(ctx, a.spec.CardID, summary); err != nil {
			return Result{Reason: "error"}, fmt.Errorf("complete task: %w", err)
		}

		return Result{Reason: "completed"}, nil

	default:
		if err := a.ops.ReleaseCard(ctx, a.spec.CardID); err != nil {
			slog.Warn("release card failed", "card", a.spec.CardID, "error", err)
		}

		if a.runErr != nil {
			return Result{Reason: "error"}, fmt.Errorf("harness run: %w", a.runErr)
		}

		return Result{Reason: "error"}, fmt.Errorf("run did not complete: %s", a.res.Reason)
	}
}

// commitPushReport commits any changes and, if a commit was made, pushes the
// branch and reports the push to ContextMatrix.
func commitPushReport(ctx context.Context, a finalizeArgs) error {
	dirty, err := a.git.CommitIfDirty(ctx, a.tcx.Title, a.spec.CardID)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	if !dirty {
		return nil
	}

	if err := a.git.Push(ctx, a.branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	if err := a.ops.ReportPush(ctx, a.spec.CardID, a.branch, ""); err != nil {
		return fmt.Errorf("report push: %w", err)
	}

	return nil
}

// reportUsage reports token usage (tokens were spent on every path). Best
// effort: a failure is logged, never masks the run's outcome.
func reportUsage(ctx context.Context, a finalizeArgs) {
	if err := a.ops.ReportUsage(ctx, a.spec.CardID, a.model, a.res.PromptTokens, a.res.CompletionTokens, 0); err != nil {
		slog.Warn("report usage failed", "card", a.spec.CardID, "error", err)
	}
}

// releaseWithError best-effort releases the claim and returns an error result.
// Used on setup failures before the harness loop runs.
func releaseWithError(ctx context.Context, ops CardOps, cardID string, err error) (Result, error) {
	if relErr := ops.ReleaseCard(ctx, cardID); relErr != nil {
		slog.Warn("release card failed", "card", cardID, "error", relErr)
	}

	return Result{Reason: "error"}, err
}

// summaryFrom builds the completion summary from the harness output: the first
// line, capped at 200 chars, falling back to the card title when the output is
// empty.
func summaryFrom(res harness.Result, tcx cmclient.TaskContext) string {
	line := strings.TrimSpace(res.Output)
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}

	line = strings.TrimSpace(line)
	if line == "" {
		return tcx.Title
	}

	if len(line) > summaryMaxLen {
		// Back off past any UTF-8 continuation bytes so the byte cut never
		// splits a multi-byte rune (the summary must be valid UTF-8).
		cut := summaryMaxLen
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}

		line = line[:cut]
	}

	return line
}

// withDefaults fills unset spec fields with their documented defaults.
func withDefaults(spec RunSpec) RunSpec {
	if spec.BashTimeoutMax <= 0 {
		spec.BashTimeoutMax = defaultBashTimeoutMax
	}

	if spec.ToolOutputMax <= 0 {
		spec.ToolOutputMax = defaultToolOutputMax
	}

	if spec.Workspace == "" {
		spec.Workspace = defaultWorkspace
	}

	return spec
}
