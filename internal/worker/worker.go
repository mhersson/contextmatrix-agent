package worker

import (
	"context"
	"errors"
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
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/redact"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

// Compile-time proof that the concrete worker collaborators satisfy the
// orchestrator's consumer-side interfaces. The import edge is one-way (worker
// imports orchestrator, never the reverse), so these asserts belong here.
var (
	_ orchestrator.Ops    = (*cmclient.Client)(nil)
	_ orchestrator.GitOps = (*Git)(nil)
)

// runOrchestrator is the FSM-entry seam: production points at orchestrator.Run;
// tests swap this var to observe the Deps the worker built and script the FSM's
// outcome without spinning up the real phase loop.
var runOrchestrator = orchestrator.Run

// reviewAttemptsCap is CM's convention: a card parks after this many review
// rounds without approval.
const reviewAttemptsCap = 5

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
	ToolOutputMax         int     // CMX_TOOL_OUTPUT_MAX_BYTES; default 131072 (128 KB)
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
	defaultToolOutputMax  = 131072
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
		func() {
			// Promote: the inbox closes itself (Wait returns ErrInboxClosed), so
			// the linear loop ends and the worker bridges to the phase loop. Log
			// the hand-off so the run record shows the mid-run mode switch.
			slog.Info("promote frame received; bridging to orchestrator after linear run", "card", spec.CardID)
		},
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

	resolvedBase, err := prepareWorkspace(ctx, git, spec, branchName)
	if err != nil {
		return releaseWithError(ctx, ops, spec.CardID, err)
	}

	// prepareWorkspace resolved an empty base to the remote default and locked
	// the push policy to it. Propagate the resolved base so the FSM's review
	// diff and integrate rebase target a real ref (an empty base makes
	// `git merge-base "" HEAD` fail).
	spec.BaseBranch = resolvedBase

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

	// Autonomous cards skip the linear harness entirely and run the phase loop.
	// Route on the card's own autonomous flag in addition to the interactive
	// flag: CM forces interactive off for autonomous cards, but the agent
	// self-corrects if a stale/forced interactive flag ever arrives. The
	// promote bridge is unaffected — a promote-flow card is non-autonomous at
	// trigger time (tcx is fetched before any promote), so it still runs HITL
	// then hands off via inbox.Promoted().
	if !spec.Interactive || tcx.Autonomous {
		return runFSM(ctx, runCtx, fsmArgs{
			ops: ops, git: git, client: client, emit: emit,
			spec: spec, tcx: tcx, branch: branchName,
			ws: ws, endSession: &endSession,
		})
	}

	// 6: registry rooted at ws. keep in sync with writeTools() (the FSM's
	// model-facing toolset); the byte-for-byte linear contract forbids
	// refactoring this inline list to call it.
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

	// Promote bridge: a promote frame arrived mid-HITL and the run ended by the
	// inbox closing (not by end_session). Hand off to the phase loop, entering at
	// the persisted phase (empty -> plan; the planner sees whatever HITL pushed).
	if inbox.Promoted() && !endSession.Load() {
		return runFSM(ctx, runCtx, fsmArgs{
			ops: ops, git: git, client: client, emit: emit,
			spec: spec, tcx: tcx, branch: branchName,
			ws: ws, endSession: &endSession,
		})
	}

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
// not exist yet, so the per-card workspace path stays fresh. It returns the
// resolved base branch (the spec base, or the clone's default when the spec
// base is empty) so the caller can propagate it to the FSM.
func prepareWorkspace(ctx context.Context, git *Git, spec RunSpec, branch string) (string, error) {
	if err := os.MkdirAll(spec.Workspace, 0o755); err != nil {
		return "", fmt.Errorf("create workspace parent: %w", err)
	}

	if err := git.Clone(ctx, spec.RepoURL, spec.BaseBranch); err != nil {
		return "", fmt.Errorf("clone %s: %w", spec.RepoURL, err)
	}

	if err := git.CreateBranch(ctx, branch); err != nil {
		return "", fmt.Errorf("create branch %s: %w", branch, err)
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

	// Return the resolved base so the FSM's review diff / integrate rebase
	// target a real ref — an empty base makes `git merge-base "" HEAD` fail.
	return baseBranch, nil
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

// fsmArgs bundles what runFSM needs. ctx is the PARENT context (used for the
// graceful finalize after a canceled run); runCtx is the run-scoped context an
// end_session frame cancels — the FSM runs under it.
type fsmArgs struct {
	ops        CardOps
	git        *Git
	client     llm.LLM
	emit       *events.Emitter
	spec       RunSpec
	tcx        cmclient.TaskContext
	branch     string
	ws         string
	endSession *atomic.Bool
}

// runFSM drives the orchestrator phase loop for an autonomous (or promoted) card
// and maps its outcome to a worker Result. The heartbeat goroutine and run
// context are owned by Run; runFSM never starts or stops them. Token usage is
// reported per-phase by the orchestrator, so the park paths here do not re-report.
//
// Error mapping (spec §3.2):
//   - nil: the FSM completed the card (it called CompleteTask itself in done) ->
//     graceful "completed", no extra CompleteTask here.
//   - ReviewParkedError: graceful "completed", card left in review, NO
//     CompleteTask and NO release — a human picks it up from review.
//   - BudgetExceededError: push WIP, release the claim, return the error
//     (non-zero exit; serve emits the failed callback).
//   - ContextLimitError: identical to the budget park — push WIP, release the
//     claim, return the error — so in-flight work survives a context-window stop.
//   - ctx.Err() (end_session/kill): C1 graceful path — push WIP, release,
//     exit 0; the persisted phase stays for a later resume.
//   - any other error: release the claim and return it.
func runFSM(ctx context.Context, runCtx context.Context, a fsmArgs) (Result, error) {
	red := redact.New([]string{a.spec.OpenRouterKey, a.spec.MCPAPIKey, a.spec.GitToken})

	d := orchestrator.Deps{
		Ops:        ops2orchestrator(a.ops),
		Git:        a.git,
		PR:         NewPRCreator(a.ws, a.spec.GitToken),
		Client:     a.client,
		Emit:       a.emit,
		Registry:   buildRegistry(runCtx, a.client, a.spec),
		WriteTools: tools.NewRegistry(writeTools(a.ws, a.spec.BashTimeoutMax)...),
		ReadTools:  tools.NewReadOnlyRegistry(a.ws),
		Redact:     red.Apply,
		Cfg: orchestrator.Config{
			Project:           a.spec.Project,
			CardID:            a.spec.CardID,
			Branch:            a.branch,
			BaseBranch:        a.spec.BaseBranch,
			AgentID:           "cmx-agent-" + strings.ToLower(a.spec.CardID),
			Workspace:         a.ws,
			MaxCardCost:       a.spec.MaxCardCost,
			PriceHeadroom:     a.spec.SelectorPriceHeadroom,
			PayloadModel:      a.spec.Model,
			DefaultModel:      a.spec.DefaultModel,
			MaxTurns:          a.spec.MaxTurns,
			ToolOutputMax:     a.spec.ToolOutputMax,
			ReviewAttemptsCap: reviewAttemptsCap,
		},
	}

	err := runOrchestrator(runCtx, d)

	return mapFSMResult(ctx, a, err)
}

// mapFSMResult turns the orchestrator's terminal error into the worker outcome
// per the error-mapping contract. Split out for direct unit coverage.
func mapFSMResult(ctx context.Context, a fsmArgs, err error) (Result, error) {
	switch {
	case err == nil:
		// The done phase completed the card itself; nothing more to do.
		return Result{Reason: "completed"}, nil

	case isReviewParked(err):
		// Parked, not failed: the card stays in review for a human. No
		// CompleteTask, no release.
		slog.Info("review parked; leaving card in review", "card", a.spec.CardID)

		return Result{Reason: "completed"}, nil

	case isBudgetExceeded(err):
		// Push the partial work so a human (or resume) can pick it up, then fail.
		// The budget numbers are already logged by the orchestrator, and usage was
		// reported per-phase as it was spent; release the claim and surface the
		// error so serve emits the failed callback.
		pushWIP(ctx, a)
		releaseQuietly(ctx, a.ops, a.spec.CardID)

		return Result{Reason: "error"}, fmt.Errorf("orchestrator: %w", err)

	case isContextLimit(err):
		// Context-window park: identical shape to the budget arm. Push the
		// partial work so a human (or resume) can pick it up, release the claim,
		// and surface the error so serve emits the failed callback. The orchestrator
		// already logged the park line; the worker re-reports neither usage nor log.
		pushWIP(ctx, a)
		releaseQuietly(ctx, a.ops, a.spec.CardID)

		return Result{Reason: "error"}, fmt.Errorf("orchestrator: %w", err)

	case a.endSession.Load() || ctx.Err() != nil || errorsIsCanceled(err):
		// end_session / kill mid-FSM: the C1 graceful park. Push whatever WIP
		// exists, release the claim, exit 0. Usage was already reported per-phase
		// by the orchestrator; the persisted phase stays so a later run resumes
		// from it.
		pushWIP(ctx, a)
		releaseQuietly(ctx, a.ops, a.spec.CardID)

		return Result{Reason: "end_session"}, nil

	default:
		releaseQuietly(ctx, a.ops, a.spec.CardID)

		return Result{Reason: "error"}, fmt.Errorf("orchestrator: %w", err)
	}
}

// isReviewParked reports whether err is the orchestrator's review-park sentinel.
func isReviewParked(err error) bool {
	var rp *orchestrator.ReviewParkedError

	return errors.As(err, &rp)
}

// isBudgetExceeded reports whether err is the orchestrator's budget-ceiling sentinel.
func isBudgetExceeded(err error) bool {
	var be *orchestrator.BudgetExceededError

	return errors.As(err, &be)
}

// isContextLimit reports whether err is (or wraps) the orchestrator's
// context-window sentinel.
func isContextLimit(err error) bool {
	var cle *orchestrator.ContextLimitError

	return errors.As(err, &cle)
}

// errorsIsCanceled reports whether err is (or wraps) context cancellation, which
// is what the FSM returns when an end_session frame cancels its run context.
func errorsIsCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// pushWIP commits any dirty tree and pushes the card branch on the PARENT ctx
// (runCtx may already be canceled). Best-effort: a failure is logged, not fatal —
// the park/fail outcome must still surface.
func pushWIP(ctx context.Context, a fsmArgs) {
	dirty, err := a.git.CommitIfDirty(ctx, a.tcx.Title, a.spec.CardID)
	if err != nil {
		slog.Warn("WIP commit failed", "card", a.spec.CardID, "error", err)

		return
	}

	if !dirty {
		return
	}

	if err := a.git.Push(ctx, a.branch); err != nil {
		slog.Warn("WIP push failed", "card", a.spec.CardID, "error", err)

		return
	}

	if err := a.ops.ReportPush(ctx, a.spec.CardID, a.branch, ""); err != nil {
		slog.Warn("report WIP push failed", "card", a.spec.CardID, "error", err)
	}
}

// releaseQuietly releases the claim, logging a failure rather than masking the
// run outcome.
func releaseQuietly(ctx context.Context, ops CardOps, cardID string) {
	if err := ops.ReleaseCard(ctx, cardID); err != nil {
		slog.Warn("release card failed", "card", cardID, "error", err)
	}
}

// ops2orchestrator widens the worker's narrow CardOps to the orchestrator's Ops
// surface. In production ops is *cmclient.Client, which satisfies both; the
// assertion is comma-ok so a test fake that only implements CardOps yields a nil
// Ops (harmless: such tests swap runOrchestrator and never touch Deps.Ops).
func ops2orchestrator(ops CardOps) orchestrator.Ops {
	if oo, ok := ops.(orchestrator.Ops); ok {
		return oo
	}

	return nil
}

// writeTools is the full model-facing toolset rooted at ws, matching the linear
// path's registry so the FSM coder has the same capabilities.
func writeTools(ws string, bashTimeoutMax int) []tools.Tool {
	return []tools.Tool{
		tools.NewReadTool(ws),
		tools.NewEditTool(ws),
		tools.NewWriteTool(ws),
		tools.NewGrepTool(ws),
		tools.NewGlobTool(ws),
		tools.NewGitTool(ws),
		tools.NewBashTool(ws).WithMaxTimeout(bashTimeoutMax),
	}
}

// buildRegistry assembles the model registry the FSM selects from: the embedded
// capabilities baseline (with its calibrated floor) plus the embedded priors,
// backed by the live OpenRouter catalog. A catalog-fetch failure degrades to an
// empty catalog — selection still works off priors/capabilities, and the
// harness enforces context limits at runtime.
func buildRegistry(ctx context.Context, client llm.LLM, spec RunSpec) *registry.Registry {
	var cat llm.Catalog

	if fetcher, ok := client.(catalogFetcher); ok {
		if fetched, err := fetcher.FetchCatalog(ctx); err == nil {
			cat = fetched
		} else {
			slog.Warn("catalog fetch failed; selecting from priors/capabilities only", "error", err)
		}
	}

	caps, meta := registry.DefaultCapabilities()

	return registry.NewRegistryWithCapabilities(nil, spec.DefaultModel, cat, caps).
		WithSelection(registry.Selection{
			Priors:        registry.DefaultPriors(),
			Floor:         meta.Floor,
			PriceHeadroom: spec.SelectorPriceHeadroom,
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
