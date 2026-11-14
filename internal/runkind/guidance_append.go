package runkind

import (
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
)

// withGuidance appends section's lines to file after one blank line. Existing
// text is kept as is, except that the file's trailing newlines become that
// blank line. Lines that would end past the grounding cap go after the file's
// first "# " line instead, or at the top.
func withGuidance(file, section string) string {
	lines := strings.TrimRight(section, "\n") + "\n"
	body := strings.TrimRight(file, "\n")

	if body == "" {
		return lines
	}

	if joined := body + "\n\n" + lines; len(joined) <= orchestrator.GroundingByteCap {
		return joined
	}

	return placeNearTop(file, lines)
}

// placeNearTop puts lines after the first "# " line, or at the top.
func placeNearTop(file, lines string) string {
	off := 0
	for line := range strings.SplitAfterSeq(file, "\n") {
		off += len(line)
		if strings.HasPrefix(line, "# ") {
			return file[:off] + "\n" + lines + "\n" + file[off:]
		}
	}

	return lines + "\n" + file
}
