package runkind

import (
	"fmt"
	"path/filepath"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// analyseTask is the analysis's task. Its slots are filled by analysePrompt.
const analyseTask = `%sYou are analysing a finished run of a card for friction: what made
the run slower, costlier or harder than it needed to be. You have read-only
tools: read, grep, glob, git (status, diff, log, show, branch) and
report_analysis. You cannot change files, run commands or write the card.

Repository: %s, at the base branch; paths are relative to it.
%s
%s

INPUTS - read them first:
- %s: the card file (YAML frontmatter, markdown body).
%s
- %s: the run's whole log section, one event per line.
  It can be very large: search it with grep, and read only ranges of it with
  offset and limit, never the whole file. If read refuses it as too large,
  grep alone.

%s
%s

Report each piece of friction the run shows as a finding of one kind:
%s
Propose no guidance that the repository's root instruction file or the
guidance branch already holds. If the run shows no friction, report no
findings.

When you are done, call report_analysis once. It ends the run.
`

// analysePrompt is the task for one analysis: the grounding, the workspace,
// the work and guidance branches, and where its inputs and the run log are.
func analysePrompt(grounding, workspace, inputs, runLog string, in protocol.AnalyseInput, workBranch, guidance string) string {
	return fmt.Sprintf(analyseTask, grounding, workspace, branchLine(workBranch), guidanceLine(guidance),
		filepath.Join(inputs, "card.md"),
		inputsLine(filepath.Join(inputs, "subtasks"), len(in.Subtasks), "the card's subtasks",
			"The card has no subtasks among the inputs."),
		runLog, stopLine(in.StopCause), verifyLine(in.Verify), findingKindsText)
}

func guidanceLine(branch string) string {
	if branch == "" {
		return "The remote has no guidance branch for this project."
	}

	return fmt.Sprintf("Guidance already proposed for this project, pending or rejected by a human,\nis on the branch fetched as origin/%s: read it with git show or git diff\nagainst HEAD, and do not repeat it.", branch)
}
