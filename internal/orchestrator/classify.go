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
// Each must be specific enough to stand alone - vague words like "should"
// are excluded, as they false-positive on ordinary feature acceptance criteria
// and would trigger a needless paid diagnose pass.
var bugBodyMarkers = []string{
	"doesn't work", "does not work", "is broken", "throws", "crashes",
	"fails when", "unexpected behavior", "regression", "stack trace",
	"panic", "error:",
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

// isCreative reports whether the card warrants a brainstorming dialogue before
// planning (create-plan Phase 0 Branch C): work that is neither pure maintenance
// (Branch A) nor bug-like (Branch B). Branch C runs in HITL mode only; the
// orchestrator gates the brainstorm call on Cfg.Interactive.
func isCreative(tc cmclient.TaskContext) bool {
	return !isPureMaintenance(tc) && !isBugLike(tc)
}

// isBugLike reports whether the card warrants a read-only debug investigation
// before planning (create-plan Phase 0 Branch B): type==bug, a bug label, a
// bug-verb title, or defect language in the body outside mockup blocks.
// Pure-maintenance cards are excluded (Branch A wins).
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

	return hasMarker(withoutMockups(tc.Description), bugBodyMarkers)
}

// withoutMockups drops fenced mockup blocks: their HTML source holds UI copy such
// as "error:", not a defect report. The grammar is shared with CM's card lint and
// the architect's contract check: an opening line of 3+ backticks then "mockup" at
// column 0, closed by a line of at least as many backticks, else running to the
// end; tilde and indented fences are not mockup blocks.
func withoutMockups(body string) string {
	var b strings.Builder

	fence := 0

	for _, line := range strings.SplitAfter(body, "\n") {
		text := strings.TrimRight(strings.TrimSuffix(line, "\n"), " \t")

		if fence > 0 {
			if len(text) >= fence && strings.Trim(text, "`") == "" {
				fence = 0
			}

			continue
		}

		if rest := strings.TrimLeft(text, "`"); rest == "mockup" && len(text)-len(rest) >= 3 {
			fence = len(text) - len(rest)

			continue
		}

		b.WriteString(line)
	}

	return b.String()
}
