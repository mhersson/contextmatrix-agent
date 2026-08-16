// Package verifyexec provides capability probing and classified execution for
// the agent's verify gate. It is deliberately target-language-agnostic: it never
// hard-codes a toolchain. Probing answers "can this command run here?" without
// running it; Exec runs a resolved command with a scrubbed environment and
// reports a structured outcome the orchestrator classifies into pass / fail /
// skipped.
//
// The package is pure enough to table-test: Probe/ProbeShell/ShellArgv/
// FilterEnvNames/LooksToolMissing depend only on the filesystem, PATH, and their
// inputs; Exec is the one subprocess boundary.
package verifyexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-harness/tools"
)

// Outcome is the raw result of running a verify command. ExitCode is -1 when the
// process never ran (StartErr) or was killed by the timeout. The orchestrator's
// classifyVerify turns an Outcome into a tri-state verify result; keeping the raw
// signals here means classification is pure, tested code rather than subprocess
// wiring.
type Outcome struct {
	Output   string // combined stdout+stderr, capped
	ExitCode int    // process exit code; -1 = did not run or was killed
	TimedOut bool   // the command's own timeout fired (inconclusive)
	StartErr bool   // the process could not be started at all
}

// captureCapBytes bounds the combined output retained from a verify run so a
// pathological suite cannot exhaust memory. HeadTail keeps the head and the tail,
// so a leading "command not found" line and a trailing failure summary both
// survive.
const captureCapBytes = 1 << 16 // 64 KiB

// toolMissingPatterns are anchored stderr signatures of a missing tool - the
// backstop for a make/just/task wrapper that shells out to a toolchain binary
// that is not installed and exits non-127. They match the shapes GNU make and
// the POSIX shells actually emit (empirically): the shell may prefix its own
// interpreter path (`/bin/sh:`) and a line-number infix (`1:` / `line 1:`), the
// message casing varies (`command not found` / `Command not found`), and make's
// own summary line reports `Error 127`. Every pattern is anchored to the START
// of a line so a verify run that merely PRINTS a not-found message mid-output
// cannot be misread as a missing tool and reclassify a real failure as skipped.
var toolMissingPatterns = []*regexp.Regexp{
	// make announcing a missing recipe tool: "make: cargo: command not found",
	// "make[1]: mvn: not found", "make: pytest: No such file or directory".
	regexp.MustCompile(`(?mi)^make(\[\d+\])?: .+: (command not found|not found|no such file or directory)`),
	// make's exit-127 summary line: "make: *** [Makefile:3: test] Error 127".
	regexp.MustCompile(`(?m)^make(\[\d+\])?: \*\*\* .* Error 127$`),
	// sh/bash reporting a missing command, with an optional leading interpreter
	// path and an optional line-number infix: "/bin/sh: 1: cargo: not found",
	// "bash: line 1: cargo: command not found".
	regexp.MustCompile(`(?mi)^(/\S+/)?(ba)?sh: ((line )?\d+: )?.+: (command not found|not found)`),
}

// LooksToolMissing reports whether output carries an anchored "tool not found"
// signature - the backstop for a wrapper (make/just/task) that shells out to a
// toolchain binary that is not installed and exits non-127.
func LooksToolMissing(output string) bool {
	for _, re := range toolMissingPatterns {
		if re.MatchString(output) {
			return true
		}
	}

	return false
}

// containerRuntimeUnavailablePatterns are anchored signatures of an unreachable
// container runtime: the worker's verify container deliberately has no Docker
// socket, no Docker CLI, CapDrop: ALL, and runs as uid 1000, so a verify
// command that needs containers (a compose-based make test, a Testcontainers
// suite) fails this way rather than with a genuine defect. Every pattern is
// anchored to the START of a line, exactly like toolMissingPatterns, so a
// verify run whose TEST OUTPUT merely prints one of these strings (an
// assertion on a Docker error message, an indented framework log line) cannot
// be misread as an unreachable runtime and reclassify a real failure as a park.
var containerRuntimeUnavailablePatterns = []*regexp.Regexp{
	// docker CLI with no daemon at the other end of the socket, with or
	// without a "docker: " prefix some wrappers add: "Cannot connect to the
	// Docker daemon at unix:///var/run/docker.sock. Is the docker daemon
	// running?".
	regexp.MustCompile(`(?m)^(docker: )?Cannot connect to the Docker daemon`),
	// Testcontainers exhausting every strategy it knows and finding none:
	// "Could not find a valid Docker environment. Please check configuration."
	regexp.MustCompile(`(?m)^Could not find a valid Docker environment`),
	// docker CLI against an unreachable DOCKER_HOST (the shape newer client
	// versions emit instead of the "Cannot connect" message above): "error
	// during connect: Get \"http://...\": dial unix ... connect: no such
	// file or directory".
	regexp.MustCompile(`(?m)^(docker: )?error during connect:`),
	// make reporting one of the container tools missing specifically - scoped
	// to docker/docker-compose/podman so this does not just duplicate
	// toolMissingPatterns for every binary: "make: docker: command not
	// found", "make[1]: docker-compose: not found".
	regexp.MustCompile(`(?mi)^make(\[\d+\])?: (docker|docker-compose|podman): (command not found|not found|no such file or directory)`),
	// sh/bash reporting the same, with an optional leading interpreter path
	// and line-number infix: "/bin/sh: 1: docker-compose: not found".
	regexp.MustCompile(`(?mi)^(/\S+/)?(ba)?sh: ((line )?\d+: )?(docker|docker-compose|podman): (command not found|not found)`),
}

// LooksContainerRuntimeUnavailable reports whether output carries an anchored
// signature of an unreachable container runtime - the worker's verify
// container has no Docker access by design, so a verify command needing one
// fails this way rather than with a real defect. Distinct from
// LooksToolMissing: that heuristic is only consulted for a detected wrapper
// and covers any toolchain binary, while this one applies regardless of
// wrapper status and is scoped to the container tools specifically.
func LooksContainerRuntimeUnavailable(output string) bool {
	for _, re := range containerRuntimeUnavailablePatterns {
		if re.MatchString(output) {
			return true
		}
	}

	return false
}

// resourceExhaustionSignatures are substrings of process-spawn failures caused
// by container resource pressure (pids/memory limits), not by the code under
// test. Matched case-insensitively anywhere in the combined output.
var resourceExhaustionSignatures = []string{
	"resource temporarily unavailable",
	"cannot allocate memory",
	"too many open files",
}

// LooksResourceExhausted reports whether a verify failure looks environmental -
// the run died of resource pressure rather than a failing check.
func LooksResourceExhausted(output string) bool {
	lower := strings.ToLower(output)

	for _, sig := range resourceExhaustionSignatures {
		if strings.Contains(lower, sig) {
			return true
		}
	}

	return false
}

// Probe reports whether argv's leading program can run in workspace WITHOUT
// running it. A program named by a path (contains a slash, e.g. ./gradlew) must
// exist and be executable relative to workspace; a bare program name must be on
// PATH. JVM build wrappers additionally require a java runtime. Any failure is
// returned as an error naming the missing tool.
func Probe(workspace string, argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}

	return probeToken(workspace, argv[0])
}

// ProbeShell reports whether every command in a shell string cmd can run in
// workspace, mechanically and fail-closed: it splits on the pipeline/list
// operators (| && || ; newline), strips leading FOO=bar env assignments from
// each segment, and probes the first real token. Shell keywords/builtins are
// skipped (they always resolve); tokens carrying a shell expansion ($, backtick,
// (), globs) are skipped as unparseable - the runtime 127 hint is the backstop
// for those. Any first token that resolves to neither a builtin nor a runnable
// program makes ProbeShell fail.
//
// A literal cd/pushd target is tracked across segments so a later path token
// resolves against the directory the command will actually run in: a declared
// "cd backend && ./mvnw test" probes backend/mvnw, not workspace/mvnw. When a
// target cannot be resolved statically (popd, bare cd, cd -, $VAR, ~, flags,
// or a directory that does not exist yet, e.g. created by an earlier mkdir),
// tracking degrades fail-open: path tokens are skipped as unprobeable while
// bare tokens are still PATH-probed, PATH lookup being cwd-independent.
func ProbeShell(workspace, cmd string) error {
	base, known := workspace, true

	for _, seg := range splitShellSegments(cmd) {
		tok := leadToken(seg)

		if tok == "cd" || tok == "pushd" || tok == "popd" {
			base, known = trackDirChange(base, known, tok, seg)

			continue
		}

		if tok == "" || isShellBuiltin(tok) || hasShellExpansion(tok) {
			continue
		}

		if strings.ContainsRune(tok, '/') && !known {
			// resolution dir is statically unknown; runtime 127 is the backstop
			continue
		}

		if err := probeToken(base, tok); err != nil {
			return err
		}
	}

	return nil
}

// trackDirChange applies a cd/pushd/popd segment to the probe's directory
// state. Only a literal target naming an EXISTING directory keeps tracking
// precise; everything else marks the base unknown so later path probes fail
// open instead of probing against the wrong root.
func trackDirChange(base string, known bool, tok, seg string) (string, bool) {
	if tok == "popd" {
		return base, false
	}

	target, ok := dirChangeTarget(seg)
	if !ok || !known {
		return base, false
	}

	p := target
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, target)
	}

	if info, err := os.Stat(p); err != nil || !info.IsDir() {
		return base, false
	}

	return p, true
}

// dirChangeTarget extracts the literal directory argument of a cd/pushd
// segment: exactly one argument after the builtin, no flag dash, no tilde,
// no shell expansion.
func dirChangeTarget(seg string) (string, bool) {
	var fields []string

	for f := range strings.FieldsSeq(seg) {
		if len(fields) == 0 && isEnvAssignment(f) {
			continue
		}

		fields = append(fields, f)
	}

	if len(fields) != 2 {
		return "", false
	}

	t := fields[1]
	if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "~") || hasShellExpansion(t) {
		return "", false
	}

	return t, true
}

// probeToken resolves a single program token relative to workspace.
func probeToken(workspace, tok string) error {
	if strings.ContainsRune(tok, '/') {
		p := tok
		if !filepath.IsAbs(p) {
			p = filepath.Join(workspace, tok)
		}

		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("%s: %w", tok, err)
		}

		if info.IsDir() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("%s: not executable", tok)
		}

		return probeJava(tok)
	}

	if _, err := exec.LookPath(tok); err != nil {
		return fmt.Errorf("%s: %w", tok, err)
	}

	return probeJava(tok)
}

// probeJava requires a java runtime on PATH when tok is a JVM build tool or
// wrapper, so a gradle/maven verify command is not chosen on a JVM-less image.
func probeJava(tok string) error {
	switch filepath.Base(tok) {
	case "gradle", "gradlew", "mvn", "mvnw":
		if _, err := exec.LookPath("java"); err != nil {
			return fmt.Errorf("java: %w", err)
		}
	}

	return nil
}

// ShellArgv wraps a shell command string as an argv. It prefers bash with
// pipefail so a failing stage of a pipeline fails the whole command; when bash is
// absent it degrades to sh (no pipefail).
func ShellArgv(cmd string) []string {
	if _, err := exec.LookPath("bash"); err == nil {
		return []string{"bash", "-c", "set -o pipefail; " + cmd}
	}

	return []string{"sh", "-c", cmd}
}

// Exec runs argv in dir with its OWN timeout context, a scrubbed environment
// (the allowlist plus extraEnv KEY=VALUE entries), and combined output capture.
// This is the CONTAINER path: the agent's verify subprocesses must never inherit
// the model-run environment. An empty argv is a start error, never a vacuous pass.
func Exec(ctx context.Context, dir string, argv []string, timeout time.Duration, extraEnv []string) Outcome {
	return execWithEnv(ctx, dir, argv, timeout, tools.ScrubbedEnv(extraEnv))
}

// ExecInherit runs argv like Exec but with the CALLER's FULL environment. It is
// for the standalone `run --verify` CLI, which runs OUTSIDE the container trust
// boundary and is expected to see the developer's environment (DATABASE_URL and
// the like). The orchestrator/container paths must keep Exec (scrubbed env).
func ExecInherit(ctx context.Context, dir string, argv []string, timeout time.Duration) Outcome {
	return execWithEnv(ctx, dir, argv, timeout, os.Environ())
}

// appendOutput adds extra to captured, separated by a newline when captured is
// non-empty.
func appendOutput(captured, extra string) string {
	if captured == "" {
		return extra
	}

	return captured + "\n" + extra
}

// execWithEnv is the shared core: it runs argv in dir with its own timeout
// context and the given process environment, capturing combined output. It
// disambiguates its own timeout (TimedOut) from a parent-context cancellation:
// on cancellation it returns an ordinary Outcome and the caller detects the abort
// via the parent ctx.
func execWithEnv(ctx context.Context, dir string, argv []string, timeout time.Duration, env []string) Outcome {
	if len(argv) == 0 {
		return Outcome{ExitCode: -1, StartErr: true}
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...) //nolint:gosec // argv is code-resolved or probe-gated, never raw model input
	cmd.Dir = dir
	cmd.Env = env

	out, err := cmd.CombinedOutput()

	o := Outcome{Output: tools.HeadTail(string(out), captureCapBytes)}

	switch {
	case cctx.Err() == context.DeadlineExceeded:
		// The command's own budget elapsed. A parent cancellation surfaces as
		// context.Canceled instead and falls through to the exit-code arms, where
		// the caller's parent-ctx check takes over.
		o.TimedOut = true
		o.ExitCode = -1
	case err == nil:
		o.ExitCode = 0
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			o.ExitCode = ee.ExitCode()
		} else {
			// The process never started, so CombinedOutput captured nothing - the
			// spawn error itself (e.g. "fork/exec ...: resource temporarily
			// unavailable") is the only signal available, and callers like
			// LooksResourceExhausted read it from Output.
			o.StartErr = true
			o.ExitCode = -1
			o.Output = appendOutput(o.Output, err.Error())
		}
	}

	return o
}

// deniedEnvPrefixes and deniedEnvSuffixes name environment variables that must
// never reach a verify subprocess or the model's bash tool even when an
// operator lists them: the agent's own control-plane variables and anything
// that reads like a credential.
var (
	deniedEnvPrefixes = []string{"CM_", "CMX_", "LLM_", "GITHUB_"}
	deniedEnvSuffixes = []string{"_TOKEN", "_KEY", "_SECRET", "_PASSWORD"}
)

var envNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// FilterEnvNames returns the subset of names safe to pass through to a verify
// subprocess or the model's bash tool: a conventional UPPER_SNAKE env name that
// is neither an agent control-plane variable nor a credential-looking one. It
// is an agent-side re-filter of the CM-validated list - the verify command may
// be model-proposed and the bash tool runs arbitrary model commands, so
// re-filtering here is load-bearing defense-in-depth.
func FilterEnvNames(names []string) []string {
	var out []string

	for _, n := range names {
		if !envNameRe.MatchString(n) {
			continue
		}

		if hasAnyPrefix(n, deniedEnvPrefixes) || hasAnySuffix(n, deniedEnvSuffixes) {
			continue
		}

		out = append(out, n)
	}

	return out
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}

	return false
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}

	return false
}

// shellSegmentReplacer collapses the pipeline/list operators to a single NUL so a
// shell command splits into its component commands. "||" and "&&" precede "|" in
// the argument list, so the longer operators win at a given position.
var shellSegmentReplacer = strings.NewReplacer(
	"||", "\x00",
	"&&", "\x00",
	"|", "\x00",
	";", "\x00",
	"\n", "\x00",
)

// splitShellSegments splits a shell command into the segments separated by the
// pipeline/list operators. It does NOT descend into subshells or command
// substitutions - those are handled by skipping expansion tokens in ProbeShell.
func splitShellSegments(cmd string) []string {
	return strings.Split(shellSegmentReplacer.Replace(cmd), "\x00")
}

// leadToken returns the first real program token of a segment: the first field
// after any leading FOO=bar env-assignment prefixes, or "" when the segment is
// blank or only env assignments.
func leadToken(seg string) string {
	for f := range strings.FieldsSeq(seg) {
		if isEnvAssignment(f) {
			continue
		}

		return f
	}

	return ""
}

// isEnvAssignment reports whether f is a leading NAME=value env prefix.
func isEnvAssignment(f string) bool {
	i := strings.IndexByte(f, '=')
	if i <= 0 {
		return false
	}

	return envNameRe.MatchString(f[:i])
}

// shellBuiltins are the keywords and builtins that never name an external
// program, so probing them would spuriously fail. It is a deliberately small,
// common set - an unknown token is treated as a program and probed.
var shellBuiltins = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
	"case": true, "esac": true, "in": true, "select": true, "function": true,
	"cd": true, "pushd": true, "popd": true, "true": true, "false": true,
	"exit": true, "return": true, "set": true, "unset": true, "export": true,
	"source": true, ".": true, ":": true, "test": true, "[": true, "[[": true,
	"echo": true, "printf": true, "read": true, "wait": true, "break": true,
	"continue": true, "local": true, "eval": true, "trap": true, "shift": true,
}

// isShellBuiltin reports whether tok is a shell keyword/builtin (never a program).
func isShellBuiltin(tok string) bool { return shellBuiltins[tok] }

// hasShellExpansion reports whether tok carries a shell metacharacter that makes
// its resolved program name undecidable statically (variable/command
// substitution, subshells, globs). Such tokens are skipped rather than
// false-rejected - the runtime 127 hint is their backstop.
func hasShellExpansion(tok string) bool {
	return strings.ContainsAny(tok, "$`(){}*?<>&")
}
