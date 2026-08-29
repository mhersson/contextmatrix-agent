# AGENTS.md - ContextMatrix Agent

Orientation for working **on** this codebase: package layout, conventions,
invariants, and commit discipline. For what the project is and how to run it,
see [`README.md`](README.md).

ContextMatrix Agent is a Go agent harness with a configurable LLM endpoint that
runs as a ContextMatrix **task backend**, replacing Claude Code headless as the
in-container agent. One binary, two roles: **`serve`** hosts ContextMatrix
lifecycle webhooks and launches one Docker worker container per card; **`work`**
is the container entrypoint that clones the target repo, claims the card, drives
the harness (HITL or autonomous), then commits, pushes, and reports back. It
edits target repositories but treats ContextMatrix - reached over MCP - as the
source of truth for card state. It is ContextMatrix's task backend; backend
selection lives in ContextMatrix, not here (see the README).

## Channels to ContextMatrix

| Channel            | Direction    | Transport                          | Carries                                                            |
| ------------------ | ------------ | ---------------------------------- | ------------------------------------------------------------------ |
| Lifecycle webhooks | CM → `serve` | HMAC over `contextmatrix-protocol` | trigger / kill / stop-all / message / promote / end-session        |
| Status callbacks   | `serve` → CM | HMAC, `POST /api/agent/status`     | running / completed / failed                                       |
| Card operations    | `work` → CM  | **MCP tools** (`CM_MCP_API_KEY`)   | claim, heartbeat, report_usage, set phase, transition, complete, … |

Card progress runs over **MCP, never raw HTTP** - the rule ContextMatrix
enforces for agents. Before promoting an autonomous card, `serve` also makes one
fail-closed signed GET, `verify-autonomous`, to
`/api/v1/cards/{project}/{cardID}/autonomous`.

## Architecture

```
cmd/contextmatrix-agent/main.go → entrypoint; builds the cobra root command

internal/cli/        → cobra commands: run, serve, work
internal/config/     → koanf config; Config (harness) and ServiceConfig (serve); CMX_* env tags
internal/registry/   → model selector: SelectByComplexity, SelectReviewPanel; priors-only, payload-driven (FromSelection) - agent-side policy, not mechanism

# Autonomous executor - the FSM and its container lifecycle
internal/orchestrator/ → hand-written FSM plan → execute → judge → document → review → integrate → pr_gates → done; phase persistence; git finalize
internal/mob/          → A2A mob-session engine: seats, moderator, loopback JSON-RPC server, transcripts - discussions for plan/review/execute checkpoints
internal/verifyexec/   → verify-command probing + bounded exec; used by cli run and the orchestrator verify gate
internal/worker/       → the `work` lifecycle: clone, claim, run the FSM (HITL-gated or autonomous), commit/push, PR; wires the orchestrator deps
internal/executor/     → Executor interface + DockerExecutor; Tracker (concurrency + awaiting-human gate); watchdogs
internal/secrets/      → Source (static env file) + RunCredentials (stages per-run CM-provisioned credentials)

# serve plumbing
internal/webhook/    → HTTP server for lifecycle webhooks; embeds backendkit's webhookcore.Core (HMAC auth, drain gate, request metrics, SSE /logs, health/readyz/images) and adds the agent-only lifecycle handlers plus message dedup
internal/callback/   → status callbacks to /api/agent/status; VerifyAutonomous (fail-closed)
internal/cmclient/   → MCP client for CM card operations (one agent identity per card)
internal/filelog/    → per-card raw container-output file logs (<log_dir>/<project>/<card_id>.log; empty log_dir no-ops)
internal/metrics/    → thin wrapper over backendkit's shared metrics bundle (cm_agent_* collectors, NormalizeEndpoint label allowlist); keeps the agent-only callback-retries counter

internal/kata/       → embedded throwaway kata fixture used by tests

docker/Dockerfile.worker → multi-target worker image family (agent binary + git/rg/fd/gh baseline; default `full` = Go/Node/Python/Rust toolchains; slim `go-node`/`python`/`rust` variants; pinned + SHA-verified)

# Inner loop - the external github.com/mhersson/contextmatrix-harness module
# (events, llm, tools, harness, redact): the FSM-free loop, the LLM client, the
# jailed tool registry (including the Skill tool), the event stream, and secret
# redaction. This repo depends on it; it takes no dependency on this repo.

# Shared serve plumbing - the external github.com/mhersson/contextmatrix-backendkit
# module (frames, webhookcore, logbridge, metrics, taskskills): the transport
# core (HMAC auth, drain gate, SSE /logs), the shared metrics bundle, the log
# bridge, and the task-skills resolver, shared with contextmatrix-chat.
```

## Boundary discipline (the load-bearing invariant)

The harness core lives in the standalone `contextmatrix-harness` module; its own
`make deps-gate` keeps the `harness` package importing only `events`/`llm`/
`tools` and the module free of any `contextmatrix-*` dependency. In this repo:

- `internal/orchestrator` imports the harness module (`harness`, `llm`, `tools`,
  `events`) plus `registry`, `cmclient`, `config`, `mob`, and `verifyexec`. It
  **never** imports `worker`; the git and card-ops surfaces are injected as
  interfaces (`Ops`, `GitOps`, `PRCreator`) declared in the orchestrator
  package.
- `internal/worker` is the only place that wires the full stack together.

If a change tempts you to push orchestration, protocol, or policy down into the
harness module, push the dependency the other way instead - inject it behind an
interface the consumer satisfies.

## Target-language agnosticism (an invariant)

The agent is **language-agnostic with respect to the target project**: prompts,
file detection, commit/staging guards, and repo grounding must carry no
assumption about the target's language or ecosystem, and no hard-coded tool or
directory names (`go build`, `node_modules`, `vendor`, `npm`, …). The
repository's own metadata - its `.gitignore`, its tracked files, its declared
config - is the single source of truth for what to ignore, stage, or read; when
you must exclude or classify a path, ask the repo (git, `.gitignore`, content
inspection), never a built-in ecosystem list.

## Tech stack

Go 1.26+, **cobra** + **koanf** (not viper), the **Docker SDK**
(`github.com/docker/docker`), the **Go MCP SDK**
(`github.com/modelcontextprotocol/go-sdk`) for card ops, and **testify**
(`assert`/`require`). Three rules that are easy to get wrong:

- HMAC is `contextmatrix-protocol`'s job - do not re-implement it locally.
- Git tokens and the LLM endpoint are CM-provisioned per run - the agent
  carries no local credential config and mints no tokens itself.
- The LLM endpoint (OpenAI-compatible `/chat/completions`) is spoken over **raw
  HTTP** behind a narrow `Send`/`SendStream` interface - no SDK in the hot path.

## Coding conventions

### Go

- Everything lives under `internal/` - nothing exported outside the module.
- Interfaces belong in the package that **uses** them; the worker provides the
  orchestrator's `Ops`/`GitOps` implementations, for example.
- Wrap errors with `fmt.Errorf("operation: %w", err)`. Never swallow errors.
- `context.Context` is the first parameter of any function that does I/O.
- No global state, no `init()` functions. Dependencies injected via struct
  fields, wired in `cli`/`worker`/`serve`.
- Constructors return concrete types; consumers take interfaces.
- Logging: `log/slog` with structured fields. No `fmt.Println` in production
  paths; in-container events go through the event stream, not stdout.
- Tests sit next to code (`harness.go` → `harness_test.go`), table-driven, with
  `t.Helper()` in helpers and `t.TempDir()` for scratch dirs.
- **Format with `gofumpt -w .` (`make fmt`), not `gofmt`.** CI flags the
  difference.
- **Spell names out.** Use "agent", never "cmr". No abbreviations in config
  keys, code, comments, or commit messages.

### Git credentials

All git tokens are CM-provisioned: ContextMatrix mints them per run and sends
them on the trigger payload (and on the task-skills pointer for the skills
clone); `secrets.RunCredentials` stages and refreshes them in per-run secret
files. Do not read raw tokens from config or env in new code paths, and do not
add local minting back.

### Config

`internal/config` has two structs. The harness `Config` uses precedence
**defaults < file < env < flags**, with pointer-optionals so "unset" is distinct
from a zero value, separate `Defaults()`/`Validate()`, and a `PrintRedacted`
that keeps secrets out of `--print-config`. The serve `ServiceConfig` layers
**defaults < file < env** only (no flags), with value-typed fields. Env keys are
tag-driven under the `CMX_*` prefix; nested keys use `__`
(`CMX_PROVIDER__SORT` → `provider.sort`). Secrets arrive via env or a mounted
file only - never via flags or committed YAML.

### Documentation

- Document the CURRENT STATE, not changed state: what exists NOW and WHY, not
  how we got here.
- Do not write doc comments on simple functions - if what it does is
  straightforward, the code itself is the documentation.
- Never use em-dashes; use hyphens (-).
- Never reference plan phases, task numbers, or private card IDs in doc comments.

## Key domain rules

1. **Orchestrator phases.**
   `plan → execute → judge → document → review → integrate → pr_gates → done`,
   in `phaseOrder`. `judge` picks the Best-of-N winner and is a no-op for
   normal single-solver runs. `pr_gates` holds a gated card in review until
   the PR's Copilot review is addressed and/or CI is green, then transitions
   it to done; it parks the card in review (clean exit, claim released by CM)
   on round exhaustion or wait-deadline. The current phase is **persisted to
   the card via MCP** before each phase, orthogonal to board state. Persisted
   phase + an incrementally pushed branch give crash-resume: a fresh container
   re-clones and re-enters at the stored phase (a run parked at `judge`
   re-enters at `execute`, since judge state is container-local).
2. **Git workflow.** Commit incrementally (one commit per subtask) and **push
   after every subtask and every review round** - `git commit` alone does not
   survive an ephemeral container. Review fixes land as
   `git commit --fixup=<sha>` targeting the commit that last touched the changed
   files. Integrate runs `RebaseAutosquash` with `GIT_SEQUENCE_EDITOR=true`,
   then `--force-with-lease` guarded by the remote tip recorded before the
   rebase. A rebase conflict falls back to soft-reset-to-merge-base + a single
   squashed recommit. The work branch is `cm/<card-id>` (card ID lowercased);
   the ID is validated against `^[A-Za-z][A-Za-z0-9-]*-[0-9]+$` (PREFIX-NNN)
   before it reaches any refspec.
3. **One container per top-level card.** All subagents - subtask workers and
   reviewers - run in-process inside that one container on one shared workspace.
   Writers run sequentially or on disjoint paths; only the read-only review
   panel fans out in parallel.
4. **Review = 3 specialists.** Correctness, Design & Maintainability, Security &
   Performance - parallel, read-only, behind a spec/test gate that
   short-circuits to the fix loop before spending reviewer tokens; the
   orchestrator synthesizes the report. Loops to the `review_attempts` cap -
   default 3, set per deployment via `review_attempts_cap` in `serve.yaml` or
   `CMX_REVIEW_ATTEMPTS_CAP`. Valid range 1-6; 6 is the ceiling because the
   loop leaves the server's `review_attempts` counter at cap+1 and CM caps it
   at 7. Also parks - independent of the cap - when less than 20 minutes of
   container time remain before a round would start, since a verify run, a
   panel, and a fix round need that much to finish without being killed
   mid-work; a re-trigger resumes at the same round.

   The synthesis verdict is severity-gated: an approved verdict cannot carry a
   critical- or important-severity finding. When one parses out, the code
   forces `Approved = false` (logged on the card as an override) so the round
   routes through the not-approved fix + re-review loop instead of the
   unreviewed post-approval cleanup pass. Approval carrying only `minor`
   (cleanup fix pass) or `nit` (report-only) findings keeps today's behavior;
   the rule is also stated in the synthesis prompts so demotions stay rare, but
   the code check is the guard. In a mob review discussion, the correctness
   seat's lens is a briefing that judges the change - and the plan decisions
   behind it - against the card's stated requirements, challenging choices the
   plan made rather than treating the plan as the spec.
5. **Model selection is priors-only.** The planner (a fixed capable model) emits
   a complexity tier per subtask - simple / moderate / complex / critical;
   deterministic code maps the tier to a cost-optimal model per role. The LLM
   never names a model. A candidate must be tool-capable, not blacklisted, fit
   the window, and carry a per-role quality prior clearing the tier bar
   (`DefaultTierBars`: simple 0.65, moderate 0.76, complex 0.82, critical 0.90);
   there is **no measured-capability gate**. An eligible operator favorite wins
   outright; otherwise the most capable candidate within a price headroom
   (default 1.5×) of the cheapest is chosen. Multi-seat picks (review panels,
   mob discussions, Best-of-N) add a soft vendor-diversity preference: each
   seat first considers only vendors not yet seated, with the price band
   re-anchored on that subset - so a diverse seat may cost more than the
   vendor-blind pick; when no unseated-vendor candidate qualifies, the seat is
   picked vendor-blind. Favorites bypass the preference. A per-card
   `max_capability` flag (trigger payload, exposed as `registry.Selection.
   MaxCapability`) overrides both: every pick chooses the most capable
   candidate in the tier regardless of price - it bypasses operator favorites
   and the price band, and keeps the tier bar, blacklist, in-run exclude set,
   window fit, and vendor-diversity preference intact; equal quality still
   tie-breaks to the cheaper model. When no candidate survives - nothing clears
   the tier bar, the pool is empty, or no `SelectionContext` catalog arrives -
   the selector returns the capable default, which resolves with precedence:
   payload (the trigger's `default_model`) first, then the serve-config default
   (`CMX_DEFAULT_MODEL`), then the compiled-in `config.DefaultCapableModel`.
   Pins are consulted separately in the orchestrator and always override the
   catalog path; the fallback precedence is
   card pin → payload default → serve-config default. Priors, favorites, and the
   blacklist are injected at run start from CM's `SelectionContext` payload
   (`registry.FromSelection`) - nothing is embedded. The blacklist is
   self-learning: a model that proves harness-incapable mid-run is reported back
   (`report_incapable_model`), excluded, and a replacement re-selected.
6. **Context bounds.** Subagent isolation, `--max-turns` caps, and window-aware
   selection bound context growth. By default there is **no compactor**: nearing
   the window emits a `context_limit` event and returns **incomplete** - the
   orchestrator treats it as a failed subtask, never a silent truncation. An
   opt-in in-window compactor exists behind `CMX_COMPACTION_ENABLED` (default
   off; `CMX_COMPACTION_THRESHOLD` 0.85, `CMX_COMPACTION_KEEP_RECENT_TURNS` 6).
7. **Per-card budget.** One cumulative USD ceiling (`CMX_MAX_CARD_COST`, default
   5.0) spans the orchestrator and every subagent. A breach parks the card - WIP
   pushed, card released, failed callback - it does not kill mid-turn.
   The ledger's floor is CM's server-priced card totals, synced from every
   `report_usage` response - the ceiling holds even when the gateway reports
   no per-call cost.
8. **Verify resolution can park as blocked.** The verify-resolution ladder
   (declared command, then repo-convention detection, then a model proposal,
   then skip - `internal/orchestrator/verify.go`) gives every tier a chance
   before giving up. Detection reads the workspace root; when the root
   resolves no test wrapper (make/just/task) and declares no marker at all, a
   one-level nested scan takes over
   (`internal/orchestrator/verify_nested.go`) and emits workspace-rooted
   scoped commands for first-level modules (`mvn -q -f backend/pom.xml test`,
   `npm --prefix frontend test`, `go test -C svc ./...`), composing up to four
   of them with `&&`. If nothing resolves and a declared command failed its
   probe, or a detected toolchain marker never resolved to a runnable command
   at any tier, resolution returns `ToolchainMissingError` instead of the
   silent skip. The marker names where it was found: `maven project` at the
   root (matching a `pom.xml` or an `mvnw`-only workspace), `maven project
   (in backend/)` one level down (where the nested row needs the pom itself -
   the emitted command references it), or `nested modules` when more than four
   marker-bearing subdirectories make a composed command a guess rather than a
   convention. The silent skip survives when neither walk implicates a
   toolchain: no root marker, and nothing the nested table recognizes. That
   table is deliberately narrower than the root's - it carries no pytest row,
   and it skips dependency/output directories - so a nested Python module
   falls through to the model proposal rather than to a park. A pure docs
   repo still resolves to skip, unverified, exactly as before.
   `execute()` logs the tier, the command or marker, and the probe failure,
   then stops the run like the other park sentinels
   (Budget/Context/MaxTurns). Unlike those, `mapFSMResult` also transitions
   the card to the board's `blocked` state before releasing the claim, so the
   park is visible on the board, not just in the log; a project whose
   `.board.yaml` lacks `in_progress -> blocked` degrades to the same silent
   WIP-push-and-release park as the others. This is an environmental park, not
   a model failure - no outcome/blacklist reporting. Every code-resolved
   outcome - a command, the unverified skip, or the park - is also upserted as
   a `## Verify Command` section on the card body, so the card and the
   activity log carry the same truth.
9. **Secrets.** `serve` stages each run's CM-provisioned credentials into
   `<secrets_dir>/runs/<project>/<card_id>/env`, refreshed from ContextMatrix ahead of
   each git-token expiry and bind-mounted read-only at `/run/cm-secrets/env`;
   the per-run dir is torn down with the run. The worker reads the LLM
   endpoint key and `CM_GIT_TOKEN` from it. Tool
   subprocesses get an allowlisted `cmd.Env` (`tools.ScrubbedEnv`) - secrets are
   not inheritable by model-driven commands - and known secret values are
   redacted from events and transcripts. The one addition on top of the
   allowlist: operator-declared `verify.env` pass-throughs, resolved by one
   shared routine (`orchestrator.ResolveVerifyEnv`, filtered + read from the
   container env) and appended for both the verify gate and the model's bash
   tool, so the model can reproduce the gate it is told to satisfy. The name
   filter is the only guard: a declared value is readable by model-run
   commands (`env`, `echo $NAME`) and lands unredacted in events and session
   transcripts sent to the LLM provider - operators must not declare names
   whose values embed credentials (e.g. `PGPASSWORD`, a `DATABASE_URL` with
   userinfo).

   The in-worker redactor never sees container stderr, so worker stderr and
   unparsable stdout are masked host-side by the log bridge alone: every
   CM-provisioned secret a run receives MUST be registered with the bridge's
   `logbridge.RedactorRegistry` before launch and unregistered when the run
   ends. Mechanics in the `serve.go` and `admitAndLaunch` doc comments.
10. **HITL gates + promote.** HITL cards run the same FSM as autonomous,
    mode-gated on `Config.Interactive`: a brainstorming dialogue for creative
    cards plus plan-approval and review-decision gates that wait on the inbox.
    Autonomous is the same FSM with the gates auto-passed and brainstorming
    skipped. A `promote` frame closes the inbox, so every later gate passes
    through and the run finishes autonomously at the persisted phase.
    Awaiting-human is **live**, not stalled - the idle watchdog suspends for a
    parked gate so a human-blocked container is not reaped.
11. **Task-skills.** Coder, fix-coder, the review panel, and the document phase
    can engage ContextMatrix task-skills (`go-development`, `code-review`, …)
    via the model-driven `Skill` tool (in the external harness module,
    constructed as `tools.NewSkillTool`): it lists the available skills by
    description and loads a chosen `SKILL.md` on demand, filtered to the
    per-card `task_skills` subset. Delivery is config-free on the agent: `serve`
    fetches a `{git_remote_url, ref}` pointer from CM
    (`GET /api/agent/task-skills-source`), shallow-clones it once via the
    backendkit `taskskills.Resolver`, and read-only-mounts it at
    `/run/cm-skills`. Engagement is reported over MCP
    (`cmclient.RecordSkillEngaged` → `add_log action=skill_engaged`). Distinct
    from `workflow-skills` and the MCP `get_skill` tool.
12. **PR gates.** The `pr_gates` phase gates on whichever combination of
    `await_ci` and `await_copilot_review` the trigger's `TaskContext` carries,
    running the Copilot gate first, then the CI gate. Each spends up to 3 fix
    rounds (`gatesRoundsCap`) on what it finds before parking the card in
    review. Entering the phase writes a `pr_gates: entering ...` line naming
    both flags, `create_pr`, the PR URL, and the effective Copilot/CI/poll
    waits; every gate decision after that - a pass, a skip, a fix round, a
    park - goes out through the same path, a `gate_progress` event plus a
    slog line plus a card log entry, so the run log carries the full decision
    sequence and not just the polls. The CI gate's poll reads `gh pr checks`
    and, when the token cannot read the Checks API (fine-grained PATs on
    private repos), falls back for the rest of the run to
    `gh run list --commit <head-sha>` plus the legacy commit-status API -
    covered by Actions: read and Commit statuses: read. Fallback mode cannot
    see third-party Checks-API-only integrations. A poll failure that repeats
    on every poll - an `unknown flag` from a gh too old for the fallback poll,
    a token refused by the Actions-runs or commit-status API - parks the gate
    at once with the verbatim gh text in the card detail instead of looping to
    the wait deadline; a transient poll failure keeps looping and, if the wait
    runs out, its last error text is the park detail. A fix round is started
    only with enough wait left for the coder run, the push, and a fresh CI
    cycle: at least 5 minutes and, once the gate has watched one cycle settle,
    that cycle plus a 2-minute coder allowance - so a repo whose CI takes
    longer than the floor never burns a coder run it cannot see through. After
    a fix round the gate re-polls the new head before any deadline verdict, so
    a park never re-lists the failures the fix addressed. A gate runs whenever
    a PR URL exists and its flag is set - a stale `pr_url` on a card whose
    `create_pr` was later disabled still gates, on the rule that a PR exists
    so the gate runs on it; a gated card whose PR was never created
    (`create_pr` true, no URL) parks fail-closed instead of completing - but
    first probes the branch for an existing OPEN PR and adopts it (re-reported
    through `report_push`) when found, since an earlier run's `report_push` may
    not have landed, or `gh pr create` may have failed because one already
    exists; no OPEN PR, or a probe failure, still parks. The Copilot gate
    first probes the PR's current head for a review already on it - a
    re-trigger after a park, or a ruleset review that landed while integrate
    was finishing - and triages that instead of requesting one, so a
    re-trigger never pays for a duplicate request and wait. Otherwise it
    reads the PR's pending reviewers through the REST `requested_reviewers`
    endpoint (`gh api`) rather than `gh pr view --json reviewRequests`, whose
    JSON exporter drops Bot-typed reviewers and would never see Copilot; if
    Copilot is not already listed, it requests the review through that same
    REST endpoint, which accepts the bot login directly where
    `gh pr edit --add-reviewer`'s GraphQL resolution cannot. The gate never
    parks on proven unavailability - a 422 "Copilot isn't available for this
    repository" response records the reason verbatim on the card's activity
    log (the only diagnostic channel for an external tester's Copilot setup)
    and lets the gate pass. Any other request failure is not treated as
    unavailability - the gate still enters the wait loop, because a
    repo-automated Copilot review may arrive regardless, and the same holds
    for a re-request that fails after a fix round: it waits for the automatic
    re-review rather than passing the gate unreviewed. A request the API
    accepts with a 2xx is not trusted either: GitHub silently discards the
    request in some setups, so the gate confirms it against the POST's own
    response body (the updated PR object's `requested_reviewers`) and, when
    that comes back empty, against one delayed re-read of the pending
    reviewer list. A request confirmed by neither is treated as dropped: the
    gate waits only a short grace window (5 minutes) for a ruleset-delivered
    review or a late-listed request - a pending request appearing during
    grace upgrades to the full wait - and then passes with a note naming the
    dropped request, instead of burning the full wait and blaming a slow
    review. The re-request after a fix round gets the same
    confirm-or-grace treatment. A confirmed request that never produces a
    review is recorded and passed at the wait deadline, 20 minutes by default
    (`CMX_GATES_COPILOT_WAIT_TIMEOUT_SECONDS`). Every triage round records a
    VALID/INVALID verdict per finding under a `## Copilot Review (Round N)`
    card section, and the same verdict per comment under its
    `### Comments triaged` subsection; a line recorded before verdicts were
    tracked carries none and reads as VALID, the conservative default. Copilot
    re-posts every open comment on each re-review: one carrying only comments
    an earlier round triaged INVALID passes the gate as already triaged,
    unreviewed further, while a comment an earlier round triaged VALID that
    the fix round failed to resolve counts as still open - it is fed back
    through another fix round rather than waved through, bounded by the same
    3-round cap as any other Copilot finding. The verdicts also reach the PR
    itself (`CMX_GATES_COPILOT_THREAD_REPLIES`, on unless set to exactly
    "false"): each triaged comment's thread gets one reply - the dismissal
    reasoning for INVALID, and for VALID the reasoning plus the head the fix
    pushed - a dismissed thread is resolved at once, and a VALID thread is
    resolved only when a re-review stops repeating its comment, so the
    still-open reopen logic keeps working. All of it is best-effort: threads
    are matched by comment id with the card's dedupe digest as the fallback,
    a thread already carrying any reply is never replied to again, and a
    write failure is one card note, never a park. The outcome is kept in its own
    detail line under `## PR Gates`, separate from the CI gate's, so a later
    CI pass never erases it. An addressed Copilot review persists a satisfied
    marker in the `## PR Gates` section, so a re-trigger skips the paid
    re-review and goes straight to the CI gate; the unavailability and
    timeout skips above never write it, so those stay retryable. After the
    enabled gates have run, the phase probes once more for a Copilot review
    that arrived meanwhile - during a CI wait, or after any wait or skip that
    left the gate unreviewed, so a review sitting on the head is never left
    unread; if that triage spends a fix round, the enabled gates run again,
    still bounded by the 3-round cap per gate. The one exception: when the
    Copilot gate recorded proven unavailability this run, the probe is
    skipped - a repo that refused a review cannot have one sitting on the
    head, so the extra probe would only read an empty result.
13. **Plan-time deliverable split and unreachable-criteria valve.** The
    planner's JSON contract adds two optional arrays alongside `card_tier` and
    `subtasks`: `followup_cards` and `unreachable_criteria`. Both are omitted
    from the response when empty and validated in `parsePlan` (non-empty
    title/description, `depends_on` referencing only earlier entries in their
    own array) without a size check - the cap is enforced where the plan is
    consumed, not there.

    The split trigger is independent deliverables ONLY - the planner
    recognizes the card is really MULTIPLE INDEPENDENT deliverables, not
    slices of one. There is no subtask-count trigger: a card that decomposes
    into many subtasks for a single deliverable stays one plan. When the
    trigger fires, the planner plans only the first deliverable as subtasks
    and emits each extra deliverable as a `followup_cards` entry: a title plus
    a SELF-CONTAINED description, since its future executor runs later in a
    fresh container holding only the repo, without this card or this plan.

    `createFollowups` creates one TOP-LEVEL card per entry (`CreateTopLevelCard`,
    never a subtask), copies the original card's `autonomous` flag onto it
    (`SetAutonomous`, re-asserted unconditionally on every resume so a crash
    mid-loop still converges), and wires `depends_on`: `depends_on_original`
    chains to the card being planned, `depends_on` indices chain to earlier
    followup entries, both resolved to real card IDs. It is resume-safe: a
    followup whose title already appears (trimmed, case-insensitive) in a
    `## Split` section written by an earlier interrupted run is not
    recreated - its recorded card ID is reused for the wiring. The `## Split`
    section is upserted after each followup resolves (created or reused), so a
    mid-loop failure still leaves every card created so far on record instead
    of orphaned.

    More than `maxFollowupCards` (4) proposed followups parks the run instead
    of mutating the board at scale: `SplitOverflowError` joins the other park
    sentinels (Budget/Context/MaxTurns/Toolchain/NoModel/VerifyParked) that
    stop execute rather than advance to the next phase, logging the count, the
    cap, and the proposed titles so a human can re-cut the card without
    re-running the planner. This is an overflow guard on the split path, not a
    trigger of its own - a card with many subtasks and no independent
    deliverable never reaches it.

    `unreachable_criteria` names acceptance criteria the planner judged
    unreachable from inside the container - reading an input that does not
    exist in the repo, or writing outside it (a criterion whose artifact does
    not exist yet but is created inside the repo by the work itself is NOT
    unreachable). `recordUnreachable` writes one
    `UNREACHABLE-AC: "<criterion>" - <reason>` add_log line per entry (the
    convention review keys on) plus a `## Unreachable Criteria` section on the
    card body naming the same claims. The coder/fix/verify-fix prompts treat
    both this section and `## Split` as out of scope to implement. Each review
    specialist verifies every `## Unreachable Criteria` claim against the repo
    as part of its normal pass and reports VERIFIED or REFUTED with one line
    of evidence; synthesis - solo and the mob moderator alike, sharing
    `unreachableVerdictRule` - excludes VERIFIED entries from the
    approve/revise decision (they stay visible to the human but never fail the
    work), treats a REFUTED entry as an ordinary unmet criterion that can
    still block, and excludes `## Split` scope from the verdict as work that
    moved to other cards.

    Both sections land on `o.body` before `createSubtasks` re-derives
    `o.taskDescription` at its end (`stripAgentSections(stripMeta(o.body))`,
    after `## Plan` and the sizing marker are recorded) - `## Split` and
    `## Unreachable Criteria` are deliberately absent from
    `stripAgentSections`'s stripped headings, so they reach every downstream
    phase in the SAME run (execute's coder prompts, the review specialists,
    both synthesizers), not just a later resumed one.

## Repo grounding

At run start (`newRun`) the orchestrator discovers the repo's instruction files
(`discoverGrounding`), formats a `REPO GROUNDING` block once (`groundingBlock`),
and caches it on `run.grounding`. All eight model-driven phases - plan,
diagnose, brainstorm, coder, fix, specialist, synthesis, document - inject the
cached block; there is no per-phase re-scan.

Two tiers, so a committed third-party tree can never masquerade as the repo's
own rules:

- **Root doc - injected in full.** The workspace root's `AGENTS.md` (preferred)
  or `CLAUDE.md` (fallback) is read and embedded verbatim, capped at
  `groundingByteCap` (64 KB, excess replaced with a truncation marker), with
  symlinks resolved and confined to the workspace - an out-of-tree or non-regular
  target is dropped, so a poisoned repo cannot smuggle secrets into the prompt.
- **Nested docs - enumerated, never injected.** Nested `AGENTS.md`/`CLAUDE.md`
  files are listed as PATHS only, for the model to read on demand; their content
  is never embedded, so a vendored `vendor/.../CLAUDE.md` cannot pose as the
  repo's own instructions. In a git workspace the listing comes from one
  `git ls-files` (tracked files only, so gitignored and untracked trees are
  structurally excluded); a non-git workspace falls back to a filesystem walk
  that skips dot-directories. Both apply the same post-filters: `AGENTS.md`
  preferred per directory, depth ≤ `groundingMaxDepth` (4), total ≤
  `groundingMaxDocs` (24, `slog.Warn` on overflow), sorted shallow → deep.

Best-effort: a missing, empty, or non-directory workspace yields an empty block
and phases run unchanged - grounding never fails a run.

Deferred: v2 proximity-scoping (the coder sees only the instruction file for its
subtask's subtree) and prompt-caching the block.

## Observability

`serve` exposes Prometheus metrics on a **separate, loopback-only admin
listener** - metrics never ride the public webhook port. `GET /metrics` on
`127.0.0.1:<admin_port>`, HMAC-signed with the same signed-GET scheme as the
webhook routes (sign `METHOD\nURI\nTS.BODY` with the backend `api_key`).
`admin_port: 0` (the default) disables the listener; the public port defaults to
`9092`, a typical admin port is `9093`. Env override: `CMX_ADMIN_PORT`.

Metrics live on a dedicated registry (`internal/metrics`, alongside the standard
`go_*`/`process_*` collectors). Endpoint labels are bounded by an allowlist
(`NormalizeEndpoint`); unknown paths collapse to `other`. No `card_id`/`project`
labels anywhere.

| Metric                                      | Type      | Labels                                                            |
| ------------------------------------------- | --------- | ----------------------------------------------------------------- |
| `cm_agent_webhook_requests_total`           | counter   | `endpoint`, `status`, `code`                                      |
| `cm_agent_webhook_request_duration_seconds` | histogram | `endpoint`                                                        |
| `cm_agent_container_duration_seconds`       | histogram | `outcome` (`success`/`failure`/`timeout`/`killed`/`idle_timeout`) |
| `cm_agent_running_containers`               | gauge     | -                                                                 |
| `cm_agent_callback_retries_total`           | counter   | `endpoint` (`status`/`verify-autonomous`)                         |
| `cm_agent_broadcaster_drops_total`          | counter   | -                                                                 |

Deferred: panic-recovery counting and OTEL tracing.

## Running and testing

```bash
make build          # go build ./... + the contextmatrix-agent binary
make test           # go test ./...
make test-race      # CGO_ENABLED=1 go test -race ./...
make lint           # golangci-lint run
make fmt            # gofumpt -w .
make docker-worker           # build the default (full) worker image
make docker-worker-variants  # build the go-node / python / rust variants
```

To drive the harness standalone against a local workspace (no ContextMatrix
needed), use `contextmatrix-agent run` - see the README's quick start.

Tests that shell out to `git`/`rg`/`fd` skip when those binaries are absent;
install them locally to exercise the full suite. `go test -race` runs in CI -
keep it clean.

### Uncommitted artifacts

These are gitignored point-in-time records - never commit them: `*-RESULTS.md`,
`capabilities-*.json`, `capabilities-*.md`, `transcripts/`, `eval-out/`,
`.envrc`. Nothing model-related is embedded in the binary: priors, favorites,
and the blacklist all arrive at run start from CM's `SelectionContext` payload
(`registry.FromSelection`), so there is no tracked baseline to keep in sync.

## Mandatory verification before proceeding

Every change is fully tested and verified before the next:

1. `go build ./...` - zero errors.
2. `make test` - no regressions; `make test-race` clean.
3. `make lint` - clean.
4. `gofumpt -l .` - empty.

Fix any failure before moving on.

## Commit discipline

```bash
make fmt       # gofumpt -w . - CI flags any gofmt-vs-gofumpt difference
make test      # clean before every commit
make lint      # clean before every commit
make build     # must build
```

**NEVER** commit code without manual approval from the user. No exceptions.

**NEVER** reference a plan phase, slice ID, task number, or a private
ContextMatrix card ID in commit messages, comments, or code - they are
meaningless to outside readers.

**ALWAYS** keep commit messages short, clear, and focused. Use bullet points in
the body to explain the "what" and "why"; avoid long paragraphs.

**ALWAYS** write conventional commit messages with a type, **scope**, and
concise description. For example:

```
feat(orchestrator): persist phase to card for crash-resume
fix(executor): kill idle containers on the watchdog interval
feat(registry): select cost-optimal model by complexity tier
```
