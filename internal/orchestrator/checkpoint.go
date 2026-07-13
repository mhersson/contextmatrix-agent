package orchestrator

import (
	"encoding/json"
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// checkpointLenses is the execute-checkpoint lens priority table; callers
// slice [:seats] like planLenses/reviewLenses.
//
//nolint:unused // consumed by the checkpoint discussion wiring landing next
var checkpointLenses = []string{
	"correctness", "diff-hygiene/simplicity", "risk/regressions",
	"architecture-fit", "performance",
}

// tierRanks orders registry tiers for the checkpoint_min_tier floor.
var tierRanks = map[registry.Tier]int{
	registry.TierSimple:   0,
	registry.TierModerate: 1,
	registry.TierComplex:  2,
	registry.TierCritical: 3,
}

// checkpointMaxFixes bounds a revise verdict's fix list (spec: at most 3
// concrete fixes per checkpoint).
//
//nolint:unused // consumed by the checkpoint discussion wiring landing next
const checkpointMaxFixes = 3

// checkpointEligible reports whether sub gets an execute checkpoint on this
// run: mob on, execute phase live, and the subtask's tier at or above the
// configured floor.
func (o *run) checkpointEligible(sub subtaskRef) bool {
	cfg := o.d.Cfg.Mob
	if !cfg.enabled() || !cfg.Execute {
		return false
	}

	return tierRanks[tierOf(sub)] >= tierRanks[tierFromString(cfg.CheckpointMinTier)]
}

// checkpointVerdict is the moderator's synthesis decision for one execute
// checkpoint: proceed, or revise with concrete fixes.
type checkpointVerdict struct {
	Verdict string `json:"verdict"` // "proceed" | "revise"
	Fixes   []fix  `json:"fixes"`
}

// parseCheckpointVerdict extracts and validates the checkpoint synthesis
// JSON (tolerating prose / code fences, like parseVerdict). Any verdict
// other than proceed/revise is a parse error so the caller can take its
// single repair turn.
func parseCheckpointVerdict(s string) (checkpointVerdict, error) {
	raw, ok := extractJSON(s)
	if !ok {
		return checkpointVerdict{}, fmt.Errorf("no JSON object found in synthesis output")
	}

	var v checkpointVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return checkpointVerdict{}, fmt.Errorf("unmarshal checkpoint verdict JSON: %w", err)
	}

	if v.Verdict != "proceed" && v.Verdict != "revise" {
		return checkpointVerdict{}, fmt.Errorf("verdict must be %q or %q (got %q)", "proceed", "revise", v.Verdict)
	}

	return v, nil
}
