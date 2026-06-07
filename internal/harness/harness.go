package harness

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

// defaultMaxTurns is used when Config.MaxTurns is unset (<= 0), so a zero value
// never silently produces a no-op "completed" run.
const defaultMaxTurns = 30

type Config struct {
	Model        string
	Models       []string
	Provider     json.RawMessage
	Reasoning    json.RawMessage
	SystemPrompt string
	MaxTurns     int
	MaxCostUSD   float64 // 0 disables the cost cap
}

type Result struct {
	Completed        bool
	Reason           string // done | max_turns | max_cost | error
	Turns            int
	TotalCostUSD     float64
	ToolCallCount    int
	ToolCallFailures int
	RepairCount      int
	ModelUsed        string
}

// Run drives the bare agent loop: model call → tool dispatch → repeat, until the
// model emits no tool calls (done) or a cap trips. FSM-free; no orchestration.
func Run(ctx context.Context, client llm.LLM, reg *tools.Registry, emit *events.Emitter, task string, cfg Config) (Result, error) {
	var res Result
	var msgs []llm.Message
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
		if resp.Model != "" {
			res.ModelUsed = resp.Model
		}
		emit.Emit(events.ModelResponse, map[string]any{
			"turn": res.Turns, "finish_reason": resp.FinishReason,
			"tool_calls": len(resp.ToolCalls), "content_len": len(resp.Content),
		})
		emit.Emit(events.UsageKind, map[string]any{
			"prompt_tokens": resp.Usage.PromptTokens, "completion_tokens": resp.Usage.CompletionTokens,
			"cost_usd": resp.Usage.Cost,
		})

		msgs = append(msgs, llm.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})

		// Authoritative: tool_calls presence drives continuation, not finish_reason.
		if len(resp.ToolCalls) == 0 {
			res.Completed = true
			res.Reason = "done"
			emit.Emit(events.StateChange, map[string]any{"stop": "done", "turns": res.Turns})
			return res, nil
		}

		for _, tc := range resp.ToolCalls {
			res.ToolCallCount++
			emit.Emit(events.ToolCallKind, map[string]any{"id": tc.ID, "name": tc.Function.Name, "raw_args": tc.Function.Arguments})

			tool, ok := reg.Get(tc.Function.Name)
			if !ok {
				msg := fmt.Sprintf("unknown tool %q", tc.Function.Name)
				res.ToolCallFailures++
				msgs = append(msgs, toolResultMsg(tc.ID, msg))
				emit.Emit(events.ToolResult, map[string]any{"id": tc.ID, "error": msg})
				continue
			}

			args, err := parseArgs(tc.Function.Arguments)
			if err != nil {
				res.RepairCount++
				rm := repairMessage(tc.Function.Name, err)
				msgs = append(msgs, toolResultMsg(tc.ID, rm))
				emit.Emit(events.ToolRepair, map[string]any{"id": tc.ID, "name": tc.Function.Name, "error": err.Error()})
				continue
			}

			out, err := tool.Execute(ctx, args)
			if err != nil {
				res.ToolCallFailures++
				em := fmt.Sprintf("tool error: %v", err)
				msgs = append(msgs, toolResultMsg(tc.ID, em))
				emit.Emit(events.ToolResult, map[string]any{"id": tc.ID, "error": em})
				continue
			}
			msgs = append(msgs, toolResultMsg(tc.ID, out))
			emit.Emit(events.ToolResult, map[string]any{"id": tc.ID, "output_len": len(out)})
		}
	}
	res.Reason = "max_turns"
	emit.Emit(events.StateChange, map[string]any{"stop": "max_turns", "turns": res.Turns})
	return res, nil
}

func toolResultMsg(id, content string) llm.Message {
	if content == "" {
		content = "(no output)"
	}
	return llm.Message{Role: "tool", ToolCallID: id, Content: content}
}
