package orchestrator

import "fmt"

// ToolchainMissingError marks the verify-resolution park: a declared verify
// command or a detected repo-convention marker implicates a toolchain that
// cannot run in this container, and no tier - including a model proposal,
// which gets its own chance to rescue first - resolved a runnable command.
// The worker maps it like the budget/context/turn-cap parks: push WIP,
// release the claim, fail - so a human can add the toolchain to the worker
// image or fix the declared command.
type ToolchainMissingError struct {
	Tier    string // "declared" or "detected"
	Subject string // the declared command, or the detected toolchain (e.g. "maven project")
	Reason  string // why the toolchain probe failed
}

func (e *ToolchainMissingError) Error() string {
	return fmt.Sprintf("verify toolchain cannot run here (%s: %s - %s)", e.Tier, e.Subject, e.Reason)
}
