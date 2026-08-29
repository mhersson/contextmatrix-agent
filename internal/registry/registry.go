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

// DistinctModels: a panel whose seats collapse onto one model is not a
// panel; callers use this to say so.
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
// treats it as reachable. The zero-value Registry still works because bars()
// falls back to DefaultTierBars.
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
// invariant: both are operator intent, and operator intent outranks a rule
// the selector derived, so a favorite bypassing the price band may out-quality
// a higher tier's automatic pick.
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
	pick, _ := r.SelectByComplexityReport(in)

	return pick
}

// SelectionReport explains where a selection landed and who competed for it.
// Rung is the tier the pick was made on and Bar that rung's configured bar;
// both are empty when the walk fell through the ladder to the capable
// default, which has no rung. Pool lists every candidate that reached the
// rung with its outcome; FilteredOut summarizes why the rest of the catalog
// never got there. A favorite pick reports the pool of the rung it was
// looked up on, the favorite marked selected.
type SelectionReport struct {
	Rung Tier
	Bar  float64
	Pool []PoolEntry
	// FilteredOut is reason-aggregated with the model slugs in catalog
	// order, so a growing Exclude set reads as one growing entry.
	FilteredOut []FilteredOutEntry
}

// PoolOutcome classifies how one pool candidate fared.
type PoolOutcome string

const (
	// PoolSelected: the candidate is the pick.
	PoolSelected PoolOutcome = "selected"
	// PoolInBand: the candidate was inside the price band but lost on
	// quality, or tied on quality and lost to a cheaper model.
	PoolInBand PoolOutcome = "in-band"
	// PoolOutOfBand: the candidate's price exceeds the cheapest candidate
	// times the headroom. With MaxCapability the band is unbounded, so no
	// candidate is out of band.
	PoolOutOfBand PoolOutcome = "out-of-band"
)

// PoolEntry is one candidate that reached a rung's pool.
type PoolEntry struct {
	Model string
	// Prior is the model's normalized prior for the requested role.
	Prior float64
	// Price is the combined prompt+completion price per token.
	Price   float64
	Outcome PoolOutcome
}

// FilterReason says why a catalog model never reached a rung's pool.
type FilterReason string

const (
	FilterPriorBelowBar   FilterReason = "prior-below-bar"
	FilterNoPrior         FilterReason = "no-prior-for-role"
	FilterNotToolsCapable FilterReason = "not-tools-capable"
	FilterExcluded        FilterReason = "excluded"
	FilterBlacklisted     FilterReason = "blacklisted"
	FilterVendorExcluded  FilterReason = "vendor-excluded"
	FilterWindowTooSmall  FilterReason = "window-too-small"
)

// FilteredOutEntry aggregates every catalog model kept out of a rung's pool
// by the same reason.
type FilteredOutEntry struct {
	Reason FilterReason
	Models []string
}

// SelectByComplexityReport is SelectByComplexity with the competing pool:
// the pick the plain selector returns, plus a report classifying every
// catalog model at the rung the pick landed on. Pins and off-ladder picks
// synthesized by callers never go through the selector and get no report.
func (r *Registry) SelectByComplexityReport(in SelectInput) (Pick, SelectionReport) {
	for _, rung := range r.descent(in.Tier) {
		at := in
		at.Tier = rung

		res := r.selectAtRung(at)
		if res.winner == "" {
			continue
		}

		pool := make([]PoolEntry, len(res.pool))
		for i, c := range res.pool {
			pool[i] = PoolEntry{Model: c.id, Prior: c.quality, Price: c.price, Outcome: res.outcomes[i]}
		}

		return r.pickFor(res.winner, in, res.source),
			SelectionReport{Rung: rung, Bar: r.barFor(rung), Pool: pool, FilteredOut: res.filtered}
	}

	if r.capable != "" && r.employable(r.capable, in) {
		return r.pickFor(r.capable, in, SourceDefault), SelectionReport{}
	}

	return Pick{Role: in.Role, RequestedTier: in.Tier, RequestedBar: r.barFor(in.Tier), LowestBar: r.lowestBar()},
		SelectionReport{}
}

// rungResult is one rung's outcome: the winner with its source, and the
// classification of every catalog model against that rung - the pool the
// winner was drawn from with per-candidate outcomes, and the reason buckets
// for the models that never reached it.
type rungResult struct {
	winner   string
	source   PickSource
	pool     []candidate
	outcomes []PoolOutcome
	filtered []FilteredOutEntry
}

// selectAtRung makes one rung's selection. An empty winner means the rung
// is dry. The pool is the pool the pick was actually made from: the
// vendor-blind classification when a favorite fired (explicit operator
// intent beats the vendor heuristic, so the vendor filter does not apply to
// it), the vendor-filtered one otherwise.
func (r *Registry) selectAtRung(in SelectInput) rungResult {
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

		pool, filtered := r.classify(blind)
		if fav := r.favoriteAmong(pool, in.Tier, in.Role); fav != "" {
			// A favorite bypasses the price band, so it is marked selected
			// wherever it sits; the band classification still shows the
			// remaining candidates what the automatic rule would have done.
			band := priceBand(pool, r.headroom(), false)
			_, bandWinner := valuePick(pool, band)
			outcomes := classifyPool(pool, band, bandWinner)

			favIdx := slices.IndexFunc(pool, func(c candidate) bool { return c.id == fav })
			outcomes[favIdx] = PoolSelected

			if bandWinner != favIdx && bandWinner >= 0 {
				outcomes[bandWinner] = PoolInBand
			}

			return rungResult{winner: fav, source: SourceFavorite, pool: pool, outcomes: outcomes, filtered: filtered}
		}
	}

	pool, filtered := r.classify(in)
	if len(pool) == 0 {
		return rungResult{filtered: filtered}
	}

	band := priceBand(pool, r.headroom(), r.sel.MaxCapability)
	best, bandWinner := valuePick(pool, band)

	return rungResult{
		winner:   best.id,
		source:   SourceAuto,
		pool:     pool,
		outcomes: classifyPool(pool, band, bandWinner),
		filtered: filtered,
	}
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

// priceBand returns the price ceiling the best-value rule admits: the
// cheapest candidate times the headroom, or unbounded with MaxCapability.
func priceBand(cands []candidate, headroom float64, maxCapability bool) float64 {
	cheapest := cands[0].price
	for _, c := range cands[1:] {
		if c.price < cheapest {
			cheapest = c.price
		}
	}

	if maxCapability {
		return math.Inf(1)
	}

	return cheapest * headroom
}

// valuePick applies the best-value rule to the in-band candidates: the
// highest quality inside the band, quality ties breaking to the cheaper
// model. It returns the winner and its index in cands; -1 when every
// candidate is out of band.
func valuePick(cands []candidate, band float64) (candidate, int) {
	best := candidate{}
	bestIdx := -1

	for i, c := range cands {
		if c.price > band {
			continue
		}

		switch {
		case bestIdx < 0:
			best, bestIdx = c, i
		case c.quality > best.quality:
			best, bestIdx = c, i
		case c.quality == best.quality && c.price < best.price:
			best, bestIdx = c, i
		}
	}

	return best, bestIdx
}

// classifyPool pairs every candidate with its outcome under the band: the
// band winner is selected, the rest in band are in-band (lower quality, or
// a tie lost to a cheaper model), and anything priced above the band is
// out-of-band.
func classifyPool(cands []candidate, band float64, winnerIdx int) []PoolOutcome {
	outcomes := make([]PoolOutcome, len(cands))

	for i, c := range cands {
		switch {
		case i == winnerIdx:
			outcomes[i] = PoolSelected
		case c.price > band:
			outcomes[i] = PoolOutOfBand
		default:
			outcomes[i] = PoolInBand
		}
	}

	return outcomes
}

// bestValue returns the id of the best-value candidate, the rule
// valuePick applies.
func bestValue(cands []candidate, headroom float64, maxCapability bool) string {
	best, _ := valuePick(cands, priceBand(cands, headroom, maxCapability))

	return best.id
}

// candidates returns the models passing every filter for the given input.
// Quality is the normalized prior for the role; a model with no prior for the
// role, a prior below the tier bar, no tool support, an exclusion, a blacklist
// entry, or a window that cannot hold the estimate is dropped.
func (r *Registry) candidates(in SelectInput) []candidate {
	pool, _ := r.classify(in)

	return pool
}

// hardFilterReason returns the hard filter that keeps e out of the pool, or
// "" when e passes the hard filters. The hard filters (tools support, run
// exclusions, blacklist, vendor exclusion) apply at every rung; the quality
// and window checks after them are per-rung and stay in classify. Models
// with no resolvable vendor are never vendor-filtered.
func (r *Registry) hardFilterReason(e llm.CatalogEntry, in SelectInput) FilterReason {
	switch {
	case !e.SupportsTools():
		return FilterNotToolsCapable
	case in.Exclude[e.ID]:
		return FilterExcluded
	case r.blacklist[e.ID]:
		return FilterBlacklisted
	case len(in.ExcludeVendors) > 0 && r.vendorOf(e.ID) != "" && in.ExcludeVendors[r.vendorOf(e.ID)]:
		return FilterVendorExcluded
	default:
		return ""
	}
}

// classify splits the catalog for one rung's input into the pool that
// reached the rung (candidates in catalog order) and the reason-aggregated
// summary of everything that did not. The filter reasons are evaluated in a
// fixed order per model and the first hit wins, so a model excluded AND
// blacklisted lands in exactly one bucket.
func (r *Registry) classify(in SelectInput) ([]candidate, []FilteredOutEntry) {
	bar := r.barFor(in.Tier)

	buckets := make(map[FilterReason][]string, 7)

	var cands []candidate

	for _, e := range r.catalog {
		hard := r.hardFilterReason(e, in)
		if hard != "" {
			buckets[hard] = append(buckets[hard], e.ID)

			continue
		}

		quality, ok := r.priors.ForRole(e.ID, in.Role)
		if !ok {
			buckets[FilterNoPrior] = append(buckets[FilterNoPrior], e.ID)

			continue
		}

		if quality < bar {
			buckets[FilterPriorBelowBar] = append(buckets[FilterPriorBelowBar], e.ID)

			continue
		}

		if in.EstTokens > 0 && !r.fitsWindow(e.ID, in.EstTokens) {
			buckets[FilterWindowTooSmall] = append(buckets[FilterWindowTooSmall], e.ID)

			continue
		}

		cands = append(cands, candidate{
			id:      e.ID,
			quality: quality,
			price:   e.PromptPricePerTok + e.CompletionPricePerTok,
		})
	}

	if len(buckets) == 0 {
		return cands, nil
	}

	// Fixed emission order for stable reports, matching the per-model check
	// order above.
	order := []FilterReason{
		FilterNotToolsCapable, FilterExcluded, FilterBlacklisted, FilterVendorExcluded,
		FilterNoPrior, FilterPriorBelowBar, FilterWindowTooSmall,
	}

	filtered := make([]FilteredOutEntry, 0, len(buckets))
	for _, reason := range order {
		if models := buckets[reason]; len(models) > 0 {
			filtered = append(filtered, FilteredOutEntry{Reason: reason, Models: models})
		}
	}

	return cands, filtered
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
// same reason: both are operator intent, which outranks a rule the selector
// derived.
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

// TierReach is one row of the reachability preflight: how many shipped
// candidates clear one tier's bar for one role, and how far short the
// catalog falls when none do.
type TierReach struct {
	Role  Role
	Tier  Tier
	Bar   float64
	Count int
	Best  float64 // highest prior available for Role, any tier
}

// Reachability reports whether the shipped candidate set can answer a
// request at each tier at all - blacklist and tool support applied, no
// run-time exclusions. Count 0 means every request at that tier degrades
// for the whole run regardless of card, which is the one thing card-level
// logs can never reveal. Rows are ordered role, then strictest tier first.
func (r *Registry) Reachability() []TierReach {
	bars := r.bars()

	tiers := slices.Collect(maps.Keys(bars))
	slices.SortFunc(tiers, func(a, b Tier) int {
		if c := cmp.Compare(bars[b], bars[a]); c != 0 {
			return c
		}

		return cmp.Compare(a, b)
	})

	out := make([]TierReach, 0, 2*len(tiers))

	for _, role := range []Role{RoleCoder, RoleReviewer} {
		best := 0.0

		for _, e := range r.catalog {
			if !e.SupportsTools() || r.blacklist[e.ID] {
				continue
			}

			if q, ok := r.priors.ForRole(e.ID, role); ok && q > best {
				best = q
			}
		}

		for _, t := range tiers {
			out = append(out, TierReach{
				Role:  role,
				Tier:  t,
				Bar:   bars[t],
				Count: len(r.candidates(SelectInput{Role: role, Tier: t})),
				Best:  best,
			})
		}
	}

	return out
}

// OrphanFavoriteTiers lists favorite tiers the ladder does not contain.
// CM sends FavoriteRule.Tier as a free string and build.go converts it
// unchecked, so a typo produces a favorite no rung can ever consult. Sorted
// for a stable log line.
func (r *Registry) OrphanFavoriteTiers() []Tier {
	bars := r.bars()

	seen := map[Tier]bool{}

	for key := range r.favorites {
		if _, ok := bars[key.Tier]; !ok {
			seen[key.Tier] = true
		}
	}

	return slices.Sorted(maps.Keys(seen))
}
