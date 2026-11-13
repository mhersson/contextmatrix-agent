package runkind

import (
	"unicode/utf8"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// The result size limits ContextMatrix validates. Anything CM applies is
// dropped when over its limit, never truncated; display text is truncated.
const (
	maxCandidates     = 32
	maxCandidateBytes = 512
	maxAbsent         = 64
	maxAbsentBytes    = 512
)

// limitResult bounds the result of whichever kind ran.
func limitResult(r protocol.RunKindResult) protocol.RunKindResult {
	if r.Check != nil {
		c := limitCheck(*r.Check)
		r.Check = &c
	}

	return r
}

// limitCheck drops candidates over 512 B, then keeps the first 32. Absent
// entries are cut at 64 and each truncated, never dropped: a dropped entry
// could let the text check pass.
func limitCheck(r protocol.CheckResult) protocol.CheckResult {
	cands := make([]string, 0, min(len(r.Candidates), maxCandidates))

	for _, c := range r.Candidates {
		if len(cands) == maxCandidates {
			break
		}

		if len(c) <= maxCandidateBytes {
			cands = append(cands, c)
		}
	}

	r.Candidates = cands

	if len(r.AbsentFromRepo) > maxAbsent {
		r.AbsentFromRepo = r.AbsentFromRepo[:maxAbsent]
	}

	r.AbsentFromRepo = mapStrings(r.AbsentFromRepo, func(s string) string { return truncate(s, maxAbsentBytes) })

	return r
}

// truncate cuts s to at most n bytes at a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
