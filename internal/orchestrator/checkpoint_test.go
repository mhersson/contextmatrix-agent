package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

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
}

func TestMobCheckpointProceed(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{
		Participants: 2, Execute: true, CheckpointMinTier: "simple", CheckpointRounds: 3,
		BudgetFactor: 0.75,
	}, 0)

	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: `{"verdict":"proceed","fixes":[]}`}}}
	o.mobEngine = eng.run

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
