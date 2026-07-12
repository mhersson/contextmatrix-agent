package worker

import (
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
)

func TestMobConfigMapping(t *testing.T) {
	tests := []struct {
		name string
		spec *protocol.MobSpec
		want orchestrator.MobConfig
	}{
		{"nil spec is off", nil, orchestrator.MobConfig{}},
		{"participants below two is off", &protocol.MobSpec{Participants: 1}, orchestrator.MobConfig{}},
		{
			"full spec maps phases and guests",
			&protocol.MobSpec{
				Participants: 3,
				Phases:       []string{"plan", "review"},
				Rounds:       3,
				BudgetFactor: 0.5,
				Guests:       []protocol.GuestSpec{{Name: "laptop", URL: "http://g:1", Token: "tok"}},
			},
			orchestrator.MobConfig{
				Participants: 3, Plan: true, Review: true, Rounds: 3, BudgetFactor: 0.5,
				Guests: []orchestrator.MobGuest{{Name: "laptop", URL: "http://g:1", Token: "tok"}},
			},
		},
		{
			"zero rounds and factor take spec defaults",
			&protocol.MobSpec{Participants: 2, Phases: []string{"plan"}},
			orchestrator.MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75},
		},
		{
			"empty phases default to plan plus review",
			&protocol.MobSpec{Participants: 2, Rounds: 1, BudgetFactor: 0.6},
			orchestrator.MobConfig{Participants: 2, Plan: true, Review: true, Rounds: 1, BudgetFactor: 0.6},
		},
		{
			"unknown phases ignored, execute not mapped in this plan",
			&protocol.MobSpec{Participants: 2, Phases: []string{"review", "execute", "bogus"}, Rounds: 1, BudgetFactor: 0.6},
			orchestrator.MobConfig{Participants: 2, Review: true, Rounds: 1, BudgetFactor: 0.6},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mobConfig(tt.spec))
		})
	}
}

func TestMobGuestTokens(t *testing.T) {
	assert.Nil(t, mobGuestTokens(nil))

	spec := &protocol.MobSpec{
		Participants: 2,
		Guests: []protocol.GuestSpec{
			{Name: "a", URL: "http://a", Token: "tok-a"},
			{Name: "b", URL: "http://b"}, // token-less guest contributes nothing
			{Name: "c", URL: "http://c", Token: "tok-c"},
		},
	}
	assert.Equal(t, []string{"tok-a", "tok-c"}, mobGuestTokens(spec))
}
