package runkind

import (
	"context"
	"fmt"
	"log/slog"
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

// checkDeps are the check's effects. trial returns its output masked; mask
// masks anything else the check writes; progressEvery paces the trial's
// progress events.
type checkDeps struct {
	candidates    func(ws string) []orchestrator.VerifyCandidate
	trial         func(ctx context.Context, ws string, c orchestrator.VerifyCandidate, timeout time.Duration, env []string) (trial, output string, err error)
	mask          func(string) string
	absent        func(ctx context.Context, ws string, tokens, declared []string) ([]string, error)
	progressEvery time.Duration
}

// newCheckDeps wires the check to the gate's detection and trial, logging to
// log and masking with mask.
func newCheckDeps(log *slog.Logger, mask func(string) string) checkDeps {
	return checkDeps{
		candidates: orchestrator.VerifyCandidates,
		trial: func(ctx context.Context, ws string, c orchestrator.VerifyCandidate, timeout time.Duration, env []string) (string, string, error) {
			return orchestrator.TrialVerify(ctx, log, ws, c, timeout, env, mask)
		},
		mask:          mask,
		absent:        absentFromRepo,
		progressEvery: time.Minute,
	}
}

// check lists the base branch's verify candidates and, when in has a Verify,
// trial-runs it once if trialCandidate accepts it. A Text adds the tokens it
// mentions that the base branch lacks.
func check(ctx context.Context, ws string, in protocol.CheckInput, d checkDeps, emit *events.Emitter) (protocol.CheckResult, error) {
	cands := d.candidates(ws)

	res := protocol.CheckResult{Candidates: make([]string, 0, len(cands))}
	for _, c := range cands {
		res.Candidates = append(res.Candidates, c.Display)
	}

	if in.Text != "" {
		declared := append([]string{in.CurrentCommand}, res.Candidates...)

		absent, err := d.absent(ctx, ws, textTokens(in.Text), declared)
		if err != nil {
			return protocol.CheckResult{}, fmt.Errorf("text check: %w", err)
		}

		res.AbsentFromRepo = absent
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
	stop := orchestrator.StartTicker(ctx, d.progressEvery, func(context.Context) {
		emit.Emit(progressKind, map[string]any{"step": "trial", "elapsed_seconds": int(time.Since(start).Seconds())})
	})

	trial, output, err := d.trial(ctx, ws, c,
		orchestrator.ClampVerifyTimeout(v.TimeoutSeconds),
		orchestrator.ResolveVerifyEnv(&orchestrator.DeclaredVerify{Env: v.Env}))

	stop()

	if err != nil {
		return "", fmt.Errorf("trial verify: %w", err)
	}

	emit.Emit(trialKind, map[string]any{"command": d.mask(c.Display), "trial": trial, "output": output})

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

// sameCommand reports whether a and b have the same non-empty lines, each with
// equal whitespace-separated fields. A newline is significant: bash runs each
// line as its own command.
func sameCommand(a, b string) bool {
	return slices.EqualFunc(commandLines(a), commandLines(b), slices.Equal[[]string])
}

// commandLines splits cmd into the fields of each non-empty line.
func commandLines(cmd string) [][]string {
	var out [][]string

	for line := range strings.Lines(cmd) {
		if f := strings.Fields(line); len(f) > 0 {
			out = append(out, f)
		}
	}

	return out
}
