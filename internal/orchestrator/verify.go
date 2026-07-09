package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"gopkg.in/yaml.v3"
)

// verifyStatus is the tri-state outcome of a verify run. The zero value is
// verifySkipped so an unrun gate reads as "not verified", never a false pass.
type verifyStatus int

const (
	verifySkipped verifyStatus = iota // did not run, timed out, or the tool was missing
	verifyPassed                      // the command exited 0
	verifyFailed                      // the command ran and failed
)

// verifySource records where a resolved verify command came from, for honest
// provenance on the card and PR surfaces.
type verifySource string

const (
	verifySourceNone     verifySource = ""
	verifySourceDeclared verifySource = "declared"
	verifySourceDetected verifySource = "detected"
	verifySourceProposed verifySource = "model-proposed"
)

// verifyPlan is the run's resolved verify command, cached once by ensureVerify.
// An empty Argv means "nothing to run" (the skip tier): the gate proceeds
// unverified. Timeout and Env apply regardless of which tier produced the
// command — an operator's declared timeout/env bind a detected or proposed
// command too.
type verifyPlan struct {
	Argv    []string
	Display string // human-facing command string
	Source  verifySource
	Timeout time.Duration
	Env     []string // resolved KEY=VALUE pass-throughs
	Notes   []string // resolution notes (e.g. a declared command that could not run)
}

// verifyResult is one execution's classified outcome. Output is redacted at
// capture (runVerifyPlan applies the run redactor); Note is the human-facing
// reason a run was skipped.
type verifyResult struct {
	Status verifyStatus
	Output string
	Note   string
}

const (
	// defaultVerifyTimeout bounds a verify run when the operator declared none.
	defaultVerifyTimeout = 10 * time.Minute
	// minVerifyTimeout / maxVerifyTimeout clamp a declared timeout: a sub-30s
	// gate flakes on cold caches, and a run over 2h is a hang, not a slow suite.
	minVerifyTimeout = 30 * time.Second
	maxVerifyTimeout = 2 * time.Hour
)

// verifyProvenance labels the source of a resolved verify command for the judge
// report and prompts. A nil plan or a none-source (defensive) reads as unknown.
func verifyProvenance(p *verifyPlan) string {
	if p == nil || p.Source == verifySourceNone {
		return "unknown source"
	}

	return string(p.Source)
}

// classifyVerify maps a raw execution Outcome to the tri-state result. It is
// pure so the whole decision table is unit-tested without a subprocess. Parent-
// context cancellation is NOT an outcome here — runVerifyPlan checks ctx.Err()
// and propagates the abort before classifying.
func classifyVerify(plan verifyPlan, out verifyexec.Outcome) verifyResult {
	switch {
	case out.ExitCode == 0 && !out.TimedOut && !out.StartErr:
		return verifyResult{Status: verifyPassed, Output: out.Output}
	case out.TimedOut:
		return verifyResult{
			Status: verifySkipped,
			Output: out.Output,
			Note:   fmt.Sprintf("verify timed out after %s — inconclusive, treated as unverified", plan.Timeout),
		}
	case out.StartErr || out.ExitCode == 127 || verifyexec.LooksToolMissing(out.Output):
		return verifyResult{
			Status: verifySkipped,
			Output: out.Output,
			Note:   "verify tool missing — the command could not run, treated as unverified",
		}
	default:
		return verifyResult{Status: verifyFailed, Output: out.Output}
	}
}

// ensureVerify resolves the verify plan once per run and caches it: the first
// phase to reach the gate (execute, judge, or review) resolves it, and every
// later phase reuses the result. It emits the single resolution log line on
// first resolve. A budget park during resolution (the proposal tier) propagates
// and is not cached, so a resumed run re-attempts.
func (o *run) ensureVerify(ctx context.Context) (verifyPlan, error) {
	if o.verify != nil {
		return *o.verify, nil
	}

	p, err := o.resolveVerify(ctx)
	if err != nil {
		return verifyPlan{}, err
	}

	o.verify = &p
	o.logVerifyResolution(ctx, p)

	return p, nil
}

// resolveVerify runs the resolution ladder: declared command (probed) beats
// repo-convention detection beats a model proposal beats skip. A declared
// command that cannot run does NOT disable the gate — it records a note and
// falls through, so a typo cannot silently drop verification. A budget park from
// the proposal tier propagates; every other failure degrades toward skip.
func (o *run) resolveVerify(ctx context.Context) (verifyPlan, error) {
	cfg := o.d.Cfg
	timeout := o.verifyTimeout()
	env := o.verifyEnv()

	var notes []string

	// Tier 1: operator-declared command.
	if d := cfg.Verify; d != nil && strings.TrimSpace(d.Command) != "" {
		cmd := strings.TrimSpace(d.Command)

		err := verifyexec.ProbeShell(cfg.Workspace, cmd)
		if err == nil {
			return verifyPlan{
				Argv:    verifyexec.ShellArgv(cmd),
				Display: cmd,
				Source:  verifySourceDeclared,
				Timeout: timeout,
				Env:     env,
			}, nil
		}

		// A declared command that cannot run does not disable the gate: note it and
		// fall through, so a typo cannot silently drop verification.
		notes = append(notes, fmt.Sprintf("declared verify command cannot run: %s (missing: %v)", cmd, err))
	}

	// Tier 2: repo-convention detection.
	if argv, display := detectVerifyCommand(cfg.Workspace); len(argv) > 0 {
		return verifyPlan{
			Argv:    argv,
			Display: display,
			Source:  verifySourceDetected,
			Timeout: timeout,
			Env:     env,
		}, nil
	}

	// Tier 3: model proposal (a code-executed command, never persisted).
	proposed, err := o.proposeVerify(ctx)
	if err != nil {
		return verifyPlan{}, err // budget park
	}

	if len(proposed.Argv) > 0 {
		return proposed, nil
	}

	// Tier 4: skip. The gate proceeds unverified; the resolution log says so.
	return verifyPlan{Source: verifySourceNone, Timeout: timeout, Env: env, Notes: notes}, nil
}

// runVerifyPlan executes the resolved plan in dir and returns the classified
// result. It is the single capture point: it redacts the output and
// disambiguates a parent-context cancel (returned as an error to propagate the
// abort) from a real verify outcome. An empty plan is a skip, never a run.
func (o *run) runVerifyPlan(ctx context.Context, dir string, plan verifyPlan) (verifyResult, error) {
	if len(plan.Argv) == 0 {
		return verifyResult{Status: verifySkipped, Note: "no verify command resolved"}, nil
	}

	out := o.runVerify(ctx, dir, plan.Argv, plan.Timeout, plan.Env)

	if err := ctx.Err(); err != nil {
		return verifyResult{}, err
	}

	res := classifyVerify(plan, out)
	if o.d.Redact != nil {
		res.Output = o.d.Redact(res.Output)
	}

	return res, nil
}

// verifyTimeout is the run's verify timeout: the operator's declared value
// clamped to [30s, 2h], or the default when none was declared.
func (o *run) verifyTimeout() time.Duration {
	t := defaultVerifyTimeout
	if d := o.d.Cfg.Verify; d != nil && d.Timeout > 0 {
		t = d.Timeout
	}

	return min(max(t, minVerifyTimeout), maxVerifyTimeout)
}

// verifyEnv resolves the operator's declared env pass-throughs to KEY=VALUE
// entries: it re-filters the names agent-side (the command may be model-proposed)
// and reads each surviving name from the container environment.
func (o *run) verifyEnv() []string {
	d := o.d.Cfg.Verify
	if d == nil || len(d.Env) == 0 {
		return nil
	}

	var out []string

	for _, name := range verifyexec.FilterEnvNames(d.Env) {
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}

	return out
}

// logVerifyResolution records the one-per-run resolution outcome on the card:
// the resolved command with its source, or the loud UNVERIFIED variants when
// nothing resolved.
func (o *run) logVerifyResolution(ctx context.Context, p verifyPlan) {
	var msg string

	switch {
	case len(p.Argv) > 0:
		msg = fmt.Sprintf("verify command resolved: %s (%s)", p.Display, p.Source)
	case len(p.Notes) > 0:
		msg = strings.Join(p.Notes, "; ") + " — no fallback found; work will proceed UNVERIFIED"
	default:
		msg = "no verify command declared, detected, or proposed — work will proceed UNVERIFIED"
	}

	_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, msg) //nolint:errcheck // advisory resolution record
}

// ---- repo-convention detection ---------------------------------------------

// detectVerifyCommand best-effort resolves the project's verify command from
// workspace markers, target-language-agnostic. A wrapper (make/just/task test)
// wins first UNLESS the repo declares a toolchain whose tools are all absent —
// then the wrapper would shell out to a missing binary and false-fail, so it is
// skipped. Otherwise the marker table is walked in priority order and the first
// toolchain whose tool actually resolves is used. Returns (nil, "") when nothing
// runnable is found.
func detectVerifyCommand(workspace string) ([]string, string) {
	if argv := detectWrapper(workspace); argv != nil {
		return argv, strings.Join(argv, " ")
	}

	for _, row := range detectRows {
		if row.present(workspace) {
			if argv := row.resolve(workspace); argv != nil {
				return argv, strings.Join(argv, " ")
			}
		}
	}

	return nil, ""
}

// detectWrapper applies the wrapper rule: a test wrapper is used when its binary
// resolves AND either the repo has no toolchain markers at all (a pure-make C
// project) or at least one declared toolchain's tool resolves. When markers
// exist but none resolve, the wrapper is skipped so it cannot false-fail.
func detectWrapper(workspace string) []string {
	argv := wrapperArgv(workspace)
	if argv == nil {
		return nil
	}

	if verifyexec.Probe(workspace, argv) != nil {
		return nil // the wrapper binary itself is not installed
	}

	if !anyMarkerPresent(workspace) {
		return argv // pure wrapper project, no toolchain to check
	}

	if anyToolchainResolves(workspace) {
		return argv
	}

	return nil
}

// wrapperArgv returns the test-wrapper command declared by the workspace, in
// precedence order Makefile > justfile > Taskfile, or nil when none declares a
// test target.
func wrapperArgv(workspace string) []string {
	switch {
	case makefileHasTestTarget(filepath.Join(workspace, "Makefile")):
		return []string{"make", "test"}
	case justfileHasTestRecipe(workspace):
		return []string{"just", "test"}
	case taskfileHasTestTask(workspace):
		return []string{"task", "test"}
	default:
		return nil
	}
}

// detectRow is one recognised toolchain: present reports whether its marker is in
// the workspace; resolve returns the runnable argv (its tool present, JVM
// wrappers with a java runtime) or nil when the marker is there but the tool is
// not installed.
type detectRow struct {
	present func(workspace string) bool
	resolve func(workspace string) []string
}

// detectRows is the marker table in priority order. Detected commands stay argv
// (no shell); the timeout/env from the declared config still bind them.
var detectRows = []detectRow{
	{present: hasFile("go.mod"), resolve: probeArgv("go", "test", "./...")},
	{present: hasFile("Cargo.toml"), resolve: probeArgv("cargo", "test")},
	{present: hasRealNPMTestScript, resolve: probeArgv("npm", "test")},
	{present: hasPytestMarker, resolve: resolvePython},
	{present: hasGradleProject, resolve: resolveGradle},
	{present: hasMavenProject, resolve: resolveMaven},
	{present: hasDotnetProject, resolve: probeArgv("dotnet", "test")},
}

// anyMarkerPresent reports whether any recognised toolchain marker is present.
func anyMarkerPresent(workspace string) bool {
	for _, row := range detectRows {
		if row.present(workspace) {
			return true
		}
	}

	return false
}

// anyToolchainResolves reports whether at least one present toolchain's tool
// actually resolves in the workspace.
func anyToolchainResolves(workspace string) bool {
	for _, row := range detectRows {
		if row.present(workspace) && row.resolve(workspace) != nil {
			return true
		}
	}

	return false
}

// hasFile returns a present-func testing for a plain file at name.
func hasFile(name string) func(string) bool {
	return func(workspace string) bool {
		return fileExists(filepath.Join(workspace, name))
	}
}

// probeArgv returns a resolve-func that yields argv when its program probes
// runnable in the workspace, else nil.
func probeArgv(argv ...string) func(string) []string {
	return func(workspace string) []string {
		if verifyexec.Probe(workspace, argv) == nil {
			return argv
		}

		return nil
	}
}

// resolvePython prefers pytest, falling back to `python3 -m pytest` when pytest
// is not installed but a python runtime is.
func resolvePython(workspace string) []string {
	if verifyexec.Probe(workspace, []string{"pytest", "-q"}) == nil {
		return []string{"pytest", "-q"}
	}

	if verifyexec.Probe(workspace, []string{"python3", "-m", "pytest"}) == nil {
		return []string{"python3", "-m", "pytest"}
	}

	return nil
}

// hasGradleProject reports a Gradle project: an executable gradlew wrapper or a
// build script.
func hasGradleProject(workspace string) bool {
	return execFileExists(filepath.Join(workspace, "gradlew")) ||
		fileExists(filepath.Join(workspace, "build.gradle")) ||
		fileExists(filepath.Join(workspace, "build.gradle.kts"))
}

// resolveGradle prefers the executable wrapper, else the system gradle; both
// require a java runtime (enforced by Probe).
func resolveGradle(workspace string) []string {
	if execFileExists(filepath.Join(workspace, "gradlew")) {
		return probeArgv("./gradlew", "test")(workspace)
	}

	return probeArgv("gradle", "test")(workspace)
}

// hasMavenProject reports a Maven project: an executable mvnw wrapper or a pom.
func hasMavenProject(workspace string) bool {
	return execFileExists(filepath.Join(workspace, "mvnw")) ||
		fileExists(filepath.Join(workspace, "pom.xml"))
}

// resolveMaven prefers the executable wrapper, else system maven; both require a
// java runtime (enforced by Probe).
func resolveMaven(workspace string) []string {
	if execFileExists(filepath.Join(workspace, "mvnw")) {
		return probeArgv("./mvnw", "-q", "test")(workspace)
	}

	return probeArgv("mvn", "-q", "test")(workspace)
}

// hasDotnetProject reports a top-level .NET solution or project file.
func hasDotnetProject(workspace string) bool {
	for _, pat := range []string{"*.sln", "*.csproj", "*.fsproj"} {
		if m, _ := filepath.Glob(filepath.Join(workspace, pat)); len(m) > 0 {
			return true
		}
	}

	return false
}

// hasPytestMarker reports a pytest project: a pyproject, a pytest.ini, or a
// setup.cfg declaring a [tool:pytest] section.
func hasPytestMarker(workspace string) bool {
	if fileExists(filepath.Join(workspace, "pyproject.toml")) ||
		fileExists(filepath.Join(workspace, "pytest.ini")) {
		return true
	}

	data, err := readVerifyMarker(filepath.Join(workspace, "setup.cfg"))
	if err != nil {
		return false
	}

	return strings.Contains(string(data), "[tool:pytest]")
}

// justfileTestRe matches a justfile "test" recipe: a line beginning with the
// recipe name "test", optionally with parameters, ending in the recipe colon.
var justfileTestRe = regexp.MustCompile(`^test([ \t][^:]*)?:`)

// justfileHasTestRecipe reports whether a justfile in the workspace declares a
// "test" recipe. Recipe names are at column 0, so the match is untrimmed.
func justfileHasTestRecipe(workspace string) bool {
	for _, name := range []string{"justfile", "Justfile", ".justfile"} {
		data, err := readVerifyMarker(filepath.Join(workspace, name))
		if err != nil {
			continue
		}

		for line := range strings.SplitSeq(string(data), "\n") {
			if justfileTestRe.MatchString(line) {
				return true
			}
		}
	}

	return false
}

// taskfileHasTestTask reports whether a Taskfile in the workspace declares a
// "test" task.
func taskfileHasTestTask(workspace string) bool {
	for _, name := range []string{"Taskfile.yml", "Taskfile.yaml"} {
		data, err := readVerifyMarker(filepath.Join(workspace, name))
		if err != nil {
			continue
		}

		var tf struct {
			Tasks map[string]any `yaml:"tasks"`
		}

		if err := yaml.Unmarshal(data, &tf); err != nil {
			continue
		}

		if _, ok := tf.Tasks["test"]; ok {
			return true
		}
	}

	return false
}

// npmInitPlaceholder is the scripts.test line `npm init` writes; it is not a real
// test command, so a package.json carrying only it is treated as having none.
const npmInitPlaceholder = `echo "Error: no test specified" && exit 1`

// hasRealNPMTestScript reports whether package.json declares a non-empty
// scripts.test that is not the npm-init placeholder.
func hasRealNPMTestScript(workspace string) bool {
	data, err := readVerifyMarker(filepath.Join(workspace, "package.json"))
	if err != nil {
		return false
	}

	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}

	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}

	test := strings.TrimSpace(pkg.Scripts["test"])

	return test != "" && test != npmInitPlaceholder
}

// verifyMarkerByteCap bounds reads of repo-controlled build-metadata files. A
// committed multi-GB file — or one symlinked to /dev/zero — must not OOM the
// worker before the marker check runs. 1 MiB holds any real marker file.
const verifyMarkerByteCap = 1 << 20

// readVerifyMarker reads at most verifyMarkerByteCap bytes from path, bounding
// the allocation before it happens (os.ReadFile slurps the whole file first).
func readVerifyMarker(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // code-selected workspace marker, not model input
	if err != nil {
		return nil, err
	}

	defer f.Close() //nolint:errcheck // read-only

	return io.ReadAll(io.LimitReader(f, verifyMarkerByteCap))
}

// makefileHasTestTarget reports whether path is a readable Makefile declaring a
// "test:" target. Make targets are declared at column 0, so the match is
// deliberately untrimmed — indented lines (recipes, comments) never match.
func makefileHasTestTarget(path string) bool {
	data, err := readVerifyMarker(path)
	if err != nil {
		return false
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "test:") {
			return true
		}
	}

	return false
}

// fileExists reports whether path is a readable non-directory file.
func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}

// execFileExists reports whether path is a readable, executable, non-directory
// file — the gradlew/mvnw wrapper check.
func execFileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
