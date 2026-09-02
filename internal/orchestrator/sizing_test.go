package orchestrator

import (
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The ladder must reproduce the factors the pre-split single-tier turn cap
// applied, so the split changes no fresh run's turn cap, and must never hand
// the harness a pathological cap when the operator's base is unset or
// negative - turnBudget(0, step) returns 0 for every step, and the harness
// substitutes its own default of 30 (harness.go: `if cfg.MaxTurns <= 0 &&
// !cfg.Interactive`).
func TestTurnBudgetLadder(t *testing.T) {
	const base = 45

	assert.Equal(t, base, turnBudget(base, seedBudgetStep(registry.TierSimple)))
	assert.Equal(t, base, turnBudget(base, seedBudgetStep(registry.TierModerate)))
	assert.Equal(t, 68, turnBudget(base, seedBudgetStep(registry.TierComplex)),
		"complex must still get 1.5x base, rounded")
	assert.Equal(t, 90, turnBudget(base, seedBudgetStep(registry.TierCritical)),
		"critical must still get 2x base")

	prev := 0

	for step := range maxBudgetStep + 1 {
		got := turnBudget(base, step)
		assert.Greater(t, got, prev, "step %d must buy turns over step %d", step, step-1)
		prev = got
	}

	assert.Equal(t, turnBudget(base, maxBudgetStep), turnBudget(base, maxBudgetStep+9), "steps clamp")
	assert.Equal(t, base, turnBudget(base, -3), "negative steps clamp to the base")
	assert.Equal(t, 90, turnBudget(base, maxBudgetStep), "the ladder tops out at 2x base - no observed run ever needed more")

	for _, bad := range []int{0, -1} {
		assert.Equal(t, bad, turnBudget(bad, 2),
			"an unset base must pass through so the harness substitutes its own default, never become a 1-turn run")
	}
}

// The harness fires the wrap-up nudge on EXACT equality with the remaining
// turns (`cfg.MaxTurns-res.Turns == cfg.WrapUpTurns`), so a cap raised without
// a re-anchored reserve buys dithering room instead of working room: a fixed
// reserve of 5 against a 3x budget leaves 130 unsupervised turns and then says
// "finish now". coderTurnCfg returns the pair so that cannot drift.
func TestCoderTurnCfgMovesBothAxes(t *testing.T) {
	const base = 45

	turns, wrap := coderTurnCfg(base, 0)
	assert.Equal(t, base, turns)
	assert.Equal(t, wrapUpTurns, wrap, "an unescalated run keeps exactly today's reserve")

	share := float64(wrapUpTurns) / float64(base)

	for step := range maxBudgetStep + 1 {
		turns, wrap := coderTurnCfg(base, step)

		assert.InDelta(t, share, float64(wrap)/float64(turns), 0.02,
			"step %d: the finish-now window must hold a constant SHARE of the budget", step)
		assert.Less(t, wrap, turns, "a reserve at or above the cap means the nudge never fires")
	}

	// A tiny operator base must not produce a reserve that swallows the run.
	turns, wrap = coderTurnCfg(3, maxBudgetStep)
	assert.Less(t, wrap, turns)
}

// The marker is orchestrator-private state on a body a human can edit. Unlike
// parsePlan, which hard-rejects an unrecognised tier, reading it must NEVER
// fail: a malformed marker costs a conservative default, never a run.
func TestReadMetaIsTotal(t *testing.T) {
	cases := []struct {
		name string
		body string
		want sizing
	}{
		{"absent", "Do the thing.", defaultSizing()},
		{"well formed", "<!-- cm:meta bar=complex budget=2 -->\n\nDo it.", sizing{registry.TierComplex, 2}},
		{"budget omitted", "<!-- cm:meta bar=critical -->\n", sizing{registry.TierCritical, 0}},
		{"unknown bar word", "<!-- cm:meta bar=heroic budget=1 -->\n", sizing{defaultBar, 1}},
		{"budget over the ceiling", "<!-- cm:meta bar=simple budget=99 -->\n", sizing{registry.TierSimple, maxBudgetStep}},
		{"negative budget", "<!-- cm:meta bar=simple budget=-4 -->\n", sizing{registry.TierSimple, 0}},
		{"garbage budget", "<!-- cm:meta bar=simple budget=x -->\n", sizing{registry.TierSimple, 0}},
		{"legacy complex", "Do it.\n\n<!-- cm:tier=complex -->", sizing{registry.TierComplex, 1}},
		{"legacy critical", "<!-- cm:tier=critical -->\n", sizing{registry.TierCritical, 2}},
		{"legacy unknown", "<!-- cm:tier=heroic -->\n", defaultSizing()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := readMeta(tc.body)
			assert.Equal(t, tc.want, got)
		})
	}
}

// A later stage adds keys to this marker. A writer that re-serialises only the
// keys it understands silently deletes them - and the writers that run most
// often are the escalation paths, i.e. exactly the rows a later analysis needs.
func TestWriteMetaRoundTripsUnknownKeys(t *testing.T) {
	body := "<!-- cm:meta bar=simple budget=0 coupling=53 coupling_src=paths seed=simple -->\n\nDo it."

	kv, s := readMeta(body)
	require.Equal(t, sizing{registry.TierSimple, 0}, s)

	kv["budget"] = "1"
	out := writeMeta(body, kv)

	kv2, s2 := readMeta(out)
	assert.Equal(t, sizing{registry.TierSimple, 1}, s2)
	assert.Equal(t, "53", kv2["coupling"], "a key this stage does not understand must survive the rewrite")
	assert.Equal(t, "paths", kv2["coupling_src"])
	assert.Equal(t, "simple", kv2["seed"], "the planner's original estimate stays recoverable after escalation")
}

// A zero-value sizing is reachable from any subtaskRef literal that omits it.
// An emitted `bar=` would be unmatchable by the read regex, so it would never
// be stripped and a second marker would be prepended above the dead one on
// every later write.
func TestWriteMetaNormalisesAnEmptyBar(t *testing.T) {
	out := writeMeta("Do it.", metaKV{"bar": "", "budget": "0"})

	_, got := readMeta(out)
	assert.Equal(t, defaultBar, got.Bar)

	again := writeMeta(out, metaKV{"bar": "complex", "budget": "1"})
	assert.Equal(t, 1, strings.Count(again, "cm:meta"), "re-marking replaces, never accumulates")
	assert.Equal(t, "Do it.", stripMeta(again), "the prompt-facing body is the marker-free original")
}

// Why writeMeta PREPENDS.
//
// An appended marker on a card that already carries a section lands INSIDE that
// section's replace range, and the next recordCheckpointOnSubtask upsert of the
// same heading deletes it, silently reverting the run to the moderate default:
//
//	run 1 checkpoint  "Do it.\n\n## Discussion\n\nSeats: round 1\n"
//	run 1 turn cap    "Do it.\n\n## Discussion\n\nSeats: round 1\n\n<!-- marker -->"
//	run 2 checkpoint  "Do it.\n\n## Discussion\n\nSeats: round 2\n"   <- escalation gone
//
// upsertSection preserves lines[:start] verbatim, so a marker on line 1 cannot
// land inside any section and survives every rewrite. This test drives the
// escalate-after-checkpoint sequence rather than a bare upsert, because a bare
// upsert leaves an appended marker intact too - a test built on one would pass
// with the bug still present.
func TestMetaMarkerSurvivesEscalationThenSectionUpsert(t *testing.T) {
	body := writeMeta("Implement the thing.", metaKV{"bar": "moderate", "budget": "0", "seed": "moderate"})

	// A mob checkpoint records a discussion on the subtask card.
	body = upsertSection(body, "Discussion", "## Discussion\n\nSeats: round 1")

	// A turn cap re-marks the body one budget rung wider.
	kv, s := readMeta(body)
	kv["budget"] = "1"
	body = writeMeta(body, kv)

	require.Equal(t, 0, s.Budget)

	// The next run checkpoints the same subtask again.
	body = upsertSection(body, "Discussion", "## Discussion\n\nSeats: round 2")

	_, got := readMeta(body)
	assert.Equal(t, sizing{registry.TierModerate, 1}, got,
		"the correction must survive the next checkpoint's section upsert")
	assert.Contains(t, body, "Implement the thing.")
	assert.Contains(t, body, "Seats: round 2")
}

// Escalation is monotone and strictly per-axis: ten turn caps must never have
// bought a stronger model.
func TestEscalationIsMonotoneAndPerAxis(t *testing.T) {
	s := sizing{registry.TierSimple, 0}

	for range 10 {
		next := s.raiseBudget()
		assert.Equal(t, s.Bar, next.Bar)
		assert.GreaterOrEqual(t, next.Budget, s.Budget)
		s = next
	}

	assert.Equal(t, maxBudgetStep, s.Budget)
	assert.Equal(t, registry.TierSimple, s.Bar)

	for range 10 {
		next := s.raiseBar()
		assert.Equal(t, s.Budget, next.Budget)
		s = next
	}

	assert.Equal(t, registry.TierCritical, s.Bar)
}

// seedSizing is the single bridge from the planner's wire word onto both axes.
// The budget seed must reproduce the pre-split single-tier turn cap exactly or
// every fresh run's turn cap moves.
func TestSeedSizingMapsBothAxes(t *testing.T) {
	for word, want := range map[string]sizing{
		"simple":   {registry.TierSimple, 0},
		"moderate": {registry.TierModerate, 0},
		"complex":  {registry.TierComplex, 1},
		"critical": {registry.TierCritical, 2},
		"":         {registry.TierModerate, 0},
		"heroic":   {registry.TierModerate, 0},
	} {
		t.Run(word, func(t *testing.T) {
			assert.Equal(t, want, seedSizing(word))
		})
	}
}

// seedSubtaskSizing folds the subtask's declared Files: volume onto the
// budget axis at creation time. The bar never moves on volume - breadth is
// evidence about turns spent, not about how capable the model must be.
func TestSeedSubtaskSizing(t *testing.T) {
	wide := "Files:\n" +
		"- backend/src/main/java/a/One.java\n- backend/src/main/java/a/Two.java\n" +
		"- backend/src/main/java/a/Three.java\n- backend/src/main/java/a/Four.java\n" +
		"- backend/src/main/java/a/Five.java\n- backend/src/main/java/a/Six.java\n" +
		"- backend/src/main/java/a/Seven.java\n- backend/src/main/java/a/Eight.java\n"
	narrow := "Files:\n- backend/src/main/java/a/One.java\n- backend/src/test/java/a/OneTest.java\n"

	tests := []struct {
		name, tier, desc string
		wantBudget       int
	}{
		{"moderate narrow keeps base", "moderate", narrow, 0},
		{"moderate wide widens one rung", "moderate", wide, 1},
		{"complex wide widens above its seed", "complex", wide, 2},
		{"critical wide clamps at the top rung", "critical", wide, maxBudgetStep},
		{"no files section keeps the seed", "moderate", "just prose", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := seedSubtaskSizing(tt.tier, tt.desc)
			assert.Equal(t, tt.wantBudget, s.Budget)
			assert.Equal(t, seedSizing(tt.tier).Bar, s.Bar) // bar never moves on volume
		})
	}
}

// An escalated fix round exists to reach a STRONGER model than the round that
// failed. The synthesizer names a fix_tier per round, so a later verdict can
// name a weaker one than the failed round actually ran at; without a floor the
// escalation then climbs from that weaker base and hands the harder problem a
// weaker fixer. The floor applies only where the correction does: the base
// round and the no-escalate cleanup pass keep the per-round tier as-is.
func TestFixSizingNeverEscalatesBelowTheBarThatFailed(t *testing.T) {
	o := &run{cardSizing: sizing{Bar: registry.TierModerate}}
	o.fixBarSteps = 1
	o.lastFixBar = registry.TierComplex

	// The synthesizer lowered fix_tier to simple after a complex fix failed:
	// the escalated round still runs one rung above complex.
	got := o.fixSizing(fixRequest{Round: 2, FixTier: "simple"})
	assert.Equal(t, registry.TierCritical, got.Bar)
	// A floored bar carries its window with it: seedBudgetStep(critical) is 2,
	// not the base window the simple fix_tier would have seeded.
	assert.Equal(t, 2, got.Budget)

	// No failed round yet: the per-round fix_tier is the base, unfloored.
	o.fixBarSteps = 0
	got = o.fixSizing(fixRequest{Round: 1, FixTier: "simple"})
	assert.Equal(t, registry.TierSimple, got.Bar)

	// A non-escalating cleanup pass ignores the floor.
	o.fixBarSteps = 1
	got = o.fixSizing(fixRequest{Round: 2, FixTier: "simple", NoEscalate: true})
	assert.Equal(t, registry.TierSimple, got.Bar)
}

// The floor rises only for a round that FAILED its gate. markFixFailed is the
// single promotion point - the same trigger that charges the bar axis - so a
// green escalated round leaves nothing behind and one failure buys exactly one
// rung. runFixModel needs the full harness deps, so the round's own bar is set
// directly here; the promotion under test runs through markFixFailed.
func TestFixSizingFloorRisesOnlyWhenTheRoundFailed(t *testing.T) {
	t.Run("a failed round promotes its bar into the floor", func(t *testing.T) {
		o := &run{cardSizing: sizing{Bar: registry.TierModerate}}
		o.pendingFixBar = registry.TierComplex

		o.markFixFailed("left the verify red")

		assert.Equal(t, registry.TierComplex, o.lastFixBar)
		assert.Equal(t, registry.TierCritical, o.fixSizing(fixRequest{Round: 2, FixTier: "simple"}).Bar)
	})

	t.Run("a green round leaves no floor", func(t *testing.T) {
		// fixBarSteps is 1 from an EARLIER failure, so the floor read is live
		// and the probe discriminates: the complex round that just went green
		// earned no floor, so the climb starts from the synthesizer's simple.
		// Promoting on every round instead would make this critical.
		o := &run{cardSizing: sizing{Bar: registry.TierModerate}}
		o.fixBarSteps = 1
		o.pendingFixBar = registry.TierComplex

		assert.Equal(t, registry.TierModerate, o.fixSizing(fixRequest{Round: 2, FixTier: "simple"}).Bar)
	})
}
