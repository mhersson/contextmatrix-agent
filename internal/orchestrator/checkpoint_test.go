package orchestrator

import (
	"testing"

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
