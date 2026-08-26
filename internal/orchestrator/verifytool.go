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
// The tool's cache is private to the tool and is never consulted by the run's
// verify gates. This tool answers "is my work passing right now"; a gate answers
// "is the tree about to be committed green", and only a gate that measures the
// tree itself can answer that. A subtask whose coder ran the tool and passed
// runs the command a second time at the gate, deliberately.
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
	// dirty reports whether anything has been written to the workspace since the
	// previous call. Execute calls it on entry and again once a run has finished,
	// so a call measures from the end of the previous run rather than from its
	// start: a command that touches a non-ignored file does not report itself as
	// a write and cost the next call a re-run.
	dirty func() bool
	// exec runs the command and classifies the outcome. It is the same executor
	// the run's verify gate uses, so the coder gets the retry, the cancellation
	// guard and the container-runtime verdict the gate gets instead of a raw
	// classification of whatever the subprocess did.
	exec verifyExecFunc

	ran        bool
	lastPassed bool
	lastResult string
}

// verifyExecFunc runs a resolved verify plan in dir and returns the classified
// result, or an error when the run was cancelled or the command needs a
// toolchain the worker does not have. (*run).runVerifyCommand implements it.
type verifyExecFunc func(ctx context.Context, dir string, plan verifyPlan) (verifyResult, error)

// NewVerifyTool builds the coder's verify tool for one workspace. It returns nil
// when no command resolved, so a run with nothing to verify never offers a tool
// that could only report its own absence.
func NewVerifyTool(plan verifyPlan, workspace string, dirty func() bool, exec verifyExecFunc) *VerifyTool {
	if len(plan.Argv) == 0 {
		return nil
	}

	return &VerifyTool{plan: plan, workspace: workspace, dirty: dirty, exec: exec}
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
	written := t.dirty()

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
	t.dirty()

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

// worktreeStateTimeout bounds one fingerprint read. The closure is on the
// coder's critical path, so a wedged git call must degrade to "assume written"
// rather than hang the turn.
const worktreeStateTimeout = 30 * time.Second

// worktreeDirty builds the VerifyTool's dirty closure: it fingerprints the
// workspace through git and reports whether the fingerprint moved since the
// previous call. The tool itself stays ignorant of both git and the filesystem.
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
// the tool simply runs the command again. That degrades to the behavior before
// this tool existed rather than to a wrong answer, which is the right way round.
func worktreeDirty(g GitOps) func() bool {
	var (
		baseline string
		have     bool
	)

	return func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), worktreeStateTimeout)
		defer cancel()

		state, err := g.WorktreeState(ctx)
		if err != nil {
			have = false

			return true
		}

		moved := !have || state != baseline
		baseline, have = state, true

		return moved
	}
}

// verifyToolFor builds the coder's verify tool for one solver's workspace, or a
// genuine nil interface when no command resolved - never a non-nil interface
// holding a nil pointer, which a registry would happily offer to the model.
func (o *run) verifyToolFor(g GitOps, dir string, plan verifyPlan) tools.Tool {
	vt := NewVerifyTool(plan, dir, worktreeDirty(g), o.runVerifyCommand)
	if vt == nil {
		return nil
	}

	return vt
}

var _ tools.Tool = (*VerifyTool)(nil)
