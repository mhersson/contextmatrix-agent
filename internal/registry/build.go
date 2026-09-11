package registry

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-protocol/selection"
)

// LadderFault is one role's payload ladder that did not validate. The
// registry has already fallen back to the built-in bars for that role; the
// worker turns each fault into a card log line so the operator learns that
// the ladder they saved is not the one running. Role is the wire name, so
// an unknown role is reported by the name CM sent.
type LadderFault struct {
	Role   string
	Reason error
}

func (f LadderFault) Error() string {
	return fmt.Sprintf("%s tier ladder from the payload did not validate (%v)", f.Role, f.Reason)
}

// FromSelection builds the registry CM's SelectionContext describes: the
// candidates, favorites, blacklist, per-role tier ladders and price headroom
// on the wire, plus the run's max-capability flag. It always returns a
// usable registry. A role whose ladder does not validate falls back to the
// built-in bars for that role alone and is reported as a LadderFault; the
// other role's ladder still applies. A headroom below 1 on the wire reads as
// the built-in one inside the selection package; CM refuses to store one,
// so there is nothing to report. A nil sc is an empty selection: every pick
// is the capable default.
func FromSelection(sc *protocol.SelectionContext, capable string, maxCapability bool) (*Registry, []LadderFault) {
	in := selection.Input{Capable: capable, MaxCapability: maxCapability}

	var faults []LadderFault

	if sc != nil {
		in.Candidates = sc.Candidates
		in.Favorites = sc.Favorites
		in.Blacklist = sc.Blacklist
		in.PriceHeadroom = sc.PriceHeadroom
		in.Ladders, faults = laddersFromPayload(sc.TierBars)
	}

	return &Registry{Selector: selection.New(in)}, faults
}

// wireRoles is the closed set of roles a payload ladder may name.
var wireRoles = map[string]Role{string(RoleCoder): RoleCoder, string(RoleReviewer): RoleReviewer}

// laddersFromPayload validates SelectionContext.TierBars one role at a time.
// selection.LaddersFromWire rejects the whole map on the first bad role; the
// agent must not, because one mistyped rung on the reviewer ladder must not
// silently reset the coder ladder too. Each role goes through the shared
// rule (merge over the built-in bars, monotone, every bar in [0,1]); a role
// that fails is left out of the result, which the selector reads as the
// built-in bars, and named in a fault. An unknown role is a fault as well: a
// typo that quietly left a role on the defaults is the failure the operator
// cannot see. Faults are ordered by role name so the card lines are stable.
// Empty input is nil, nil.
func laddersFromPayload(in map[string]map[string]float64) (selection.Ladders, []LadderFault) {
	if len(in) == 0 {
		return nil, nil
	}

	ladders := selection.Ladders{}

	var faults []LadderFault

	for _, name := range slices.Sorted(maps.Keys(in)) {
		role, ok := wireRoles[name]
		if !ok {
			faults = append(faults, LadderFault{
				Role:   name,
				Reason: fmt.Errorf("unknown role (known: %s)", strings.Join(slices.Sorted(maps.Keys(wireRoles)), ", ")),
			})

			continue
		}

		bars, err := selection.TierBarsFromStrings(in[name])
		if err != nil {
			faults = append(faults, LadderFault{Role: name, Reason: err})

			continue
		}

		if bars != nil {
			ladders[role] = bars
		}
	}

	if len(ladders) == 0 {
		ladders = nil
	}

	return ladders, faults
}
