package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
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
		// failure - the tool-missing heuristic is not consulted for it.
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
// under a make/just/task wrapper - which masks the inner 127 as a plain exit - is
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

// TestClassifyVerifyResourceExhaustionNote pins the note for a spawn that died
// of container resource pressure: the operator must read exhaustion, not a
// "tool missing" hunt for a toolchain that is present. A start-error WITHOUT
// the signature keeps the tool-missing note.
func TestClassifyVerifyResourceExhaustionNote(t *testing.T) {
	exhausted := classifyVerify(verifyPlan{}, verifyexec.Outcome{
		StartErr: true, ExitCode: -1,
		Output: "fork/exec /usr/bin/x: resource temporarily unavailable",
	})
	assert.Equal(t, verifySkipped, exhausted.Status)
	assert.Contains(t, exhausted.Note, "resource exhaustion")
	assert.NotContains(t, exhausted.Note, "tool missing")

	plain := classifyVerify(verifyPlan{}, verifyexec.Outcome{
		StartErr: true, ExitCode: -1,
		Output: "fork/exec /usr/bin/x: no such file or directory",
	})
	assert.Equal(t, verifySkipped, plain.Status)
	assert.Contains(t, plain.Note, "tool missing")
}

func TestDetectVerifyCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probing is POSIX-only")
	}

	t.Run("go.mod with go on path", func(t *testing.T) {
		stubTools(t, "go")

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module example.com/x\n")

		det := detectVerifyCommand(dir)
		assert.Equal(t, []string{"go", "test", "./..."}, det.Argv)
		assert.Equal(t, "go test ./...", det.Display)
		assert.False(t, det.Wrapper, "a marker-table command is not a wrapper")
		assert.Empty(t, det.Marker, "a resolved command carries no diagnostic marker")
		assert.Empty(t, det.Reason)
	})

	t.Run("cargo project", func(t *testing.T) {
		stubTools(t, "cargo")

		dir := t.TempDir()
		writeFile(t, dir, "Cargo.toml", "[package]\nname=\"x\"\n")

		det := detectVerifyCommand(dir)
		assert.Equal(t, []string{"cargo", "test"}, det.Argv)
	})

	t.Run("Go repo with Makefile+go uses make", func(t *testing.T) {
		stubTools(t, "make", "go")

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "build:\n\tgo build ./...\ntest:\n\tgo test ./...\n")
		writeFile(t, dir, "go.mod", "module example.com/x\n")

		det := detectVerifyCommand(dir)
		assert.Equal(t, []string{"make", "test"}, det.Argv)
		assert.True(t, det.Wrapper, "a make wrapper is flagged for the tool-missing heuristic")
	})

	t.Run("pure-make repo uses make", func(t *testing.T) {
		stubTools(t, "make")

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "test:\n\t./run-tests.sh\n")

		det := detectVerifyCommand(dir)
		assert.Equal(t, []string{"make", "test"}, det.Argv)
		assert.True(t, det.Wrapper)
	})

	t.Run("Rust Makefile without cargo skips the wrapper", func(t *testing.T) {
		// make resolves but cargo does not: the make wrapper would shell out to
		// cargo and false-fail, so detection skips it AND the cargo row, returning
		// nothing (the model-proposal tier would take over in a real run).
		stubTools(t, "make") // no cargo

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "test:\n\tcargo test\n")
		writeFile(t, dir, "Cargo.toml", "[package]\nname=\"x\"\n")

		det := detectVerifyCommand(dir)
		assert.Nil(t, det.Argv, "make must be skipped when the declared toolchain is absent")
		assert.Equal(t, "Cargo.toml", det.Marker, "the diagnostic walk tracks the unresolved cargo row")
		assert.Contains(t, det.Reason, "cargo")
	})

	t.Run("npm placeholder not detected", func(t *testing.T) {
		stubTools(t, "npm")

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`)

		det := detectVerifyCommand(dir)
		assert.Nil(t, det.Argv, "the npm-init placeholder test script is not a real command")
	})

	t.Run("real npm test detected", func(t *testing.T) {
		stubTools(t, "npm")

		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"vitest run"}}`)

		det := detectVerifyCommand(dir)
		assert.Equal(t, []string{"npm", "test"}, det.Argv)
	})

	t.Run("gradlew without exec bit not chosen", func(t *testing.T) {
		stubTools(t, "java") // no gradle on path, gradlew present but not executable

		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "gradlew"), []byte("#!/bin/sh\n"), 0o644))

		det := detectVerifyCommand(dir)
		assert.Nil(t, det.Argv, "a non-executable gradlew and no system gradle resolves nothing")
	})

	t.Run("no markers resolves nothing", func(t *testing.T) {
		stubTools(t, "go")

		dir := t.TempDir()

		det := detectVerifyCommand(dir)
		assert.Nil(t, det.Argv)
		assert.Empty(t, det.Display)
		assert.Empty(t, det.Marker, "no recognised marker present at all - a pure docs repo keeps the silent skip")
		assert.Empty(t, det.Reason)
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

// TestResolveVerifyDeclaredProbeFailsSentinel pins the toolchain-missing park:
// a declared verify command that cannot run, with no detected marker to fall
// back to, must raise ToolchainMissingError at the final fall-through rather
// than silently skip.
func TestResolveVerifyDeclaredProbeFailsSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t) // nothing on PATH: pytest cannot probe

	o := &run{d: Deps{Cfg: Config{
		Workspace: t.TempDir(),
		Verify:    &DeclaredVerify{Command: "pytest -q"},
	}}}

	plan, err := o.resolveVerify(context.Background())
	require.Error(t, err)

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)
	assert.Equal(t, "declared", tme.Tier)
	assert.Equal(t, "pytest -q", tme.Subject)
	assert.NotEmpty(t, tme.Reason)
	assert.Empty(t, plan.Argv, "an errored resolution returns a zero plan")
}

// TestResolveVerifyDetectedMarkerUnresolvedSentinel pins the toolchain-missing
// park for Tier 2: a pom.xml is present, mvn resolves but java does not, and no
// other marker is present - resolution must raise the sentinel rather than
// silently skip.
func TestResolveVerifyDetectedMarkerUnresolvedSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "mvn") // mvn resolves, java does not

	dir := t.TempDir()
	writeFile(t, dir, "pom.xml", "<project></project>\n")

	o := &run{d: Deps{Cfg: Config{Workspace: dir}}}

	plan, err := o.resolveVerify(context.Background())
	require.Error(t, err)

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)
	assert.Equal(t, "detected", tme.Tier)
	assert.Equal(t, "maven project", tme.Subject, "the generic marker name, not the specific pom.xml/mvnw file that happened to trigger it")
	assert.Contains(t, tme.Reason, "java")
	assert.Empty(t, plan.Argv, "an errored resolution returns a zero plan")
}

// TestResolveVerifyGradleMarkerUnresolvedSentinel pins the toolchain-missing
// park for a gradle marker: a build.gradle is present, gradle resolves but java
// does not, and no other marker is present - resolution must raise the sentinel
// with an accurate reason. Closes the zero-coverage gap on the former
// gradleReason ladder.
func TestResolveVerifyGradleMarkerUnresolvedSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "gradle") // gradle resolves, java does not

	dir := t.TempDir()
	writeFile(t, dir, "build.gradle", "// gradle build\n")

	o := &run{d: Deps{Cfg: Config{Workspace: dir}}}

	plan, err := o.resolveVerify(context.Background())
	require.Error(t, err)

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)
	assert.Equal(t, "detected", tme.Tier)
	assert.Equal(t, "gradle project", tme.Subject)
	assert.Contains(t, tme.Reason, "java")
	assert.Empty(t, plan.Argv, "an errored resolution returns a zero plan")
}

// TestResolveVerifyPythonMarkerUnresolvedSentinel pins the toolchain-missing
// park for a pytest marker: a pytest.ini declares pytest config, but neither
// pytest nor python3 resolve - resolution must raise the sentinel with an
// accurate reason. Closes the zero-coverage gap on the former pythonReason
// ladder.
func TestResolveVerifyPythonMarkerUnresolvedSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t) // neither pytest nor python3 on PATH

	dir := t.TempDir()
	writeFile(t, dir, "pytest.ini", "[pytest]\n")

	o := &run{d: Deps{Cfg: Config{Workspace: dir}}}

	plan, err := o.resolveVerify(context.Background())
	require.Error(t, err)

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)
	assert.Equal(t, "detected", tme.Tier)
	assert.Equal(t, "pytest config", tme.Subject)
	assert.Contains(t, tme.Reason, "python3")
	assert.Empty(t, plan.Argv, "an errored resolution returns a zero plan")
}

// TestResolveVerifyCanceledContextDoesNotPark pins the cancellation fix: a
// canceled Tier-3 model call is inconclusive, not proof of a missing toolchain
// (it never got a real chance to rescue), so the Tier-4 trigger must not read
// the pre-Tier-3 declared/detected state as a sentinel when the context is
// already canceled - it returns the ctx error instead.
func TestResolveVerifyCanceledContextDoesNotPark(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "mvn") // mvn resolves, java does not - would sentinel if not canceled

	dir := t.TempDir()
	writeFile(t, dir, "pom.xml", "<project></project>\n")

	o := &run{d: Deps{Cfg: Config{Workspace: dir}}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	plan, err := o.resolveVerify(ctx)
	require.Error(t, err)

	var tme *ToolchainMissingError
	assert.NotErrorAs(t, err, &tme, "a canceled context must not produce the toolchain sentinel")
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, plan.Argv, "an errored resolution returns a zero plan")
}

// TestResolveVerifyCanceledContextDeclaredDoesNotPark pins the same
// cancellation guard for the declared trigger: a declared command whose probe
// fails, with the context already canceled, returns the ctx error - never the
// sentinel.
func TestResolveVerifyCanceledContextDeclaredDoesNotPark(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t) // nothing on PATH: the declared pytest cannot probe - would sentinel if not canceled

	o := &run{d: Deps{Cfg: Config{
		Workspace: t.TempDir(),
		Verify:    &DeclaredVerify{Command: "pytest -q"},
	}}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	plan, err := o.resolveVerify(ctx)
	require.Error(t, err)

	var tme *ToolchainMissingError
	assert.NotErrorAs(t, err, &tme, "a canceled context must not produce the toolchain sentinel")
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, plan.Argv, "an errored resolution returns a zero plan")
}

// TestResolveVerifyDocsRepoSkipUnchanged pins that a repo with no declared
// command and no recognised toolchain marker keeps today's silent skip - the
// sentinel must never fire when nothing implicates a toolchain at all.
func TestResolveVerifyDocsRepoSkipUnchanged(t *testing.T) {
	stubTools(t)

	dir := t.TempDir()
	writeFile(t, dir, "README.md", "# docs only\n")

	o := &run{d: Deps{Cfg: Config{Workspace: dir}}}

	plan, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceNone, plan.Source)
	assert.Empty(t, plan.Argv)
}

// TestResolveVerifyOnlyGoModResolvesUnchanged pins that a normal resolving
// repo is untouched by the toolchain-missing tracking: go.mod present and go
// resolves must still resolve exactly as before, with no error.
func TestResolveVerifyOnlyGoModResolvesUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "go")

	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/x\n")

	o := &run{d: Deps{Cfg: Config{Workspace: dir}}}

	plan, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDetected, plan.Source)
	assert.Equal(t, []string{"go", "test", "./..."}, plan.Argv)
}

// TestResolveVerifyMixedRepoNpmWinsNoPark pins that mixed-repo masking is
// intentionally out of scope for this fix: detectVerifyCommand's fixed
// priority table returns the npm test script and never even looks at the
// broken pom.xml row, so no toolchain-missing park fires. Closing this gap
// (surfacing a broken toolchain when a HIGHER-priority one resolves fine) is a
// deliberate non-goal - see the parent card's Risk/scope notes.
func TestResolveVerifyMixedRepoNpmWinsNoPark(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "npm") // no mvn, no java

	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"vitest run"}}`)
	writeFile(t, dir, "pom.xml", "<project></project>\n")

	o := &run{d: Deps{Cfg: Config{Workspace: dir}}}

	plan, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDetected, plan.Source)
	assert.Equal(t, []string{"npm", "test"}, plan.Argv)
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
// (pre-code) is NOT final - a phase that adds the project's tooling makes the
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

// withFastVerifyRetryWait shrinks the package-level retry wait for the
// duration of a test, following the same save/restore pattern as
// subtaskHeartbeatInterval in execute_test.go. Mutates package state; the
// caller's test cannot run in parallel.
func withFastVerifyRetryWait(t *testing.T) {
	t.Helper()

	prev := verifyRetryWait
	verifyRetryWait = time.Millisecond

	t.Cleanup(func() { verifyRetryWait = prev })
}

func TestRunVerifyPlanRetriesOnResourceExhaustion(t *testing.T) {
	withFastVerifyRetryWait(t)

	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}

	calls := 0
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		calls++
		if calls == 1 {
			return verifyexec.Outcome{
				ExitCode: 2,
				Output:   "fork/exec /usr/bin/x: resource temporarily unavailable",
			}
		}

		return verifyexec.Outcome{ExitCode: 0}
	}

	res, err := o.runVerifyPlan(context.Background(), "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, verifyPassed, res.Status)
	assert.Equal(t, 2, calls, "an exhausted first run retries once")
}

// TestRunVerifyPlanRetriesOnStartErrResourceExhaustion covers the actual
// process-spawn-failure shape (StartErr, no exit code): execWithEnv folds the
// spawn error's own text - e.g. "fork/exec ...: resource temporarily
// unavailable" - into Output, and that text is what LooksResourceExhausted
// reads to trigger the retry.
func TestRunVerifyPlanRetriesOnStartErrResourceExhaustion(t *testing.T) {
	withFastVerifyRetryWait(t)

	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}

	calls := 0
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		calls++
		if calls == 1 {
			return verifyexec.Outcome{
				ExitCode: -1,
				StartErr: true,
				Output:   "fork/exec /usr/bin/x: resource temporarily unavailable",
			}
		}

		return verifyexec.Outcome{ExitCode: 0}
	}

	res, err := o.runVerifyPlan(context.Background(), "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, verifyPassed, res.Status)
	assert.Equal(t, 2, calls, "a start-error carrying the exhaustion signature in its text retries once")
}

// TestRunVerifyPlanStartErrExhaustionOnBothAttempts pins the postmortem's
// literal terminal state: both spawns die of resource pressure. The result is
// skipped (environmental, exempt from outcome reporting) with the
// exhaustion note - not a misleading "tool missing" - and no third attempt.
func TestRunVerifyPlanStartErrExhaustionOnBothAttempts(t *testing.T) {
	withFastVerifyRetryWait(t)

	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}

	calls := 0
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		calls++

		return verifyexec.Outcome{
			ExitCode: -1,
			StartErr: true,
			Output:   "fork/exec /usr/bin/x: resource temporarily unavailable",
		}
	}

	res, err := o.runVerifyPlan(context.Background(), "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, verifySkipped, res.Status)
	assert.Contains(t, res.Note, "resource exhaustion")
	assert.Equal(t, 2, calls, "exactly one retry, then the terminal classification stands")
}

func TestRunVerifyPlanDoesNotRetryPlainFailure(t *testing.T) {
	withFastVerifyRetryWait(t)

	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}

	calls := 0
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		calls++

		return verifyexec.Outcome{ExitCode: 1, Output: "--- FAIL: TestFoo\n2 tests failed"}
	}

	res, err := o.runVerifyPlan(context.Background(), "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, verifyFailed, res.Status)
	assert.Equal(t, 1, calls, "a plain failure is never retried")
}

func TestRunVerifyPlanRetryExhaustedAgainStaysFailed(t *testing.T) {
	withFastVerifyRetryWait(t)

	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}

	calls := 0
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		calls++

		return verifyexec.Outcome{ExitCode: 2, Output: "cannot allocate memory"}
	}

	res, err := o.runVerifyPlan(context.Background(), "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, verifyFailed, res.Status)
	assert.Equal(t, 2, calls, "exhaustion on the retry itself is not retried a second time")
}

// TestRunVerifyPlanDoesNotRetryTimedOutOutcome proves a timed-out run is never
// retried even when its partial output carries an exhaustion signature:
// retrying would double a run already at the wall-clock ceiling, and a rerun
// would not fit the same timeout anyway.
func TestRunVerifyPlanDoesNotRetryTimedOutOutcome(t *testing.T) {
	withFastVerifyRetryWait(t)

	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}

	calls := 0
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		calls++

		return verifyexec.Outcome{TimedOut: true, ExitCode: -1, Output: "too many open files"}
	}

	res, err := o.runVerifyPlan(context.Background(), "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, verifySkipped, res.Status)
	assert.Contains(t, res.Note, "verify timed out")
	assert.Equal(t, 1, calls, "a timed-out outcome is never retried regardless of its output")
}

// TestRunVerifyPlanCancelDuringRetryWaitPropagates proves a parent-context
// cancellation that lands during the retry wait aborts the run rather than
// letting the retry fire.
func TestRunVerifyPlanCancelDuringRetryWaitPropagates(t *testing.T) {
	prev := verifyRetryWait
	verifyRetryWait = 300 * time.Millisecond

	t.Cleanup(func() { verifyRetryWait = prev })

	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}

	calls := 0
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		calls++

		return verifyexec.Outcome{ExitCode: 2, Output: "resource temporarily unavailable"}
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := o.runVerifyPlan(ctx, "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.Error(t, err, "a parent cancel during the retry wait propagates the abort, not a verify outcome")
	assert.Equal(t, 1, calls, "the retry never runs once cancellation interrupts the wait")
}

// TestRunVerifyPlanRedactsRetriedResult proves redaction applies to the FINAL
// (retried) output, not the first attempt's - so a retried run's output is
// never returned unredacted.
func TestRunVerifyPlanRedactsRetriedResult(t *testing.T) {
	withFastVerifyRetryWait(t)

	o := &run{d: Deps{
		Redact: func(s string) string { return "[REDACTED]" },
		Cfg:    Config{Workspace: t.TempDir()},
	}}

	calls := 0
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		calls++
		if calls == 1 {
			return verifyexec.Outcome{ExitCode: 2, Output: "token=hunter2: resource temporarily unavailable"}
		}

		return verifyexec.Outcome{ExitCode: 1, Output: "token=hunter2: still failing"}
	}

	res, err := o.runVerifyPlan(context.Background(), "dir", verifyPlan{Argv: []string{"x"}, Timeout: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, verifyFailed, res.Status)
	assert.Equal(t, "[REDACTED]", res.Output, "the retried result's output is redacted, not the first attempt's")
	assert.Equal(t, 2, calls)
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

// TestResolveVerifyNestedMonorepoDetected pins the headline fix: a monorepo
// with only nested markers previously resolved NOTHING and review ran blind;
// it must now resolve a composed, detected command.
func TestResolveVerifyNestedMonorepoDetected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "mvn", "java", "npm", "bash")

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "backend"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "frontend"), 0o755))
	writeFile(t, filepath.Join(dir, "backend"), "pom.xml", "<project></project>\n")
	writeFile(t, filepath.Join(dir, "frontend"), "package.json", `{"name":"x","scripts":{"test":"vitest run"}}`)

	o := &run{d: Deps{Cfg: Config{Workspace: dir}}}

	plan, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDetected, plan.Source)
	assert.Equal(t, "mvn -q -f backend/pom.xml test && npm --prefix frontend test", plan.Display)
	assert.False(t, plan.Wrapper)
}

// TestResolveVerifyNestedMarkerParksSentinel pins the stock-image outcome: a
// nested pom.xml whose toolchain cannot run must PARK with the detected-tier
// sentinel instead of silently proceeding unverified.
func TestResolveVerifyNestedMarkerParksSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "mvn") // java missing

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "backend"), 0o755))
	writeFile(t, filepath.Join(dir, "backend"), "pom.xml", "<project></project>\n")

	o := &run{d: Deps{Cfg: Config{Workspace: dir}}}

	plan, err := o.resolveVerify(context.Background())
	require.Error(t, err)

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)
	assert.Equal(t, "detected", tme.Tier)
	assert.Equal(t, "maven project (in backend/)", tme.Subject)
	assert.Contains(t, tme.Reason, "java")
	assert.Empty(t, plan.Argv)
}

// TestResolveVerifyDeclaredCdCommandResolves pins the declared-tier fix: the
// operator's natural monorepo command probes correctly with cd tracking and
// resolves at Tier 1 instead of parking as toolchain-missing.
func TestResolveVerifyDeclaredCdCommandResolves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "java", "bash")

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "backend"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "backend", "mvnw"), []byte("#!/bin/sh\n"), 0o755))

	o := &run{d: Deps{Cfg: Config{
		Workspace: dir,
		Verify:    &DeclaredVerify{Command: "cd backend && ./mvnw -q test"},
	}}}

	plan, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDeclared, plan.Source)
	assert.Equal(t, "cd backend && ./mvnw -q test", plan.Display)
}

// TestLogVerifyResolutionRecordsUnverifiedSection pins that the quietest
// possible outcome - nothing declared, detected, or proposed - is recorded on
// the card body, not only as one activity-log line.
func TestLogVerifyResolutionRecordsUnverifiedSection(t *testing.T) {
	ops := &fakeOps{}
	o := &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1", Workspace: t.TempDir()}}}

	p, err := o.resolveVerify(context.Background())
	require.NoError(t, err)
	require.Empty(t, p.Argv)

	o.logVerifyResolution(context.Background(), p)

	body := ops.lastBody()
	assert.Contains(t, body, "## Verify Command")
	assert.Contains(t, body, "UNVERIFIED")
}

// TestLogVerifyResolutionUpgradeReplacesUnverifiedSection pins the re-resolve
// path: a run that starts unverified and later gains a detectable marker must
// end with a section naming the command, not a stale UNVERIFIED warning.
func TestLogVerifyResolutionUpgradeReplacesUnverifiedSection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "go")

	dir := t.TempDir()
	ops := &fakeOps{}
	o := &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1", Workspace: dir}}}

	_, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	require.Contains(t, ops.lastBody(), "UNVERIFIED")

	writeFile(t, dir, "go.mod", "module example.com/x\n")

	p, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, p.Argv)

	body := ops.lastBody()
	assert.Contains(t, body, "go test ./...")
	assert.NotContains(t, body, "UNVERIFIED", "the upsert must replace the stale warning")
}

// TestResolveVerifyNestedPartialCoverageReachesTheCard pins the note plumbing
// end to end: a nested module whose toolchain cannot run must surface on the
// resolved PLAN and on the card body, not only inside detectNested's return
// value. Partial coverage nobody can see is the silence this PR exists to end.
func TestResolveVerifyNestedPartialCoverageReachesTheCard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "npm") // no mvn, no java: the backend cannot resolve

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "backend"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "frontend"), 0o755))
	writeFile(t, filepath.Join(dir, "backend"), "pom.xml", "<project></project>\n")
	writeFile(t, filepath.Join(dir, "frontend"), "package.json", `{"name":"x","scripts":{"test":"vitest run"}}`)

	ops := &fakeOps{}
	o := &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1", Workspace: dir}}}

	p, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDetected, p.Source)
	require.Len(t, p.Notes, 1, "the uncovered module's note must reach the resolved plan")

	body := ops.lastBody()
	assert.Contains(t, body, "npm --prefix frontend test")
	assert.Contains(t, body, "NOT covered")
	assert.Contains(t, body, "\n- nested module backend/",
		"notes render as list items; markdown folds newline-separated lines into one paragraph")
}

// TestEnsureVerifyToolchainParkReplacesUnverifiedSection pins the park's card
// record: a run that first recorded the UNVERIFIED section and then parks on a
// detected-but-unrunnable toolchain must end with the park's own remedy on the
// body. The stale "declare a verify command" advice is wrong for this park - a
// command WAS detected - and the card is about to be transitioned to blocked
// for a human to read.
func TestEnsureVerifyToolchainParkReplacesUnverifiedSection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "mvn") // java missing: a nested pom cannot resolve

	dir := t.TempDir()
	ops := &fakeOps{}
	o := &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1", Workspace: dir}}}

	_, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	require.Contains(t, ops.lastBody(), "UNVERIFIED")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "backend"), 0o755))
	writeFile(t, filepath.Join(dir, "backend"), "pom.xml", "<project></project>\n")

	_, err = o.ensureVerify(context.Background())

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)

	body := ops.lastBody()
	assert.Contains(t, body, "PARKED")
	assert.Contains(t, body, "maven project (in backend/)")
	assert.NotContains(t, body, "No verify command was declared",
		"the park must replace the stale UNVERIFIED remedy, not sit under it")
}

// TestVerifyToolchainSectionRemedyMatchesThePark pins the two park shapes apart:
// the nested-module cap is the one ToolchainMissingError where every toolchain
// runs fine and detection merely declined to guess, so its card must not send
// the operator hunting for a tool that is already installed.
func TestVerifyToolchainSectionRemedyMatchesThePark(t *testing.T) {
	capPark := verifyToolchainSection(&ToolchainMissingError{
		Tier:    "detected",
		Subject: nestedModulesMarker,
		Reason:  "5 nested modules detected - declare a verify command that covers them",
	})
	assert.Contains(t, capPark, "Declare a verify command")
	assert.NotContains(t, capPark, "Install the toolchain",
		"nothing is missing on the cap park; the install remedy would misdirect")
	assert.NotContains(t, capPark, "toolchain cannot run here")

	missingPark := verifyToolchainSection(&ToolchainMissingError{
		Tier:    "detected",
		Subject: "maven project (in backend/)",
		Reason:  `java: exec: "java": executable file not found in $PATH`,
	})
	assert.Contains(t, missingPark, "toolchain cannot run here")
	assert.Contains(t, missingPark, "Install the toolchain")
	assert.Contains(t, missingPark, "java")
}

// TestRunVerifyPlanEmitsVerification: the gate is a subprocess, not a tool call,
// so without this event a passed or skipped gate leaves no output on any
// channel. The transcript is the only post-mortem surface for it.
func TestRunVerifyPlanEmitsVerification(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	o := &run{d: Deps{
		Ops:  ops,
		Cfg:  Config{CardID: "CARD-1", Workspace: t.TempDir()},
		Emit: events.NewEmitter(nil, &transcript),
	}}
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0, Output: "Tests run: 25, Failures: 0\nBUILD SUCCESS\n"}
	}

	res, err := o.runVerifyPlan(context.Background(), "dir", verifyPlan{
		Argv:    []string{"mvn", "-q", "verify"},
		Display: "mvn -q verify",
		Source:  verifySourceDetected,
		Timeout: time.Minute,
	})
	require.NoError(t, err)
	require.Equal(t, verifyPassed, res.Status)

	line := transcript.String()
	require.Contains(t, line, `"kind":"verification"`, "a verification event is emitted; transcript=%s", line)
	assert.Contains(t, line, `"ok":true`)
	assert.Contains(t, line, `"status":"passed"`)
	assert.Contains(t, line, "mvn -q verify", "the command is named in the event")
	assert.Contains(t, line, "BUILD SUCCESS", "the gate output is carried, not just its length")
}

// TestRunVerifyPlanEmitNilSafe: Deps.Emit is nil on several construction paths
// and events.Emitter.Emit has no nil-receiver guard, so the emit must be
// guarded the way pinwarn.go does it.
func TestRunVerifyPlanEmitNilSafe(t *testing.T) {
	o := &run{d: Deps{Cfg: Config{Workspace: t.TempDir()}}}
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0, Output: "ok"}
	}

	assert.NotPanics(t, func() {
		res, err := o.runVerifyPlan(context.Background(), "dir",
			verifyPlan{Argv: []string{"x"}, Display: "x", Timeout: time.Minute})
		require.NoError(t, err)
		assert.Equal(t, verifyPassed, res.Status)
	})
}

func TestLogVerifyRound(t *testing.T) {
	tests := []struct {
		name string
		res  verifyResult
		want string
	}{
		{
			name: "passed",
			res:  verifyResult{Status: verifyPassed},
			want: "verify passed - review round 1",
		},
		{
			name: "failed",
			res:  verifyResult{Status: verifyFailed},
			want: "verify failed - review round 2",
		},
		{
			name: "skipped names the reason and says it proceeds unverified",
			res:  verifyResult{Status: verifySkipped, Note: "timed out after 10m0s"},
			want: "verify skipped (timed out after 10m0s) - review round 3 - proceeding unverified",
		},
		{
			name: "skipped without a note still says it proceeds unverified",
			res:  verifyResult{Status: verifySkipped},
			want: "verify skipped - review round 1 - proceeding unverified",
		},
	}

	rounds := []int{1, 2, 3, 1}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			o := &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1"}}}

			o.logVerifyRound(context.Background(), tt.res, rounds[i])

			require.Len(t, ops.logs, 1, "exactly one line per round; logs=%v", ops.logs)
			assert.Equal(t, tt.want, ops.logs[0])
		})
	}
}

// TestResolveVerifyParksOnUnreadableConfig: an operator declared a gate and we
// could not read it. That is not "nothing declared" - the ladder must note it,
// still try detection, and park rather than ship unverified when nothing else
// resolves.
func TestResolveVerifyParksOnUnreadableConfig(t *testing.T) {
	ws := t.TempDir() // empty workspace: no marker, so detection finds nothing

	ops := &fakeOps{}
	o := &run{d: Deps{
		Ops: ops,
		Cfg: Config{
			CardID:            "CARD-1",
			Workspace:         ws,
			VerifyConfigError: "CMX_VERIFY could not be parsed: unexpected end of JSON input",
		},
	}}
	o.proposeAttempted = true // skip Tier 3; no model in this test

	_, err := o.resolveVerify(context.Background())

	var missing *ToolchainMissingError
	require.ErrorAs(t, err, &missing, "an unreadable declared config must park, not proceed unverified")
	assert.Equal(t, "declared", missing.Tier)
	assert.Contains(t, missing.Reason, "could not be parsed")
}
