package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"math"
	"sort"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// HarnessVersion stamps the eval baseline with the harness revision that produced
// it, so a baseline measured by an older scoring/parsing harness is recognizable.
// Bump on any change to scoring, the tamper guard, or the parse-artifact rule.
const HarnessVersion = "b2.5"

// DefaultZ is the Wilson confidence factor (≈95%) shared by the per-cell scores and
// the calibrated floor, so the floor and the scores it gates demonstrably live at one
// confidence level. Callers that need a different z (e.g. the raw Score helper in
// tests) pass it explicitly.
const DefaultZ = 1.96

// floorFraction calibrates the functional floor as a fraction of the achievable
// Wilson-LB ceiling for the battery size. A fixed bar (e.g. 0.8) becomes unreachable
// at small n because a perfect record's lower bound sits below it; the floor must
// track what the battery can actually award.
const floorFraction = 0.75

// CalibratedFloor returns the functional floor for a battery of n samples at
// confidence factor z: floorFraction × the Wilson lower bound of a perfect n/n
// record (the achievable ceiling). The reference model must clear this floor or the
// battery is presumed broken.
func CalibratedFloor(n int, z float64) float64 {
	return floorFraction * wilsonLowerBound(n, n, z)
}

// TaskLibraryHash returns a deterministic sha256 (hex) over the embedded fixture FS:
// every file's slash-path and contents, walked in sorted-path order. Two baselines
// with different hashes were measured against different task libraries and are not
// comparable.
func TaskLibraryHash() string {
	var paths []string

	_ = fs.WalkDir(fixturesFS, "fixtures", func(p string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil {
			return err
		}

		if !d.IsDir() {
			paths = append(paths, p)
		}

		return nil
	})

	sort.Strings(paths)

	h := sha256.New()

	for _, p := range paths {
		data, _ := fixturesFS.ReadFile(p) //nolint:errcheck
		// Include the path so a rename changes the hash even if contents are identical.
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))
}

// Outcome is one (model, task, sample) result. The runner (Task 4) produces these;
// scoring consumes Model/Role/Pass. Cost and Res support the matrix runner and audit.
type Outcome struct {
	Model string
	Role  registry.Role
	Task  string
	Pass  bool
	Cost  float64
	Res   harness.Result
	// Tampered is true when a coder run failed because it altered protected fixture
	// files (hidden tests, go.mod, helpers) rather than because the answer was wrong.
	// Always paired with Pass=false; reserved for the merge step to distinguish
	// "failed: tampered" from "failed: wrong answer" (no consumer reads it yet).
	Tampered bool
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
// Wilson lower bound of the pass rate. It is retained for tests and external callers
// that want raw scores over all outcomes; production scoring goes through
// MeasuredComplete, which additionally excludes partial-coverage and parse-artifact
// cells before scoring.
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

// DroppedCell records a (model, role) cell excluded from the merged baseline and
// why, so every drop can be logged — the spec forbids silent truncation.
type DroppedCell struct {
	Cell   CellKey
	Reason string
}

// MeasuredComplete scores only the cells of mr that are trustworthy to merge,
// returning the scored capability map plus the list of excluded cells (each with a
// reason). Two exclusion rules, both applied per (model, role) cell:
//
//   - partial coverage: fewer recorded outcomes than the expected battery size
//     (a budget abort or errors cut the cell short). The prior baseline value is
//     retained by the caller's merge — an incomplete cell never overwrites it.
//   - parse artifact: every recorded outcome FAILED and made zero tool calls. That
//     is the empty no-op signature of a tool-call format the client could not parse
//     (e.g. gpt-oss harmony), NOT a capability verdict, so it must not be written as
//     0.00. A cell with any pass — or any tool call — is a real measurement and is
//     kept (a reviewer can legitimately pass on text alone with no tool calls).
//
// A tampered outcome is a REAL failed trial (it made tool calls): it counts as
// present coverage and toward the failure rate, and does NOT trigger either rule.
func MeasuredComplete(mr MatrixResult) (map[string]map[registry.Role]float64, []DroppedCell) {
	type bucket struct {
		passes, total int
		anyToolCall   bool
	}

	cells := map[CellKey]*bucket{}

	for _, o := range mr.Outcomes {
		k := CellKey{Model: o.Model, Role: o.Role}

		b := cells[k]
		if b == nil {
			b = &bucket{}
			cells[k] = b
		}

		b.total++
		if o.Pass {
			b.passes++
		}

		if o.Res.ToolCallCount > 0 {
			b.anyToolCall = true
		}
	}

	out := map[string]map[registry.Role]float64{}

	var dropped []DroppedCell

	// Sort cells for a deterministic drop log and output order.
	keys := make([]CellKey, 0, len(cells))
	for k := range cells {
		keys = append(keys, k)
	}

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Model != keys[j].Model {
			return keys[i].Model < keys[j].Model
		}

		return keys[i].Role < keys[j].Role
	})

	for _, k := range keys {
		b := cells[k]

		if exp := mr.Expected[k]; exp > 0 && b.total < exp {
			dropped = append(dropped, DroppedCell{Cell: k, Reason: fmt.Sprintf("partial coverage %d/%d", b.total, exp)})

			continue
		}

		if b.passes == 0 && !b.anyToolCall {
			dropped = append(dropped, DroppedCell{Cell: k, Reason: fmt.Sprintf("parse artifact: all %d samples failed with zero tool calls", b.total)})

			continue
		}

		if out[k.Model] == nil {
			out[k.Model] = map[registry.Role]float64{}
		}

		out[k.Model][k.Role] = wilsonLowerBound(b.passes, b.total, DefaultZ)
	}

	return out, dropped
}

// AssertReferenceModel verifies the configured reference model clears the calibrated
// floor on EVERY role it was measured on this sweep. It is an eval-time canary: if the
// reference model — known-capable — scores below the floor on any role, the battery
// for that role is broken (parsing, provisioning, or fixture regression) and the whole
// measurement is suspect.
//
// Asserting per-role (not a single hardcoded role) closes two gaps: a `--role reviewer`
// sweep would otherwise find no coder entry and silently pass, and the reviewer side of
// a `--role all` sweep would go unchecked. A role with no measurement for the reference
// model is skipped (can't assert what wasn't run); if the reference model is absent
// from the measured set entirely, no role is asserted and no error is returned.
func AssertReferenceModel(measured map[string]map[registry.Role]float64, model string, floor float64) error {
	roles, ok := measured[model]
	if !ok {
		return nil
	}

	// Deterministic order so the reported role is stable when several are below floor.
	present := make([]registry.Role, 0, len(roles))
	for r := range roles {
		present = append(present, r)
	}

	sort.Slice(present, func(i, j int) bool { return present[i] < present[j] })

	for _, r := range present {
		if roles[r] < floor {
			return fmt.Errorf("battery appears broken: reference model %s scored %.3f on %s, below the calibrated floor %.3f", model, roles[r], r, floor)
		}
	}

	return nil
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
