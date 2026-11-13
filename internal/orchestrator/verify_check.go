package orchestrator

import (
	"context"
	"errors"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
)

// VerifyCandidate is one verify command the repo declares by convention.
// Display is what the gate would show; Argv is what it would exec.
type VerifyCandidate struct {
	Argv    []string
	Display string
	Wrapper bool
}

// VerifyCandidates lists every runnable detected verify command: the wrapper
// first, then each marker row that resolves in table order, or the nested
// scan's command when the root has no marker. Never nil.
func VerifyCandidates(workspace string) []VerifyCandidate {
	out := []VerifyCandidate{}
	seen := map[string]bool{}

	add := func(argv []string, display string, wrapper bool) {
		if argv == nil || seen[display] {
			return
		}

		seen[display] = true

		out = append(out, VerifyCandidate{Argv: argv, Display: display, Wrapper: wrapper})
	}

	if a := detectWrapper(workspace); a != nil {
		add(a, strings.Join(a, " "), true)
	}

	for _, row := range detectRows {
		if !row.present(workspace) {
			continue
		}

		if a, _ := row.resolve(workspace); a != nil {
			add(a, strings.Join(a, " "), false)
		}
	}

	if !anyMarkerPresent(workspace) {
		det := detectNested(workspace)
		add(det.Argv, det.Display, det.Wrapper)
	}

	return out
}

// trialOutcome names a trial's result with the protocol's trial constants.
func trialOutcome(res verifyResult, err error) string {
	var tm *ToolchainMissingError
	if errors.As(err, &tm) {
		return protocol.TrialInfrastructure
	}

	switch res.Skip {
	case skipTimeout:
		return protocol.TrialTimeout
	case skipExhausted:
		return protocol.TrialInfrastructure
	case skipToolMissing:
		return protocol.TrialToolchain
	default:
		return protocol.TrialFinished
	}
}

// TrialVerify runs candidate c once with the gate's protections. err is set
// only for a canceled context.
func TrialVerify(
	ctx context.Context,
	dir string,
	c VerifyCandidate,
	timeout time.Duration,
	env []string,
	redact func(string) string,
) (trial, output string, err error) {
	plan := verifyPlan{Argv: c.Argv, Timeout: timeout, Env: env, Wrapper: c.Wrapper}

	res, err := execVerifyCommand(ctx, verifyexec.Exec, dir, plan, redact)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", ctxErr
	}

	return trialOutcome(res, err), res.Output, nil
}
