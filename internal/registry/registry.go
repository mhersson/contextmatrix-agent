// Package registry resolves roles to concrete model specs for the harness. It
// is caller-facing: the FSM-free harness loop never imports it. SelectByComplexity
// is the best-value selector: a normalized external prior carries the tier bar,
// blacklisted slugs are excluded, and eligible favorites win before the
// cost-optimal pick.
package registry

import (
	"cmp"
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

// DefaultTierBars are the normalized-prior thresholds per complexity tier.
func DefaultTierBars() map[Tier]float64 {
	return map[Tier]float64{
		TierSimple:   0.65,
		TierModerate: 0.76,
		TierComplex:  0.82,
		TierCritical: 0.90,
	}
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
// do what you asked", which is the whole of DEFECT 1: the dishonesty was
// never the fallback, it was that the return type had no field in which to
// admit one happened. Pick embeds ModelSpec so callers that only feed the
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

// barFor replaces tierBar (registry.go:443). A tier absent from the ladder
// has bar 0 - today's behaviour for an unrecognised tier. The zero-value
// Registry still works (TestTierBarsIncludeCritical builds one directly).
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
// rung would have made, so escalating can never yield a worse model than
// asking for less - the property the old fall-through broke. Favorites and
// pins are declared exceptions to that invariant (a favorite bypasses the
// price band by design); see the package contract.
//
// Below the lowest configured bar the answer is the operator's capable
// default - the trigger's default_model, the serve default, or the
// compiled-in guard - marked SourceDefault and subject to the SAME hard
// filters as any candidate. That last clause is new and load-bearing:
// today's fall-through ignores Exclude, so a model proven harness-incapable
// this run can be re-handed forever (candidates.go:427 documents the hole).
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
		// landed on. Preserved subtlety: a rung counts as non-dry when a
		// vendor-BLIND favorite is eligible even if the vendor-FILTERED
		// pool is empty - today's ordering (favoriteFor ran before
		// candidates), and changing it would let a soft vendor preference
		// silently override an explicit favorite.
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
// (tier, any role) - that is present among cands. An empty string means no
// eligible favorite.
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

// SelectReviewPanel returns n specs for the review specialists: distinct models
// chosen by repeated SelectByComplexity with a growing Exclude set. Each seat
// softly prefers vendors not yet on the panel: the pick runs vendor-filtered
// when that still leaves a qualifying candidate, vendor-blind otherwise. When
// the pool runs dry, the last pick is reused to fill remaining slots rather
// than escalating price.
func (r *Registry) SelectReviewPanel(in SelectInput, n int) []ModelSpec {
	if n <= 0 {
		return nil
	}

	exclude := map[string]bool{}
	for id := range in.Exclude {
		exclude[id] = true
	}

	usedVendors := map[string]bool{}
	for v := range in.ExcludeVendors {
		usedVendors[v] = true // e.g. a Best-of-N pin's vendor
	}

	panel := make([]ModelSpec, 0, n)

	var last ModelSpec

	for len(panel) < n {
		next := in
		next.Exclude = exclude
		next.ExcludeVendors = nil // the dry probe and the fallback are vendor-blind

		// Probe the candidate pool directly: an empty pool means no distinct
		// model remains, so reuse the last real pick rather than escalating to
		// the (pricier) capable default. The probe duplicates the filter work
		// SelectByComplexity does internally - accepted for clarity at catalog
		// sizes.
		if len(r.candidates(next)) == 0 {
			if len(panel) == 0 {
				// Dry from the start: every slot is the capable default, so the
				// panel is always n non-empty specs.
				last = r.SelectByComplexity(next).ModelSpec
			}

			panel = append(panel, last)

			continue
		}

		// Soft vendor preference: restrict to unseated vendors only when that
		// still leaves a qualifying candidate. The price band re-anchors on the
		// filtered subset, so a diverse seat may cost more than the
		// vendor-blind pick would have - accepted, diversity is the point.
		if len(usedVendors) > 0 {
			filtered := next
			filtered.ExcludeVendors = usedVendors

			if len(r.candidates(filtered)) > 0 {
				next = filtered
			}
		}

		spec := r.SelectByComplexity(next).ModelSpec
		panel = append(panel, spec)
		last = spec
		exclude[spec.Model] = true

		if v := r.vendorOf(spec.Model); v != "" {
			usedVendors[v] = true
		}
	}

	return panel
}

// SelectDiscussionPanel returns n distinct models for mob session discussion seats.
// It is the review-panel diversity walk by construction - distinct-first with
// wrap-around when the pool runs dry - honoring the caller's exclusions
// (review discussions exclude the models that coded the card). It exists as a
// named seam so discussion selection can diverge from review selection
// without touching call sites.
func (r *Registry) SelectDiscussionPanel(in SelectInput, n int) []ModelSpec {
	return r.SelectReviewPanel(in, n)
}

// SelectCandidateModels picks n coder models for a Best-of-N fan-out. pin, if
// non-empty, occupies slot 1 (excluded from the auto picks); the remaining
// slots are distinct-first with wrap-around when the pool is smaller than n
// (SelectReviewPanel semantics) - model scarcity never shrinks n.
func (r *Registry) SelectCandidateModels(in SelectInput, n int, pin string) []ModelSpec {
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

	out := make([]ModelSpec, 0, n)
	out = append(out, ModelSpec{Model: pin, ContextWindow: r.ContextWindow(pin)})

	return append(out, r.SelectReviewPanel(next, n-1)...)
}

// specFor builds a ModelSpec for id, filling the context window from the catalog.
func (r *Registry) specFor(id string) ModelSpec {
	spec := ModelSpec{Model: id}
	if e, ok := r.catalog.Find(id); ok {
		spec.ContextWindow = e.ContextLength
	}

	return spec
}
