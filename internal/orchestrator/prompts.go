package orchestrator

import "strings"

// planPrompt is the read-only planner's instruction block. It is adapted from
// the create-plan workflow skill's task-decomposition guidance: the same
// rules for splitting work, dependency thinking, and right-sizing apply, but
// the planner has NO card tools — it only reads code (read/grep/glob) and
// emits a strict JSON plan. Card creation happens in code from the parsed JSON.
//
// The trailing %s slots are filled by runPlan: card title, card description,
// an optional resume block (existing subtasks), and an optional repair block
// (the previous parse error). Empty optional blocks collapse to nothing.
const planPrompt = `You are the planning agent for a software task. You have read-only
tools (read, grep, glob) to inspect the codebase. You do NOT create or modify
cards or files — you only read code and output a plan as JSON.

Decompose the task into subtasks following these rules:

- Each subtask must be completable by a single agent in one focused session
  (~2 hours of work or less).
- Each subtask should touch at most 4-5 files. If it touches more, split it.
- Subtasks must be independently verifiable — each one produces a testable
  result. Each subtask includes its own tests; do NOT create separate
  "write tests" subtasks.
- Set depends_on correctly: a subtask that needs another subtask's output
  must declare the dependency. depends_on lists the indices of EARLIER
  subtasks in this array (a subtask may only depend on subtasks that appear
  before it). Index 0 is the first subtask.
- Order subtasks so independent ones can run in parallel. Parallel-eligible
  siblings (same dependency level) MUST touch disjoint files. If two subtasks
  need the same file, merge them or sequence them via depends_on.
- Write clear, specific titles — an agent reading only the title should
  understand the scope.
- Each subtask description must specify concrete actions, the files touched
  ("Files:" line), and acceptance criteria. No placeholders, no "TBD", no
  vague hand-waves like "implement appropriately".
- Do not over-engineer: solve the problem at hand, no speculative
  abstractions or premature generalization.
- Do not include documentation subtasks — documentation is handled
  separately after execution.

Also assign an overall card_tier reflecting the whole task's complexity, and a
per-subtask tier. Tiers: "simple" (mechanical, low-risk), "moderate"
(standard feature work), "complex" (architectural or high-risk).

Read the relevant code first to ground the plan in the real structure, then
respond.

PARENT CARD
Title: %s

Description:
%s
%s%s
Respond with ONLY a JSON object, no prose:
{"card_tier":"simple|moderate|complex",
 "subtasks":[{"title":"...","description":"...","depends_on":[<earlier indices>],"tier":"simple|moderate|complex"}]}
`

// coderPrompt is the per-subtask coder instruction block. The coder runs with
// the FULL write toolset rooted at the shared workspace and implements exactly
// one subtask on the current branch, where prior subtasks' commits are already
// visible. The orchestrator commits and pushes after the run; the coder does
// NOT run git itself — it ends with a single COMMIT line the orchestrator parses
// into the commit message.
//
// The trailing %s slots are filled by runExecute: subtask title, subtask
// description, parent card title, parent card body.
const coderPrompt = `You are the coding agent for one subtask of a larger task. You have the full
write toolset (read, grep, glob, edit, write, bash) rooted at the workspace.
Implement EXACTLY this subtask — nothing from sibling subtasks, nothing
speculative.

Work happens on the current branch. Prior subtasks have already been committed
and their changes are visible in the working tree; build on them, do not redo
them. Do NOT run git yourself (no commit, no push, no branch) — the orchestrator
commits and pushes your changes after you finish.

Write tests alongside the code and run them. When the subtask is complete, end
your FINAL message with exactly one line of the form:

COMMIT: <conventional commit message>

for example:

COMMIT: feat(api): add health endpoint

The COMMIT line must be a single line, a real conventional-commit summary for the
change you made, and the LAST line of your message.

SUBTASK
Title: %s

Description:
%s

PARENT CARD (context only — implement the subtask, not the whole parent)
Title: %s

Description:
%s
`

// resumeBlock renders the existing-subtask reuse instruction inserted into the
// planner prompt on resume. titles is the list of existing subtask titles.
func resumeBlock(titles []string) string {
	if len(titles) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("\nEXISTING SUBTASKS (a previous planning pass created these — reuse them by\n" +
		"keeping the SAME titles where the work still applies; do not duplicate):\n")

	for _, t := range titles {
		b.WriteString("- ")
		b.WriteString(t)
		b.WriteString("\n")
	}

	return b.String()
}

// repairBlock renders the parse-error feedback inserted into the planner prompt
// on the single repair turn. parseErr is the error from the failed parse.
func repairBlock(parseErr string) string {
	if parseErr == "" {
		return ""
	}

	return "\nYOUR PREVIOUS RESPONSE COULD NOT BE PARSED: " + parseErr + "\n" +
		"Respond again with ONLY the JSON object described below — no prose, no code fences.\n"
}
