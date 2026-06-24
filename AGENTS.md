# AGENTS.md — ContextMatrix Agent

Orientation for anyone — human or agent — working on this codebase: layout,
build/test commands, conventions, invariants, and commit discipline.

## What is this project?

ContextMatrix Agent is a custom Go agent harness, backed by OpenRouter, that
runs as a ContextMatrix **task backend**. It replaces Claude Code headless as
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

internal/cli/        → cobra commands: run, serve, work, eval, sweep, fanout, priors-refresh
internal/config/     → cobra + koanf config; Config (harness) and ServiceConfig (serve); CMX_* env tags

# Inner loop — FSM-free, dependency-light (the extraction-ready harness core)
internal/harness/    → Harness.Run: the tool-dispatch loop; SpawnSubagents fan-out; Verify
internal/llm/        → raw-HTTP OpenAI-compatible OpenRouter client (Send/SendStream, SSE, models[] failover, catalog)
internal/tools/      → tool registry + impls: read/edit/write/grep/glob/git/bash, jailed + env-scrubbed
internal/registry/   → model selector: Resolve(actor, role), SelectByComplexity, SelectReviewPanel; capabilities + priors
internal/events/     → event stream (model_request | model_response | tool_call | tool_result | usage | state_change | context_limit | error | …)

# Autonomous executor — the FSM and its container lifecycle
internal/orchestrator/ → light hand-FSM plan → execute → document → review → integrate → done; phase persistence; git finalize
internal/worker/       → the `work` lifecycle: clone, claim, HITL-or-FSM, commit/push, PR; wires orchestrator.Deps
internal/executor/     → Executor interface + DockerExecutor; Tracker (concurrency gate); watchdogs
internal/secrets/      → SecretSource (static env file) + Refresher (mints GitHub tokens via githubauth)

# serve plumbing
internal/webhook/    → HTTP server for lifecycle webhooks; HMAC verify; replay cache + message dedup
internal/callback/   → status callbacks to /api/agent/*; VerifyAutonomous (fail-closed)
internal/cmclient/   → MCP client to CM card operations (one agent identity per card)
internal/logbridge/  → worker JSONL events → protocol.LogEntry, fan-out to /logs SSE, redaction, awaiting-human signal
internal/frames/     → stdin control protocol (user_message | promote | end_session)
internal/redact/     → literal-secret redaction for logs and transcripts

# Quality
internal/eval/       → capability harness: coder + reviewer batteries, Wilson-LB scoring, capabilities.json output
internal/kata/       → embedded throwaway kata used by the spike/sweep paths

docker/Dockerfile.worker → the worker image (agent binary + git/rg/fd/gh/node/Go toolchain, pinned + SHA-verified)
docs/model-priors.md     → model-priors refresh procedure
```

## Boundary discipline (the load-bearing invariant)

The harness core must stay **FSM-free and dependency-light** so it can be
promoted to a standalone `contextmatrix-harness` module and shared with a future
chat service. Verified import rules:

- `internal/harness` imports **only** `internal/events`, `internal/llm`,
  `internal/tools`. It must **never** import `orchestrator`, `worker`, `config`,
  `registry`, `cli`, `cmclient`, or `webhook`.
- `internal/orchestrator` imports `harness`, `registry`, `tools`, `events`, and
  `cmclient` (for the `TaskContext` type). It **never** imports `worker`; the
  git and card-ops surfaces are injected as interfaces (`Ops`, `GitOps`,
  `PRCreator`).
- `internal/worker` is the only place that wires the full stack together.

If a change makes the harness reach for orchestration, push the dependency the
other way — inject it behind an interface the worker satisfies.

## Tech stack

- **Go 1.26+** — backend.
- **cobra** (commands/flags) + **koanf** (config) — not viper.
- **Docker SDK** (`github.com/docker/docker`) — worker container lifecycle.
- **`contextmatrix-protocol`** — shared HMAC wire contract (DTOs + signer +
  validation). Do not re-implement HMAC locally.
- **`contextmatrix-githubauth`** — the only path to GitHub tokens (App + PAT).
- **Go MCP SDK** (`github.com/modelcontextprotocol/go-sdk`) — the CM card-ops
  client.
- **OpenRouter** via its OpenAI-compatible `/chat/completions`, spoken over
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

## Key domain rules

1. **Orchestrator phases.** `plan → execute → document → review → integrate → done`,
   in `phaseOrder`. The current phase is **persisted to the card via MCP** before
   work, orthogonal to board state. Persisted phase + an incrementally pushed
   branch give crash-resume: a fresh container re-clones and re-enters at the
   stored phase.
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
5. **Model selection is split in two.** The planner (a fixed capable model)
   emits a complexity tier per role; deterministic code maps the tier to the
   cost-optimal model. The LLM never names a model. Pins (global → project →
   card) override. The window-fit guard keeps a too-small model off oversized
   work.
6. **No compactor in v1.** Subagent isolation + `--max-turns`/`--max-cost`
   caps + window-aware selection bound context growth. Nearing the window emits
   a `context_limit` event and returns **incomplete** — the orchestrator treats
   it as a failed subtask, never a silent truncation.
7. **Per-card budget.** One cumulative USD ceiling (`CMX_MAX_CARD_COST`) spans
   the orchestrator and every subagent; the run aborts when exceeded.
8. **Secrets.** `serve` writes `<secrets_dir>/shared/env`, refreshed ahead of
   each GitHub-token expiry, bind-mounted read-only at `/run/cm-secrets/env`.
   The worker reads `OPENROUTER_API_KEY` and `CM_GIT_TOKEN` from it. Tool
   subprocesses get an allowlisted `cmd.Env` — secrets are not inheritable by
   model-driven commands — and known secret values are redacted from events and
   transcripts.
9. **Promote bridge.** A HITL card promoted mid-run (human-only) stops awaiting
   stdin and hands control to the FSM at the persisted phase. Awaiting-human is
   **live**, not stalled — the idle watchdog must not false-stall it.

## Running and testing

```bash
make build          # go build ./... + the contextmatrix-agent binary
make test           # go test ./...
make lint           # golangci-lint run
make fmt            # gofumpt -w .
make eval           # eval matrix cost estimate (--dry-run)
make docker-worker  # build the worker image

# Drive the harness locally, no ContextMatrix needed:
export OPENROUTER_API_KEY=sk-or-...
./contextmatrix-agent run --model openai/gpt-oss-120b --workspace /path/to/checkout \
  --task "..." --verify "go test ./..." --transcript run.jsonl
```

The `tools` tests skip `git`/`rg`/`fd`-dependent cases when those binaries are
absent; install them locally to exercise the full suite. `go test -race` runs in
CI — keep it clean.

### Uncommitted artifacts

These are gitignored point-in-time records — never commit them: `*-RESULTS.md`,
`capabilities-*.json`, `capabilities-*.md`, `transcripts/`, `eval-out/`,
`.envrc`. The **only** tracked capability baselines are
`internal/registry/data/capabilities.json` and
`internal/registry/data/model-priors.json`, both embedded at build time
(changing either requires a rebuild).

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
