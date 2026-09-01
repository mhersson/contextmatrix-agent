# Orchestrator

How a card run works: the FSM in `internal/orchestrator`, its git workflow,
the review loop, resource bounds, verify resolution, the plan-time deliverable
split, and the run-start grounding scan. `internal/worker` wires the
orchestrator's dependencies (`Ops`, `GitOps`, `PRCreator`) and owns the
clone/claim/finalize lifecycle around it.

## Phases

`plan → execute → judge → document → review → integrate → pr_gates → done`, in
`phaseOrder`. `judge` picks the Best-of-N winner and is a no-op for normal
single-solver runs. `pr_gates` holds a gated card in review until the PR's
Copilot review is addressed and/or CI is green - full behavior in
[`pr-gates.md`](pr-gates.md).

The current phase is persisted to the card via MCP before each phase,
orthogonal to board state. Persisted phase plus an incrementally pushed branch
give crash-resume: a fresh container re-clones and re-enters at the stored
phase. A run parked at `judge` re-enters at `execute`, since judge state is
container-local.

## Git workflow

The worker commits incrementally (one commit per subtask) and pushes after
every subtask and every review round - `git commit` alone does not survive an
ephemeral container. Review fixes land as `git commit --fixup=<sha>` targeting
the commit that last touched the changed files. Integrate runs
`RebaseAutosquash` with `GIT_SEQUENCE_EDITOR=true`, then `--force-with-lease`
guarded by the remote tip recorded before the rebase. A rebase conflict falls
back to soft-reset-to-merge-base plus a single squashed recommit.

The work branch is `cm/<card-id>` (card ID lowercased); the ID is validated
against `^[A-Za-z][A-Za-z0-9-]*-[0-9]+$` (PREFIX-NNN) before it reaches any
refspec.

## One container per top-level card

All subagents - subtask workers and reviewers - run in-process inside that one
container on one shared workspace. Writers run sequentially or on disjoint
paths; only the read-only review panel fans out in parallel.

## Review

Three specialists - Correctness, Design & Maintainability, Security &
Performance - run in parallel, read-only, behind a spec/test gate that
short-circuits to the fix loop before spending reviewer tokens; the
orchestrator synthesizes the report.

The mob discussion briefing always diffs the full branch against the base
branch; the solo panel does too on round 1 and the authoritative pass, but on
later cheap rounds it narrows to the delta since the last review round's
snapshot, re-widening to the full branch whenever a fix round lands no
commit (an unchanged HEAD would otherwise leave the next round diffing
nothing). Either way, alongside that diff sits a second, separately labeled
block showing what the previous round's fix actually changed
(`fixDeltaBlock`, capped at 32 KiB) - context, not a narrower review scope.
It lets a reviewer tell fix-introduced code from the rest of the delivered
work; the block is empty on round 1, before any fix has committed. A finding
about fix-introduced code needs concrete evidence - a demonstrated
correctness bug, a real vulnerability, a broken or vacuous test, or a missed
acceptance criterion - to earn an Important or Critical severity; without
that evidence it is Minor at most. This evidence bar, and the convergence
rule described below, are worded identically in the solo synthesis prompt
and the mob moderator prompt, so the two review paths cannot drift.

The loop runs to the `review_attempts` cap - default 3, set per deployment via
`review_attempts_cap` in `serve.yaml` or `CMX_REVIEW_ATTEMPTS_CAP`. Valid
range 1-6; 6 is the ceiling because the loop leaves the server's
`review_attempts` counter at cap+1 and CM caps it at 7. Independent of the
cap, the loop also parks when less than 20 minutes of container time remain
before a round would start, since a verify run, a panel, and a fix round need
that much to finish without being killed mid-work; a re-trigger resumes at the
same round.

A verify-red round - the gate fails before any specialist runs - produces no
verdict, so charging it against the attempts cap would spend review budget on
a build failure rather than a critique. Each verify-red round instead extends
the cliff that triggers the authoritative pass by one round, up to
`maxVerifyRedCredit` (3): the cliff sits at `attemptsCap + verifyRedCredit`
rounds, not the bare cap. The server-side `review_attempts` counter still
increments on every round, verify-red rounds included - it is the
resume-stable round numbering and the lifetime ceiling, and both keep
counting regardless of what a round produced.

The credit is itself clamped to `min(maxVerifyRedCredit,
config.MaxReviewAttemptsCap - attemptsCap)` before the loop starts, so it can
never push the authoritative pass's final increments past CM's server-side
ceiling of 7: at the default cap of 3 the clamp is a no-op (`min(3, 6-3) ==
3`), and at the maximum cap of 6 it computes to zero, reproducing the
uncredited cliff exactly.

Synthesis - the model call that reads the specialist findings and emits the
verdict - runs under its own turn cap (`synthesisMaxTurns`, 12, min'd with
the configured base) rather than inheriting the flat per-phase budget, with a
wrap-up nudge at 3 turns remaining (`synthesisWrapUpTurns`) that forces an
emit-now instruction instead of letting the model keep investigating into
the cap. An attempt that still hits the cap is retried once with an explicit
emit-now repair block; a second cap, or a retry that lands but returns
unparseable output, both park the round - the specialist findings that round
already paid for are recorded on the card body under the round's own
`## Review Findings` heading (so a resumed run replaces rather than
duplicates them) before the park. Only a parse failure with no cap anywhere
in the call is fatal to the run.

Cross-round context is bounded, not unbounded. A non-authoritative round
carries forward only the most recently synthesized panel verdict
(`lastPanelFindings`); when no panel verdict has synthesized yet in this run
- round 1, or a run made up so far entirely of verify-red rounds - it falls
back to the most recent round's raw output (`lastFindings`) instead. The
authoritative pass, and the value a resumed run seeds at start (`newRun`
reading the card body before any round runs), both use the
`reviewHistoryWindow` (3) most recently recorded `## Review Findings`
sections concatenated (`recentReviewFindingsHistory`) instead of the full
history - rounds older than the window are redundant, since each recorded
verdict already carries every surviving finding forward, and an unbounded
history is what pushes synthesis past its own turn budget over a
long-running review.

The synthesis verdict is severity-gated in both directions. An approved
verdict cannot carry a critical- or important-severity finding: when one
parses out, `demoteContradictoryApproval` forces `Approved = false` (logged
on the card as an override) so the round routes through the not-approved fix
+ re-review loop instead of the unreviewed post-approval cleanup pass.
Approval carrying only `minor` (cleanup fix pass) or `nit` (report-only)
findings keeps the normal behavior. Its mirror, `promoteConsistentRevise`,
runs the other direction: a revise verdict whose every fix is minor or nit is
forced to `Approved = true`, since minors and nits never block by the
verdict's own rules and looping the review on findings it already calls
non-blocking would never converge; a revise with no fixes at all, or with an
unlabelled severity, is left alone rather than promoted. Both gates run at
`settleVerdict`, the single choke point the solo synthesis path and the mob
moderator path both return through, so neither can drift from the other.

The verdict also carries `prior_findings_resolved`, the synthesizer's own
report of whether every finding under PRIOR FINDINGS is resolved or
withdrawn - false on round 1 and whenever any prior remains open. It never
changes the verdict; it makes a non-converging review legible. A revise
verdict that reports every prior finding resolved logs a convergence line on
the card naming the round; when the authoritative pass parks after such a
round, its park note says the outstanding items are new observations rather
than unaddressed carryover. The signal resets to false before every round's
verify gate runs, so a round that short-circuits on verify-red - producing no
verdict - can never leave a stale true in place from an earlier round.

An approved verdict is durable across parks. On approval - from the cheap
loop or the gated authoritative pass - the orchestrator records a
`## Review Approval` section on the card body before any post-approval cleanup
pass runs. The section binds the verdict to the commit it approved: the branch
HEAD SHA at the moment of approval, the approval summary, and the surviving
fixes. Recording is best-effort and skips when HEAD is unreadable or empty -
the Review Approval section is a fail-open, SHA-bound adoption gate on resume,
distinct from the card body's human-facing record role: a missing or
unparseable section is treated as "no record" and the normal review loop runs,
so the card body is the adoption gate's source of truth for control flow on
resume.

A resume entering the autonomous review path consults the record first. It
adopts the approval only when the current branch HEAD equals the recorded SHA
and the resolved verify gate still passes - a red or erroring verify refuses
adoption, so an approval never bypasses a red gate. Adoption skips the
specialist panel and synthesis, runs the recorded surviving fixes as the usual
non-escalating cleanup pass (skipped when the list is empty or nit-only, and
gated by the same verify-or-discard of the fixup as a live approval), logs
`adopted recorded approval` on the card, then clears the record before the FSM
proceeds to the next phase. Any mismatch - no record, a different HEAD, a red
verify - falls through to the normal review loop unchanged, and only approvals
are persisted: a rejected verdict never produces a record.

In a mob review discussion, the correctness seat's lens is a briefing that
judges the change - and the plan decisions behind it - against the card's
stated requirements, challenging choices the plan made rather than treating
the plan as the spec.

## Context bounds

Subagent isolation, `--max-turns` caps, and window-aware selection bound
context growth. By default there is no compactor: nearing the window emits a
`context_limit` event and returns incomplete - the orchestrator treats it as a
failed subtask, never a silent truncation. An opt-in in-window compactor
exists behind `CMX_COMPACTION_ENABLED` (default off;
`CMX_COMPACTION_THRESHOLD` 0.85, `CMX_COMPACTION_KEEP_RECENT_TURNS` 6).

## Per-card budget

One cumulative USD ceiling (`CMX_MAX_CARD_COST`, default 5.0) spans the
orchestrator and every subagent. A breach parks the card - WIP pushed, card
released, failed callback - it does not kill mid-turn. The ledger's floor is
CM's server-priced card totals, synced from every `report_usage` response -
the ceiling holds even when the gateway reports no per-call cost.

## Verify resolution

The verify-resolution ladder (declared command, then repo-convention
detection, then a model proposal, then skip -
`internal/orchestrator/verify.go`) gives every tier a chance before giving up.

Detection reads the workspace root. When the root resolves no test wrapper
(make/just/task) and declares no marker at all, a one-level nested scan takes
over (`internal/orchestrator/verify_nested.go`) and emits workspace-rooted
scoped commands for first-level modules (`mvn -q -f backend/pom.xml test`,
`npm --prefix frontend test`, `go test -C svc ./...`), composing up to four of
them with `&&`.

If nothing resolves and a declared command failed its probe, or a detected
toolchain marker never resolved to a runnable command at any tier, resolution
returns `ToolchainMissingError` instead of the silent skip. The marker names
where it was found: `maven project` at the root (matching a `pom.xml` or an
`mvnw`-only workspace), `maven project (in backend/)` one level down (where
the nested row needs the pom itself - the emitted command references it), or
`nested modules` when more than four marker-bearing subdirectories make a
composed command a guess rather than a convention.

The silent skip survives when neither walk implicates a toolchain: no root
marker, and nothing the nested table recognizes. That table is deliberately
narrower than the root's - it carries no pytest row, and it skips
dependency/output directories - so a nested Python module falls through to
the model proposal rather than to a park. A pure docs repo still resolves to
skip, unverified.

`execute()` logs the tier, the command or marker, and the probe failure, then
stops the run like the other park sentinels (Budget/Context/MaxTurns). Unlike
those, `mapFSMResult` also transitions the card to the board's `blocked` state
before releasing the claim, so the park is visible on the board, not just in
the log; a project whose `.board.yaml` lacks `in_progress -> blocked` degrades
to the same silent WIP-push-and-release park as the others. This is an
environmental park, not a model failure - no outcome/blacklist reporting.

Every code-resolved outcome - a command, the unverified skip, or the park - is
also upserted as a `## Verify Command` section on the card body, so the card
and the activity log carry the same truth.

## HITL gates and promote

HITL cards run the same FSM as autonomous, mode-gated on `Config.Interactive`:
a brainstorming dialogue for creative cards plus plan-approval and
review-decision gates that wait on the inbox. Autonomous is the same FSM with
the gates auto-passed and brainstorming skipped. A `promote` frame closes the
inbox, so every later gate passes through and the run finishes autonomously at
the persisted phase. Awaiting-human is live, not stalled - the idle watchdog
suspends for a parked gate so a human-blocked container is not reaped.

## Task skills

Coder, fix-coder, the review panel, and the document phase can engage
ContextMatrix task-skills (`go-development`, `code-review`, ...) via the
model-driven `Skill` tool (in the external harness module, constructed as
`tools.NewSkillTool`): it lists the available skills by description and loads
a chosen `SKILL.md` on demand, filtered to the per-card `task_skills` subset.

Delivery is config-free on the agent: `serve` fetches a
`{git_remote_url, ref}` pointer from CM (`GET /api/agent/task-skills-source`),
shallow-clones it once via the backendkit `taskskills.Resolver`, and
read-only-mounts it at `/run/cm-skills`. Engagement is reported over MCP
(`cmclient.RecordSkillEngaged` → `add_log action=skill_engaged`). Distinct
from `workflow-skills` and the MCP `get_skill` tool.

## Plan-time deliverable split and unreachable criteria

The planner's JSON contract adds two optional arrays alongside `card_tier` and
`subtasks`: `followup_cards` and `unreachable_criteria`. Both are omitted from
the response when empty and validated in `parsePlan` (non-empty
title/description, `depends_on` referencing only earlier entries in their own
array) without a size check - the cap is enforced where the plan is consumed,
not there.

### Split trigger

The split trigger is independent deliverables ONLY - the planner recognizes
the card is really MULTIPLE INDEPENDENT deliverables, not slices of one. There
is no subtask-count trigger: a card that decomposes into many subtasks for a
single deliverable stays one plan. When the trigger fires, the planner plans
only the first deliverable as subtasks and emits each extra deliverable as a
`followup_cards` entry: a title plus a SELF-CONTAINED description, since its
future executor runs later in a fresh container holding only the repo, without
this card or this plan.

### Follow-up creation

`createFollowups` creates one TOP-LEVEL card per entry (`CreateTopLevelCard`,
never a subtask), copies the original card's `autonomous` flag onto it
(`SetAutonomous`, re-asserted unconditionally on every resume so a crash
mid-loop still converges), and wires `depends_on`: `depends_on_original`
chains to the card being planned, `depends_on` indices chain to earlier
followup entries, both resolved to real card IDs.

It is resume-safe: a followup whose title already appears (trimmed,
case-insensitive) in a `## Split` section written by an earlier interrupted
run is not recreated - its recorded card ID is reused for the wiring. The
`## Split` section is upserted after each followup resolves (created or
reused), so a mid-loop failure still leaves every card created so far on
record instead of orphaned.

More than `maxFollowupCards` (4) proposed followups parks the run instead of
mutating the board at scale: `SplitOverflowError` joins the other park
sentinels (Budget/Context/MaxTurns/Toolchain/NoModel/VerifyParked) that stop
execute rather than advance to the next phase, logging the count, the cap, and
the proposed titles so a human can re-cut the card without re-running the
planner. This is an overflow guard on the split path, not a trigger of its
own - a card with many subtasks and no independent deliverable never reaches
it.

### Unreachable criteria

`unreachable_criteria` names acceptance criteria the planner judged
unreachable from inside the container - reading an input that does not exist
in the repo, or writing outside it. A criterion whose artifact does not exist
yet but is created inside the repo by the work itself is NOT unreachable.

`recordUnreachable` writes a `## Unreachable Criteria` section on the card
body naming every claim - the section, reaching prompts via the refreshed
description, is what review actually keys its exemption on - plus an
`UNREACHABLE-AC: "<criterion>" - <reason>` add_log line per entry as the human
audit trail, capped at `maxUnreachableLogLines` (10): past the cap only the
first 9 entries get their own line and one summary line covers the rest,
though the section itself always lists every entry regardless.

The coder/fix prompts treat both this section and `## Split` as out of scope
to implement; `verifyFixPrompt` carries neither note - it shows the coder only
the parent card's title, never its description, so a note pointing at sections
it cannot show would be untethered.

Each review specialist verifies every `## Unreachable Criteria` claim against
the repo as part of its normal pass and reports VERIFIED or REFUTED with one
line of evidence (`unreachableVerifyInstruction`, shared by the solo
specialist prompt and the mob `reviewBriefing` so neither review path can
drift). Synthesis - solo and the mob moderator alike, sharing
`unreachableVerdictRule` - excludes VERIFIED entries from the approve/revise
decision (they stay visible to the human but never fail the work), treats a
REFUTED entry as an ordinary unmet criterion that can still block, and
excludes `## Split` scope from the verdict as work that moved to other cards.

The HITL plan-approval gate (`formatPlannedPlan`) renders unreachable
criteria - alongside any follow-up cards - before a human approves the plan,
so approval never hides which criteria review will later exempt.

### Downstream visibility

Both sections land on `o.body` before `createSubtasks` re-derives
`o.taskDescription` at its end (`stripAgentSections(stripMeta(o.body))`, after
`## Plan` and the sizing marker are recorded). `## Split` and
`## Unreachable Criteria` are deliberately absent from `stripAgentSections`'s
stripped headings, so they reach every downstream phase in the SAME run
(execute's coder prompts, the review specialists, both synthesizers), not just
a later resumed one.

## Repo grounding

At run start (`newRun`) the orchestrator discovers the repo's instruction
files (`discoverGrounding`), formats a `REPO GROUNDING` block once
(`groundingBlock`), and caches it on `run.grounding`. All eight model-driven
phases - plan, diagnose, brainstorm, coder, fix, specialist, synthesis,
document - inject the cached block; there is no per-phase re-scan.

Two tiers, so a committed third-party tree can never masquerade as the repo's
own rules:

- **Root doc - injected in full.** The workspace root's `AGENTS.md`
  (preferred) or `CLAUDE.md` (fallback) is read and embedded verbatim, capped
  at `groundingByteCap` (64 KB, excess replaced with a truncation marker),
  with symlinks resolved and confined to the workspace - an out-of-tree or
  non-regular target is dropped, so a poisoned repo cannot smuggle secrets
  into the prompt.
- **Nested docs - enumerated, never injected.** Nested `AGENTS.md`/`CLAUDE.md`
  files are listed as PATHS only, for the model to read on demand; their
  content is never embedded, so a vendored `vendor/.../CLAUDE.md` cannot pose
  as the repo's own instructions. In a git workspace the listing comes from
  one `git ls-files` (tracked files only, so gitignored and untracked trees
  are structurally excluded); a non-git workspace falls back to a filesystem
  walk that skips dot-directories. Both apply the same post-filters:
  `AGENTS.md` preferred per directory, depth ≤ `groundingMaxDepth` (4), total
  ≤ `groundingMaxDocs` (24, `slog.Warn` on overflow), sorted shallow → deep.

Best-effort: a missing, empty, or non-directory workspace yields an empty
block and phases run unchanged - grounding never fails a run.

Deferred: v2 proximity-scoping (the coder sees only the instruction file for
its subtask's subtree) and prompt-caching the block.
