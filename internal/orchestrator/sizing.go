package orchestrator

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// sizing is the two-axis replacement for the single complexity tier.
//
// Bar is how capable the model must be. Its failure mode is SILENT - a model
// below the bar ships a subtly wrong change that verify and the panel may both
// pass - so its only evidence is a quality failure, and it is corrected one
// rung at a time. Its vocabulary stays registry.Tier because that vocabulary is
// a cross-repo wire contract: CM keys operator favorites by tier and role, and
// spells checkpoint_min_tier in the same four words.
//
// Budget is how many harness turns the run may spend, held as a STEP INDEX into
// turnBudgetLadder rather than an absolute count: the operator's base cap is a
// knob that can change between runs, so persisting a step keeps that base
// authoritative. Its failure mode is LOUD - the cap trips and the phase parks -
// so a turn cap is evidence about volume and about nothing else.
//
// One word answered both questions before this type existed, which meant a turn
// cap bought a more expensive model on every later attempt.
type sizing struct {
	Bar    registry.Tier
	Budget int
}

// defaultBar is the conservative fallback for an absent or unrecognised bar:
// under-selecting a model for real work costs more than slightly over-paying.
// It is what tierFromString returns for anything it does not recognise.
const defaultBar = registry.TierModerate

// turnBudgetLadder scales a coder run's turn budget above the configured base.
// Steps 1 and 2 are the complex and critical factors this ladder replaces, so
// a fresh run's cap is unchanged. Step 3 exists because critical used to be the
// ceiling, and a cap there escalated nothing at all.
//
// Factors of the base rather than absolute floors, so lifting the operator's
// base lifts every rung with it.
var turnBudgetLadder = [...]float64{1.0, 1.5, 2.0, 3.0}

// maxBudgetStep is the top rung. Derived from the ladder so the two cannot drift.
const maxBudgetStep = len(turnBudgetLadder) - 1

func defaultSizing() sizing { return sizing{Bar: defaultBar} }

// seedSizing maps the planner's wire word onto both axes at creation time. The
// budget seed reproduces the factors the pre-split turn cap applied, so a fresh
// run's cap is byte-identical either side of the change.
func seedSizing(plannerTier string) sizing {
	bar := tierFromString(plannerTier)

	return sizing{Bar: bar, Budget: seedBudgetStep(bar)}
}

// seedBudgetStep is the ladder rung a bar opens at.
func seedBudgetStep(bar registry.Tier) int {
	switch bar {
	case registry.TierComplex:
		return 1
	case registry.TierCritical:
		return 2
	default:
		return 0
	}
}

// raiseBar climbs one rung of the quality ladder, leaving the budget alone.
// Critical is the ceiling, so it returns an equal value there - which is what
// lets a caller detect an exhausted axis by comparison.
func (s sizing) raiseBar() sizing {
	s.Bar = escalateTier(s.Bar)

	return s
}

// raiseBudget widens the turn budget one rung, leaving the bar alone.
func (s sizing) raiseBudget() sizing {
	s.Budget = min(s.Budget+1, maxBudgetStep)

	return s
}

// escalateTier is one tier up; critical stays critical.
func escalateTier(t registry.Tier) registry.Tier {
	switch t {
	case registry.TierSimple:
		return registry.TierModerate
	case registry.TierModerate:
		return registry.TierComplex
	case registry.TierComplex:
		return registry.TierCritical
	default:
		return registry.TierCritical
	}
}

// turnBudget is the harness turn cap for a ladder step against the operator's
// base. A base of zero or less passes through unchanged: the harness
// substitutes its own default there, and turning that into a 1-turn run would
// be a far worse failure than the misconfiguration it came from.
func turnBudget(base, step int) int {
	if base <= 0 {
		return base
	}

	step = min(max(step, 0), maxBudgetStep)

	return max(1, int(math.Round(float64(base)*turnBudgetLadder[step])))
}

// wrapUpFor re-anchors the wrap-up reserve to the effective window.
//
// The harness injects the nudge on exact equality with the REMAINING turns, so
// a constant reserve against a widened cap buys unsupervised dithering room and
// then says "finish now" far too late. Holding the reserve at a constant SHARE
// of the window keeps the nudge where it was designed to land. Clamped strictly
// below the window, because a reserve at or above the cap is a nudge that never
// fires. An unwidened window keeps exactly today's constant.
func wrapUpFor(base, eff int) int {
	if base <= 0 || eff <= base {
		return wrapUpTurns
	}

	return min(max(wrapUpTurns, int(math.Round(float64(wrapUpTurns*eff)/float64(base)))), eff-1)
}

// coderTurnCfg returns the turn cap and its wrap-up reserve together, so a cap
// raise cannot happen without a reserve raise.
func coderTurnCfg(base, step int) (maxTurns, wrapUp int) {
	maxTurns = turnBudget(base, step)

	return maxTurns, wrapUpFor(base, maxTurns)
}

// metaKV is one marker's raw key/value pairs, including keys this package does
// not understand. Every writer must fetch, parse, mutate keys and re-serialise
// the WHOLE map - never rebuild from a subset - so a later stage can add keys
// without touching this code, and a turn cap cannot delete them.
type metaKV map[string]string

// metaRe matches the marker line. The value class is deliberately permissive so
// a malformed marker is still MATCHED, and therefore still stripped, rather
// than orphaned above a freshly written one.
var metaRe = regexp.MustCompile(`(?m)^[ \t]*<!--[ \t]*cm:meta((?:[ \t]+[a-z_]+=[^ \t>]*)*)[ \t]*-->[ \t]*\r?\n?`)

// legacyTierRe matches the single-axis marker every card created before this
// change carries. It is read forever and mapped through seedSizing, so every
// live card keeps the bar and the turn budget it has today. No sweep, no
// backfill. Permissive in the same way as metaRe, so an unrecognised legacy
// word is stripped rather than left behind.
var legacyTierRe = regexp.MustCompile(`(?m)^[ \t]*<!--[ \t]*cm:tier=([a-z]+)[ \t]*-->[ \t]*\r?\n?`)

// readMeta parses the marker off a card body. It NEVER fails: an absent marker,
// an unknown bar word, and a missing, garbage or out-of-range budget all
// degrade to the conservative default. This is deliberately the opposite of
// parsePlan's hard reject - the plan is a wire contract a model emits, while
// this is orchestrator-private state on a body a human can edit.
func readMeta(body string) (metaKV, sizing) {
	if m := metaRe.FindStringSubmatch(body); m != nil {
		kv := metaKV{}

		for _, f := range strings.Fields(m[1]) {
			k, v, ok := strings.Cut(f, "=")
			if ok {
				kv[k] = v
			}
		}

		return kv, sizing{Bar: tierFromString(kv["bar"]), Budget: clampBudget(kv["budget"])}
	}

	if m := legacyTierRe.FindStringSubmatch(body); m != nil {
		s := seedSizing(m[1])

		return metaKV{
			"bar":    string(s.Bar),
			"budget": strconv.Itoa(s.Budget),
			"seed":   m[1],
		}, s
	}

	return metaKV{}, defaultSizing()
}

// clampBudget reads a budget value into the ladder's range. Anything
// unparseable is step 0 - the base budget every run starts at.
func clampBudget(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}

	return min(max(n, 0), maxBudgetStep)
}

// writeMeta replaces every marker form on body with one prepended cm:meta line.
//
// PREPENDED, not appended: upsertSection preserves the lines above its heading
// verbatim, so a marker on line 1 cannot land inside any section's replace
// range. An appended marker on a body that already carries a section ends up
// inside it, and the next upsert of that heading deletes it - which is how a
// persisted escalation is lost between runs on any card with a checkpoint
// discussion.
//
// The bar is normalised before serialisation: an empty or unrecognised value
// is replaced with defaultBar so the marker's on-disk value is always one of
// the four known bar words - a human reading the raw card body sees a real
// bar, never an empty or nonsense token.
func writeMeta(body string, kv metaKV) string {
	if !validTiers[kv["bar"]] {
		kv["bar"] = string(defaultBar)
	}

	keys := slices.Sorted(maps.Keys(kv))
	parts := make([]string, 0, len(keys))

	for _, k := range keys {
		parts = append(parts, k+"="+kv[k])
	}

	return fmt.Sprintf("<!-- cm:meta %s -->\n\n%s", strings.Join(parts, " "), stripMeta(body))
}

// stripMeta returns the prompt-facing body: every marker form removed. The
// marker is orchestrator state and must never reach a model.
func stripMeta(body string) string {
	body = metaRe.ReplaceAllString(body, "")
	body = legacyTierRe.ReplaceAllString(body, "")

	return strings.TrimSpace(body)
}

// budgetLabel renders a budget step for a human: the factor of the operator's
// configured base that rung buys. The step index means nothing to a reader of
// the card; the factor does.
func budgetLabel(step int) string {
	step = min(max(step, 0), maxBudgetStep)

	f := turnBudgetLadder[step]
	if f == 1 {
		return "base"
	}

	return strconv.FormatFloat(f, 'g', -1, 64) + "x base"
}
