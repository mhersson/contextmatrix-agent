// Package registry resolves roles to concrete model specs for the harness. It
// is caller-facing: the FSM-free harness loop never imports it. SelectByComplexity
// is the best-value selector: a normalized external prior carries the tier bar,
// blacklisted slugs are excluded, and eligible favorites win before the
// cost-optimal pick.
package registry

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
)

type Role string

const (
	RoleCoder    Role = "coder"
	RoleReviewer Role = "reviewer"
)

type Tier string

const (
	TierSimple   Tier = "simple"
	TierModerate Tier = "moderate"
	TierComplex  Tier = "complex"
	TierCritical Tier = "critical"
)

// tierRung pairs a tier with its default quality bar. tierLadder is the
// single ordered source both DefaultTierBars and the monotone check in
// TierBarsFromStrings are built from, so a tier added here is automatically
// present and ordered in both.
type tierRung struct {
	tier Tier
	bar  float64
}

var tierLadder = []tierRung{
	{TierSimple, 0.65},
	{TierModerate, 0.76},
	{TierComplex, 0.82},
	{TierCritical, 0.90},
}

// DefaultTierBars are the normalized-prior thresholds per complexity tier.
func DefaultTierBars() map[Tier]float64 {
	out := make(map[Tier]float64, len(tierLadder))
	for _, rung := range tierLadder {
		out[rung.tier] = rung.bar
	}

	return out
}

// TierBarsFromStrings converts an operator ladder to the typed map. It
// MERGES over the defaults rather than replacing them: a partial map raises
// or lowers the rungs it names and inherits the rest, so a one-line edit
// like {"critical": 0.95} cannot silently drop the other three bars to zero,
// which would admit every model carrying any prior while every Pick still
// reported AtBar() true.
//
// It also rejects a non-monotone ladder. An inverted ladder passes name and
// range validation but makes descent() treat the strictest tier as the
// weakest rung with nothing below it, so escalating would return a WORSE
// model and report no clamp. The check needs no threshold and catches
// transposed values.
func TierBarsFromStrings(in map[string]float64) (map[Tier]float64, error) {
	if len(in) == 0 {
		return nil, nil
	}

	out := DefaultTierBars()

	for name, bar := range in {
		t := Tier(name)
		if _, ok := out[t]; !ok {
			return nil, fmt.Errorf("tier bars: unknown tier %q (known: %v)",
				name, slices.Sorted(maps.Keys(DefaultTierBars())))
		}

		if math.IsNaN(bar) || bar < 0 || bar > 1 {
			return nil, fmt.Errorf("tier bars: %s must be in [0,1], got %g", name, bar)
		}

		out[t] = bar
	}

	for i := 1; i < len(tierLadder); i++ {
		hi, lo := tierLadder[i].tier, tierLadder[i-1].tier
		if out[hi] < out[lo] {
			return nil, fmt.Errorf("tier bars: ladder must not decrease: %s %g is below %s %g",
				hi, out[hi], lo, out[lo])
		}
	}

	return out, nil
}

// PickSource says HOW a selection was reached. It is orthogonal to AtBar,
// which says what the selection is WORTH: a SourceDefault pick may clear
// the requested bar and a SourceAuto pick may not.
type PickSource uint8

const (
	SourceAuto     PickSource = iota // best-value pick from a rung's pool
	SourceFavorite                   // an eligible operator favorite at the rung
	SourcePinned                     // an operator pin, synthesized by the caller
	SourceDefault                    // the operator's capable default; off the ladder
)

func (s PickSource) String() string {
	switch s {
	case SourceFavorite:
		return "favorite"
	case SourcePinned:
		return "pinned"
	case SourceDefault:
		return "capable-default"
	default:
		return "auto"
	}
}

// ModelSpec is what the caller feeds into harness.Config for a given role.
type ModelSpec struct {
	Model         string
	ContextWindow int // from the catalog; 0 if unknown
}

// Pick is one selection outcome. A ModelSpec alone cannot say "I could not
// do what you asked": a caller that only sees the chosen model has no way
// to tell an at-bar pick from a degraded fallback. Pick carries that
// provenance explicitly. It embeds ModelSpec so callers that only feed the
// harness need no edit.
type Pick struct {
	ModelSpec

	Role          Role
	RequestedTier Tier
	// MetTier is the strictest configured tier at or below RequestedTier
	// whose bar this model's prior clears; "" when it has no prior. It is
	// MEASURED for every Source - a pin that clears nothing reports "".
	MetTier      Tier
	RequestedBar float64
	MetBar       float64
	Prior        float64
	HasPrior     bool // separates "prior is 0" from "no prior"
	Source       PickSource
	Duplicate    bool    // panel seat repeating an earlier seat's model
	OK           bool    // false only when even the capable default is barred
	LowestBar    float64 // the bottom of the configured ladder that was walked
}

// AtBar reports that the pick clears the bar that was asked for. Callers
// using a tier as a GATE rather than a preference - resolveDecisionModel's
// floor, the authoritative review pass, the escalated fix round - branch on
// this. It is the reason SelectByComplexity returns a value instead of
// taking a Refuse|Degrade knob: the phase knows what a shortfall costs.
func (p Pick) AtBar() bool { return p.OK && p.MetTier == p.RequestedTier }

// BelowBar is the reportable shortfall: a real selection that did not meet
// the requested bar.
func (p Pick) BelowBar() bool { return p.OK && p.MetTier != p.RequestedTier }

// DistinctModels counts distinct slugs across picks. A panel whose seats
// collapse onto one model is not a panel; callers use this to say so.
func DistinctModels(picks []Pick) int {
	seen := make(map[string]bool, len(picks))
	for _, p := range picks {
		seen[p.Model] = true
	}

	return len(seen)
}

// favKey indexes operator-pinned favorites by complexity tier and (optionally)
// role. A zero Role applies the favorite list to every role at that tier.
type favKey struct {
	Tier Tier
	Role Role // "" = applies to all roles
}

// Registry maps roles to models, backed by the live catalog for window/price.
type Registry struct {
	capable   string
	catalog   llm.Catalog
	priors    Priors
	blacklist map[string]bool
	favorites map[favKey][]string
	creators  map[string]string // slug -> creator, behind the vendor-diversity preference
	sel       Selection         // selection config (price headroom, tier bars)
}

// Selection configures the best-value selector. Zero value is valid: headroom
// defaults to 1.5.
type Selection struct {
	PriceHeadroom float64 // <= 0 -> defaultPriceHeadroom

	// MaxCapability makes every pick choose the most capable candidate in the
	// tier regardless of price, and bypass operator favorites. Per-card, from
	// the trigger payload.
	MaxCapability bool

	// TierBars is the operator's quality ladder. Empty uses DefaultTierBars.
	// The map is the ONLY source of tier ordering: descent() sorts it, so an
	// operator ladder with different rungs walks correctly.
	TierBars map[Tier]float64
}

const defaultPriceHeadroom = 1.5

// NewRegistryFromParts builds a payload-driven registry: the live catalog plus
// CM-injected priors, blacklist, and favorites. Quality is the normalized prior
// only; there is no measured-capability gate.
func NewRegistryFromParts(cat llm.Catalog, pr Priors, blacklist map[string]bool, favorites map[favKey][]string, capable string) *Registry {
	if blacklist == nil {
		blacklist = map[string]bool{}
	}

	if favorites == nil {
		favorites = map[favKey][]string{}
	}

	return &Registry{
		capable:   capable,
		catalog:   cat,
		priors:    pr,
		blacklist: blacklist,
		favorites: favorites,
		sel:       Selection{PriceHeadroom: defaultPriceHeadroom},
	}
}

// SelectInput describes a single best-value selection request.
type SelectInput struct {
	Role      Role
	Tier      Tier
	EstTokens int             // window-fit estimate; 0 skips the window check
	Exclude   map[string]bool // diversity: models to avoid if alternatives exist
	// ExcludeVendors is a hard filter in candidates(); the panel walk applies
	// it softly (vendor-filtered attempt first, retry without on an empty pool).
	ExcludeVendors map[string]bool
}

// NewRegistry builds a priors-only registry with the given capable default.
// Selection is payload-driven: with no priors injected, SelectByComplexity
// always falls back to the capable default.
func NewRegistry(capableDefault string, catalog llm.Catalog) *Registry {
	return NewRegistryFromParts(catalog, Priors{}, nil, nil, capableDefault)
}

// WithCreators attaches the slug -> creator map behind the vendor-diversity
// preference. nil disables vendor tracking. Returns r for chaining.
func (r *Registry) WithCreators(creators map[string]string) *Registry {
	r.creators = creators

	return r
}

// WithTierBars replaces the quality ladder. nil restores the defaults.
// Returns r for chaining.
func (r *Registry) WithTierBars(bars map[Tier]float64) *Registry {
	r.sel.TierBars = bars

	return r
}

// Vendor is the model's vendor as the diversity preference sees it: the
// CM-supplied creator when known, else the slug prefix; "" when neither resolves.
func (r *Registry) Vendor(id string) string {
	return r.vendorOf(id)
}

// vendorOf resolves a model's vendor: the CM-supplied creator first, else the
// namespace prefix of a namespaced slug (OpenRouter-leg fallback for CMs that
// predate CandidateModel.Creator), else "". The two vocabularies (AA creator
// slugs like "zai" vs OR prefixes like "z-ai") never mix within one run: the
// fallback only fires when CM sent no creator for the slug.
func (r *Registry) vendorOf(id string) string {
	if v := r.creators[id]; v != "" {
		return v
	}

	if vendor, _, ok := strings.Cut(id, "/"); ok && vendor != "" {
		return vendor
	}

	return ""
}

// Has reports whether model is present in the live catalog. The orchestrator
// uses it to decide whether a card-pinned model slug is resolvable before
// honouring the pin.
func (r *Registry) Has(model string) bool {
	_, ok := r.catalog.Find(model)

	return ok
}

// ContextWindow returns model's context window from the live catalog, or 0 if
// the model is absent (0 disables the harness context-limit check for it).
func (r *Registry) ContextWindow(model string) int {
	e, ok := r.catalog.Find(model)
	if !ok {
		return 0
	}

	return e.ContextLength
}

// fitsWindow reports whether model's context window can hold estTokens. Models
// absent from the catalog are treated as fitting (fail-open; the harness still
// enforces context_limit at runtime).
func (r *Registry) fitsWindow(model string, estTokens int) bool {
	e, ok := r.catalog.Find(model)
	if !ok {
		return true
	}

	return e.ContextLength >= estTokens
}

// candidate is a model that passed the gate/bar/window filters, carried with the
// quality score and blended price used by the best-value rule.
type candidate struct {
	id      string
	quality float64
	price   float64
}

func (r *Registry) bars() map[Tier]float64 {
	if len(r.sel.TierBars) > 0 {
		return r.sel.TierBars
	}

	return DefaultTierBars()
}

// barFor returns the quality bar for t. A tier absent from the configured
// ladder has bar 0, so it can never gate a candidate out and descent always
// treats it as reachable. The zero-value Registry still works
// (TestTierBarsIncludeCritical builds one directly) because bars() falls
// back to DefaultTierBars.
func (r *Registry) barFor(t Tier) float64 { return r.bars()[t] }

// lowestBar is the bottom of the configured ladder: the floor a walk can ever
// reach before falling to the capable default.
func (r *Registry) lowestBar() float64 {
	return slices.Min(slices.Collect(maps.Values(r.bars())))
}

// descent lists the rungs a request walks: the requested tier, then every
// configured tier with a STRICTLY LOWER bar, highest bar first. It never
// walks up. Derived by sorting the bar table, so there is no second
// ordering to keep in sync and an operator ladder with different rungs
// walks correctly. Ties on bar break by tier name for determinism; note
// that two tiers configured to the same bar collapse into one rung.
func (r *Registry) descent(requested Tier) []Tier {
	bars := r.bars()
	want := bars[requested]

	rungs := make([]Tier, 0, len(bars))

	for t, b := range bars {
		if t != requested && b < want {
			rungs = append(rungs, t)
		}
	}

	slices.SortFunc(rungs, func(a, b Tier) int {
		if c := cmp.Compare(bars[b], bars[a]); c != 0 {
			return c
		}

		return cmp.Compare(a, b)
	})

	return append([]Tier{requested}, rungs...)
}

// metTierFor is the strictest configured tier at or below requested whose
// bar prior clears; "" when none does or the model has no prior. Every Pick
// gets its MetTier from here, so a pin and a walked-down auto pick are
// reported on the same scale and an aggregate over MetTier is meaningful.
func (r *Registry) metTierFor(prior float64, hasPrior bool, requested Tier) (Tier, float64) {
	if !hasPrior {
		return "", 0
	}

	bars := r.bars()

	for _, rung := range r.descent(requested) {
		if prior >= bars[rung] {
			return rung, bars[rung]
		}
	}

	return "", 0
}

// pickFor assembles a Pick for a chosen model, measuring MetTier from the
// model's own prior rather than from the rung it was found on.
func (r *Registry) pickFor(id string, in SelectInput, src PickSource) Pick {
	prior, has := r.priors.ForRole(id, in.Role)
	met, metBar := r.metTierFor(prior, has, in.Tier)

	return Pick{
		ModelSpec:     r.specFor(id),
		Role:          in.Role,
		RequestedTier: in.Tier,
		MetTier:       met,
		RequestedBar:  r.barFor(in.Tier),
		MetBar:        metBar,
		Prior:         prior,
		HasPrior:      has,
		Source:        src,
		OK:            true,
		LowestBar:     r.lowestBar(),
	}
}

// SelectByComplexity picks the best-value model for (role, tier). A
// candidate must be tools-capable, not excluded, not blacklisted, carry a
// normalized prior for the role clearing the tier bar, and fit the window
// estimate. An eligible operator favorite for the rung wins outright;
// otherwise the most capable candidate within PriceHeadroom of the cheapest
// wins, quality ties breaking to the cheaper model.
//
// When the requested tier's pool is empty the selection CLAMPS DOWN the
// configured ladder: it re-runs at the next tier with a strictly lower bar,
// and so on. Each rung is exactly the selection a direct request at that
// rung would have made, so escalating a tier can never yield a worse model
// than asking for less. Favorites and pins are declared exceptions to that
// invariant (a favorite bypasses the price band by design); see the package
// contract.
//
// Below the lowest configured bar the answer is the operator's capable
// default - the trigger's default_model, the serve default, or the
// compiled-in guard - marked SourceDefault and subject to the SAME hard
// filters as any candidate (Exclude, blacklist, ExcludeVendors, window
// fit). Hard-filtering the default is what keeps a model this run has
// already proven harness-incapable from being handed back indefinitely: it
// answers "what did the operator choose for this run?", not "what is
// selectable regardless of what has already failed?".
//
// OK is false only when even that default is excluded, blacklisted, or
// window-short. The caller decides what a refusal costs.
func (r *Registry) SelectByComplexity(in SelectInput) Pick {
	for _, rung := range r.descent(in.Tier) {
		at := in
		at.Tier = rung

		if id, src := r.pickAt(at); id != "" {
			return r.pickFor(id, in, src)
		}
	}

	if r.capable != "" && r.employable(r.capable, in) {
		return r.pickFor(r.capable, in, SourceDefault)
	}

	return Pick{Role: in.Role, RequestedTier: in.Tier, RequestedBar: r.barFor(in.Tier), LowestBar: r.lowestBar()}
}

// pickAt makes one rung's selection. "" means the rung is dry.
func (r *Registry) pickAt(in SelectInput) (string, PickSource) {
	if !r.sel.MaxCapability {
		// Favorites bypass the vendor-diversity preference: explicit
		// operator intent beats the emergent heuristic. Evaluated per rung,
		// so a clamped pick consults the favorites of the tier it actually
		// landed on. The favorite lookup runs vendor-blind and before the
		// vendor-filtered candidate pool, so a rung counts as non-dry when
		// an eligible favorite exists even if the vendor-filtered pool is
		// empty - a soft vendor preference must never silently override an
		// explicit favorite.
		blind := in
		blind.ExcludeVendors = nil

		if fav := r.favoriteAmong(r.candidates(blind), in.Tier, in.Role); fav != "" {
			return fav, SourceFavorite
		}
	}

	cands := r.candidates(in)
	if len(cands) == 0 {
		return "", SourceAuto
	}

	return bestValue(cands, r.headroom(), r.sel.MaxCapability), SourceAuto
}

// employable applies the hard filters - and only the hard filters - to a
// model that is not being judged on quality. The capable default answers a
// question the bars cannot ("what did the operator choose for this run?"),
// but it must never resurrect a model this run has already barred.
// Deliberately omitted: SupportsTools(). The capable default is typically an
// operator-configured slug absent from the live catalog, where tool support
// cannot be evaluated at all - requiring it here would refuse every
// out-of-catalog default rather than just the ones that legitimately lack
// tool support.
func (r *Registry) employable(id string, in SelectInput) bool {
	if in.Exclude[id] || r.blacklist[id] {
		return false
	}

	if len(in.ExcludeVendors) > 0 {
		if v := r.vendorOf(id); v != "" && in.ExcludeVendors[v] {
			return false
		}
	}

	return in.EstTokens <= 0 || r.fitsWindow(id, in.EstTokens)
}

func (r *Registry) headroom() float64 {
	if r.sel.PriceHeadroom <= 0 {
		return defaultPriceHeadroom
	}

	return r.sel.PriceHeadroom
}

func bestValue(cands []candidate, headroom float64, maxCapability bool) string {
	cheapest := cands[0].price
	for _, c := range cands[1:] {
		if c.price < cheapest {
			cheapest = c.price
		}
	}

	band := math.Inf(1)
	if !maxCapability {
		band = cheapest * headroom
	}

	best := candidate{}
	have := false

	for _, c := range cands {
		if c.price > band {
			continue
		}

		switch {
		case !have:
			best, have = c, true
		case c.quality > best.quality:
			best = c
		case c.quality == best.quality && c.price < best.price:
			best = c
		}
	}

	return best.id
}

// candidates returns the models passing every filter for the given input.
// Quality is the normalized prior for the role; a model with no prior for the
// role, a prior below the tier bar, no tool support, an exclusion, a blacklist
// entry, or a window that cannot hold the estimate is dropped.
func (r *Registry) candidates(in SelectInput) []candidate {
	bar := r.barFor(in.Tier)

	var cands []candidate

	for _, e := range r.catalog {
		if !e.SupportsTools() || in.Exclude[e.ID] || r.blacklist[e.ID] {
			continue
		}

		if len(in.ExcludeVendors) > 0 {
			// Models with no resolvable vendor are never vendor-filtered.
			if v := r.vendorOf(e.ID); v != "" && in.ExcludeVendors[v] {
				continue
			}
		}

		quality, ok := r.priors.ForRole(e.ID, in.Role)
		if !ok || quality < bar {
			continue
		}

		if in.EstTokens > 0 && !r.fitsWindow(e.ID, in.EstTokens) {
			continue
		}

		cands = append(cands, candidate{
			id:      e.ID,
			quality: quality,
			price:   e.PromptPricePerTok + e.CompletionPricePerTok,
		})
	}

	return cands
}

// favoriteAmong returns the first operator favorite for (tier, role) - then
// (tier, any role) - present in cands. Favorites are looked up at the tier
// the pick is being MADE on, never at a different one: a moderate favorite
// is an operator statement about moderate work, so it must not be promoted
// onto a higher rung whose bar it does not clear, and must not be skipped
// when a higher request clamps down onto the moderate rung.
//
// cands is the caller's already-computed pool, so eligibility (bar,
// blacklist, exclusions, window) is enforced by construction and the filter
// work is not repeated. pickAt passes the vendor-blind pool: explicit
// operator intent beats the emergent diversity heuristic.
//
// A favorite bypasses the price band, so it is the declared exception to
// the tier-monotonicity invariant - the same exemption a pin gets, for the
// same reason. See the package contract.
func (r *Registry) favoriteAmong(cands []candidate, tier Tier, role Role) string {
	if len(r.favorites) == 0 || len(cands) == 0 {
		return ""
	}

	eligible := make(map[string]bool, len(cands))
	for _, c := range cands {
		eligible[c.id] = true
	}

	for _, key := range []favKey{{Tier: tier, Role: role}, {Tier: tier}} {
		for _, slug := range r.favorites[key] {
			if eligible[slug] {
				return slug
			}
		}
	}

	return ""
}

// SelectReviewPanel returns exactly n seats for the review specialists:
// distinct models chosen by repeated SelectByComplexity with a growing
// Exclude set, each seat softly preferring vendors not yet seated. Because
// each pick walks the tier ladder, a seat that cannot stay at the requested
// tier takes a distinct model one rung down rather than duplicating the
// seat above it: for a panel, independent judgement from a lower-bar
// reviewer is worth more than a second copy of the higher-bar one, and
// costs less.
//
// Duplicate fill is the last resort. It duplicates the previous seat and
// sets Duplicate on the copy: three identical slugs with no Duplicate flag
// are indistinguishable from three independent judgements, and the
// synthesizer would read that as agreement.
//
// The panel is always n seats: callers index it positionally against a
// fixed lens list, so a short panel deletes a lens rather than thinning
// the panel.
func (r *Registry) SelectReviewPanel(in SelectInput, n int) []Pick {
	if n <= 0 {
		return nil
	}

	exclude := maps.Clone(in.Exclude)
	if exclude == nil {
		exclude = map[string]bool{}
	}

	usedVendors := maps.Clone(in.ExcludeVendors) // e.g. a Best-of-N pin's vendor
	if usedVendors == nil {
		usedVendors = map[string]bool{}
	}

	panel := make([]Pick, 0, n)

	var last Pick

	for len(panel) < n {
		seat := in
		seat.Exclude = exclude
		seat.ExcludeVendors = nil

		blind := r.SelectByComplexity(seat)

		// No distinct model remains at any rung, and even the capable
		// default is barred: repeat the last real pick.
		if !blind.OK {
			if len(panel) == 0 {
				return nil // nothing is selectable for this role at all
			}

			dup := last
			dup.Duplicate = true
			panel = append(panel, dup)

			continue
		}

		// The capable default sits below every configured rung. Once a real
		// seat already holds the panel, an off-ladder default is not a
		// second independent judgement - it is the same terminal fill as an
		// unselectable rung, so it repeats the previous seat instead of
		// occupying a seat of its own. The first seat is the exception:
		// with nothing on the panel yet, the default IS the answer.
		if blind.Source == SourceDefault && len(panel) > 0 {
			dup := last
			dup.Duplicate = true
			panel = append(panel, dup)

			continue
		}

		pick := blind

		// Soft vendor preference, bounded to the rung the vendor-blind pick
		// landed on: diversity breaks ties within a rung, it never
		// overrides the quality ladder. Accepting a filtered pick from a
		// lower rung would let a soft, emergent preference outrank a
		// measured bar - and high-prior models cluster in a handful of
		// vendors, so a 3-seat panel would routinely push seats 2-3 far
		// down the ladder chasing a fresh vendor. The price band still
		// re-anchors on the filtered subset, so a diverse seat may cost
		// more than the vendor-blind pick - accepted, that is the
		// documented cost of diversity.
		if len(usedVendors) > 0 {
			filtered := seat
			filtered.ExcludeVendors = usedVendors

			if f := r.SelectByComplexity(filtered); f.OK && f.MetTier == blind.MetTier {
				pick = f
			}
		}

		panel = append(panel, pick)
		last = pick
		exclude[pick.Model] = true

		if v := r.vendorOf(pick.Model); v != "" {
			usedVendors[v] = true
		}
	}

	return panel
}

// SelectDiscussionPanel returns n seats for mob session discussion, honoring
// the caller's exclusions (review discussions exclude the models that coded
// the card). It is the review-panel selection by construction - distinct
// models first, a flagged repeat of the last seat as the last resort, nil
// when nothing is selectable at all - named as its own seam so discussion
// selection can diverge from review selection without touching call sites.
func (r *Registry) SelectDiscussionPanel(in SelectInput, n int) []Pick {
	return r.SelectReviewPanel(in, n)
}

// SelectCandidateModels picks n coder models for a Best-of-N fan-out. pin, if
// non-empty, occupies slot 1 and is never degraded away; the remaining slots
// follow SelectReviewPanel's contract (distinct models first, a flagged
// repeat as the last resort). With no pin, an unselectable pool returns nil
// like SelectReviewPanel; with a pin, an unselectable auto pool instead fills
// the remaining slots with the pin itself, flagged as a repeat, so a pinned
// fan-out still gets n candidates.
func (r *Registry) SelectCandidateModels(in SelectInput, n int, pin string) []Pick {
	if n <= 0 {
		return nil
	}

	if pin == "" {
		return r.SelectReviewPanel(in, n)
	}

	next := in
	next.Exclude = map[string]bool{pin: true}

	for id := range in.Exclude {
		next.Exclude[id] = true
	}

	// The pin occupies a seat, so its vendor counts as seated for the
	// auto-picked slots (fresh map; the caller's is never mutated).
	if v := r.vendorOf(pin); v != "" {
		next.ExcludeVendors = map[string]bool{v: true}
		for u := range in.ExcludeVendors {
			next.ExcludeVendors[u] = true
		}
	}

	// The pin is operator intent and is never degraded away - but its MetTier
	// and Prior are measured like every other seat's. Asserting MetTier ==
	// in.Tier would make the field mean "measured" for auto picks and
	// "asserted" for pins, and any aggregate over it would then be silently
	// wrong for every pinned run. Source SourcePinned is what carries the
	// authority; AtBar carries the measurement.
	pinPick := r.pickFor(pin, in, SourcePinned)

	out := make([]Pick, 0, n)
	out = append(out, pinPick)

	rest := r.SelectReviewPanel(next, n-1)
	if rest == nil && n > 1 {
		// Nothing else is selectable: fill the remaining seats with the pin,
		// flagged, so the fan-out still gets n candidates.
		for range n - 1 {
			dup := pinPick
			dup.Duplicate = true
			out = append(out, dup)
		}

		return out
	}

	return append(out, rest...)
}

// specFor builds a ModelSpec for id, filling the context window from the catalog.
func (r *Registry) specFor(id string) ModelSpec {
	spec := ModelSpec{Model: id}
	if e, ok := r.catalog.Find(id); ok {
		spec.ContextWindow = e.ContextLength
	}

	return spec
}
