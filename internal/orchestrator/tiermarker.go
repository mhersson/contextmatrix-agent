package orchestrator

import (
	"fmt"
	"regexp"
	"strings"
)

// The tier marker persists the planner's tier on the subtask card body - CM
// cards are the ONLY persistence a resumed run can read, and list_cards carries
// just id/title/state. An HTML comment renders invisibly in the UI's markdown.
var tierMarkerRe = regexp.MustCompile(`(?m)^\s*<!-- cm:tier=(simple|moderate|complex|critical) -->\s*$`)

// withTierMarker appends the invisible tier marker to body, trimming any
// trailing newlines first so the marker always lands on its own trailing line.
func withTierMarker(body, tier string) string {
	return strings.TrimRight(body, "\n") + fmt.Sprintf("\n\n<!-- cm:tier=%s -->", tier)
}

// parseTierMarker extracts the persisted tier ("" when absent or invalid) and
// returns the body with the marker line removed.
func parseTierMarker(body string) (string, string) {
	m := tierMarkerRe.FindStringSubmatch(body)
	clean := strings.TrimSpace(tierMarkerRe.ReplaceAllString(body, ""))

	if m == nil {
		return "", clean
	}

	return m[1], clean
}

// nextTier returns the tier one step up the escalation ladder: simple ->
// moderate -> complex -> critical. Critical is the ceiling - there is no
// stronger tier to escalate into, so it maps to itself. An absent or
// unparseable marker ("") is treated as the moderate default (parseTierMarker's
// contract) and so escalates to complex.
func nextTier(tier string) string {
	switch tier {
	case "simple":
		return "moderate"
	case "moderate", "":
		return "complex"
	case "complex":
		return "critical"
	default:
		return "critical"
	}
}
