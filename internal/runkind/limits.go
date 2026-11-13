package runkind

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-agent/internal/runoutcome"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// The result size limits ContextMatrix validates. Anything CM applies is
// dropped when over its limit, never truncated; display text is truncated.
const (
	maxCandidates     = 32
	maxCandidateBytes = 512
	maxAbsent         = 64
	maxAbsentBytes    = 512

	maxDiagnosisBytes = 32 << 10
	maxSectionBytes   = 16 << 10
	maxIDs            = 50
	maxFindings       = 50
	maxTitleBytes     = 200
	maxTextBytes      = 16 << 10
	maxEvidenceBytes  = 4 << 10
)

// maxOutcomeBytes bounds the encoded outcome data, leaving room for the event
// envelope under serve's 1 MiB status limit.
const maxOutcomeBytes = 1000 << 10

// limitResult bounds the result of whichever kind ran.
func limitResult(r protocol.RunKindResult) protocol.RunKindResult {
	if r.Check != nil {
		c := limitCheck(*r.Check)
		r.Check = &c
	}

	if r.Investigate != nil {
		i := limitInvestigate(*r.Investigate)
		r.Investigate = &i
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

// limitInvestigate truncates the diagnosis and each finding's evidence, drops a
// fix whose section or retired IDs are over their limits, drops forward_to
// when over its limit, and drops each finding whose title or text is over its
// limit before it keeps the first 50.
func limitInvestigate(r protocol.InvestigateResult) protocol.InvestigateResult {
	r.Diagnosis = truncate(r.Diagnosis, maxDiagnosisBytes)

	if r.Fix != nil && (len(r.Fix.Section) > maxSectionBytes || len(r.Fix.RetireSubtasks) > maxIDs) {
		r.Fix = nil
	}

	if len(r.ForwardTo) > maxIDs {
		r.ForwardTo = nil
	}

	if r.Findings == nil {
		return r
	}

	findings := make([]protocol.FrictionFinding, 0, min(len(r.Findings), maxFindings))

	for _, f := range r.Findings {
		if len(findings) == maxFindings {
			break
		}

		if len(f.Title) > maxTitleBytes || len(f.Text) > maxTextBytes {
			continue
		}

		f.Evidence = truncate(f.Evidence, maxEvidenceBytes)
		findings = append(findings, f)
	}

	r.Findings = findings

	return r
}

// fitOutcome drops an investigation's last finding until d encodes to at most
// maxBytes, so the diagnosis and the fix always survive. Nothing else changes.
func fitOutcome(d runoutcome.Data, maxBytes int) runoutcome.Data {
	if d.Result == nil || d.Result.Investigate == nil {
		return d
	}

	res := *d.Result
	inv := *res.Investigate
	res.Investigate = &inv
	d.Result = &res

	for len(inv.Findings) > 0 {
		enc, err := json.Marshal(d)
		if err != nil || len(enc) <= maxBytes {
			break
		}

		inv.Findings = inv.Findings[:len(inv.Findings)-1]
	}

	return d
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
