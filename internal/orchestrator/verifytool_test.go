package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestVerifyToolRunsResolvedPlan(t *testing.T) {
	t.Parallel()

	vt := NewVerifyTool(verifyPlan{
		Argv:    []string{"sh", "-c", "echo checks-output-marker"},
		Display: "run the checks",
		Source:  verifySourceDetected,
		Timeout: time.Minute,
	}, t.TempDir(), func() bool { return true })
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
	}, t.TempDir(), func() bool { return true })
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

	vt := NewVerifyTool(countingPlan(counter), ws, func() bool { return false })
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

	vt := NewVerifyTool(countingPlan(counter), ws, func() bool { return true })
	require.NotNil(t, vt)

	_, err := vt.Execute(context.Background(), nil)
	require.NoError(t, err)

	_, err = vt.Execute(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 2, countLines(t, counter), "a write since the last call must re-run the checks")
}

func TestNewVerifyToolNilWithNoCommand(t *testing.T) {
	t.Parallel()

	assert.Nil(t, NewVerifyTool(verifyPlan{}, t.TempDir(), func() bool { return true }),
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
