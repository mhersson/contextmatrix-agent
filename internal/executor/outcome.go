package executor

import "github.com/mhersson/contextmatrix-agent/internal/metrics"

// resolveCause folds a recorded kill reason into what the supervision goroutine
// observed, producing the single value the run's terminal event and log footer
// carry. Precedence: a kill this goroutine performed itself (the container
// timeout, or a failed wait) wins; then a kill another part of the process
// recorded a reason for; otherwise what the wait result showed.
//
// The vocabulary is closed. A reason it does not name leaves the observed cause
// in place, so adding a kill reason means naming its cause here rather than
// widening the wire field by accident.
func resolveCause(observed ExitCause, reason string) ExitCause {
	switch {
	case observed == ExitTimeout, observed == ExitWaitFailure:
		return observed
	case reason == metrics.OutcomeIdleTimeout:
		return ExitIdleTimeout
	case reason == metrics.OutcomeKilled:
		return ExitKilled
	default:
		return observed
	}
}

// resolveOutcome maps the way a container ended to a container_duration outcome
// label. Precedence: an explicit container timeout wins; then a recorded kill
// reason (idle_timeout / killed); then a daemon-flagged wait, whose zero exit
// code cannot be trusted and is a failure regardless; otherwise the exit code
// (0 = success, any other = failure). The label vocabulary is coarser than the
// cause vocabulary, and stays that way: dashboards are built on these five
// values.
func resolveOutcome(cause ExitCause, reason string, exitCode int64) string {
	switch {
	case cause == ExitTimeout:
		return metrics.OutcomeTimeout
	case reason != "":
		return reason
	case cause == ExitDaemonError:
		return metrics.OutcomeFailure
	case exitCode == 0:
		return metrics.OutcomeSuccess
	default:
		return metrics.OutcomeFailure
	}
}
