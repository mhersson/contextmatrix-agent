# Secrets and credentials

All credentials are CM-provisioned per run: ContextMatrix mints git tokens and
the LLM endpoint credentials per run and sends them on the trigger payload
(and on the task-skills pointer for the skills clone). The agent carries no
local credential config and mints no git tokens or upstream LLM keys itself;
never read raw tokens from config or env.

## Staging

`serve` stages each run's credentials into
`<secrets_dir>/runs/<project>/<card_id>/env` (`secrets.RunCredentials`),
refreshed from ContextMatrix ahead of each git-token expiry and bind-mounted
read-only at `/run/cm-secrets/env`; the per-run dir is torn down with the run.
The worker reads the stand-in token and URL for the [LLM proxy](#llm-proxy)
and `CM_GIT_TOKEN` from it. In `claude_code` mode the file carries
`LLM_TYPE=claude_code` and `LLM_OAUTH_TOKEN` instead, with no `LLM_API_KEY` or
`LLM_BASE_URL`, plus `LLM_EFFORT` when the endpoint carries a Claude Code
effort level; the worker reads the token and the effort from the file only,
never from env.

The per-run dir is `0700` and the env file `0600`, both owned by the UID `serve`
runs as, while the worker container runs as UID 1000 with every capability
dropped. **`serve` must therefore run as host UID 1000 on rootful Docker.**
Under any other host UID, or under rootless Docker (where container UID 1000
maps to a subordinate host UID), every run exits 1 with
`read secrets: open env file: open /run/cm-secrets/env: permission denied`. On
Linux, `serve` logs a warning at startup when its UID is not 1000; it cannot
detect rootless Docker.

## LLM proxy

The real LLM key never enters a worker container. `serve` keeps it in memory
behind backendkit's `llmproxy`, mounted on `serve`'s own listener at `/llm`,
and gives each run a stand-in token that works only there and only for that
run.

The one exception is a `claude_code` endpoint: it is never registered with the
proxy because the `claude` binary must hold the subscription token itself, so
the token is staged into the run's env file.

```
 CM ──trigger (real key)──▶ serve (host)
                            ┌──────────────────────────────┐
                            │ keeps REAL KEY in memory     │
                            │ proxy: token T → REAL KEY    │
                            └──▲─────────────────┬─────────┘
            gives the container│only token T     │ Authorization: REAL KEY
            + proxy URL        │                 ▼
                  ┌────────────┼── worker ─┐   LLM gateway / OpenRouter
                  │ LLM_API_KEY = T (stand-in)
                  │ LLM_BASE_URL = http://host.docker.internal:<port>/llm
                  │ worker ──Authorization: T──▶ proxy on serve
                  │ bad dependency: reads T   │
                  └───────────┬───────────────┘
                              │ sends T out
                              ▼
            T at the gateway   → rejected (the gateway has never seen T)
            T at serve's proxy → reachable only inside the trusted network,
                                 and dead once the run ends
```

- **Admission.** `serve` registers the payload's LLM endpoint with the proxy
  under the run's correlation id, then stages the returned token as
  `LLM_API_KEY` and `<container_serve_url>/llm` as `LLM_BASE_URL` in the run's
  env file; `LLM_TYPE` is unchanged. The token is registered with the
  host-side redactor (below).
- **Calls.** The proxy forwards only `POST /llm/chat/completions` and
  `GET /llm/models`, swapping the token for the real bearer; an unknown or
  revoked token gets 401. The route sits outside the webhook HMAC check and
  drain gate, so the token is its only credential.
- **Revocation.** The token is revoked when the container exits, when
  credential provisioning or the launch fails or times out, when a kill or
  stop-all stops the launch, when a newer run of the same card displaces the
  run's credentials, and in the shutdown sweep.
- **URL.** `container_serve_url` defaults to
  `http://host.docker.internal:<port>`: every worker maps
  `host.docker.internal` to the host gateway, and `serve` listens on all
  interfaces.

Operator notes:

- Upstream LLM calls leave from `serve`, so its own `HTTP(S)_PROXY`/`NO_PROXY`
  environment applies to them and `ca_cert_file` is trusted for their TLS. An
  egress proxy set only in `worker_extra_env` does not reach LLM traffic;
  set it on the `serve` process or its service unit.
- A `claude_code` endpoint is the exception: `claude` calls Anthropic from the
  worker, so `serve`'s proxy environment does not apply. Set its egress proxy
  in `worker_extra_env`; `claude` gets the worker's `HTTP(S)_PROXY`/`NO_PROXY`,
  with `127.0.0.1`, `localhost` and `::1` added to both `NO_PROXY` and
  `no_proxy` so its loopback tool bridge never goes through the proxy.
  `NO_PROXY` still needs the hosts the container reaches directly, as below.
  `HTTP_PROXY` alone also carries Claude Code's HTTPS API traffic.
  `ca_cert_file` reaches `claude` as `NODE_EXTRA_CA_CERTS`.
- In endpoint mode workers must reach `serve`'s port at `container_serve_url`
  (the LLM proxy), so a host firewall must allow container-to-host traffic on
  that port. A `claude_code` worker never contacts `serve`: it stages no
  `LLM_BASE_URL`, and `claude` calls Anthropic directly.
- An HTTP proxy in `worker_extra_env` needs the ContextMatrix host of
  `container_contextmatrix_url` in `NO_PROXY` when the proxy cannot reach it
  (the worker's MCP connection honours the proxy variables in both modes). In
  endpoint mode it also needs the serve host (`host.docker.internal` by
  default), because the worker's LLM client calls the proxy there. In
  `claude_code` mode the serve host is not needed; loopback is added for you.
- An operator `LLM_API_KEY`, `LLM_BASE_URL` or `LLM_TYPE` in
  `worker_extra_env` wins over the staged values, since container env beats the
  secrets file (`resolveLLMValue`): setting both `LLM_API_KEY` and
  `LLM_BASE_URL` bypasses the proxy and puts that key in the container, and
  setting only one breaks the run (the proxy answers a real key with 401, and
  an upstream has never seen the token). An `LLM_TYPE` overrides the trigger's
  endpoint type, a `claude_code` one included, so leave it unset.
  `LLM_OAUTH_TOKEN` and `LLM_EFFORT` are read only from the staged file; a
  value in `worker_extra_env` is ignored.

Limits:

- Revoking a token stops new requests; a stream already in flight runs to its
  end.
- The token is readable inside the container, so the run can spend through
  the proxy until its token is revoked.

## Claude Code token

In `claude_code` mode the worker hands the subscription token to the `claude`
process environment, where the commands its Bash tool runs can read it: a
prompt-injected agent could exfiltrate it. Other secrets stay out of that
environment, which starts from the scrubbed allowlist (plus the proxy variables
and `NODE_EXTRA_CA_CERTS`).

This is an accepted risk, and the agent does not harden it. Inside the worker
container everything runs as UID 1000, so the model's Bash can read the token
from its own environment (inherited from `claude`) and from the bind-mounted
`/run/cm-secrets/env`. The token is not in the container's launch environment,
so `/proc/1/environ` and the worker's own environment do not carry it.

The MCP key carries the same accepted risk, in both modes: `CM_MCP_API_KEY` is
in the container's launch environment, so any process in the container, the
model's Bash included, can read it from `/proc/1/environ`. The scrubbed
allowlist keeps it out of the model's subprocess environment, not out of
`/proc`.

## Subprocess scrubbing

Tool subprocesses get an allowlisted `cmd.Env` (`tools.ScrubbedEnv`) - secrets
are not inheritable by model-driven commands, except the subscription token in
`claude_code` mode (see above) - and known secret values are redacted from
events and transcripts.

## verify.env pass-throughs

The one addition on top of the allowlist: operator-declared `verify.env`
pass-throughs, resolved by one shared routine
(`orchestrator.ResolveVerifyEnv`, filtered and read from the container env)
and appended for both the verify gate and the model's bash tool, so the model
can reproduce the gate it is told to satisfy. The name filter is the only
guard: a declared value is readable by model-run commands (`env`,
`echo $NAME`) and lands unredacted in events and session transcripts sent to
the LLM provider. Operators must not declare names whose values embed
credentials (e.g. `PGPASSWORD`, a `DATABASE_URL` with userinfo).

## Host-side log redaction

The in-worker redactor never sees container stderr, so worker stderr and
unparsable stdout are masked host-side by the log bridge alone: every
CM-provisioned secret a run receives MUST be registered with the bridge's
`logbridge.RedactorRegistry` before launch and unregistered when the run ends.
The `claude_code` subscription token is registered with the session redactor
and with the worker's run redactor. Mechanics in the `serve.go` and `admitAndLaunch` doc comments.
