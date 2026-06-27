package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
)

// gateOutcome is the human's decision at a sign-off gate. There is no hard
// reject: anything that is not an approval is an adjustment that loops with the
// human's feedback. A human ends a run for good with an end_session frame.
type gateOutcome int

const (
	gateApprove gateOutcome = iota
	gateAdjust
)

// gateKind names the gate for the classification prompt and activity log.
type gateKind string

const (
	gatePlanApproval   gateKind = "plan-approval"
	gateReviewDecision gateKind = "review-decision"
)

// gate presents `presentation` to the human and blocks for their verdict.
//
// Autonomous (Cfg.Interactive == false) is pure pass-through: it returns
// gateApprove without emitting anything or touching the inbox, so the autonomous
// phase flow is byte-for-byte unchanged.
//
// HITL emits the presentation (a model_response event -> chat) and the
// awaiting-human signal, then blocks on the inbox:
//   - a human turn   -> classify into approve/adjust (one cheap model call);
//   - ErrInboxClosed -> the card was promoted mid-run: return gateApprove so the
//     run finishes autonomously (the inbox stays closed, so every later gate
//     passes through too);
//   - ctx error      -> end_session/kill: return the error; the worker maps it to
//     the graceful WIP-push park.
//
// The returned outcome is meaningful only when err == nil; callers check err
// first.
func (o *run) gate(ctx context.Context, kind gateKind, model, presentation string) (gateOutcome, string, error) {
	if !o.d.Cfg.Interactive {
		return gateApprove, "", nil
	}

	o.presentToHuman(presentation)
	o.emitAwaitingHuman(string(kind))

	msg, err := o.d.Human.Wait(ctx)

	switch {
	case errors.Is(err, harness.ErrInboxClosed):
		_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory
			fmt.Sprintf("%s gate: promoted mid-run; proceeding autonomously", kind))

		return gateApprove, "", nil
	case err != nil:
		return gateApprove, "", err
	}

	o.d.Emit.Emit(events.UserInput, map[string]any{"message_id": msg.MessageID, "content_len": len(msg.Content)})

	return o.classifyVerdict(ctx, kind, model, msg.Content)
}

// classifyVerdict runs one cheap model call mapping the human's freeform reply to
// an approve/adjust verdict plus the feedback to fold into the next round. A
// parse failure or any non-"approve" verdict is treated as adjust with the raw
// reply as feedback — adjust is the fail-safe default, never an accidental
// approval. A budget breach returns the *BudgetExceededError so execute() parks.
func (o *run) classifyVerdict(ctx context.Context, kind gateKind, model, reply string) (gateOutcome, string, error) {
	if err := o.ledger.Check(); err != nil {
		return gateApprove, "", err
	}

	task := fmt.Sprintf(gateClassifyPrompt, kind, reply)

	res, err := o.runModel(ctx, o.d.ReadTools, task, model)

	o.ledger.Spend(res.TotalCostUSD)

	used := res.ModelUsed
	if used == "" {
		used = model
	}

	if reportErr := o.d.Ops.ReportUsage(ctx, o.d.Cfg.CardID, used,
		res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
		slog.Warn("gate: report classification usage failed", "card_id", o.d.Cfg.CardID, "error", reportErr)
	}

	if err != nil {
		// A classification model error must not silently approve.
		slog.Warn("gate: classification model run failed; treating as adjust",
			"card_id", o.d.Cfg.CardID, "gate", kind, "error", err)

		return gateAdjust, reply, nil
	}

	verdict, feedback := parseGateVerdict(res.Output)
	if verdict == "approve" {
		return gateApprove, "", nil
	}

	if feedback == "" {
		feedback = reply
	}

	return gateAdjust, feedback, nil
}

// gateVerdict is the classification model's structured output.
type gateVerdict struct {
	Verdict  string `json:"verdict"`
	Feedback string `json:"feedback"`
}

// parseGateVerdict extracts the verdict JSON (tolerating prose / code fences). A
// missing or malformed object yields ("", "") so the caller falls back to adjust.
func parseGateVerdict(s string) (verdict, feedback string) {
	raw, ok := extractJSON(s)
	if !ok {
		return "", ""
	}

	var v gateVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", ""
	}

	return strings.ToLower(strings.TrimSpace(v.Verdict)), strings.TrimSpace(v.Feedback)
}

// presentToHuman emits the orchestrator's chat message to the human: a
// model_response event whose content logbridge maps to a text LogEntry on the
// /logs SSE stream (untruncated).
func (o *run) presentToHuman(text string) {
	o.d.Emit.Emit(events.ModelResponse, map[string]any{"content": text})
}

// emitAwaitingHuman emits the awaiting-human signal so the serve-side idle
// watchdog treats the parked gate as live, not stalled.
func (o *run) emitAwaitingHuman(gate string) {
	o.d.Emit.Emit(events.StateChange, map[string]any{"state": "awaiting_human", "gate": gate})
}
