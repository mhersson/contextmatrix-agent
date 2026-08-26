package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingPlan is a verify plan whose command appends one line to counter every
// time it runs, so a test can prove how many executions a sequence of tool calls
// actually caused.
func countingPlan(counter string) verifyPlan {
	return verifyPlan{
		Argv:    []string{"sh", "-c", "echo ran >> " + counter},
		Display: "run the checks",
		Source:  verifySourceDeclared,
		Timeout: time.Minute,
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}

		require.NoError(t, err)
	}

	return len(strings.Fields(string(b)))
}

// directVerifyExec is the run's shared verify executor over the real subprocess
// runner, for tests that drive the tool end to end against a real command.
func directVerifyExec() verifyExecFunc {
	return verifyToolRun(verifyexec.Exec).runVerifyCommand
}

func TestVerifyToolRunsResolvedPlan(t *testing.T) {
	t.Parallel()

	vt := NewVerifyTool(verifyPlan{
		Argv:    []string{"sh", "-c", "echo checks-output-marker"},
		Display: "run the checks",
		Source:  verifySourceDetected,
		Timeout: time.Minute,
	}, t.TempDir(), func() bool { return true }, directVerifyExec())
	require.NotNil(t, vt)

	res, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, res.Text, "passed", "a zero exit must be reported as a pass")
	assert.Contains(t, res.Text, "checks-output-marker", "the command's own output must reach the model")
}

func TestVerifyToolCombinesFailureOutput(t *testing.T) {
	t.Parallel()

	vt := NewVerifyTool(verifyPlan{
		Argv:    []string{"sh", "-c", "echo checks-failure-marker; exit 3"},
		Display: "run the checks",
		Source:  verifySourceDetected,
		Timeout: time.Minute,
	}, t.TempDir(), func() bool { return true }, directVerifyExec())
	require.NotNil(t, vt)

	res, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, res.Text, "failed", "a non-zero exit must be reported as a failure")
	assert.Contains(t, res.Text, "checks-failure-marker", "the failing command's output must reach the model")
}

func TestVerifyToolReportsAlreadyPassedWithoutRerunning(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	counter := filepath.Join(ws, "runs.txt")

	vt := NewVerifyTool(countingPlan(counter), ws, func() bool { return false }, directVerifyExec())
	require.NotNil(t, vt)

	_, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)

	res, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 1, countLines(t, counter), "a passing command must not re-run while nothing has been written")
	assert.Contains(t, res.Text, "already passed")
	assert.Contains(t, res.Text, "nothing has been written since")
}

func TestVerifyToolReRunsAfterAWrite(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	counter := filepath.Join(ws, "runs.txt")

	vt := NewVerifyTool(countingPlan(counter), ws, func() bool { return true }, directVerifyExec())
	require.NotNil(t, vt)

	_, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)

	_, err = vt.Execute(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 2, countLines(t, counter), "a write since the last call must re-run the checks")
}

func TestNewVerifyToolNilWithNoCommand(t *testing.T) {
	t.Parallel()

	assert.Nil(t, NewVerifyTool(verifyPlan{}, t.TempDir(), func() bool { return true }, directVerifyExec()),
		"a run with no resolvable verify command must not offer the tool")
}

// recordingWriteTools is a WriteToolsForDir factory that records the dir and the
// verify tool each call was handed, and builds a registry that carries the tool.
func recordingWriteTools(dirs *[]string, verifies *[]tools.Tool) func(string, tools.Tool) *tools.Registry {
	return func(dir string, verify tools.Tool) *tools.Registry {
		*dirs = append(*dirs, dir)
		*verifies = append(*verifies, verify)

		if verify == nil {
			return testWriteTools()
		}

		return tools.NewRegistry(append(testWriteTools().All(), verify)...)
	}
}

// The solo solver's coder registry carries the verify tool once the plan has
// resolved. Without the binding the coder sees only the prompt sentence and goes
// back to guessing shell commands.
func TestExecuteBindsVerifyToolForTheSoloSolver(t *testing.T) {
	var (
		dirs     []string
		verifies []tools.Tool
	)

	d := execTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
	d.Cfg.Workspace = "/ws"
	d.WriteToolsForDir = recordingWriteTools(&dirs, &verifies)

	o := newExecRun(d, nil, 0)
	seedResolvedVerifyPlan(o)

	require.NoError(t, runExecute(context.Background(), o))

	require.Equal(t, []string{"/ws"}, dirs, "the solver's registry must be built for its own workspace")
	require.NotNil(t, verifies[0])
	assert.Equal(t, "verify", verifies[0].Name())

	_, ok := o.solver.tools.Get("verify")
	assert.True(t, ok, "the coder must be able to call the verify tool")
}

// A run with no resolvable command leaves the solver's registry untouched, so
// no-gate runs are unchanged.
func TestExecuteBindsNoVerifyToolWithoutACommand(t *testing.T) {
	var (
		dirs     []string
		verifies []tools.Tool
	)

	d := execTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
	d.Cfg.Workspace = "/ws"
	d.WriteToolsForDir = recordingWriteTools(&dirs, &verifies)

	o := newExecRun(d, nil, 0)

	require.NoError(t, runExecute(context.Background(), o))

	assert.Empty(t, dirs, "no command resolved: the registry must not be rebuilt")

	_, ok := o.solver.tools.Get("verify")
	assert.False(t, ok, "a run with nothing to verify must offer no verify tool")
}

// Every Best-of-N candidate gets its own verify tool, rooted at its own
// worktree: the candidates are coders too, and one registered path is not both.
func TestCandidateSolverBindsVerifyTool(t *testing.T) {
	var (
		dirs     []string
		verifies []tools.Tool
	)

	d := execTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
	d.Cfg.Workspace = "/ws"
	d.WriteToolsForDir = recordingWriteTools(&dirs, &verifies)

	o := newExecRun(d, nil, 0)
	seedResolvedVerifyPlan(o)

	c := &candidate{idx: 1, model: "alpha/coder", dir: "/ws/.worktrees/c1", git: &fakeGit{}, ledger: NewLedger(0, 0)}
	require.NoError(t, o.runCandidate(context.Background(), c, nil, 1))

	require.Equal(t, []string{"/ws/.worktrees/c1"}, dirs, "a candidate's registry must be rooted at its own worktree")
	require.NotNil(t, verifies[0])
	assert.Equal(t, "verify", verifies[0].Name())
}

// verifyToolRun builds a bare run whose raw verify runner is stubbed, so a test
// drives the tool through the same shared executor production uses.
func verifyToolRun(exec verifyRunner) *run {
	o := &run{d: Deps{Cfg: Config{CardID: "CARD-1"}}}
	o.runVerify = exec

	return o
}

func toolPlan() verifyPlan {
	return verifyPlan{
		Argv:    []string{"x"},
		Display: "run the checks",
		Source:  verifySourceDeclared,
		Timeout: time.Minute,
	}
}

// An unreachable container runtime is not a defect the coder can fix. The gate
// parks on it rather than calling it a failure; a tool call cannot park, so it
// must reach the coder as inconclusive with the reason - never as a failing
// check under a prompt that tells it to make the checks pass.
func TestVerifyToolReportsContainerRuntimeAsInconclusive(t *testing.T) {
	t.Parallel()

	o := verifyToolRun(func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		return verifyexec.Outcome{
			ExitCode: 1,
			Output:   "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
		}
	})

	vt := NewVerifyTool(toolPlan(), t.TempDir(), func() bool { return true }, o.runVerifyCommand)
	require.NotNil(t, vt)

	res, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)
	assert.NotContains(t, res.Text, "failed", "an unreachable container runtime must not read as a failing check")
	assert.Contains(t, res.Text, "container runtime", "the coder must be told why the run is inconclusive")
	assert.False(t, vt.lastPassed, "an inconclusive run must not be cached as a pass")
}

// Container pressure gets the same single retry the gate takes: without it the
// coder is handed a spawn failure that would have passed on the retry.
func TestVerifyToolRetriesResourceExhaustion(t *testing.T) {
	withFastVerifyRetryWait(t)

	calls := 0
	o := verifyToolRun(func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		calls++

		if calls == 1 {
			return verifyexec.Outcome{ExitCode: 2, Output: "fork/exec /usr/bin/x: resource temporarily unavailable"}
		}

		return verifyexec.Outcome{ExitCode: 0, Output: "everything ok"}
	})

	vt := NewVerifyTool(toolPlan(), t.TempDir(), func() bool { return true }, o.runVerifyCommand)
	require.NotNil(t, vt)

	res, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "an exhausted first run retries once")
	assert.Contains(t, res.Text, "passed", "the retry's pass is what the coder must see")
}

// A cancelled run must not reach the coder as a verify outcome: the killed
// command classifies as a failure, and the coder would read the abort as a
// defect in its own code. The stub executor returns a result despite the
// cancellation, so this pins the guard in Execute.
func TestVerifyToolReportsCancellationAsAnError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	exec := func(context.Context, string, verifyPlan) (verifyResult, error) {
		return verifyResult{Status: verifyFailed, Output: "signal: killed"}, nil
	}

	vt := NewVerifyTool(toolPlan(), t.TempDir(), func() bool { return true }, exec)
	require.NotNil(t, vt)

	res, err := vt.Execute(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, res.Text, "a cancelled run reports no outcome")
	assert.False(t, vt.ran, "a cancelled run must not be cached as a run")
}

// workspaceDirty is a dirty closure over the workspace's own file contents,
// standing in for the git fingerprint: it reports whether anything directly
// under ws changed since the previous call, so a test can prove WHEN the
// baseline is taken rather than only that one is.
func workspaceDirty(t *testing.T, ws string) func() bool {
	t.Helper()

	var (
		baseline string
		have     bool
	)

	return func() bool {
		entries, err := os.ReadDir(ws)
		require.NoError(t, err)

		var sb strings.Builder

		for _, e := range entries {
			if e.IsDir() {
				continue
			}

			b, err := os.ReadFile(filepath.Join(ws, e.Name()))
			require.NoError(t, err)

			fmt.Fprintf(&sb, "%s:%s\n", e.Name(), b)
		}

		state := sb.String()
		moved := !have || state != baseline
		baseline, have = state, true

		return moved
	}
}

// The baseline is the tree the run left behind, not the tree it found: a check
// command that writes into the workspace (a lockfile, an unignored artifact)
// must not report itself as a write and cost the next call a re-run of a check
// that just passed.
func TestVerifyToolBaselineIsTakenAfterTheRun(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	counter := filepath.Join(ws, "runs.txt")

	vt := NewVerifyTool(countingPlan(counter), ws, workspaceDirty(t, ws), directVerifyExec())
	require.NotNil(t, vt)

	_, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)

	res, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 1, countLines(t, counter), "a command's own writes must not make the next call re-run it")
	assert.Contains(t, res.Text, "already passed")
}

// The schema's prohibition is scoped the same way the prompt's is, and for the
// same reason: this tool runs one command, so the coder must stay free to run
// the checks it does not reach.
func TestVerifyToolSchemaScopesTheBashProhibition(t *testing.T) {
	t.Parallel()

	vt := NewVerifyTool(verifyPlan{
		Argv:    []string{"make", "check"},
		Display: "make check",
		Source:  verifySourceDeclared,
		Timeout: time.Minute,
	}, t.TempDir(), func() bool { return true }, directVerifyExec())
	require.NotNil(t, vt)

	assert.Contains(t, prohibitionSentence(t, vt.Schema().Function.Description), "make check",
		"the prohibition must be scoped to the command the tool runs")
}
