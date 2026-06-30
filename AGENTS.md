# AGENTS.md — ContextMatrix Agent

Orientation for anyone — human or agent — working on this codebase: layout,
build/test commands, conventions, invariants, and commit discipline.

## What is this project?

ContextMatrix Agent is a custom Go agent harness with a configurable LLM
endpoint, that runs as a ContextMatrix **task backend**. It replaces Claude Code headless as
the in-container agent. A single binary plays two runtime roles:

- **`serve`** — a long-running host service that hosts ContextMatrix lifecycle
  webhooks and launches one Docker worker container per card.
- **`work`** — the container entrypoint (the image `ENTRYPOINT`). One process
  per card: it clones the target repo, claims the card, and drives the harness —
  HITL (human-steered) or autonomous (the orchestrator FSM) — then commits,
  pushes, and reports back.

This service implements ContextMatrix's `TaskBackend` contract and coexists with
`contextmatrix-runner`. The runner stays the one-flip global fallback. This repo
is a **coordination-aware executor**: it touches target code repositories
(clone, edit, commit, push) but treats ContextMatrix as the source of truth for
card state, reached over MCP.

## v1 parity and backend selection

This agent is at **v1 parity** with `contextmatrix-runner` — a co-equal,
operator-selectable task backend. Selection is global and lives in
ContextMatrix, not here: CM's `backends` config picks the active task backend
(`runner` if enabled, else `agent`), read once at CM startup. To run cards
through this agent, the operator sets
`backends.agent.{url, api_key, enabled: true}` and
`backends.runner.enabled: false` in CM (matching HMAC keys), then restarts CM.
The runner remains the default; the agent is opt-in. Per-project routing is not
supported — selection is instance-wide. CM's `docs/agent-backend-parity.md`
carries the full parity matrix.

## Two channels to ContextMatrix

| Channel            | Direction    | Transport                          | Used for                                                           |
| ------------------ | ------------ | ---------------------------------- | ------------------------------------------------------------------ |
| Lifecycle webhooks | CM → `serve` | HMAC over `contextmatrix-protocol` | trigger / kill / stop-all / message / promote / end-session        |
| Status callbacks   | `serve` → CM | HMAC, `/api/agent/*`               | running / completed / failed; verify-autonomous before promote     |
| Card operations    | `work` → CM  | **MCP tools** (`CM_MCP_API_KEY`)   | claim, heartbeat, report_usage, set phase, transition, complete, … |

Card progress runs over **MCP**, never raw HTTP — the same rule ContextMatrix
itself enforces for agents. The lifecycle channel is the only thing the webhook
protocol carries.

## Architecture

```
cmd/contextmatrix-agent/main.go → entrypoint; builds the cobra root command

internal/cli/        → cobra commands: run, serve, work
internal/config/     → cobra + koanf config; Config (harness) and ServiceConfig (serve); CMX_* env tags

# Inner loop — now the external github.com/mhersson/contextmatrix-harness module
# (events, llm, tools, harness, redact): the FSM-free loop, the LLM
# client, the jailed tool registry, the event stream, and secret redaction.
internal/registry/   → model selector: Resolve(actor, role), SelectByComplexity, SelectReviewPanel; priors-only, payload-driven (FromSelection) — stays agent-side (policy, not mechanism)

# Autonomous executor — the FSM and its container lifecycle
internal/orchestrator/ → light hand-FSM plan → execute → document → review → integrate → done; phase persistence; git finalize
internal/worker/       → the `work` lifecycle: clone, claim, run the FSM (HITL-gated or autonomous), commit/push, PR; wires orchestrator.Deps
internal/executor/     → Executor interface + DockerExecutor; Tracker (concurrency gate); watchdogs
internal/secrets/      → SecretSource (static env file) + Refresher (mints GitHub tokens via githubauth)

# serve plumbing
internal/webhook/    → HTTP server for lifecycle webhooks; HMAC verify; replay cache + message dedup
internal/callback/   → status callbacks to /api/agent/*; VerifyAutonomous (fail-closed)
internal/cmclient/   → MCP client to CM card operations (one agent identity per card)
internal/logbridge/  → worker JSONL events → protocol.LogEntry, fan-out to /logs SSE, redaction, awaiting-human signal
internal/frames/     → stdin control protocol (user_message | promote | end_session)

# Quality
internal/kata/       → embedded throwaway kata fixture used by tests

docker/Dockerfile.worker → the worker image (agent binary + git/rg/fd/gh/node/Go toolchain, pinned + SHA-verified)
```

## Boundary discipline (the load-bearing invariant)

The harness core now lives in the standalone `contextmatrix-harness` module. Its
invariant is enforced **there** by `scripts/deps-gate.sh` (`make deps-gate` in
that repo): the `harness` package imports only `events`/`llm`/`tools`, and the
module takes no `contextmatrix-*` dependency. Verified import rules in this repo:

- `internal/orchestrator` imports the harness module (`harness`, `llm`, `tools`,
  `events`), plus `registry` and `cmclient` (for the `TaskContext` type). It
  **never** imports `worker`; the git and card-ops surfaces are injected as
  interfaces (`Ops`, `GitOps`, `PRCreator`).
- `internal/worker` is the only place that wires the full stack together.

If a change tempts you to push orchestration, protocol, or policy down into the
harness module, push the dependency the other way instead — inject it behind an
interface the consumer satisfies.

## Tech stack

- **Go 1.26+** — backend.
- **cobra** (commands/flags) + **koanf** (config) — not viper.
- **Docker SDK** (`github.com/docker/docker`) — worker container lifecycle.
- **`contextmatrix-protocol`** — shared HMAC wire contract (DTOs + signer +
  validation). Do not re-implement HMAC locally.
- **`contextmatrix-githubauth`** — the only path to GitHub tokens (App + PAT).
- **Go MCP SDK** (`github.com/modelcontextprotocol/go-sdk`) — the CM card-ops
  client.
- **LLM endpoint** (OpenAI-compatible `/chat/completions`), spoken over
  **raw HTTP** behind a narrow `Send`/`SendStream` interface — no SDK in the hot
  path.
- **testify** — assertions (`assert`) and fatal checks (`require`).

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
  paths; in-container events go through the event stream, not stdout printing.
- Tests sit next to code (`harness.go` → `harness_test.go`), table-driven, with
  `t.Helper()` in helpers and `t.TempDir()` for scratch dirs.
- **Format with `gofumpt -w .` (`make fmt`), not `gofmt`.** CI flags the
  difference.
- **Spell names out.** Use "runner" and "agent", never "cmr". No abbreviations
  in config keys, code, comments, or commit messages.

### GitHub auth

All GitHub tokens come from `githubauth` providers (App or PAT) via the
`secrets.Refresher`. Do not read raw tokens from config or env in new code
paths.

### Config

`internal/config` uses the precedence **defaults < file < env < flags**, with
pointer-optionals so "unset" is distinct from a zero value. Env keys are
tag-driven under the `CMX_*` prefix; nested keys use `__`
(`CMX_GITHUB__AUTH_MODE`). Keep `Defaults()` and `Validate()` separate, and keep
secrets out of `--print-config` output (`PrintRedacted`). Secrets arrive via env
or a mounted file only — never via flags or committed YAML.

### Documentation

- Document the CURRENT STATE not changed state. What exists NOW and WHY, not how
  we got here

## Key domain rules

1. **Orchestrator phases.**
   `plan → execute → document → review → integrate → done`, in `phaseOrder`. The
   current phase is **persisted to the card via MCP** before work, orthogonal to
   board state. Persisted phase + an incrementally pushed branch give
   crash-resume: a fresh container re-clones and re-enters at the stored phase.
2. **Git workflow.** Commit incrementally (one logical commit per subtask) and
   **push** after execute and after each review round — `git commit` alone does
   not survive an ephemeral container. Review fixes land as
   `git commit --fixup=<sha>`. Integrate runs `RebaseAutosquash` with
   `GIT_SEQUENCE_EDITOR=true`, then `--force-with-lease` guarded by the recorded
   remote tip. Rebase conflict falls back to soft-reset-to-base + a single
   squashed recommit. The work branch is `cm/<card-id>`; the card ID is
   validated against `PREFIX-NNN` before it reaches any refspec.
3. **One container per top-level card.** All subagents — subtask workers and
   reviewers — run in-process inside that one container on one shared workspace.
   Writers run sequentially or on disjoint paths; only the read-only review
   panel fans out in parallel.
4. **Review = 3 specialists.** Correctness, Design & Maintainability, Security &
   Performance — parallel, read-only (`NewReadOnlyRegistry`), behind a spec/test
   gate; the orchestrator synthesizes the report. Loop to the `review_attempts`
   cap.
5. **Model selection is priors-only.** The planner (a fixed capable model) emits
   a complexity tier per role — simple / moderate / complex / critical;
   deterministic code maps the tier to the cost-optimal model. The LLM never
   names a model. A candidate must be tool-capable, not blacklisted, fit the
   window, and carry a per-role quality prior that clears the tier bar (floor
   0.65); there is **no measured-capability gate**. An eligible operator
   favorite wins outright; otherwise the most capable candidate within a price
   headroom of the cheapest is chosen. Pins (global → project → card) override.
   Priors, favorites, and the blacklist are injected at run start from CM's
   `SelectionContext` payload (`registry.FromSelection`) — nothing is embedded.
   The blacklist is self-learning: a model that proves harness-incapable mid-run
   is reported back, excluded, and a replacement re-selected.
6. **No compactor in v1.** Subagent isolation + `--max-turns`/`--max-cost`
   caps + window-aware selection bound context growth. Nearing the window emits
   a `context_limit` event and returns **incomplete** — the orchestrator treats
   it as a failed subtask, never a silent truncation.
7. **Per-card budget.** One cumulative USD ceiling (`CMX_MAX_CARD_COST`) spans
   the orchestrator and every subagent; the run aborts when exceeded.
8. **Secrets.** `serve` writes `<secrets_dir>/shared/env`, refreshed ahead of
   each GitHub-token expiry, bind-mounted read-only at `/run/cm-secrets/env`.
   The worker reads the configured LLM endpoint key and `CM_GIT_TOKEN` from it. Tool
   subprocesses get an allowlisted `cmd.Env` — secrets are not inheritable by
   model-driven commands — and known secret values are redacted from events and
   transcripts.
9. **HITL gates + promote.** HITL cards run the same FSM as autonomous,
   mode-gated on `Config.Interactive`: a brainstorming dialogue for creative
   cards plus plan-approval and review-decision sign-off gates that wait on the
   inbox. Autonomous is the same FSM with the gates auto-passed and
   brainstorming skipped. A promote frame
   closes the inbox, so every later gate passes through and the run finishes
   autonomously at the persisted phase. Awaiting-human is **live**, not stalled
   — the idle watchdog must not false-stall a parked gate.
10. **Task-skills.** Coder, fix-coder, the review panel, and the document phase
    can engage ContextMatrix's task-skills (`go-development`, `code-review`, …)
    via the model-driven `Skill` tool (`internal/tools/skill.go`): it lists the
    available skills by description and loads a chosen `SKILL.md` on demand,
    filtered to the per-card `task_skills` subset. Delivery is config-free on
    the agent: `serve` fetches a `{git_remote_url, ref}` pointer from CM
    (`GET /api/agent/task-skills-source`), shallow-clones it once
    (`internal/taskskills`), and read-only-mounts it at `/run/cm-skills`.
    Engagement is reported over MCP (`cmclient.RecordSkillEngaged` →
    `add_log action=skill_engaged`), CM's source-agnostic "Path A". Distinct
    from `workflow-skills` and the MCP `get_skill` tool.

## Repo grounding

At the start of every run (`newRun`), the orchestrator walks the cloned
workspace (`Cfg.Workspace`) with `discoverGrounding` and caches the result on
`run.grounding`. All model-driven phases — plan, diagnose, brainstorm, coder,
fix, specialist, synthesis, and document — inject this block into their prompts.

**Discovery rules (`discoverGrounding`):**

- Root is always visited. Walk descends up to `groundingMaxDepth` (4) levels.
- Per directory: `AGENTS.md` is preferred; `CLAUDE.md` is the fallback. Only one
  file per directory is included — never both.
- Skipped directories: `.git`, `node_modules`, `vendor`, `dist`, `build`,
  `.next`, `target`, `.worktrees`.
- Per-file cap: `groundingByteCap` (64 KB); excess is replaced with a
  `[... truncated at 64 KB ...]` marker.
- Total file cap: `groundingMaxDocs` (24); shallowest files are kept, a
  `slog.Warn` is emitted on overflow.
- Sorted root-first, then shallow → deep (alphabetical within the same depth).

**Injection and caching:**

`groundingBlock` formats the docs into a `REPO GROUNDING` prompt block. `newRun`
builds it once and stores it on `run.grounding`; every phase reads the cached
field — there is no per-phase re-scan.

**Best-effort semantics:**

A missing, empty, or non-directory workspace returns `nil` from
`discoverGrounding`; `groundingBlock` then returns `""`. Phases inject nothing
and run as they otherwise would — grounding never fails a run.

**Deferred (future work):**

- **v2 proximity-scoping:** the coder sees only the instruction file for its
  subtask's directory subtree, not the full block.
- **Prompt-caching the grounding block:** the primary cost lever (the block is
  identical across all phases in a run and is a natural cache candidate).

## Observability

`serve` exposes Prometheus metrics on a **separate, loopback-only admin
listener**, mirroring the runner — metrics never ride the public webhook port.

- **Endpoint:** `GET /metrics` on `127.0.0.1:<admin_port>`, HMAC-signed (the
  same signed-GET scheme as the webhook routes: sign `METHOD\nURI\nTS.BODY` with
  the backend `api_key`). `admin_port: 0` (the default) disables the listener; a
  typical enabled value is `9093` (the public port defaults to `9092`). Env
  override: `CMX_ADMIN_PORT`.
- **Metric set** (`cm_agent_*`, on a dedicated registry that also carries the
  standard `go_*` / `process_*` collectors):

  | Metric                                      | Type      | Labels                                                            |
  | ------------------------------------------- | --------- | ----------------------------------------------------------------- |
  | `cm_agent_webhook_requests_total`           | counter   | `endpoint`, `status`, `code`                                      |
  | `cm_agent_webhook_request_duration_seconds` | histogram | `endpoint`                                                        |
  | `cm_agent_container_duration_seconds`       | histogram | `outcome` (`success`/`failure`/`timeout`/`killed`/`idle_timeout`) |
  | `cm_agent_running_containers`               | gauge     | —                                                                 |
  | `cm_agent_callback_retries_total`           | counter   | `endpoint` (`status`/`verify-autonomous`)                         |
  | `cm_agent_broadcaster_drops_total`          | counter   | —                                                                 |

  `endpoint` labels are bounded by an allowlist
  (`internal/metrics.NormalizeEndpoint`); unknown paths collapse to `other`. No
  `card_id` / `project` labels anywhere.

- **Deferred:** panic-recovery counting (`panic_recovered_total` — the serve
  process has no recovery defers yet) and OTEL tracing. The agent keeps its
  correlation IDs.

## Running and testing

```bash
make build          # go build ./... + the contextmatrix-agent binary
make test           # go test ./...
make lint           # golangci-lint run
make fmt            # gofumpt -w .
make docker-worker  # build the worker image

# Drive the harness locally, no ContextMatrix needed:
export LLM_API_KEY=<your-api-key>
# For non-OpenRouter endpoints, also set:
#   export LLM_TYPE=openai
#   export LLM_BASE_URL=https://your-llm-endpoint.example/v1
./contextmatrix-agent run --model openai/gpt-oss-120b --workspace /path/to/checkout \
  --task "..." --verify "go test ./..." --transcript run.jsonl
```

The `tools` tests skip `git`/`rg`/`fd`-dependent cases when those binaries are
absent; install them locally to exercise the full suite. `go test -race` runs in
CI — keep it clean.

### Uncommitted artifacts

These are gitignored point-in-time records — never commit them: `*-RESULTS.md`,
`capabilities-*.json`, `capabilities-*.md`, `transcripts/`, `eval-out/`,
`.envrc`. Nothing model-related is embedded in the binary: priors, favorites,
and the blacklist all arrive at run start from CM's `SelectionContext` payload
(`registry.FromSelection`), so there is no tracked baseline to keep in sync.

## Mandatory verification before proceeding

Every change is fully tested and verified before the next:

1. `go build ./...` — zero errors.
2. `make test` — no regressions; `go test -race ./...` clean.
3. `make lint` — clean.
4. `gofumpt -l .` — empty.

Fix any failure before moving on.

## Commit discipline

```bash
make fmt    # gofumpt -w . — CI flags any gofmt-vs-gofumpt difference
make test   # must be clean before every commit
make lint   # must be clean before every commit
make build  # must build
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
feat(harness): add SpawnSubagents read-only fan-out with depth and cost caps
feat(orchestrator): persist phase to card for crash-resume
fix(executor): kill idle containers on the watchdog interval
feat(registry): select cost-optimal model by complexity tier
```
