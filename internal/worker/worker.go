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

	"github.com/mhersson/contextmatrix-agent/internal/attempt"
	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/config"
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

// gatesFinalizeMargin is how much of the container's lifetime the pr_gates
// deadline leaves unspent, so a gate that runs out of patience still has room to
// write its park note and exit cleanly before serve kills the container. A
// gates park pushes no WIP - the work was already pushed by integrate - and
// releases no claim - CM clears it when it sees the completed callback.
const gatesFinalizeMargin = 10 * time.Minute

// RunSpec is the container-side contract: populated from CM_* env by the
// work command.
type RunSpec struct {
	CardID      string // CM_CARD_ID (required)
	Project     string // CM_PROJECT (required)
	RepoURL     string // CM_REPO_URL (required)
	BaseBranch  string // CM_BASE_BRANCH (optional)
	Interactive bool   // CM_INTERACTIVE ("true")
	BestOfN     int    // CM_BEST_OF_N; >= 2 races N candidate implementations (0 = normal run)
	Model       string // CM_MODEL (optional; honored if catalog-resolvable; also the first-choice selector fallback in buildRegistry)
	RunID       string // CM_RUN_ID; serve's per-run correlation id, attached as a default log attribute (absent with an older serve)

	// Attempt is this container's ordinal for the card: 1 for the first run, 2
	// for a container that replaced it, and so on (CMX_ATTEMPT). It is stamped
	// on every transcript line, so a card whose container restarted has two
	// separable runs in one log rather than two runs whose event sequences both
	// start at 1.
	Attempt int

	// Mob configures mob session discussions for this run: scalar knobs from
	// CM_MOB_* env, guest specs (bearer tokens inside) from the mounted
	// secrets file. nil = mob session off.
	Mob *protocol.MobSpec

	MCPURL    string // CM_MCP_URL (required)
	MCPAPIKey string // CM_MCP_API_KEY (required)

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

	// SelectorTierBars is the operator's quality ladder (CMX_SELECTOR_TIER_BARS,
	// JSON-encoded). Empty uses registry.DefaultTierBars.
	SelectorTierBars map[string]float64

	// ContainerTimeout is serve's hard kill ceiling for this run's container
	// (CMX_CONTAINER_TIMEOUT_SECONDS). 0 = unknown - an older serve, or a host
	// that never configured it - so a phase that must park before the kill
	// has no deadline to respect.
	ContainerTimeout time.Duration

	// GatesPollInterval is how often the pr_gates phase polls CI and Copilot
	// review status. CMX_GATES_POLL_INTERVAL_SECONDS; default 60s.
	GatesPollInterval time.Duration

	// GatesCIWaitTimeout bounds how long the pr_gates phase waits for CI to
	// finish before parking. CMX_GATES_CI_WAIT_TIMEOUT_SECONDS; default 45m.
	GatesCIWaitTimeout time.Duration

	// GatesCopilotWaitTimeout bounds how long the pr_gates phase waits for the
	// requested Copilot review before proceeding without one.
	// CMX_GATES_COPILOT_WAIT_TIMEOUT_SECONDS; default 20m.
	GatesCopilotWaitTimeout time.Duration

	// GatesCopilotThreadReplies posts the Copilot gate's triage verdicts back
	// to the PR's review threads and resolves settled ones.
	// CMX_GATES_COPILOT_THREAD_REPLIES; on unless set to exactly "false".
	GatesCopilotThreadReplies bool

	MaxCapability bool // CM_MAX_CAPABILITY; every pick chooses the most capable model in the tier regardless of price

	// ReviewAttemptsCap is the number of review rounds before the card parks in
	// review. Zero or negative means unset and resolves to
	// config.DefaultReviewAttemptsCap; values above config.MaxReviewAttemptsCap
	// are lowered to it.
	ReviewAttemptsCap int // CMX_REVIEW_ATTEMPTS_CAP

	CompactionEnabled         bool    // CMX_COMPACTION_ENABLED; false (default) keeps the hard context_limit stop
	CompactionThreshold       float64 // CMX_COMPACTION_THRESHOLD; fraction of the context window (default 0.85)
	CompactionKeepRecentTurns int     // CMX_COMPACTION_KEEP_RECENT_TURNS; recent turns kept verbatim (default 6)

	DefaultModel    string // CMX_DEFAULT_MODEL; fallback when Model is absent/unresolvable
	ReasoningEffort string // CMX_REASONING_EFFORT; empty = off (no reasoning overhead)
	Workspace       string // CMX_WORKSPACE; parent dir for the clone (default /home/user/workspace)
	CACertFile      string // CMX_CA_CERT_FILE; in-container path to an extra CA PEM (empty = disabled)

	// Selection carries the CM-resolved model selection inputs (candidates,
	// favorites, blacklist). Nil when CM sends none (old CM).
	Selection *protocol.SelectionContext // CMX_SELECTION (JSON-encoded)

	// Verify carries CM's card-over-project verify config for this run. Nil when
	// absent (nothing declared, or an old CM). Delivered via CMX_VERIFY.
	Verify *protocol.VerifyConfig

	// VerifyConfigError is set when CMX_VERIFY was present but unusable: bad
	// JSON, or a clean decode to an all-zero config (json.Unmarshal ignores
	// unknown fields, so a misspelled key lands here). Empty means the value was
	// read cleanly. It reaches the verify ladder as declared-tier intent we
	// failed to honour, so the run notes it and parks rather than quietly
	// shipping under a weaker gate.
	VerifyConfigError string

	// ReadOnlyRoots are absolute trees the read tools may resolve in addition to
	// the workspace (CMX_READ_ONLY_ROOTS, colon-separated). Declared by the
	// worker image for its own toolchain source caches; an operator override
	// rides the launcher's extra-env channel. Empty leaves every tool confined
	// to the workspace.
	ReadOnlyRoots []string

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
	ReportUsage(ctx context.Context, cardID string, u cmclient.UsageReport) (float64, error)
	ReportPush(ctx context.Context, cardID, branch, prURL string) error
	ReportParked(ctx context.Context, cardID, reason string) error
	CompleteTask(ctx context.Context, cardID, summary string) error
	ReleaseCard(ctx context.Context, cardID string) error
	TransitionCard(ctx context.Context, cardID, state string) error
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
			// EOF always means "session over" - finalize without completing.
			// Canceling runCtx wakes a parked Wait (the liveness contract)
			// and aborts an in-flight autonomous turn alike.
			endSession.Store(true)
			cancelRun()
		},
	)

	go inbox.Pump(stdin)

	branchName := "cm/" + strings.ToLower(spec.CardID)

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

	if err := ops.ClaimCard(ctx, spec.CardID); err != nil {
		return releaseWithError(ctx, ops, spec.CardID, fmt.Errorf("claim card: %w", err))
	}

	// Worker bootstrap reads only scalar fields (Autonomous, Title); images are
	// not used here and would be wasted bytes on this run-gating call.
	tcx, err := ops.GetTaskContext(ctx, spec.CardID, false)
	if err != nil {
		return releaseWithError(ctx, ops, spec.CardID, fmt.Errorf("get task context: %w", err))
	}

	// Stalled-card recovery: the server's heartbeat-timeout sweep sets a card
	// to "stalled" and clears its claim (e.g. after a mid-run MCP outage
	// suppressed heartbeats while the previous run kept working). The
	// ClaimCard above has already re-acquired the claim - claiming has no
	// state gate - so only the state is left to recover before the FSM starts.
	if tcx.State == "stalled" {
		tcx = recoverStalledCard(ctx, ops, spec, tcx)
	}

	// Heartbeat goroutine for the whole run, including human waits.
	stopHeartbeat := startHeartbeat(runCtx, ops, spec.CardID)
	defer stopHeartbeat()

	// Every card runs the FSM. HITL (interactive && !autonomous) runs it in HITL
	// mode - sign-off gates wait on the inbox and creative cards brainstorm;
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
	// target a real ref - an empty base makes `git merge-base "" HEAD` fail.
	return baseBranch, nil
}

// startHeartbeat ticks ops.Heartbeat on heartbeatInterval until the returned
// stop func is called. Failures are logged, not fatal: a transient MCP hiccup
// must not abort an otherwise healthy run. Stop joins the goroutine: Run must
// not return while a tick could still touch a card it has already released or
// completed.
func startHeartbeat(ctx context.Context, ops CardOps, cardID string) func() {
	return orchestrator.StartTicker(ctx, heartbeatInterval, func(ctx context.Context) {
		if err := ops.Heartbeat(ctx, cardID); err != nil {
			slog.Warn("heartbeat failed", "card", cardID, "error", err)
		}
	})
}

// fsmArgs bundles what runFSM needs. ctx is the PARENT context (used for the
// graceful finalize after a canceled run); runCtx is the run-scoped context an
// end_session frame cancels - the FSM runs under it.
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
//     CompleteTask and NO release - a human picks it up from review.
//   - GatesParkedError: identical shape - the pr_gates phase could not clear a
//     PR gate, so the card waits in review with its ## PR Gates note.
//   - BudgetExceededError: push WIP, release the claim, return the error
//     (non-zero exit; serve emits the failed callback).
//   - ContextLimitError: identical to the budget park - push WIP, release the
//     claim, return the error - so in-flight work survives a context-window stop.
//   - ctx.Err() (end_session/kill): graceful path - push WIP, release,
//     exit 0; the persisted phase stays for a later resume. Checked BEFORE
//     ToolchainMissingError below: a context-canceled Tier-3 model call can
//     still surface the toolchain sentinel, and an operator-initiated kill
//     must win that race rather than being reported as an environmental
//     blocked-park.
//   - ToolchainMissingError: same push-WIP/release/error shape as
//     Budget/Context above, plus the genuinely new step - transition the card
//     to blocked (before the release, since ownership may be required) so a
//     human sees the environmental park on the board instead of a silently
//     released claim. A failed transition (blocked is project-configurable)
//     is logged and degrades to the same silent park as the other arms -
//     never fatal.
//   - VerifyParkedError: identical to the toolchain arm - push WIP, blocked,
//     release, fail. The pre-commit gate refused to COMMIT the coder's work as
//     finished; the push is what keeps that work from dying with the container.
//   - any other error: release the claim and return it.
func runFSM(ctx context.Context, runCtx context.Context, a fsmArgs) (Result, error) {
	// Guest bearer tokens are known at worker start, so they join the
	// immutable redactor alongside the endpoint credentials.
	red := redact.New(append([]string{a.spec.LLMKey, a.spec.MCPAPIKey, a.spec.GitToken},
		mobGuestTokens(a.spec.Mob)...))

	hitl := a.spec.Interactive && !a.tcx.Autonomous

	// Genuine nil for autonomous (the nil-concrete footgun guard); the live
	// inbox for HITL. Mode is read from Cfg.Interactive, never from Human != nil.
	var human harness.Inbox
	if hitl {
		human = a.human
	}

	skillTool := buildSkillTool(a.spec, a.ops)
	dv := declaredVerify(a.spec.Verify)
	verifyEnv := orchestrator.ResolveVerifyEnv(dv)
	// One tracker for the whole run: threaded into every read-roots tool
	// construction below (and into Deps for reviewSubagentTools) so a
	// declaration rebuilt more than once - the two Deps fields below share one
	// declaration, and PlanTools/WriteToolsForDir are closures the orchestrator
	// can invoke again later - reports its outcome once, not once per rebuild.
	rrLog := orchestrator.NewReadRootsLog()
	// The main workspace registry is built before the run resolves its verify
	// command; the execute phase rebinds the solver's registry through
	// WriteToolsForDir once it has one.
	wt := writeToolsFor(rrLog, a.spec.CardID, a.ws, a.spec.BashTimeoutMax, verifyEnv, a.spec.ReadOnlyRoots, nil)

	if skillTool != nil {
		wt = append(wt, skillTool)
	}

	mob := mobConfig(a.spec.Mob)

	// One gh handle serves both PR roles: integrate opens the pull request
	// through it, pr_gates polls the same PR's checks and reviews.
	pr := NewPRCreator(a.ws, secretsPathForAuth(a.spec), a.spec.CACertFile, a.spec.RepoURL)

	// The gates must stop waiting before serve kills the container, leaving room
	// for the FSM to finalize (park note, WIP push, release). An unknown
	// container timeout leaves the deadline zero - the gates' own waits bound them.
	var deadline time.Time
	if a.spec.ContainerTimeout > 0 {
		deadline = time.Now().Add(a.spec.ContainerTimeout - gatesFinalizeMargin)
	}

	// An invalid operator tier ladder fails here, before any phase runs: no
	// work has been done yet, so this releases the claim and reports the
	// error rather than parking or pushing anything.
	reg, err := buildRegistry(a.spec)
	if err != nil {
		releaseQuietly(ctx, a.ops, a.spec.CardID)

		return Result{Reason: "error"}, err
	}

	orchOps := ops2orchestrator(a.ops)
	logReachability(ctx, reg, orchOps, a.spec.CardID)

	d := orchestrator.Deps{
		Ops: orchOps,
		Git: a.git,
		GitForDir: func(dir string) orchestrator.GitOps {
			return NewGit(dir, secretsPathForAuth(a.spec), hostFromRepoURL(a.spec.RepoURL), a.spec.CACertFile)
		},
		PR:         pr,
		PRGates:    pr,
		Client:     a.client,
		Emit:       a.emit,
		Registry:   reg,
		WriteTools: tools.NewRegistry(wt...),
		WriteToolsForDir: func(dir string, verify tools.Tool) *tools.Registry {
			// Candidates get the same skill tool as the main solver - the
			// skills mount is a fixed path, not workspace-relative, so the
			// shared instance is safe across worktrees.
			wts := writeToolsFor(rrLog, a.spec.CardID, dir, a.spec.BashTimeoutMax, verifyEnv, a.spec.ReadOnlyRoots, verify)
			if skillTool != nil {
				wts = append(wts, skillTool)
			}

			return tools.NewRegistry(wts...)
		},
		ReadTools: tools.NewRegistry(readOnlyToolsWithRoots(rrLog, a.spec.CardID, a.ws, a.spec.ReadOnlyRoots)...),
		PlanTools: func() *tools.Registry {
			return tools.NewRegistry(append(
				readOnlyToolsWithRoots(rrLog, a.spec.CardID, a.ws, a.spec.ReadOnlyRoots), orchestrator.NewFindingsTool())...)
		},
		ReadRoots:    a.spec.ReadOnlyRoots,
		ReadRootsLog: rrLog,
		SkillTool:    skillTool,
		Redact:       red.Apply,
		Human:        human,
		// The work command writes the JSONL transcript to process stdout;
		// seat-debug lines belong on the same stream (kind-rewritten so the
		// log bridge skips them).
		SeatDebugWriter: attempt.NewWriter(os.Stdout, a.spec.Attempt),
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
			ReviewAttemptsCap: a.spec.ReviewAttemptsCap,
			Interactive:       hitl,
			BestOfN:           resolveBestOfN(a.spec.BestOfN, mob),
			Mob:               mob,
			Compaction: orchestrator.Compaction{
				Enabled:         a.spec.CompactionEnabled,
				Threshold:       a.spec.CompactionThreshold,
				KeepRecentTurns: a.spec.CompactionKeepRecentTurns,
			},
			Verify:                    dv,
			VerifyConfigError:         a.spec.VerifyConfigError,
			Deadline:                  deadline,
			GatesPollInterval:         a.spec.GatesPollInterval,
			GatesCIWaitTimeout:        a.spec.GatesCIWaitTimeout,
			GatesCopilotWaitTimeout:   a.spec.GatesCopilotWaitTimeout,
			GatesCopilotThreadReplies: a.spec.GatesCopilotThreadReplies,
		},
	}

	err = runOrchestrator(runCtx, d)

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
		reportParkedQuietly(ctx, a, parkReason(err))

		return Result{Reason: "completed"}, nil

	case isGatesParked(err):
		// Parked, not failed: the card stays in review with a ## PR Gates
		// note. No CompleteTask, no release - same shape as review parking.
		slog.Info("pr gates parked; leaving card in review", "card", a.spec.CardID)
		reportParkedQuietly(ctx, a, parkReason(err))

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
		// context-limit park - push WIP so resume can pick it up, release the
		// claim, surface the error so serve emits the failed callback.
		pushWIP(ctx, a)
		releaseQuietly(ctx, a.ops, a.spec.CardID)

		return Result{Reason: "error"}, fmt.Errorf("orchestrator: %w", err)

	case a.endSession.Load() || ctx.Err() != nil || errorsIsCanceled(err):
		// end_session / kill mid-FSM: the graceful park. Checked BEFORE the
		// toolchain arm below - a context-canceled Tier-3 model call can still
		// surface a ToolchainMissingError (an inconclusive proposal, not a real
		// resolution), and an operator-initiated kill must never be reported as
		// an environmental blocked-park. Push whatever WIP exists, release the
		// claim, exit 0. Usage was already reported per-phase by the
		// orchestrator; the persisted phase stays so a later run resumes from it.
		pushWIP(ctx, a)
		releaseQuietly(ctx, a.ops, a.spec.CardID)

		return Result{Reason: "end_session"}, nil

	case isToolchainMissing(err):
		// Toolchain-missing park: an environmental failure, not a model
		// failure - no ReportModelOutcomes/BlacklistModel call belongs here,
		// mirroring the Budget/Context/MaxTurns arms above. The orchestrator
		// already wrote the blocker reason to the card log; this path does not
		// duplicate it.
		return parkBlocked(ctx, a, err)

	case isNoModel(err):
		// Model-selection park: no catalogued model clears any configured bar
		// for the role and the operator's capable default is barred too, so
		// there is nothing to run the work on. Environmental like the
		// toolchain arm above, and mapped identically. The orchestrator
		// already wrote the reason to the card log.
		return parkBlocked(ctx, a, err)

	case isVerifyParked(err):
		// Pre-commit verify park: a subtask's verify stayed red through its one
		// fix pass, so the orchestrator refused to commit that work as
		// finished. Refusing the COMMIT is not a reason to destroy the tree
		// with the container, so this maps exactly like the environmental
		// parks - the push carries the refused work out on the card branch,
		// and the blocked transition makes the park visible on the board.
		// Unlike those this park IS about the model's output, but the
		// orchestrator already reported that outcome row while it still held
		// the subtask claim, and it wrote the card log line carrying the
		// failing command and output; the worker duplicates neither.
		return parkBlocked(ctx, a, err)

	default:
		releaseQuietly(ctx, a.ops, a.spec.CardID)

		return Result{Reason: "error"}, fmt.Errorf("orchestrator: %w", err)
	}
}

// parkBlocked maps one of the environmental park arms (toolchain-missing,
// no-model, verify-parked) onto the shared blocked-park sequence. The genuinely
// distinguishing step: transition the card to blocked BEFORE releasing the
// claim, since ownership may be required for the transition. blocked is
// project-configurable (not every .board.yaml declares in_progress ->
// blocked), so a failure here is logged and the park still completes - never
// fatal.
func parkBlocked(ctx context.Context, a fsmArgs, err error) (Result, error) {
	if terr := a.ops.TransitionCard(ctx, a.spec.CardID, "blocked"); terr != nil {
		slog.Warn("transition to blocked failed; leaving card state as-is",
			"card", a.spec.CardID, "error", terr)
	}

	pushWIP(ctx, a)
	releaseQuietly(ctx, a.ops, a.spec.CardID)

	return Result{Reason: "error"}, fmt.Errorf("orchestrator: %w", err)
}

// parkReason renders the park sentinel's own message, unwrapped from any
// orchestrator wrapping, for the board's parked report.
func parkReason(err error) string {
	var rp *orchestrator.ReviewParkedError
	if errors.As(err, &rp) {
		return rp.Error()
	}

	var gp *orchestrator.GatesParkedError
	if errors.As(err, &gp) {
		return gp.Error()
	}

	return err.Error()
}

// reportParkedQuietly reports the park to CM so the board shows the parked
// status. Best-effort: the park itself must never fail on a reporting error
// (an older server without the tool just misses the board signal).
func reportParkedQuietly(ctx context.Context, a fsmArgs, reason string) {
	if err := a.ops.ReportParked(ctx, a.spec.CardID, reason); err != nil {
		slog.Warn("report parked failed; board will not show the park",
			"card", a.spec.CardID, "reason_head", reason[:min(len(reason), 120)], "error", err)
	}
}

// isReviewParked reports whether err is the orchestrator's review-park sentinel.
func isReviewParked(err error) bool {
	var rp *orchestrator.ReviewParkedError

	return errors.As(err, &rp)
}

// isGatesParked reports whether err is the orchestrator's pr-gates park sentinel.
func isGatesParked(err error) bool {
	var gp *orchestrator.GatesParkedError

	return errors.As(err, &gp)
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

// isNoModel reports whether err is (or wraps) the orchestrator's
// model-selection sentinel.
func isNoModel(err error) bool {
	var nme *orchestrator.NoModelError

	return errors.As(err, &nme)
}

// isToolchainMissing reports whether err is (or wraps) the orchestrator's
// toolchain-missing sentinel.
func isToolchainMissing(err error) bool {
	var tme *orchestrator.ToolchainMissingError

	return errors.As(err, &tme)
}

// isVerifyParked reports whether err is (or wraps) the orchestrator's
// pre-commit verify-park sentinel.
func isVerifyParked(err error) bool {
	var vpe *orchestrator.VerifyParkedError

	return errors.As(err, &vpe)
}

// errorsIsCanceled reports whether err is (or wraps) context cancellation, which
// is what the FSM returns when an end_session frame cancels its run context.
func errorsIsCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isTransitionRejected reports whether err is a state-transition rejection
// from ContextMatrix: the wire error contains "cannot transition from", which
// is the actual ValidationError text reachable on the client (the
// transition_card tool description's "invalid state transition" sentinel
// never reaches the client). This is used to distinguish a genuine transition
// rejection from a transient MCP/network error, so the fallback is only
// triggered by a board-configuration rejection.
func isTransitionRejected(err error) bool {
	if err == nil {
		return false
	}

	return strings.Contains(err.Error(), "cannot transition from")
}

// recoverStalledCard attempts to recover a stalled card back to "in_progress"
// before the FSM starts. The caller has already re-claimed the card (the
// server's stall sweep cleared the previous claim when it set the state), so
// only the state needs fixing. It tries:
//
//  1. Direct TransitionCard(cardID, "in_progress").
//  2. If that fails with a transition rejection (cannot transition from):
//     TransitionCard(cardID, "todo") + ClaimCard (which auto-transitions
//     todo to in_progress).
//  3. Re-fetch task context and verify state is "in_progress".
//  4. If both transitions fail (board with stalled: [] is legal config):
//     warn and continue - the run will later hard-fail at start_review,
//     but this is the documented degrade path.
//
// Returns the updated TaskContext. If recovery fails the returned context
// still carries whatever state the last GetTaskContext returned.
func recoverStalledCard(ctx context.Context, ops CardOps, spec RunSpec, original cmclient.TaskContext) cmclient.TaskContext {
	slog.Info("card is stalled; attempting recovery", "card", spec.CardID)

	// Attempt 1: direct transition to in_progress.
	err := ops.TransitionCard(ctx, spec.CardID, "in_progress")
	if err != nil && isTransitionRejected(err) {
		// Fallback: transition to "todo", then re-claim. Claim_card
		// auto-transitions todo to in_progress, so no trailing transition
		// is needed.
		slog.Info("direct in_progress transition rejected; falling back to todo+reclaim",
			"card", spec.CardID, "error", err)

		if terr := ops.TransitionCard(ctx, spec.CardID, "todo"); terr != nil {
			if isTransitionRejected(terr) {
				slog.Warn("stalled card could not be recovered; both transitions failed",
					"card", spec.CardID, "direct_error", err, "fallback_error", terr)
			} else {
				slog.Warn("todo transition failed with non-rejection error; cannot recover",
					"card", spec.CardID, "error", terr)
			}

			// Both transitions failed. Re-fetch to see current state and return,
			// falling through to the degrade path.
			tcx, tErr := ops.GetTaskContext(ctx, spec.CardID, false)
			if tErr != nil {
				slog.Warn("re-fetch after recovery failure failed", "card", spec.CardID, "error", tErr)

				return original
			}

			return tcx
		}

		// todo transition succeeded. Re-claim, which auto-transitions to in_progress.
		if cErr := ops.ClaimCard(ctx, spec.CardID); cErr != nil {
			slog.Warn("re-claim after todo transition failed", "card", spec.CardID, "error", cErr)

			// Claim failed; re-fetch to see current state and degrade.
			tcx, tErr := ops.GetTaskContext(ctx, spec.CardID, false)
			if tErr != nil {
				slog.Warn("re-fetch after failed re-claim failed", "card", spec.CardID, "error", tErr)

				return original
			}

			return tcx
		}
	} else if err != nil {
		// Non-rejection error: anything from a transient MCP/network failure
		// to a permanent server-side gate whose text differs from the
		// state-machine rejection (e.g. "blocked by dependencies"). The todo
		// fallback only helps when the direct edge is missing from the board
		// config, so skip it and let the verify re-fetch below report the
		// outcome.
		slog.Warn("direct in_progress transition failed with non-rejection error; skipping fallback",
			"card", spec.CardID, "error", err)
	}

	// Re-fetch context and verify the state was recovered.
	tcx, tErr := ops.GetTaskContext(ctx, spec.CardID, false)
	if tErr != nil {
		slog.Warn("re-fetch after stalled recovery failed", "card", spec.CardID, "error", tErr)

		return original
	}

	if tcx.State != "in_progress" {
		slog.Warn("stalled card recovery did not produce in_progress state",
			"card", spec.CardID, "state", tcx.State)
	} else {
		slog.Info("stalled card recovered", "card", spec.CardID)
	}

	return tcx
}

// pushWIP commits any dirty tree and pushes the card branch on the PARENT ctx
// (runCtx may already be canceled). Best-effort: a failure is logged, not fatal -
// the park/fail outcome must still surface.
func pushWIP(ctx context.Context, a fsmArgs) {
	dirty, err := a.git.CommitIfDirty(ctx, a.tcx.Title, a.spec.CardID)
	if err != nil {
		slog.Warn("WIP commit failed", "card", a.spec.CardID, "error", err)

		return
	}

	if !dirty {
		// A clean tree can still hold work: the turn-cap salvage path commits
		// before its verify gate, and a declined salvage leaves that commit
		// stranded locally unless it is pushed here. A failed count falls
		// through to the push - the same transient pressure that kills a
		// rev-list spawn can spare a push spawn moments later, and an
		// unnecessary push of already-present commits is a no-op, while
		// returning here would re-strand the commit this branch exists to save.
		n, cerr := a.git.UnpushedCount(ctx)
		if cerr != nil {
			slog.Warn("WIP unpushed-count failed; pushing anyway", "card", a.spec.CardID, "error", cerr)
		} else if n == 0 {
			return
		}
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
// no-op - the done phase released it first - so it is not logged.
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
// linear path's registry so the FSM coder has the same capabilities. It is the
// one source of truth behind both the main workspace's WriteTools registry and
// Best-of-N's per-candidate WriteToolsForDir factory: the root dir and the
// verify tool vary per call site, every other argument is fixed for the run.
// extraEnv carries the resolved verify.env pass-throughs into the bash tool's
// scrubbed environment, so the model's shell resolves the same set as the
// verify gate and can reproduce it. verify is the orchestrator's verify tool
// for this dir, nil when the run resolved no verify command - registering it
// here is what keeps both registries from drifting apart on it. readRoots widens
// the read path tools to the operator's declared trees; edit, write and bash keep
// the single dir root, so the write boundary stays the workspace. cardID
// attributes the read-roots outcome logged for this construction; rrLog is
// the run's shared tracker (see orchestrator.ReadRootsLog) that keeps repeat
// constructions of the same declaration from each logging their own line.
func writeToolsFor(rrLog *orchestrator.ReadRootsLog, cardID, dir string, bashTimeoutMax int, extraEnv, readRoots []string, verify tools.Tool) []tools.Tool {
	// The read, grep and glob tools built here from the same (dir, readRoots)
	// all sanitize identically, so only the read tool's outcome needs logging.
	readTool := tools.NewReadTool(dir).WithReadRoots(readRoots)
	rrLog.Log(cardID, dir, readTool.ReadRoots())

	wt := []tools.Tool{
		readTool,
		tools.NewEditTool(dir),
		tools.NewWriteTool(dir),
		tools.NewGrepTool(dir).WithReadRoots(readRoots),
		tools.NewGlobTool(dir).WithReadRoots(readRoots),
		tools.NewGitTool(dir),
		tools.NewBashTool(dir).WithMaxTimeout(bashTimeoutMax).WithExtraEnv(extraEnv),
		orchestrator.NewFinishTool(),
	}

	if verify != nil {
		wt = append(wt, verify)
	}

	return wt
}

// readOnlyToolsWithRoots is tools.ReadOnlyTools widened by the operator's
// declared read-only roots. The harness's ReadOnlyTools takes only the
// workspace, so the three path tools are constructed individually here. The git
// tool is unchanged: it operates on the workspace repository, not on arbitrary
// paths. Roots come from the resolved configuration and are sanitized inside
// the harness, which drops anything that would widen access rather than add a
// sibling tree - ReadRootsLog is what surfaces that outcome, so nothing else
// validates it here. An empty list yields tools identical to the harness
// defaults. cardID attributes the logged outcome; ws is also the workspace
// the log line names; rrLog is the run's shared tracker (see writeToolsFor).
func readOnlyToolsWithRoots(rrLog *orchestrator.ReadRootsLog, cardID, ws string, roots []string) []tools.Tool {
	// Same reasoning as writeToolsFor: read, grep and glob share one outcome.
	readTool := tools.NewReadTool(ws).WithReadRoots(roots)
	rrLog.Log(cardID, ws, readTool.ReadRoots())

	return []tools.Tool{
		readTool,
		tools.NewGrepTool(ws).WithReadRoots(roots),
		tools.NewGlobTool(ws).WithReadRoots(roots),
		tools.NewGitTool(ws),
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
// is the authoritative source - the registry is built entirely from the
// payload-injected catalog, priors, favorites, and blacklist. No live catalog
// fetch or embedded baseline is consulted.
//
// The capable default (the fallback when the candidate pool is empty) resolves
// with precedence: (1) spec.Model (the trigger's default_model), when non-empty;
// (2) spec.DefaultModel (the serve-config default); (3) config.DefaultCapableModel
// (a compiled-in guard).
//
// spec.SelectorTierBars carries the operator's quality ladder. An invalid
// ladder is returned as an error so the worker exits rather than running the
// card on a half-understood ladder.
func buildRegistry(spec RunSpec) (*registry.Registry, error) {
	capable := spec.Model

	if capable == "" {
		capable = spec.DefaultModel
	}

	if capable == "" {
		capable = config.DefaultCapableModel
	}

	bars, err := registry.TierBarsFromStrings(spec.SelectorTierBars)
	if err != nil {
		return nil, fmt.Errorf("build registry: %w", err)
	}

	return registry.FromSelection(spec.Selection, capable, spec.SelectorPriceHeadroom, spec.MaxCapability).
		WithTierBars(bars), nil
}

// logReachability reports structural tier unreachability BEFORE the first
// model call. It never gates the run: refusing to start because `critical`
// is unreachable would block every `simple` card too. ops is nil in tests
// that only implement the worker's narrower CardOps (see ops2orchestrator);
// the card-log line is then simply skipped.
func logReachability(ctx context.Context, reg *registry.Registry, ops orchestrator.Ops, cardID string) {
	var unreachable []string

	for _, tr := range reg.Reachability() {
		slog.Info("selector: tier reachability",
			"card_id", cardID, "role", string(tr.Role), "tier", string(tr.Tier),
			"bar", tr.Bar, "candidates", tr.Count, "best_available", tr.Best)

		if tr.Count == 0 {
			unreachable = append(unreachable,
				fmt.Sprintf("%s/%s bar %.2f (best available %.2f)", tr.Role, tr.Tier, tr.Bar, tr.Best))
		}
	}

	for _, t := range reg.OrphanFavoriteTiers() {
		slog.Warn("selector: favorite configured for an unknown tier; it can never be consulted",
			"card_id", cardID, "tier", string(t))
	}

	if len(unreachable) > 0 && ops != nil {
		_ = ops.AddLog(ctx, cardID, //nolint:errcheck // advisory
			"model catalog cannot reach every tier: "+strings.Join(unreachable, ", "))
	}
}

// declaredVerify maps the protocol verify config onto the orchestrator-local
// type so the orchestrator package need not import protocol. TimeoutSeconds
// becomes a duration; the orchestrator clamps it. nil in -> nil out.
func declaredVerify(v *protocol.VerifyConfig) *orchestrator.DeclaredVerify {
	if v == nil {
		return nil
	}

	return &orchestrator.DeclaredVerify{
		Command: v.Command,
		Timeout: time.Duration(v.TimeoutSeconds) * time.Second,
		Env:     v.Env,
	}
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
// a git token - so git and gh re-read the current token per operation - or ""
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

	// Zero or negative means unset - serve omits the env var in that case, so
	// resolve it to the same default the orchestrator's review loop falls back
	// to rather than inventing a third answer. Serve rejects an out-of-range cap
	// at startup, but this clamp is still load-bearing: worker_extra_env is
	// appended after the validated value and wins under last-wins env semantics,
	// so an operator passthrough (or a hand-run container) can set any value.
	switch {
	case spec.ReviewAttemptsCap <= 0:
		spec.ReviewAttemptsCap = config.DefaultReviewAttemptsCap
	case spec.ReviewAttemptsCap > config.MaxReviewAttemptsCap:
		slog.Warn("review_attempts_cap above the safe maximum; using the maximum",
			"requested", spec.ReviewAttemptsCap, "using", config.MaxReviewAttemptsCap)

		spec.ReviewAttemptsCap = config.MaxReviewAttemptsCap
	}

	return spec
}

// mobConfig maps the payload mob session spec onto the orchestrator's config:
// the phase list becomes per-phase booleans and the spec-level defaults (2
// critique rounds, budget factor 0.75, phases review-only, checkpoint tier
// "simple" / 3 rounds) fill zero values so the orchestrator never sees an
// ambiguous zero. "execute" is live only when the payload's server flag rode
// along - a bare phase value from a stale CM stays inert. nil or
// participants < 2 = off (zero value).
func mobConfig(spec *protocol.MobSpec) orchestrator.MobConfig {
	if spec == nil || spec.Participants < 2 {
		return orchestrator.MobConfig{}
	}

	c := orchestrator.MobConfig{
		Participants: spec.Participants,
		Rounds:       spec.Rounds,
		BudgetFactor: spec.BudgetFactor,
	}

	for _, ph := range spec.Phases {
		switch ph {
		case "plan":
			c.Plan = true
		case "review":
			c.Review = true
		case "execute":
			c.Execute = spec.ExecuteCheckpoints
		}
	}

	if len(spec.Phases) == 0 {
		c.Review = true
	}

	if c.Rounds <= 0 {
		c.Rounds = 2
	}

	if c.BudgetFactor <= 0 {
		c.BudgetFactor = 0.75
	}

	if c.Execute {
		c.CheckpointMinTier = spec.CheckpointMinTier
		if c.CheckpointMinTier == "" {
			c.CheckpointMinTier = "simple"
		}

		c.CheckpointRounds = spec.CheckpointRounds
		if c.CheckpointRounds <= 0 {
			c.CheckpointRounds = 3
		}
	}

	for _, g := range spec.Guests {
		c.Guests = append(c.Guests, orchestrator.MobGuest{Name: g.Name, URL: g.URL, Token: g.Token})
	}

	return c
}

// resolveBestOfN applies the mob-coding priority rule: a live execute
// checkpoint config zeroes the candidate race. CM already arbitrates this at
// trigger time; the mirror keeps a stale CM from racing candidates and
// checkpointing at once.
func resolveBestOfN(bestOfN int, mob orchestrator.MobConfig) int {
	if bestOfN >= 2 && mob.Execute {
		slog.Warn("best_of_n ignored: mob coding takes priority", "best_of_n", bestOfN)

		return 0
	}

	return bestOfN
}

// mobGuestTokens extracts the non-empty guest bearer tokens for redactor
// registration.
func mobGuestTokens(spec *protocol.MobSpec) []string {
	if spec == nil {
		return nil
	}

	var tokens []string

	for _, g := range spec.Guests {
		if g.Token != "" {
			tokens = append(tokens, g.Token)
		}
	}

	return tokens
}
