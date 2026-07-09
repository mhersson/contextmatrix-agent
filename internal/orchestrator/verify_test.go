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
		{"anchored-make-not-found-skip", verifyexec.Outcome{ExitCode: 2, Output: "make: cargo: command not found"}, verifySkipped, true},
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

		argv, display := detectVerifyCommand(dir)
		assert.Equal(t, []string{"go", "test", "./..."}, argv)
		assert.Equal(t, "go test ./...", display)
	})

	t.Run("cargo project", func(t *testing.T) {
		stubTools(t, "cargo")

		dir := t.TempDir()
		writeFile(t, dir, "Cargo.toml", "[package]\nname=\"x\"\n")

		argv, _ := detectVerifyCommand(dir)
		assert.Equal(t, []string{"cargo", "test"}, argv)
	})

	t.Run("Go repo with Makefile+go uses make", func(t *testing.T) {
		stubTools(t, "make", "go")

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "build:\n\tgo build ./...\ntest:\n\tgo test ./...\n")
		writeFile(t, dir, "go.mod", "module example.com/x\n")

		argv, _ := detectVerifyCommand(dir)
		assert.Equal(t, []string{"make", "test"}, argv)
	})

	t.Run("pure-make repo uses make", func(t *testing.T) {
		stubTools(t, "make")

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "test:\n\t./run-tests.sh\n")

		argv, _ := detectVerifyCommand(dir)
		assert.Equal(t, []string{"make", "test"}, argv)
	})

	t.Run("Rust Makefile without cargo skips the wrapper", func(t *testing.T) {
		// make resolves but cargo does not: the make wrapper would shell out to
		// cargo and false-fail, so detection skips it AND the cargo row, returning
		// nothing (the model-proposal tier would take over in a real run).
		stubTools(t, "make") // no cargo

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "test:\n\tcargo test\n")
		writeFile(t, dir, "Cargo.toml", "[package]\nname=\"x\"\n")

		argv, _ := detectVerifyCommand(dir)
		assert.Nil(t, argv, "make must be skipped when the declared toolchain is absent")
	})

	t.Run("npm placeholder not detected", func(t *testing.T) {
		stubTools(t, "npm")

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`)

		argv, _ := detectVerifyCommand(dir)
		assert.Nil(t, argv, "the npm-init placeholder test script is not a real command")
	})

	t.Run("real npm test detected", func(t *testing.T) {
		stubTools(t, "npm")

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"vitest run"}}`)

		argv, _ := detectVerifyCommand(dir)
		assert.Equal(t, []string{"npm", "test"}, argv)
	})

	t.Run("gradlew without exec bit not chosen", func(t *testing.T) {
		stubTools(t, "java") // no gradle on path, gradlew present but not executable

		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "gradlew"), []byte("#!/bin/sh\n"), 0o644))

		argv, _ := detectVerifyCommand(dir)
		assert.Nil(t, argv, "a non-executable gradlew and no system gradle resolves nothing")
	})

	t.Run("no markers resolves nothing", func(t *testing.T) {
		stubTools(t, "go")

		dir := t.TempDir()

		argv, display := detectVerifyCommand(dir)
		assert.Nil(t, argv)
		assert.Empty(t, display)
	})
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

	plan := o.resolveVerify()
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

	plan := o.resolveVerify()
	assert.Equal(t, verifySourceDetected, plan.Source)
	assert.Equal(t, []string{"go", "test", "./..."}, plan.Argv)
}

func TestResolveVerifySkipWhenNothingResolves(t *testing.T) {
	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}

	plan := o.resolveVerify()
	assert.Equal(t, verifySourceNone, plan.Source)
	assert.Empty(t, plan.Argv)
}

func TestEnsureVerifyCachesAndLogs(t *testing.T) {
	ops := &fakeOps{}
	o := &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1", Workspace: t.TempDir()}}}

	plan := o.ensureVerify(context.Background())
	assert.Equal(t, verifySourceNone, plan.Source)
	require.NotNil(t, o.verify)
	assert.True(t, ops.loggedContains("work will proceed UNVERIFIED"), "the loud skip line fires once; logs=%v", ops.logs)

	// A second call reuses the cache and does not log again.
	logsBefore := len(ops.logs)

	o.ensureVerify(context.Background())
	assert.Len(t, ops.logs, logsBefore, "ensureVerify caches: no second resolution log")
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
