package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

// defaultMaxTurns is used when Config.MaxTurns is unset (<= 0), so a zero value
// never silently produces a no-op "completed" run.
const defaultMaxTurns = 30

// contextLimitThreshold is the fraction of the model's context window that, once
// the prompt reaches it, makes the harness stop and return incomplete (v1 has
// no compactor). Detection uses the provider's authoritative prompt_tokens.
const contextLimitThreshold = 0.85

type Config struct {
	Model              string
	Models             []string
	Provider           json.RawMessage
	Reasoning          json.RawMessage
	SystemPrompt       string
	MaxTurns           int
	MaxCostUSD         float64             // 0 disables the cost cap
	ContextWindow      int                 // 0 disables context-limit detection
	ToolOutputMaxBytes int                 // 0 disables the tool-result size cap
	RedactToolOutput   func(string) string // nil = identity; applied before the size cap
	Inbox              Inbox               // nil = autonomous; non-nil feeds mid-run human input
}

type Result struct {
	Completed        bool
	Reason           string // done | max_turns | max_cost | context_limit | error
	Turns            int
	TotalCostUSD     float64
	PromptTokens     int64
	CompletionTokens int64
	ToolCallCount    int
	ToolCallFailures int
	RepairCount      int
	ModelUsed        string
	Output           string // final assistant text of the last turn
}

// Run drives the bare agent loop: model call → tool dispatch → repeat, until the
// model emits no tool calls (done) or a cap trips. FSM-free; no orchestration.
func Run(ctx context.Context, client llm.LLM, reg *tools.Registry, emit *events.Emitter, task string, cfg Config) (Result, error) {
	var (
		res  Result
		msgs []llm.Message
	)

	if cfg.SystemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: cfg.SystemPrompt})
	}

	msgs = append(msgs, llm.Message{Role: "user", Content: task})

	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = defaultMaxTurns
	}

	for res.Turns < cfg.MaxTurns {
		if cfg.MaxCostUSD > 0 && res.TotalCostUSD >= cfg.MaxCostUSD {
			res.Reason = "max_cost"
			emit.Emit(events.StateChange, map[string]any{"stop": "max_cost", "cost_usd": res.TotalCostUSD})

			return res, nil
		}

		msgs = drainInbox(cfg, msgs, emit)

		res.Turns++

		req := llm.Request{
			Model:     cfg.Model,
			Models:    cfg.Models,
			Provider:  cfg.Provider,
			Reasoning: cfg.Reasoning,
			Messages:  msgs,
			Tools:     reg.Schemas(),
		}
		emit.Emit(events.ModelRequest, map[string]any{"turn": res.Turns, "model": cfg.Model, "messages": len(msgs)})

		resp, err := client.SendStream(ctx, req, nil)
		if err != nil {
			emit.Emit(events.ErrorKind, map[string]any{"error": err.Error()})

			res.Reason = "error"

			return res, err
		}

		res.TotalCostUSD += resp.Usage.Cost
		res.PromptTokens += int64(resp.Usage.PromptTokens)
		res.CompletionTokens += int64(resp.Usage.CompletionTokens)

		if resp.Model != "" {
			res.ModelUsed = resp.Model
		}

		res.Output = resp.Content
		emit.Emit(events.ModelResponse, map[string]any{
			"turn": res.Turns, "finish_reason": resp.FinishReason,
			"tool_calls": len(resp.ToolCalls), "content_len": len(resp.Content),
			"content": resp.Content, "model": cfg.Model,
		})
		emit.Emit(events.UsageKind, map[string]any{
			"prompt_tokens": resp.Usage.PromptTokens, "completion_tokens": resp.Usage.CompletionTokens,
			"cost_usd": resp.Usage.Cost, "model": cfg.Model,
		})

		if cfg.ContextWindow > 0 && resp.Usage.PromptTokens >= int(contextLimitThreshold*float64(cfg.ContextWindow)) {
			res.Reason = "context_limit"

			emit.Emit(events.ContextLimit, map[string]any{
				"prompt_tokens":  resp.Usage.PromptTokens,
				"context_window": cfg.ContextWindow,
				"ratio":          float64(resp.Usage.PromptTokens) / float64(cfg.ContextWindow),
				"threshold":      contextLimitThreshold,
			})

			return res, nil
		}

		msgs = append(msgs, llm.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})

		// Authoritative: tool_calls presence drives continuation, not finish_reason.
		if len(resp.ToolCalls) == 0 {
			if cfg.Inbox == nil {
				res.Completed = true
				res.Reason = "done"
				emit.Emit(events.StateChange, map[string]any{"stop": "done", "turns": res.Turns})

				return res, nil
			}

			if pending := cfg.Inbox.Drain(); len(pending) > 0 {
				for _, um := range pending {
					emit.Emit(events.UserInput, map[string]any{"message_id": um.MessageID, "content_len": len(um.Content)})
					msgs = append(msgs, llm.Message{Role: "user", Content: um.Content})
				}

				continue
			}

			emit.Emit(events.StateChange, map[string]any{"state": "awaiting_human", "turns": res.Turns})

			um, err := cfg.Inbox.Wait(ctx)

			switch {
			case errors.Is(err, ErrInboxClosed):
				res.Completed = true
				res.Reason = "done"
				emit.Emit(events.StateChange, map[string]any{"stop": "done", "turns": res.Turns})

				return res, nil
			case err != nil:
				res.Reason = "canceled"

				emit.Emit(events.StateChange, map[string]any{"stop": "canceled"})

				return res, err
			}

			emit.Emit(events.UserInput, map[string]any{"message_id": um.MessageID, "content_len": len(um.Content)})
			msgs = append(msgs, llm.Message{Role: "user", Content: um.Content})

			continue
		}

		interrupted := false

		var pendingMsgs []UserMessage

		for i, tc := range resp.ToolCalls {
			if interrupted {
				msgs = append(msgs, toolResultMsg(tc.ID, "skipped: user interjected"))
				emit.Emit(events.ToolResult, map[string]any{"id": tc.ID, "skipped": true})

				continue
			}

			// dispatch the call; the body's internal short-circuits are returns
			// so the interrupt check below always runs after each executed call.
			func() {
				res.ToolCallCount++

				emit.Emit(events.ToolCallKind, map[string]any{"id": tc.ID, "name": tc.Function.Name, "raw_args": tc.Function.Arguments})

				tool, ok := reg.Get(tc.Function.Name)
				if !ok {
					msg := fmt.Sprintf("unknown tool %q", tc.Function.Name)
					res.ToolCallFailures++

					msgs = append(msgs, toolResultMsg(tc.ID, msg))
					emit.Emit(events.ToolResult, map[string]any{"id": tc.ID, "error": msg})

					return
				}

				args, err := parseArgs(tc.Function.Arguments)
				if err != nil {
					res.RepairCount++
					rm := repairMessage(tc.Function.Name, err)
					msgs = append(msgs, toolResultMsg(tc.ID, rm))
					emit.Emit(events.ToolRepair, map[string]any{"id": tc.ID, "name": tc.Function.Name, "error": err.Error()})

					return
				}

				out, err := tool.Execute(ctx, args)
				if err != nil {
					res.ToolCallFailures++

					em := fmt.Sprintf("tool error: %v", err)
					if cfg.RedactToolOutput != nil {
						em = cfg.RedactToolOutput(em)
					}

					msgs = append(msgs, toolResultMsg(tc.ID, em))
					emit.Emit(events.ToolResult, map[string]any{"id": tc.ID, "error": em})

					return
				}

				if cfg.RedactToolOutput != nil {
					out = cfg.RedactToolOutput(out)
				}

				out = tools.HeadTail(out, cfg.ToolOutputMaxBytes)
				msgs = append(msgs, toolResultMsg(tc.ID, out))
				emit.Emit(events.ToolResult, map[string]any{"id": tc.ID, "output_len": len(out)})
			}()

			// Drain mid-batch only if there are remaining calls to skip.
			if cfg.Inbox != nil && i < len(resp.ToolCalls)-1 {
				if pending := cfg.Inbox.Drain(); len(pending) > 0 {
					interrupted = true
					pendingMsgs = pending // stash; appended after the loop
				}
			}
		}

		// All tool results (executed and skipped) precede the user messages,
		// preserving the assistant/tool pairing the OpenRouter API requires.
		for _, um := range pendingMsgs {
			emit.Emit(events.UserInput, map[string]any{"message_id": um.MessageID, "content_len": len(um.Content)})
			msgs = append(msgs, llm.Message{Role: "user", Content: um.Content})
		}
	}

	res.Reason = "max_turns"
	emit.Emit(events.StateChange, map[string]any{"stop": "max_turns", "turns": res.Turns})

	return res, nil
}

// drainInbox appends any pending human messages (emitting a user_input event
// per message) and returns the extended slice. A nil Inbox is a no-op.
func drainInbox(cfg Config, msgs []llm.Message, emit *events.Emitter) []llm.Message {
	if cfg.Inbox == nil {
		return msgs
	}

	for _, um := range cfg.Inbox.Drain() {
		emit.Emit(events.UserInput, map[string]any{"message_id": um.MessageID, "content_len": len(um.Content)})
		msgs = append(msgs, llm.Message{Role: "user", Content: um.Content})
	}

	return msgs
}

func toolResultMsg(id, content string) llm.Message {
	if content == "" {
		content = "(no output)"
	}

	return llm.Message{Role: "tool", ToolCallID: id, Content: content}
}
