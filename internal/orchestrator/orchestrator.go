// Package orchestrator drives an autonomous card through plan -> execute ->
// judge -> document -> review -> integrate -> pr_gates -> done. Code owns all
// inside phases. Each phase persists itself to the card BEFORE doing work, so the
// stored phase always reads "in progress or interrupted".
//
// Boundary rule: this package imports harness, llm, registry, tools, events,
// and cmclient - never internal/worker. The git surface the FSM needs is
// declared here as the GitOps interface (consuming-package convention);
// *worker.Git satisfies it.
package orchestrator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// Ops is the card-operation surface the FSM needs. It is satisfied by
// *cmclient.Client; the compile-time assertion lives in internal/worker
// (which is allowed to import both packages).
type Ops interface {
	ClaimCard(ctx context.Context, cardID string) error
	Heartbeat(ctx context.Context, cardID string) error
	GetTaskContext(ctx context.Context, cardID string, includeImages bool) (cmclient.TaskContext, error)
	CreateCard(ctx context.Context, project, parent, title, body string, dependsOn []string) (string, error)
	CreateTopLevelCard(ctx context.Context, project, title, body string, dependsOn []string) (string, error)
	SetPhase(ctx context.Context, cardID, phase string) error
	UpdateCardBody(ctx context.Context, cardID, body string) error
	SetAutonomous(ctx context.Context, cardID string, autonomous bool) error
	TransitionCard(ctx context.Context, cardID, state string) error
	StartReview(ctx context.Context, cardID string) error
	IncrementReviewAttempts(ctx context.Context, cardID string) (int, error)
	SubtaskStates(ctx context.Context, project, parentID string) ([]cmclient.SubtaskState, error)
	AddLog(ctx context.Context, cardID, message string) error
	ReportUsage(ctx context.Context, cardID string, u cmclient.UsageReport) (float64, error)
	ReportPush(ctx context.Context, cardID, branch, prURL string) error
	ReportModelOutcomes(ctx context.Context, cardID string, outcomes []cmclient.ModelOutcome) error
	BlacklistModel(ctx context.Context, cardID, model, reason string) error
	CompleteTask(ctx context.Context, cardID, summary string) error
	ReleaseCard(ctx context.Context, cardID string) error
}

// ErrRebaseConflict is the sentinel the integrate phase matches to take its
// conflict-fallback path. It lives here, in the consuming package, so the FSM
// can detect the conflict class without importing internal/worker (the import
// boundary is one-way: worker may import orchestrator, never the reverse). The
// worker's RebaseAutosquash wraps THIS sentinel so errors.Is matches across the
// package boundary.
var ErrRebaseConflict = errors.New("rebase conflict")

// PRCreator opens a pull request for the integrated branch and returns its URL.
// It is the seam over the gh CLI: the worker provides the real implementation;
// tests inject a fake. The orchestrator writes the body before calling Create.
type PRCreator interface {
	Create(ctx context.Context, title, body, base, head string) (string, error)
}

// GitOps is the slice of the worker git helper the FSM uses. It is defined
// here, on the consuming side, per the interface-ownership convention;
// *worker.Git implements it.
type GitOps interface {
	Push(ctx context.Context, branch string) error
	ForcePushWithLease(ctx context.Context, branch, expectedTip string) error
	Fetch(ctx context.Context, ref string) error
	RemoteTip(ctx context.Context, branch string) (string, error)
	MergeBase(ctx context.Context, a, b string) (string, error)
	CommitWithMessage(ctx context.Context, message string) (bool, error)
	CommitFixup(ctx context.Context, target string) (bool, error)
	LastCommitTouching(ctx context.Context, paths []string) (string, error)
	RebaseAutosquash(ctx context.Context, onto string) error
	SoftReset(ctx context.Context, to string) error
	Head(ctx context.Context) (string, error)
	Checkout(ctx context.Context, ref string) error
	Diff(ctx context.Context, base string) (string, error)
	AddWorktree(ctx context.Context, path, branch, startRef string) error
	RemoveWorktree(ctx context.Context, path string) error
	DeleteBranch(ctx context.Context, name string) error
	HardReset(ctx context.Context, ref string) error
	DiffStat(ctx context.Context, base string) (string, error)
	DisableAutoGC(ctx context.Context) error
	AddInfoExclude(ctx context.Context, pattern string) error
	// WorktreeState is an opaque fingerprint of everything uncommitted in the
	// worktree, built from the repository's own ignore rules so that artifacts a
	// check command produces do not move it. Equal fingerprints mean nothing was
	// written in between AT THE SAME COMMIT: uncommitted state is all it covers,
	// so a clean tree before a commit and the clean tree that commit leaves
	// behind are equal here while being entirely different trees. A caller that
	// needs "the same tree" pairs it with Head - see worktreeIdentity. The read
	// is bounded in both time and bytes, so an implementation may report an
	// error instead of fingerprinting a tree it cannot read cheaply; callers
	// treat that as "assume written".
	WorktreeState(ctx context.Context) (string, error)
}

// Config carries the per-run parameters the FSM needs.
type Config struct {
	Project           string
	CardID            string
	Branch            string // cm/<card-id-lower>
	BaseBranch        string
	Workspace         string
	MaxCardCost       float64
	PayloadModel      string // CM's default_model from the trigger; "" = serve default
	DefaultModel      string // serve-config default
	ReasoningEffort   string // CMX_REASONING_EFFORT; empty = off
	MaxTurns          int
	ToolOutputMax     int
	ReviewAttemptsCap int // review rounds before the card parks; <=0 falls back to config.DefaultReviewAttemptsCap
	// Interactive is the sole mode flag: true => HITL (gates wait on Human and
	// brainstorming runs for creative cards); false => autonomous (gates pass
	// through, brainstorming skipped). Autonomous behavior is byte-for-byte the
	// pre-HITL behavior.
	Interactive bool

	// BestOfN, when >= 2, fans execute out into N candidate implementations
	// judged before document. 0/1 = normal single-solver run.
	BestOfN int

	// Mob configures mob session discussions (spec 2026-07-10). Zero value = off.
	Mob MobConfig

	// Compaction configures optional in-window context compaction for phase
	// model runs. Disabled (the zero value) preserves the hard context_limit
	// stop, which is the agent's default behavior.
	Compaction Compaction

	// Provider is the raw OpenRouter provider-routing object (e.g.
	// require_parameters/order/sort) applied to every phase model run via
	// harnessConfig and inherited by review subagents. nil = default routing.
	// No serve/work env knob populates it yet; it is the orchestrator-level
	// seam mirroring the standalone run command's provider config.
	Provider json.RawMessage

	// Verify is the operator-declared verify gate (CM's card-over-project
	// resolution), delivered via CMX_VERIFY. nil means nothing declared - the
	// gate falls back to repo-convention detection and then a model proposal.
	// It is an orchestrator-local type so the package need not import protocol.
	Verify *DeclaredVerify

	// VerifyConfigError is set when the worker received a CMX_VERIFY it could
	// not use (bad JSON, or a clean decode to an all-zero config). Verify is nil
	// in that case, but this is NOT "nothing declared": the ladder treats it as
	// declared-tier intent we failed to honour, so it notes the problem and
	// parks rather than shipping under a silently weaker gate.
	VerifyConfigError string

	// Deadline is the wall-clock instant the pr_gates phase must be finished
	// waiting by - the container's own kill ceiling minus a finalize margin. The
	// zero value means the worker was not told its timeout, leaving the gates
	// bounded only by their own wait knobs.
	Deadline time.Time

	// GatesPollInterval is how often the pr_gates phase re-checks CI and Copilot.
	// Zero falls back to the package default.
	GatesPollInterval time.Duration

	// GatesCIWaitTimeout bounds how long the CI gate waits for checks to settle.
	// Zero falls back to the package default.
	GatesCIWaitTimeout time.Duration

	// GatesCopilotWaitTimeout bounds how long the Copilot gate waits for a
	// review to land. Zero falls back to the package default.
	GatesCopilotWaitTimeout time.Duration

	// GatesCopilotThreadReplies posts the Copilot gate's triage verdicts back
	// to the PR's review threads and resolves settled ones. The worker wires
	// it from CMX_GATES_COPILOT_THREAD_REPLIES (on unless exactly "false"); a
	// Deps built directly leaves it false and writes nothing.
	GatesCopilotThreadReplies bool
}

// DeclaredVerify is the operator-declared verify configuration for a run, mapped
// from protocol.VerifyConfig by the worker. Command is a shell string; Timeout is
// 0 when unset (the default applies); Env names container variables to pass
// through to the verify subprocess (re-filtered agent-side before use).
type DeclaredVerify struct {
	Command string
	Timeout time.Duration
	Env     []string
}

// Compaction configures in-window context compaction for the FSM's phase model
// runs. Enabled=false (the zero value, the default) preserves the hard
// context_limit stop; when enabled, harnessConfig passes the threshold and
// keep-recent settings through to the harness loop.
type Compaction struct {
	Enabled         bool
	Threshold       float64
	KeepRecentTurns int
}

// Deps bundles the collaborators the FSM drives.
type Deps struct {
	Ops Ops
	Git GitOps
	// GitForDir returns a GitOps rooted at dir with NO branch policy set -
	// guardPush fails closed, so candidate handles structurally cannot push.
	// Used by Best-of-N to hand each candidate worktree its own git handle.
	GitForDir  func(dir string) GitOps
	PR         PRCreator // opens the pull request in the integrate phase (gh CLI seam)
	PRGates    PRGates   // checks/review polling in the pr_gates phase (gh CLI seam)
	Client     llm.LLM
	Emit       *events.Emitter
	Registry   *registry.Registry
	WriteTools *tools.Registry // full toolset rooted at the workspace
	// WriteToolsForDir builds the full write toolset rooted at dir, plus the
	// caller's verify tool when it has one (a genuine nil interface when the run
	// resolved no verify command). Used by Best-of-N to give each candidate
	// worktree its own jailed tool registry, and by the execute phase to bind the
	// solo solver's registry once the verify plan has resolved.
	WriteToolsForDir func(dir string, verify tools.Tool) *tools.Registry
	ReadTools        *tools.Registry // read-only subset for planner/reviewers
	// PlanTools builds the plan phase's own registry: the read-only subset plus
	// the findings tool. A factory, not a registry, because the findings tool is
	// stateful - one instance per draftPlan call, so the repair attempt inherits
	// the first attempt's list while separate drafts start clean. Nil falls back
	// to ReadTools.
	PlanTools func() *tools.Registry
	// ReadRoots are the operator's declared read-only roots, resolved once at
	// worker start. Phases hand them to tools that read paths; nothing here
	// derives them from a card body, a plan, or a model output.
	ReadRoots []string
	// ReadRootsLog dedupes the read-only-roots outcome line across every tool
	// construction in this run (see ReadRootsLog). The worker constructs one
	// instance per run and threads it here as well as into writeToolsFor and
	// readOnlyToolsWithRoots, so reviewSubagentTools - which only has this
	// Deps value to work from - reports through the same tracker. Nil is safe
	// (Log degrades to no dedup) but production always sets it.
	ReadRootsLog *ReadRootsLog
	SkillTool    tools.Tool // optional; engaged by coder/review/document subagents (nil when no task-skills)
	Cfg          Config
	Redact       func(string) string // nil = identity; scrubs tool output in phase runs (wired by the worker)
	// Human is the HITL ask-and-wait channel, satisfied by the worker's live
	// Inbox. It is a genuine nil for autonomous runs; mode is read from
	// Cfg.Interactive, never from Human != nil (the nil-concrete footgun).
	Human harness.Inbox

	// SeatDebugWriter receives the JSONL event lines of mob session seat sub-runs,
	// kind-rewritten to "seat_debug" so the service log bridge (which skips
	// unknown kinds) keeps them off the live card stream while they stay in
	// the container stdout for debugging. The worker points it at process
	// stdout - the same stream the work command's transcript emitter writes.
	// nil = io.Discard (tests, standalone runs).
	SeatDebugWriter io.Writer
}

// phaseOrder is the fixed forward sequence of phases. Run enters at the card's
// persisted phase and never moves backward through this slice. The judge phase
// is a no-op for normal single-solver runs (Cfg.BestOfN < 2); it races and picks
// the Best-of-N winner between execute and document. The pr_gates phase is a
// pass-through for a card with no gate flags; when gated it holds the card in
// review until the PR's gates pass, and it owns the transition to done.
var phaseOrder = []string{"plan", "execute", "judge", "document", "review", "integrate", "pr_gates", "done"}

// phaseFn is a single phase's body.
type phaseFn func(context.Context) error

// run is the live FSM state for one card. The phase functions are stored as
// fields so tests can replace them.
type run struct {
	d      Deps
	tc     cmclient.TaskContext
	ledger *Ledger

	// solver is the parent run's implementation seam: the collaborators the
	// execute phase writes each subtask through (main-workspace git and tools,
	// the run ledger, the run's coder-model resolver) plus the board-ops/push
	// flags. Built in newRun bound to today's exact collaborators; Best-of-N
	// derives additional candidate solvers that target worktrees and stay off
	// the board.
	solver *solverCtx

	// Best-of-N fan-out state (Cfg.BestOfN >= 2). candidates holds every
	// candidate implementation raced inside execute; winner is the one the judge
	// selects; judgeModel is the model the judge phase runs on; notes fans mid-run
	// human turns out to the live candidates. All nil/empty for a single-solver
	// run.
	candidates []*candidate
	winner     *candidate // set by the judge phase when it picks a winner.
	judgeModel string     // the model the judge phase ran on ("" for an auto-win or fallback).
	notes      *userNotes

	// First-arrival subtask claims (Best-of-N only). The run - not any single
	// candidate - claims each subtask once, when the first candidate reaches
	// it, so the board shows in_progress while the race runs (and CM's parent
	// auto-transition fires on the first claim). subClaimMu guards claimedSubs;
	// stopSubHB stops the fan-out heartbeater that keeps those claims alive
	// against CM's stall sweep until the winner replay completes them (nil when
	// no heartbeater is running).
	subClaimMu  sync.Mutex
	claimedSubs map[string]bool
	stopSubHB   func()

	// Plan-phase outputs, consumed by later phases. Set by runPlan, or - on
	// resume - pre-loaded by reconcile from SubtaskStates before any phase runs.
	subtasks []subtaskRef
	// cardSizing is the card-level bar and budget, seeded from the planner's
	// card_tier and persisted on the parent body so a resumed run restores it.
	// Before it was persisted it had one writer and no reader on resume, so
	// every resumed run sized its review panel and its Best-of-N pool at the
	// moderate default no matter what the planner said. cardPlannerBar is the
	// planner's own word, kept for the same reason as subtaskRef.PlannerBar.
	cardSizing     sizing
	cardPlannerBar string

	// curPhase is the phase currently executing, set by the sequential FSM loop
	// in execute BEFORE each phase runs and read by spendAndReport to tag usage
	// reports. Written only by that loop (never by a concurrent candidate or
	// mob seat), so it needs no synchronization: the fan-out reads it while the
	// loop is blocked inside the phase.
	curPhase string

	// threadWriteFailNoted dedupes the review-thread write-back failure note
	// across the run's Copilot cycles: one broken permission fails the same
	// way in every cycle, and one card note says everything the fifth would.
	threadWriteFailNoted bool

	// body is the live parent-card body the FSM accumulates run history into
	// (## Diagnosis, ## Plan, ## Review Findings ...). Seeded from the task
	// context at newRun; recordSection upserts a section and pushes the updated
	// body to CM. On resume it starts from the refetched body, so prior sections
	// are preserved and re-recorded sections replace rather than duplicate.
	body string

	// taskDescription is the prompt-facing view of the parent description:
	// tc.Description with the recorded run-history sections stripped. On fresh
	// runs it equals the raw description; on resume it restores fresh-run
	// prompt shape instead of re-absorbing the accumulated history at every
	// site. Phases that need prior state re-supply it explicitly (planner:
	// design and prior plan; review: lastFindings). A human-authored section
	// named like a recorded one is stripped too - accepted cost, mitigated by
	// the planner re-supply baking it into subtask bodies.
	//
	// Written twice: newRun seeds it from tc.Description before planning, then
	// createSubtasks re-derives it from the now-current o.body once plan-phase
	// mutations (createFollowups' "## Split", recordUnreachable's
	// "## Unreachable Criteria") have landed - both headings survive
	// stripAgentSections by design, so the refresh is what lets execute's
	// coder prompts and the review specialists/synthesizers see them. The
	// first write stays authoritative through diagnose/design/drafting - not
	// because the HITL adjust loop avoids this field (plannerDescription
	// returns it as the base, only splicing in the prior "## Plan" from
	// o.tc.Description), but because of ordering: every draftPlan/mobDraftPlan
	// call that reads it happens strictly before createSubtasks, and every
	// createSubtasks call site in runPlan is terminal (a return follows
	// immediately), so the refresh below can never land before a planner read
	// it could poison.
	taskDescription string

	// staleRemoteTip is the remote tip of this run's card branch as observed at
	// reconcile time on a FRESH run (phase == ""). A non-empty value means a stale
	// branch from a prior, abandoned run exists: per spec §5.1 the fresh run owns
	// the branch and overwrites it at its first push with a force-with-lease
	// against this tip. Empty means the branch is absent (plain push). It is NOT
	// recorded on resume - resume continues the fetched branch, which is the state.
	staleRemoteTip string

	// firstPushDone guards the one-time stale-branch overwrite: the execute phase's
	// FIRST push uses ForcePushWithLease(branch, staleRemoteTip) when a stale tip
	// was recorded, and every push after that is plain (the branch is now ours).
	firstPushDone bool

	// prURL is the pull request the integrate phase opened this run, handed to
	// the pr_gates phase. Empty when no PR was opened - and on a resumed run
	// entering at pr_gates, where the gate falls back to tc.PRUrl (the URL the
	// earlier run reported through report_push).
	prURL string

	// ciSawChecks is set once any CI check was observed this run, so a CI gate
	// re-entered after a Copilot fix push never reads an empty first poll as
	// "no CI".
	ciSawChecks bool

	// ciObservedSettle is how long the CI gate waited from its start to the
	// first settled poll after its first read - one observed CI cycle. Zero
	// until a cycle has been watched. Sizes the fix-round reserve.
	ciObservedSettle time.Duration

	// reviewSummary is the review outcome captured on approval and carried into
	// the integrate phase's PR body: the synthesis verdict's one-line summary
	// alone when nothing survived the critique round, otherwise that summary
	// plus the surviving findings, framed by whether anything fixed them (see
	// approvedDespiteFindings and its siblings). Empty when review was skipped
	// (resume entering at integrate) or the summary was blank.
	reviewSummary string

	// selMu guards the shared model-selection state (coderModels, reselects,
	// excluded) so the Best-of-N fan-out's parallel candidate goroutines - which
	// all run through runCoderWith and recoverIncapable - never race on it. The
	// single-threaded parent execute path acquires it uncontended (a no-op).
	selMu sync.Mutex

	// coderModels records every distinct model that coded a subtask during
	// execute, so the review phase can exclude them from the specialist panel
	// (a model should not review its own code). Populated in runCoderWith under
	// selMu.
	coderModels map[string]bool

	// reselects counts in-run model re-selections triggered by a harness-incapable
	// model (one per recoverIncapable). It is capped at 3 per card across BOTH the
	// execute (coder) and review (synthesis/fix) recovery paths - a shared budget,
	// so a card that keeps drawing dud models parks rather than burning re-selections
	// forever.
	reselects int

	// fixFailed is the set of fix-coder models whose review fix round failed
	// this run - it landed no commit, left the verify red, or hit its turn
	// cap with a failing verify. Every later fix pick excludes them.
	//
	// fixBarSteps and fixBudgetSteps are the two correction counters, both
	// MONOTONE for the whole run. A round that produced nothing or left the
	// verify red is quality evidence and climbs the bar; a round that ran out
	// of turns is volume evidence and widens the budget without blaming the
	// model. A round that did BOTH - spent its whole window and still
	// committed a tree the next verify rejects - is charged on both axes.
	// Neither counter is ever lowered: runFix is shared with pr_gates, which
	// runs AFTER review approval, so clearing them on an approving verdict
	// handed the first gate round the model that had already failed. A call
	// site that must not escalate says so per-call, via fixRequest.NoEscalate.
	//
	// fixFailReason and fixCapReason are the card-log wording for the last
	// failure of each kind, kept apart because the two readers of a reason
	// state different things: one says the bar was escalated, the other that
	// the fix pool is exhausted, and neither is true of a round that merely ran
	// out of turns. Only markFixFailed writes fixFailReason; only markFixCapped
	// writes fixCapReason. lastFixModel is the model the most recent fix round
	// ran on.
	fixFailed      map[string]bool
	fixBarSteps    int
	fixBudgetSteps int
	fixFailReason  string
	fixCapReason   string
	lastFixModel   string

	// lastFixExhausted records whether the most recent fix round spent every turn
	// it was given, including a grace-turn landing that returns no error. The
	// loop reads it to charge the budget axis for caps the MaxTurnsError arm
	// never sees.
	lastFixExhausted bool

	// fixCappedPending records that the most recent fix round hit its turn cap
	// while committing work, and that its quality verdict is not in yet: the
	// next review round's verify gate settles it. A red gate there charges the
	// bar axis too (markFixFailed, "hit its turn cap with a failing verify");
	// a green or skipped gate consumes the flag with no bar charge. The gate
	// that settles it is whichever one runs next, on either path into review:
	// the cheap loop's consumption block, or the authoritative pass at the
	// cliff (which captures and clears the flag on entry, before delegating -
	// the loop's own block never runs that iteration). Cleared unconditionally
	// at every cheap-loop gate it reaches, and on the authoritative pass's
	// entry before any early return it can take, so it can never ride into a
	// park or error path.
	fixCappedPending bool

	// prevRoundGreen is whether the LAST review round's verify gate passed - the
	// half of the green->red comparison the loop otherwise forgets. A round that
	// discarded a regressing fix records it as green too: the branch is back on
	// the tree that gate proved.
	//
	// preFixHead is the commit the branch was on before the most recent fix
	// pass, captured so a fix that regresses the verify can be discarded instead
	// of parked on. Empty when the head could not be read - never a value from
	// an earlier round, which would discard more than the fixup that regressed.
	prevRoundGreen bool
	preFixHead     string

	// lastFixBase is the commit the branch sat on before the most recently
	// COMMITTED fix - set only when a fix pass actually lands, never on a
	// no-op or a discarded regression, so fixDeltaBlock diffs exactly the
	// change the next round's panel should see labeled as the prior fix.
	// Never cleared once set: a later round with no new fix still benefits
	// from seeing the last one that landed.
	lastFixBase string

	// excluded is the per-card set of models proven harness-incapable on this run.
	// It is threaded into every SelectInput.Exclude (coder selection and the review
	// panel) so a model that could not drive the tool loop is never re-picked.
	// Initialized in newRun.
	excluded map[string]bool

	// coderPinWarned and reviewerPinWarned guard the once-per-run advisory
	// warning when a non-empty pin fails resolvePin on the coder/reviewer
	// selector paths. Zero-initialized (false) by Go, so newRun needs no
	// explicit init.
	coderPinWarned    bool
	reviewerPinWarned bool

	// shortfallWarned guards the once-per-run advisory for a selection that
	// could not be served at the tier it asked for, keyed per phase, role and
	// the requested -> met bar pair. shortfallMu guards it and NOTHING else:
	// the Best-of-N candidate resolver holds selMu across a selection, so an
	// advisory raised from there must not reach for selMu.
	shortfallMu     sync.Mutex
	shortfallWarned map[string]bool

	// lastReviewBase is the HEAD SHA captured at the end of the previous round's
	// specialist review (mirrors CM's review-task workflow skill, which records
	// review_completed head=<sha>). The
	// next round diffs against it so the panel sees only the change since the last
	// review, not the whole branch. Empty -> full diff vs BaseBranch (round 1, or
	// before any specialist review has run). NOT restored on crash-resume: the
	// activity log is not readable through the current interfaces, and a resumed
	// run safely re-runs one full review, after which the delta base re-establishes.
	lastReviewBase string

	// lastFindings is the previous review round's findings text, fed to the next
	// round's panel and synthesizer so they verify resolution without re-raising
	// it as new scope (cross-round memory). Empty on a fresh run's round 1; on
	// resume it is seeded from the recorded review history so prior findings
	// reach the panel through this framing rather than as raw description text.
	lastFindings string

	// lastPanelFindings is the most recent SYNTHESIZED panel verdict, retained
	// across verify-red rounds: those replace lastFindings with the verify tail,
	// which would otherwise erase the mandate a regressing fix was chasing.
	lastPanelFindings string

	// grounding is the prebuilt REPO GROUNDING block (root + nested
	// AGENTS.md/CLAUDE.md), injected into model phases. Built once in
	// newRun; "" when the workspace has no instruction files.
	grounding string

	// taskImages are the assigned card's body images as OpenAI data-URL content
	// parts, attached to the planning-phase model calls only. nil when none.
	taskImages []llm.ImageURL

	// verify is the resolved verify plan for this run, cached by ensureVerify on
	// the first phase to reach the gate (execute, judge, or review). nil until
	// resolved. A resolved COMMAND is reused; a prior SKIP is re-resolved on
	// re-entry, so a bootstrap task that adds the project's tooling is verified.
	verify *verifyPlan

	// proposeAttempted records that the model-proposal tier has already fired this
	// run, so a skip re-resolved at a later phase re-runs only the cheap
	// declared/detection tiers and never fires a second proposal model call.
	proposeAttempted bool

	// lastVerify is the run's most recent gate result, updated each review round
	// (and left the zero skipped value when no gate ran). It feeds the honest
	// verify trailers on the PR body and the completion note - the run-level
	// answer to "was this change verified?".
	lastVerify verifyResult

	// runVerify is the RAW verify subprocess runner: it executes an argv and
	// reports the unclassified outcome, so classifyVerify (pure) does the tri-state
	// decision and test stubs stay trivial. It is a struct field so tests can stub
	// the subprocess; the default is verifyexec.Exec.
	runVerify verifyRunner

	// seatDebug is where mob session seat sub-run events go (kind-rewritten
	// JSONL); io.Discard when the worker supplied no writer. Set once in newRun.
	seatDebug io.Writer

	// mobSeats records the seat lineup of the most recent discussion so the
	// ## Discussion card record can name seats and models.
	mobSeats []mob.SeatConfig

	// envFacts caches the checkpoint briefings' environment block; computed
	// lazily on the first checkpoint so runs without execute checkpoints
	// never probe.
	envFacts string

	// mobEngine is the discussion-engine seam: tests script (Outcome, error)
	// here. nil = the real engine (mob.NewEngine(cfg).Discuss).
	mobEngine func(ctx context.Context, cfg mob.EngineConfig, t mob.Topic) (mob.Outcome, error)

	planFn      phaseFn
	executeFn   phaseFn
	judgeFn     phaseFn
	documentFn  phaseFn
	reviewFn    phaseFn
	integrateFn phaseFn
	prGatesFn   phaseFn
	doneFn      phaseFn
}

// verifyRunner runs a verify command (argv) rooted at dir with a per-command
// timeout and extra KEY=VALUE env, and reports the raw execution outcome. dir is
// the review workspace for the review gate and the candidate worktree for the
// Best-of-N judge. classifyVerify turns the outcome into the tri-state result;
// the default runner is verifyexec.Exec, and tests inject a stub.
type verifyRunner func(ctx context.Context, dir string, argv []string, timeout time.Duration, extraEnv []string) verifyexec.Outcome

// dataURLs encodes card image blobs as base64 data URLs for OpenAI image_url
// content parts. Returns nil for no blobs.
func dataURLs(blobs []cmclient.ImageBlob) []llm.ImageURL {
	if len(blobs) == 0 {
		return nil
	}

	out := make([]llm.ImageURL, 0, len(blobs))
	for _, b := range blobs {
		enc := base64.StdEncoding.EncodeToString(b.Data)
		out = append(out, llm.ImageURL{URL: "data:" + b.MIME + ";base64," + enc})
	}

	return out
}

// newRun builds a run seeded from the task context, with the budget ledger
// pre-loaded from the card's already-reported cost.
func newRun(d Deps, tc cmclient.TaskContext) *run {
	o := &run{
		d:  d,
		tc: tc,
		// The run ledger spans the shared phases; effectiveCeiling scales it for
		// Best-of-N (N execute allowances plus one for plan/judge/document/review/
		// integrate). For BestOfN < 2 effectiveCeiling degenerates to MaxCardCost,
		// so a single-solver run is byte-identical to before.
		ledger: NewLedger(effectiveCeiling(d.Cfg), tc.ReportedCostUSD),
	}

	o.coderModels = map[string]bool{}
	o.excluded = map[string]bool{}
	kv, s := readMeta(tc.Description)
	o.cardSizing = s
	o.cardPlannerBar = kv["seed"]
	// o.body keeps the marker: it is the persisted body, and dropping it here
	// would delete the marker on the next recordSection push.
	o.body = tc.Description
	o.taskDescription = stripAgentSections(stripMeta(tc.Description))
	o.lastFindings = recentReviewFindingsHistory(tc.Description)
	o.taskImages = dataURLs(tc.Images)

	// The parent solver binds the execute seam to today's exact collaborators:
	// the main workspace git/tools, the run ledger (o.ledger IS its ledger), and
	// the run's coder-model resolver, driving the board and pushing.
	o.solver = &solverCtx{
		git:        d.Git,
		ledger:     o.ledger,
		tools:      d.WriteTools,
		workspace:  d.Cfg.Workspace,
		coderModel: o.resolveCoderModel,
		boardOps:   true,
		push:       true,
	}

	grounding := groundingBlock(discoverGrounding(d.Cfg.Workspace))
	if d.Redact != nil {
		// The seed prompt is NOT covered by the harness redactor (it masks only
		// tool output/events), so a secret reaching the grounding block would go to
		// the LLM endpoint unmasked. Redact here, mirroring the tool-output contract
		// - defense-in-depth behind readGroundingFile's containment guard.
		grounding = d.Redact(grounding)
	}

	o.grounding = grounding
	// verifyexec.Exec matches verifyRunner: the review gate passes the workspace,
	// the judge passes each candidate worktree, and both pass the plan's timeout/env.
	o.runVerify = verifyexec.Exec

	o.seatDebug = d.SeatDebugWriter
	if o.seatDebug == nil {
		o.seatDebug = io.Discard
	}

	o.planFn = func(ctx context.Context) error { return runPlan(ctx, o) }
	o.executeFn = func(ctx context.Context) error { return runExecute(ctx, o) }
	o.judgeFn = func(ctx context.Context) error { return runJudge(ctx, o) }
	o.documentFn = func(ctx context.Context) error { return runDocument(ctx, o) }
	o.reviewFn = func(ctx context.Context) error { return runReview(ctx, o) }
	o.integrateFn = func(ctx context.Context) error { return runIntegrate(ctx, o) }
	o.prGatesFn = func(ctx context.Context) error { return runPRGates(ctx, o) }
	o.doneFn = func(ctx context.Context) error { return runDone(ctx, o) }

	return o
}

// phaseFnFor returns the phase function bound to the named phase.
func (o *run) phaseFnFor(phase string) phaseFn {
	switch phase {
	case "plan":
		return o.planFn
	case "execute":
		return o.executeFn
	case "judge":
		return o.judgeFn
	case "document":
		return o.documentFn
	case "review":
		return o.reviewFn
	case "integrate":
		return o.integrateFn
	case "pr_gates":
		return o.prGatesFn
	case "done":
		return o.doneFn
	default:
		return func(context.Context) error {
			return fmt.Errorf("unknown phase %q", phase)
		}
	}
}

// execute drives the FSM from the card's persisted phase to done. For every
// phase it persists the phase to the card BEFORE running the body, so an
// interrupted run leaves the stored phase pointing at the in-progress step.
//
// Budget parking is handled in this one place: if a phase body returns a
// *BudgetExceededError, execute logs the numbers to the card and returns the
// error without entering any further phase. The worker maps the sentinel to a
// WIP push plus a failed callback.
func (o *run) execute(ctx context.Context) error {
	start := o.tc.Phase
	if start == "" {
		start = "plan"
	}

	// Judge state is container-local (candidate worktrees, verify results, the
	// raced diffs) and never persisted, so a run that crashed in judge cannot be
	// resumed there - re-enter at execute to re-race the fan-out.
	if start == "judge" {
		start = "execute"
	}

	from := indexOf(phaseOrder, start)
	if from < 0 {
		return fmt.Errorf("card has unknown persisted phase %q", start)
	}

	// Crash-resume reconciliation runs ONCE, before any phase: it sorts out the
	// card branch (fresh: record the stale tip for the guarded overwrite; resume:
	// fetch + check out the branch that IS the state) and loads prior subtask
	// state into o.subtasks so phases skip finished work. See reconcile.
	if err := o.reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	for _, phase := range phaseOrder[from:] {
		o.curPhase = phase

		if err := o.d.Ops.SetPhase(ctx, o.d.Cfg.CardID, phase); err != nil {
			return fmt.Errorf("persist phase %s: %w", phase, err)
		}

		if err := o.phaseFnFor(phase)(ctx); err != nil {
			var be *BudgetExceededError
			if errors.As(err, &be) {
				// Park: record the numbers, then stop without entering the
				// next phase. Log failure is best-effort - the budget error is
				// the one that must surface to the worker.
				o.d.logCard(ctx, "%s", budgetLogMessage(be))
			}

			var cle *ContextLimitError
			if errors.As(err, &cle) {
				// Context-window park: same shape as the budget arm - log the
				// numbers best-effort, then stop without entering the next phase.
				o.d.logCard(ctx, "%s", contextLimitLogMessage(cle))
			}

			var mte *MaxTurnsError
			if errors.As(err, &mte) {
				// Turn-cap park: same shape as the budget/context arms - log
				// best-effort, then stop without entering the next phase.
				o.d.logCard(ctx, "%s", maxTurnsLogMessage(phase, mte))
			}

			var tme *ToolchainMissingError
			if errors.As(err, &tme) {
				// Toolchain-missing park: same shape as the other arms - log
				// best-effort, then stop without entering the next phase.
				o.d.logCard(ctx, "%s", toolchainLogMessage(tme))
			}

			var nme *NoModelError
			if errors.As(err, &nme) {
				// Model-selection park: same shape as the other arms - log
				// best-effort, then stop without entering the next phase.
				o.d.logCard(ctx, "%s", noModelLogMessage(nme))
			}

			var vpe *VerifyParkedError
			if errors.As(err, &vpe) {
				// Pre-commit verify park: same shape as the other arms, with
				// the failing output carried along - see verifyParkedLogMessage.
				o.d.logCard(ctx, "%s", verifyParkedLogMessage(vpe))
			}

			var soe *SplitOverflowError
			if errors.As(err, &soe) {
				// Split-overflow park: same shape as the other arms - log
				// best-effort, then stop without entering the next phase.
				o.d.logCard(ctx, "%s", splitOverflowLogMessage(soe))
			}

			return err
		}
	}

	return nil
}

// budgetLogMessage is the canonical card-log line for a budget park.
func budgetLogMessage(be *BudgetExceededError) string {
	return fmt.Sprintf("budget ceiling reached: spent $%.4f of $%.4f - parking work", be.Spent, be.Max)
}

// contextLimitLogMessage is the canonical card-log line for a context-window park.
func contextLimitLogMessage(cle *ContextLimitError) string {
	return fmt.Sprintf("context window reached on model %q (%d tokens) - parking work; split the subtask or pin a larger-window model", cle.Model, cle.ContextWindow)
}

// maxTurnsLogMessage is the canonical card-log line for a turn-cap park. The
// remedy differs by phase: the plan phase's budget is capped at planMaxTurns
// (see runmodel.go), an unexported constant with no config field, env var, or
// flag - an operator cannot raise it - and it is the phase that creates
// subtasks, so "raise CMX_MAX_TURNS" or "split the subtask" would both be
// no-ops there. Every other phase keeps the original wording.
func maxTurnsLogMessage(phase string, mte *MaxTurnsError) string {
	if phase == "plan" {
		return fmt.Sprintf("turn cap reached on model %q after %d turns - parking work; narrow the card's scope", mte.Model, mte.Turns)
	}

	return fmt.Sprintf("turn cap reached on model %q after %d turns - parking work; raise CMX_MAX_TURNS or split the subtask", mte.Model, mte.Turns)
}

// toolchainLogMessage is the canonical card-log line for a toolchain-missing
// park. A seeded verify-config error (see verifyConfigErrorMarker) and a
// container-runtime park (see containerRuntimeUnavailableMarker) each get
// their own wording, matching verifyToolchainSection: neither implicates a
// missing toolchain, so "verify toolchain cannot run here" would misdirect -
// rebuilding the worker image for a config problem, or installing a toolchain
// that was never the issue.
func toolchainLogMessage(tme *ToolchainMissingError) string {
	switch tme.Subject {
	case verifyConfigErrorMarker:
		return fmt.Sprintf("declared verify config could not be read (%s); parking card as blocked", tme.Reason)
	case containerRuntimeUnavailableMarker:
		return fmt.Sprintf("verify needs a container runtime the worker does not have (%s); parking card as blocked", tme.Reason)
	}

	return fmt.Sprintf("verify toolchain cannot run here (%s: %s - %s); parking card as blocked", tme.Tier, tme.Subject, tme.Reason)
}

// verifyParkNoteMax bounds the whole park note. ContextMatrix REJECTS an
// activity-log message over 2000 bytes (its maxLogMessage) instead of clipping
// it, and logCard swallows that rejection into a warning - so an over-long note
// does not arrive trimmed, it does not arrive at all.
const verifyParkNoteMax = 1900

// verifyParkOutputElision marks a note whose output was trimmed to fit, so the
// block visibly starts mid-stream rather than reading as the whole run.
const verifyParkOutputElision = "[earlier output elided]\n"

// verifyParkedLogMessage is the canonical card-log line for a pre-commit verify
// park. Alone among the park lines it carries output: the command and what it
// printed ARE the reason the card is blocked, and the container that held them
// is destroyed before a human reads the card.
//
// The header always survives, so the card names the subtask and the command
// even when nothing else fits; the output is then trimmed from the FRONT, since
// a build tool's diagnostics are concentrated in its last bytes.
func verifyParkedLogMessage(vpe *VerifyParkedError) string {
	header := fmt.Sprintf("subtask %s: `%s` still failed after one fix pass; parking card as blocked",
		vpe.Subtask, vpe.Command)

	const label = "\n\nVerify output (tail):\n\n"

	room := verifyParkNoteMax - len(header) - len(label) - len(verifyParkOutputElision)
	if vpe.Output == "" || room <= 0 {
		// A header long enough to crowd out the output can also overrun the cap
		// on its own (Command is whatever the project declared), so it takes the
		// same bound - keeping its head, which is where the subtask and command
		// are.
		return truncateBytes(header, verifyParkNoteMax)
	}

	return header + label + verifyParkOutputTail(vpe.Output, room)
}

// verifyParkOutputTail returns at most n bytes of out's tail, cut on a rune
// boundary and on a line boundary where one is available, marked as elided
// whenever anything was dropped.
func verifyParkOutputTail(out string, n int) string {
	if len(out) <= n {
		return out
	}

	tail := out[len(out)-n:]

	// Advance past a rune split by the cut, then past the partial first line, so
	// the block opens on a whole line rather than mid-word.
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}

	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)-1 {
		tail = tail[i+1:]
	}

	return verifyParkOutputElision + tail
}

// splitOverflowLogMessage is the canonical card-log line for a split-overflow
// park: the count, the cap, and the proposed titles so a human can re-cut the
// card without re-running the planner. The header always survives, so the
// card names the count and cap even when nothing else fits; the titles are
// then trimmed to what remains, mirroring verifyParkedLogMessage's bound.
func splitOverflowLogMessage(soe *SplitOverflowError) string {
	header := fmt.Sprintf("plan proposes %d follow-up cards (max %d) - parking; re-cut this card manually. Proposed: ",
		soe.Count, maxFollowupCards)

	room := verifyParkNoteMax - len(header)
	if room <= 0 {
		return truncateBytes(header, verifyParkNoteMax)
	}

	return header + truncateBytes(strings.Join(soe.Titles, "; "), room)
}

// truncateBytes caps s at n BYTES, keeping its head and cutting on a rune
// boundary. Distinct from truncateRunes, which caps a rune COUNT - the board's
// log limit is on bytes, which n runes can overrun by up to 4x.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}

	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}

// reselectCap bounds in-run model re-selections per card. A model that emits
// tool calls every turn but never forms valid arguments (harness-incapable) is
// blacklisted and swapped for the next-best pick; after this many swaps the run
// parks rather than churning through the catalog indefinitely.
const reselectCap = 3

// recoverIncapable handles a harness-incapable model encountered mid-phase: it
// blacklists the model on CM (best-effort), records the exclusion so the next
// selection skips it, and logs the swap. It returns an error - wrapping the
// IncapableError - once the per-card re-selection cap is exhausted, which the
// caller propagates to park the run. The incapable model executed no tools, so
// the caller can simply re-select and re-run the same unit; no git reset is
// needed.
func (o *run) recoverIncapable(ctx context.Context, ie *IncapableError) error {
	// Exclusion and the cap check must be atomic: under a Best-of-N fan-out,
	// parallel candidates share o.reselects and o.excluded, and the cap is a
	// single shared budget. Hold selMu across the whole mutation, then release
	// before the advisory I/O.
	o.selMu.Lock()

	if o.excluded == nil {
		o.excluded = map[string]bool{}
	}

	o.excluded[ie.Model] = true

	attempt := 0

	exhausted := o.reselects >= reselectCap
	if !exhausted {
		o.reselects++
		attempt = o.reselects
	}

	o.selMu.Unlock()

	// Best-effort, and done for the cap-exhausting model too: a proven
	// incapable model must not be the first pick of the next run just because
	// it happened to be the one that spent the last re-selection.
	_ = o.d.Ops.BlacklistModel(ctx, o.d.Cfg.CardID, ie.Model, ie.Reason) //nolint:errcheck

	if exhausted {
		o.d.logCard(ctx, "model %q harness-incapable; blacklisted - re-selection cap (%d) exhausted: %s", ie.Model, reselectCap, ie.Reason)

		return fmt.Errorf("re-selection cap (%d) exhausted after model %q: %w", reselectCap, ie.Model, ie)
	}

	o.d.logCard(ctx, "model %q harness-incapable; blacklisted and re-selecting (attempt %d/%d): %s", ie.Model, attempt, reselectCap, ie.Reason)

	return nil
}

// excludedModels returns a copy of the run's exclusion set, safe to hand to a
// selector while Best-of-N candidates may be growing the original.
func (o *run) excludedModels() map[string]bool {
	o.selMu.Lock()
	defer o.selMu.Unlock()

	return maps.Clone(o.excluded)
}

// isTerminal reports whether a subtask state counts as already finished and
// must not be re-run. The terminal set currently holds "done" (completed by a
// prior run) and "not_planned" (deliberately cancelled); future additions are
// added to this map in one place.
func isTerminal(state string) bool {
	_, ok := terminalStates[state]

	return ok
}

// terminalStates is the set of subtask states that must never be re-executed.
// A map[string]struct{} is used for O(1) lookup and the zero-value false
// semantics - no initializer needed.
var terminalStates = map[string]struct{}{
	"done":        {},
	"not_planned": {},
}

// indexOf returns the position of v in s, or -1 if absent.
func indexOf(s []string, v string) int {
	for i := range s {
		if s[i] == v {
			return i
		}
	}

	return -1
}

// Run drives the FSM for one card from its persisted phase (empty -> plan).
// It fetches the task context, seeds the budget ledger from the card's reported
// cost, and runs each phase in order, persisting the phase before working.
// Images are requested here because tc.Images feeds the planning phase.
func Run(ctx context.Context, d Deps) error {
	tc, err := d.Ops.GetTaskContext(ctx, d.Cfg.CardID, true)
	if err != nil {
		return fmt.Errorf("get task context: %w", err)
	}

	return newRun(d, tc).execute(ctx)
}
