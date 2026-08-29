# PR gates

The `pr_gates` phase holds a gated card in review until the PR's Copilot
review is addressed and/or CI is green, then transitions it to done. On round
exhaustion or wait-deadline it parks the card in review (clean exit, claim
released by CM); a re-trigger resumes.

The phase gates on whichever combination of `await_ci` and
`await_copilot_review` the trigger's `TaskContext` carries, running the
Copilot gate first, then the CI gate. Each gate spends up to 3 fix rounds
(`gatesRoundsCap`) on what it finds before parking the card in review.

## Logging

Entering the phase writes a `pr_gates: entering ...` line naming both flags,
`create_pr`, the PR URL, and the effective Copilot/CI/poll waits. Every gate
decision after that - a pass, a skip, a fix round, a park - goes out through
the same path: a `gate_progress` event plus a slog line plus a card log entry,
so the run log carries the full decision sequence and not just the polls.

## When a gate runs

A gate runs whenever a PR URL exists and its flag is set - a stale `pr_url` on
a card whose `create_pr` was later disabled still gates, on the rule that a PR
exists so the gate runs on it.

A gated card whose PR was never created (`create_pr` true, no URL) parks
fail-closed instead of completing - but first probes the branch for an
existing OPEN PR and adopts it (re-reported through `report_push`) when found,
since an earlier run's `report_push` may not have landed, or `gh pr create`
may have failed because one already exists. No OPEN PR, or a probe failure,
still parks.

## CI gate

The poll reads `gh pr checks` and, when the token cannot read the Checks API
(fine-grained PATs on private repos), falls back for the rest of the run to
`gh run list --commit <head-sha>` plus the legacy commit-status API - covered
by Actions: read and Commit statuses: read. Fallback mode cannot see
third-party Checks-API-only integrations.

A poll failure that repeats on every poll - an `unknown flag` from a gh too
old for the fallback poll, a token refused by the Actions-runs or
commit-status API - parks the gate at once with the verbatim gh text in the
card detail instead of looping to the wait deadline. A transient poll failure
keeps looping and, if the wait runs out, its last error text is the park
detail.

A fix round is started only with enough wait left for the coder run, the push,
and a fresh CI cycle: at least 5 minutes and, once the gate has watched one
cycle settle, that cycle plus a 2-minute coder allowance - so a repo whose CI
takes longer than the floor never burns a coder run it cannot see through.
After a fix round the gate re-polls the new head before any deadline verdict,
so a park never re-lists the failures the fix addressed.

## Copilot gate

### Requesting the review

The gate first probes the PR's current head for a review already on it - a
re-trigger after a park, or a ruleset review that landed while integrate was
finishing - and triages that instead of requesting one, so a re-trigger never
pays for a duplicate request and wait.

Otherwise it reads the PR's pending reviewers through the REST
`requested_reviewers` endpoint (`gh api`) rather than
`gh pr view --json reviewRequests`, whose JSON exporter drops Bot-typed
reviewers and would never see Copilot. If Copilot is not already listed, it
requests the review through that same REST endpoint, which accepts the bot
login directly where `gh pr edit --add-reviewer`'s GraphQL resolution cannot.

### Unavailability and request failures

The gate never parks on proven unavailability: a 422 "Copilot isn't available
for this repository" response records the reason verbatim on the card's
activity log (the only diagnostic channel for an external tester's Copilot
setup) and lets the gate pass. Any other request failure is not treated as
unavailability - the gate still enters the wait loop, because a
repo-automated Copilot review may arrive regardless. The same holds for a
re-request that fails after a fix round: it waits for the automatic re-review
rather than passing the gate unreviewed.

A request the API accepts with a 2xx is not trusted either: GitHub silently
discards the request in some setups, so the gate confirms it against the
POST's own response body (the updated PR object's `requested_reviewers`) and,
when that comes back empty, against one delayed re-read of the pending
reviewer list. A request confirmed by neither is treated as dropped: the gate
waits only a short grace window (5 minutes) for a ruleset-delivered review or
a late-listed request, and re-issues the dropped request twice inside the
window (about 60 and 180 seconds in). Each re-request gets the same two-step
confirmation as the first - the response body, then a delayed re-read of the
pending reviewer list. A retry that lands, by either confirmation, upgrades to
the full wait, as does a pending request that appears during grace. A retry
answering the 422 unavailability is proof Copilot cannot review the
repository: it records the reason verbatim and takes the same skip as a
first-request 422. When every retry is dropped too, the gate still passes at
the grace deadline with a note naming the dropped request, instead of burning
the full wait and blaming a slow review. The re-request after a fix round gets
the same re-tried confirm-or-grace treatment.

A confirmed request that never produces a review is recorded and passed at the
wait deadline, 20 minutes by default
(`CMX_GATES_COPILOT_WAIT_TIMEOUT_SECONDS`).

### Triage

Every triage round records a VALID/INVALID verdict per finding under a
`## Copilot Review (Round N)` card section, and the same verdict per comment
under its `### Comments triaged` subsection. A line recorded before verdicts
were tracked carries none and reads as VALID, the conservative default.

Copilot re-posts every open comment on each re-review: a re-review carrying
only comments an earlier round triaged INVALID passes the gate as already
triaged, unreviewed further, while a comment an earlier round triaged VALID
that the fix round failed to resolve counts as still open - it is fed back
through another fix round rather than waved through, bounded by the same
3-round cap as any other Copilot finding.

### Thread replies

The verdicts also reach the PR itself (`CMX_GATES_COPILOT_THREAD_REPLIES`, on
unless set to exactly "false"): each triaged comment's thread gets one reply -
the dismissal reasoning for INVALID, and for VALID the reasoning plus the head
the fix pushed. A dismissed thread is resolved at once; a VALID thread is
resolved only when a re-review stops repeating its comment, so the still-open
reopen logic keeps working. All of it is best-effort: threads are matched by
comment id with the card's dedupe digest as the fallback, a thread already
carrying any reply is never replied to again, and a write failure is one card
note, never a park.

### Outcome persistence

The Copilot outcome is kept in its own detail line under `## PR Gates`,
separate from the CI gate's, so a later CI pass never erases it. An addressed
Copilot review persists a satisfied marker in the `## PR Gates` section, so a
re-trigger skips the paid re-review and goes straight to the CI gate. The
unavailability and timeout skips never write the marker, so those stay
retryable.

## Final probe

After the enabled gates have run, the phase probes once more for a Copilot
review that arrived meanwhile - during a CI wait, or after any wait or skip
that left the gate unreviewed - so a review sitting on the head is never left
unread. If that triage spends a fix round, the enabled gates run again, still
bounded by the 3-round cap per gate. The one exception: when the Copilot gate
recorded proven unavailability this run, the probe is skipped - a repo that
refused a review cannot have one sitting on the head, so the extra probe would
only read an empty result.
