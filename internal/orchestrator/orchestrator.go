// Package orchestrator drives an autonomous card through plan -> execute ->
// review -> integrate -> done. Code owns all sequencing; models run inside
// phases. Each phase persists itself to the card BEFORE doing work, so the
// stored phase always reads "in progress or interrupted".
//
// Boundary rule: this package imports harness, llm, registry, tools, events,
// and cmclient — never internal/worker. The git surface the FSM needs is
// declared here as the GitOps interface (consuming-package convention);
// *worker.Git satisfies it via the worker wiring task.
package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

// Ops is the card-operation surface the FSM needs. It is satisfied by
// *cmclient.Client; the compile-time assertion lands in the worker wiring task
// (which is allowed to import both packages).
type Ops interface {
	ClaimCard(ctx context.Context, cardID string) error
	GetTaskContext(ctx context.Context, cardID string) (cmclient.TaskContext, error)
	CreateCard(ctx context.Context, project, parent, title, body string, dependsOn []string) (string, error)
	SetPhase(ctx context.Context, cardID, phase string) error
	TransitionCard(ctx context.Context, cardID, state string) error
	StartReview(ctx context.Context, cardID string) error
	IncrementReviewAttempts(ctx context.Context, cardID string) (int, error)
	SubtaskStates(ctx context.Context, project, parentID string) ([]cmclient.SubtaskState, error)
	AddLog(ctx context.Context, cardID, message string) error
	ReportUsage(ctx context.Context, cardID, model string, promptTokens, completionTokens int64, actualCostUSD float64) error
	ReportPush(ctx context.Context, cardID, branch, prURL string) error
	CompleteTask(ctx context.Context, cardID, summary string) error
	ReleaseCard(ctx context.Context, cardID string) error
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
}

// Config carries the per-run parameters the FSM needs.
type Config struct {
	Project           string
	CardID            string
	Branch            string // cm/<card-id-lower>
	BaseBranch        string
	AgentID           string
	Workspace         string
	MaxCardCost       float64
	PriceHeadroom     float64
	PayloadModel      string // CM's default_model from the trigger; "" = serve default
	DefaultModel      string // serve-config default
	MaxTurns          int
	ToolOutputMax     int
	ReviewAttemptsCap int // 5, CM's convention
}

// Deps bundles the collaborators the FSM drives.
type Deps struct {
	Ops        Ops
	Git        GitOps
	Client     llm.LLM
	Emit       *events.Emitter
	Registry   *registry.Registry
	WriteTools *tools.Registry // full toolset rooted at the workspace
	ReadTools  *tools.Registry // read-only subset for planner/reviewers
	Cfg        Config
}

// phaseOrder is the fixed forward sequence of phases. Run enters at the card's
// persisted phase and never moves backward through this slice.
var phaseOrder = []string{"plan", "execute", "review", "integrate", "done"}

// errNotImplemented is the default phase-function result until the real phase
// implementations land in later tasks. Tests override the phase functions.
var errNotImplemented = errors.New("phase not implemented")

// phaseFn is a single phase's body.
type phaseFn func(context.Context) error

// run is the live FSM state for one card. The phase functions are stored as
// fields so tests can replace them and later tasks can wire in real bodies.
type run struct {
	d      Deps
	tc     cmclient.TaskContext
	ledger *Ledger

	// Plan-phase outputs, consumed by later phases. Set by runPlan.
	subtasks []subtaskRef
	cardTier string

	planFn      phaseFn
	executeFn   phaseFn
	reviewFn    phaseFn
	integrateFn phaseFn
	doneFn      phaseFn
}

// newRun builds a run seeded from the task context, with the budget ledger
// pre-loaded from the card's already-reported cost and the phase functions
// defaulting to the not-yet-implemented stubs.
func newRun(d Deps, tc cmclient.TaskContext) *run {
	o := &run{
		d:      d,
		tc:     tc,
		ledger: NewLedger(d.Cfg.MaxCardCost, tc.ReportedCostUSD),
	}

	o.planFn = func(ctx context.Context) error { return runPlan(ctx, o) }
	o.executeFn = o.notImplemented
	o.reviewFn = o.notImplemented
	o.integrateFn = o.notImplemented
	o.doneFn = o.notImplemented

	return o
}

func (o *run) notImplemented(context.Context) error { return errNotImplemented }

// phaseFnFor returns the phase function bound to the named phase.
func (o *run) phaseFnFor(phase string) phaseFn {
	switch phase {
	case "plan":
		return o.planFn
	case "execute":
		return o.executeFn
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

	from := indexOf(phaseOrder, start)
	if from < 0 {
		return fmt.Errorf("card has unknown persisted phase %q", start)
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

			return err
		}
	}

	return nil
}

// budgetLogMessage is the canonical card-log line for a budget park.
func budgetLogMessage(be *BudgetExceededError) string {
	return fmt.Sprintf("budget ceiling reached: spent $%.4f of $%.4f — parking work", be.Spent, be.Max)
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
func Run(ctx context.Context, d Deps) error {
	tc, err := d.Ops.GetTaskContext(ctx, d.Cfg.CardID)
	if err != nil {
		return fmt.Errorf("get task context: %w", err)
	}

	return newRun(d, tc).execute(ctx)
}
