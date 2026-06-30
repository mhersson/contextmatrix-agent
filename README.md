# ContextMatrix Agent

> [!WARNING]
>
> This project is under heavy development. Breaking changes should be expected
> at the current stage.

A custom Go agent harness with a configurable LLM endpoint, that runs as a ContextMatrix
**task backend**. It replaces Claude Code headless as the in-container agent:
ContextMatrix dispatches a card, this service launches a Docker worker container,
and the worker drives a hand-built model-in-the-loop harness — claiming the card,
working the code in a target repository, and reporting progress back to the board
over MCP.

It is for operators who run [ContextMatrix](https://github.com/mhersson/contextmatrix)
and want model-flexible, cost-optimized autonomous execution: any
OpenAI-compatible model per role, picked by external quality priors per role
rather than a hard-coded vendor.

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
  same surface the runner-driven agent uses.

## Architecture

One binary, two runtime roles:

```mermaid
flowchart LR
    CM(["ContextMatrix"])

    subgraph serve["contextmatrix-agent serve · host service"]
        direction TB
        S["Hosts CM lifecycle webhooks<br/>Launches one Docker worker per card<br/>Mints GitHub tokens · stages secrets<br/>Streams /logs · drains on SIGTERM"]
    end

    subgraph work["contextmatrix-agent work · one Docker container per card"]
        direction TB
        W["Clone repo on cm/&lt;card-id&gt; · claim the card"]
        FSM["Orchestrator FSM — mode-gated on Cfg.Interactive<br/>plan → execute → document → review → integrate → done"]
        HITL["HITL: brainstorming for creative cards,<br/>plus plan-approval &amp; review-decision human gates"]
        AUTO["Autonomous: gates auto-passed, brainstorming skipped"]
        W --> FSM
        FSM --> HITL
        FSM --> AUTO
    end

    CM ==>|"lifecycle webhooks (HMAC)"| S
    S ==>|"docker run"| W
    W -.->|"card ops over MCP"| CM
```

- **`serve`** is the task backend. It owns container lifecycle, resource limits,
  idle/timeout watchdogs, secret refresh, and graceful drain.
- **`work`** is the in-container agent. It runs the inner loop, fans out
  read-only review subagents, commits and pushes incrementally, and finalizes
  with an autosquash + force-push.

The inner loop lives in the standalone `github.com/mhersson/contextmatrix-harness`
module (`events`, `llm`, `tools`, `harness`, `redact`) — FSM-free and
dependency-free, shared with a planned chat backend. This service wraps it with
the task FSM (`orchestrator`/`worker`) to execute board cards.

## Requirements

- **Go 1.26+** to build.
- **Docker** on the host running `serve` (the worker runtime).
- An **LLM endpoint API key** with access to the models you route to.
- A reachable **ContextMatrix** instance (API + MCP) and its MCP API key.
- **GitHub auth** — a GitHub App (App ID + installation ID + private key) or a
  fine-grained PAT — for cloning and pushing target repositories.

## Quick start: the harness, standalone

Build the binary and drive the loop against a local workspace, no ContextMatrix
required. This is the fastest way to watch the inner loop work and to size a
weak model's tool-call reliability.

```bash
make build
export LLM_API_KEY=<your-api-key>
# For non-OpenRouter endpoints, also set:
#   export LLM_TYPE=openai
#   export LLM_BASE_URL=https://your-llm-endpoint.example/v1

# Run the harness on a workspace with a free-form task.
./contextmatrix-agent run \
  --model openai/gpt-oss-120b \
  --workspace /path/to/a/git/checkout \
  --task "Fix the failing TestAdd in calc_test.go, then run the tests." \
  --verify "go test ./..." \
  --transcript run.jsonl
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
   # set: api_key, mcp_api_key, llm_endpoint.api_key, base_image,
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

### Service management

For an unattended deployment, run the agent as a systemd **user** service
instead of the foreground `serve` command:

```bash
make build                    # build the contextmatrix-agent binary
./svc.sh install              # write + enable ~/.config/systemd/user/contextmatrix-agent.service
./svc.sh start                # start it (also: stop / status / print / verify / uninstall)
```

The generated unit is sandboxed (read-only home, restricted syscalls, resource
caps) and runs `serve --config ${XDG_CONFIG_HOME:-~/.config}/contextmatrix-agent/serve.yaml`.

`redeploy.sh` updates a running install in place — rebuild the binary and worker
image, pin the new image digest into `serve.yaml`, and restart the service:

```bash
./redeploy.sh
```

> **Writable runtime dir.** The agent writes secrets under `secrets_dir`
> (default `/var/run/cm-agent/secrets`). `/var/run` is root-owned and not created
> for a user service — either pre-create `/var/run/cm-agent` and `chown` it to
> your user, or set `secrets_dir` to a path under your home (e.g.
> `~/.cm-agent/secrets`); the unit whitelists both.

## Commands

| Command | Purpose                                                                     |
| ------- | --------------------------------------------------------------------------- |
| `serve` | Run the task backend: host CM lifecycle webhooks, launch worker containers. |
| `work`  | Container entrypoint (hidden): execute one card under CM control.           |
| `run`   | Run the harness on a workspace with a free-form task (standalone).          |

## Model selection

The agent never asks a model to name a model. During planning, a fixed capable
model emits a **complexity tier per role** — simple / moderate / complex /
critical; deterministic code then maps each tier to the cost-optimal model. A
candidate must be tool-capable, not blacklisted, fit the work's context window,
and carry an external quality **prior** for the role that clears the tier's bar
(floor 0.65). Among those, an eligible operator favorite wins outright;
otherwise the selector picks the most capable candidate whose blended price is
within a headroom band of the cheapest. Selection is **priors-only — there is no
measured-capability gate.** Explicit pins (global → project → card) always
override.

The selector's inputs are supplied by ContextMatrix, not embedded in the binary.
Each trigger payload carries a `SelectionContext` with the candidate set, their
per-role priors, the operator favorites, and a self-learning blacklist;
`registry.FromSelection` consumes it at run start. The Artificial-Analysis
sourcing, normalization, and tier-bar tuning all live on the ContextMatrix side.

The blacklist is self-learning: when a model proves harness-incapable mid-run
(for example, it cannot reliably call tools), the agent reports it back so it is
excluded and a replacement is re-selected.

## Development

```bash
make build          # go build ./... + the binary
make test           # go test ./...
make lint           # golangci-lint run
make fmt            # gofumpt -w .
make docker-worker  # build the worker image
```

CI gates every pull request on `go test`, `go test -race`, `golangci-lint`, and
`go build`, plus `govulncheck` and a worker-image scan.

Conventions, package boundaries, the git workflow, and commit discipline for
working **on** this codebase live in [`AGENTS.md`](AGENTS.md).

## Further reading

- [`AGENTS.md`](AGENTS.md) — orientation for contributors and agents.
- [`serve.yaml.example`](serve.yaml.example) — every service config field, documented.
- [ContextMatrix](https://github.com/mhersson/contextmatrix) — the control plane.

## License

MIT — see [`LICENSE`](LICENSE).
