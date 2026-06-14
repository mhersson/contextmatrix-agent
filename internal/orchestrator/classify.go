package orchestrator

import (
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
)

// maintenanceLabels mark mechanical work that skips the debug gate (create-plan
// Branch A). A maintenance label outranks a bug type/label.
var maintenanceLabels = map[string]bool{
	"simple": true, "chore": true, "dependencies": true, "infra": true,
}

// bugLabels mark a card as bug-like (create-plan Branch B).
var bugLabels = map[string]bool{"bug": true, "bugfix": true}

// maintenanceTitleVerbs are first-word mechanical-action verbs (create-plan
// Branch A title test).
var maintenanceTitleVerbs = map[string]bool{
	"bump": true, "update": true, "rename": true, "move": true,
	"pin": true, "upgrade": true, "downgrade": true,
}

// bugTitleVerbs are first-word investigation/repair verbs (create-plan Branch B).
var bugTitleVerbs = map[string]bool{
	"fix": true, "bugfix": true, "repair": true, "resolve": true,
	"investigate": true, "debug": true,
}

// bugBodyMarkers are defect-report phrases (create-plan Branch B body test).
var bugBodyMarkers = []string{
	"doesn't work", "does not work", "is broken", "throws", "crashes",
	"fails when", "unexpected behavior", "regression", "stack trace",
	"panic", "error:", "should", // "should X but Y" — paired with a bug verb/label upstream
}

// firstWord returns the lowercased first whitespace-delimited word of s,
// stripped of trailing punctuation.
func firstWord(s string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	if len(fields) == 0 {
		return ""
	}

	return strings.TrimRight(fields[0], ".:,!?")
}

func hasAnyLabel(labels []string, set map[string]bool) bool {
	for _, l := range labels {
		if set[strings.ToLower(strings.TrimSpace(l))] {
			return true
		}
	}

	return false
}

func hasMarker(body string, markers []string) bool {
	lower := strings.ToLower(body)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}

	return false
}

// isPureMaintenance reports whether the card is mechanical work that skips the
// debug gate (create-plan Phase 0 Branch A): a maintenance label AND a
// mechanical-action title verb. Maintenance outranks a bug type/label.
func isPureMaintenance(tc cmclient.TaskContext) bool {
	return hasAnyLabel(tc.Labels, maintenanceLabels) && maintenanceTitleVerbs[firstWord(tc.Title)]
}

// isBugLike reports whether the card warrants a read-only debug investigation
// before planning (create-plan Phase 0 Branch B): type==bug, a bug label, a
// bug-verb title, or defect language in the body. Pure-maintenance cards are
// excluded (Branch A wins).
func isBugLike(tc cmclient.TaskContext) bool {
	if isPureMaintenance(tc) {
		return false
	}

	if strings.EqualFold(tc.Type, "bug") {
		return true
	}

	if hasAnyLabel(tc.Labels, bugLabels) {
		return true
	}

	if bugTitleVerbs[firstWord(tc.Title)] {
		return true
	}

	return hasMarker(tc.Description, bugBodyMarkers)
}
