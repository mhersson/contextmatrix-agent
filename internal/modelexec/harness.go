// Package modelexec holds the model executors behind orchestrator.Executor.
package modelexec

import (
	"context"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/harness"
)

// Harness runs model work through the in-process harness loop over the run's
// LLM client.
type Harness struct{}

var _ orchestrator.Executor = Harness{}

func (Harness) Run(ctx context.Context, req orchestrator.RunRequest) (harness.Result, error) {
	return harness.Run(ctx, req.Client, req.Registry, req.Emit, req.Task, req.Config)
}

func (Harness) SubscriptionUsage() []cmclient.SubscriptionUsage { return nil }

func (Harness) SpawnSubagents(ctx context.Context, req orchestrator.FanoutRequest) ([]harness.SubagentResult, error) {
	return harness.SpawnSubagents(ctx, req.Client, req.Root, req.Emit, req.Specs, req.Opts)
}
