package runkind

import protocol "github.com/mhersson/contextmatrix-protocol"

// limitFindings drops each finding whose title or text is over its limit
// before it keeps the first 50, and truncates evidence. A non-nil input stays
// non-nil.
func limitFindings(fs []protocol.FrictionFinding) []protocol.FrictionFinding {
	if fs == nil {
		return nil
	}

	out := make([]protocol.FrictionFinding, 0, min(len(fs), maxFindings))

	for _, f := range fs {
		if len(out) == maxFindings {
			break
		}

		if len(f.Title) > maxTitleBytes || len(f.Text) > maxTextBytes {
			continue
		}

		f.Evidence = truncate(f.Evidence, maxEvidenceBytes)
		out = append(out, f)
	}

	return out
}

// maskFindings masks every string of fs, kind included: a token the model
// echoed can land in any of them.
func maskFindings(fs []protocol.FrictionFinding, mask func(string) string) []protocol.FrictionFinding {
	if fs == nil {
		return nil
	}

	out := make([]protocol.FrictionFinding, len(fs))
	for i, f := range fs {
		f.Kind = mask(f.Kind)
		f.Title, f.Text, f.Evidence = mask(f.Title), mask(f.Text), mask(f.Evidence)
		out[i] = f
	}

	return out
}
