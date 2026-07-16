package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// recordSection upserts a "## <heading>" markdown section into the parent card
// body and pushes the updated body to CM, so the parent accumulates the run's
// history (diagnosis, plan, review rounds) for a complete record — mirroring
// CM's workflow-skills, which write these sections onto the card. section
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

// recordReview records one review round on the parent card body, matching
// CM's review-task workflow skill: round 1 uses the bare "## Review Findings"
// heading, later rounds use "## Review Findings (Round N)". Each round is
// preserved (the per-round heading makes the upsert effectively an append),
// while a re-run of the same round on resume replaces rather than duplicates.
// Every round leads with the round's verify result, so a human reading the card
// sees whether the change was verified — never inferring it from silence.
func (o *run) recordReview(ctx context.Context, round int, findings string, approved bool, vres verifyResult) {
	heading := "Review Findings"
	if round > 1 {
		heading = fmt.Sprintf("Review Findings (Round %d)", round)
	}

	verdict := "revise"
	if approved {
		verdict = "approve"
	}

	// The run-level verify status tracks the latest round's gate result, feeding
	// the PR-body and completion-note trailers.
	o.lastVerify = vres

	section := fmt.Sprintf("## %s\n\n%s\n\n%s\n\n### Recommendation\n\n%s",
		heading, verifyRoundLine(vres, o.resolvedVerifyPlan()), strings.TrimSpace(findings), verdict)

	o.recordSection(ctx, heading, section)
}

// verifyRoundLine renders the "**Verify:** ..." status line prepended to each
// recorded review round. It is derived BY CODE from the round's verify result
// and the resolved plan's provenance, so the honesty of the record does not
// depend on any model choosing to mention it.
func verifyRoundLine(vres verifyResult, plan verifyPlan) string {
	switch vres.Status {
	case verifyPassed:
		return fmt.Sprintf("**Verify:** PASSED _(source: %s)_", plan.Source)
	case verifyFailed:
		return fmt.Sprintf("**Verify:** FAILED _(source: %s)_", plan.Source)
	default:
		note := vres.Note
		if note == "" {
			note = "no verify command resolved"
		}

		if plan.Source == verifySourceNone {
			return "**Verify:** SKIPPED — " + note
		}

		return fmt.Sprintf("**Verify:** SKIPPED — %s _(source: %s)_", note, plan.Source)
	}
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

// formatPlannedPlan renders a parsed (not-yet-created) plan for the HITL
// plan-approval gate: numbered subtasks with tier and depends-on-by-index, so
// the human sees the decomposition before any subtask card exists.
func formatPlannedPlan(p plan) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Overall tier: %s\n\n", p.CardTier)

	for i, st := range p.Subtasks {
		deps := "none"

		if len(st.DependsOn) > 0 {
			parts := make([]string, len(st.DependsOn))
			for j, dep := range st.DependsOn {
				parts[j] = fmt.Sprintf("#%d", dep+1)
			}

			deps = strings.Join(parts, ", ")
		}

		fmt.Fprintf(&b, "### %d. %s\n", i+1, st.Title)
		fmt.Fprintf(&b, "_Tier: %s · Depends on: %s_\n", st.Tier, deps)

		if body := strings.TrimSpace(st.Description); body != "" {
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

// extractSection returns the "## <heading>" block in body, from that heading
// line through the line before the next "## " heading (or the end of the body),
// trimmed of trailing newlines. It returns "" when the heading is absent. The
// returned block includes its "## <heading>" line, so it is a complete section
// ready to hand back to upsertSection. Heading matching is exact, mirroring
// upsertSection.
func extractSection(body, heading string) string {
	marker := "## " + heading

	lines := strings.Split(body, "\n")

	start := -1

	for i, l := range lines {
		if strings.TrimSpace(l) == marker {
			start = i

			break
		}
	}

	if start < 0 {
		return ""
	}

	end := len(lines)

	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i

			break
		}
	}

	return strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n")
}
