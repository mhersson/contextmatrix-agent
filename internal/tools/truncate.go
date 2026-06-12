package tools

import "fmt"

// HeadTail bounds s to roughly limit bytes by keeping the first two thirds and
// the last third with an explicit marker in between. limit <= 0 disables the
// cap. The cut points are byte offsets — fine for tool output destined for a
// model prompt.
func HeadTail(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}

	head := limit * 2 / 3
	tail := limit - head

	return s[:head] + fmt.Sprintf("\n[... %d bytes truncated ...]\n", len(s)-head-tail) + s[len(s)-tail:]
}
