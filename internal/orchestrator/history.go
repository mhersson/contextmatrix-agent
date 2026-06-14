package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// recordSection upserts a "## <heading>" markdown section into the parent card
// body and pushes the updated body to CM, so the parent accumulates the run's
// history (diagnosis, plan, review rounds) for a complete record — mirroring the
// runner's workflow-skills, which write these sections onto the card. section
// must be the COMPLETE block, starting with its "## <heading>" line.
//
// Best-effort: a failure is logged, not fatal — the body is a human-facing
// record, never control state, and must not fail a phase.
func (o *run) recordSection(ctx context.Context, heading, section string) {
	o.body = upsertSection(o.body, heading, section)

	if err := o.d.Ops.UpdateCardBody(ctx, o.d.Cfg.CardID, o.body); err != nil {
		slog.Warn("record parent card section failed",
			"card_id", o.d.Cfg.CardID, "heading", heading, "error", err)
	}
}

// recordReview records one review round on the parent card body, matching the
// runner's review-task convention: round 1 uses the bare "## Review Findings"
// heading, later rounds use "## Review Findings (Round N)". Each round is
// preserved (the per-round heading makes the upsert effectively an append),
// while a re-run of the same round on resume replaces rather than duplicates.
func (o *run) recordReview(ctx context.Context, round int, findings string, approved bool) {
	heading := "Review Findings"
	if round > 1 {
		heading = fmt.Sprintf("Review Findings (Round %d)", round)
	}

	verdict := "revise"
	if approved {
		verdict = "approve"
	}

	section := fmt.Sprintf("## %s\n\n%s\n\n### Recommendation\n\n%s",
		heading, strings.TrimSpace(findings), verdict)

	o.recordSection(ctx, heading, section)
}

// sectionFrom normalizes content into a "## <heading>" section: if content
// already begins with that heading (the planner / diagnose models emit it), it
// is returned as-is; otherwise the heading is prepended. This keeps
// upsertSection's replace-detection working whether or not the model included
// the heading.
func sectionFrom(heading, content string) string {
	c := strings.TrimSpace(content)
	if strings.HasPrefix(c, "## "+heading) {
		return c
	}

	return "## " + heading + "\n\n" + c
}

// formatPlan renders the subtask decomposition as the "## Plan" body, so the
// parent card shows the whole plan, not just scattered subtask cards. Each
// subtask is a numbered heading plus an italic tier/deps line and its own body
// block, keeping multi-line bodies readable rather than crammed onto one line.
func formatPlan(subs []subtaskRef) string {
	var b strings.Builder

	for i, s := range subs {
		deps := "none"
		if len(s.DependsOnIDs) > 0 {
			deps = strings.Join(s.DependsOnIDs, ", ")
		}

		fmt.Fprintf(&b, "### %d. %s — %s\n", i+1, s.ID, s.Title)
		fmt.Fprintf(&b, "_Tier: %s · Depends on: %s_\n", s.Tier, deps)

		if body := strings.TrimSpace(s.Body); body != "" {
			b.WriteString("\n")
			b.WriteString(body)
			b.WriteString("\n")
		}

		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// upsertSection replaces the "## <heading>" block in body (from that heading to
// the next "## " heading, or the end of the body) with section, or appends
// section when the heading is absent. section must be the complete block,
// starting with its "## <heading>" line. Heading matching is exact, so
// "## Review Findings" does not match "## Review Findings (Round 2)".
func upsertSection(body, heading, section string) string {
	marker := "## " + heading
	section = strings.TrimRight(section, "\n")

	lines := strings.Split(body, "\n")

	start := -1

	for i, l := range lines {
		if strings.TrimSpace(l) == marker {
			start = i

			break
		}
	}

	if start < 0 {
		trimmed := strings.TrimRight(body, "\n")
		if trimmed == "" {
			return section + "\n"
		}

		return trimmed + "\n\n" + section + "\n"
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
		b.WriteString("\n\n")
	}

	b.WriteString(section)

	if after != "" {
		b.WriteString("\n\n")
		b.WriteString(after)
	}

	b.WriteString("\n")

	return b.String()
}
