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

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
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
// rounds without approval. Three matches the runner's MAX_REVISION_PASSES; with
// the convergence safeguards in place, three rounds are enough.
const reviewAttemptsCap = 3

// RunSpec is the container-side contract: populated from CM_* env by the
// work command.
type RunSpec struct {
	CardID      string // CM_CARD_ID (required)
	Project     string // CM_PROJECT (required)
	RepoURL     string // CM_REPO_URL (required)
	BaseBranch  string // CM_BASE_BRANCH (optional)
	Interactive bool   // CM_INTERACTIVE ("true")
	BestOfN     int    // CM_BEST_OF_N; >= 2 races N candidate implementations (0 = normal run)
	Model       string // CM_MODEL (optional; honored if catalog-resolvable)
	MCPURL      string // CM_MCP_URL (required)
	MCPAPIKey   string // CM_MCP_API_KEY (required)

	LLMKey     string // from /run/cm-secrets/env via the secrets source
	LLMBaseURL string // from /run/cm-secrets/env via the secrets source
	LLMType    string // from /run/cm-secrets/env via the secrets source
	GitToken   string // from /run/cm-secrets/env via the secrets source; startup value, used for redaction and to gate credential injection

	// SecretsEnvPath is the KEY=value secrets file (/run/cm-secrets/env) that git
	// and gh re-read the current CM_GIT_TOKEN from per operation, so a token the
	// host rotates on disk reaches a long-running worker without a restart.
	SecretsEnvPath string

	BashTimeoutMax        int     // CMX_BASH_TIMEOUT_MAX_SECONDS; default 600
	ToolOutputMax         int     // CMX_TOOL_OUTPUT_MAX_BYTES; default 131072 (128 KB)
	MaxTurns              int     // CMX_MAX_TURNS
	MaxCardCost           float64 // CMX_MAX_CARD_COST; 0 disables
	SelectorPriceHeadroom float64 // CMX_SELECTOR_PRICE_HEADROOM; 0 uses worker default

	CompactionEnabled         bool    // CMX_COMPACTION_ENABLED; false (default) keeps the hard context_limit stop
	CompactionThreshold       float64 // CMX_COMPACTION_THRESHOLD; fraction of the context window (default 0.85)
	CompactionKeepRecentTurns int     // CMX_COMPACTION_KEEP_RECENT_TURNS; recent turns kept verbatim (default 6)

	DefaultModel    string // CMX_DEFAULT_MODEL; fallback when Model is absent/unresolvable
	ReasoningEffort string // CMX_REASONING_EFFORT; empty = off (no reasoning overhead)
	Workspace       string // CMX_WORKSPACE; parent dir for the clone (default /home/user/workspace)
	CACertFile      string // CMX_CA_CERT_FILE; in-container path to an extra CA PEM (empty = disabled)

	// Selection carries the CM-resolved model selection inputs (candidates,
	// favorites, blacklist). Nil when absent (runner backend or old CM).
	Selection *protocol.SelectionContext // CMX_SELECTION (JSON-encoded)

	TaskSkillsDir string   // in-container skills mount path (CMX_TASK_SKILLS_DIR); empty = no skills
	TaskSkills    []string // per-card subset (CM_TASK_SKILLS)
	TaskSkillsSet bool     // whether the subset was set (CM_TASK_SKILLS_SET)
}

// CardOps is the slice of cmclient the worker needs (interface here, where
// it's consumed, so tests fake it without MCP).
type CardOps interface {
	ClaimCard(ctx context.Context, cardID string) error
	GetTaskContext(ctx context.Context, cardID string, includeImages bool) (cmclient.TaskContext, error)
	Heartbeat(ctx context.Context, cardID string) error
	ReportUsage(ctx context.Context, cardID, model string, promptTokens, completionTokens int64, actualCostUSD float64) error
	ReportPush(ctx context.Context, cardID, branch, prURL string) error
	CompleteTask(ctx context.Context, cardID, summary string) error
	ReleaseCard(ctx context.Context, cardID string) error
	RecordSkillEngaged(ctx context.Context, cardID, skillName string) error
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
)

// Run executes the card-scoped sequence for one container: clone the repo on
// a work branch, claim the card, fetch its context, drive the FSM, then
// finalize. It builds the Inbox and the run-scoped context internally so the
// inbox liveness contract holds: an end_session frame cancels runCtx, waking
// any parked Wait.
func Run(ctx context.Context, spec RunSpec, ops CardOps, client llm.LLM, emit *events.Emitter, stdin io.Reader) (Result, error) {
	spec = withDefaults(spec)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	var endSession atomic.Bool

	inbox := NewInbox(
		spec.Interactive,
		func() {},
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

	// 1-2: workspace + clone + branch.
	ws := filepath.Join(spec.Workspace, strings.ToLower(spec.CardID))

	git := NewGit(ws, secretsPathForAuth(spec), hostFromRepoURL(spec.RepoURL), spec.CACertFile)

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

	// Worker bootstrap reads only scalar fields (Autonomous, Title); images are
	// not used here and would be wasted bytes on this run-gating call.
	tcx, err := ops.GetTaskContext(ctx, spec.CardID, false)
	if err != nil {
		return releaseWithError(ctx, ops, spec.CardID, fmt.Errorf("get task context: %w", err))
	}

	// 4: heartbeat goroutine for the whole run, including human waits.
	stopHeartbeat := startHeartbeat(runCtx, ops, spec.CardID)
	defer stopHeartbeat()

	// Every card runs the FSM. HITL (interactive && !autonomous) runs it in HITL
	// mode — sign-off gates wait on the inbox and creative cards brainstorm;
	// autonomous/non-interactive runs it with gates auto-passed and brainstorming
	// skipped. The freeform linear path is retired.
	return runFSM(ctx, runCtx, fsmArgs{
		ops: ops, git: git, client: client, emit: emit,
		spec: spec, tcx: tcx, branch: branchName,
		ws: ws, endSession: &endSession, human: inbox,
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
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

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

	// The stop func joins the goroutine, not just signals it: Run must not
	// return while its heartbeat goroutine can still tick a card it has
	// already released or completed.
	return func() {
		if once.CompareAndSwap(false, true) {
			close(done)
		}

		<-stopped
	}
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
	human      *Inbox
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
//   - ctx.Err() (end_session/kill): graceful path — push WIP, release,
//     exit 0; the persisted phase stays for a later resume.
//   - any other error: release the claim and return it.
func runFSM(ctx context.Context, runCtx context.Context, a fsmArgs) (Result, error) {
	red := redact.New([]string{a.spec.LLMKey, a.spec.MCPAPIKey, a.spec.GitToken})

	hitl := a.spec.Interactive && !a.tcx.Autonomous

	// Genuine nil for autonomous (the nil-concrete footgun guard); the live
	// inbox for HITL. Mode is read from Cfg.Interactive, never from Human != nil.
	var human harness.Inbox
	if hitl {
		human = a.human
	}

	skillTool := buildSkillTool(a.spec, a.ops)
	wt := writeToolsFor(a.ws, a.spec.BashTimeoutMax)

	if skillTool != nil {
		wt = append(wt, skillTool)
	}

	d := orchestrator.Deps{
		Ops: ops2orchestrator(a.ops),
		Git: a.git,
		GitForDir: func(dir string) orchestrator.GitOps {
			return NewGit(dir, secretsPathForAuth(a.spec), hostFromRepoURL(a.spec.RepoURL), a.spec.CACertFile)
		},
		PR:         NewPRCreator(a.ws, secretsPathForAuth(a.spec), a.spec.CACertFile, a.spec.RepoURL),
		Client:     a.client,
		Emit:       a.emit,
		Registry:   buildRegistry(a.spec),
		WriteTools: tools.NewRegistry(wt...),
		WriteToolsForDir: func(dir string) *tools.Registry {
			// Candidates get the same skill tool as the main solver — the
			// skills mount is a fixed path, not workspace-relative, so the
			// shared instance is safe across worktrees.
			wts := writeToolsFor(dir, a.spec.BashTimeoutMax)
			if skillTool != nil {
				wts = append(wts, skillTool)
			}

			return tools.NewRegistry(wts...)
		},
		ReadTools: tools.NewReadOnlyRegistry(a.ws),
		SkillTool: skillTool,
		Redact:    red.Apply,
		Human:     human,
		Cfg: orchestrator.Config{
			Project:           a.spec.Project,
			CardID:            a.spec.CardID,
			Branch:            a.branch,
			BaseBranch:        a.spec.BaseBranch,
			Workspace:         a.ws,
			MaxCardCost:       a.spec.MaxCardCost,
			PayloadModel:      a.spec.Model,
			DefaultModel:      a.spec.DefaultModel,
			ReasoningEffort:   a.spec.ReasoningEffort,
			MaxTurns:          a.spec.MaxTurns,
			ToolOutputMax:     a.spec.ToolOutputMax,
			ReviewAttemptsCap: reviewAttemptsCap,
			Interactive:       hitl,
			BestOfN:           a.spec.BestOfN,
			Compaction: orchestrator.Compaction{
				Enabled:         a.spec.CompactionEnabled,
				Threshold:       a.spec.CompactionThreshold,
				KeepRecentTurns: a.spec.CompactionKeepRecentTurns,
			},
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

	case isMaxTurns(err):
		// Turn-cap park: the harness stopped mid-task, so the tree may hold
		// half-done work that must NEVER be completed. Same shape as the
		// context-limit park — push WIP so resume can pick it up, release the
		// claim, surface the error so serve emits the failed callback.
		pushWIP(ctx, a)
		releaseQuietly(ctx, a.ops, a.spec.CardID)

		return Result{Reason: "error"}, fmt.Errorf("orchestrator: %w", err)

	case a.endSession.Load() || ctx.Err() != nil || errorsIsCanceled(err):
		// end_session / kill mid-FSM: the graceful park. Push whatever WIP
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

// isMaxTurns reports whether err is (or wraps) the orchestrator's turn-cap sentinel.
func isMaxTurns(err error) bool {
	var mte *orchestrator.MaxTurnsError

	return errors.As(err, &mte)
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

// releaseQuietly releases the claim, logging a real failure rather than masking
// the run outcome. An already-unclaimed card (ErrCardNotClaimed) is a benign
// no-op — the done phase released it first — so it is not logged.
func releaseQuietly(ctx context.Context, ops CardOps, cardID string) {
	if err := ops.ReleaseCard(ctx, cardID); err != nil && !errors.Is(err, cmclient.ErrCardNotClaimed) {
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

// writeToolsFor is the full model-facing toolset rooted at dir, matching the
// linear path's registry so the FSM coder has the same capabilities. It is
// parameterized only by the root dir — every other argument is fixed for the
// run — so it is the one source of truth behind both the main workspace's
// WriteTools registry and Best-of-N's per-candidate WriteToolsForDir factory.
func writeToolsFor(dir string, bashTimeoutMax int) []tools.Tool {
	return []tools.Tool{
		tools.NewReadTool(dir),
		tools.NewEditTool(dir),
		tools.NewWriteTool(dir),
		tools.NewGrepTool(dir),
		tools.NewGlobTool(dir),
		tools.NewGitTool(dir),
		tools.NewBashTool(dir).WithMaxTimeout(bashTimeoutMax),
	}
}

// buildSkillTool constructs the per-run Skill tool from the mounted skills dir
// and the per-card subset, wiring onEngage to report engagement on the
// top-level card. Returns nil when no skills are available, so no-skills runs
// register no Skill tool and stay byte-identical.
func buildSkillTool(spec RunSpec, ops CardOps) tools.Tool {
	if spec.TaskSkillsDir == "" {
		return nil
	}

	var onEngage func(ctx context.Context, name string) error
	if ops != nil {
		onEngage = func(ctx context.Context, name string) error {
			return ops.RecordSkillEngaged(ctx, spec.CardID, name)
		}
	}

	st, ok := tools.NewSkillTool(spec.TaskSkillsDir, spec.TaskSkills, spec.TaskSkillsSet, onEngage)
	if !ok {
		return nil
	}

	slog.Info("skill tool registered",
		"card_id", spec.CardID,
		"dir", spec.TaskSkillsDir,
		"skills", strings.Count(st.MenuText(), "\n"))

	return st
}

// buildRegistry assembles the model registry the FSM selects from. When a
// SelectionContext is present on the spec (injected by CM at trigger time), it
// is the authoritative source — the registry is built entirely from the
// payload-injected catalog, priors, favorites, and blacklist. No live catalog
// fetch or embedded baseline is consulted.
func buildRegistry(spec RunSpec) *registry.Registry {
	return registry.FromSelection(spec.Selection, spec.DefaultModel, spec.SelectorPriceHeadroom)
}

// releaseWithError best-effort releases the claim and returns an error result.
// Used on setup failures before the harness loop runs.
func releaseWithError(ctx context.Context, ops CardOps, cardID string, err error) (Result, error) {
	if relErr := ops.ReleaseCard(ctx, cardID); relErr != nil && !errors.Is(relErr, cmclient.ErrCardNotClaimed) {
		slog.Warn("release card failed", "card", cardID, "error", relErr)
	}

	return Result{Reason: "error"}, err
}

// secretsPathForAuth returns the secrets env file path when the run started with
// a git token — so git and gh re-read the current token per operation — or ""
// when the run has no git auth (public or file:// remotes), which disables
// credential injection. Auth PRESENCE is fixed at startup (GitToken); only the
// token VALUE rotates and is read fresh from this path per operation.
func secretsPathForAuth(spec RunSpec) string {
	if spec.GitToken == "" {
		return ""
	}

	return spec.SecretsEnvPath
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
