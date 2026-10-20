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
		{"error: inside a mockup block is no bug", cmclient.TaskContext{Type: "feature", Title: "Add the booking screen", Description: "## Solution\nBuild it.\n\n```mockup\n<p>error: that slot is taken</p>\n```\n"}, false},
		{"error: after a mockup block is", cmclient.TaskContext{Type: "feature", Title: "Add the booking screen", Description: "```mockup\n<p>x</p>\n```\nIt throws error: on save.\n"}, true},
		{"a longer fence holds a shorter backtick line", cmclient.TaskContext{Type: "feature", Title: "Add the list", Description: "````mockup\n```\npanic\n````\n"}, false},
		{"an unclosed mockup block runs to the end", cmclient.TaskContext{Type: "feature", Title: "Add the list", Description: "```mockup\nstack trace\n"}, false},
		{"a tilde fence hides nothing", cmclient.TaskContext{Type: "feature", Title: "Add the list", Description: "~~~mockup\npanic\n~~~\n"}, true},
		{"another language hides nothing", cmclient.TaskContext{Type: "feature", Title: "Add the list", Description: "```html\npanic\n```\n"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isBugLike(tt.tc))
		})
	}
}

func TestWithoutMockups(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"no block", "## Context\nwhy\n", "## Context\nwhy\n"},
		{"a block", "a\n```mockup\nerror: x\n```\nb\n", "a\nb\n"},
		{"trailing spaces on the fences", "a\n```mockup  \nx\n```\t\nb\n", "a\nb\n"},
		{"a longer fence holds a shorter backtick line", "a\n````mockup\n```\nx\n````\nb\n", "a\nb\n"},
		{"an unclosed block runs to the end", "a\n```mockup\nx\npanic\n", "a\n"},
		{"two blocks", "a\n```mockup\nx\n```\nb\n```mockup\ny\n```\nc\n", "a\nb\nc\n"},
		{"a tilde fence is no block", "a\n~~~mockup\nx\n~~~\n", "a\n~~~mockup\nx\n~~~\n"},
		{"an indented fence is no block", "a\n  ```mockup\nx\n  ```\n", "a\n  ```mockup\nx\n  ```\n"},
		{"another language is no block", "a\n```html\nx\n```\n", "a\n```html\nx\n```\n"},
		{"mockup plus words is no block", "a\n```mockup two\nx\n```\n", "a\n```mockup two\nx\n```\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, withoutMockups(tt.in)) })
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

func TestIsCreative(t *testing.T) {
	tests := []struct {
		name string
		tc   cmclient.TaskContext
		want bool
	}{
		{"plain feature is creative", cmclient.TaskContext{Type: "task", Title: "Add a config palette"}, true},
		{"bug is not creative", cmclient.TaskContext{Type: "bug", Title: "Fix the parser"}, false},
		{"maintenance is not creative", cmclient.TaskContext{Labels: []string{"chore"}, Title: "Rename the package"}, false},
		{"bug body language is not creative", cmclient.TaskContext{Title: "Palette", Description: "it crashes on empty input"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isCreative(tt.tc))
		})
	}
}
