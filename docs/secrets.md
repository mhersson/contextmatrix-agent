# Secrets and credentials

All credentials are CM-provisioned per run: ContextMatrix mints git tokens and
the LLM endpoint credentials per run and sends them on the trigger payload
(and on the task-skills pointer for the skills clone). The agent carries no
local credential config and mints no tokens itself. Do not read raw tokens
from config or env in new code paths, and do not add local minting back.

## Staging

`serve` stages each run's credentials into
`<secrets_dir>/runs/<project>/<card_id>/env` (`secrets.RunCredentials`),
refreshed from ContextMatrix ahead of each git-token expiry and bind-mounted
read-only at `/run/cm-secrets/env`; the per-run dir is torn down with the run.
The worker reads the LLM endpoint key and `CM_GIT_TOKEN` from it.

## Subprocess scrubbing

Tool subprocesses get an allowlisted `cmd.Env` (`tools.ScrubbedEnv`) - secrets
are not inheritable by model-driven commands - and known secret values are
redacted from events and transcripts.

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
Mechanics in the `serve.go` and `admitAndLaunch` doc comments.
