package harness

import (
	"context"

	"github.com/mhersson/contextmatrix-agent/internal/events"
)

// Verdict is the result of an out-of-loop success check.
type Verdict struct {
	OK     bool
	Detail string
}

// Check evaluates a success criterion against the post-run workspace state.
type Check func(ctx context.Context) (Verdict, error)

// Verify runs check once (after Run), emits a `verification` event, and returns
// the verdict. It is intentionally separate from Run: the loop is FSM-free and
// never adjudicates success. A nil check is a no-op (callers with no criterion
// skip verification) and emits nothing.
func Verify(ctx context.Context, emit *events.Emitter, check Check) (Verdict, error) {
	if check == nil {
		return Verdict{}, nil
	}
	v, err := check(ctx)
	if err != nil {
		emit.Emit(events.Verification, map[string]any{"ok": false, "error": err.Error()})
		return Verdict{}, err
	}
	emit.Emit(events.Verification, map[string]any{"ok": v.OK, "detail": v.Detail})
	return v, nil
}
