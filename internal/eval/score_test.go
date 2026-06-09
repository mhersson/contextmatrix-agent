package eval

import (
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/stretchr/testify/assert"
)

func TestWilsonLowerBound(t *testing.T) {
	assert.Equal(t, 0.0, wilsonLowerBound(0, 0, 1.96))
	// All-pass at small n is pulled well below 1.0 (conservative).
	lb := wilsonLowerBound(3, 3, 1.96)
	assert.Less(t, lb, 0.5)
	assert.Greater(t, lb, 0.0)
	// More trials at 100% raises the bound.
	assert.Greater(t, wilsonLowerBound(20, 20, 1.96), wilsonLowerBound(3, 3, 1.96))
	// Half-pass sits below 0.5.
	assert.Less(t, wilsonLowerBound(5, 10, 1.96), 0.5)
}

func TestScoreGroupsByModelRole(t *testing.T) {
	outcomes := []Outcome{
		{Model: "m1", Role: registry.RoleCoder, Pass: true},
		{Model: "m1", Role: registry.RoleCoder, Pass: true},
		{Model: "m1", Role: registry.RoleReviewer, Pass: false},
		{Model: "m2", Role: registry.RoleCoder, Pass: false},
	}
	s := Score(outcomes, 1.96)
	assert.Greater(t, s["m1"][registry.RoleCoder], s["m1"][registry.RoleReviewer])
	assert.Equal(t, 0.0, s["m2"][registry.RoleCoder]) // 0/1 -> 0
}

func TestTierRank(t *testing.T) {
	assert.Equal(t, 0, TierRank(0.39))
	assert.Equal(t, 1, TierRank(0.4))
	assert.Equal(t, 2, TierRank(0.6))
	assert.Equal(t, 3, TierRank(0.8))
}
