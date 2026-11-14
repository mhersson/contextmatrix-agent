package runkind

import (
	"errors"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
)

// GuidanceMarkerPrefix starts every guidance block marker. A guidance section
// may not contain it, so a section can never forge or close a block.
const GuidanceMarkerPrefix = "<!-- sentinel:"

// guidanceFile picks the root instruction file in grounding's order.
func guidanceFile(hasAgents, hasClaude bool) string {
	if !hasAgents && hasClaude {
		return "CLAUDE.md"
	}

	return "AGENTS.md"
}

func blockMarkers(project string) (start, end string) {
	return GuidanceMarkerPrefix + project + ":start -->", GuidanceMarkerPrefix + project + ":end -->"
}

var errMalformedBlock = errors.New("guidance block markers are malformed")

// withGuidance appends section to project's block in file, or adds the block.
// Existing text is kept as is, except that a block-less file's trailing
// newlines become one blank line before the new block. A block that would end
// past the grounding cap moves, whole, to after the title.
func withGuidance(file, project, section string) (string, error) {
	start, end := blockMarkers(project)
	section = strings.TrimRight(section, "\n")

	var starts, ends []int

	endLen := 0

	off := 0

	for line := range strings.SplitAfterSeq(file, "\n") {
		switch strings.TrimSpace(line) {
		case start:
			starts = append(starts, off)
		case end:
			ends = append(ends, off)
			endLen = len(line)
		}

		off += len(line)
	}

	if len(starts) > 1 || len(ends) > 1 || len(starts) != len(ends) ||
		(len(ends) == 1 && ends[0] < starts[0]) {
		return "", errMalformedBlock
	}

	if len(ends) == 1 {
		appended := file[:ends[0]] + "\n" + section + "\n" + file[ends[0]:]

		blockEnd := ends[0] + len(section) + 2 + endLen
		if blockEnd <= orchestrator.GroundingByteCap {
			return appended, nil
		}

		before := appended[:starts[0]]
		if before == "\n" || strings.HasSuffix(before, "\n\n") {
			before = before[:len(before)-1]
		}

		block := appended[starts[0]:blockEnd]
		if !strings.HasSuffix(block, "\n") {
			block += "\n"
		}

		return placeBlockNearTop(before+appended[blockEnd:], block), nil
	}

	block := start + "\n## Sentinel guidance\n\n" + section + "\n" + end + "\n"
	body := strings.TrimRight(file, "\n")

	if body == "" {
		return block, nil
	}

	if joined := body + "\n\n" + block; len(joined) <= orchestrator.GroundingByteCap {
		return joined, nil
	}

	return placeBlockNearTop(file, block), nil
}

// placeBlockNearTop puts block after the first "# " line, or at the top.
func placeBlockNearTop(file, block string) string {
	off := 0
	for line := range strings.SplitAfterSeq(file, "\n") {
		off += len(line)
		if strings.HasPrefix(line, "# ") {
			return file[:off] + "\n" + block + "\n" + file[off:]
		}
	}

	return block + "\n" + file
}
