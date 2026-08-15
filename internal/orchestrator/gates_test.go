package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGates is a scripted PRGates. The zero value reports no checks, no Copilot
// review and no request-in-flight, so a test that only cares about "was the gate
// entered at all" needs no scripting. Every method records its name and the PR
// URL it received (fakeGit's convention), so tests can assert both the call and
// the URL that reached it. Checks pops the successive scripted results and
// returns the last one once the queue is exhausted.
type fakeGates struct {
	mu sync.Mutex

	checks     [][]CheckResult
	checksErr  error
	headSHA    string
	requested  bool
	requestErr error
	review     *CopilotReview
	logs       string

	calls []string
	i     int
}

func (f *fakeGates) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call)
}

// recorded returns a copy of the call log.
func (f *fakeGates) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.calls))
	copy(out, f.calls)

	return out
}

func (f *fakeGates) Checks(_ context.Context, prURL string) ([]CheckResult, error) {
	f.record("Checks:" + prURL)

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.checks) == 0 {
		return nil, f.checksErr
	}

	idx := min(f.i, len(f.checks)-1)
	f.i++

	return f.checks[idx], f.checksErr
}

func (f *fakeGates) HeadSHA(_ context.Context, prURL string) (string, error) {
	f.record("HeadSHA:" + prURL)

	return f.headSHA, nil
}

func (f *fakeGates) CopilotRequested(_ context.Context, prURL string) (bool, error) {
	f.record("CopilotRequested:" + prURL)

	return f.requested, nil
}

func (f *fakeGates) RequestCopilotReview(_ context.Context, prURL string) error {
	f.record("RequestCopilotReview:" + prURL)

	return f.requestErr
}

func (f *fakeGates) CopilotReview(_ context.Context, prURL string) (*CopilotReview, error) {
	f.record("CopilotReview:" + prURL)

	return f.review, nil
}

func (f *fakeGates) FailureLogs(_ context.Context, prURL string, _ []CheckResult) (string, error) {
	f.record("FailureLogs:" + prURL)

	return f.logs, nil
}

// compile-time assertion that the fake satisfies the consumer interface.
var _ PRGates = (*fakeGates)(nil)

// gatesTestRun builds a run wired for the pr_gates phase: the integrate deps
// plus the scripted gates seam.
func gatesTestRun(ops *fakeOps, gates PRGates, tc cmclient.TaskContext) *run {
	d := integrateTestDeps(ops, &fakeGit{}, &fakePR{}, &planLLM{})
	d.PRGates = gates

	return newIntegrateRun(d, tc, 0)
}

// TestPRGates_PassThroughWhenNoFlags: a PR-opening card with no gate flags is a
// pure pass-through - it transitions to done without touching the gh seam.
func TestPRGates_PassThroughWhenNoFlags(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{}

	o := gatesTestRun(ops, gates, cmclient.TaskContext{Title: "No gates", CreatePR: true})
	o.prURL = "https://example.test/pr/1" // recorded by integrate

	require.NoError(t, runPRGates(context.Background(), o))

	calls := ops.recorded()
	assert.GreaterOrEqual(t, indexOfCall(calls, "TransitionCard:done"), 0,
		"pr_gates owns the done transition; calls=%v", calls)
	assert.Empty(t, gates.recorded(), "no gate flags means the gh seam is never touched")
}

// TestPRGates_PassThroughWhenCreatePRDisabled: gate flags without a PR to gate
// on are ignored by design - the card still completes rather than parking.
func TestPRGates_PassThroughWhenCreatePRDisabled(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{}

	o := gatesTestRun(ops, gates, cmclient.TaskContext{Title: "No PR", CreatePR: false, AwaitCI: true})

	require.NoError(t, runPRGates(context.Background(), o))

	calls := ops.recorded()
	assert.GreaterOrEqual(t, indexOfCall(calls, "TransitionCard:done"), 0,
		"a gate flag without a PR must not block completion; calls=%v", calls)
	assert.Empty(t, gates.recorded(), "no PR means nothing to poll")
}

// TestPRGates_FailClosedWhenPRCreationFailed: a gated card that intended a PR
// but has no URL (creation failed) parks instead of completing - there is
// nothing to gate on, so silently finishing would ship past the gate.
func TestPRGates_FailClosedWhenPRCreationFailed(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{}

	o := gatesTestRun(ops, gates, cmclient.TaskContext{Title: "Gated", CreatePR: true, AwaitCI: true})
	require.Empty(t, o.prURL)
	require.Empty(t, o.tc.PRUrl)

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked, "a gated card with no PR must park")

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "TransitionCard:done"),
		"a parked card must NOT reach done; calls=%v", calls)

	require.NotEmpty(t, ops.bodyUpdates, "the park reason is recorded on the card")
	assert.Contains(t, ops.lastBody(), "## PR Gates")
	assert.Contains(t, ops.lastBody(), "parked:")
	assert.Empty(t, gates.recorded(), "no URL means nothing to poll")
}

// TestPRGates_ResumeReadsPRUrlFromTaskContext: a fresh container resuming at
// pr_gates has no in-memory prURL, so the gate must read the PR recorded by the
// earlier run's report_push - never re-create the PR.
func TestPRGates_ResumeReadsPRUrlFromTaskContext(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{{{Name: "build", Bucket: "pass"}}}}

	tc := cmclient.TaskContext{
		Title:    "Resume",
		Phase:    "pr_gates",
		CreatePR: true,
		AwaitCI:  true,
		PRUrl:    "https://example.test/pr/9",
	}

	o := gatesTestRun(ops, gates, tc)
	require.Empty(t, o.prURL, "a fresh container has no in-memory PR URL")

	require.NoError(t, runPRGates(context.Background(), o))

	calls := ops.recorded()
	assert.GreaterOrEqual(t, indexOfCall(calls, "TransitionCard:done"), 0,
		"a green gate completes the card; calls=%v", calls)
	assert.Equal(t, []string{"Checks:https://example.test/pr/9"}, gates.recorded(),
		"the resumed PR URL is what the CI gate polls")
}

// TestGatesStateRoundTrips: the round counters survive a park/re-trigger because
// they live in the "## PR Gates" card section, not in memory.
func TestGatesStateRoundTrips(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{}
	o := gatesTestRun(ops, &fakeGates{}, cmclient.TaskContext{Description: "Original body."})

	o.recordGates(ctx, gatesState{
		CopilotRounds: 1,
		CIRounds:      2,
		Status:        "waiting for CI",
		Detail:        "- build: failing",
	})

	assert.Contains(t, o.body, "## PR Gates")
	assert.Contains(t, o.body, "Original body.", "the section is upserted, not a body replacement")
	assert.Contains(t, o.body, "- build: failing", "the detail lines are carried")

	st := o.loadGatesState()
	assert.Equal(t, 1, st.CopilotRounds)
	assert.Equal(t, 2, st.CIRounds)

	// A later record replaces the section rather than appending a second one.
	o.recordGates(ctx, gatesState{CopilotRounds: 2, CIRounds: 3, Status: "passed"})
	assert.Equal(t, 1, strings.Count(o.body, "## PR Gates"), "one section, upserted; body=%q", o.body)

	st = o.loadGatesState()
	assert.Equal(t, 2, st.CopilotRounds)
	assert.Equal(t, 3, st.CIRounds)

	// A resumed run whose card never reached the gates starts from zero.
	fresh := gatesTestRun(ops, &fakeGates{}, cmclient.TaskContext{Description: "no gate section here"})
	assert.Equal(t, gatesState{}, fresh.loadGatesState())
}

// TestGatesKnobDefaults: a Deps built directly (tests, standalone runs) leaves
// the knobs zero, so each effective-knob helper falls back to its default.
func TestGatesKnobDefaults(t *testing.T) {
	zero := &run{}
	assert.Equal(t, 30*time.Second, zero.gatesPoll())
	assert.Equal(t, 45*time.Minute, zero.ciWait())
	assert.Equal(t, 10*time.Minute, zero.copilotWait())

	set := &run{d: Deps{Cfg: Config{
		GatesPollInterval:       5 * time.Second,
		GatesCIWaitTimeout:      time.Minute,
		GatesCopilotWaitTimeout: 2 * time.Minute,
	}}}
	assert.Equal(t, 5*time.Second, set.gatesPoll())
	assert.Equal(t, time.Minute, set.ciWait())
	assert.Equal(t, 2*time.Minute, set.copilotWait())
}

// TestGateDeadlineClampsToContainerDeadline: a gate never waits past the
// container's own deadline, and an unset (zero) deadline leaves it unbounded.
func TestGateDeadlineClampsToContainerDeadline(t *testing.T) {
	unbounded := &run{}
	assert.WithinDuration(t, time.Now().Add(time.Hour), unbounded.gateDeadline(time.Hour), time.Minute,
		"no container deadline: the gate's own wait bounds it")

	container := time.Now().Add(5 * time.Minute)
	clamped := &run{d: Deps{Cfg: Config{Deadline: container}}}
	assert.Equal(t, container, clamped.gateDeadline(time.Hour),
		"an earlier container deadline wins over the gate's wait")

	roomy := &run{d: Deps{Cfg: Config{Deadline: time.Now().Add(2 * time.Hour)}}}
	assert.WithinDuration(t, time.Now().Add(time.Hour), roomy.gateDeadline(time.Hour), time.Minute,
		"a later container deadline leaves the gate's wait intact")
}
