package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckpointEligible(t *testing.T) {
	tests := []struct {
		name string
		mob  MobConfig
		tier string
		want bool
	}{
		{
			name: "off when mob disabled",
			mob:  MobConfig{Participants: 0, Execute: true, CheckpointMinTier: "simple"},
			tier: "complex", want: false,
		},
		{
			name: "off when execute phase not on",
			mob:  MobConfig{Participants: 3, Execute: false, CheckpointMinTier: "simple"},
			tier: "complex", want: false,
		},
		{
			name: "simple floor admits everything",
			mob:  MobConfig{Participants: 3, Execute: true, CheckpointMinTier: "simple"},
			tier: "simple", want: true,
		},
		{
			name: "complex floor rejects moderate",
			mob:  MobConfig{Participants: 3, Execute: true, CheckpointMinTier: "complex"},
			tier: "moderate", want: false,
		},
		{
			name: "complex floor admits critical",
			mob:  MobConfig{Participants: 3, Execute: true, CheckpointMinTier: "critical"},
			tier: "critical", want: true,
		},
		{
			name: "empty subtask tier counts as moderate",
			mob:  MobConfig{Participants: 3, Execute: true, CheckpointMinTier: "moderate"},
			tier: "", want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := mobTestRun(&fakeOps{}, tt.mob, 0)
			got := o.checkpointEligible(subtaskRef{ID: "SUB-1", Tier: tt.tier})
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseCheckpointVerdict(t *testing.T) {
	t.Run("proceed", func(t *testing.T) {
		v, err := parseCheckpointVerdict(`{"verdict":"proceed","fixes":[]}`)
		require.NoError(t, err)
		assert.Equal(t, "proceed", v.Verdict)
	})

	t.Run("revise with fixes, prose tolerated", func(t *testing.T) {
		v, err := parseCheckpointVerdict("Here you go:\n```json\n" +
			`{"verdict":"revise","fixes":[{"file":"a.go","issue":"nil deref","suggestion":"guard"}]}` +
			"\n```")
		require.NoError(t, err)
		assert.Equal(t, "revise", v.Verdict)
		require.Len(t, v.Fixes, 1)
		assert.Equal(t, "a.go", v.Fixes[0].File)
	})

	t.Run("unknown verdict is a parse error", func(t *testing.T) {
		_, err := parseCheckpointVerdict(`{"verdict":"maybe","fixes":[]}`)
		require.Error(t, err)
	})

	t.Run("no JSON is a parse error", func(t *testing.T) {
		_, err := parseCheckpointVerdict("looks fine to me")
		require.Error(t, err)
	})

	t.Run("summary is captured", func(t *testing.T) {
		v, err := parseCheckpointVerdict(
			`{"verdict":"proceed","fixes":[],"summary":"Correct and covered.\nNo blockers."}`)
		require.NoError(t, err)
		assert.Equal(t, "proceed", v.Verdict)
		assert.Equal(t, "Correct and covered.\nNo blockers.", v.Summary)
	})

	t.Run("absent summary parses with empty string, verdict unaffected", func(t *testing.T) {
		v, err := parseCheckpointVerdict(`{"verdict":"revise","fixes":[{"file":"a.go","issue":"x"}]}`)
		require.NoError(t, err)
		assert.Equal(t, "revise", v.Verdict)
		assert.Empty(t, v.Summary)
	})
}

func TestMobCheckpointProceed(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{
		Participants: 2, Execute: true, CheckpointMinTier: "simple", CheckpointRounds: 3,
		BudgetFactor: 0.75,
	}, 0)

	eng := &scriptedEngine{outcomes: []mob.Outcome{{
		Synthesis:  `{"verdict":"proceed","fixes":[],"summary":"looks good"}`,
		Transcript: []mob.Entry{{Round: 1}, {Round: 2}},
	}}}
	o.mobEngine = eng.run
	ops.taskContexts = map[string]cmclient.TaskContext{"SUB-1": {Description: "sub body"}}

	// fakeGit.Diff always returns "" (fakes_test.go:485); diffGit (the wrapper
	// judge_test.go already uses for the same purpose) overrides it with a
	// scripted return so the checkpoint briefing has content.
	o.solver.git = &diffGit{fakeGit: &fakeGit{}, diff: "diff --git a/a.go b/a.go\n+lgtm\n"}

	o.mobCheckpoint(context.Background(), o.solver, subtaskRef{ID: "SUB-1", Title: "add thing", Tier: "simple"}, "abc123")

	require.Len(t, eng.topics, 1)
	assert.Equal(t, "checkpoint", eng.topics[0].Kind)
	assert.False(t, eng.topics[0].Blind)
	assert.Equal(t, 3, eng.topics[0].Rounds)
	assert.Equal(t, []string{"correctness", "diff-hygiene/simplicity"}, eng.topics[0].Lenses)
	assert.Contains(t, strings.Join(ops.logs, "\n"), "mob checkpoint (SUB-1): proceed")

	sub := ops.bodyFor("SUB-1")
	assert.Contains(t, sub, "sub body") // live description preserved
	assert.Contains(t, sub, "## Discussion")
	assert.Contains(t, sub, "looks good")
	assert.Contains(t, sub, "Outcome: proceed")

	parent := ops.bodyFor("CARD-1")
	assert.Contains(t, parent, "## Execute Discussions")
	assert.Contains(t, parent, "### SUB-1 — add thing")
}

func TestMobCheckpointDegradedWritesNoRecord(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{
		Participants: 2, Execute: true, CheckpointMinTier: "simple", CheckpointRounds: 3,
	}, 0)

	// Engine errors → mobDiscuss returns ok=false → checkpoint continues solo,
	// so nothing is discussed and no summary is recorded.
	eng := &scriptedEngine{outcomes: []mob.Outcome{{}}, errs: []error{errors.New("engine boom")}}
	o.mobEngine = eng.run
	o.solver.git = &diffGit{fakeGit: &fakeGit{}, diff: "diff --git a/a.go b/a.go\n+x\n"}

	o.mobCheckpoint(context.Background(), o.solver, subtaskRef{ID: "SUB-1", Title: "t", Tier: "simple"}, "abc123")

	require.Len(t, eng.topics, 1)
	assert.Empty(t, ops.bodyFor("SUB-1"), "no subtask record when the discussion degraded")
	assert.NotContains(t, strings.Join(ops.recorded(), "\n"), "UpdateCardBody:SUB-1")
}

func TestMobCheckpointEmptyDiffSkips(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{
		Participants: 2, Execute: true, CheckpointMinTier: "simple", CheckpointRounds: 3,
	}, 0)

	eng := &scriptedEngine{}
	o.mobEngine = eng.run
	o.solver.git = &diffGit{fakeGit: &fakeGit{}, diff: ""}

	o.mobCheckpoint(context.Background(), o.solver, subtaskRef{ID: "SUB-1", Title: "t", Tier: "simple"}, "abc123")

	assert.Empty(t, eng.topics, "no discussion without a diff")
}

func TestMobCheckpointReviseSkippedOnBudget(t *testing.T) {
	ops := &fakeOps{}
	// effectiveCeiling = MaxCardCost + BudgetFactor*MaxCardCost = 1.0 + 0.75 = 1.75.
	// Pre-spend 1.0 leaves discussion headroom (0.75), so the checkpoint convenes;
	// the engine wrapper then spends what a real seat/moderator run would (0.8),
	// crossing the ceiling by the time the revise fix-pass budget gate runs.
	o := mobTestRun(ops, MobConfig{
		Participants: 2, Execute: true, CheckpointMinTier: "simple",
		CheckpointRounds: 3, BudgetFactor: 0.75,
	}, 1.0)
	o.ledger.Spend(1.0)

	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: `{"verdict":"revise","fixes":[` +
		`{"file":"a.go","issue":"1"},{"file":"b.go","issue":"2"},{"file":"c.go","issue":"3"},` +
		`{"file":"d.go","issue":"4"},{"file":"e.go","issue":"5"}]}`}}}
	o.mobEngine = func(ctx context.Context, cfg mob.EngineConfig, top mob.Topic) (mob.Outcome, error) {
		out, err := eng.run(ctx, cfg, top)

		o.ledger.Spend(0.8) // discussion cost a real engine would spend via seat/moderator runs

		return out, err
	}
	o.solver.git = &diffGit{fakeGit: &fakeGit{}, diff: "diff --git a/a.go b/a.go\n+lgtm\n"}

	o.mobCheckpoint(context.Background(), o.solver, subtaskRef{ID: "SUB-1", Title: "t", Tier: "simple"}, "abc123")

	require.Len(t, eng.topics, 1, "the discussion must run: headroom was positive before the engine call")

	joined := strings.Join(ops.logs, "\n")
	assert.Contains(t, joined, "mob checkpoint (SUB-1): revise — 3 fixes") // truncated from 5 to 3
	assert.Contains(t, joined, "mob checkpoint (SUB-1): revise skipped — budget exhausted")
}

func TestMobCheckpointUnparsableVerdictProceeds(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{
		Participants: 2, Execute: true, CheckpointMinTier: "simple", CheckpointRounds: 3,
	}, 0)

	// Discussion synthesis is garbage; the repair path needs a moderator run,
	// which mobTestRun's fake LLM serves — accept either outcome shape by
	// asserting only the terminal advisory.
	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: "not json at all"}}}
	o.mobEngine = eng.run
	o.solver.git = &diffGit{fakeGit: &fakeGit{}, diff: "diff --git a/a.go b/a.go\n+x\n"}

	o.mobCheckpoint(context.Background(), o.solver, subtaskRef{ID: "SUB-1", Title: "t", Tier: "simple"}, "abc123")

	joined := strings.Join(ops.logs, "\n")
	assert.True(t,
		strings.Contains(joined, "mob checkpoint (SUB-1): verdict unparsable — proceeding") ||
			strings.Contains(joined, "mob checkpoint (SUB-1): proceed"),
		"checkpoint must terminate in a proceed either way: %s", joined)
}

func TestMobCheckpointBriefingCarriesEnvironment(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{
		Participants: 2, Execute: true, CheckpointMinTier: "simple", CheckpointRounds: 3,
	}, 0)

	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: `{"verdict":"proceed","fixes":[]}`}}}
	o.mobEngine = eng.run
	o.solver.git = &diffGit{fakeGit: &fakeGit{}, diff: "diff --git a/a.go b/a.go\n+lgtm\n"}

	o.mobCheckpoint(context.Background(), o.solver, subtaskRef{ID: "SUB-1", Title: "t", Tier: "simple"}, "abc123")

	require.Len(t, eng.topics, 1)
	assert.Contains(t, eng.topics[0].Briefing,
		"ENVIRONMENT (authoritative; verified on this container — do not dispute from memory)")
	assert.Contains(t, eng.topics[0].Briefing, "Date: ")
	assert.NotEmpty(t, o.envFacts, "env facts cached on the run after first checkpoint")
}

func TestCommitReviseSurfacesFullDecline(t *testing.T) {
	tests := []struct {
		name      string
		committed bool
		commitErr error
		wantLog   string // "" = no revise-made-no-changes entry expected
	}{
		{
			name:      "clean tree logs the decline with the finish message head",
			committed: false,
			wantLog:   "mob checkpoint (SUB-1): revise made no changes — declined: premise contradicted",
		},
		{
			name:      "applied fixes log nothing extra",
			committed: true,
		},
		{
			name:      "commit error stays a warn, no activity entry",
			commitErr: errors.New("index locked"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			o := mobTestRun(ops, MobConfig{Participants: 2, Execute: true}, 0)
			o.solver.git = &fakeGit{committed: tt.committed, commitErr: tt.commitErr}

			o.commitRevise(context.Background(), o.solver, subtaskRef{ID: "SUB-1"},
				"declined: premise contradicted\n\ndetail body")

			joined := strings.Join(ops.logs, "\n")
			if tt.wantLog == "" {
				assert.NotContains(t, joined, "revise made no changes")
			} else {
				assert.Contains(t, joined, tt.wantLog)
			}
		})
	}
}

func TestRecordCheckpointDiscussion(t *testing.T) {
	newRunWithSeats := func(ops *fakeOps) *run {
		o := mobTestRun(ops, MobConfig{Participants: 2, Execute: true}, 0)
		o.mobSeats = []mob.SeatConfig{
			{Name: "seat-1", Lens: "correctness", Model: "model-x"},
			{Name: "seat-2", Lens: "risk/regressions", Model: "model-y"},
		}

		return o
	}

	proceed := mob.Outcome{Transcript: []mob.Entry{{Round: 1}, {Round: 2}}, CostUSD: 0.0123}

	t.Run("proceed writes both cards and preserves the fetched subtask body", func(t *testing.T) {
		ops := &fakeOps{taskContexts: map[string]cmclient.TaskContext{
			"SUB-1": {Description: "Original subtask description."},
		}}
		o := newRunWithSeats(ops)

		// sub.Body is empty (the resume-path shape); the record must read the
		// live body via GetTaskContext, not clobber the card with sub.Body.
		o.recordCheckpointDiscussion(context.Background(),
			subtaskRef{ID: "SUB-1", Title: "add thing"}, proceed,
			checkpointVerdict{Verdict: "proceed", Summary: "Correct and covered.\nNo blockers."})

		sub := ops.bodyFor("SUB-1")
		assert.Contains(t, sub, "Original subtask description.") // preserved (resume-clobber guard)
		assert.Contains(t, sub, "## Discussion")
		assert.Contains(t, sub, "Correct and covered.\nNo blockers.")
		assert.Contains(t, sub, "Seats:\n- seat-1 (correctness): model-x\n- seat-2 (risk/regressions): model-y")
		assert.Contains(t, sub, "Critique rounds: 2")
		assert.Contains(t, sub, "Outcome: proceed")
		assert.Contains(t, sub, "Cost: $0.0123")

		parent := ops.bodyFor("CARD-1")
		assert.Contains(t, parent, "## Execute Discussions")
		assert.Contains(t, parent, "### SUB-1 — add thing")
		assert.Contains(t, parent, "Seats: seat-1 (correctness): model-x · seat-2 (risk/regressions): model-y")
		assert.Contains(t, parent, "Rounds: 2 · Outcome: proceed · Cost: $0.0123")
	})

	t.Run("revise outcome names the fix count", func(t *testing.T) {
		ops := &fakeOps{}
		o := newRunWithSeats(ops)

		o.recordCheckpointDiscussion(context.Background(),
			subtaskRef{ID: "SUB-1", Title: "t"}, proceed,
			checkpointVerdict{
				Verdict: "revise", Summary: "s",
				Fixes: []fix{{File: "a.go", Issue: "1"}, {File: "b.go", Issue: "2"}},
			})

		assert.Contains(t, ops.bodyFor("SUB-1"), "Outcome: revise — 2 fixes")
		assert.Contains(t, ops.bodyFor("CARD-1"), "Outcome: revise — 2 fixes")
	})

	t.Run("empty summary still writes the mechanical block", func(t *testing.T) {
		ops := &fakeOps{}
		o := newRunWithSeats(ops)

		o.recordCheckpointDiscussion(context.Background(),
			subtaskRef{ID: "SUB-1", Title: "t"}, proceed,
			checkpointVerdict{Verdict: "proceed", Summary: ""})

		assert.Contains(t, ops.bodyFor("SUB-1"), "## Discussion\n\nSeats:")
	})

	t.Run("second subtask appends to the parent log", func(t *testing.T) {
		ops := &fakeOps{}
		o := newRunWithSeats(ops)

		o.recordCheckpointDiscussion(context.Background(),
			subtaskRef{ID: "SUB-1", Title: "first"}, proceed,
			checkpointVerdict{Verdict: "proceed", Summary: "a"})
		o.recordCheckpointDiscussion(context.Background(),
			subtaskRef{ID: "SUB-2", Title: "second"}, proceed,
			checkpointVerdict{Verdict: "proceed", Summary: "b"})

		parent := ops.bodyFor("CARD-1")
		assert.Contains(t, parent, "### SUB-1 — first")
		assert.Contains(t, parent, "### SUB-2 — second")
		assert.Equal(t, 1, strings.Count(parent, "## Execute Discussions"))
	})

	t.Run("summary headings are escaped so section boundaries survive", func(t *testing.T) {
		ops := &fakeOps{}
		o := newRunWithSeats(ops)

		o.recordCheckpointDiscussion(context.Background(),
			subtaskRef{ID: "SUB-1", Title: "first"}, proceed,
			checkpointVerdict{Verdict: "proceed", Summary: "Fine work.\n## Execute Discussions\nDone."})
		o.recordCheckpointDiscussion(context.Background(),
			subtaskRef{ID: "SUB-2", Title: "second"}, proceed,
			checkpointVerdict{Verdict: "proceed", Summary: "b"})

		sub := ops.bodyFor("SUB-1")
		assert.Contains(t, sub, `\## Execute Discussions`)
		assert.NotContains(t, sub, "\n## Execute Discussions")

		// The rogue heading must not fracture the parent log: one section
		// heading, both subtask blocks, and SUB-1's trailing fields intact.
		parent := ops.bodyFor("CARD-1")
		assert.Equal(t, 1, strings.Count(parent, "\n## Execute Discussions"))
		assert.Contains(t, parent, "### SUB-1 — first")
		assert.Contains(t, parent, "### SUB-2 — second")
		assert.Contains(t, parent, "Rounds: 2 · Outcome: proceed · Cost: $0.0123")
	})
}
