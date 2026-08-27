package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// VerifyTool hands the coder the run's already-resolved verify command as a
// single tool call. The alternative is what the coder does without it:
// rediscover the project's checks by guessing shell commands from whatever
// ecosystem it recognises, one command per turn, and climb the same ladder
// again after everything has already passed.
//
// It knows nothing about what a check command looks like. The argv, timeout and
// environment come from the resolved plan, which came from what the repository
// declares about itself. Recognising particular commands here would be a
// language-specific dependency this package must not take.
//
// The tool's cache is private to the tool and is never consulted by a gate.
// What the tool does hand out is the verdict of its last actual run paired with
// the fingerprint that run was measured against - see verifyToolPass. A gate
// answers "is the tree about to be committed green", so it measures the tree
// itself; that a pass it holds was earned by the tool rather than by its own
// subprocess costs it nothing, because the fingerprints prove the two ran
// against the same tree.
//
// Known limitation, recorded rather than papered over: the resolved verify
// command is a SINGLE command, while a project's real check set usually also
// covers formatting, linting and building. Widening it is a change to the
// DECLARATION - an ordered list in the operator-declared config, or a proposal
// prompt that asks for the repository's whole check set - never a change that
// teaches this package what a check is. It is not done here, so the coder's
// prompt and this tool's schema scope their bash prohibition to the one command
// the tool runs and leave the rungs it cannot reach to the coder.
type VerifyTool struct {
	plan      verifyPlan
	workspace string
	// probe reports whether anything has been written to the workspace since the
	// previous call, and the fingerprint it measured. Execute calls it on entry
	// and again once a run has finished, so a call measures from the end of the
	// previous run rather than from its start: a command that touches a
	// non-ignored file does not report itself as a write and cost the next call
	// a re-run.
	probe worktreeProbe
	// exec runs the command and classifies the outcome. It is the same executor
	// the run's verify gate uses, so the coder gets the retry, the cancellation
	// guard and the container-runtime verdict the gate gets instead of a raw
	// classification of whatever the subprocess did.
	exec verifyExecFunc
	// record publishes the verdict of every completed run, paired with the
	// fingerprint of the tree it left behind, to whoever bound the tool. Nil
	// when nobody is listening.
	record func(verifyToolPass)

	ran        bool
	lastPassed bool
	lastResult string
}

// verifyToolPass is what the coder's verify tool learned on its last actual run:
// whether the resolved command passed, and the worktree fingerprint that verdict
// was measured against. Empty fingerprint means the read failed, which is
// evidence of nothing.
//
// A pair, published on every run, rather than a cache the gate shares with the
// tool: a gate that read the tool's already-passed shortcut would be trusting
// the tool's own bookkeeping about what has been written since. Reading the pair
// leaves the gate measuring the tree itself and comparing - so an unreadable
// fingerprint on either side, a write in between, or a run that did not pass all
// land in the same place, the gate running the command.
type verifyToolPass struct {
	passed      bool
	fingerprint string
}

// verifyExecFunc runs a resolved verify plan in dir and returns the classified
// result, or an error when the run was cancelled or the command needs a
// toolchain the worker does not have. (*run).runVerifyCommand implements it.
type verifyExecFunc func(ctx context.Context, dir string, plan verifyPlan) (verifyResult, error)

// NewVerifyTool builds the coder's verify tool for one workspace. It returns nil
// when no command resolved, so a run with nothing to verify never offers a tool
// that could only report its own absence. record may be nil.
func NewVerifyTool(plan verifyPlan, workspace string, probe worktreeProbe, exec verifyExecFunc, record func(verifyToolPass)) *VerifyTool {
	if len(plan.Argv) == 0 {
		return nil
	}

	return &VerifyTool{plan: plan, workspace: workspace, probe: probe, exec: exec, record: record}
}

func (t *VerifyTool) Name() string { return "verify" }

func (t *VerifyTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: "verify",
		Description: fmt.Sprintf(
			"Run the project's checks: `%s` (%s), executed in the workspace, with its combined output returned. "+
				"Do not run `%s` yourself with bash - this tool is the one that counts; checks that command "+
				"does not cover are still yours to run. "+
				"Call it when your changes are complete, and call it again only after you have written "+
				"something since the last call.",
			t.plan.Display, t.plan.Source, t.plan.Display),
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}}
}

func (t *VerifyTool) Execute(ctx context.Context, _ map[string]any) (tools.Result, error) {
	written, _ := t.probe()

	if t.ran && t.lastPassed && !written {
		return tools.Result{Text: fmt.Sprintf(
			"`%s` already passed and nothing has been written since, so it was not re-run. "+
				"Write something before calling verify again.\n\n%s",
			t.plan.Display, t.lastResult,
		)}, nil
	}

	res, err := t.exec(ctx, t.workspace, t.plan)

	if cerr := ctx.Err(); cerr != nil {
		// An aborted run is not an outcome. The killed command classifies as a
		// failure, and the coder would read the abort as a defect in its own code.
		return tools.Result{}, cerr
	}

	if err != nil {
		var missing *ToolchainMissingError
		if !errors.As(err, &missing) {
			return tools.Result{}, err
		}

		// The gate parks on a missing toolchain; a tool call cannot park, and the
		// condition is not one the coder can do anything about. Report it as
		// inconclusive with the reason rather than as a failing check the coder
		// will spend a fix round on.
		res = verifyResult{Status: verifySkipped, Note: missing.Reason}
	}

	t.ran = true
	t.lastPassed = res.Status == verifyPassed
	t.lastResult = renderVerifyToolResult(t.plan, res)

	// Re-read the fingerprint so the recorded baseline is the tree the run left
	// behind. Nothing else can write while the command runs: the harness
	// dispatches tool calls sequentially and this tool never reaches a subagent
	// registry, so anything the fingerprint moved by is the command's own doing.
	_, fingerprint := t.probe()

	// Publish the verdict with the fingerprint it was measured against - every
	// run, pass or not, so a later failure retracts an earlier pass rather than
	// leaving it standing.
	if t.record != nil {
		t.record(verifyToolPass{passed: t.lastPassed, fingerprint: fingerprint})
	}

	return tools.Result{Text: t.lastResult}, nil
}

// renderVerifyToolResult states the outcome in the first line and carries the
// command's own output below it, so a failure reaches the model with the detail
// it needs to fix it and a pass is unambiguous. An inconclusive run (a timeout,
// a missing tool) is neither: it says so and names the reason, so the coder does
// not read a run that never happened as a green one.
func renderVerifyToolResult(plan verifyPlan, res verifyResult) string {
	head := fmt.Sprintf("`%s` %s", plan.Display, verifyStatusWord(res.Status))

	if res.Note != "" {
		head += " (" + res.Note + ")"
	}

	if res.Output == "" {
		return head + "\n\n(no output)"
	}

	return head + "\n\n" + res.Output
}

// worktreeStateTimeout bounds one fingerprint read end to end - the git calls
// and the untracked content both. The closure is on the coder's critical path,
// so a wedged read must degrade to "assume written" rather than hang the turn.
const worktreeStateTimeout = 30 * time.Second

// worktreeProbe reports whether anything was written to the workspace since the
// previous call, and the fingerprint it measured - empty when the read failed,
// which is why an empty fingerprint can never match another.
type worktreeProbe func() (written bool, fingerprint string)

// worktreeDirty builds the VerifyTool's probe: it fingerprints the workspace
// through git and reports whether the fingerprint moved since the previous call,
// alongside the fingerprint itself. The tool itself stays ignorant of both git
// and the filesystem.
//
// Git rather than a filesystem walk, because only git can tell "the coder wrote
// something" from "the toolchain left an artifact": the fingerprint is built
// from the repository's own ignore rules, so an artifact tree the project
// ignores does not move it. A plain walk would count every artifact any command
// dropped between two calls as a write, and the already-passed report would
// rarely fire.
//
// Unknown state is written, never clean: the first call has no baseline, and a
// fingerprint that could not be read is not evidence that nothing changed.
//
// A project that does not ignore its build artifacts keeps them in the
// fingerprint. The baseline the tool records after each run folds in whatever
// that run wrote, so artifact churn caused by the checks themselves does not
// cost a re-run; anything written between two calls does count as a write, and
// the tool simply runs the command again. An artifact tree big enough to run
// past the fingerprint's own read budget lands in the same place by the error
// path: unreadable state is written, so every call re-runs the command. Both
// degrade to the behavior before this tool existed rather than to a wrong
// answer, which is the right way round.
func worktreeDirty(g GitOps) worktreeProbe {
	var (
		baseline string
		have     bool
	)

	return func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), worktreeStateTimeout)
		defer cancel()

		state, err := g.WorktreeState(ctx)
		if err != nil {
			have = false

			return true, ""
		}

		moved := !have || state != baseline
		baseline, have = state, true

		return moved, state
	}
}

// verifyToolFor builds the coder's verify tool for one solver's workspace, or a
// genuine nil interface when no command resolved - never a non-nil interface
// holding a nil pointer, which a registry would happily offer to the model.
// record is where the tool publishes each run's verdict, or nil when nobody
// reads it.
func (o *run) verifyToolFor(g GitOps, dir string, plan verifyPlan, record func(verifyToolPass)) tools.Tool {
	vt := NewVerifyTool(plan, dir, worktreeDirty(g), o.runVerifyCommand, record)
	if vt == nil {
		return nil
	}

	return vt
}

var _ tools.Tool = (*VerifyTool)(nil)
