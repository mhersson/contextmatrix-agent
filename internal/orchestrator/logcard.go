package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
)

// logMessageHeadMax bounds how much of a swallowed AddLog failure's message
// reaches slog, so a long advisory line (a completion note, a verify tail)
// does not spam the process log with what the board never got anyway.
const logMessageHeadMax = 200

// logCard appends a best-effort advisory entry to the parent card's activity
// log; logCardID targets an explicit card. Failures are swallowed - an
// advisory log line must never abort the run - but a rejected write (e.g. CM's
// add_log requiring an active claim) is warned to stderr with the card ID and
// the message head, so a silently dropped note is at least visible somewhere.
func (d *Deps) logCard(ctx context.Context, format string, args ...any) {
	d.logCardID(ctx, d.Cfg.CardID, format, args...)
}

func (d *Deps) logCardID(ctx context.Context, cardID, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)

	if err := d.Ops.AddLog(ctx, cardID, msg); err != nil {
		head := msg
		if len(head) > logMessageHeadMax {
			head = head[:logMessageHeadMax] + "..."
		}

		slog.Warn("add_log failed; advisory note dropped from the board",
			"card_id", cardID, "message", head, "error", err)
	}
}
