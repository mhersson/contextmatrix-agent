package coop

import (
	"context"
	"iter"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// seatExecutor adapts one internal seat's SeatRunner to the a2asrv
// AgentExecutor contract. One instance serves one seat; conversation state
// lives in the server's in-memory task store, so the executor is stateless.
type seatExecutor struct {
	seat   SeatConfig
	runner SeatRunner
}

// newSeatExecutor returns the a2asrv.AgentExecutor for one internal seat.
func newSeatExecutor(seat SeatConfig, runner SeatRunner) a2asrv.AgentExecutor {
	return &seatExecutor{seat: seat, runner: runner}
}

// Execute handles one moderator message. A close control completes the task
// (close is only ever sent on an existing task); a round message runs the
// seat runner and parks the task in input-required — A2A's native "your
// move" state — with the utterance as the status message.
func (e *seatExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if parseControl(execCtx.Message).Kind == controlClose {
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)

			return
		}

		if execCtx.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}

		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}

		history := rebuildHistory(execCtx.StoredTask, execCtx.Message.ID)

		utterance, costUSD, err := e.runner(ctx, e.seat, history, messageText(execCtx.Message))
		if err != nil {
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("seat error: "+err.Error()))
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, msg), nil)

			return
		}

		msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(utterance))
		setCost(msg, costUSD)
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, msg), nil)
	}
}

// Cancel acknowledges a moderator CancelTask. The canceled terminal event
// also unblocks a concurrently running Execute: a2a-go's event consumer
// returns on it, which cancels the producer context and thereby the running
// SeatRunner's ctx.
func (e *seatExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// messageText concatenates the text parts of m.
func messageText(m *a2a.Message) string {
	if m == nil {
		return ""
	}

	var b strings.Builder

	for _, p := range m.Parts {
		if p != nil {
			b.WriteString(p.Text())
		}
	}

	return b.String()
}

// rebuildHistory reconstructs the seat's prior conversation as role-tagged
// turns. a2a-go v2.3.1 stored-task semantics (verified against the module
// source) force three corrections over a naive History walk:
//
//   - the incoming round message is appended to History BEFORE Execute runs,
//     so the entry matching currentID must be dropped (it is the prompt, not
//     history);
//   - the seat's most recent utterance is parked in Status.Message and moves
//     into History only on the NEXT status update, so it must be appended;
//   - each earlier utterance was history-appended after the round message
//     that followed it, so raw History interleaves out of chronological
//     order. The turn protocol is strictly alternating (one moderator
//     message, one utterance, repeat), so re-interleaving by role restores
//     chronology.
func rebuildHistory(t *a2a.Task, currentID string) []Turn {
	if t == nil {
		return nil
	}

	var users, agents []Turn

	for _, m := range t.History {
		if m == nil || m.ID == currentID {
			continue
		}

		switch m.Role {
		case a2a.MessageRoleUser:
			users = append(users, Turn{Role: "user", Content: messageText(m)})
		case a2a.MessageRoleAgent:
			agents = append(agents, Turn{Role: "assistant", Content: messageText(m)})
		default: // non-conversational roles are not part of the exchange
		}
	}

	if sm := t.Status.Message; sm != nil && sm.Role == a2a.MessageRoleAgent && sm.ID != currentID {
		agents = append(agents, Turn{Role: "assistant", Content: messageText(sm)})
	}

	turns := make([]Turn, 0, len(users)+len(agents))

	for i := 0; i < len(users) || i < len(agents); i++ {
		if i < len(users) {
			turns = append(turns, users[i])
		}

		if i < len(agents) {
			turns = append(turns, agents[i])
		}
	}

	return turns
}
