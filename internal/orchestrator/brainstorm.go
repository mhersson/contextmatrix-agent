package orchestrator

import "strings"

// designMarker is the sentinel the brainstorming model appends once the human has
// confirmed the design. Same single-line-handoff convention as commitMarker.
const designMarker = "DESIGN_COMPLETE"

// extractDesign reports whether the brainstorming model signalled completion (a
// DESIGN_COMPLETE marker) and returns the design text to record: the "## Design"
// section when present, else the whole message with the marker line removed.
func extractDesign(output string) (string, bool) {
	if !strings.Contains(output, designMarker) {
		return "", false
	}

	var kept []string

	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == designMarker {
			continue
		}

		kept = append(kept, line)
	}

	text := strings.TrimSpace(strings.Join(kept, "\n"))

	if idx := strings.Index(text, "## Design"); idx >= 0 {
		return strings.TrimSpace(text[idx:]), true
	}

	return text, true
}

// hasDesignSection reports whether body already carries a "## Design" heading, so
// a card that arrives with a design (a prior brainstorm pass, or a
// thoroughly-written description) skips the dialogue.
func hasDesignSection(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "## Design" {
			return true
		}
	}

	return false
}
