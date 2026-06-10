package eval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// costLLM returns one no-tool-call response carrying a fixed cost, so each run
// "completes" immediately and adds Cost to the matrix total.
type costLLM struct{ cost float64 }

func (c costLLM) Send(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{FinishReason: "stop", Usage: llm.Usage{Cost: c.cost}}, nil
}

func (c costLLM) SendStream(_ context.Context, _ llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return llm.Response{FinishReason: "stop", Usage: llm.Usage{Cost: c.cost}}, nil
}

// fakeTask is a Provision/Check stub that never touches the filesystem or LLM
// output; pass is fixed.
type fakeTask struct {
	name string
	role registry.Role
	pass bool
}

func (t fakeTask) Name() string           { return t.name }
func (t fakeTask) Role() registry.Role    { return t.role }
func (t fakeTask) Provision(string) error { return nil }
func (t fakeTask) Prompt() string         { return "do it" }
func (t fakeTask) Check(context.Context, string, harness.Result) (harness.Verdict, error) {
	return harness.Verdict{OK: t.pass}, nil
}

func TestRunMatrixAggregates(t *testing.T) {
	tasks := []Task{
		fakeTask{name: "c1", role: registry.RoleCoder, pass: true},
		fakeTask{name: "r1", role: registry.RoleReviewer, pass: false},
	}
	mr, err := RunMatrix(context.Background(), costLLM{cost: 0.01}, MatrixOpts{
		Models: []string{"m1"}, Tasks: tasks, Samples: 2, MaxTurns: 3,
	})
	require.NoError(t, err)
	assert.Len(t, mr.Outcomes, 4) // 1 model × 2 tasks × 2 samples
	assert.InEpsilon(t, 0.04, mr.TotalCost, 1e-9)
	assert.False(t, mr.Aborted)
}

func TestRunMatrixBudgetAbort(t *testing.T) {
	tasks := []Task{fakeTask{name: "c1", role: registry.RoleCoder, pass: true}}
	mr, err := RunMatrix(context.Background(), costLLM{cost: 0.50}, MatrixOpts{
		Models: []string{"m1"}, Tasks: tasks, Samples: 5, MaxTurns: 3, MaxTotalCost: 0.60,
	})
	require.NoError(t, err)
	assert.True(t, mr.Aborted)
	assert.Len(t, mr.Outcomes, 2) // stops before the 3rd run (cost would be 1.50 >= 0.60)
}

// errLLM fails every call, simulating a transient provider/stream error.
type errLLM struct{}

func (errLLM) Send(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("provider stream error")
}

func (errLLM) SendStream(context.Context, llm.Request, func(llm.Delta)) (llm.Response, error) {
	return llm.Response{}, errors.New("provider stream error")
}

func TestRunMatrixSkipsErroredRuns(t *testing.T) {
	tasks := []Task{fakeTask{name: "c1", role: registry.RoleCoder, pass: true}}
	mr, err := RunMatrix(context.Background(), errLLM{}, MatrixOpts{
		Models: []string{"m1"}, Tasks: tasks, Samples: 3, MaxTurns: 3,
	})
	require.NoError(t, err)       // a per-run error must NOT abort the sweep
	assert.Equal(t, 3, mr.Errors) // all 3 runs errored and were skipped
	assert.Empty(t, mr.Outcomes)  // nothing scored
	assert.False(t, mr.Aborted)
}

// captureLLM records the last request's Provider so a test can assert that
// MatrixOpts.Provider is threaded all the way to the wire.
type captureLLM struct{ provider *json.RawMessage }

func (c captureLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	*c.provider = req.Provider

	return llm.Response{FinishReason: "stop"}, nil
}

func (c captureLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	*c.provider = req.Provider

	return llm.Response{FinishReason: "stop"}, nil
}

func TestRunMatrixForwardsProvider(t *testing.T) {
	var seen json.RawMessage

	prov := json.RawMessage(`{"sort":"throughput","require_parameters":true}`)
	_, err := RunMatrix(context.Background(), captureLLM{provider: &seen}, MatrixOpts{
		Models:  []string{"m1"},
		Tasks:   []Task{fakeTask{name: "c", role: registry.RoleCoder, pass: true}},
		Samples: 1, MaxTurns: 2, Provider: prov,
	})
	require.NoError(t, err)
	assert.JSONEq(t, string(prov), string(seen))
}
