package executor

import "github.com/mhersson/contextmatrix-agent/internal/metrics"

// resolveOutcome maps the way a container ended to a container_duration outcome
// label. Precedence: an explicit container timeout wins; then a recorded kill
// reason (idle_timeout / killed); otherwise the exit code (0 = success, any
// other = failure).
func resolveOutcome(timedOut bool, reason string, exitCode int64) string {
	switch {
	case timedOut:
		return metrics.OutcomeTimeout
	case reason != "":
		return reason
	case exitCode == 0:
		return metrics.OutcomeSuccess
	default:
		return metrics.OutcomeFailure
	}
}
