package orchestrator

import (
	"context"
	"fmt"
)

// logCard appends a best-effort advisory entry to the parent card's activity
// log; logCardID targets an explicit card. Failures are swallowed - an
// advisory log line must never abort the run.
func (d *Deps) logCard(ctx context.Context, format string, args ...any) {
	d.logCardID(ctx, d.Cfg.CardID, format, args...)
}

func (d *Deps) logCardID(ctx context.Context, cardID, format string, args ...any) {
	_ = d.Ops.AddLog(ctx, cardID, fmt.Sprintf(format, args...)) //nolint:errcheck // advisory; never fails the run
}
