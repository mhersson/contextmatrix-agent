package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
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

	setPhaseErr    error
	addLogErr      error
	claimErr       error
	updateBodyErr  error
	releaseCardErr error

	// logs captures every AddLog message (verbatim) so model-selection tests can
	// assert the activity feed received the expected entry.
	logs []string

	// bodyUpdates captures every UpdateCardBody body (verbatim) so history tests
	// can assert the parent card accumulated the expected sections.
	bodyUpdates []string

	// IncrementReviewAttempts scripting: reviewAttempts seeds the counter; each
	// call increments it and returns the new running total, mirroring the server
	// semantics (the card's persisted review_attempts plus this increment).
	reviewAttempts int

	// CreateCard scripting: createdIDs supplies the returned card ID per call
	// (index-aligned to call order); when exhausted, IDs fall back to NEW-<n>.
	// createCardArgs captures every CreateCard invocation for dependency-edge
	// assertions. createCardErr fails every call; nil = success.
	createdIDs     []string
	createCardArgs []createCardCall
	createCardErr  error

	// SubtaskStates scripting.
	subtaskStates    []cmclient.SubtaskState
	subtaskStatesErr error

	// ReportPush scripting: reportPushURLs captures the pr_url passed on each
	// call so integrate tests can assert the PR URL flowing through.
	reportPushURLs []string

	// ReportModelOutcomes scripting: reportOutcomes captures each call's outcome
	// rows (index-aligned to call order); reportOutcomesErr fails every call.
	reportOutcomes    [][]cmclient.ModelOutcome
	reportOutcomesErr error
}

// createCardCall is a recorded CreateCard invocation.
type createCardCall struct {
	project   string
	parent    string
	title     string
	body      string
	dependsOn []string
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

	return f.claimErr
}

func (f *fakeOps) Heartbeat(_ context.Context, cardID string) error {
	f.record("Heartbeat:" + cardID)

	return nil
}

func (f *fakeOps) GetTaskContext(_ context.Context, cardID string, _ bool) (cmclient.TaskContext, error) {
	f.record("GetTaskContext:" + cardID)

	return f.taskContext, f.taskCtxErr
}

func (f *fakeOps) CreateCard(_ context.Context, project, parent, title, body string, dependsOn []string) (string, error) {
	f.mu.Lock()
	idx := len(f.createCardArgs)
	f.createCardArgs = append(f.createCardArgs, createCardCall{
		project:   project,
		parent:    parent,
		title:     title,
		body:      body,
		dependsOn: append([]string(nil), dependsOn...),
	})
	f.mu.Unlock()

	f.record(fmt.Sprintf("CreateCard:%s/%s/%s", project, parent, title))

	if f.createCardErr != nil {
		return "", f.createCardErr
	}

	if idx < len(f.createdIDs) {
		return f.createdIDs[idx], nil
	}

	return fmt.Sprintf("NEW-%d", idx+1), nil
}

func (f *fakeOps) SetPhase(_ context.Context, cardID, phase string) error {
	f.record("SetPhase:" + phase)

	return f.setPhaseErr
}

func (f *fakeOps) UpdateCardBody(_ context.Context, cardID, body string) error {
	f.mu.Lock()
	f.bodyUpdates = append(f.bodyUpdates, body)
	f.mu.Unlock()

	f.record("UpdateCardBody:" + cardID)

	return f.updateBodyErr
}

// lastBody returns the most recent UpdateCardBody body, or "" if none.
func (f *fakeOps) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.bodyUpdates) == 0 {
		return ""
	}

	return f.bodyUpdates[len(f.bodyUpdates)-1]
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

	f.mu.Lock()
	defer f.mu.Unlock()

	f.reviewAttempts++

	return f.reviewAttempts, nil
}

func (f *fakeOps) SubtaskStates(_ context.Context, project, parentID string) ([]cmclient.SubtaskState, error) {
	f.record(fmt.Sprintf("SubtaskStates:%s/%s", project, parentID))

	return f.subtaskStates, f.subtaskStatesErr
}

func (f *fakeOps) AddLog(_ context.Context, cardID, message string) error {
	f.mu.Lock()
	f.logs = append(f.logs, message)
	f.mu.Unlock()

	f.record("AddLog:" + message)

	return f.addLogErr
}

// loggedContains reports whether any AddLog message contains sub.
func (f *fakeOps) loggedContains(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, m := range f.logs {
		if strings.Contains(m, sub) {
			return true
		}
	}

	return false
}

func (f *fakeOps) ReportUsage(_ context.Context, cardID, model string, promptTokens, completionTokens int64, actualCostUSD float64) error {
	f.record("ReportUsage:" + cardID)

	return nil
}

func (f *fakeOps) ReportPush(_ context.Context, cardID, branch, prURL string) error {
	f.mu.Lock()
	f.reportPushURLs = append(f.reportPushURLs, prURL)
	f.mu.Unlock()

	f.record("ReportPush:" + cardID)

	return nil
}

func (f *fakeOps) BlacklistModel(_ context.Context, cardID, model, reason string) error {
	f.record(fmt.Sprintf("BlacklistModel:%s/%s", cardID, model))

	return nil
}

func (f *fakeOps) CompleteTask(_ context.Context, cardID, summary string) error {
	f.record("CompleteTask:" + cardID)

	return nil
}

func (f *fakeOps) ReleaseCard(_ context.Context, cardID string) error {
	f.record("ReleaseCard:" + cardID)

	return f.releaseCardErr
}

func (f *fakeOps) ReportModelOutcomes(_ context.Context, cardID string, outcomes []cmclient.ModelOutcome) error {
	f.mu.Lock()
	f.reportOutcomes = append(f.reportOutcomes, outcomes)
	f.mu.Unlock()

	f.record("ReportModelOutcomes:" + cardID)

	return f.reportOutcomesErr
}

// compile-time assertion that the fake satisfies the consumer interface.
var _ Ops = (*fakeOps)(nil)

// fakeGit is a scripted implementation of the GitOps interface. It records every
// call in order so tests can assert sequencing, and exposes programmable returns
// for the methods the execute phase exercises. Only the methods the execute
// phase calls carry interesting behaviour; the rest record and return zero.
type fakeGit struct {
	mu    sync.Mutex
	calls []string

	// CommitWithMessage scripting: committed is the returned "something was
	// committed" flag; commitErr fails the call. commitMsgs captures each
	// message passed so tests can assert the extracted commit line.
	committed  bool
	commitErr  error
	commitMsgs []string

	// Push scripting: pushErr fails the call; pushBranches captures each branch.
	pushErr      error
	pushBranches []string

	// LastCommitTouching scripting: lastCommitTarget is the returned target SHA
	// (empty -> the caller falls back to HEAD); lastCommitPaths captures the path
	// set passed on each call so tests can assert the fixup targeting input.
	lastCommitTarget string
	lastCommitPaths  [][]string

	// Integrate-phase scripting.
	remoteTip      string // RemoteTip return ("" -> plain push path / absent branch)
	remoteTipErr   error  // RemoteTip error (resume probe failure -> fatal)
	rebaseErr      error  // RebaseAutosquash return (ErrRebaseConflict -> fallback)
	mergeBaseValue string // MergeBase return

	// Resume-phase scripting: fetchErr/checkoutErr drive the transient-failure
	// fatal paths (the branch exists per remoteTip, but fetch/checkout fail).
	// leaseBranches/leaseTips capture each ForcePushWithLease branch and expected
	// tip (index-aligned) so first-push-lease tests can assert the exact values
	// reaching the git layer.
	fetchErr      error
	checkoutErr   error
	leaseBranches []string
	leaseTips     []string

	// Diff/Head scripting: diffBases captures the base of each Diff call in order
	// so delta-review tests can assert round 1 diffs the base branch and later
	// rounds diff the captured reviewed head. headSHA is what Head returns; the
	// default "" preserves the no-snapshot behaviour the other review tests rely on.
	diffBases []string
	headSHA   string

	// Worktree/branch lifecycle scripting (Best-of-N candidate fan-out):
	// worktreeErr fails AddWorktree and RemoveWorktree; deleteBranchErr fails
	// DeleteBranch; hardResetErr fails HardReset. removedWorktrees,
	// deletedBranches, and hardResetRefs capture each call's argument, in order.
	worktreeErr      error
	deleteBranchErr  error
	hardResetErr     error
	removedWorktrees []string
	deletedBranches  []string
	hardResetRefs    []string

	// AddInfoExclude scripting: infoExcludes captures each pattern passed so
	// fan-out tests can assert the candidate-worktree exclude was written;
	// infoExcludeErr fails every call.
	infoExcludes   []string
	infoExcludeErr error
}

// assertErr builds a sentinel error for fake scripting in tests.
func assertErr(msg string) error { return errors.New(msg) }

func (g *fakeGit) record(call string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.calls = append(g.calls, call)
}

// recorded returns a copy of the call log.
func (g *fakeGit) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]string, len(g.calls))
	copy(out, g.calls)

	return out
}

func (g *fakeGit) CommitWithMessage(_ context.Context, message string) (bool, error) {
	g.mu.Lock()
	g.commitMsgs = append(g.commitMsgs, message)
	g.mu.Unlock()

	g.record("CommitWithMessage")

	return g.committed, g.commitErr
}

func (g *fakeGit) Push(_ context.Context, branch string) error {
	g.mu.Lock()
	g.pushBranches = append(g.pushBranches, branch)
	g.mu.Unlock()

	g.record("Push:" + branch)

	return g.pushErr
}

func (g *fakeGit) ForcePushWithLease(_ context.Context, branch, expectedTip string) error {
	g.mu.Lock()
	g.leaseBranches = append(g.leaseBranches, branch)
	g.leaseTips = append(g.leaseTips, expectedTip)
	g.mu.Unlock()

	g.record("ForcePushWithLease:" + branch)

	return nil
}

func (g *fakeGit) Fetch(_ context.Context, ref string) error {
	g.record("Fetch:" + ref)

	return g.fetchErr
}

func (g *fakeGit) RemoteTip(_ context.Context, branch string) (string, error) {
	g.record("RemoteTip:" + branch)

	return g.remoteTip, g.remoteTipErr
}

func (g *fakeGit) MergeBase(_ context.Context, a, b string) (string, error) {
	g.record("MergeBase")

	return g.mergeBaseValue, nil
}

func (g *fakeGit) CommitFixup(_ context.Context, target string) (bool, error) {
	g.record("CommitFixup:" + target)

	return g.committed, g.commitErr
}

func (g *fakeGit) LastCommitTouching(_ context.Context, paths []string) (string, error) {
	g.mu.Lock()
	g.lastCommitPaths = append(g.lastCommitPaths, append([]string(nil), paths...))
	g.mu.Unlock()

	g.record("LastCommitTouching")

	return g.lastCommitTarget, nil
}

func (g *fakeGit) RebaseAutosquash(_ context.Context, onto string) error {
	g.record("RebaseAutosquash:" + onto)

	return g.rebaseErr
}

func (g *fakeGit) SoftReset(_ context.Context, to string) error {
	g.record("SoftReset:" + to)

	return nil
}

func (g *fakeGit) Head(_ context.Context) (string, error) {
	g.record("Head")

	return g.headSHA, nil
}

// assertOrder fails the test unless the named calls appear in g's recorded log
// in the given relative order (gaps allowed). Missing calls fail too.
func (g *fakeGit) assertOrder(t *testing.T, names ...string) {
	t.Helper()

	calls := g.recorded()
	prev := -1

	for _, n := range names {
		idx := indexOfCall(calls, n)
		if idx < 0 {
			t.Fatalf("expected call %q not recorded; git=%v", n, calls)
		}

		if idx <= prev {
			t.Fatalf("call %q out of order; git=%v", n, calls)
		}

		prev = idx
	}
}

func (g *fakeGit) Checkout(_ context.Context, ref string) error {
	g.record("Checkout:" + ref)

	return g.checkoutErr
}

func (g *fakeGit) Diff(_ context.Context, base string) (string, error) {
	g.mu.Lock()
	g.diffBases = append(g.diffBases, base)
	g.mu.Unlock()

	g.record("Diff")

	return "", nil
}

func (g *fakeGit) AddWorktree(_ context.Context, path, branch, startRef string) error {
	g.record("AddWorktree:" + branch)

	return g.worktreeErr
}

func (g *fakeGit) RemoveWorktree(_ context.Context, path string) error {
	g.mu.Lock()
	g.removedWorktrees = append(g.removedWorktrees, path)
	g.mu.Unlock()

	g.record("RemoveWorktree:" + path)

	return g.worktreeErr
}

func (g *fakeGit) DeleteBranch(_ context.Context, name string) error {
	g.mu.Lock()
	g.deletedBranches = append(g.deletedBranches, name)
	g.mu.Unlock()

	g.record("DeleteBranch:" + name)

	return g.deleteBranchErr
}

func (g *fakeGit) HardReset(_ context.Context, ref string) error {
	g.mu.Lock()
	g.hardResetRefs = append(g.hardResetRefs, ref)
	g.mu.Unlock()

	g.record("HardReset:" + ref)

	return g.hardResetErr
}

func (g *fakeGit) DiffStat(_ context.Context, base string) (string, error) {
	g.record("DiffStat")

	return "", nil
}

func (g *fakeGit) DisableAutoGC(_ context.Context) error {
	g.record("DisableAutoGC")

	return nil
}

func (g *fakeGit) AddInfoExclude(_ context.Context, pattern string) error {
	g.mu.Lock()
	g.infoExcludes = append(g.infoExcludes, pattern)
	g.mu.Unlock()

	g.record("AddInfoExclude:" + pattern)

	return g.infoExcludeErr
}

// compile-time assertion that the fake satisfies the consumer interface.
var _ GitOps = (*fakeGit)(nil)

// planLLM is a scripted llm.LLM for the orchestrator phase tests. It returns
// the queued responses in order (each as a single no-tool-call assistant turn
// so harness.Run treats it as done) and captures the task string and request
// model of every call so tests can assert on prompt contents and the model the
// harness was configured with. All mutable state is mutex-guarded (mirroring
// fakeGit) so future goroutine fan-out can't trip the race detector.
type planLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	tasks     []string
	models    []string
	i         int

	// providers/reasonings capture each request's raw routing objects
	// (index-aligned with models) so propagation tests can assert children
	// inherit the parent routing.
	providers  []json.RawMessage
	reasonings []json.RawMessage
}

func (p *planLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return p.next(req), nil
}

func (p *planLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return p.next(req), nil
}

func (p *planLLM) next(req llm.Request) llm.Response {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.models = append(p.models, req.Model)
	p.providers = append(p.providers, req.Provider)
	p.reasonings = append(p.reasonings, req.Reasoning)

	// Capture the last user message — the phase task prompt.
	for j := len(req.Messages) - 1; j >= 0; j-- {
		if req.Messages[j].Role == "user" {
			p.tasks = append(p.tasks, req.Messages[j].Content)

			break
		}
	}

	if p.i >= len(p.responses) {
		return llm.Response{FinishReason: "stop"}
	}

	r := p.responses[p.i]
	p.i++

	return r
}

// stopResp wraps final assistant text as a no-tool-call (done) turn.
func stopResp(content string, cost float64) llm.Response {
	return llm.Response{Content: content, FinishReason: "stop", Usage: llm.Usage{Cost: cost}}
}

// finishResp is a coder/document turn that ends the run by calling the finish
// tool with the given commit message (replaces the old stopResp COMMIT: prose).
func finishResp(commitMessage string, cost float64) llm.Response {
	args, _ := json.Marshal(map[string]string{"commit_message": commitMessage})

	return llm.Response{
		ToolCalls: []llm.ToolCall{{
			ID:       "finish-1",
			Type:     "function",
			Function: llm.FunctionCall{Name: "finish", Arguments: string(args)},
		}},
		FinishReason: "tool_calls",
		Usage:        llm.Usage{Cost: cost},
	}
}

// testWriteTools is the write registry for orchestrator tests that drive a coder
// or document run: it includes the finish tool so the real harness recognizes
// the finishResp call as terminal.
func testWriteTools() *tools.Registry {
	return tools.NewRegistry(tools.NewReadTool("."), NewFinishTool())
}

func planTestCatalog() llm.Catalog {
	return llm.Catalog{
		{ID: "payload/model", ContextLength: 131072, SupportedParameters: []string{"tools"}},
		{ID: "default/model", ContextLength: 131072, SupportedParameters: []string{"tools"}},
		{ID: "pinned/model", ContextLength: 131072, SupportedParameters: []string{"tools"}},
	}
}

func planTestRegistry() *registry.Registry {
	return registry.NewRegistry(nil, "default/model", planTestCatalog())
}

// isolateVerify pins a run's verify plan to a resolved skip and marks the
// model-proposal tier spent, so ensureVerify is a cheap no-op in tests that do
// not exercise the gate: a re-resolve finds nothing and never fires a proposal
// model call. Tests that DO exercise the gate set o.verify to a real plan.
func isolateVerify(o *run) {
	o.verify = &verifyPlan{Source: verifySourceNone}
	o.proposeAttempted = true
}

// writeFile writes name under dir with the given content, failing the test on
// any I/O error. Used by detectVerifyCommand tests to seed marker files.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func planTestDeps(ops *fakeOps, client llm.LLM) Deps {
	return Deps{
		Ops:       ops,
		Client:    client,
		Emit:      events.NewEmitter(nil, nil),
		Registry:  planTestRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Cfg: Config{
			Project:      "proj",
			CardID:       "CARD-1",
			PayloadModel: "payload/model",
			DefaultModel: "default/model",
			MaxTurns:     20, // > wrapUpTurns so the nudge never fires on 1-turn plan-content fakes
		},
	}
}

// fakeInbox is a scripted harness.Inbox for the HITL orchestrator tests. It
// returns queued messages in order; once they are exhausted it either blocks
// until ctx is done (block=true, simulating end_session) or returns
// ErrInboxClosed (block=false, simulating a promote / pre-closed inbox).
type fakeInbox struct {
	mu    sync.Mutex
	msgs  []harness.UserMessage
	block bool
	i     int
}

func (f *fakeInbox) Drain() []harness.UserMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := f.msgs[f.i:]
	f.i = len(f.msgs)

	return out
}

func (f *fakeInbox) Wait(ctx context.Context) (harness.UserMessage, error) {
	f.mu.Lock()
	if f.i < len(f.msgs) {
		m := f.msgs[f.i]
		f.i++
		f.mu.Unlock()

		return m, nil
	}

	block := f.block
	f.mu.Unlock()

	if block {
		<-ctx.Done()

		return harness.UserMessage{}, ctx.Err()
	}

	return harness.UserMessage{}, harness.ErrInboxClosed
}

var _ harness.Inbox = (*fakeInbox)(nil)
