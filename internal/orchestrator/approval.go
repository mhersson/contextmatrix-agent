package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// approvalHeading is the card-body heading for the durable approval record,
// chosen to be unique - NOT a prefix match for "Review Findings", so
// reviewFindingsHistory (which collects "Review Findings" sections) and
// stripAgentSections (which tracks agentRoundSectionHeadings) both treat it
// independently.
const approvalHeading = "Review Approval"

// approval is the persisted verdict record, bound to the commit it approved
// so a resume can verify nothing changed since the approval was made.
type approval struct {
	HeadSHA string `json:"head_sha"`
	// Summary is the human-readable description of what was approved - the
	// review summary text (the rendered findings from the verdict).
	Summary string `json:"summary"`
	// Fixes are the surviving fixes from the approving verdict, whether
	// actionable (the cleanup pass will address them) or nit-only.
	Fixes []fix `json:"fixes"`
}

// recordApproval writes the approval section onto the parent card body,
// capturing the branch HEAD SHA at the time of approval so a later resume
// can verify nothing changed before adopting. The section is a markdown
// block with the SHA in the body followed by a fenced-JSON payload for the
// summary and fixes. A failed write degrades to the pre-change re-review
// behavior - the approval is not lost because the body is the adoption gate's
// source of truth and a missing section is treated as "no record".
//
// Best-effort: a failure is logged, not fatal. When HEAD cannot be read the
// record is skipped (nothing to bind to) and the caller proceeds normally.
func (o *run) recordApproval(ctx context.Context, summary string, fixes []fix) {
	d := o.d

	sha, err := d.Git.Head(ctx)
	if err != nil {
		slog.Warn("review: could not record approval - HEAD unreadable",
			"card_id", d.Cfg.CardID, "error", err)

		return
	}

	if sha == "" {
		slog.Warn("review: could not record approval - HEAD is empty",
			"card_id", d.Cfg.CardID)

		return
	}

	body := formatApproval(approval{HeadSHA: sha, Summary: summary, Fixes: fixes})

	o.recordSection(ctx, approvalHeading, body)
}

// formatApproval renders the approval record as a "## Review Approval" section
// whose body carries the HEAD SHA as a visible line plus a fenced JSON block
// for machine extraction.
func formatApproval(a approval) string {
	payload, err := json.Marshal(a)
	if err != nil {
		// A structurally valid approval struct should never fail marshalling.
		// This is defensive: fall back to a SHA-only record.
		slog.Warn("approval: failed to marshal JSON payload", "error", err)

		payload = []byte(`{"error":"marshal failed"}`)
	}

	return fmt.Sprintf("## %s\n\nCommit: %s\n\n```json\n%s\n```",
		approvalHeading, a.HeadSHA, string(payload))
}

// extractApproval reads the "## Review Approval" section from body and
// parses its JSON payload back into an approval struct. It returns a zero
// value and false when the section is absent or unparseable.
func extractApproval(body string) (approval, bool) {
	section := extractSection(body, approvalHeading)
	if section == "" {
		return approval{}, false
	}

	// Find the fenced JSON block within the section.
	start := strings.Index(section, "```json\n")
	if start < 0 {
		return approval{}, false
	}

	start += len("```json\n")

	end := strings.Index(section[start:], "\n```")
	if end < 0 {
		return approval{}, false
	}

	raw := section[start : start+end]

	var a approval
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return approval{}, false
	}

	if a.HeadSHA == "" {
		return approval{}, false
	}

	return a, true
}

// clearApproval removes the "## Review Approval" section from the parent card
// body and pushes the updated body to CM, so a resumed run that adopted an
// approval clears it before the FSM proceeds to integrate. Best-effort: a
// failure is logged, not fatal.
func (o *run) clearApproval(ctx context.Context) {
	o.body = removeApprovalSection(o.body)

	if err := o.d.Ops.UpdateCardBody(ctx, o.d.Cfg.CardID, o.body); err != nil {
		slog.Warn("review: failed to clear approval record",
			"card_id", o.d.Cfg.CardID, "error", err)
	}
}

// removeApprovalSection removes the "## Review Approval" block from body and
// returns the result. Heading matching is exact, matching
// upsertSection/extractSection. Returns the input byte-identical when the
// heading is absent.
func removeApprovalSection(body string) string {
	marker := "## " + approvalHeading

	lines := strings.Split(body, "\n")

	start := -1

	for i, l := range lines {
		if strings.TrimSpace(l) == marker {
			start = i

			break
		}
	}

	if start < 0 {
		return body
	}

	// Find the end of the existing block: the next "## " heading after start.
	end := len(lines)

	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i

			break
		}
	}

	before := strings.TrimRight(strings.Join(lines[:start], "\n"), "\n")
	after := strings.TrimLeft(strings.Join(lines[end:], "\n"), "\n")

	var b strings.Builder

	if before != "" {
		b.WriteString(before)
	}

	if after != "" {
		if before != "" {
			b.WriteString("\n\n")
		}

		b.WriteString(after)
	}

	b.WriteString("\n")

	return b.String()
}
