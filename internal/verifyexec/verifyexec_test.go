package verifyexec

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubBin writes an executable no-op script named each of names into a fresh
// bin dir and prepends it to PATH for the test, so exec.LookPath resolves
// exactly the named programs and nothing else the host happens to have.
func stubBin(t *testing.T, names ...string) string {
	t.Helper()

	bin := t.TempDir()

	for _, n := range names {
		p := filepath.Join(bin, n)
		require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	}

	t.Setenv("PATH", bin)

	return bin
}

func TestProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probing is POSIX-only")
	}

	stubBin(t, "cargo", "go")

	ws := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "gradlew"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "gradlew.noexec"), []byte("#!/bin/sh\n"), 0o644))

	tests := []struct {
		name string
		argv []string
		ok   bool
	}{
		{"on-path", []string{"cargo", "test"}, true},
		{"missing", []string{"pytest", "-q"}, false},
		{"empty", nil, false},
		{"wrapper-exec", []string{"./gradlew", "test"}, false}, // no java on PATH
		{"path-no-exec-bit", []string{"./gradlew.noexec"}, false},
		{"path-absent", []string{"./missing.sh"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Probe(ws, tt.argv)
			assert.Equal(t, tt.ok, err == nil, "err=%v", err)
		})
	}
}

func TestProbeJVMWrapperNeedsJava(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probing is POSIX-only")
	}

	ws := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "gradlew"), []byte("#!/bin/sh\n"), 0o755))

	// No java on PATH -> the executable wrapper still cannot run.
	stubBin(t) // empty bin
	require.Error(t, Probe(ws, []string{"./gradlew", "test"}))

	// java present -> wrapper is runnable.
	stubBin(t, "java")
	require.NoError(t, Probe(ws, []string{"./gradlew", "test"}))

	// bare gradle also needs java.
	stubBin(t, "gradle")
	require.Error(t, Probe(ws, []string{"gradle", "test"}))

	stubBin(t, "gradle", "java")
	require.NoError(t, Probe(ws, []string{"gradle", "test"}))
}

func TestProbeShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probing is POSIX-only")
	}

	stubBin(t, "cargo", "go", "tee", "grep")

	ws := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "run.sh"), []byte("#!/bin/sh\n"), 0o755))

	tests := []struct {
		name string
		cmd  string
		ok   bool
	}{
		{"plain", "cargo test", true},
		{"pipeline", "cargo test | tee log", true},
		{"and-list", "go build && cargo test", true},
		{"semicolon", "cargo test ; go vet ./...", true},
		{"env-prefix", "RUST_BACKTRACE=1 cargo test", true},
		{"path-token", "./run.sh", true},
		{"builtin-lead", "cd sub && cargo test", true},
		{"subshell-unparsed-ok", "cargo test $(date +%s)", true},
		{"missing-tool", "pytest -q", false},
		{"missing-in-pipeline", "cargo test | pytest", false},
		{"missing-path", "./missing.sh", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ProbeShell(ws, tt.cmd)
			assert.Equal(t, tt.ok, err == nil, "err=%v", err)
		})
	}
}

func TestShellArgv(t *testing.T) {
	t.Run("bash-with-pipefail", func(t *testing.T) {
		stubBin(t, "bash")

		got := ShellArgv("cargo test | tee log")
		assert.Equal(t, []string{"bash", "-c", "set -o pipefail; cargo test | tee log"}, got)
	})

	t.Run("sh-fallback", func(t *testing.T) {
		stubBin(t) // no bash

		got := ShellArgv("cargo test")
		assert.Equal(t, []string{"sh", "-c", "cargo test"}, got)
	})
}

func TestFilterEnvNames(t *testing.T) {
	in := []string{
		"JAVA_HOME",     // allowed
		"GRADLE_OPTS",   // allowed
		"lowercase",     // denied: not upper-snake
		"1BAD",          // denied: leading digit
		"HAS-DASH",      // denied: dash
		"CM_SECRET_DIR", // denied: CM_ prefix
		"CMX_WORKSPACE", // denied: CMX_ prefix
		"LLM_API_KEY",   // denied: LLM_ prefix
		"GITHUB_TOKEN",  // denied: GITHUB_ prefix
		"MY_TOKEN",      // denied: _TOKEN suffix
		"SIGNING_KEY",   // denied: _KEY suffix
		"DB_SECRET",     // denied: _SECRET suffix
		"ROOT_PASSWORD", // denied: _PASSWORD suffix
		"PYTHONPATH",    // allowed
	}

	got := FilterEnvNames(in)
	assert.Equal(t, []string{"JAVA_HOME", "GRADLE_OPTS", "PYTHONPATH"}, got)
	assert.Nil(t, FilterEnvNames(nil))
}

func TestLooksToolMissing(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"make-command-not-found", "make: cargo: command not found", true},
		{"make-sublevel", "make[1]: mvn: command not found", true},
		{"make-not-found", "make: pytest: not found", true},
		{"sh-not-found", "sh: gradle: not found", true},
		{"bash-command-not-found", "bash: dotnet: command not found", true},
		{"printed-mid-output", "FAIL: test asserted 'command not found' message", false},
		{"printed-with-leading-space", "  bash: x: command not found", false},
		{"ordinary-failure", "--- FAIL: TestFoo\n2 tests failed", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, LooksToolMissing(tt.output))
		})
	}
}

func TestExecExitCodes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	out := Exec(ctx, dir, []string{"sh", "-c", "echo hi; exit 0"}, time.Minute, nil)
	assert.Equal(t, 0, out.ExitCode)
	assert.False(t, out.TimedOut)
	assert.False(t, out.StartErr)
	assert.Contains(t, out.Output, "hi")

	out = Exec(ctx, dir, []string{"sh", "-c", "exit 2"}, time.Minute, nil)
	assert.Equal(t, 2, out.ExitCode)
	assert.False(t, out.TimedOut)

	out = Exec(ctx, dir, []string{"sh", "-c", "exit 127"}, time.Minute, nil)
	assert.Equal(t, 127, out.ExitCode)

	out = Exec(ctx, dir, []string{"definitely-not-a-real-binary-xyz"}, time.Minute, nil)
	assert.True(t, out.StartErr)
	assert.Equal(t, -1, out.ExitCode)

	out = Exec(ctx, dir, nil, time.Minute, nil)
	assert.True(t, out.StartErr)
}

func TestExecTimeoutVsParentCancel(t *testing.T) {
	dir := t.TempDir()

	// The command's OWN timeout fires -> TimedOut.
	out := Exec(context.Background(), dir, []string{"sh", "-c", "sleep 5"}, 100*time.Millisecond, nil)
	assert.True(t, out.TimedOut, "own timeout must set TimedOut")
	assert.Equal(t, -1, out.ExitCode)

	// The PARENT context is cancelled -> NOT TimedOut (the caller detects the
	// abort via the parent ctx.Err()).
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	out = Exec(ctx, dir, []string{"sh", "-c", "sleep 5"}, 5*time.Second, nil)
	assert.False(t, out.TimedOut, "parent cancel must not read as a timeout")
	require.Error(t, ctx.Err())
}

func TestExecPassesExtraEnv(t *testing.T) {
	dir := t.TempDir()

	out := Exec(context.Background(), dir, []string{"sh", "-c", "echo $JAVA_HOME"}, time.Minute, []string{"JAVA_HOME=/opt/jdk"})
	assert.Contains(t, out.Output, "/opt/jdk")
}
