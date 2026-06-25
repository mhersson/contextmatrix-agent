# ContextMatrix Agent

A custom Go agent harness, backed by OpenRouter, that runs as a ContextMatrix
**task backend**. It replaces Claude Code headless as the in-container agent:
ContextMatrix dispatches a card, this service launches a Docker worker container,
and the worker drives a hand-built model-in-the-loop harness — claiming the card,
working the code in a target repository, and reporting progress back to the board
over MCP.

It is for operators who run [ContextMatrix](https://github.com/mhersson/contextmatrix)
and want model-flexible, cost-optimized autonomous execution: any
OpenRouter-served model per role, picked by measured capability rather than a
hard-coded vendor.

## How it fits ContextMatrix

ContextMatrix splits its backend contract into a `TaskBackend` (card lifecycle)
and a `ChatBackend` (interactive chat), resolved independently. This service
implements `TaskBackend`. It coexists with `contextmatrix-runner`; the task
cutover is a single global config flip on the ContextMatrix side, and the runner
stays the one-flip fallback.

Two channels connect the agent to ContextMatrix:

- **Lifecycle webhooks (CM → agent).** ContextMatrix calls `/trigger`, `/kill`,
  `/stop-all`, `/message`, `/promote`, and `/end-session` over the
  [`contextmatrix-protocol`](https://github.com/mhersson/contextmatrix-protocol)
  HMAC contract. The agent reports status back to `/api/agent/*`.
- **Card operations (worker → CM, over MCP).** Inside each container the worker
  claims the card, heartbeats, reports usage, sets the orchestrator phase,
  transitions state, and completes the task using ContextMatrix MCP tools — the
  same surface the runner-driven agent uses today, inherited unchanged.

## Architecture

One binary, two runtime roles:

```
contextmatrix-agent serve     contextmatrix-agent work  (ENTRYPOINT)
─────────────────────────     ──────────────────────────────────────
Long-running host service.    Container entrypoint, one per card.
Hosts CM lifecycle webhooks.  Clones the repo on a cm/<card-id> branch,
Launches one Docker worker     claims the card, then drives either:
container per card. Mints       • HITL: the bare harness loop (stdin
GitHub tokens, stages the         injects human turns), or
shared secrets env file,        • autonomous: the orchestrator FSM
streams /logs, drains on          (plan → execute → document →
SIGTERM.                          review → integrate → done).
        │                                    │
        │ launches ▼                         │ reports over MCP ▼
        └──────────── Docker container ──────┴────────────► ContextMatrix
```

- **`serve`** is the task backend. It owns container lifecycle, resource limits,
  idle/timeout watchdogs, secret refresh, and graceful drain.
- **`work`** is the in-container agent. It runs the inner loop, fans out
  read-only review subagents, commits and pushes incrementally, and finalizes
  with an autosquash + force-push.

The inner loop itself (`internal/harness`) is FSM-free and dependency-light by
design — it is extraction-ready for a future shared `contextmatrix-harness`
module consumed by both the autonomous agent and a later chat service.

## Requirements

- **Go 1.26+** to build.
- **Docker** on the host running `serve` (the worker runtime).
- An **OpenRouter API key** with credits for the models you route to.
- A reachable **ContextMatrix** instance (API + MCP) and its MCP API key.
- **GitHub auth** — a GitHub App (App ID + installation ID + private key) or a
  fine-grained PAT — for cloning and pushing target repositories.

## Quick start: the harness, standalone

Build the binary and drive the loop against a local workspace, no ContextMatrix
required. This is the fastest way to watch the inner loop work and to size a
weak model's tool-call reliability.

```bash
make build
export OPENROUTER_API_KEY=sk-or-...

# Run the harness on a workspace with a free-form task.
./contextmatrix-agent run \
  --model openai/gpt-oss-120b \
  --workspace /path/to/a/git/checkout \
  --task "Fix the failing TestAdd in calc_test.go, then run the tests." \
  --verify "go test ./..." \
  --transcript run.jsonl

# Compare a weak model against a stronger control on the same kata.
./contextmatrix-agent sweep --model google/gemma-4-31b-it:free --control-model openai/gpt-oss-120b:free

# Fan out parallel read-only subagents over a workspace.
./contextmatrix-agent fanout --workspace /path/to/checkout --model openai/gpt-oss-120b \
  --task "correctness=Check the diff for logic bugs" \
  --task "security=Check the diff for injection and auth flaws"
```

## Running as a ContextMatrix backend

1. **Build the worker image.** The worker image bundles the agent binary plus
   the CLIs the harness expects (`git`, `rg`, `fd`, `gh`, `node`, a Go
   toolchain). Toolchain versions are pinned and SHA256-verified.

   ```bash
   make docker-worker          # tags contextmatrix-agent-worker:dev
   ```

   For deployment, publish a digest-pinned image (for example
   `ghcr.io/mhersson/contextmatrix-agent@sha256:...`) and reference it from
   `base_image`.

2. **Write the service config.** Copy the template and edit it. Every field also
   has a `CMX_*` environment override (nested keys use `__`, e.g.
   `CMX_GITHUB__AUTH_MODE`).

   ```bash
   mkdir -p ~/.config/contextmatrix-agent
   cp serve.yaml.example ~/.config/contextmatrix-agent/serve.yaml
   # set: api_key, mcp_api_key, openrouter_api_key, base_image,
   #      container_contextmatrix_url, github.*
   ```

   `container_contextmatrix_url` is required in practice — workers resolve their
   MCP URL from it; without it they point at their own localhost and fail to
   connect. For Docker bridge networking it is typically the bridge gateway
   (`http://172.17.0.1:8080`).

3. **Run the service.**

   ```bash
   ./contextmatrix-agent serve            # listens on :9092
   ```

   `serve` reads `--config` (default `~/.config/contextmatrix-agent/serve.yaml`)
   and validates it on startup. To inspect effective harness config with secrets
   redacted, use `run --print-config`.

4. **Point ContextMatrix at it.** Register this service as the task backend in
   ContextMatrix's `backends` config (URL + shared `api_key` + callback path
   `/api/agent/*`) and flip `default_backend` to it. A backend switch needs a
   ContextMatrix restart: drain running jobs → switch → restart.

## Commands

| Command          | Purpose                                                                     |
| ---------------- | --------------------------------------------------------------------------- |
| `serve`          | Run the task backend: host CM lifecycle webhooks, launch worker containers. |
| `work`           | Container entrypoint (hidden): execute one card under CM control.           |
| `run`            | Run the harness on a workspace with a free-form task (standalone).          |
| `sweep`          | Run the same kata on a weak and a control model and compare.                |
| `fanout`         | Fan out parallel read-only subagents over a workspace.                      |
| `eval`           | Measure per-role capability scores across a model set (Wilson-LB).          |
| `priors-refresh` | Propose model-priors updates from Artificial Analysis indices.              |

## Model selection

The agent never asks a model to name a model. During planning, a fixed capable
model emits a **complexity tier per role** (simple / moderate / complex);
deterministic code then maps each tier to the cost-optimal model — the cheapest
whose measured capability clears the tier bar, within a price-headroom band, and
whose context window fits the work. Explicit pins (global → project → card)
always override.

Two data sources back the selector, both embedded into the binary at build time:

- **`internal/registry/data/capabilities.json`** — per-role tool-use scores
  measured by the eval harness on this project's own task batteries.
- **`internal/registry/data/model-priors.json`** — external quality priors
  (coding / intelligence indices) used as the tier bar. See
  [`docs/model-priors.md`](docs/model-priors.md) for the refresh procedure.

The `eval` command re-measures capabilities; `priors-refresh` proposes prior
updates from Artificial Analysis. Updating either requires a rebuild to re-embed.

## Development

```bash
make build          # go build ./... + the binary
make test           # go test ./...
make lint           # golangci-lint run
make fmt            # gofumpt -w .
make eval           # dry-run cost estimate for the eval matrix
make docker-worker  # build the worker image
```

CI runs `go vet`, `go test`, `go test -race`, and `golangci-lint` on every push.

Conventions, package boundaries, the git workflow, and commit discipline for
working **on** this codebase live in [`AGENTS.md`](AGENTS.md).

## Further reading

- [`AGENTS.md`](AGENTS.md) — orientation for contributors and agents.
- [`serve.yaml.example`](serve.yaml.example) — every service config field, documented.
- [`docs/model-priors.md`](docs/model-priors.md) — model-priors refresh.
- [ContextMatrix](https://github.com/mhersson/contextmatrix) — the control plane.

## License

MIT — see [`LICENSE`](LICENSE).
