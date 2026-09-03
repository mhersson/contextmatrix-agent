package orchestrator

import (
	"errors"

	"github.com/mhersson/contextmatrix-harness/events"
)

// sizingObservationKind is the transcript row recording what one coder attempt
// cost in turns against its window - one row per ATTEMPT, incapable attempts
// included, so their near-zero turns do not pollute the ratio. Nothing in the
// orchestrator reads it; the field names are a wire contract for offline
// analysis. Like model_selected it reaches the durable transcript only.
const sizingObservationKind = "sizing_observation"

// sizingObs is one coder attempt's measurement.
type sizingObs struct {
	Phase  string
	Solver string // solo | candidate | fix
	// Subtask is the unit this attempt ran on: the subtask a coder attempt
	// implemented, or the one a pre-commit verify fix repaired. Empty for a
	// card-scoped fix round, which addresses findings across the whole card.
	Subtask string
	// Reselect is THIS INVOCATION's attempt index, 0 for the first attempt. It
	// restarts at 0 for every subtask and every fix round; the cap bounding it
	// is run-wide and shared across both paths, so a run's re-selections are
	// not the sum of this column.
	Reselect   int
	Model      string
	Bar        string
	BudgetStep int
	// PlannerBar is the planner's own word for the unit named by Subtask.
	// Pairing it with Bar separates a run at the planner's estimate from a run
	// at a corrected one, which only holds while both halves describe the same
	// unit.
	PlannerBar  string
	MaxTurns    int
	WrapUpTurns int
	Turns       int
	// TurnRatio is derived by emitSizingObs, so no call site can compute it
	// against a window it did not run at. Zero when MaxTurns is not positive:
	// the harness substituted a default this side does not know.
	TurnRatio  float64
	Outcome    string // done | done_at_cap | incapable | max_turns | error
	DurationMS int64
}

// sizingOutcome classifies how an attempt ended, from the error the choke point
// normalised and from the turns the attempt actually spent. The values are
// disjoint and ordered, so an attempt matches exactly one.
//
// The turn pair is what separates done_at_cap from done, and it is needed
// because the error alone cannot make that split: the coder family runs with
// the harness grace turn, which grants one terminal call after the cap is spent
// and returns without setting the max_turns reason, so an attempt that used its
// whole window arrives here with a nil error. max_turns therefore means the run
// was cut off at the cap, and done_at_cap means it ended cleanly with the window
// spent - both are exhausted windows, told apart by whether the model landed a
// terminal call.
func sizingOutcome(err error, turns, maxTurns int) string {
	var (
		ie  *IncapableError
		mte *MaxTurnsError
	)

	switch {
	case err == nil && windowExhausted(turns, maxTurns):
		return "done_at_cap"
	case err == nil:
		return "done"
	case errors.As(err, &ie):
		return "incapable"
	case errors.As(err, &mte):
		return "max_turns"
	default:
		return "error"
	}
}

// emitSizingObs writes one attempt's measurement to the run transcript. A nil
// emitter is a no-op, which is how the orchestrator is wired in tests that do
// not read the transcript.
//
// The field names are a wire contract: a later analysis parses them to ask what
// a bar and a budget were worth.
func (o *run) emitSizingObs(rec sizingObs) {
	if o.d.Emit == nil {
		return
	}

	rec.TurnRatio = 0
	if rec.MaxTurns > 0 {
		rec.TurnRatio = float64(rec.Turns) / float64(rec.MaxTurns)
	}

	o.d.Emit.Emit(events.Kind(sizingObservationKind), map[string]any{
		"phase":         rec.Phase,
		"solver":        rec.Solver,
		"subtask":       rec.Subtask,
		"reselect":      rec.Reselect,
		"model":         rec.Model,
		"bar":           rec.Bar,
		"budget_step":   rec.BudgetStep,
		"planner_bar":   rec.PlannerBar,
		"max_turns":     rec.MaxTurns,
		"wrap_up_turns": rec.WrapUpTurns,
		"turns":         rec.Turns,
		"turn_ratio":    rec.TurnRatio,
		"outcome":       rec.Outcome,
		"duration_ms":   rec.DurationMS,
	})
}
