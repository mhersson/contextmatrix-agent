package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPriorsForRole(t *testing.T) {
	coder, reviewer := 0.92, 0.90
	p := Priors{Models: map[string]PriorEntry{
		"openai/gpt-5.5": {Coder: &coder, Reviewer: &reviewer},
	}}

	score, ok := p.ForRole("openai/gpt-5.5", RoleCoder)
	assert.True(t, ok)
	assert.InDelta(t, 0.92, score, 1e-9)

	score, ok = p.ForRole("openai/gpt-5.5", RoleReviewer)
	assert.True(t, ok)
	assert.InDelta(t, 0.90, score, 1e-9)

	_, ok = p.ForRole("unknown/model", RoleCoder)
	assert.False(t, ok)
}

func TestPriorsForRoleOmittedVsExplicitZero(t *testing.T) {
	// A model entry that sets only coder has NO reviewer prior (ok == false) —
	// the selector must fall back to measured scores, not treat it as a prior
	// of zero. An explicit 0 is a deliberate curator statement: (0, true).
	onlyCoder, zeroCoder, zeroReviewer := 0.88, 0.70, 0.0
	p := Priors{Models: map[string]PriorEntry{
		"only/coder":    {Coder: &onlyCoder},
		"zero/reviewer": {Coder: &zeroCoder, Reviewer: &zeroReviewer},
	}}

	score, ok := p.ForRole("only/coder", RoleCoder)
	assert.True(t, ok)
	assert.InDelta(t, 0.88, score, 1e-9)

	_, ok = p.ForRole("only/coder", RoleReviewer)
	assert.False(t, ok, "omitted role must report no prior, not zero")

	score, ok = p.ForRole("zero/reviewer", RoleReviewer)
	assert.True(t, ok, "explicit zero is a real prior")
	assert.InDelta(t, 0.0, score, 1e-9)
}
