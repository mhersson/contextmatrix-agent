# AGENTS.md - ContextMatrix Agent

Orientation for working **on** this codebase: package layout, conventions,
invariants, and commit discipline. For what the project is and how to run it,
see [`README.md`](README.md).

ContextMatrix Agent is a Go agent harness that runs as ContextMatrix's **task
backend**. One binary, two roles: **`serve`** hosts CM lifecycle webhooks and
launches one Docker worker container per card; **`work`** is the container
entrypoint that clones the target repo, claims the card, drives the harness
(HITL or autonomous), then commits, pushes, and reports back. It edits target
repositories but treats ContextMatrix - reached over MCP - as the source of
truth for card state. Backend selection lives in ContextMatrix, not here.

## Reference documents

Read the relevant one before working in its area:

| Document                  | Read before working on                                                                                          |
| ------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `docs/orchestrator.md`    | The FSM: phases, git workflow, review loop, verify resolution, plan split, HITL gates, budget/context bounds, repo grounding, task skills. |
| `docs/pr-gates.md`        | The `pr_gates` phase: CI gate, Copilot gate, triage verdicts, thread replies.                                    |
| `docs/model-selection.md` | Selection policy: tier bars, favorites, vendor diversity, `max_capability`, fallback precedence.                 |
| `docs/secrets.md`         | Credential staging, env scrubbing, redaction, `verify.env` pass-throughs.                                        |
| `docs/observability.md`   | Prometheus metrics and the admin listener.                                                                       |
| `docs/custom-images.md`   | Custom worker images and `CMX_READ_ONLY_ROOTS`.                                                                  |

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
internal/registry/   → model selector: SelectByComplexity, SelectReviewPanelReport; priors-only, payload-driven (FromSelection) - agent-side policy, not mechanism

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

All git tokens are CM-provisioned per run. Do not read raw tokens from config
or env in new code paths, and do not add local minting back. Staging, refresh,
and redaction mechanics: `docs/secrets.md`.

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

Summaries only - full detail lives in the reference documents above.

1. **Orchestrator phases.**
   `plan → execute → judge → document → review → integrate → pr_gates → done`.
   The current phase is persisted to the card via MCP before each phase;
   persisted phase + an incrementally pushed branch give crash-resume.
2. **Git workflow.** One commit per subtask; push after every subtask and every
   review round. Review fixes are `--fixup` commits; integrate autosquashes and
   force-pushes with lease. Work branch `cm/<card-id>`; the ID is validated
   before it reaches any refspec.
3. **One container per top-level card.** All subagents run in-process on one
   shared workspace; only the read-only review panel fans out in parallel.
4. **Review = 3 specialists** behind a spec/test gate, looping to the
   `review_attempts` cap (default 3, range 1-6). The synthesis verdict is
   severity-gated: approval cannot carry a critical or important finding.
5. **Model selection is priors-only.** The planner emits a complexity tier per
   subtask; deterministic code maps the tier to a model. The LLM never names a
   model; there is no measured-capability gate. Pins always override.
6. **Context bounds.** No compactor by default: nearing the window returns
   incomplete - a failed subtask, never a silent truncation.
7. **Per-card budget.** One cumulative USD ceiling (`CMX_MAX_CARD_COST`) spans
   the orchestrator and every subagent; a breach parks the card, never kills
   mid-turn.
8. **Verify resolution can park as blocked.** The ladder (declared → detected →
   proposed → skip) returns `ToolchainMissingError` when a toolchain is
   implicated but unrunnable; the card transitions to the board's `blocked`
   state. An environmental park, not a model failure.
9. **Secrets.** Per-run CM-provisioned credentials, bind-mounted read-only;
   tool subprocesses get a scrubbed env; every secret a run receives must be
   registered with the host-side log-bridge redactor.
10. **HITL gates + promote.** Same FSM, mode-gated on `Config.Interactive`; a
    `promote` frame closes the inbox so later gates auto-pass. Awaiting-human
    is live, not stalled.
11. **Task-skills.** Model-driven `Skill` tool, filtered to the per-card
    `task_skills` subset; delivery is config-free (CM pointer, cloned once,
    mounted read-only). Distinct from `workflow-skills`.
12. **PR gates.** `pr_gates` gates on `await_ci` / `await_copilot_review`, up
    to 3 fix rounds per gate, then parks in review. Copilot verdicts land on
    the card and (best-effort) on the PR threads.
13. **Plan-time deliverable split.** The planner can emit `followup_cards`
    (independent deliverables only; cap 4, overflow parks) and
    `unreachable_criteria` (review verifies each claim and exempts VERIFIED
    ones from the verdict).
14. **Repo grounding.** The workspace root's `AGENTS.md`/`CLAUDE.md` is
    injected verbatim into every model-driven phase; nested instruction files
    are enumerated as paths only, never embedded.

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
