package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTools writes executable no-op scripts for each named tool into a fresh bin
// dir and points PATH at it, so detection probes resolve exactly those tools.
func stubTools(t *testing.T, names ...string) {
	t.Helper()

	bin := t.TempDir()

	for _, n := range names {
		require.NoError(t, os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	}

	t.Setenv("PATH", bin)
}

func TestClassifyVerify(t *testing.T) {
	plan := verifyPlan{Timeout: 5 * time.Minute}

	tests := []struct {
		name    string
		out     verifyexec.Outcome
		status  verifyStatus
		hasNote bool
	}{
		{"exit-zero-pass", verifyexec.Outcome{ExitCode: 0}, verifyPassed, false},
		{"exit-one-fail", verifyexec.Outcome{ExitCode: 1, Output: "1 test failed"}, verifyFailed, false},
		{"exit-two-fail", verifyexec.Outcome{ExitCode: 2, Output: "build error"}, verifyFailed, false},
		{"exit-127-skip", verifyexec.Outcome{ExitCode: 127}, verifySkipped, true},
		{"start-err-skip", verifyexec.Outcome{StartErr: true, ExitCode: -1}, verifySkipped, true},
		{"timeout-skip", verifyexec.Outcome{TimedOut: true, ExitCode: -1}, verifySkipped, true},
		// A non-wrapper command whose output prints a not-found line is a REAL
		// failure — the tool-missing heuristic is not consulted for it.
		{"non-wrapper-not-found-stays-fail", verifyexec.Outcome{ExitCode: 2, Output: "make: cargo: command not found"}, verifyFailed, false},
		{"printed-not-found-stays-fail", verifyexec.Outcome{ExitCode: 1, Output: "FAIL: asserted 'command not found'"}, verifyFailed, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := classifyVerify(plan, tt.out)
			assert.Equal(t, tt.status, res.Status)
			assert.Equal(t, tt.hasNote, res.Note != "", "note=%q", res.Note)
		})
	}
}

// TestClassifyVerifyWrapperScopedToolMissing pins that the tool-missing heuristic
// is consulted ONLY for a detected wrapper: a plain-argv command that fails while
// printing an anchored not-found line (its suite shells to a missing helper)
// stays FAILED, so a real failure is never downgraded to skipped; the same output
// under a make/just/task wrapper — which masks the inner 127 as a plain exit — is
// SKIPPED.
func TestClassifyVerifyWrapperScopedToolMissing(t *testing.T) {
	out := verifyexec.Outcome{ExitCode: 1, Output: "/bin/sh: 1: helper: not found\n--- FAIL: TestX"}

	plain := classifyVerify(verifyPlan{}, out)
	assert.Equal(t, verifyFailed, plain.Status, "a non-wrapper failure that prints a not-found line stays FAILED")

	wrapped := classifyVerify(verifyPlan{Wrapper: true}, out)
	assert.Equal(t, verifySkipped, wrapped.Status, "a wrapper masks the inner missing tool -> SKIPPED")
}

func TestClassifyVerifyTimeoutNote(t *testing.T) {
	res := classifyVerify(verifyPlan{Timeout: 90 * time.Second}, verifyexec.Outcome{TimedOut: true})
	assert.Equal(t, verifySkipped, res.Status)
	assert.Contains(t, res.Note, "timed out after 1m30s")
}

func TestDetectVerifyCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probing is POSIX-only")
	}

	t.Run("go.mod with go on path", func(t *testing.T) {
		stubTools(t, "go")

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module example.com/x\n")

		argv, display, wrapper := detectVerifyCommand(dir)
		assert.Equal(t, []string{"go", "test", "./..."}, argv)
		assert.Equal(t, "go test ./...", display)
		assert.False(t, wrapper, "a marker-table command is not a wrapper")
	})

	t.Run("cargo project", func(t *testing.T) {
		stubTools(t, "cargo")

		dir := t.TempDir()
		writeFile(t, dir, "Cargo.toml", "[package]\nname=\"x\"\n")

		argv, _, _ := detectVerifyCommand(dir)
		assert.Equal(t, []string{"cargo", "test"}, argv)
	})

	t.Run("Go repo with Makefile+go uses make", func(t *testing.T) {
		stubTools(t, "make", "go")

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "build:\n\tgo build ./...\ntest:\n\tgo test ./...\n")
		writeFile(t, dir, "go.mod", "module example.com/x\n")

		argv, _, wrapper := detectVerifyCommand(dir)
		assert.Equal(t, []string{"make", "test"}, argv)
		assert.True(t, wrapper, "a make wrapper is flagged for the tool-missing heuristic")
	})

	t.Run("pure-make repo uses make", func(t *testing.T) {
		stubTools(t, "make")

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "test:\n\t./run-tests.sh\n")

		argv, _, wrapper := detectVerifyCommand(dir)
		assert.Equal(t, []string{"make", "test"}, argv)
		assert.True(t, wrapper)
	})

	t.Run("Rust Makefile without cargo skips the wrapper", func(t *testing.T) {
		// make resolves but cargo does not: the make wrapper would shell out to
		// cargo and false-fail, so detection skips it AND the cargo row, returning
		// nothing (the model-proposal tier would take over in a real run).
		stubTools(t, "make") // no cargo

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "test:\n\tcargo test\n")
		writeFile(t, dir, "Cargo.toml", "[package]\nname=\"x\"\n")

		argv, _, _ := detectVerifyCommand(dir)
		assert.Nil(t, argv, "make must be skipped when the declared toolchain is absent")
	})

	t.Run("npm placeholder not detected", func(t *testing.T) {
		stubTools(t, "npm")

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`)

		argv, _, _ := detectVerifyCommand(dir)
		assert.Nil(t, argv, "the npm-init placeholder test script is not a real command")
	})

	t.Run("real npm test detected", func(t *testing.T) {
		stubTools(t, "npm")

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"vitest run"}}`)

		argv, _, _ := detectVerifyCommand(dir)
		assert.Equal(t, []string{"npm", "test"}, argv)
	})

	t.Run("gradlew without exec bit not chosen", func(t *testing.T) {
		stubTools(t, "java") // no gradle on path, gradlew present but not executable

		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "gradlew"), []byte("#!/bin/sh\n"), 0o644))

		argv, _, _ := detectVerifyCommand(dir)
		assert.Nil(t, argv, "a non-executable gradlew and no system gradle resolves nothing")
	})

	t.Run("no markers resolves nothing", func(t *testing.T) {
		stubTools(t, "go")

		dir := t.TempDir()

		argv, display, _ := detectVerifyCommand(dir)
		assert.Nil(t, argv)
		assert.Empty(t, display)
	})
}

func TestJustfileTestRecipeRegex(t *testing.T) {
	recipes := []string{"test:", "test arg:", "test a b:", "test foo: dep"}
	notRecipes := []string{`test := "just test"`, `test  :=  "x"`, "testfile:", "test-helper:", "# a test: comment"}

	for _, l := range recipes {
		assert.True(t, justfileTestRe.MatchString(l), "recipe line should match: %q", l)
	}

	for _, l := range notRecipes {
		assert.False(t, justfileTestRe.MatchString(l), "non-recipe line must not match: %q", l)
	}
}

func TestHasPytestMarker(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
		want    bool
	}{
		{"bare pyproject is not pytest", "pyproject.toml", "[tool.poetry]\nname = \"x\"\n", false},
		{"pyproject with pytest table", "pyproject.toml", "[tool.pytest.ini_options]\naddopts = \"-q\"\n", true},
		{"pytest.ini", "pytest.ini", "[pytest]\n", true},
		{"setup.cfg with tool:pytest", "setup.cfg", "[tool:pytest]\n", true},
		{"setup.cfg without pytest", "setup.cfg", "[metadata]\nname = x\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, tt.file, tt.content)
			assert.Equal(t, tt.want, hasPytestMarker(dir))
		})
	}
}

func TestVerifyTimeoutClamp(t *testing.T) {
	tests := []struct {
		name    string
		declare *DeclaredVerify
		want    time.Duration
	}{
		{"default when nil", nil, defaultVerifyTimeout},
		{"default when zero", &DeclaredVerify{}, defaultVerifyTimeout},
		{"honored in range", &DeclaredVerify{Timeout: 20 * time.Minute}, 20 * time.Minute},
		{"clamped to min", &DeclaredVerify{Timeout: time.Second}, minVerifyTimeout},
		{"clamped to max", &DeclaredVerify{Timeout: 5 * time.Hour}, maxVerifyTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &run{d: Deps{Cfg: Config{Verify: tt.declare}}}
			assert.Equal(t, tt.want, o.verifyTimeout())
		})
	}
}

func TestVerifyEnvFiltersAndResolves(t *testing.T) {
	t.Setenv("JAVA_HOME", "/opt/jdk")
	t.Setenv("GITHUB_TOKEN", "secret") // denied by prefix

	o := &run{d: Deps{Cfg: Config{Verify: &DeclaredVerify{Env: []string{"JAVA_HOME", "GITHUB_TOKEN", "MISSING_VAR"}}}}}

	got := o.verifyEnv()
	assert.Equal(t, []string{"JAVA_HOME=/opt/jdk"}, got, "denied names dropped, unset names skipped")
}

func TestResolveVerifyDeclaredRunnable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "cargo", "bash")

	o := &run{d: Deps{Cfg: Config{
		Workspace: t.TempDir(),
		Verify:    &DeclaredVerify{Command: "cargo test"},
	}}}

	plan, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDeclared, plan.Source)
	assert.Equal(t, "cargo test", plan.Display)
	assert.Equal(t, []string{"bash", "-c", "set -o pipefail; cargo test"}, plan.Argv)
}

func TestResolveVerifyDeclaredUnrunnableFallsThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	// The declared command names a missing tool, but a detectable go.mod exists:
	// resolution must fall through to detection rather than disable the gate.
	stubTools(t, "go") // no "pytest"

	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/x\n")

	o := &run{d: Deps{Cfg: Config{
		Workspace: dir,
		Verify:    &DeclaredVerify{Command: "pytest -q"},
	}}}

	plan, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDetected, plan.Source)
	assert.Equal(t, []string{"go", "test", "./..."}, plan.Argv)
}

func TestResolveVerifyDeclaredNoteThreadedIntoResolvedPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	// A declared command that cannot run, but a detectable go.mod exists: the
	// declared-cannot-run note must survive onto the resolved plan and reach the
	// resolution log, rather than being silently dropped when a lower tier wins.
	stubTools(t, "go") // no pytest

	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/x\n")

	ops := &fakeOps{}
	o := &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1", Workspace: dir, Verify: &DeclaredVerify{Command: "pytest -q"}}}}

	p, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDetected, p.Source)
	require.NotEmpty(t, p.Notes, "the declared-cannot-run note is carried onto the resolved plan")
	assert.Contains(t, p.Notes[0], "declared verify command cannot run: pytest -q")

	o.logVerifyResolution(context.Background(), p)
	assert.True(t, ops.loggedContains("declared verify command cannot run: pytest -q"),
		"the resolution log surfaces the dropped-declared note; logs=%v", ops.logs)
	assert.True(t, ops.loggedContains("verify command resolved: go test ./... (detected)"),
		"the resolution line still names the resolved command; logs=%v", ops.logs)
}

func TestResolveVerifySkipWhenNothingResolves(t *testing.T) {
	// No registry -> the proposal tier is skipped, resolution falls to skip.
	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}

	plan, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceNone, plan.Source)
	assert.Empty(t, plan.Argv)
}

func TestEnsureVerifyCachesAndLogs(t *testing.T) {
	ops := &fakeOps{}
	o := &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1", Workspace: t.TempDir()}}}

	plan, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceNone, plan.Source)
	require.NotNil(t, o.verify)
	assert.True(t, ops.loggedContains("work will proceed UNVERIFIED"), "the loud skip line fires once; logs=%v", ops.logs)

	// A skip re-resolves on re-entry but, still finding nothing, does not re-log.
	logsBefore := len(ops.logs)

	_, err = o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Len(t, ops.logs, logsBefore, "a re-confirmed skip does not re-log the UNVERIFIED line")
}

// TestEnsureVerifyReresolvesSkip pins finding 4: a skip resolved at execute entry
// (pre-code) is NOT final — a phase that adds the project's tooling makes the
// command detectable, and the later gate must run it rather than ship unverified.
func TestEnsureVerifyReresolvesSkip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "go")

	ops := &fakeOps{}
	dir := t.TempDir()
	// No registry -> the proposal tier is a no-op, isolating the detection re-resolve.
	o := &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1", Workspace: dir}}}

	// Execute-entry resolution: no markers yet -> skip.
	p1, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceNone, p1.Source)

	// A bootstrap phase adds the project's tooling.
	writeFile(t, dir, "go.mod", "module example.com/x\n")

	// Review-entry resolution re-resolves the skip and now detects the command.
	p2, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDetected, p2.Source, "a prior skip must re-resolve once tooling exists")
	assert.Equal(t, []string{"go", "test", "./..."}, p2.Argv)
	assert.True(t, ops.loggedContains("verify command resolved"), "the upgrade from skip to a real command is logged")
}

func TestRunVerifyPlanRedactsAndSkipsEmpty(t *testing.T) {
	o := &run{d: Deps{
		Redact: func(s string) string { return "[REDACTED]" },
		Cfg:    Config{Workspace: t.TempDir()},
	}}
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 1, Output: "token=hunter2"}
	}

	// Empty plan -> skipped, runner never called.
	res, err := o.runVerifyPlan(context.Background(), "dir", verifyPlan{})
	require.NoError(t, err)
	assert.Equal(t, verifySkipped, res.Status)

	// Non-empty plan -> runner called, output redacted at capture.
	res, err = o.runVerifyPlan(context.Background(), "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, verifyFailed, res.Status)
	assert.Equal(t, "[REDACTED]", res.Output)
}

func TestRunVerifyPlanPropagatesParentCancel(t *testing.T) {
	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: -1}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := o.runVerifyPlan(ctx, "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.Error(t, err, "a cancelled parent context propagates the abort, not a verify outcome")
}
