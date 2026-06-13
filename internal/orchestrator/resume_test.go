package orchestrator

import (
	"context"
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
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", Phase: phase}

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

	// No SubtaskStates call on a fresh run — nothing to reconcile.
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
	// phase "" (fresh) + RemoteTip error: NOT fatal — the fresh probe is
	// best-effort, and staleRemoteTip stays "" (plain push later). The asymmetry
	// with the resume path is deliberate: a wrong "no stale branch" guess here
	// only costs a rejected non-fast-forward plain push (run aborts at push,
	// nothing overwritten), whereas a wrong guess on resume risks silent loss.
	ops := &fakeOps{}
	git := &fakeGit{remoteTipErr: assertErr("remote unreachable")}
	o := reconcileTestRun(ops, git, "")

	require.NoError(t, o.reconcile(context.Background()))

	assert.Empty(t, o.staleRemoteTip, "failed probe must leave the stale tip empty")

	// No overwrite log, no subtask load — the fresh row stays side-effect free.
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

	// Branch fetched + checked out — the fetched branch IS the state.
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

func TestReconcileTierRecoveryDefaultsModerate(t *testing.T) {
	// Tier recovery: reconciled refs (no in-memory tier — tiers aren't persisted
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
	// branch never existed. The RemoteTip probe returns "" — the ONLY case where
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
	// The branch EXISTS (probe returned a tip) but Fetch fails — a transient
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

func TestExecuteCalledFromExecuteDriver(t *testing.T) {
	// The phase-loop driver calls reconcile BEFORE the loop: a fresh run records
	// the stale tip even though no SubtaskStates load happens.
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"},
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
	o.reviewFn = func(context.Context) error { return nil }
	o.integrateFn = func(context.Context) error { return nil }
	o.doneFn = func(context.Context) error { return nil }

	require.NoError(t, o.execute(context.Background()))

	assert.Equal(t, "deadbeef", o.staleRemoteTip, "execute() must call reconcile before the phase loop")
}
