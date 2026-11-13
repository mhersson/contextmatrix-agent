# Run kinds

## What a run kind is

A run kind is a sandboxed run ContextMatrix's Sentinel dispatches for one card:
`check` (no model) and `investigate` (a model run). A run kind gets no MCP key,
never writes the card, and reports only through its one terminal status. It
runs in the project's worker image, beside the card's own run if one is live.
`analyse` and `guidance_pr` are reserved names, refused until they ship.

## Dispatch

ContextMatrix calls `POST /run-kind` with a `protocol.RunKindPayload`. The
route is HMAC-authenticated and drain-gated like `/trigger`.
`runKindInvalidField` (`internal/webhook/runkind_validate.go`) checks, in
order: the kind (`check` or `investigate`), the card ID (the pattern `work`
enforces, `internal/cardid`), the git token, that exactly the kind's own input
is set, that an `llm_endpoint` is set exactly for `investigate`, and that
`investigate` has `max_cost_usd > 0`. The answers are:

| Status | Code            | When                                           |
| ------ | --------------- | ---------------------------------------------- |
| 400    | `invalid_json`  | the body does not decode                       |
| 400    | `invalid_field` | a field fails validation; the message names it |
| 413    | `too_large`     | the body is over 1 MiB                         |
| 429    | `limit_reached` | `max_concurrent` runs are tracked              |
| 503    | `draining`      | serve is shutting down                         |
| 202    | -               | accepted; the launch runs asynchronously       |

`handleRunKind` (`internal/webhook/runkind.go`) mints the run id from the card
ID and the kind plus a random suffix, then the `X-Correlation-ID` header, as
`/trigger` does, and launches through the card runs' admission path
(`admitAndLaunch`, `internal/webhook/launch.go`). A run kind sends no `running`
status. A launch that fails sends one `failed` status with `kind`, `run_id` and
the failure's stop cause: `infrastructure`, or `error` for a missing run log. A
launch a `/kill` or `/stop-all` stopped sends none.

`buildRunKindSpec` (`internal/webhook/runkind_spec.go`) gives the container
`CM_RUN_KIND`, `CM_CARD_ID`, `CM_PROJECT`, `CM_REPO_URL` and `CM_BASE_BRANCH`;
`investigate` adds `CM_MODEL`, `CM_MAX_COST_USD` and `CMX_SELECTION`. Then come
the `CMX_*` knobs every run takes (default model, reasoning effort, bash
timeout, tool output cap, container timeout) and `worker_extra_env`. The
payload's `worker_image` overrides `base_image`. There is no `CM_MCP_URL`,
`CM_MCP_API_KEY`, `CMX_ATTEMPT` or `CMX_VERIFY`, and no task skills.

The terminal status goes out when the container exits (`exitPayload`,
`internal/cli/serve_outcome.go`). It carries `kind`, `run_id` and the stop
cause; `usage` from the worker's `run_outcome` line, on completed and failed
statuses alike; and `result` only on a `completed` status with stop cause
`done`. A line with more than 16 usage rows turns the status into `failed`
with cause `error`, no result and no usage. A status whose encoding is over
1 MiB turns into `failed` with cause `error` and no result, keeping its usage.

The executor cuts a run kind's output lines at 2 MiB, a card run's at 1 MiB
(`internal/executor/docker.go`), so the 1 MiB status limit is what binds for
the `run_outcome` line. A run kind's output and its `run_end` event go to its
file log only, never to the live `/logs` stream, which is keyed by card
(`containerLogSink` and `emitRunEnd`, `internal/cli/serve.go`).

## Keying

Every per-run structure is keyed by project, card and kind, the kind empty for
a card run (`executor.RunKey`, `internal/executor/tracker.go`): the tracker,
the admission lock, the container and its labels, the credentials dir and the
file log. A card has at most one run of each kind at a time; its card run and
its run kinds coexist and never block each other. All of them share
`max_concurrent`, and `/health`'s `running_containers` counts them.
`RunKey.Name()` is `<card>.<kind>`: the container is
`cm-agent-<project>-<card>.<kind>`, and the file log is
`<log_dir>/<project>/<card>-<kind>.log`, so a run kind's header never counts
toward the card's attempt number.

## Kill and listing

`/kill` with a `kind` stops that run kind's admission if one is in flight and
kills its container, with no `end_session` first; without a kind it touches
the card run only (`handleKill`, `internal/webhook/handler.go`). Both answer 200
when nothing is tracked. `GET /containers` lists each run with its `kind`,
absent for a card run. `/stop-all` stops card runs and run kinds alike. On
shutdown a run kind gets no `end_session`: the shutdown sweep kills it, and its
status is `failed` with its kind and stop cause `shutdown`
(`internal/cli/serve.go`).

## Credentials

The agent stages the payload's git token into the run's own credentials dir
once, for the clone at start, and never refreshes it: the payload carries no
expiry. The agent cannot narrow the token, so on a personal access token
installation the run may carry the PAT's full scope. The run kinds give the
model no tool that writes to git.

An `investigate` run gets its LLM access as a card run does: a stand-in token
for serve's LLM proxy, or the subscription token for a `claude_code` endpoint.
A `check` registers nothing with the proxy and stages no LLM values
(`stageLLM`, `internal/webhook/launch.go`). On Claude Code the built-in Read is
not confined to the read roots, so the model can read `/run/cm-secrets/env`,
the git token and the subscription token included; both are masked in every
output, and this is an accepted risk. Redaction and the env file: see
[secrets.md](secrets.md#run-kinds).

## Inputs

Each run gets its own input dir on disk, `<log_dir>/.run-inputs/<run id>/`,
mounted read-only at `/run/cm-input` (`internal/runinput`). Serve writes the
kind's input there as `input.json` before the launch. The dir is removed when
the run exits, before its status goes out, and when its launch fails; serve
sweeps `.run-inputs` at start (`internal/cli/serve.go`).

## The stopped run's log

For `investigate`, serve finds the stopped run's section in the card's durable
log by its `run=<id>` header and footer fields and copies all of it into
`run.log` in the run's input dir, next to `input.json`, before the launch
(`internal/filelog/section.go`, `stageRunLog` in `internal/webhook/runkind.go`).
The dir is mounted read-only, so the model reads the section with its grep and
read tools and never whole. A run still writing has its section copied up to the
end of the file. A 256 MiB safety cap keeps the first 32 MiB and the last 224
MiB, with a `==== <n> bytes cut ====` line between them. A missing section fails
the investigation as `run log not found` with stop cause `error`; any other
staging error fails it with stop cause `infrastructure`. Nothing launches in
either case. A run kind writes its own `<card>-<kind>` log, never into the
card's file.

## In the worker

`work` enters the run-kind path when `CM_RUN_KIND` is set (`runKindMain`,
`internal/cli/runkind.go`). It needs no `CM_MCP_URL` or `CM_MCP_API_KEY`,
reads its git token and LLM values from `/run/cm-secrets/env` as a card run
does, and reads its input from `/run/cm-input/input.json`. When the secrets
file carries LLM access, which serve stages only for a kind that needs a model,
it builds the LLM client as a card run does; a `check` gets none. `runkind.Run`
(`internal/runkind/runkind.go`) clones the base branch, or the remote default
when none is set, into `/home/user/workspace/<card id in lower case>`, decodes
the input and runs the kind. Its stdout ends with one `run_outcome` line: the
cause, a model kind's usage rows, and on `done` the result. Before it is
written, the result is masked with the secrets the worker holds (the git token,
the LLM key and the subscription token), then bounded to the size limits below,
then fitted to the outcome limit (`fitOutcome`, `internal/runkind/limits.go`).
A run that cannot read its env or files, build its model executor, clone, or
decode its input ends with cause `error` and no result. Any cause but `done`
exits non-zero.

## Check

`check` runs no model. On the clone of the base branch it lists `candidates`:
every runnable row of the verify detection the gate uses
(`orchestrator.VerifyCandidates`), as the display string the gate would run, in
detection order. That is the wrapper, then each convention row that resolves,
in table order, or the nested scan's command when the root has no marker. The
list is always present, `[]` when there are none.

With `verify` in the input, the check trial-runs one command once
(`trialCandidate`, `internal/runkind/check.go`). Commands compare line by line:
the same non-empty lines, each with equal whitespace-separated fields. A
newline is significant, since bash runs each line as its own command, so a
command whose words are only split differently across lines is a changed one.

- A command equal to `current_command` counts as declared: it runs as the gate
  runs a declared command, under `bash -c` with pipefail, with no candidate or
  plain-argv check.
- A changed command runs only when it equals a candidate and is plain argv (no
  shell metacharacter, not a no-op or read-only program); it then runs as that
  candidate.
- Anything else, an empty command included, is `rejected`, and nothing runs.

The trial takes the input's timeout, clamped as the gate clamps it (10 minutes
when unset, else 30 seconds to 2 hours), and its env names, filtered and read
from the container as the gate does, and runs with the gate's protections
(`orchestrator.TrialVerify`). Its outcome is one of:

| `trial`          | When                                                                            |
| ---------------- | ------------------------------------------------------------------------------- |
| `finished`       | it exited within its timeout; failing tests count                               |
| `timeout`        | it ran past its timeout                                                         |
| `toolchain`      | exit 127, a start error, or a wrapper's tool-missing output                     |
| `infrastructure` | resource exhaustion, a missing container runtime, or any other error running it |
| `rejected`       | not run                                                                         |

`trial` is absent when the input has no `verify`. While the trial runs, the
check writes a `run_kind_progress` event every minute, so a long trial is not
reaped as idle. After it, one `check_trial` event records the outcome and the
command and its output, both masked with the run's secrets. Both events reach
the run kind's file log only.

The limits (`limitCheck`, `internal/runkind/limits.go`): candidates over 512
bytes are dropped, then the first 32 are kept; `absent_from_repo` is cut at 64
entries, each truncated to 512 bytes at a rune boundary. Absent entries are
never dropped for length, since a dropped entry could let the text check pass.

## Text check

When the input has `text` (proposed `## Sentinel` content), `check` extracts
what it mentions and reports the ones absent from the base branch in
`absent_from_repo` (`textTokens` and `absentFromRepo`,
`internal/runkind/textcheck.go`):

- a URL is a `scheme://...` run with trailing `.,;:)]}'"` and backticks
  trimmed; its host is a token too;
- a hostname is a bare token of three or more dot-separated labels whose last
  label is letters only, so a version or a two-label file name is not one;
- a command is an inline code span or a non-empty line of a fenced block,
  trimmed, without a leading `$ `.

A token is present when it equals a declared command by fields (the current
verify command or a candidate, compared as the trial compares commands),
appears in tracked content at HEAD (`git grep -F`, case-insensitive for
hostnames), or is contained in a tracked path. Git runs in the clone with fixed
strings and no shell. ContextMatrix subtracts the card's own text from
`absent_from_repo` and decides the verdict.

## Investigate

`investigate` runs one model pass that diagnoses why a card's autonomous run
stopped (`investigate`, `internal/runkind/investigate.go`). It never writes the
card: its result, the fix included, is a proposal ContextMatrix vets.

It reads:

- the card, its subtasks and the siblings from the input, written read-only
  under `/home/user/investigation` as `card.md`, `subtasks/<id>.md` and
  `siblings/<id>.md` (`writeInputs`, `internal/runkind/investigate_inputs.go`).
  A file whose frontmatter has no valid card ID is named by its 1-based position.
- the stopped run's whole log section, in place at `/run/cm-input/run.log` on
  the read-only input mount. The prompt names it and tells the model to search
  it with grep and read it in ranges, never whole. On the endpoint channel the
  harness `read` refuses any file over 8 MiB, so a larger run log can only be
  searched with `grep`, which has no context lines and prints whole matching
  lines, at most 200. Claude Code's built-in Read pages a large file with
  offset and limit.
- the repository at the base branch, and the card's work branch fetched into
  `refs/remotes/origin/`: `cm/<card id in lower case>` when the remote has it,
  else the one `cm/<card id in lower case>-<Jira key>` branch (`workBranch`, `internal/runkind/investigate_inputs.go`).
  With no work branch on the remote the investigation runs without one; a
  failure to list the remote's branches, or to fetch one that exists, ends the
  run with cause `error`. Git runs
  with the credential helper, so the token stays out of argv.

Its tools are the diagnose pass's read-only set: `read`, `grep` and `glob` on
the workspace, with the inputs dir and the run log's dir as extra read-only
roots, and the read-only `git` tool (status, diff, log, show, branch), plus the
terminal `report_investigation` tool. There is no bash, edit or write. On
Claude Code, `read` is the built-in Read, which is not confined to those roots,
and the others are bridged.

The model is the diagnose pass's decision floor (`orchestrator.DecisionModel`):
the reviewer pick at the complex tier from the payload's selection when it
meets its bar, else the base model: `CM_MODEL`, else `CMX_DEFAULT_MODEL`, else
the built-in capable default. The executor is built as a card run's is: the
harness over the metered client on an endpoint, the `claude` binary with the
endpoint's effort on Claude Code. The run has an 80-turn cap. On an endpoint it
also gets a wrap-up nudge 5 turns before the cap, one grace call for the
report, the context window from the selection and the reasoning effort knob.
The output of the harness tools, bridged ones included, is masked with the
run's secrets and capped by the tool output knob.

The result is `report_investigation`'s arguments, a
`protocol.InvestigateResult`: the `diagnosis`, one `cause` (`card`, `model`,
`environment` or `ecosystem`), an optional `fix` (a `## Sentinel` section,
verify fields, subtasks to retire), `forward_to` (sibling IDs the fix suits) and
friction `findings`. The tool refuses arguments that do not decode, and a report
with an empty diagnosis, a cause outside the four, or a finding whose kind is
not one of `guidance`, `code`, `card_advice`, `project_verify`, `hard_card` and
`ecosystem` or whose text is empty (`reportFault`), so the model can correct
them: ContextMatrix rejects a whole status over a bad cause or finding kind. A
run that ends without the call ends with cause `error`; one stopped at the turn
cap, the context limit or the cost ceiling ends with `max_turns`,
`context_limit` or `budget_exceeded`. Its usage row goes back on every path that
ran the model. The fix and `forward_to` pass through as reported: ContextMatrix
vets the fix and treats `card` without a fix, or a fix with another cause, as no
fix.

The limits, applied after masking (`limitInvestigate` and `fitOutcome`,
`internal/runkind/limits.go`):

| Item                       | Limit         | Over the limit                            |
| -------------------------- | ------------- | ----------------------------------------- |
| `diagnosis`                | 32 KiB        | truncated at a rune boundary              |
| `fix.section`              | 16 KiB        | the fix is dropped                        |
| `fix.retire_subtasks`      | 50 IDs        | the fix is dropped                        |
| `forward_to`               | 50 IDs        | the list is dropped                       |
| finding `title`, `text`    | 200 B, 16 KiB | the finding is dropped                    |
| `findings`                 | 50            | the first 50 are kept                     |
| finding `evidence`         | 4 KiB         | truncated at a rune boundary              |
| encoded `run_outcome` data | 1000 KiB      | the last finding is dropped until it fits |

The fit leaves room for the event envelope under serve's 1 MiB status limit,
and the diagnosis and the fix always survive it.

## Spend

A model run kind has no MCP key, so it cannot report usage to the card.
`max_cost_usd` is the run's own ceiling. On an endpoint, the LLM client is
wrapped (`meteredClient`, `internal/runkind/spend.go`) and checks the ceiling
before every call; a breach ends the run without making the call. Each call is
charged at the gateway's cost, else at the list price from the payload's
selection candidates (`callCost`) when the call's model is listed, else the run
ends with cause `error`, so the ceiling never goes blind. A listed model with
zero prices is free. A list-priced call charges cache tokens at the prompt
price, an upper bound. On Claude Code there is no wrapper: the ceiling goes to
the `claude` binary through the harness config's `MaxCostUSD`, and it charges
the notional cost.

Usage goes back as per-model rows on the terminal status (`usageRow`). A row
carries the model's tokens; on an endpoint its cost is the gateway's sum, and
ContextMatrix applies its catalog estimate when that is 0. A Claude Code row
carries the subscription cost basis and the usage windows, with no cost. The
rows never reach the card's budget.

## Where it lives

| File                                     | Responsibility                                                        |
| ---------------------------------------- | --------------------------------------------------------------------- |
| `internal/webhook/runkind.go`            | `POST /run-kind`: admission, input staging, the launch                |
| `internal/webhook/runkind_validate.go`   | Synchronous payload validation                                        |
| `internal/webhook/runkind_spec.go`       | The run kind's launch spec and env                                    |
| `internal/webhook/launch.go`             | One admission and launch path for card runs and run kinds             |
| `internal/cardid/cardid.go`              | The card ID pattern, shared by `work` and `/run-kind`                 |
| `internal/filelog/section.go`            | Finds one run's section of a card's log and copies it, capped         |
| `internal/runinput/runinput.go`          | A run's input dir on disk: create, remove, start-up sweep; mount path |
| `internal/executor/tracker.go`           | `RunKey` and the tracker keyed by it                                  |
| `internal/executor/docker.go`            | Container name and labels, the input mount, the per-line cut          |
| `internal/runoutcome/runoutcome.go`      | The `run_outcome` line, with a run kind's result and usage            |
| `internal/cli/serve_outcome.go`          | The terminal status and its run-kind rules                            |
| `internal/cli/serve.go`                  | Input store wiring, the exit path, the live stream                    |
| `internal/cli/runkind.go`                | The worker's run-kind entry: env, secrets and input, the outcome line |
| `internal/runkind/runkind.go`            | The spec, the clone, the kind switch, masking and the stop cause      |
| `internal/runkind/check.go`              | The check kind: candidates, the trial rules, the trial and its events |
| `internal/runkind/textcheck.go`          | The text check: token extraction and the base-branch lookup           |
| `internal/runkind/investigate.go`        | The investigate kind: model access, the model run                     |
| `internal/runkind/investigate_inputs.go` | The card files written as read-only inputs, the work-branch lookup    |
| `internal/runkind/investigate_tool.go`   | The `report_investigation` tool: decoding and checking the report     |
| `internal/runkind/investigate_prompt.go` | The investigation's task prompt                                       |
| `internal/runkind/spend.go`              | The metered client and the usage rows                                 |
| `internal/runkind/limits.go`             | The result size limits and the outcome fit                            |
