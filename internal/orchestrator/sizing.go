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

// sizing is a subtask's two independent axes. Bar is how capable the model must
// be (registry.Tier, a cross-repo wire word), corrected one rung at a time and
// only on quality evidence. Budget is a step index into turnBudgetLadder - a
// factor of the operator's base cap, so a persisted step survives a base change
// - corrected only on turn-cap evidence. Volume never buys a stronger model.
type sizing struct {
	Bar    registry.Tier
	Budget int
}

// defaultBar is the conservative fallback for an absent or unrecognised bar:
// under-selecting a model for real work costs more than slightly over-paying.
// It is what tierFromString returns for anything it does not recognise.
const defaultBar = registry.TierModerate

// turnBudgetLadder scales a coder run's turn budget above the configured base.
// 2x is the ceiling: a wider rung would only compound the cost of a cap that is
// already retrying. Factors of the base rather than absolute floors, so lifting
// the operator's base lifts every rung with it.
var turnBudgetLadder = [...]float64{1.0, 1.5, 2.0}

// maxBudgetStep is the top rung. Derived from the ladder so the two cannot drift.
const maxBudgetStep = len(turnBudgetLadder) - 1

func defaultSizing() sizing { return sizing{Bar: defaultBar} }

// seedSizing maps the planner's wire word onto both axes at creation time.
func seedSizing(plannerTier string) sizing {
	bar := tierFromString(plannerTier)

	return sizing{Bar: bar, Budget: seedBudgetStep(bar)}
}

// wideSubtaskFiles is the declared-file count at which a fresh subtask opens
// one budget rung wider. The plan prompt's file guidance is ~5; a subtask
// listing this many was emitted under the cross-cutting exception and is known
// wide at emission time - the one case that needs a wider window and costs no
// inference to detect.
const wideSubtaskFiles = 7

// seedSubtaskSizing sizes a fresh subtask from the planner's tier word plus
// the volume the subtask itself declares in its Files: list. The tier word
// only ever answers risk; breadth arrives on the budget axis alone, so a wide
// subtask can never buy a more expensive model.
func seedSubtaskSizing(plannerTier, description string) sizing {
	s := seedSizing(plannerTier)

	if len(filePathTokens(filesSection(description))) >= wideSubtaskFiles {
		s = s.raiseBudget()
	}

	return s
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

// tierRank orders the ladder for comparisons: simple < moderate < complex < critical.
func tierRank(t registry.Tier) int {
	switch t {
	case registry.TierModerate:
		return 1
	case registry.TierComplex:
		return 2
	case registry.TierCritical:
		return 3
	default:
		return 0
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

// windowExhausted reports whether an attempt spent every turn it was given.
//
// It is deliberately a property of the MEASUREMENT rather than of the harness's
// mechanism: the coder family runs with the grace turn, which grants one
// terminal call after the cap and returns a clean result, so an exhausted run
// can arrive with no error at all. A model that lands its terminal call exactly
// on the last turn is in the same position and reads the same way here.
//
// A non-positive cap means the operator left the budget unset and the harness
// substituted its own, so this side does not know what the window was and must
// not guess one.
func windowExhausted(turns, maxTurns int) bool {
	return maxTurns > 0 && turns >= maxTurns
}

// metaKV is one marker's raw key/value pairs, including keys this package does
// not understand. Every writer must fetch, parse, mutate keys and re-serialise
// the WHOLE map - never rebuild from a subset - so a later stage can add keys
// without touching this code, and a turn cap cannot delete them.
type metaKV map[string]string

// markerFor is the marker a freshly sized card or subtask is created with: both
// axes plus the write-once planner word that produced them. One home for the
// three-key convention, so a creation site cannot spell it differently from
// another.
func markerFor(s sizing, seed string) metaKV {
	return setSizing(metaKV{"seed": seed}, s)
}

// setSizing writes both axes onto kv and returns it. Every marker writer goes
// through it, so the axis key names have exactly one home. A writer correcting
// an existing marker passes the map it PARSED, which is what lets keys this
// package does not understand round-trip untouched.
func setSizing(kv metaKV, s sizing) metaKV {
	kv["bar"] = string(s.Bar)
	kv["budget"] = strconv.Itoa(s.Budget)

	return kv
}

// metaRe matches the marker line. The value class is deliberately permissive so
// a malformed marker is still MATCHED, and therefore still stripped, rather
// than orphaned above a freshly written one.
var metaRe = regexp.MustCompile(`(?m)^[ \t]*<!--[ \t]*cm:meta((?:[ \t]+[a-z_]+=[^ \t>]*)*)[ \t]*-->[ \t]*\r?\n?`)

// legacyTierRe matches the single-axis cm:tier= marker; readMeta maps it
// through seedSizing so those cards keep their bar and budget. Permissive in
// the same way as metaRe, so an unrecognised word is stripped rather than left
// behind.
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

		return markerFor(s, m[1]), s
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
