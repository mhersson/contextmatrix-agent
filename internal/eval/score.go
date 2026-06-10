package eval

import (
	"math"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// Outcome is one (model, task, sample) result. The runner (Task 4) produces these;
// scoring consumes Model/Role/Pass. Cost and Res support the matrix runner and audit.
type Outcome struct {
	Model string
	Role  registry.Role
	Task  string
	Pass  bool
	Cost  float64
	Res   harness.Result
}

// wilsonLowerBound returns the lower bound of the Wilson score interval for
// `passes` successes in `n` trials at confidence factor z (1.96 ≈ 95%). It is the
// conservative capability estimate stored per (model, role): small n pulls the
// bound down so a lucky pass cannot promote a model across a tier. n==0 -> 0.
func wilsonLowerBound(passes, n int, z float64) float64 {
	if n == 0 {
		return 0
	}

	fn := float64(n)
	phat := float64(passes) / fn
	z2 := z * z
	denom := 1 + z2/fn
	center := phat + z2/(2*fn)
	margin := z * math.Sqrt(phat*(1-phat)/fn+z2/(4*fn*fn))

	lower := (center - margin) / denom
	if lower < 0 {
		return 0
	}

	return lower
}

// Score aggregates outcomes into capability scores per (model, role) using the
// Wilson lower bound of the pass rate.
func Score(outcomes []Outcome, z float64) map[string]map[registry.Role]float64 {
	type key struct {
		model string
		role  registry.Role
	}

	passes, total := map[key]int{}, map[key]int{}

	for _, o := range outcomes {
		k := key{o.Model, o.Role}

		total[k]++
		if o.Pass {
			passes[k]++
		}
	}

	out := map[string]map[registry.Role]float64{}
	for k, n := range total {
		if out[k.model] == nil {
			out[k.model] = map[registry.Role]float64{}
		}

		out[k.model][k.role] = wilsonLowerBound(passes[k], n, z)
	}

	return out
}

// TierRank buckets a capability score onto the registry's tier ladder:
// 3=complex(>=0.8) 2=moderate(>=0.6) 1=simple(>=0.4) 0=none. Used by the
// regression check to detect a dropped tier.
func TierRank(score float64) int {
	switch {
	case score >= 0.8:
		return 3
	case score >= 0.6:
		return 2
	case score >= 0.4:
		return 1
	default:
		return 0
	}
}
