package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
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
// command - an operator's declared timeout/env bind a detected or proposed
// command too.
type verifyPlan struct {
	Argv    []string
	Display string // human-facing command string
	Source  verifySource
	Timeout time.Duration
	Env     []string // resolved KEY=VALUE pass-throughs
	Notes   []string // resolution notes (e.g. a declared command that could not run)
	// Wrapper marks a DETECTED test-wrapper command (make/just/task), which masks
	// a missing inner toolchain binary as an ordinary non-127 exit. Only for a
	// wrapper does classifyVerify consult the tool-missing heuristic; every other
	// command keeps the strict 127/start-error signal, so a genuine failure that
	// merely prints a not-found line is never downgraded to skipped.
	Wrapper bool
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

// verifyRetryWait is how long runVerifyPlan waits before its one retry of a
// resource-exhausted verify run, giving the prior run's processes time to be
// reaped. A package var (not a const) so tests can shrink it - see
// subtaskHeartbeatInterval in execute.go for the same pattern.
var verifyRetryWait = 5 * time.Second

// resolvedVerifyPlan returns the run's resolved verify plan, or a zero (skip)
// plan when resolution has not run - so prompt/render helpers can read it
// unconditionally.
func (o *run) resolvedVerifyPlan() verifyPlan {
	if o.verify == nil {
		return verifyPlan{}
	}

	return *o.verify
}

// verifyStatusWord is the lowercase word for a verify status, for activity-log
// lines ("passed" / "failed" / "skipped").
func verifyStatusWord(s verifyStatus) string {
	switch s {
	case verifyPassed:
		return "passed"
	case verifyFailed:
		return "failed"
	default:
		return "skipped"
	}
}

// logVerifyRound records one review round's gate outcome on the card. Without
// it the board carries only the resolution line, so a gate that failed and a
// gate that never ran read identically to a human reviewing the activity log.
// A skip names its reason and says out loud that the round proceeds unverified
// - unless the reason already says so: every skip note classifyVerify actually
// emits already ends in "treated as unverified", and appending the suffix on
// top of one reads as "unverified" and "verify" twice in the same line. Only a
// noteless skip (the "no verify command resolved" case) needs the suffix
// spelled out.
func (o *run) logVerifyRound(ctx context.Context, res verifyResult, round int) {
	msg := fmt.Sprintf("verify %s", verifyStatusWord(res.Status))

	if res.Note != "" {
		msg += " (" + res.Note + ")"
	}

	msg += fmt.Sprintf(" - review round %d", round)

	if res.Status == verifySkipped && res.Note == "" {
		msg += " - proceeding unverified"
	}

	o.d.logCard(ctx, "%s", msg)
}

// verifyDocContext is the advisory verify line handed to the document phase:
// the winner's judged result for a Best-of-N run, else the resolved command the
// review gate will run (the single-solver gate has not run yet at document time).
// It is context only - the document model's prose is not a guaranteed surface.
func (o *run) verifyDocContext() string {
	if o.winner != nil {
		return verifyStatusLine(o.winner.verify, o.resolvedVerifyPlan())
	}

	p := o.resolvedVerifyPlan()
	if len(p.Argv) == 0 {
		return "no verify command resolved for this run"
	}

	return fmt.Sprintf("the change will be verified by `%s` (%s)", p.Display, p.Source)
}

// verifyStatusLine renders the run-level verify status for the PR body and the
// completion note: PASSED/FAILED name the command and source; a non-pass reads as
// NOT VERIFIED with the reason. It is built BY CODE so these surfaces are honest
// regardless of what any model wrote.
func verifyStatusLine(vres verifyResult, plan verifyPlan) string {
	switch vres.Status {
	case verifyPassed:
		if plan.Display != "" {
			return fmt.Sprintf("PASSED - `%s` (%s)", plan.Display, plan.Source)
		}

		return "PASSED"
	case verifyFailed:
		if plan.Display != "" {
			return fmt.Sprintf("FAILED - `%s` (%s)", plan.Display, plan.Source)
		}

		return "FAILED"
	default:
		note := vres.Note
		if note == "" {
			note = "no verify command resolved"
		}

		return "NOT VERIFIED - " + note
	}
}

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
// context cancellation is NOT an outcome here - runVerifyPlan checks ctx.Err()
// and propagates the abort before classifying.
func classifyVerify(plan verifyPlan, out verifyexec.Outcome) verifyResult {
	switch {
	case out.ExitCode == 0 && !out.TimedOut && !out.StartErr:
		return verifyResult{Status: verifyPassed, Output: out.Output}
	case out.TimedOut:
		return verifyResult{
			Status: verifySkipped,
			Output: out.Output,
			Note:   fmt.Sprintf("verify timed out after %s - inconclusive, treated as unverified", plan.Timeout),
		}
	case out.StartErr && verifyexec.LooksResourceExhausted(out.Output):
		// A spawn that died with an exhaustion signature (fork/exec EAGAIN under
		// container pressure) is not a missing tool: the note must not send the
		// operator hunting for a toolchain that is present. Same skipped status,
		// so everything keyed on verifySkipped is unaffected.
		return verifyResult{
			Status: verifySkipped,
			Output: out.Output,
			Note:   "verify could not run - container resource exhaustion, treated as unverified",
		}
	case out.StartErr || out.ExitCode == 127 || (plan.Wrapper && verifyexec.LooksToolMissing(out.Output)):
		// The tool-missing heuristic is consulted ONLY for a detected wrapper, which
		// swallows an inner tool's 127 into a plain exit code. For every other
		// command a genuine failure that merely prints a not-found line stays FAILED.
		return verifyResult{
			Status: verifySkipped,
			Output: out.Output,
			Note:   "verify tool missing - the command could not run, treated as unverified",
		}
	default:
		return verifyResult{Status: verifyFailed, Output: out.Output}
	}
}

// ensureVerify resolves the verify plan for the phase that reached the gate. A
// resolved COMMAND is stable and reused from cache; a prior SKIP is NOT final -
// a phase (e.g. a bootstrap task that adds go.mod and tests) can make a command
// resolvable, so a cached skip is re-resolved on re-entry (cheap: only the
// declared/detection tiers re-run; the proposal fires at most once per run). A
// budget park during resolution propagates and is not cached, so a resumed run
// re-attempts.
func (o *run) ensureVerify(ctx context.Context) (verifyPlan, error) {
	if o.verify != nil && len(o.verify.Argv) > 0 {
		return *o.verify, nil
	}

	firstResolve := o.verify == nil

	p, err := o.resolveVerify(ctx)
	if err != nil {
		// A toolchain park must correct the card body on its way out. An earlier
		// phase may have recorded the UNVERIFIED variant, whose "declare a verify
		// command" remedy is wrong here - a command WAS implicated, it just cannot
		// run in this container - and the worker is about to transition the card
		// to blocked for a human to read. Every other resolution error (budget,
		// cancellation) leaves the section alone: those park without implicating
		// the verify command at all.
		var tme *ToolchainMissingError
		if errors.As(err, &tme) {
			o.recordSection(ctx, "Verify Command", verifyToolchainSection(tme))
		}

		return verifyPlan{}, err
	}

	o.verify = &p

	// Log the first resolution, and any later re-resolve that UPGRADED a prior
	// skip to a real command - but not a skip re-confirmed as a skip each phase.
	if firstResolve || len(p.Argv) > 0 {
		o.logVerifyResolution(ctx, p)
	}

	return p, nil
}

// verifyConfigErrorMarker is the declared-tier subject when the operator's
// CMX_VERIFY could not be read at all (bad JSON, or a clean decode to an
// all-zero config). It is the one declared-tier park where no toolchain is
// implicated, so the card-section and log-line renderers key on it to give
// the right remedy (see verifyToolchainSection and toolchainLogMessage)
// instead of the generic "install the toolchain" one.
const verifyConfigErrorMarker = "CMX_VERIFY (unreadable)"

// containerRuntimeUnavailableMarker is the runtime-tier subject when a verify
// command failed because it needed a container runtime the worker does not
// have: no Docker socket bind, CapDrop: ALL, no-new-privileges, uid 1000, and
// no Docker CLI in the worker image, all by design. Unlike the declared/
// detected tiers above, this is discovered at EXECUTION time - runVerifyPlan
// reading the failed output - not at resolution time, so the card-section and
// log-line renderers key on it for the "no containers here" remedy instead of
// "install the toolchain" (see verifyToolchainSection and toolchainLogMessage).
const containerRuntimeUnavailableMarker = "container runtime (unavailable)"

// resolveVerify runs the resolution ladder: declared command (probed) beats
// repo-convention detection beats a model proposal beats skip. A declared
// command that cannot run does NOT stop resolution right there - it records a
// note and falls through to detection/proposal, so a typo cannot silently drop
// verification before a lower tier gets its chance. A budget park from the
// proposal tier propagates. If nothing resolves, the final fall-through raises
// ToolchainMissingError instead of the silent skip when Tier 1 or Tier 2 - or
// the seeded verify-config error above - implicated a toolchain that never
// became runnable at any tier; otherwise (nothing ever implicated a toolchain,
// e.g. a pure docs repo) it degrades to the skip exactly as before. A canceled
// context at the fall-through is inconclusive rather than a sentinel-worthy
// signal - see the Tier 4 comment.
func (o *run) resolveVerify(ctx context.Context) (verifyPlan, error) {
	cfg := o.d.Cfg
	timeout := o.verifyTimeout()
	env := o.verifyEnv()

	var notes []string

	var (
		declaredFailed bool
		declaredCmd    string
		declaredReason string
	)

	// A CMX_VERIFY we could not read is operator intent we failed to honour, not
	// an absent declaration. Seed the declared-tier failure so the run notes it,
	// still falls through to detection and proposal, and parks at Tier 4 if
	// nothing else resolves.
	if e := cfg.VerifyConfigError; e != "" {
		notes = append(notes, e)

		declaredFailed = true
		declaredCmd = verifyConfigErrorMarker
		declaredReason = e
	}

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

		declaredFailed = true
		declaredCmd = cmd
		declaredReason = err.Error()
	}

	// Tier 2: repo-convention detection. The same walk also tracks the first
	// present-but-unresolved marker/reason for the Tier-4 sentinel below, so
	// the walk runs exactly once on this path (see detectVerifyCommand).
	det := detectVerifyCommand(cfg.Workspace)

	notes = append(notes, det.Notes...)

	if len(det.Argv) > 0 {
		return verifyPlan{
			Argv:    det.Argv,
			Display: det.Display,
			Source:  verifySourceDetected,
			Timeout: timeout,
			Env:     env,
			Notes:   notes, // carry declared-cannot-run and coverage notes onto the plan
			Wrapper: det.Wrapper,
		}, nil
	}

	// Tier 3: model proposal - ONCE per run (a code-executed command, never
	// persisted). A skip re-resolved at a later phase re-runs only the cheap tiers
	// above; it does not fire a second proposal model call.
	if !o.proposeAttempted {
		o.proposeAttempted = true

		proposed, err := o.proposeVerify(ctx)
		if err != nil {
			return verifyPlan{}, err // budget park
		}

		if len(proposed.Argv) > 0 {
			proposed.Notes = notes // carry a declared-cannot-run note onto the resolved plan

			return proposed, nil
		}
	}

	// Tier 4: a declared command or a detected marker implicated a toolchain
	// that never became runnable at any tier - park instead of silently
	// shipping unverified. Prefer the declared command in the error when both
	// implicate a toolchain: it is explicit operator intent. Otherwise nothing
	// implicated a toolchain at all (e.g. a pure docs repo): the gate proceeds
	// unverified, and the resolution log says so.
	//
	// A canceled context is checked FIRST: a canceled Tier 3 model call above
	// degrades to an ordinary "nothing proposed" (proposeVerify swallows any
	// non-budget error, including cancellation, into a clean skip), so by the
	// time execution reaches here Tier 3 never got a real chance to rescue.
	// Reading declaredFailed/det.Marker as proof of a missing toolchain in
	// that case would park on operator-initiated cancellation; propagate the
	// cancellation instead.
	switch {
	case ctx.Err() != nil:
		return verifyPlan{}, ctx.Err()
	case declaredFailed:
		reason := declaredReason

		if det.Marker != "" {
			// Both fired: the declared error outranks detection as the actionable
			// root cause, but det.Reason must not be lost - without it the
			// operator would need a second round trip to learn what detection
			// found too.
			reason = fmt.Sprintf("%s (detection also found %s: %s)", declaredReason, det.Marker, det.Reason)
		}

		return verifyPlan{}, &ToolchainMissingError{Tier: "declared", Subject: declaredCmd, Reason: reason}
	case det.Marker != "":
		return verifyPlan{}, &ToolchainMissingError{Tier: "detected", Subject: det.Marker, Reason: det.Reason}
	default:
		return verifyPlan{Source: verifySourceNone, Timeout: timeout, Env: env, Notes: notes}, nil
	}
}

// runVerifyPlan executes the resolved plan in dir and returns the classified
// result. It is the single capture point: it redacts the output and
// disambiguates a parent-context cancel (returned as an error to propagate the
// abort) from a real verify outcome. An empty plan is a skip, never a run. A
// non-pass whose output looks resource-exhausted (container pressure, not a
// real defect) gets exactly one retry after a short wait; a plain failure is
// never retried, and neither is a run that hit its own timeout. A non-pass
// whose output looks like an unreachable container runtime takes precedence
// over the skipped/failed classification and returns a *ToolchainMissingError
// instead of a verifyResult - see the check below.
func (o *run) runVerifyPlan(ctx context.Context, dir string, plan verifyPlan) (verifyResult, error) {
	if len(plan.Argv) == 0 {
		return verifyResult{Status: verifySkipped, Note: "no verify command resolved"}, nil
	}

	out := o.runVerify(ctx, dir, plan.Argv, plan.Timeout, plan.Env)

	if err := ctx.Err(); err != nil {
		return verifyResult{}, err
	}

	res := classifyVerify(plan, out)

	if res.Status != verifyPassed && !out.TimedOut && verifyexec.LooksResourceExhausted(res.Output) {
		// One retry: spawn failures under container resource pressure are
		// transient once the previous run's processes are reaped. Never retry a
		// plain failure - that is a real defect and rerunning it funds nothing.
		// A timed-out run is excluded even when its partial output carries a
		// signature: retrying would double a run already at the wall-clock
		// ceiling, and a rerun would not fit the same timeout anyway. A genuine
		// defect whose output happens to say e.g. "too many open files" is an
		// accepted false positive here - one bounded duplicate run, and
		// classification is unaffected since a failure stays a failure either way.
		slog.Warn("verify: output looks resource-exhausted; retrying once", "card_id", o.d.Cfg.CardID)

		select {
		case <-ctx.Done():
			return verifyResult{}, ctx.Err()
		case <-time.After(verifyRetryWait):
		}

		out = o.runVerify(ctx, dir, plan.Argv, plan.Timeout, plan.Env)

		if err := ctx.Err(); err != nil {
			return verifyResult{}, err
		}

		res = classifyVerify(plan, out)
	}

	if o.d.Redact != nil {
		res.Output = o.d.Redact(res.Output)
	}

	// A non-pass whose output carries an unreachable-container-runtime
	// signature is neither a code defect (verifyFailed, which would burn a fix
	// round on a coder that cannot do anything about a socket that is not
	// there) nor an ordinary skip (verifySkipped, which would let the card ship
	// with a NOT VERIFIED trailer and no explanation): park instead, with the
	// honest remedy. This check runs BEFORE the classification below is
	// returned to any caller, so it takes precedence over both shapes the
	// classification could have produced - a wrapper masking the daemon's exit
	// as a plain 127 (would classify skipped) and a wrapper with the CLI
	// installed but no daemon (would classify failed) - and both converge on
	// the same park. The command output is deliberately NOT threaded into the
	// error: the park reason is fixed, so it cannot leak whatever the verify
	// command printed.
	if res.Status != verifyPassed && verifyexec.LooksContainerRuntimeUnavailable(res.Output) {
		return verifyResult{}, &ToolchainMissingError{
			Tier:    "runtime",
			Subject: containerRuntimeUnavailableMarker,
			Reason:  "the verify command needs a container runtime, and the worker has none by design",
		}
	}

	// The gate is a subprocess, not a tool call, so nothing else records what it
	// printed: a FAILED gate reaches the card body as review findings, but a
	// PASSED or SKIPPED one would leave no output on any channel. res.Output is
	// already redacted above by the agent's own redactor - which scrubs its own
	// credentials (LLM key, MCP key, git token, mob guest tokens), not arbitrary
	// secrets a chatty build could print via the verify.env passthrough - and
	// already bounded at 64 KiB by verifyexec, so this carries a hard ceiling
	// with no cap of its own. Guarded: Emit is nil on several construction paths
	// and events.Emitter.Emit does not nil-check.
	if o.d.Emit != nil {
		o.d.Emit.Emit(events.Verification, map[string]any{
			// ok collapses to false for both FAILED and SKIPPED; status
			// disambiguates them, so read ok:false alongside status, never alone -
			// a SKIPPED gate is not a defect the way a FAILED one is.
			"ok":      res.Status == verifyPassed,
			"status":  verifyStatusWord(res.Status),
			"command": plan.Display,
			"detail":  res.Output,
		})
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

// verifyEnv resolves the operator's declared env pass-throughs for the gate.
func (o *run) verifyEnv() []string {
	return ResolveVerifyEnv(o.d.Cfg.Verify)
}

// ResolveVerifyEnv resolves the operator's declared env pass-throughs to
// KEY=VALUE entries: it re-filters the names agent-side (the command may be
// model-proposed) and reads each surviving name from the container environment.
// Shared by the gate and the worker's bash-tool construction so the model's
// shell and the verify subprocess resolve the identical set.
func ResolveVerifyEnv(d *DeclaredVerify) []string {
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
		if p.Source == verifySourceProposed {
			// proposeVerify already logged the command and its provenance; keep this
			// resolution line terse to avoid two near-identical lines.
			msg = "verify command resolved via model proposal (see the model-proposed note above)"
		} else {
			msg = fmt.Sprintf("verify command resolved: %s (%s)", p.Display, p.Source)
		}

		if len(p.Notes) > 0 {
			msg += " - " + strings.Join(p.Notes, "; ")
		}
	case len(p.Notes) > 0:
		msg = strings.Join(p.Notes, "; ") + " - no fallback found; work will proceed UNVERIFIED"
	default:
		msg = "no verify command declared, detected, or proposed - work will proceed UNVERIFIED"
	}

	o.d.logCard(ctx, "%s", msg)

	// The card body must carry the same truth as the log: a run whose gate will
	// execute nothing has that fact upserted as a section, and a later upgrade
	// to a real command replaces it (recordSection upserts by heading). A
	// model-proposed command is excluded - proposeVerify records its own
	// section with promote-to-config guidance under the same heading.
	if p.Source != verifySourceProposed {
		o.recordSection(ctx, "Verify Command", verifyResolutionSection(p))
	}
}

// verifyResolutionSection renders the "## Verify Command" card section for a
// code-resolved (or unresolved) verify plan. The UNVERIFIED variant is the
// loud counterpart of the activity-log line: a run whose gate will compile and
// test nothing must say so on the card body, never only in a log line.
func verifyResolutionSection(p verifyPlan) string {
	var s string

	if len(p.Argv) == 0 {
		s = "## Verify Command\n\n**NONE - this run is UNVERIFIED.** No verify command was declared, " +
			"detected, or proposed: the review gate will compile and test nothing. Declare a verify " +
			"command in the project's agent settings to close this gap."
	} else {
		s = fmt.Sprintf("## Verify Command\n\nThe verify gate runs `%s` (%s).", p.Display, p.Source)
	}

	// A bullet per note: markdown folds consecutive single-newline lines into one
	// paragraph, which would run several uncovered-module warnings together into
	// a single unreadable sentence on the card.
	if len(p.Notes) > 0 {
		s += "\n\n- " + strings.Join(p.Notes, "\n- ")
	}

	return s
}

// verifyToolchainSection renders the "## Verify Command" card section for a
// toolchain park. It is the third variant of the same heading (alongside the
// resolved and UNVERIFIED ones), so the upsert replaces whatever a prior phase
// recorded - the card a human opens in `blocked` must name the remedy the park
// actually needs, not the one a stale UNVERIFIED section prescribes.
func verifyToolchainSection(tme *ToolchainMissingError) string {
	headline := "**NONE - this run PARKED: the verify toolchain cannot run here.**"
	remedy := "Install the toolchain in the worker image, or declare a verify command that runs with what the image provides."

	switch tme.Subject {
	case nestedModulesMarker:
		// The nested-module cap is the one park where nothing is missing: detection
		// declined to guess a composed command over a sprawling repo. Telling the
		// operator to install a toolchain would send them after a tool that is
		// already installed and working.
		headline = "**NONE - this run PARKED: no verify command could be resolved.**"
		remedy = "Declare a verify command for the project that covers the modules it should build and test."
	case verifyConfigErrorMarker:
		// The declared verify config itself could not be read: no toolchain is
		// missing, so the install remedy would send the operator rebuilding the
		// worker image for a problem that lives in the project or card config.
		headline = "**NONE - this run PARKED: the declared verify config could not be read.**"
		remedy = "Fix the project or card `verify` block, or remove a hand-set CMX_VERIFY from worker_extra_env."
	case containerRuntimeUnavailableMarker:
		// The verify command needs a container runtime; the worker has none by
		// design (no Docker socket, no Docker CLI, CapDrop: ALL). Neither the
		// install remedy above (there is no toolchain to install) nor the
		// nested-module one applies.
		headline = "**NONE - this run PARKED: the worker has no container runtime.**"
		remedy = "Declare a verify command with no container dependency, or give the worker a reachable " +
			"Docker endpoint through the project's `verify.env`."
	}

	return fmt.Sprintf("## Verify Command\n\n%s\n\n- tier: %s\n- subject: `%s`\n- reason: %s\n\n%s",
		headline, tme.Tier, tme.Subject, tme.Reason, remedy)
}

// ---- repo-convention detection ---------------------------------------------

// detection is the outcome of Tier-2 repo-convention detection: a runnable
// command (Argv non-nil), or the diagnostic Marker/Reason naming the first
// present-but-unresolved toolchain for the Tier-4 sentinel. Notes carry
// human-facing caveats onto the resolved plan (e.g. a nested module the
// detected command does not cover).
type detection struct {
	Argv    []string
	Display string
	Wrapper bool
	Marker  string
	Reason  string
	Notes   []string
}

// detectVerifyCommand best-effort resolves the project's verify command from
// workspace markers, target-language-agnostic. A wrapper (make/just/task test)
// wins first UNLESS the repo declares a toolchain whose tools are all absent -
// then the wrapper would shell out to a missing binary and false-fail, so it is
// skipped. Otherwise the marker table is walked in priority order and the first
// toolchain whose tool actually resolves is used. Returns a detection with nil
// Argv when nothing runnable is found. Wrapper reports whether the returned
// command is a test-wrapper (make/just/task) - the caller uses it to scope the
// tool-missing heuristic to exactly that case.
//
// The SAME walk also tracks Marker/Reason: the first present-but-unresolved
// row's marker and probe-failure reason, for the caller's toolchain-missing
// diagnostic. Both are empty when a command resolved or no recognised marker is
// present at all (e.g. a pure docs repo, which must keep the silent skip). This
// is diagnostic only - it never changes which command Tier 2 resolves - and it
// costs nothing extra: detectRows is walked exactly once either way.
//
// When the root declares no marker at all, a one-level nested scan takes over
// (see verify_nested.go).
func detectVerifyCommand(workspace string) detection {
	if a := detectWrapper(workspace); a != nil {
		return detection{Argv: a, Display: strings.Join(a, " "), Wrapper: true}
	}

	var marker, reason string

	for _, row := range detectRows {
		if !row.present(workspace) {
			continue
		}

		a, r := row.resolve(workspace)
		if a != nil {
			return detection{Argv: a, Display: strings.Join(a, " ")}
		}

		if marker == "" {
			marker, reason = row.marker, r
		}
	}

	if marker == "" {
		// Nothing at the root at all: fall back to a one-level nested scan
		// before giving up (see verify_nested.go). A root marker that failed to
		// resolve keeps its park - nested coverage must not paper over it.
		return detectNested(workspace)
	}

	return detection{Marker: marker, Reason: reason}
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

// detectRow is one recognised toolchain: present reports whether its marker is
// in the workspace; resolve returns the runnable argv (its tool present, JVM
// wrappers with a java runtime) and an empty reason, or a nil argv with the
// probe-failure reason when the marker is there but the tool is not installed.
// marker and a nil-argv reason are diagnostic-only, surfaced by
// detectVerifyCommand's walk to build the toolchain-missing sentinel - they
// never change what Tier 2 resolves.
type detectRow struct {
	present func(workspace string) bool
	resolve func(workspace string) (argv []string, reason string)
	marker  string
}

// detectRows is the marker table in priority order. Detected commands stay argv
// (no shell); the timeout/env from the declared config still bind them.
var detectRows = []detectRow{
	{marker: "go.mod", present: hasFile("go.mod"), resolve: probeArgvReason("go", "test", "./...")},
	{marker: "Cargo.toml", present: hasFile("Cargo.toml"), resolve: probeArgvReason("cargo", "test")},
	{marker: "package.json", present: hasRealNPMTestScript, resolve: probeArgvReason("npm", "test")},
	{marker: "pytest config", present: hasPytestMarker, resolve: resolvePython},
	{marker: "gradle project", present: hasGradleProject, resolve: resolveGradle},
	{marker: "maven project", present: hasMavenProject, resolve: resolveMaven},
	{marker: ".NET project file", present: hasDotnetProject, resolve: probeArgvReason("dotnet", "test")},
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
		if !row.present(workspace) {
			continue
		}

		if argv, _ := row.resolve(workspace); argv != nil {
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

// probeArgvReason returns a resolve-func that yields argv when its program
// probes runnable in the workspace, else a nil argv with the probe-failure
// reason - the single source for both what Tier 2 runs and why a present
// marker did not resolve.
func probeArgvReason(argv ...string) func(workspace string) ([]string, string) {
	return func(workspace string) ([]string, string) {
		if err := verifyexec.Probe(workspace, argv); err != nil {
			return nil, err.Error()
		}

		return argv, ""
	}
}

// resolvePython prefers pytest, falling back to `python3 -m pytest` when pytest
// is not installed but a python runtime is; the reason mirrors the fallback,
// reporting the final probe's failure.
func resolvePython(workspace string) ([]string, string) {
	if verifyexec.Probe(workspace, []string{"pytest", "-q"}) == nil {
		return []string{"pytest", "-q"}, ""
	}

	if err := verifyexec.Probe(workspace, []string{"python3", "-m", "pytest"}); err != nil {
		return nil, err.Error()
	}

	return []string{"python3", "-m", "pytest"}, ""
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
func resolveGradle(workspace string) ([]string, string) {
	if execFileExists(filepath.Join(workspace, "gradlew")) {
		return probeArgvReason("./gradlew", "test")(workspace)
	}

	return probeArgvReason("gradle", "test")(workspace)
}

// hasMavenProject reports a Maven project: an executable mvnw wrapper or a pom.
func hasMavenProject(workspace string) bool {
	return execFileExists(filepath.Join(workspace, "mvnw")) ||
		fileExists(filepath.Join(workspace, "pom.xml"))
}

// resolveMaven prefers the executable wrapper, else system maven; both require a
// java runtime (enforced by Probe).
func resolveMaven(workspace string) ([]string, string) {
	if execFileExists(filepath.Join(workspace, "mvnw")) {
		return probeArgvReason("./mvnw", "-q", "test")(workspace)
	}

	return probeArgvReason("mvn", "-q", "test")(workspace)
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

// hasPytestMarker reports a project that EXPLICITLY configures pytest: a
// pytest.ini, a pyproject.toml declaring a [tool.pytest...] table, or a setup.cfg
// declaring a [tool:pytest] section. A BARE pyproject.toml (poetry/hatch metadata
// with no pytest config) is deliberately NOT a marker - running `pytest -q` on a
// project with no collectable tests exits 5 and reads as a false failure - so it
// falls through to the model-proposal tier, the sanctioned language-aware
// fallback. (The classifier stays command-neutral; no pytest-exit-5 special case.)
func hasPytestMarker(workspace string) bool {
	if fileExists(filepath.Join(workspace, "pytest.ini")) {
		return true
	}

	if data, err := readVerifyMarker(filepath.Join(workspace, "pyproject.toml")); err == nil &&
		strings.Contains(string(data), "[tool.pytest") {
		return true
	}

	if data, err := readVerifyMarker(filepath.Join(workspace, "setup.cfg")); err == nil &&
		strings.Contains(string(data), "[tool:pytest]") {
		return true
	}

	return false
}

// justfileTestRe matches a justfile "test" recipe: a line beginning with the
// recipe name "test", optionally with parameters, ending in the recipe colon.
// The trailing `(?:[^=]|$)` guards against a justfile VARIABLE assignment
// (`test := "..."`), whose `:=` would otherwise be read as a recipe colon and
// send `just test` at a nonexistent recipe.
var justfileTestRe = regexp.MustCompile(`^test([ \t][^:]*)?:(?:[^=]|$)`)

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
// committed multi-GB file - or one symlinked to /dev/zero - must not OOM the
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
// deliberately untrimmed - indented lines (recipes, comments) never match.
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
// file - the gradlew/mvnw wrapper check.
func execFileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// ---- verify-failed finding excerpt -----------------------------------------

// verifyFailBlockWindow bounds how many lines follow a "--- FAIL" marker that
// verifyFailureExcerpt keeps for context - typically the Error:/Messages:
// detail beneath the failing test's name.
const verifyFailBlockWindow = 12

// verifyFailureMarkers are the language-neutral, line-anchored substrings
// (besides "--- FAIL", handled separately below for its context window, and
// the two-part Rust "thread '...' panicked" rule) that flag a line of verify
// output as part of a failure.
var verifyFailureMarkers = []string{
	"FAIL",
	"panic:",
	"FAILED",
	"ERROR",
	"Error:",
	"error[",
	"Traceback",
	"AssertionError",
	"not ok",
	"npm ERR!",
	"failures:",
	"✗",
	"✕",
	"●",
}

// isVerifyFailureLine reports whether trimmed - a line already stripped of
// leading whitespace - starts with a recognised failure marker.
func isVerifyFailureLine(trimmed string) bool {
	for _, m := range verifyFailureMarkers {
		if strings.HasPrefix(trimmed, m) {
			return true
		}
	}

	return strings.HasPrefix(trimmed, "thread '") && strings.Contains(trimmed, "panicked")
}

// isVerifyStopLine reports whether trimmed closes a "--- FAIL" context
// window: the start of a new test result or the suite's own pass/fail
// summary.
func isVerifyStopLine(trimmed string) bool {
	for _, p := range []string{"--- ", "=== ", "FAIL", "ok", "PASS"} {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}

	return false
}

// appendFailBlock writes lines[i] - a "--- FAIL" line - and up to
// verifyFailBlockWindow following lines to excerpt, stopping early at the
// next line that closes the window (see isVerifyStopLine). It returns how
// many lines were consumed, so the caller can skip past them rather than
// re-processing a context line that is itself a marker.
func appendFailBlock(excerpt *strings.Builder, lines []string, i int) int {
	excerpt.WriteString(lines[i])
	excerpt.WriteByte('\n')

	consumed := 1

	for j := i + 1; j < len(lines) && j <= i+verifyFailBlockWindow; j++ {
		if isVerifyStopLine(strings.TrimLeft(lines[j], " \t")) {
			break
		}

		excerpt.WriteString(lines[j])
		excerpt.WriteByte('\n')

		consumed++
	}

	return consumed
}

// verifyFailureExcerpt scans a verify command's captured output for
// language-neutral failure markers and returns the lines around them - a
// noisy suite that keeps logging after the failure leaves a plain
// verifyOutputTail-byte tail ending in "FAIL <pkg>" with no test name at all,
// and the fix coder has to re-run the whole suite just to learn what broke.
// Each matching line is kept, with a "--- FAIL" line also keeping up to
// verifyFailBlockWindow following lines of context. The collected excerpt is
// capped at verifyOutputTail bytes (keeping the first matches), followed by
// the plain tail of the last 1500 bytes. When no marker matches at all, it
// returns lastChars(output, verifyOutputTail) exactly as before.
func verifyFailureExcerpt(output string) string {
	lines := strings.Split(output, "\n")

	var excerpt strings.Builder

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimLeft(lines[i], " \t")

		switch {
		case strings.HasPrefix(trimmed, "--- FAIL"):
			// -1 because the loop's own i++ also advances past the block.
			i += appendFailBlock(&excerpt, lines, i) - 1
		case isVerifyFailureLine(trimmed):
			excerpt.WriteString(lines[i])
			excerpt.WriteByte('\n')
		}

		// Early exit only - a fail block can overshoot; the hard cap is
		// enforced below.
		if excerpt.Len() >= verifyOutputTail {
			break
		}
	}

	if excerpt.Len() == 0 {
		return lastChars(output, verifyOutputTail)
	}

	result := strings.TrimRight(excerpt.String(), "\n")
	if len(result) > verifyOutputTail {
		// Back off to a rune boundary so a multi-byte marker (✗, ✕, ●) at
		// the cut is dropped whole rather than split into invalid UTF-8.
		cut := verifyOutputTail
		for cut > 0 && !utf8.RuneStart(result[cut]) {
			cut--
		}

		result = result[:cut]
	}

	return result + "\n\nOutput tail:\n" + lastChars(output, 1500)
}
