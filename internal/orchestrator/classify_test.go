package orchestrator

import (
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/stretchr/testify/assert"
)

func TestIsBugLike(t *testing.T) {
	tests := []struct {
		name string
		tc   cmclient.TaskContext
		want bool
	}{
		{"type bug", cmclient.TaskContext{Type: "bug", Title: "Something"}, true},
		{"bug label", cmclient.TaskContext{Labels: []string{"bugfix"}, Title: "Something"}, true},
		{"fix title", cmclient.TaskContext{Title: "Fix the broken parser"}, true},
		{"body language", cmclient.TaskContext{Title: "Parser", Description: "it throws on empty input"}, true},
		{"plain feature", cmclient.TaskContext{Type: "task", Title: "Add a health endpoint"}, false},
		{"feature with 'should' in body is not a bug", cmclient.TaskContext{
			Type: "task", Title: "Paginate the cards endpoint",
			Description: "The endpoint should return 50 cards per page.",
		}, false},
		{"maintenance wins over bug label", cmclient.TaskContext{
			Type: "bug", Labels: []string{"dependencies"}, Title: "Bump go-git to v5.13",
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isBugLike(tt.tc))
		})
	}
}

func TestIsPureMaintenance(t *testing.T) {
	assert.True(t, isPureMaintenance(cmclient.TaskContext{
		Labels: []string{"chore"}, Title: "Rename the package",
	}))
	assert.False(t, isPureMaintenance(cmclient.TaskContext{
		Labels: []string{"chore"}, Title: "Investigate the flaky test",
	}))
	assert.False(t, isPureMaintenance(cmclient.TaskContext{
		Title: "Bump dep", // mechanical title but no maintenance label
	}))
}
