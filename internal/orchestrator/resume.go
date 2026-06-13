package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
)

// reconcileTierDefault is the conservative tier assigned to every reconciled
// subtask ref. Tiers are NOT persisted on subtask cards (SubtaskStates carries
// only id/title/state), so a resumed run cannot recover the planner's original
// tier. "moderate" is the safe middle: under-selecting a coder model for real
// work is worse than slightly over-paying. tierOf maps this string to
// registry.TierModerate.
const reconcileTierDefault = "moderate"

// reconcile is crash-resume reconciliation: a single pass, run once by execute()
// BEFORE the phase loop, that aligns the run with whatever a prior, interrupted
// run left behind. It is driven entirely by the card's persisted phase
// (o.tc.Phase) and has exactly one direct code path per spec §5.7 row.
//
// Branch handling (spec §5.1):
//   - Fresh (phase == ""): the run owns its card branch. Probe the remote tip
//     best-effort and record it on o.staleRemoteTip; a non-empty tip means a
//     stale branch from an abandoned run exists, so log the planned overwrite —
//     the execute phase's FIRST push carries the force-with-lease against this
//     tip. We do NOT fetch/checkout: we start from base on the freshly-created
//     local branch.
//   - Resume (phase != ""): the fetched branch IS the state. Probe the remote
//     FIRST to decide: an absent branch (crash-before-first-push) continues on
//     the fresh local branch; an existing branch is fetched + checked out, and
//     any failure there is FATAL. See the inline rationale below.
//
// Subtask loading (resume only): SubtaskStates -> o.subtasks with the recovered
// state and the conservative moderate tier. The execute phase skips "done" refs
// and queues the rest in dependency order; the plan phase reuses the titles.
//
// reconcile has NO side effects beyond git (fetch/checkout) and reads
// (RemoteTip/SubtaskStates) plus the one advisory AddLog: it never creates or
// claims cards.
func (o *run) reconcile(ctx context.Context) error {
	cfg := o.d.Cfg

	if o.tc.Phase == "" {
		// FRESH run (§5.7 row ""): no SubtaskStates needed. Record the stale remote
		// tip for the first-push overwrite lease; the value lives on o.staleRemoteTip
		// and is consumed by the execute phase's first Push.
		tip, err := o.d.Git.RemoteTip(ctx, cfg.Branch)
		if err != nil {
			// Best-effort: a missing/unreachable remote branch is the common fresh
			// case. Treat it as "no stale branch" — a plain push will run later.
			slog.Warn("reconcile: remote tip probe failed; assuming no stale branch",
				"card_id", cfg.CardID, "branch", cfg.Branch, "error", err)

			return nil
		}

		o.staleRemoteTip = tip

		if tip != "" {
			// Spec §5.1: the planned overwrite of the stale branch is activity-logged.
			_ = o.d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory note; failure must not abort the run
				fmt.Sprintf("resume: stale remote branch %s detected (tip %s); first push will overwrite it with --force-with-lease",
					cfg.Branch, tip))
		}

		return nil
	}

	// RESUME run (phase != ""): the fetched card branch IS the state.
	//
	// Probe the remote FIRST: the probe is the only signal that distinguishes
	// "crashed before the first push" (branch genuinely absent — continuing on
	// the freshly-created local branch is safe) from "branch exists but
	// fetch/checkout hit a transient failure" (NOT safe — silently rebuilding
	// from base would drop the pushed work, and once every subtask reads "done"
	// the run would sail into integrate and lease-overwrite the genuine remote
	// branch with the incomplete tree).
	tip, err := o.d.Git.RemoteTip(ctx, cfg.Branch)
	if err != nil {
		// Probe failure is fatal on resume: we cannot decide between the two
		// cases above, and guessing wrong risks silent data loss. (Contrast the
		// fresh path, where a failed probe only costs a rejected plain push.)
		return fmt.Errorf("probe remote tip of resume branch %q: %w", cfg.Branch, err)
	}

	if tip == "" {
		// Crash-before-first-push edge: a run persists its phase BEFORE working,
		// so a crash before the first push leaves a phase set but NO remote
		// branch. Skip fetch/checkout and continue on the freshly-created local
		// branch; the subtask states loaded below still drive correct
		// re-scheduling, and the next push creates the branch.
		slog.Info("reconcile: resume branch absent on remote (crashed before first push); continuing on local branch",
			"card_id", cfg.CardID, "branch", cfg.Branch)
	} else {
		// The branch exists, so it IS the state: any failure to fetch or check it
		// out is a transient blip and must fail fast, never rebuild from base.
		if err := o.d.Git.Fetch(ctx, cfg.Branch); err != nil {
			return fmt.Errorf("fetch resume branch %q: %w", cfg.Branch, err)
		}

		if err := o.d.Git.Checkout(ctx, cfg.Branch); err != nil {
			return fmt.Errorf("checkout resume branch %q: %w", cfg.Branch, err)
		}
	}

	// Load subtask state for every resume row (plan/execute use it for
	// scheduling; review/integrate/done carry it only for context). On the plan
	// row these reconciled refs ARE the planner's reuse list — runPlan consumes
	// o.subtasks, it does not re-call SubtaskStates.
	states, err := o.d.Ops.SubtaskStates(ctx, cfg.Project, cfg.CardID)
	if err != nil {
		return fmt.Errorf("load subtask states for resume: %w", err)
	}

	o.subtasks = make([]subtaskRef, 0, len(states))
	for _, st := range states {
		o.subtasks = append(o.subtasks, subtaskRef{
			ID:    st.CardID,
			Title: st.Title,
			State: st.State,
			Tier:  reconcileTierDefault, // tiers aren't persisted; conservative recovery
		})
	}

	return nil
}
