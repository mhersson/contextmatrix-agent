package modelexec

import (
	"context"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoLLM answers every request with a text reply naming the requested model.
type echoLLM struct{}

func (echoLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return llm.Response{Content: "reply " + req.Model, FinishReason: "stop"}, nil
}

func (e echoLLM) SendStream(ctx context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return e.Send(ctx, req)
}

func TestHarnessRunForwardsToTheHarnessLoop(t *testing.T) {
	res, err := Harness{}.Run(t.Context(), orchestrator.RunRequest{
		Client:   echoLLM{},
		Registry: tools.NewRegistry(),
		Emit:     events.NewEmitter(nil, nil),
		Task:     "task",
		Config:   harness.Config{Model: "m/1", MaxTurns: 3},
	})
	require.NoError(t, err)
	assert.Equal(t, "reply m/1", res.Output)
	assert.True(t, res.Completed)
}

func TestHarnessSpawnSubagentsKeepsSpecOrder(t *testing.T) {
	specs := []harness.SubagentSpec{
		{Role: "first", Prompt: "p", Model: "m/1", MaxTurns: 3},
		{Role: "second", Prompt: "p", Model: "m/2", MaxTurns: 3},
	}

	results, err := Harness{}.SpawnSubagents(t.Context(), orchestrator.FanoutRequest{
		Client: echoLLM{},
		Root:   t.TempDir(),
		Emit:   events.NewEmitter(nil, nil),
		Specs:  specs,
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "first", results[0].Role)
	assert.Equal(t, "reply m/1", results[0].Output)
	assert.Equal(t, "second", results[1].Role)
	assert.Equal(t, "reply m/2", results[1].Output)
}
