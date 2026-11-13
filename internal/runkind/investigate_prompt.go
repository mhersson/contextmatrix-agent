package runkind

import (
	"fmt"
	"path/filepath"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// investigateTask is the investigation's task. Its slots are filled by
// investigatePrompt.
const investigateTask = `%sYou are investigating why an autonomous run of a card stopped. You
have read-only tools: read, grep, glob, git (status, diff, log, show, branch)
and report_investigation. You cannot change files, run commands or write the
card.

Repository: %s, at the base branch; paths are relative to it.
%s

INPUTS - read them first:
- %s: the card file (YAML frontmatter, markdown body).
%s
%s
- %s: the stopped run's whole log section, one event per line.
  It can be very large: search it with grep, and read only ranges of it with
  offset and limit, never the whole file. If read refuses it as too large,
  grep alone. Its end holds the stop and the errors before it.

%s
%s

Find why the run stopped. The diagnosis is for a human: what stopped the run,
the log lines that show it, and why the cause you choose fits. Choose exactly
one cause:
- card: the card is the problem, and a fix to it lets a rerun succeed.
- model: the card is fine; it is rerun unchanged and the agent escalates.
- environment: outside the card and the agent: credentials, network, the
  worker image.
- ecosystem: a bug in ContextMatrix or the agent, or its generic prompts.

Only with cause card, propose a fix. It can hold only these fields:
- section: the content of a "## Sentinel" body section, without its heading
  and with no H2 line: guidance the next run needs.
- verify: the card's verify fields, replaced whole.
  - The command must be the one in force, or one the repository itself
    declares, as plain argv with no shell operators.
  - Environment variable names may only be removed, never added.
  - Drop a check only when it cannot run in the worker at all (it needs a
    container runtime, an external service or a secret), and only when what
    remains still covers every acceptance criterion. Name the dropped check,
    and why, in the diagnosis.
- retire_subtasks: IDs, among the subtask inputs, of subtasks that have not
  started, when the split itself is wrong; the rerun plans a new split, and
  the section can guide it.
Never remove, weaken or loosen an acceptance criterion: the fix has no field
for the card's title or body, and the section must not argue a criterion away.

forward_to: IDs, among the sibling inputs, that the same fix suits without
loosening any of their criteria. Leave it empty when none do.

findings: the friction the run shows, each of one kind:
- guidance: lines for the project's AGENTS.md or CLAUDE.md.
- code: friction in the project's code, as a card: a title and a body.
- card_advice: advice on writing cards like this one.
- project_verify: a proposal for the project's verify setting, for admins.
- hard_card: a note that this card is hard.
- ecosystem: a bug in ContextMatrix or the agent, or its generic prompts, as
  an issue: a title and a body.
Evidence summarises what in the run shows the friction and names no card IDs.

When you are done, call report_investigation once. It ends the run.
`

// investigatePrompt is the task for one investigation: the grounding, the
// workspace and work branch, and where its inputs and the run log are.
func investigatePrompt(grounding, workspace, inputs, runLog string, in protocol.InvestigateInput, branch string) string {
	return fmt.Sprintf(investigateTask, grounding, workspace, branchLine(branch),
		filepath.Join(inputs, "card.md"),
		inputsLine(filepath.Join(inputs, "subtasks"), len(in.Subtasks), "the card's subtasks",
			"The card has no subtasks among the inputs: leave retire_subtasks empty."),
		inputsLine(filepath.Join(inputs, "siblings"), len(in.Siblings), "the not-yet-started cards of the same playbook run",
			"There are no sibling cards: leave forward_to empty."),
		runLog, stopLine(in.StopCause), verifyLine(in.Verify))
}

func branchLine(branch string) string {
	if branch == "" {
		return "The remote has no work branch for this card."
	}

	return fmt.Sprintf("The stopped run's work branch is fetched as origin/%s: compare it with\nHEAD using git log and git diff to see what the run committed.", branch)
}

func inputsLine(dir string, n int, what, none string) string {
	if n == 0 {
		return "- " + none
	}

	return fmt.Sprintf("- %s/: %s, one card file each, named by\n  ID, or by position when a file has none.", dir, what)
}

func stopLine(cause string) string {
	if cause == "" {
		return "The run has no recorded stop cause: ContextMatrix's stall or lost-worker detection ended it."
	}

	return fmt.Sprintf("The run stopped with cause %q.", cause)
}

func verifyLine(v *protocol.VerifyConfig) string {
	if v == nil || strings.TrimSpace(v.Command) == "" {
		return "No verify command is declared for the card or its project."
	}

	return fmt.Sprintf("The verify in force: command %q, timeout %d seconds (0 is the default), env names %q.",
		v.Command, v.TimeoutSeconds, v.Env)
}
