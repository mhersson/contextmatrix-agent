package mob

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// renderEntry formats one transcript line per the wire convention:
// "[round N] author (lens): text"; lens is omitted when "".
func renderEntry(e Entry) string {
	if e.Lens == "" {
		return fmt.Sprintf("[round %d] %s: %s", e.Round, e.Author, e.Content)
	}

	return fmt.Sprintf("[round %d] %s (%s): %s", e.Round, e.Author, e.Lens, e.Content)
}

// renderDelta renders entries[from:] excluding forAuthor's own entries (a
// seat holds its own prior utterances in its task history), one blank line
// between entries, followed by a blank line and the instruction line.
// Returns "" when there is nothing to render and no instruction.
func renderDelta(entries []Entry, from int, forAuthor, instruction string) string {
	if from < 0 {
		from = 0
	}

	if from > len(entries) {
		from = len(entries)
	}

	lines := make([]string, 0, len(entries)-from)

	for _, e := range entries[from:] {
		if e.Author == forAuthor {
			continue
		}

		lines = append(lines, renderEntry(e))
	}

	body := strings.Join(lines, "\n\n")

	switch {
	case body == "" && instruction == "":
		return ""
	case body == "":
		return instruction
	case instruction == "":
		return body
	default:
		return body + "\n\n" + instruction
	}
}

// truncateUtterance enforces utteranceCap bytes with a rune-safe cut and the
// truncation marker. The seat is not penalized - the moderator just bounds
// what enters the transcript.
func truncateUtterance(s string) string {
	if len(s) <= utteranceCap {
		return s
	}

	cut := utteranceCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + truncationMarker
}
