// Package orchestrator drives an autonomous card through plan -> execute ->
// document -> review -> integrate -> done. Code owns all sequencing; models run
// inside phases. Each phase persists itself to the card BEFORE doing work, so the
// stored phase always reads "in progress or interrupted".
//
// Boundary rule: this package imports harness, llm, registry, tools, events,
// and cmclient — never internal/worker. The git surface the FSM needs is
// declared here as the GitOps interface (consuming-package convention);
// *worker.Git satisfies it via the worker wiring task.
package orchestrator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/coop"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// Ops is the card-operation surface the FSM needs. It is satisfied by
// *cmclient.Client; the compile-time assertion lands in the worker wiring task
// (which is allowed to import both packages).
type Ops interface {
	ClaimCard(ctx context.Context, cardID string) error
	Heartbeat(ctx context.Context, cardID string) error
	GetTaskContext(ctx context.Context, cardID string, includeImages bool) (cmclient.TaskContext, error)
	CreateCard(ctx context.Context, project, parent, title, body string, dependsOn []string) (string, error)
	SetPhase(ctx context.Context, cardID, phase string) error
	UpdateCardBody(ctx context.Context, cardID, body string) error
	TransitionCard(ctx context.Context, cardID, state string) error
	StartReview(ctx context.Context, cardID string) error
	IncrementReviewAttempts(ctx context.Context, cardID string) (int, error)
	SubtaskStates(ctx context.Context, project, parentID string) ([]cmclient.SubtaskState, error)
	AddLog(ctx context.Context, cardID, message string) error
	ReportUsage(ctx context.Context, cardID, model string, promptTokens, completionTokens int64, actualCostUSD float64) error
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
	ReviewAttemptsCap int // 3, CM's convention
	// Interactive is the sole mode flag: true => HITL (gates wait on Human and
	// brainstorming runs for creative cards); false => autonomous (gates pass
	// through, brainstorming skipped). Autonomous behavior is byte-for-byte the
	// pre-HITL behavior.
	Interactive bool

	// BestOfN, when >= 2, fans execute out into N candidate implementations
	// judged before document. 0/1 = normal single-solver run.
	BestOfN int

	// Coop configures co-op discussions (spec 2026-07-10). Zero value = off.
	Coop CoopConfig

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
	// resolution), delivered via CMX_VERIFY. nil means nothing declared — the
	// gate falls back to repo-convention detection and then a model proposal.
	// It is an orchestrator-local type so the package need not import protocol.
	Verify *DeclaredVerify
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
	// GitForDir returns a GitOps rooted at dir with NO branch policy set —
	// guardPush fails closed, so candidate handles structurally cannot push.
	// Used by Best-of-N to hand each candidate worktree its own git handle.
	GitForDir  func(dir string) GitOps
	PR         PRCreator // opens the pull request in the integrate phase (gh CLI seam)
	Client     llm.LLM
	Emit       *events.Emitter
	Registry   *registry.Registry
	WriteTools *tools.Registry // full toolset rooted at the workspace
	// WriteToolsForDir builds the full write toolset rooted at dir. Used by
	// Best-of-N to give each candidate worktree its own jailed tool registry.
	WriteToolsForDir func(dir string) *tools.Registry
	ReadTools        *tools.Registry // read-only subset for planner/reviewers
	SkillTool        tools.Tool      // optional; engaged by coder/review/document subagents (nil when no task-skills)
	Cfg              Config
	Redact           func(string) string // nil = identity; scrubs tool output in phase runs (wired by the worker)
	// Human is the HITL ask-and-wait channel, satisfied by the worker's live
	// Inbox. It is a genuine nil for autonomous runs; mode is read from
	// Cfg.Interactive, never from Human != nil (the nil-concrete footgun).
	Human harness.Inbox

	// SeatDebugWriter receives the JSONL event lines of co-op seat sub-runs,
	// kind-rewritten to "seat_debug" so the service log bridge (which skips
	// unknown kinds) keeps them off the live card stream while they stay in
	// the container stdout for debugging. The worker points it at process
	// stdout — the same stream the work command's transcript emitter writes.
	// nil = io.Discard (tests, standalone runs).
	SeatDebugWriter io.Writer
}

// phaseOrder is the fixed forward sequence of phases. Run enters at the card's
// persisted phase and never moves backward through this slice. The judge phase
// is a no-op for normal single-solver runs (Cfg.BestOfN < 2); it races and picks
// the Best-of-N winner between execute and document.
var phaseOrder = []string{"plan", "execute", "judge", "document", "review", "integrate", "done"}

// phaseFn is a single phase's body.
type phaseFn func(context.Context) error

// run is the live FSM state for one card. The phase functions are stored as
// fields so tests can replace them and later tasks can wire in real bodies.
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

	// First-arrival subtask claims (Best-of-N only). The run — not any single
	// candidate — claims each subtask once, when the first candidate reaches
	// it, so the board shows in_progress while the race runs (and CM's parent
	// auto-transition fires on the first claim). subClaimMu guards claimedSubs;
	// stopSubHB stops the fan-out heartbeater that keeps those claims alive
	// against CM's stall sweep until the winner replay completes them (nil when
	// no heartbeater is running).
	subClaimMu  sync.Mutex
	claimedSubs map[string]bool
	stopSubHB   func()

	// Plan-phase outputs, consumed by later phases. Set by runPlan, or — on
	// resume — pre-loaded by reconcile from SubtaskStates before any phase runs.
	subtasks []subtaskRef
	cardTier string

	// body is the live parent-card body the FSM accumulates run history into
	// (## Diagnosis, ## Plan, ## Review Findings ...). Seeded from the task
	// context at newRun; recordSection upserts a section and pushes the updated
	// body to CM. On resume it starts from the refetched body, so prior sections
	// are preserved and re-recorded sections replace rather than duplicate.
	body string

	// staleRemoteTip is the remote tip of this run's card branch as observed at
	// reconcile time on a FRESH run (phase == ""). A non-empty value means a stale
	// branch from a prior, abandoned run exists: per spec §5.1 the fresh run owns
	// the branch and overwrites it at its first push with a force-with-lease
	// against this tip. Empty means the branch is absent (plain push). It is NOT
	// recorded on resume — resume continues the fetched branch, which is the state.
	staleRemoteTip string

	// firstPushDone guards the one-time stale-branch overwrite: the execute phase's
	// FIRST push uses ForcePushWithLease(branch, staleRemoteTip) when a stale tip
	// was recorded, and every push after that is plain (the branch is now ours).
	firstPushDone bool

	// reviewSummary is the synthesis verdict's one-line summary captured on
	// approval, carried into the integrate phase's PR body. Empty when review was
	// skipped (resume entering at integrate) or the summary was blank.
	reviewSummary string

	// selMu guards the shared model-selection state (coderModels, reselects,
	// excluded) so the Best-of-N fan-out's parallel candidate goroutines — which
	// all run through runCoderWith and recoverIncapable — never race on it. The
	// single-threaded parent execute path acquires it uncontended (a no-op).
	selMu sync.Mutex

	// coderModels records every distinct model that coded a subtask during
	// execute, so the review phase can exclude them from the specialist panel
	// (a model should not review its own code). Populated in runCoderWith under
	// selMu.
	coderModels map[string]bool

	// reselects counts in-run model re-selections triggered by a harness-incapable
	// model (one per recoverIncapable). It is capped at 3 per card across BOTH the
	// execute (coder) and review (synthesis/fix) recovery paths — a shared budget,
	// so a card that keeps drawing dud models parks rather than burning re-selections
	// forever.
	reselects int

	// excluded is the per-card set of models proven harness-incapable on this run.
	// It is threaded into every SelectInput.Exclude (coder selection and the review
	// panel) so a model that could not drive the tool loop is never re-picked.
	// Initialized in newRun.
	excluded map[string]bool

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
	// it as new scope (cross-round memory). Empty on round 1 and on resume.
	lastFindings string

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
	// verify trailers on the PR body and the completion note — the run-level
	// answer to "was this change verified?".
	lastVerify verifyResult

	// runVerify is the RAW verify subprocess runner: it executes an argv and
	// reports the unclassified outcome, so classifyVerify (pure) does the tri-state
	// decision and test stubs stay trivial. It is a struct field so tests can stub
	// the subprocess; the default is verifyexec.Exec.
	runVerify verifyRunner

	// seatDebug is where co-op seat sub-run events go (kind-rewritten JSONL);
	// io.Discard when the worker supplied no writer. Set once in newRun.
	seatDebug io.Writer

	// coopSeats records the seat lineup of the most recent discussion so the
	// ## Discussion card record can name seats and models.
	coopSeats []coop.SeatConfig

	// coopEngine is the discussion-engine seam: tests script (Outcome, error)
	// here. nil = the real engine (coop.NewEngine(cfg).Discuss).
	coopEngine func(ctx context.Context, cfg coop.EngineConfig, t coop.Topic) (coop.Outcome, error)

	planFn      phaseFn
	executeFn   phaseFn
	judgeFn     phaseFn
	documentFn  phaseFn
	reviewFn    phaseFn
	integrateFn phaseFn
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
// pre-loaded from the card's already-reported cost and the phase functions
// defaulting to the not-yet-implemented stubs.
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
	o.body = tc.Description
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
		// — defense-in-depth behind readGroundingFile's containment guard.
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
	// resumed there — re-enter at execute to re-race the fan-out.
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
		if err := o.d.Ops.SetPhase(ctx, o.d.Cfg.CardID, phase); err != nil {
			return fmt.Errorf("persist phase %s: %w", phase, err)
		}

		if err := o.phaseFnFor(phase)(ctx); err != nil {
			var be *BudgetExceededError
			if errors.As(err, &be) {
				// Park: record the numbers, then stop without entering the
				// next phase. Log failure is best-effort — the budget error is
				// the one that must surface to the worker.
				_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, budgetLogMessage(be)) //nolint:errcheck
			}

			var cle *ContextLimitError
			if errors.As(err, &cle) {
				// Context-window park: same shape as the budget arm — log the
				// numbers best-effort, then stop without entering the next phase.
				_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, contextLimitLogMessage(cle)) //nolint:errcheck
			}

			var mte *MaxTurnsError
			if errors.As(err, &mte) {
				// Turn-cap park: same shape as the budget/context arms — log
				// best-effort, then stop without entering the next phase.
				_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, maxTurnsLogMessage(mte)) //nolint:errcheck
			}

			return err
		}
	}

	return nil
}

// budgetLogMessage is the canonical card-log line for a budget park.
func budgetLogMessage(be *BudgetExceededError) string {
	return fmt.Sprintf("budget ceiling reached: spent $%.4f of $%.4f — parking work", be.Spent, be.Max)
}

// contextLimitLogMessage is the canonical card-log line for a context-window park.
func contextLimitLogMessage(cle *ContextLimitError) string {
	return fmt.Sprintf("context window reached on model %q (%d tokens) — parking work; split the subtask or pin a larger-window model", cle.Model, cle.ContextWindow)
}

// maxTurnsLogMessage is the canonical card-log line for a turn-cap park.
func maxTurnsLogMessage(mte *MaxTurnsError) string {
	return fmt.Sprintf("turn cap reached on model %q after %d turns — parking work; raise CMX_MAX_TURNS or split the subtask", mte.Model, mte.Turns)
}

// reselectCap bounds in-run model re-selections per card. A model that emits
// tool calls every turn but never forms valid arguments (harness-incapable) is
// blacklisted and swapped for the next-best pick; after this many swaps the run
// parks rather than churning through the catalog indefinitely.
const reselectCap = 3

// recoverIncapable handles a harness-incapable model encountered mid-phase: it
// blacklists the model on CM (best-effort), records the exclusion so the next
// selection skips it, and logs the swap. It returns an error — wrapping the
// IncapableError — once the per-card re-selection cap is exhausted, which the
// caller propagates to park the run. The incapable model executed no tools, so
// the caller can simply re-select and re-run the same unit; no git reset is
// needed.
func (o *run) recoverIncapable(ctx context.Context, ie *IncapableError) error {
	// The cap check + increment + exclusion must be atomic: under a Best-of-N
	// fan-out, parallel candidates share o.reselects and o.excluded, and the cap
	// is a single shared budget. Hold selMu across the whole mutation, capture the
	// attempt number, then release before the advisory I/O.
	o.selMu.Lock()

	if o.reselects >= reselectCap {
		o.selMu.Unlock()

		return fmt.Errorf("re-selection cap (%d) exhausted after model %q: %w", reselectCap, ie.Model, ie)
	}

	o.reselects++
	attempt := o.reselects

	if o.excluded == nil {
		o.excluded = map[string]bool{}
	}

	o.excluded[ie.Model] = true

	o.selMu.Unlock()

	// Best-effort: the recovery proceeds (re-select + re-run) regardless of a
	// reporting failure; the blacklist is an advisory hint to CM and future runs.
	_ = o.d.Ops.BlacklistModel(ctx, o.d.Cfg.CardID, ie.Model, ie.Reason) //nolint:errcheck
	_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID,                              //nolint:errcheck
		fmt.Sprintf("model %q harness-incapable; blacklisted and re-selecting (attempt %d/%d)", ie.Model, attempt, reselectCap))

	return nil
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
