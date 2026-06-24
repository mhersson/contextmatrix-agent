package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
)

// designMarker is the sentinel the brainstorming model appends once the human has
// confirmed the design. Same single-line-handoff convention as commitMarker.
const designMarker = "DESIGN_COMPLETE"

// extractDesign reports whether the brainstorming model signalled completion (a
// DESIGN_COMPLETE marker) and returns the design text to record: the "## Design"
// section when present, else the whole message with the marker line removed.
func extractDesign(output string) (string, bool) {
	if !strings.Contains(output, designMarker) {
		return "", false
	}

	var kept []string

	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == designMarker {
			continue
		}

		kept = append(kept, line)
	}

	text := strings.TrimSpace(strings.Join(kept, "\n"))

	if idx := strings.Index(text, "## Design"); idx >= 0 {
		return strings.TrimSpace(text[idx:]), true
	}

	return text, true
}

// hasDesignSection reports whether body already carries a "## Design" heading, so
// a card that arrives with a design (a prior brainstorm pass, or a
// thoroughly-written description) skips the dialogue.
func hasDesignSection(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "## Design" {
			return true
		}
	}

	return false
}

// maxBrainstormTurns bounds a dialogue that never converges on DESIGN_COMPLETE,
// so a stuck conversation proceeds to planning instead of looping forever. The
// budget ledger is the other bound.
const maxBrainstormTurns = 30

// runBrainstorm drives the creative-card design dialogue (create-plan Phase 0
// Branch C), folded into the plan phase for HITL creative cards. Each turn is a
// read-only, inbox-free harness run so the model can explore the repo and then
// stop on a no-tool-call turn; the orchestrator owns the human turns via the
// inbox and threads the dialogue as a text block (keeping internal/harness
// generic). The agreed design is captured from the model's marked output and
// recorded as a ## Design section; the model never writes the card.
//
// Termination: a DESIGN_COMPLETE marker records ## Design and returns nil; a
// promote (ErrInboxClosed) returns nil so planning proceeds autonomously; an
// end_session (ctx error) returns the error so the worker parks; a budget breach
// returns the *BudgetExceededError so execute() parks; the turn cap logs and
// returns nil so planning grounds on the card alone.
func (o *run) runBrainstorm(ctx context.Context, model string) error {
	d := o.d
	cfg := d.Cfg

	convo := ""

	for turn := 0; turn < maxBrainstormTurns; turn++ {
		if err := o.ledger.Check(); err != nil {
			return err
		}

		task := fmt.Sprintf(brainstormPrompt, o.tc.Title, o.tc.Description, convoBlock(convo))

		res, err := o.runModel(ctx, d.ReadTools, task, model)

		o.ledger.Spend(res.TotalCostUSD)

		used := res.ModelUsed
		if used == "" {
			used = model
		}

		if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, used,
			res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
			slog.Warn("brainstorm: report usage failed", "card_id", cfg.CardID, "error", reportErr)
		}

		if err != nil {
			return fmt.Errorf("brainstorm run: %w", err)
		}

		// The model's message already reached chat via harness.Run's
		// model_response emit. Check whether the human confirmed the design.
		if design, done := extractDesign(res.Output); done {
			o.recordSection(ctx, "Design", sectionFrom("Design", design))

			return nil
		}

		o.emitAwaitingHuman(string(gatePlanApproval)) // brainstorm is part of the plan phase

		msg, werr := d.Human.Wait(ctx)

		switch {
		case errors.Is(werr, harness.ErrInboxClosed):
			_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory
				"brainstorm: promoted mid-dialogue; proceeding to planning")

			return nil
		case werr != nil:
			return werr
		}

		d.Emit.Emit(events.UserInput, map[string]any{"message_id": msg.MessageID, "content_len": len(msg.Content)})

		convo += "\n\nFACILITATOR:\n" + strings.TrimSpace(res.Output) +
			"\n\nUSER:\n" + strings.TrimSpace(msg.Content)
	}

	_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory
		"brainstorm: turn cap reached without a confirmed design; proceeding to planning")

	return nil
}

// convoBlock renders the dialogue-so-far for the brainstorm prompt slot, or a
// first-turn placeholder when empty.
func convoBlock(convo string) string {
	if strings.TrimSpace(convo) == "" {
		return "(no messages yet — open the dialogue with the user)"
	}

	return convo
}
