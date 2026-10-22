package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replayedThinking is one reasoning block as a provider returns it.
var replayedThinking = json.RawMessage(`[{"type":"reasoning.text","text":"weigh the options","signature":"c2ln","index":0}]`)

// replayTestRegistry catalogues models on both sides of replayMinContextWindow.
func replayTestRegistry() *registry.Registry {
	return registry.NewRegistryFromParts(llm.Catalog{
		{ID: "wide/model", ContextLength: 1_048_576, SupportedParameters: []string{"tools"}},
		{ID: "other/model", ContextLength: 1_048_576, SupportedParameters: []string{"tools"}},
		{ID: "edge/model", ContextLength: 400_000, SupportedParameters: []string{"tools"}},
		{ID: "narrow/model", ContextLength: 262_144, SupportedParameters: []string{"tools"}},
	}, registry.Priors{}, nil, nil, "wide/model")
}

// thinkingToolCall reasons, then calls a tool no registry has, so the run
// takes a second turn whose request carries this reply back.
func thinkingToolCall() llm.Response {
	return llm.Response{
		FinishReason:     "tool_calls",
		ReasoningDetails: replayedThinking,
		Provider:         "Provider A",
		ToolCalls: []llm.ToolCall{{
			ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: "nope", Arguments: "{}"},
		}},
	}
}

// assistantDetails is the reasoning of the first assistant message in msgs.
func assistantDetails(msgs []llm.Message) json.RawMessage {
	for _, m := range msgs {
		if m.Role == "assistant" {
			return m.ReasoningDetails
		}
	}

	return nil
}

// modelLLM answers each model from its own script, so concurrent children
// stay deterministic, and records each model's requests.
type modelLLM struct {
	mu      sync.Mutex
	scripts map[string][]llm.Response
	seen    map[string][][]llm.Message
}

func (m *modelLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	return m.SendStream(ctx, req, nil)
}

func (m *modelLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := len(m.seen[req.Model])
	m.seen[req.Model] = append(m.seen[req.Model], req.Messages)

	if script := m.scripts[req.Model]; n < len(script) {
		return script[n], nil
	}

	return stopResp("done", 0), nil
}

func replayRun(client llm.LLM, on bool) *run {
	d := planTestDeps(&fakeOps{}, client)
	d.Registry = replayTestRegistry()
	d.Cfg.ReplayReasoning = on

	return newRun(d, cmclient.TaskContext{})
}

func TestReplaysByModel(t *testing.T) {
	tests := []struct {
		name  string
		on    bool
		model string
		want  bool
	}{
		{"on, wide window", true, "wide/model", true},
		{"off", false, "wide/model", false},
		{"on, at the floor", true, "edge/model", true},
		{"on, under the floor", true, "narrow/model", false},
		{"on, uncatalogued", true, "ghost/model", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := replayRun(&planLLM{}, tt.on)

			assert.Equal(t, tt.want, o.replays(tt.model))
			assert.Equal(t, tt.want, o.harnessConfig(tt.model).ReplayReasoning, "every phase run takes the decision from harnessConfig")
		})
	}
}

func TestRunModelReplaysReasoningOnTheWire(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("replay=%t", on), func(t *testing.T) {
			client := &planLLM{responses: []llm.Response{thinkingToolCall(), stopResp("done", 0)}}
			o := replayRun(client, on)

			_, _, err := o.runModel(t.Context(), tools.NewRegistry(), "task", "wide/model")
			require.NoError(t, err)

			got := assistantDetails(client.messagesOf(1))
			if on {
				assert.Equal(t, replayedThinking, got, "the second request carries the first reply's reasoning")
			} else {
				assert.Empty(t, got, "off sends none")
			}
		})
	}
}

func TestSpawnSubagentsReplaysPerChildModel(t *testing.T) {
	models := []string{"wide/model", "narrow/model", "ghost/model"}
	client := &modelLLM{scripts: map[string][]llm.Response{}, seen: map[string][][]llm.Message{}}
	specs := make([]harness.SubagentSpec, len(models))

	for i, m := range models {
		client.scripts[m] = []llm.Response{thinkingToolCall(), stopResp("review by "+m, 0)}
		specs[i] = harness.SubagentSpec{Role: m, Prompt: "review", Model: m, MaxTurns: 4}
	}

	o := replayRun(client, true)

	results, err := o.spawnSubagents(t.Context(), client, t.TempDir(), specs, harness.SubagentOpts{})
	require.NoError(t, err)
	require.Len(t, results, len(models))

	for i, m := range models {
		assert.Equal(t, m, results[i].Role, "results keep spec order")
		assert.Equal(t, "review by "+m, results[i].Output)
		require.Len(t, client.seen[m], 2, "%s took two turns", m)
	}

	assert.Equal(t, replayedThinking, assistantDetails(client.seen["wide/model"][1]), "a replaying child gets its reasoning back")
	assert.Empty(t, assistantDetails(client.seen["narrow/model"][1]), "a child under the window floor gets none")
	assert.Empty(t, assistantDetails(client.seen["ghost/model"][1]), "an uncatalogued child gets none")
}

func TestSpawnSubagentsFanOutCount(t *testing.T) {
	tests := []struct {
		name   string
		on     bool
		models []string
		want   int
	}{
		{"replay off: one fan-out as today", false, []string{"wide/model", "narrow/model"}, 1},
		{"every child replays: one fan-out", true, []string{"wide/model", "other/model"}, 1},
		{"children differ: one fan-out per side", true, []string{"wide/model", "narrow/model"}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var transcript bytes.Buffer

			client := &modelLLM{scripts: map[string][]llm.Response{}, seen: map[string][][]llm.Message{}}
			o := replayRun(client, tt.on)
			o.d.Emit = events.NewEmitter(nil, &transcript)

			specs := make([]harness.SubagentSpec, len(tt.models))
			for i, m := range tt.models {
				specs[i] = harness.SubagentSpec{Role: m, Prompt: "review", Model: m}
			}

			_, err := o.spawnSubagents(t.Context(), client, t.TempDir(), specs, harness.SubagentOpts{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, strings.Count(transcript.String(), `"spawn_subagents"`))
		})
	}
}
