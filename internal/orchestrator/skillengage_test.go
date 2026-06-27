package orchestrator

import (
	"context"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
)

type menuToolStub struct{}

func (menuToolStub) Name() string                                            { return "skill" }
func (menuToolStub) Schema() llm.Tool                                        { return llm.Tool{} }
func (menuToolStub) Execute(context.Context, map[string]any) (string, error) { return "", nil }
func (menuToolStub) MenuText() string                                        { return "- go-development: Use when writing Go.\n" }

func TestSkillEngageBlockContent(t *testing.T) {
	b := skillEngageBlock("- go-development: Use when writing Go.\n")

	assert.Contains(t, b, "`skill`", "names the skill tool")
	assert.Contains(t, b, "engage", "instructs engagement")
	assert.Contains(t, b, "Available skills:")
	assert.Contains(t, b, "- go-development: Use when writing Go.")
}

func TestRunSkillEngagePresentAndAbsent(t *testing.T) {
	with := &run{d: Deps{SkillTool: menuToolStub{}}}
	got := with.skillEngage()

	assert.Contains(t, got, "Available skills:")
	assert.Contains(t, got, "go-development")

	without := &run{d: Deps{}}
	assert.Empty(t, without.skillEngage(), "nil SkillTool -> empty block -> byte-identical prompts")
}
