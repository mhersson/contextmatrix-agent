package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
)

// proposeTurnCap bounds the proposal investigation. The model only needs to read
// a few convention files and emit one line, so a tight budget keeps the tier
// cheap; it is min'd with the configured cap so a smaller MaxTurns is never
// raised.
const proposeTurnCap = 8

// maxProposedCommandLen bounds an accepted proposed command. A real test command
// is short; anything longer is a model that misunderstood the task.
const maxProposedCommandLen = 200

// proposeLeadDeny are program names a proposed command may not start with: shell
// no-ops and trivial commands that form a vacuous always-green gate, plus
// read-only inspection commands that run cleanly while verifying nothing.
var proposeLeadDeny = map[string]bool{
	// shell no-ops / trivial commands
	"true": true, "false": true, ":": true, "exit": true, "echo": true,
	"printf": true, "test": true, "[": true, "sleep": true, "cd": true,
	"pwd": true, "ls": true, "cat": true,
	// read-only / inspection commands that exit 0 without running any tests
	"git": true, "find": true, "grep": true, "rg": true, "which": true,
	"whereis": true, "file": true, "stat": true, "head": true, "tail": true,
	"wc": true, "env": true, "date": true,
}

// proposedForbidden are the characters a model-proposed command may not contain.
// A proposed command is executed as PLAIN ARGV - never through a shell - so any
// shell metacharacter (substitution, pipeline, list, redirection, glob, quoting)
// is an arbitrary-code-execution vector from attacker-influenced convention docs
// and rejects the whole proposal. Operator-DECLARED commands keep full shell
// semantics; this restriction is Source==proposed only.
const proposedForbidden = "$`();|&<>{}*?\\\"'\n\r\t"

// verifyProposePrompt asks a read-only model to name the repository's own test
// command. It is deliberately target-language-agnostic: it names no toolchain and
// leans entirely on the repo's declared conventions. The %s slots are the repo
// grounding block and the workspace root.
const verifyProposePrompt = `%sYou are selecting the command an autonomous agent should run to execute this
repository's automated tests, so it can verify its own work before finishing.

Repo root: %s - paths are relative to it.

Use your read-only tools to inspect the repository's OWN convention files - its
build/test configuration, task runner, CI workflow, and contributor docs - and
determine the single command a developer runs from the repo root to execute the
test suite.

Rules:
- Output a SINGLE plain command: one program and its arguments, on ONE line, with
  NO shell operators - no pipes (|), no "&&"/";"/"||", no redirection (< >), no
  command substitution ($(...) or backticks), no globs (* ?), no quoting. A
  command that needs shell operators will be rejected.
- Name the project's own aggregate test command, not an ad-hoc inspection command.
- Base it strictly on what the repository declares; do NOT invent a command.
- If the repository genuinely has no automated tests, output an empty command.

Respond with ONLY a JSON object, no prose:
{"command":"<one-line plain test command, or empty string when there is no test suite>"}
`

// proposeVerify runs ONE read-only model call to propose the repository's verify
// command when nothing was declared or detected. It is budget-gated and
// usage-reported like every model-bearing step. A budget park PROPAGATES (the run
// parks); a transport or selection failure degrades to skip. An accepted command
// is executed BY CODE this run only and is never persisted for mechanical
// re-execution: a future resume re-proposes from scratch, because a card body is
// attacker-writable (GitHub import) and re-running a body-parsed command would be
// prompt-injection into code execution.
func (o *run) proposeVerify(ctx context.Context) (verifyPlan, error) {
	d := o.d
	cfg := d.Cfg

	if d.Registry == nil {
		return verifyPlan{}, nil
	}

	if err := o.ledger.Check(); err != nil {
		return verifyPlan{}, err // budget park propagates
	}

	model := d.Registry.SelectByComplexity(registry.SelectInput{
		Role: registry.RoleReviewer,
		Tier: registry.TierSimple,
	}).Model
	if model == "" {
		return verifyPlan{}, nil
	}

	task := fmt.Sprintf(verifyProposePrompt, o.grounding, cfg.Workspace)

	hc := o.harnessConfig(model)
	hc.MaxTurns = min(cfg.MaxTurns, proposeTurnCap)

	res, dur, err := o.runModelCfg(ctx, d.ReadTools, task, model, hc)

	used := o.spendAndReport(ctx, o.ledger, cfg.CardID, "verify: report propose usage failed", res, model, "verify_propose", dur)

	if err != nil {
		// A budget overspend during the call parks; any other model error (transport,
		// context limit, incapable) degrades to skip - an unproposed gate is safe.
		if isBudgetError(err) {
			return verifyPlan{}, err
		}

		slog.Warn("verify: proposal model run failed; proceeding unverified", "card_id", cfg.CardID, "error", err)

		return verifyPlan{}, nil
	}

	cmd, ok := parseProposedCommand(res.Output)
	if !ok {
		return verifyPlan{}, nil
	}

	// A proposed command is metachar-free and runs as PLAIN ARGV, never a shell -
	// so a substitution/pipeline from attacker-influenced convention docs cannot
	// reach bash -c. Probe argv[0] directly.
	argv, ok := acceptProposedCommand(cmd)
	if !ok {
		return verifyPlan{}, nil
	}

	if verifyexec.Probe(cfg.Workspace, argv) != nil {
		return verifyPlan{}, nil
	}

	d.logCard(ctx, "model-proposed verify command: %s (proposed by %s) - promote it to the project's verify config to make it durable", cmd, used)
	o.recordSection(ctx, "Verify Command", verifyCommandSection(cmd, used))

	return verifyPlan{
		Argv:    argv, // plain argv - executed WITHOUT a shell
		Display: cmd,
		Source:  verifySourceProposed,
		Timeout: o.verifyTimeout(),
		Env:     o.verifyEnv(),
	}, nil
}

// parseProposedCommand extracts the {"command":"..."} JSON from the model output
// (tolerating prose/fences) and returns the trimmed command. An empty command
// (the repo has no test suite) or unparsable output returns ok=false.
func parseProposedCommand(output string) (string, bool) {
	raw, ok := extractJSON(output)
	if !ok {
		return "", false
	}

	var p struct {
		Command string `json:"command"`
	}

	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "", false
	}

	cmd := strings.TrimSpace(p.Command)

	return cmd, cmd != ""
}

// acceptProposedCommand validates a model-proposed command and splits it into a
// plain argv, or returns ok=false to reject it: it rejects anything over the
// length bound, anything carrying a shell metacharacter (so the command can be
// run WITHOUT a shell), and any command whose program is a shell no-op or a
// read-only inspection command. Runnability is verified separately by Probe on
// the returned argv.
func acceptProposedCommand(cmd string) ([]string, bool) {
	if len(cmd) > maxProposedCommandLen || strings.ContainsAny(cmd, proposedForbidden) {
		return nil, false
	}

	fields := strings.Fields(cmd)
	if len(fields) == 0 || proposeLeadDeny[fields[0]] {
		return nil, false
	}

	return fields, true
}

// verifyCommandSection renders the human-facing "## Verify Command" body recorded
// on the card when a command was model-proposed, so a human can see what ran and
// make it durable.
func verifyCommandSection(cmd, model string) string {
	return fmt.Sprintf("## Verify Command\n\nThe verify gate ran `%s`, proposed by `%s` because the "+
		"repository declares no verify command this agent recognises. Add a verify config to the "+
		"project to make this durable.", cmd, model)
}
