package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconcileTestRun builds a run wired for reconcile tests: scripted ops + git, a
// task context with the given persisted phase, and the standard branch config.
func reconcileTestRun(ops *fakeOps, git *fakeGit, phase string) *run {
	d := Deps{
		Ops: ops,
		Git: git,
		Cfg: Config{
			Project:    "proj",
			CardID:     "CARD-1",
			Branch:     "cm/card-1",
			BaseBranch: "main",
		},
	}
	tc := cmclient.TaskContext{Title: "Parent", Description: "body", Phase: phase}

	return newRun(d, tc)
}

func TestReconcileFreshRecordsStaleTip(t *testing.T) {
	// phase "" (fresh) + a stale remote branch: RemoteTip is recorded on the run
	// and the planned overwrite is activity-logged. No SubtaskStates call.
	ops := &fakeOps{}
	git := &fakeGit{remoteTip: "abc123"}
	o := reconcileTestRun(ops, git, "")

	require.NoError(t, o.reconcile(context.Background()))

	assert.Equal(t, "abc123", o.staleRemoteTip, "fresh run records the stale remote tip")

	gitCalls := git.recorded()
	assert.GreaterOrEqual(t, indexOfCall(gitCalls, "RemoteTip:cm/card-1"), 0, "fresh run probes the remote tip")
	assert.Equal(t, -1, indexOfCall(gitCalls, "Fetch:cm/card-1"), "fresh run must not fetch the card branch")
	assert.Equal(t, -1, indexOfCall(gitCalls, "Checkout:cm/card-1"), "fresh run stays on the freshly-created branch")

	// No SubtaskStates call on a fresh run - nothing to reconcile.
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "SubtaskStates:proj/CARD-1"))

	// The planned overwrite is activity-logged.
	var logged bool

	for _, c := range ops.recorded() {
		if len(c) >= 7 && c[:7] == "AddLog:" {
			logged = true
		}
	}

	assert.True(t, logged, "stale-branch overwrite must be activity-logged; calls=%v", ops.recorded())
}

func TestReconcileFreshNoRemoteBranch(t *testing.T) {
	// phase "" (fresh) + absent remote branch: RemoteTip "" recorded; no lease
	// needed later (plain push), and no overwrite log.
	ops := &fakeOps{}
	git := &fakeGit{remoteTip: ""}
	o := reconcileTestRun(ops, git, "")

	require.NoError(t, o.reconcile(context.Background()))

	assert.Empty(t, o.staleRemoteTip, "absent remote branch records an empty tip")

	// No overwrite log when there is nothing to overwrite.
	for _, c := range ops.recorded() {
		assert.NotContains(t, c, "AddLog:", "no overwrite log when the remote branch is absent")
	}
}

func TestReconcileFreshProbeFailure(t *testing.T) {
	// phase "" (fresh) + RemoteTip error: NOT fatal - the fresh probe is
	// best-effort, and staleRemoteTip stays "" (plain push later). The asymmetry
	// with the resume path is deliberate: a wrong "no stale branch" guess here
	// only costs a rejected non-fast-forward plain push (run aborts at push,
	// nothing overwritten), whereas a wrong guess on resume risks silent loss.
	ops := &fakeOps{}
	git := &fakeGit{remoteTipErr: assertErr("remote unreachable")}
	o := reconcileTestRun(ops, git, "")

	require.NoError(t, o.reconcile(context.Background()))

	assert.Empty(t, o.staleRemoteTip, "failed probe must leave the stale tip empty")

	// No overwrite log, no subtask load - the fresh row stays side-effect free.
	for _, c := range ops.recorded() {
		assert.NotContains(t, c, "AddLog:", "no overwrite log on a failed probe")
		assert.NotContains(t, c, "SubtaskStates:", "fresh run never loads subtask states")
	}
}

func TestReconcilePlanLoadsExistingTitles(t *testing.T) {
	// phase "plan": SubtaskStates loaded into subtaskRefs; the planner reuse list
	// is fed from the RECONCILED refs (asserted in plan via the captured prompt
	// below). Here: reconcile loads the refs with state + conservative tier.
	ops := &fakeOps{
		subtaskStates: []cmclient.SubtaskState{
			{CardID: "SUB-OLD-1", Title: "alpha", State: "in_progress"},
			{CardID: "SUB-OLD-2", Title: "beta", State: "todo"},
		},
	}
	git := &fakeGit{remoteTip: "tip-1"} // the remote branch exists: it IS the state
	o := reconcileTestRun(ops, git, "plan")

	require.NoError(t, o.reconcile(context.Background()))

	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "SubtaskStates:proj/CARD-1"), 0,
		"plan resume must load subtask states")

	require.Len(t, o.subtasks, 2)
	assert.Equal(t, "SUB-OLD-1", o.subtasks[0].ID)
	assert.Equal(t, "alpha", o.subtasks[0].Title)
	assert.Equal(t, "in_progress", o.subtasks[0].State)
	assert.Equal(t, "beta", o.subtasks[1].Title)

	// Branch fetched + checked out - the fetched branch IS the state.
	git.assertOrder(t, "Fetch:cm/card-1", "Checkout:cm/card-1")
}

func TestReconcileExecuteQueuesUnfinished(t *testing.T) {
	// phase "execute": SubtaskStates loaded -> done marked done in subtaskRefs;
	// in_progress/todo queued; Fetch(branch) + Checkout(branch) recorded.
	ops := &fakeOps{
		subtaskStates: []cmclient.SubtaskState{
			{CardID: "SUB-1", Title: "done one", State: "done"},
			{CardID: "SUB-2", Title: "in progress", State: "in_progress"},
			{CardID: "SUB-3", Title: "queued", State: "todo"},
		},
	}
	git := &fakeGit{remoteTip: "tip-1"} // the remote branch exists: it IS the state
	o := reconcileTestRun(ops, git, "execute")

	require.NoError(t, o.reconcile(context.Background()))

	require.Len(t, o.subtasks, 3)
	assert.Equal(t, "done", o.subtasks[0].State, "done subtask stays done (skipped on re-run)")
	assert.Equal(t, "in_progress", o.subtasks[1].State)
	assert.Equal(t, "todo", o.subtasks[2].State)

	git.assertOrder(t, "Fetch:cm/card-1", "Checkout:cm/card-1")
}

func TestReconcileSortsSubtasksByCardID(t *testing.T) {
	// list_cards returns subtasks in nondeterministic (map-iteration) order and
	// carries no dependency edges, so reconcile must impose a stable, dependency-
	// valid order. Card IDs are server-assigned in zero-padded creation order, and
	// the planner only makes a subtask depend on an earlier one, so sorting by card
	// ID recovers a valid topological order regardless of list_cards' return order.
	ops := &fakeOps{
		subtaskStates: []cmclient.SubtaskState{
			{CardID: "SUB-003", Title: "c", State: "todo"},
			{CardID: "SUB-001", Title: "a", State: "done"},
			{CardID: "SUB-002", Title: "b", State: "todo"},
		},
	}
	git := &fakeGit{remoteTip: "tip-1"} // resume path: branch exists, fetch+checkout
	o := reconcileTestRun(ops, git, "execute")

	require.NoError(t, o.reconcile(context.Background()))

	require.Len(t, o.subtasks, 3)
	got := []string{o.subtasks[0].ID, o.subtasks[1].ID, o.subtasks[2].ID}
	assert.Equal(t, []string{"SUB-001", "SUB-002", "SUB-003"}, got,
		"reconciled subtasks must be sorted by card ID for a deterministic, dependency-valid resume order")
}

func TestReconcileTierRecoveryDefaultsModerate(t *testing.T) {
	// Tier recovery: reconciled refs (no in-memory tier - tiers aren't persisted
	// on subtask cards) get the conservative "moderate" default.
	ops := &fakeOps{
		subtaskStates: []cmclient.SubtaskState{
			{CardID: "SUB-1", Title: "one", State: "todo"},
		},
	}
	git := &fakeGit{}
	o := reconcileTestRun(ops, git, "execute")

	require.NoError(t, o.reconcile(context.Background()))

	require.Len(t, o.subtasks, 1)
	assert.Equal(t, "moderate", o.subtasks[0].Tier, "reconciled refs default to the conservative moderate tier")
}

func TestReconcileContextPhasesNoSideEffects(t *testing.T) {
	// phase "review"/"integrate"/"done": subtask list loaded for context; branch
	// fetched + checked out; reconcile itself creates no cards and claims nothing.
	for _, phase := range []string{"review", "integrate", "done"} {
		t.Run(phase, func(t *testing.T) {
			ops := &fakeOps{
				subtaskStates: []cmclient.SubtaskState{
					{CardID: "SUB-1", Title: "context", State: "done"},
				},
			}
			git := &fakeGit{remoteTip: "tip-1"} // the remote branch exists: it IS the state
			o := reconcileTestRun(ops, git, phase)

			require.NoError(t, o.reconcile(context.Background()))

			// Loaded for context.
			require.Len(t, o.subtasks, 1)
			assert.Equal(t, "SUB-1", o.subtasks[0].ID)

			// Branch fetched + checked out.
			git.assertOrder(t, "Fetch:cm/card-1", "Checkout:cm/card-1")

			// No plan/execute side effects: nothing created, nothing claimed.
			for _, c := range ops.recorded() {
				assert.NotContains(t, c, "CreateCard:", "reconcile must not create cards")
				assert.NotContains(t, c, "ClaimCard:", "reconcile must not claim cards")
			}
		})
	}
}

func TestReconcileResumeBranchGenuinelyAbsent(t *testing.T) {
	// Edge: phase set but the run crashed before its first push, so the remote
	// branch never existed. The RemoteTip probe returns "" - the ONLY case where
	// continuing on the freshly-created local branch is safe. Fetch/Checkout are
	// skipped entirely (not attempted-and-tolerated), and the subtask list loads.
	ops := &fakeOps{
		subtaskStates: []cmclient.SubtaskState{
			{CardID: "SUB-1", Title: "one", State: "todo"},
		},
	}
	git := &fakeGit{remoteTip: ""} // probe says: branch does not exist
	o := reconcileTestRun(ops, git, "execute")

	require.NoError(t, o.reconcile(context.Background()))

	gitCalls := git.recorded()
	assert.GreaterOrEqual(t, indexOfCall(gitCalls, "RemoteTip:cm/card-1"), 0, "resume must probe the remote first")
	assert.Equal(t, -1, indexOfCall(gitCalls, "Fetch:cm/card-1"), "absent branch: fetch must be skipped, not tolerated")
	assert.Equal(t, -1, indexOfCall(gitCalls, "Checkout:cm/card-1"), "absent branch: checkout must be skipped")

	for _, c := range gitCalls {
		assert.False(t, strings.HasPrefix(c, "HardReset"),
			"absent branch: no remote tip to adopt, HardReset must be skipped; calls=%v", gitCalls)
	}

	require.Len(t, o.subtasks, 1)
	assert.Equal(t, "SUB-1", o.subtasks[0].ID)
}

func TestReconcileResumeProbeFailureFatal(t *testing.T) {
	// RemoteTip itself failing on resume is FATAL: without the probe we cannot
	// distinguish "branch never pushed" from "branch exists but unreachable", and
	// guessing wrong risks rebuilding from base and later lease-overwriting the
	// genuine branch with an incomplete tree.
	ops := &fakeOps{}
	git := &fakeGit{remoteTipErr: assertErr("remote unreachable")}
	o := reconcileTestRun(ops, git, "execute")

	err := o.reconcile(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote tip")

	// Nothing proceeds past the failed probe.
	assert.Equal(t, -1, indexOfCall(git.recorded(), "Fetch:cm/card-1"))
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "SubtaskStates:proj/CARD-1"))
}

func TestReconcileResumeFetchFailureFatal(t *testing.T) {
	// The branch EXISTS (probe returned a tip) but Fetch fails - a transient
	// blip, not a missing branch. FATAL: silently proceeding on a fresh-from-base
	// tree would drop the pushed work and later overwrite the genuine branch.
	ops := &fakeOps{}
	git := &fakeGit{
		remoteTip: "tip-1",
		fetchErr:  assertErr("connection reset"),
	}
	o := reconcileTestRun(ops, git, "execute")

	err := o.reconcile(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch")

	// Nothing proceeds past the failed fetch.
	assert.Equal(t, -1, indexOfCall(git.recorded(), "Checkout:cm/card-1"))
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "SubtaskStates:proj/CARD-1"))
}

func TestReconcileResumeCheckoutFailureFatal(t *testing.T) {
	// The branch EXISTS and Fetch succeeded, but Checkout fails. Same reasoning:
	// the run must not continue on a tree that is not the fetched branch state.
	ops := &fakeOps{}
	git := &fakeGit{
		remoteTip:   "tip-1",
		checkoutErr: assertErr("pathspec did not match"),
	}
	o := reconcileTestRun(ops, git, "execute")

	err := o.reconcile(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checkout")

	assert.Equal(t, -1, indexOfCall(ops.recorded(), "SubtaskStates:proj/CARD-1"))
}

func TestReconcileResumeAdoptsRemoteTip(t *testing.T) {
	// The core resume contract: an existing remote branch is fetched, checked
	// out, AND hard-reset onto the probed tip. The reset is load-bearing - the
	// worker's prepareWorkspace already created a local branch of this name at
	// base HEAD, so `git checkout <branch>` alone leaves the local branch at base
	// (git does not fast-forward a pre-existing local branch to the fetched tip).
	// Without the adopt the run silently restarts from base and its next WIP push
	// is a non-fast-forward reject. reconcile must issue HardReset with the exact
	// probed tip, in order, after the checkout.
	ops := &fakeOps{
		subtaskStates: []cmclient.SubtaskState{
			{CardID: "SUB-1", Title: "one", State: "in_progress"},
		},
	}
	git := &fakeGit{remoteTip: "wip-tip-9"} // the remote branch exists: it IS the state
	o := reconcileTestRun(ops, git, "execute")

	require.NoError(t, o.reconcile(context.Background()))

	git.assertOrder(t, "RemoteTip:cm/card-1", "Fetch:cm/card-1", "Checkout:cm/card-1", "HardReset:wip-tip-9")
	assert.Equal(t, []string{"wip-tip-9"}, git.hardResetRefs,
		"resume must adopt the exact probed remote tip, not some other ref")
}

func TestReconcileResumeHardResetFailureFatal(t *testing.T) {
	// The branch EXISTS and Fetch + Checkout succeeded, but the hard reset onto
	// the remote tip fails. FATAL for the same reason as fetch/checkout failure:
	// proceeding would run on a base-pointing tree that is NOT the resumed branch
	// state, dropping pushed work and later overwriting the genuine branch with an
	// incomplete tree.
	ops := &fakeOps{}
	git := &fakeGit{
		remoteTip:    "wip-tip-9",
		hardResetErr: assertErr("reset --hard: unknown revision"),
	}
	o := reconcileTestRun(ops, git, "execute")

	err := o.reconcile(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adopt")

	// Nothing proceeds past the failed adopt.
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "SubtaskStates:proj/CARD-1"))
}

func TestExecuteCalledFromExecuteDriver(t *testing.T) {
	// The phase-loop driver calls reconcile BEFORE the loop: a fresh run records
	// the stale tip even though no SubtaskStates load happens.
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
	}
	git := &fakeGit{remoteTip: "deadbeef"}
	d := Deps{
		Ops: ops,
		Git: git,
		Cfg: Config{Project: "proj", CardID: "CARD-1", Branch: "cm/card-1", BaseBranch: "main"},
	}

	o := newRun(d, ops.taskContext)
	// Stub the phase bodies so the driver only exercises reconcile + persistence.
	o.planFn = func(context.Context) error { return nil }
	o.executeFn = func(context.Context) error { return nil }
	o.documentFn = func(context.Context) error { return nil }
	o.reviewFn = func(context.Context) error { return nil }
	o.integrateFn = func(context.Context) error { return nil }
	o.doneFn = func(context.Context) error { return nil }

	require.NoError(t, o.execute(context.Background()))

	assert.Equal(t, "deadbeef", o.staleRemoteTip, "execute() must call reconcile before the phase loop")
}

func TestReconcileRestoresTierAndBody(t *testing.T) {
	// The core persistence contract: a pending subtask's card body carries the
	// marker written at creation time (plan.go's withTierMarker), and reconcile
	// restores both the tier and the marker-stripped planner body from it.
	ops := &fakeOps{}
	ops.subtaskStates = []cmclient.SubtaskState{{CardID: "SUB-2", Title: "impl", State: "todo"}}
	ops.taskContexts = map[string]cmclient.TaskContext{
		"SUB-2": {Title: "impl", Description: withTierMarker("planner body", "complex")},
	}
	git := &fakeGit{remoteTip: "abc123"}

	o := reconcileTestRun(ops, git, "execute")
	require.NoError(t, o.reconcile(context.Background()))

	require.Len(t, o.subtasks, 1)
	assert.Equal(t, "complex", o.subtasks[0].Tier, "persisted tier survives resume")
	assert.Equal(t, "planner body", o.subtasks[0].Body, "planner body survives resume, marker stripped")
}

func TestReconcileTierFallbackWithoutMarker(t *testing.T) {
	// A subtask card body without a marker (e.g. created before this feature, or
	// hand-edited) falls back to the conservative default tier, and the body is
	// still restored from the fetched description (better than a bare title).
	ops := &fakeOps{}
	ops.subtaskStates = []cmclient.SubtaskState{{CardID: "SUB-2", Title: "impl", State: "todo"}}
	ops.taskContexts = map[string]cmclient.TaskContext{
		"SUB-2": {Title: "impl", Description: "planner body, no marker"},
	}
	git := &fakeGit{remoteTip: "abc123"}

	o := reconcileTestRun(ops, git, "execute")
	require.NoError(t, o.reconcile(context.Background()))

	require.Len(t, o.subtasks, 1)
	assert.Equal(t, "moderate", o.subtasks[0].Tier, "no marker: falls back to the conservative default")
	assert.Equal(t, "planner body, no marker", o.subtasks[0].Body, "body is still restored from the fetched description")
}

func TestReconcileToleratesTaskContextFailure(t *testing.T) {
	// Resume must not become fragile: a GetTaskContext failure for a subtask
	// degrades to today's defaults (title-only body, "moderate" tier) rather than
	// failing the whole resume.
	ops := &fakeOps{}
	ops.subtaskStates = []cmclient.SubtaskState{{CardID: "SUB-2", Title: "impl", State: "todo"}}
	ops.taskCtxErr = assertErr("context fetch unavailable")
	git := &fakeGit{remoteTip: "abc123"}

	o := reconcileTestRun(ops, git, "execute")
	require.NoError(t, o.reconcile(context.Background()), "a subtask context fetch failure must not fail reconcile")

	require.Len(t, o.subtasks, 1)
	assert.Equal(t, "moderate", o.subtasks[0].Tier, "fetch failure falls back to the conservative default")
	assert.Empty(t, o.subtasks[0].Body, "fetch failure leaves the body unset (title-only)")
}
