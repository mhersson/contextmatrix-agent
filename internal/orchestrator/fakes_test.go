package orchestrator

import (
	"context"
	"fmt"
	"sync"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
)

// fakeOps is a scripted implementation of the Ops interface. It records every
// call in order so tests can assert sequencing, and exposes programmable
// returns for the methods the tests exercise. Add per-method error fields as
// needed; nil means success.
type fakeOps struct {
	mu    sync.Mutex
	calls []string

	taskContext cmclient.TaskContext
	taskCtxErr  error

	setPhaseErr error
	addLogErr   error
}

func (f *fakeOps) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call)
}

// recorded returns a copy of the call log.
func (f *fakeOps) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.calls))
	copy(out, f.calls)

	return out
}

func (f *fakeOps) ClaimCard(_ context.Context, cardID string) error {
	f.record("ClaimCard:" + cardID)

	return nil
}

func (f *fakeOps) GetTaskContext(_ context.Context, cardID string) (cmclient.TaskContext, error) {
	f.record("GetTaskContext:" + cardID)

	return f.taskContext, f.taskCtxErr
}

func (f *fakeOps) CreateCard(_ context.Context, project, parent, title, _ string, _ []string) (string, error) {
	f.record(fmt.Sprintf("CreateCard:%s/%s/%s", project, parent, title))

	return "NEW-1", nil
}

func (f *fakeOps) SetPhase(_ context.Context, cardID, phase string) error {
	f.record("SetPhase:" + phase)

	return f.setPhaseErr
}

func (f *fakeOps) TransitionCard(_ context.Context, cardID, state string) error {
	f.record("TransitionCard:" + state)

	return nil
}

func (f *fakeOps) StartReview(_ context.Context, cardID string) error {
	f.record("StartReview:" + cardID)

	return nil
}

func (f *fakeOps) IncrementReviewAttempts(_ context.Context, cardID string) (int, error) {
	f.record("IncrementReviewAttempts:" + cardID)

	return 1, nil
}

func (f *fakeOps) SubtaskStates(_ context.Context, project, parentID string) ([]cmclient.SubtaskState, error) {
	f.record(fmt.Sprintf("SubtaskStates:%s/%s", project, parentID))

	return nil, nil
}

func (f *fakeOps) AddLog(_ context.Context, cardID, message string) error {
	f.record("AddLog:" + message)

	return f.addLogErr
}

func (f *fakeOps) ReportUsage(_ context.Context, cardID, model string, promptTokens, completionTokens int64, actualCostUSD float64) error {
	f.record("ReportUsage:" + cardID)

	return nil
}

func (f *fakeOps) ReportPush(_ context.Context, cardID, branch, prURL string) error {
	f.record("ReportPush:" + cardID)

	return nil
}

func (f *fakeOps) CompleteTask(_ context.Context, cardID, summary string) error {
	f.record("CompleteTask:" + cardID)

	return nil
}

func (f *fakeOps) ReleaseCard(_ context.Context, cardID string) error {
	f.record("ReleaseCard:" + cardID)

	return nil
}

// compile-time assertion that the fake satisfies the consumer interface.
var _ Ops = (*fakeOps)(nil)
