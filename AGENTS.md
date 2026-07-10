# AGENTS.md — ContextMatrix Agent

Orientation for working **on** this codebase: package layout, conventions,
invariants, and commit discipline. For what the project is and how to run it,
see [`README.md`](README.md).

ContextMatrix Agent is a Go agent harness with a configurable LLM endpoint that
runs as a ContextMatrix **task backend**, replacing Claude Code headless as the
in-container agent. One binary, two roles: **`serve`** hosts ContextMatrix
lifecycle webhooks and launches one Docker worker container per card; **`work`**
is the container entrypoint that clones the target repo, claims the card, drives
the harness (HITL or autonomous), then commits, pushes, and reports back. It
edits target repositories but treats ContextMatrix — reached over MCP — as the
source of truth for card state. It is ContextMatrix's task backend; backend
selection lives in ContextMatrix, not here (see the README).

## Channels to ContextMatrix

| Channel            | Direction    | Transport                          | Carries                                                            |
| ------------------ | ------------ | ---------------------------------- | ------------------------------------------------------------------ |
| Lifecycle webhooks | CM → `serve` | HMAC over `contextmatrix-protocol` | trigger / kill / stop-all / message / promote / end-session        |
| Status callbacks   | `serve` → CM | HMAC, `POST /api/agent/status`     | running / completed / failed                                       |
| Card operations    | `work` → CM  | **MCP tools** (`CM_MCP_API_KEY`)   | claim, heartbeat, report_usage, set phase, transition, complete, … |

Card progress runs over **MCP, never raw HTTP** — the rule ContextMatrix
enforces for agents. Before promoting an autonomous card, `serve` also makes one
fail-closed signed GET, `verify-autonomous`, to
`/api/v1/cards/{project}/{cardID}/autonomous`.

## Architecture

```
cmd/contextmatrix-agent/main.go → entrypoint; builds the cobra root command

internal/cli/        → cobra commands: run, serve, work
internal/config/     → koanf config; Config (harness) and ServiceConfig (serve); CMX_* env tags
internal/registry/   → model selector: SelectByComplexity, SelectReviewPanel; priors-only, payload-driven (FromSelection) — agent-side policy, not mechanism

# Autonomous executor — the FSM and its container lifecycle
internal/orchestrator/ → hand-written FSM plan → execute → judge → document → review → integrate → done; phase persistence; git finalize
internal/worker/       → the `work` lifecycle: clone, claim, run the FSM (HITL-gated or autonomous), commit/push, PR; wires the orchestrator deps
internal/executor/     → Executor interface + DockerExecutor; Tracker (concurrency + awaiting-human gate); watchdogs
internal/secrets/      → Source (static env file) + RunCredentials (stages per-run CM-provisioned credentials)
internal/taskskills/   → fetches the {git_remote_url, ref} task-skills pointer from CM and shallow-clones it for read-only mount

# serve plumbing
internal/webhook/    → HTTP server for lifecycle webhooks; HMAC verify; replay cache + message dedup
internal/callback/   → status callbacks to /api/agent/status; VerifyAutonomous (fail-closed)
internal/cmclient/   → MCP client for CM card operations (one agent identity per card)
internal/logbridge/  → worker JSONL events → protocol.LogEntry; fan-out to /logs SSE; redaction; awaiting-human signal
internal/frames/     → stdin control protocol (user_message | promote | end_session)
internal/metrics/    → Prometheus registry + cm_agent_* collectors; NormalizeEndpoint label allowlist

internal/kata/       → embedded throwaway kata fixture used by tests

docker/Dockerfile.worker → multi-target worker image family (agent binary + git/rg/fd/gh baseline; default `full` = Go/Node/Python/Rust toolchains; slim `go-node`/`python`/`rust` variants; pinned + SHA-verified)

# Inner loop — the external github.com/mhersson/contextmatrix-harness module
# (events, llm, tools, harness, redact): the FSM-free loop, the LLM client, the
# jailed tool registry (including the Skill tool), the event stream, and secret
# redaction. This repo depends on it; it takes no dependency on this repo.
```

## Boundary discipline (the load-bearing invariant)

The harness core lives in the standalone `contextmatrix-harness` module; its own
`make deps-gate` keeps the `harness` package importing only `events`/`llm`/
`tools` and the module free of any `contextmatrix-*` dependency. In this repo:

- `internal/orchestrator` imports the harness module (`harness`, `llm`, `tools`,
  `events`) plus `registry` and `cmclient`. It **never** imports `worker`; the
  git and card-ops surfaces are injected as interfaces (`Ops`, `GitOps`,
  `PRCreator`) declared in the orchestrator package.
- `internal/worker` is the only place that wires the full stack together.

If a change tempts you to push orchestration, protocol, or policy down into the
harness module, push the dependency the other way instead — inject it behind an
interface the consumer satisfies.

## Target-language agnosticism (an invariant)

The agent is **language-agnostic with respect to the target project**: prompts,
file detection, commit/staging guards, and repo grounding must carry no
assumption about the target's language or ecosystem, and no hard-coded tool or
directory names (`go build`, `node_modules`, `vendor`, `npm`, …). The
repository's own metadata — its `.gitignore`, its tracked files, its declared
config — is the single source of truth for what to ignore, stage, or read; when
you must exclude or classify a path, ask the repo (git, `.gitignore`, content
inspection), never a built-in ecosystem list.

## Tech stack

Go 1.26+, **cobra** + **koanf** (not viper), the **Docker SDK**
(`github.com/docker/docker`), the **Go MCP SDK**
(`github.com/modelcontextprotocol/go-sdk`) for card ops, and **testify**
(`assert`/`require`). Three rules that are easy to get wrong:

- HMAC is `contextmatrix-protocol`'s job — do not re-implement it locally.
- Git tokens and the LLM endpoint are CM-provisioned per run — the agent
  carries no local credential config and mints no tokens itself.
- The LLM endpoint (OpenAI-compatible `/chat/completions`) is spoken over **raw
  HTTP** behind a narrow `Send`/`SendStream` interface — no SDK in the hot path.

## Coding conventions

### Go

- Everything lives under `internal/` — nothing exported outside the module.
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
(`CMX_GITHUB__AUTH_MODE`). Secrets arrive via env or a mounted file only — never
via flags or committed YAML.

### Documentation

- Document the CURRENT STATE, not changed state: what exists NOW and WHY, not
  how we got here.

## Key domain rules

1. **Orchestrator phases.**
   `plan → execute → judge → document → review → integrate → done`, in
   `phaseOrder`. `judge` picks the Best-of-N winner and is a no-op for normal
   single-solver runs. The current phase is **persisted to the card via MCP**
   before each phase, orthogonal to board state. Persisted phase + an
   incrementally pushed branch give crash-resume: a fresh container re-clones
   and re-enters at the stored phase (a run parked at `judge` re-enters at
   `execute`, since judge state is container-local).
2. **Git workflow.** Commit incrementally (one commit per subtask) and **push
   after every subtask and every review round** — `git commit` alone does not
   survive an ephemeral container. Review fixes land as
   `git commit --fixup=<sha>` targeting the commit that last touched the changed
   files. Integrate runs `RebaseAutosquash` with `GIT_SEQUENCE_EDITOR=true`,
   then `--force-with-lease` guarded by the remote tip recorded before the
   rebase. A rebase conflict falls back to soft-reset-to-merge-base + a single
   squashed recommit. The work branch is `cm/<card-id>` (card ID lowercased);
   the ID is validated against `^[A-Za-z][A-Za-z0-9-]*-[0-9]+$` (PREFIX-NNN)
   before it reaches any refspec.
3. **One container per top-level card.** All subagents — subtask workers and
   reviewers — run in-process inside that one container on one shared workspace.
   Writers run sequentially or on disjoint paths; only the read-only review
   panel fans out in parallel.
4. **Review = 3 specialists.** Correctness, Design & Maintainability, Security &
   Performance — parallel, read-only, behind a spec/test gate that
   short-circuits to the fix loop before spending reviewer tokens; the
   orchestrator synthesizes the report. Loops to the `review_attempts` cap
   (default 3).
5. **Model selection is priors-only.** The planner (a fixed capable model) emits
   a complexity tier per subtask — simple / moderate / complex / critical;
   deterministic code maps the tier to a cost-optimal model per role. The LLM
   never names a model. A candidate must be tool-capable, not blacklisted, fit
   the window, and carry a per-role quality prior clearing the tier bar
   (`DefaultTierBars`: simple 0.65, moderate 0.76, complex 0.82, critical 0.90);
   there is **no measured-capability gate**. An eligible operator favorite wins
   outright; otherwise the most capable candidate within a price headroom
   (default 1.5×) of the cheapest is chosen. Pins override, precedence card pin
   → payload default → serve-config default. Priors, favorites, and the
   blacklist are injected at run start from CM's `SelectionContext` payload
   (`registry.FromSelection`) — nothing is embedded. The blacklist is
   self-learning: a model that proves harness-incapable mid-run is reported back
   (`report_incapable_model`), excluded, and a replacement re-selected.
6. **Context bounds.** Subagent isolation, `--max-turns` caps, and window-aware
   selection bound context growth. By default there is **no compactor**: nearing
   the window emits a `context_limit` event and returns **incomplete** — the
   orchestrator treats it as a failed subtask, never a silent truncation. An
   opt-in in-window compactor exists behind `CMX_COMPACTION_ENABLED` (default
   off; `CMX_COMPACTION_THRESHOLD` 0.85, `CMX_COMPACTION_KEEP_RECENT_TURNS` 6).
7. **Per-card budget.** One cumulative USD ceiling (`CMX_MAX_CARD_COST`, default
   5.0) spans the orchestrator and every subagent. A breach parks the card — WIP
   pushed, card released, failed callback — it does not kill mid-turn.
8. **Secrets.** `serve` stages each run's CM-provisioned credentials into
   `<secrets_dir>/runs/<card_id>/env`, refreshed from ContextMatrix ahead of
   each git-token expiry and bind-mounted read-only at `/run/cm-secrets/env`;
   the per-run dir is torn down with the run. The worker reads the LLM
   endpoint key and `CM_GIT_TOKEN` from it. Tool
   subprocesses get an allowlisted `cmd.Env` (`tools.ScrubbedEnv`) — secrets are
   not inheritable by model-driven commands — and known secret values are
   redacted from events and transcripts.
9. **HITL gates + promote.** HITL cards run the same FSM as autonomous,
   mode-gated on `Config.Interactive`: a brainstorming dialogue for creative
   cards plus plan-approval and review-decision gates that wait on the inbox.
   Autonomous is the same FSM with the gates auto-passed and brainstorming
   skipped. A `promote` frame closes the inbox, so every later gate passes
   through and the run finishes autonomously at the persisted phase.
   Awaiting-human is **live**, not stalled — the idle watchdog suspends for a
   parked gate so a human-blocked container is not reaped.
10. **Task-skills.** Coder, fix-coder, the review panel, and the document phase
    can engage ContextMatrix task-skills (`go-development`, `code-review`, …)
    via the model-driven `Skill` tool (in the external harness module,
    constructed as `tools.NewSkillTool`): it lists the available skills by
    description and loads a chosen `SKILL.md` on demand, filtered to the
    per-card `task_skills` subset. Delivery is config-free on the agent: `serve`
    fetches a `{git_remote_url, ref}` pointer from CM
    (`GET /api/agent/task-skills-source`), shallow-clones it once
    (`internal/taskskills`), and read-only-mounts it at `/run/cm-skills`.
    Engagement is reported over MCP (`cmclient.RecordSkillEngaged` →
    `add_log action=skill_engaged`). Distinct from `workflow-skills` and the MCP
    `get_skill` tool.

## Repo grounding

At run start (`newRun`) the orchestrator discovers the repo's instruction files
(`discoverGrounding`), formats a `REPO GROUNDING` block once (`groundingBlock`),
and caches it on `run.grounding`. All eight model-driven phases — plan,
diagnose, brainstorm, coder, fix, specialist, synthesis, document — inject the
cached block; there is no per-phase re-scan.

Two tiers, so a committed third-party tree can never masquerade as the repo's
own rules:

- **Root doc — injected in full.** The workspace root's `AGENTS.md` (preferred)
  or `CLAUDE.md` (fallback) is read and embedded verbatim, capped at
  `groundingByteCap` (64 KB, excess replaced with a truncation marker), with
  symlinks resolved and confined to the workspace — an out-of-tree or non-regular
  target is dropped, so a poisoned repo cannot smuggle secrets into the prompt.
- **Nested docs — enumerated, never injected.** Nested `AGENTS.md`/`CLAUDE.md`
  files are listed as PATHS only, for the model to read on demand; their content
  is never embedded, so a vendored `vendor/.../CLAUDE.md` cannot pose as the
  repo's own instructions. In a git workspace the listing comes from one
  `git ls-files` (tracked files only, so gitignored and untracked trees are
  structurally excluded); a non-git workspace falls back to a filesystem walk
  that skips dot-directories. Both apply the same post-filters: `AGENTS.md`
  preferred per directory, depth ≤ `groundingMaxDepth` (4), total ≤
  `groundingMaxDocs` (24, `slog.Warn` on overflow), sorted shallow → deep.

Best-effort: a missing, empty, or non-directory workspace yields an empty block
and phases run unchanged — grounding never fails a run.

Deferred: v2 proximity-scoping (the coder sees only the instruction file for its
subtask's subtree) and prompt-caching the block.

## Observability

`serve` exposes Prometheus metrics on a **separate, loopback-only admin
listener** — metrics never ride the public webhook port. `GET /metrics` on
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
| `cm_agent_running_containers`               | gauge     | —                                                                 |
| `cm_agent_callback_retries_total`           | counter   | `endpoint` (`status`/`verify-autonomous`)                         |
| `cm_agent_broadcaster_drops_total`          | counter   | —                                                                 |

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
needed), use `contextmatrix-agent run` — see the README's quick start.

Tests that shell out to `git`/`rg`/`fd` skip when those binaries are absent;
install them locally to exercise the full suite. `go test -race` runs in CI —
keep it clean.

### Uncommitted artifacts

These are gitignored point-in-time records — never commit them: `*-RESULTS.md`,
`capabilities-*.json`, `capabilities-*.md`, `transcripts/`, `eval-out/`,
`.envrc`. Nothing model-related is embedded in the binary: priors, favorites,
and the blacklist all arrive at run start from CM's `SelectionContext` payload
(`registry.FromSelection`), so there is no tracked baseline to keep in sync.

## Mandatory verification before proceeding

Every change is fully tested and verified before the next:

1. `go build ./...` — zero errors.
2. `make test` — no regressions; `make test-race` clean.
3. `make lint` — clean.
4. `gofumpt -l .` — empty.

Fix any failure before moving on.

## Commit discipline

```bash
go fix ./...   # run before every commit
make fmt       # gofumpt -w . — CI flags any gofmt-vs-gofumpt difference
make test      # clean before every commit
make lint      # clean before every commit
make build     # must build
```

**NEVER** commit code without manual approval from the user. No exceptions.

**NEVER** reference a plan phase, slice ID, task number, or a private
ContextMatrix card ID in commit messages, comments, or code — they are
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
