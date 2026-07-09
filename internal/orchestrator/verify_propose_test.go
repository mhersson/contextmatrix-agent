package orchestrator

import (
	"context"
	"runtime"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProposedCommand(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantCmd string
		wantOK  bool
	}{
		{"plain", `{"command":"cargo test"}`, "cargo test", true},
		{"with prose and fence", "Sure:\n```json\n{\"command\":\"go test ./...\"}\n```", "go test ./...", true},
		{"empty command is no suite", `{"command":""}`, "", false},
		{"whitespace command", `{"command":"   "}`, "", false},
		{"no json", "there is no test command here", "", false},
		{"malformed", `{"command":`, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := parseProposedCommand(tt.output)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantCmd, cmd)
		})
	}
}

func TestAcceptProposedCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{"real command", "cargo test", true},
		{"pipeline", "make check | tee log", true},
		{"deny true", "true", false},
		{"deny echo", "echo ok", false},
		{"deny colon", ": always green", false},
		{"deny exit", "exit 0", false},
		{"multiline", "cargo test\nrm -rf /", false},
		{"carriage return", "cargo test\r", false},
		{"too long", "cargo test " + string(make([]byte, maxProposedCommandLen)), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, acceptProposedCommand(tt.cmd))
		})
	}
}

// newProposeRun builds a run wired for the proposal tier: a scripted client, a
// registry that falls back to the capable default, and the given workspace.
func newProposeRun(t *testing.T, ops *fakeOps, client llm.LLM, workspace string) *run {
	t.Helper()

	d := planTestDeps(ops, client)
	d.Cfg.Workspace = workspace

	return newRun(d, cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"})
}

func TestProposeVerifyAcceptedAndCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "cargo", "bash")

	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp(`{"command":"cargo test"}`, 0.01)}}
	o := newProposeRun(t, ops, client, t.TempDir())

	plan, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceProposed, plan.Source)
	assert.Equal(t, "cargo test", plan.Display)
	assert.Equal(t, []string{"bash", "-c", "set -o pipefail; cargo test"}, plan.Argv)
	assert.True(t, ops.loggedContains("model-proposed verify command: cargo test"), "provenance is logged; logs=%v", ops.logs)
	assert.Contains(t, o.body, "## Verify Command", "the proposal is recorded on the card for a human to make durable")

	// A second resolution reuses the cache: no second model call.
	callsAfterFirst := len(client.models)
	_, err = o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Len(t, client.models, callsAfterFirst, "the accepted proposal is cached; no second model call")
}

func TestProposeVerifyRejectedDenyList(t *testing.T) {
	stubTools(t, "bash")

	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp(`{"command":"echo all good"}`, 0.01)}}
	o := newProposeRun(t, ops, client, t.TempDir())

	plan, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceNone, plan.Source, "a deny-listed lead token is rejected -> skip")
}

func TestProposeVerifyRejectedProbeFail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "bash") // pytest deliberately absent

	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp(`{"command":"pytest -q"}`, 0.01)}}
	o := newProposeRun(t, ops, client, t.TempDir())

	plan, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceNone, plan.Source, "an unrunnable proposed command is rejected -> skip")
}

func TestProposeVerifyEmptyCommand(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp(`{"command":""}`, 0.01)}}
	o := newProposeRun(t, ops, client, t.TempDir())

	plan, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceNone, plan.Source, "an empty proposal (no test suite) -> skip")
	require.False(t, ops.loggedContains("model-proposed verify command"), "no provenance log for a no-suite proposal")
}

func TestProposeVerifyBudgetParkPropagates(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{}
	o := newProposeRun(t, ops, client, t.TempDir())
	// Seed the ledger at its ceiling so the proposal's pre-check parks.
	o.ledger = NewLedger(0.01, 0.02)

	_, err := o.resolveVerify(context.Background())
	require.Error(t, err)

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be, "a budget park during proposal propagates")
}
