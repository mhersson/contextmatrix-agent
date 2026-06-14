package eval

import (
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/stretchr/testify/assert"
)

func TestWilsonLowerBound(t *testing.T) {
	assert.InDelta(t, 0.0, wilsonLowerBound(0, 0, 1.96), 1e-9)
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
	assert.InDelta(t, 0.0, s["m2"][registry.RoleCoder], 1e-9) // 0/1 -> 0
}

func TestTierRank(t *testing.T) {
	assert.Equal(t, 0, TierRank(0.39))
	assert.Equal(t, 1, TierRank(0.4))
	assert.Equal(t, 2, TierRank(0.6))
	assert.Equal(t, 3, TierRank(0.8))
}

// TestFloorCalibration: the functional floor is calibrated against the achievable
// Wilson-LB ceiling for the battery size, not a fixed bar. At 15 samples the old
// fixed 0.8 complex bar EXCEEDS the achievable ceiling (a perfect 15/15 record's
// Wilson lower bound), so a fixed bar would be unreachable. The calibrated floor
// must sit strictly below the ceiling.
func TestFloorCalibration(t *testing.T) {
	const n = 15

	ceiling := wilsonLowerBound(n, n, 1.96)
	assert.Less(t, ceiling, 0.8, "a perfect 15/15 Wilson LB is below the old fixed 0.8 bar")

	floor := CalibratedFloor(n, 1.96)
	assert.Less(t, floor, ceiling, "calibrated floor must sit below the achievable ceiling")
	assert.InDelta(t, floorFraction*ceiling, floor, 1e-9)
	assert.Greater(t, floor, 0.0)
}

// TestTaskLibraryHash: the hash is deterministic (sorted paths + contents over the
// embedded fixture FS) and stable across calls, and non-empty.
func TestTaskLibraryHash(t *testing.T) {
	h1 := TaskLibraryHash()
	h2 := TaskLibraryHash()
	assert.Equal(t, h1, h2, "hash must be deterministic across calls")
	assert.NotEmpty(t, h1)
	assert.Len(t, h1, 64, "sha256 hex digest is 64 chars")
}
