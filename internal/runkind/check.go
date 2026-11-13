package runkind

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// trialKind is the event that records the check's trial run.
const trialKind events.Kind = "check_trial"

// checkDeps are the check's effects.
type checkDeps struct {
	candidates func(ws string) []orchestrator.VerifyCandidate
	trial      func(ctx context.Context, ws string, c orchestrator.VerifyCandidate, timeout time.Duration, env []string) (trial, output string, err error)
}

// check lists the base branch's verify candidates and, when in has a Verify,
// trial-runs it once if trialCandidate accepts it.
func check(ctx context.Context, ws string, in protocol.CheckInput, d checkDeps, emit *events.Emitter) (protocol.CheckResult, error) {
	cands := d.candidates(ws)

	res := protocol.CheckResult{Candidates: make([]string, 0, len(cands))}
	for _, c := range cands {
		res.Candidates = append(res.Candidates, c.Display)
	}

	if in.Verify == nil {
		return res, nil
	}

	c, ok := trialCandidate(in, cands)
	if !ok {
		res.Trial = protocol.TrialRejected

		return res, nil
	}

	trial, err := runTrial(ctx, ws, c, *in.Verify, d, emit)
	if err != nil {
		return protocol.CheckResult{}, err
	}

	res.Trial = trial

	return res, nil
}

// runTrial runs c once with v's timeout and env, reporting progress while it
// runs and the outcome after.
func runTrial(
	ctx context.Context,
	ws string,
	c orchestrator.VerifyCandidate,
	v protocol.VerifyConfig,
	d checkDeps,
	emit *events.Emitter,
) (string, error) {
	start := time.Now()
	stop := orchestrator.StartTicker(ctx, progressInterval, func(context.Context) {
		emit.Emit(progressKind, map[string]any{"step": "trial", "elapsed_seconds": int(time.Since(start).Seconds())})
	})

	trial, output, err := d.trial(ctx, ws, c,
		orchestrator.ClampVerifyTimeout(v.TimeoutSeconds),
		orchestrator.ResolveVerifyEnv(&orchestrator.DeclaredVerify{Env: v.Env}))

	stop()

	if err != nil {
		return "", fmt.Errorf("trial verify: %w", err)
	}

	emit.Emit(trialKind, map[string]any{"command": c.Display, "trial": trial, "output": output})

	return trial, nil
}

// trialCandidate picks what the trial runs. The command in force today counts
// as declared and runs as the gate runs a declared one; a changed command runs
// only as a candidate it matches and only when it is plain argv.
func trialCandidate(in protocol.CheckInput, cands []orchestrator.VerifyCandidate) (orchestrator.VerifyCandidate, bool) {
	if in.Verify == nil {
		return orchestrator.VerifyCandidate{}, false
	}

	cmd := in.Verify.Command
	if len(strings.Fields(cmd)) == 0 {
		return orchestrator.VerifyCandidate{}, false
	}

	if sameCommand(cmd, in.CurrentCommand) {
		return orchestrator.VerifyCandidate{Argv: verifyexec.ShellArgv(cmd), Display: cmd}, true
	}

	if _, ok := orchestrator.AcceptProposedCommand(cmd); !ok {
		return orchestrator.VerifyCandidate{}, false
	}

	for _, c := range cands {
		if sameCommand(cmd, c.Display) {
			return c, true
		}
	}

	return orchestrator.VerifyCandidate{}, false
}

// sameCommand reports whether a and b have equal whitespace-separated fields.
func sameCommand(a, b string) bool {
	return slices.Equal(strings.Fields(a), strings.Fields(b))
}
