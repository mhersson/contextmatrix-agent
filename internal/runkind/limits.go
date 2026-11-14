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

	if r.Analyse != nil {
		a := limitAnalyse(*r.Analyse)
		r.Analyse = &a
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

// limitInvestigate truncates the diagnosis, drops a fix whose section or
// retired IDs are over their limits, drops forward_to when over its limit and
// bounds the findings.
func limitInvestigate(r protocol.InvestigateResult) protocol.InvestigateResult {
	r.Diagnosis = truncate(r.Diagnosis, maxDiagnosisBytes)

	if r.Fix != nil && (len(r.Fix.Section) > maxSectionBytes || len(r.Fix.RetireSubtasks) > maxIDs) {
		r.Fix = nil
	}

	if len(r.ForwardTo) > maxIDs {
		r.ForwardTo = nil
	}

	r.Findings = limitFindings(r.Findings)

	return r
}

// limitAnalyse bounds the findings; nil becomes empty.
func limitAnalyse(r protocol.AnalyseResult) protocol.AnalyseResult {
	r.Findings = limitFindings(r.Findings)
	if r.Findings == nil {
		r.Findings = []protocol.FrictionFinding{}
	}

	return r
}

// fitOutcome drops the last finding of an investigation or an analysis until d
// encodes to at most maxBytes, so the rest of the result always survives.
func fitOutcome(d runoutcome.Data, maxBytes int) runoutcome.Data {
	if d.Result == nil {
		return d
	}

	res := *d.Result
	d.Result = &res

	var findings *[]protocol.FrictionFinding

	switch {
	case res.Investigate != nil:
		inv := *res.Investigate
		res.Investigate = &inv
		findings = &inv.Findings
	case res.Analyse != nil:
		an := *res.Analyse
		res.Analyse = &an
		findings = &an.Findings
	default:
		return d
	}

	for len(*findings) > 0 {
		enc, err := json.Marshal(d)
		if err != nil || len(enc) <= maxBytes {
			break
		}

		*findings = (*findings)[:len(*findings)-1]
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
