package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
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

	checks    [][]CheckResult
	checksErr error
	headSHA   string

	// Copilot scripting: requested is the reviewer-present answer, flipped to
	// true by a successful RequestCopilotReview unless requestSilentlyNoOps
	// reproduces the API failure mode where the request succeeds but no reviewer
	// is ever added. reviews pops the successive scripted reviews (nil until the
	// queue is seeded, and the last one sticks once it is exhausted).
	requested            bool
	requestErr           error
	requestSilentlyNoOps bool
	reviews              []*CopilotReview

	logs string

	// FindPRURL scripting: the recovery probe's result for a fail-closed gate.
	findPRURL    string
	findPRURLErr error

	calls []string
	i     int
	r     int
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

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.requested, nil
}

func (f *fakeGates) RequestCopilotReview(_ context.Context, prURL string) error {
	f.record("RequestCopilotReview:" + prURL)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.requestErr != nil {
		return f.requestErr
	}

	if !f.requestSilentlyNoOps {
		f.requested = true
	}

	return nil
}

func (f *fakeGates) CopilotReview(_ context.Context, prURL string) (*CopilotReview, error) {
	f.record("CopilotReview:" + prURL)

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.reviews) == 0 {
		return nil, nil
	}

	idx := min(f.r, len(f.reviews)-1)
	f.r++

	return f.reviews[idx], nil
}

func (f *fakeGates) FailureLogs(_ context.Context, prURL string, _ []CheckResult) (string, error) {
	f.record("FailureLogs:" + prURL)

	return f.logs, nil
}

func (f *fakeGates) FindPRURL(_ context.Context) (string, error) {
	f.record("FindPRURL")

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.findPRURL, f.findPRURLErr
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
	assert.Empty(t, ops.bodyUpdates, "an ungated card's body is never touched")
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
	assert.Equal(t, []string{"FindPRURL"}, gates.recorded(),
		"the recovery probe runs before parking; no other gh seam call is reachable with no PR")
}

// TestPRGates_RecoversBranchPRWhenCreateFailed: a gated card whose recorded PR
// creation failed but whose branch already has an open PR (an earlier run's
// report_push never landed, or `gh pr create` failed with "a pull request
// already exists") recovers instead of parking: the found URL flows into the
// gates, is card-logged, and is re-reported through the same ReportPush call
// integrate makes - so a later park/resume reads it back without re-probing.
func TestPRGates_RecoversBranchPRWhenCreateFailed(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		findPRURL: "https://example.test/pr/9",
		checks:    [][]CheckResult{{{Name: "build", Bucket: "pass"}}},
	}

	o := gatesTestRun(ops, gates, cmclient.TaskContext{Title: "Gated", CreatePR: true, AwaitCI: true})
	require.Empty(t, o.prURL)
	require.Empty(t, o.tc.PRUrl)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Contains(t, calls, "FindPRURL")
	assert.Contains(t, calls, "Checks:https://example.test/pr/9",
		"the recovered URL flows into the gate; calls=%v", calls)

	opsCalls := ops.recorded()
	assert.GreaterOrEqual(t, indexOfCall(opsCalls, "TransitionCard:done"), 0,
		"a recovered PR gates normally through to done; calls=%v", opsCalls)
	assert.Contains(t, ops.reportPushURLs, "https://example.test/pr/9",
		"the recovery is made durable via ReportPush")
	assert.True(t, ops.loggedContains("recovered"), "a card log line records the recovery")
}

// TestPRGates_StillParksWhenNoBranchPRExists: the recovery probe runs, but the
// branch genuinely has no PR - the gate parks exactly as before, with the same
// recovery note.
func TestPRGates_StillParksWhenNoBranchPRExists(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{findPRURL: ""}

	o := gatesTestRun(ops, gates, cmclient.TaskContext{Title: "Gated", CreatePR: true, AwaitCI: true})
	require.Empty(t, o.prURL)
	require.Empty(t, o.tc.PRUrl)

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked, "no recovered PR must still park")

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "TransitionCard:done"),
		"a parked card must NOT reach done; calls=%v", calls)
	assert.Contains(t, gates.recorded(), "FindPRURL", "the probe must run before parking")
	assert.Contains(t, ops.lastBody(), "disable the PR-gate flags", "the park note is unchanged")
}

// TestPRGates_ProbeErrorFallsBackToPark: the recovery probe itself fails (an
// auth failure, say) - the probe failure never crashes the phase, it just
// degrades to the same park as no PR found.
func TestPRGates_ProbeErrorFallsBackToPark(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{findPRURLErr: errors.New("gh pr view: HTTP 401: Bad credentials")}

	o := gatesTestRun(ops, gates, cmclient.TaskContext{Title: "Gated", CreatePR: true, AwaitCI: true})

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked, "a probe failure must still park, not crash the phase")

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "TransitionCard:done"),
		"a parked card must NOT reach done; calls=%v", calls)
	assert.Contains(t, gates.recorded(), "FindPRURL")
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

// TestGatesState_SatisfiedMarkerRoundTrips: a satisfied Copilot gate persists as
// a literal marker line alongside the round counters, and an unsatisfied one
// writes no such line at all - a resumed run must be able to tell "never
// addressed" apart from "addressed, do not ask again".
func TestGatesState_SatisfiedMarkerRoundTrips(t *testing.T) {
	ctx := context.Background()
	ops := &fakeOps{}
	o := gatesTestRun(ops, &fakeGates{}, cmclient.TaskContext{Description: "Original body."})

	o.recordGates(ctx, gatesState{
		CopilotRounds:    1,
		CIRounds:         2,
		CopilotSatisfied: true,
		Status:           "passed",
	})

	assert.Contains(t, o.body, "- Copilot gate: satisfied")

	st := o.loadGatesState()
	assert.True(t, st.CopilotSatisfied)
	assert.Equal(t, 1, st.CopilotRounds)
	assert.Equal(t, 2, st.CIRounds)

	unsatisfied := gatesTestRun(&fakeOps{}, &fakeGates{}, cmclient.TaskContext{Description: "Original body."})
	unsatisfied.recordGates(ctx, gatesState{CopilotRounds: 1, CIRounds: 1, Status: "waiting for CI"})

	assert.NotContains(t, unsatisfied.body, "Copilot gate:",
		"an unsatisfied gate must write no marker line; body=%q", unsatisfied.body)
	assert.False(t, unsatisfied.loadGatesState().CopilotSatisfied)
}

// TestGatesKnobDefaults: a Deps built directly (tests, standalone runs) leaves
// the knobs zero, so each effective-knob helper falls back to its default.
func TestGatesKnobDefaults(t *testing.T) {
	zero := &run{}
	assert.Equal(t, 60*time.Second, zero.gatesPoll())
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

// TestGatePoller_EmitsEveryPollButShowsOnlyChanges pins the split the gate
// progress depends on: every poll emits, because the serve-side idle watchdog
// reads the worker's output as proof the container is alive, but only a status
// that MOVED is marked for the transcript. A gate can sit on the same counts
// for many minutes, and showing each of those polls buries the ones that
// mattered.
func TestGatePoller_EmitsEveryPollButShowsOnlyChanges(t *testing.T) {
	shrinkGateProgressHeartbeat(t, time.Hour)

	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)
	p := &gatePoller{gate: "ci"}

	p.poll(emit, "CI checks: 0 passed, 7 pending, 0 failed", nil)
	p.poll(emit, "CI checks: 0 passed, 7 pending, 0 failed", nil)
	p.poll(emit, "CI checks: 0 passed, 7 pending, 0 failed", nil)
	p.poll(emit, "CI checks: 3 passed, 4 pending, 0 failed", nil)

	assert.Equal(t, 4, strings.Count(strings.TrimSpace(transcript.String()), "\n")+1,
		"every poll emits, so the idle watchdog keeps seeing output")
	assert.Equal(t, []string{
		"CI checks: 0 passed, 7 pending, 0 failed",
		"CI checks: 3 passed, 4 pending, 0 failed",
	}, gateProgressStatuses(t, &transcript), "only the polls that moved are shown")
}

// TestGatePoller_HeartbeatShowsAnUnchangedStatusAgain: a gate that waits past
// the heartbeat window shows its status again, so a long quiet wait reads as
// alive rather than hung.
func TestGatePoller_HeartbeatShowsAnUnchangedStatusAgain(t *testing.T) {
	shrinkGateProgressHeartbeat(t, time.Nanosecond)

	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)
	p := &gatePoller{gate: "ci"}

	p.poll(emit, "CI checks: 0 passed, 7 pending, 0 failed", nil)
	p.poll(emit, "CI checks: 0 passed, 7 pending, 0 failed", nil)

	assert.Len(t, gateProgressStatuses(t, &transcript), 2,
		"past the heartbeat window an unchanged status is shown again")
}

// TestCopilotPollStatus: a review that is not on the current head reads
// differently from no review at all - the former is a review of code a fix
// round already superseded.
func TestCopilotPollStatus(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Copilot review: received", copilotPollStatus(true, true))
	assert.Equal(t, "Copilot review: waiting - the review on file is for an older commit",
		copilotPollStatus(true, false))
	assert.Equal(t, "Copilot review: waiting", copilotPollStatus(false, false))
}

// TestCIGate_ShowsOnlyThePollsThatMoved is the gate-level half of
// TestGatePoller_EmitsEveryPollButShowsOnlyChanges: the CI gate's own polls go
// through the poller, so a run that waits on the same counts contributes one
// transcript row, not one per poll.
func TestCIGate_ShowsOnlyThePollsThatMoved(t *testing.T) {
	shrinkGateProgressHeartbeat(t, time.Hour)

	var transcript bytes.Buffer

	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{
		{{Name: "build", Bucket: "pending"}},
		{{Name: "build", Bucket: "pending"}},
		{{Name: "build", Bucket: "pending"}},
		{{Name: "build", Bucket: "pass"}},
	}}

	o := prGateRun(ops, gates, &fakeGit{}, &planLLM{}, ciGateContext("Quiet wait", "body"), 0)
	o.d.Emit = events.NewEmitter(nil, &transcript)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, []string{
		"CI checks: 0 passed, 1 pending, 0 failed",
		"CI checks: 1 passed, 0 pending, 0 failed",
	}, gateProgressStatuses(t, &transcript))
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

// gatePRURL is the open PR every gate test polls.
const gatePRURL = "https://example.test/pr/1"

// prGateRun builds a run wired for one of the PR gates: the scripted gh seam plus
// a millisecond poll interval, so the waiting loops run at test speed.
func prGateRun(ops *fakeOps, gates PRGates, git *fakeGit, client llm.LLM, tc cmclient.TaskContext, maxCost float64) *run {
	d := integrateTestDeps(ops, git, &fakePR{}, client)
	d.PRGates = gates
	d.Cfg.GatesPollInterval = time.Millisecond

	return newIntegrateRun(d, tc, maxCost)
}

// ciGateContext is the task context of a CI-gated card whose PR is already open.
func ciGateContext(title, body string) cmclient.TaskContext {
	return cmclient.TaskContext{
		Title:       title,
		Description: body,
		Phase:       "pr_gates",
		CreatePR:    true,
		AwaitCI:     true,
		PRUrl:       gatePRURL,
	}
}

// gateProgressStatuses decodes the statuses of every gate_progress event the
// emitter wrote, keeping only the ones the log bridge would SHOW (repeat=false).
func gateProgressStatuses(t *testing.T, transcript *bytes.Buffer) []string {
	t.Helper()

	var shown []string

	for line := range strings.SplitSeq(strings.TrimSpace(transcript.String()), "\n") {
		if line == "" {
			continue
		}

		var ev struct {
			Kind string `json:"kind"`
			Data struct {
				Status string `json:"status"`
				Repeat bool   `json:"repeat"`
			} `json:"data"`
		}

		require.NoError(t, json.Unmarshal([]byte(line), &ev))

		if ev.Kind == gateProgressKind && !ev.Data.Repeat {
			shown = append(shown, ev.Data.Status)
		}
	}

	return shown
}

// shrinkGateProgressHeartbeat shortens the gate heartbeat for one test.
func shrinkGateProgressHeartbeat(t *testing.T, d time.Duration) {
	t.Helper()

	prev := gateProgressHeartbeat
	gateProgressHeartbeat = d

	t.Cleanup(func() { gateProgressHeartbeat = prev })
}

// shrinkNoChecksGrace shortens the no-CI grace window for one test, so the
// "this repo has no CI" conclusion is reached in milliseconds.
func shrinkNoChecksGrace(t *testing.T, d time.Duration) {
	t.Helper()

	prev := gatesNoChecksGrace
	gatesNoChecksGrace = d

	t.Cleanup(func() { gatesNoChecksGrace = prev })
}

// shrinkFixRoundReserve shortens the wait a fix round must have left for one
// test, so a millisecond-scale gate still funds its rounds.
func shrinkFixRoundReserve(t *testing.T, d time.Duration) {
	t.Helper()

	prev := gatesFixRoundReserve
	gatesFixRoundReserve = d

	t.Cleanup(func() { gatesFixRoundReserve = prev })
}

// failingCheck is a red check with the run link the failure-log fetch parses.
func failingCheck() CheckResult {
	return CheckResult{
		Name:        "build",
		Bucket:      "fail",
		Link:        "https://github.test/acme/repo/actions/runs/42/job/7",
		Description: "build failed",
	}
}

// promptOfCall joins every message of one scripted call: a coder run appends its
// wrap-up nudge as the last user message, so the seed prompt a gate fed it is not
// the one planLLM captures in tasks.
func promptOfCall(c *planLLM, call int) string {
	var b strings.Builder

	for _, m := range c.messagesOf(call) {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}

	return b.String()
}

// modelCallCount is how many LLM calls the scripted client saw.
func modelCallCount(c *planLLM) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.models)
}

// TestCIGate_GreenImmediately: an all-green first poll passes the gate outright -
// no fix round, no model spend, and the card completes.
func TestCIGate_GreenImmediately(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{{
		{Name: "build", Bucket: "pass"},
		{Name: "codeql", Bucket: "skipping"},
	}}}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, ciGateContext("Green", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := ops.recorded()
	assert.GreaterOrEqual(t, indexOfCall(calls, "TransitionCard:done"), 0,
		"a green gate completes the card; calls=%v", calls)
	assert.Contains(t, ops.lastBody(), "- Status: passed")
	assert.Equal(t, []string{"Checks:" + gatePRURL}, gates.recorded(),
		"green on the first poll needs exactly one Checks call")
	assert.Zero(t, modelCallCount(client), "a green gate spends nothing")
}

// TestCIGate_NoChecksGraceThenPass: a PR that never grows a check is a repo
// without CI - the gate waits out the grace window, says so on the card, and
// passes rather than blocking the card forever.
func TestCIGate_NoChecksGraceThenPass(t *testing.T) {
	shrinkNoChecksGrace(t, 5*time.Millisecond)

	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{{}, {}, {}}}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, ciGateContext("No CI", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.True(t, ops.loggedContains("no CI checks"),
		"the card records why the gate passed without a check; logs=%v", ops.recorded())
	assert.GreaterOrEqual(t, len(gates.recorded()), 2,
		"the gate keeps polling through the grace window; calls=%v", gates.recorded())
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
	assert.Zero(t, modelCallCount(client), "no checks means no fix round")
}

// TestCIGate_RedFixGreen: a failing check funds one fix round - failure logs in,
// coder out, fixup pushed - and the next poll sees the new head green.
func TestCIGate_RedFixGreen(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		checks: [][]CheckResult{
			{failingCheck()},
			{{Name: "build", Bucket: "pass"}},
		},
		logs: "build failed: undefined: helper",
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{stopResp("coder: fixed the build", 0.05)}}

	o := prGateRun(ops, gates, git, client, ciGateContext("Red then green", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Contains(t, gates.recorded(), "FailureLogs:"+gatePRURL,
		"the fix round is fed the real failure digest; calls=%v", gates.recorded())
	assert.Equal(t, 1, modelCallCount(client), "exactly one fix round ran")
	assert.Contains(t, git.recorded(), "Push:cm/card-1", "the fixup is pushed; calls=%v", git.recorded())

	body := ops.lastBody()
	assert.Contains(t, body, "- CI rounds used: 1/3")
	assert.Contains(t, body, "- Status: passed")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCIGate_ThreeRoundsThenPark: CI that stays red outlives its fix budget -
// three rounds, then the card parks in review with the failing checks named.
func TestCIGate_ThreeRoundsThenPark(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{
		{failingCheck()}, {failingCheck()}, {failingCheck()}, {failingCheck()},
	}}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		stopResp("coder: attempt 1", 0.01),
		stopResp("coder: attempt 2", 0.01),
		stopResp("coder: attempt 3", 0.01),
	}}

	o := prGateRun(ops, gates, git, client, ciGateContext("Stays red", "body"), 0)

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "3 fix rounds")
	assert.Equal(t, 3, modelCallCount(client), "the rounds cap is 3, not 4")

	body := ops.lastBody()
	assert.Contains(t, body, "- CI rounds used: 3/3")
	assert.Contains(t, body, "- Status: parked:")
	assert.Contains(t, body, "- build: https://github.test/acme/repo/actions/runs/42/job/7",
		"the human needs the failing check and its link; body=%q", body)
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"),
		"a parked card must NOT reach done")
}

// TestCIGate_PendingPastDeadlineParks: checks that never settle park at the wait
// deadline instead of holding the container until it is killed.
func TestCIGate_PendingPastDeadlineParks(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{{{Name: "build", Bucket: "pending"}}}}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, ciGateContext("Pending forever", "body"), 0)
	o.d.Cfg.GatesCIWaitTimeout = 10 * time.Millisecond

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "pending")
	assert.Zero(t, modelCallCount(client), "pending is not a failure - nothing to fix")
	assert.Contains(t, ops.lastBody(), "- CI rounds used: 0/3")
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"))
}

// TestCIGate_BudgetParkDuringFix: a budget that runs out mid-gate parks the card
// in review rather than failing the run - the work is already pushed, so there is
// nothing to WIP-push and a human can pick the PR up as it stands.
func TestCIGate_BudgetParkDuringFix(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{{failingCheck()}}}
	client := &planLLM{}

	tc := ciGateContext("Broke", "body")
	tc.ReportedCostUSD = 5.0

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, tc, 1.0)

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)

	var budget *BudgetExceededError

	require.NotErrorAs(t, err, &budget,
		"gate-phase budget exhaustion parks in review; it never surfaces as the budget error")

	assert.Contains(t, parked.Reason, "budget")
	assert.Contains(t, ops.lastBody(), "budget")
	assert.Zero(t, modelCallCount(client), "the budget is checked before the fix model runs")
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"))
}

// TestCIGate_ResumeCountsPersistedRounds: the rounds cap spans park/re-trigger -
// a card whose body already records 2 used rounds gets exactly one more.
func TestCIGate_ResumeCountsPersistedRounds(t *testing.T) {
	body := "Task body.\n\n## PR Gates\n\n" +
		"- Copilot rounds used: 0/3\n- CI rounds used: 2/3\n- Status: parked: CI still red\n"

	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{{failingCheck()}, {failingCheck()}}}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{stopResp("coder: last attempt", 0.01)}}

	o := prGateRun(ops, gates, git, client, ciGateContext("Resumed", body), 0)
	require.Equal(t, 2, o.loadGatesState().CIRounds, "the seeded body carries two used rounds")

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Equal(t, 1, modelCallCount(client), "two rounds were already spent; only one is left")
	assert.Contains(t, ops.lastBody(), "- CI rounds used: 3/3")
}

// TestCIGate_EmptyPollAfterAFixIsNeverNoCI: once any check has been seen, an
// empty poll means the new head's run has not registered yet - never "this repo
// has no CI". Without that memory a fix round's immediate re-poll could pass a PR
// that was red one poll earlier. The grace window is zeroed here so only the
// checks-were-seen memory can hold the gate.
func TestCIGate_EmptyPollAfterAFixIsNeverNoCI(t *testing.T) {
	shrinkNoChecksGrace(t, 0)

	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{
		{failingCheck()},                  // red: funds one fix round
		{},                                // the pushed head's run has not registered
		{},                                // still not registered
		{{Name: "build", Bucket: "pass"}}, // it registers, and it is green
	}}
	client := &planLLM{responses: []llm.Response{stopResp("coder: fixed it", 0.01)}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, ciGateContext("Re-poll", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.False(t, ops.loggedContains("no CI checks"),
		"a PR whose checks were already seen can never be read as a repo without CI")
	assert.True(t, ops.loggedContains("CI green"), "the gate waited for the new head's run")
	assert.Equal(t, 1, modelCallCount(client))
}

// TestCIGate_ResumeNeverReopensNoCI: the checks-were-seen memory has to survive
// a park/re-trigger too. A resumed gate whose persisted section records fix
// rounds ran CI in an earlier container, so an empty first poll is the new head's
// run registering - never "this repo has no CI".
func TestCIGate_ResumeNeverReopensNoCI(t *testing.T) {
	shrinkNoChecksGrace(t, 0) // only the persisted-rounds memory can hold the gate

	body := "Task body.\n\n## PR Gates\n\n" +
		"- Copilot rounds used: 0/3\n- CI rounds used: 1/3\n- Status: parked: CI still red\n"

	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{
		{},                                // the resumed head's run has not registered yet
		{{Name: "build", Bucket: "pass"}}, // it registers, and it is green
	}}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, ciGateContext("Resumed", body), 0)
	require.Equal(t, 1, o.loadGatesState().CIRounds, "the seeded body carries a used round")

	require.NoError(t, runPRGates(context.Background(), o))

	assert.False(t, ops.loggedContains("no CI checks"),
		"a gate that already ran CI rounds must never re-open the no-CI conclusion; logs=%v", ops.recorded())
	assert.True(t, ops.loggedContains("CI green"), "the gate waited for the resumed head's run")
	assert.Zero(t, modelCallCount(client), "green needs no fix round")
}

// TestCIGate_WaitsAPollIntervalAfterAFixRound: the gate lets the pushed head
// register with CI before re-polling, rather than re-reading the superseded run.
func TestCIGate_WaitsAPollIntervalAfterAFixRound(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{
		{failingCheck()},
		{{Name: "build", Bucket: "pass"}},
	}}
	client := &planLLM{responses: []llm.Response{stopResp("coder: fixed it", 0.01)}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, ciGateContext("Settle", "body"), 0)
	o.d.Cfg.GatesPollInterval = 25 * time.Millisecond

	start := time.Now()

	require.NoError(t, runPRGates(context.Background(), o))

	assert.GreaterOrEqual(t, time.Since(start), 25*time.Millisecond,
		"the re-poll after a fix waits one poll interval")
}

// TestCIGate_DeadlineParkNamesTheOutage: a gate that spent its wait on failing
// polls must not park blaming checks it never managed to read.
func TestCIGate_DeadlineParkNamesTheOutage(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checksErr: errors.New("gh: API rate limit exceeded")}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, ciGateContext("gh down", "body"), 0)
	o.d.Cfg.GatesCIWaitTimeout = 10 * time.Millisecond

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "could not be read")
	assert.NotContains(t, parked.Reason, "pending",
		"the checks were never read, so the card must not claim they were pending")
	assert.Zero(t, modelCallCount(client))
}

// TestCIGate_PendingParkDropsStaleFailureDetail: the park note describes the poll
// that parked, not a red round the fixes already addressed.
func TestCIGate_PendingParkDropsStaleFailureDetail(t *testing.T) {
	shrinkFixRoundReserve(t, 0) // a millisecond-scale gate still funds its round

	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{
		{failingCheck()},                     // red: one fix round, Detail records the check
		{{Name: "build", Bucket: "pending"}}, // the new run never settles
	}}
	client := &planLLM{responses: []llm.Response{stopResp("coder: fixed it", 0.01)}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, ciGateContext("Never settles", "body"), 0)
	o.d.Cfg.GatesCIWaitTimeout = 30 * time.Millisecond

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)

	body := ops.lastBody()
	assert.NotContains(t, body, "https://github.test/acme/repo/actions/runs/42/job/7",
		"the fixed round's failing check must not be listed as current; body=%q", body)
	assert.Contains(t, body, "at the deadline", "the park note describes the final poll; body=%q", body)
}

// TestCIGate_RedWithNoWaitLeftSkipsTheFixRound: a fix round is a multi-minute
// coder run plus a fresh CI cycle. With no wait left the gate parks on the red
// checks instead of spending a round it cannot see through.
func TestCIGate_RedWithNoWaitLeftSkipsTheFixRound(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{{failingCheck()}}}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, ciGateContext("No time", "body"), 0)
	o.d.Cfg.GatesCIWaitTimeout = 10 * time.Millisecond

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "fix round")
	assert.Zero(t, modelCallCount(client), "no doomed coder run")
	assert.Contains(t, ops.lastBody(), "- CI rounds used: 0/3", "an unspent round is not counted")
	assert.Contains(t, ops.lastBody(), "- build: https://github.test/acme/repo/actions/runs/42/job/7",
		"the human still gets the failing check and its link")
}

// TestClassifyChecks covers the bucket mapping, including the two arms no gate
// test exercises: a canceled check is a failure, and a bucket gh has not taught us
// yet counts as pending rather than as a pass.
func TestClassifyChecks(t *testing.T) {
	cases := []struct {
		name        string
		checks      []CheckResult
		wantFailed  []string
		wantPending int
		wantPassing int
	}{
		{name: "empty"},
		{
			name:        "pass and skipping both count as passing",
			checks:      []CheckResult{{Name: "build", Bucket: "pass"}, {Name: "codeql", Bucket: "skipping"}},
			wantPassing: 2,
		},
		{
			name:       "fail and cancel both count as failed",
			checks:     []CheckResult{{Name: "build", Bucket: "fail"}, {Name: "e2e", Bucket: "cancel"}},
			wantFailed: []string{"build", "e2e"},
		},
		{
			name:        "an unknown bucket is pending, never a pass",
			checks:      []CheckResult{{Name: "build", Bucket: "pending"}, {Name: "new", Bucket: "quarantined"}},
			wantPending: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := classifyChecks(tc.checks)

			names := make([]string, 0, len(b.failed))
			for _, c := range b.failed {
				names = append(names, c.Name)
			}

			assert.Equal(t, tc.wantFailed, nonEmpty(names))
			assert.Equal(t, tc.wantPending, b.pending)
			assert.Equal(t, tc.wantPassing, b.passing)
		})
	}
}

// copilotGateContext is the task context of a Copilot-gated card whose PR is
// already open.
func copilotGateContext(title, body string) cmclient.TaskContext {
	return cmclient.TaskContext{
		Title:              title,
		Description:        body,
		Phase:              "pr_gates",
		CreatePR:           true,
		AwaitCopilotReview: true,
		PRUrl:              gatePRURL,
	}
}

// shrinkCopilotRecheck shortens the pause between requesting a Copilot review
// and re-checking that the reviewer appeared, so the silent-no-op branch is
// reached in milliseconds.
func shrinkCopilotRecheck(t *testing.T, d time.Duration) {
	t.Helper()

	prev := gatesCopilotRecheck
	gatesCopilotRecheck = d

	t.Cleanup(func() { gatesCopilotRecheck = prev })
}

// copilotVerdict scripts one triage response: the strict JSON the gate asks for.
func copilotVerdict(findings ...copilotFinding) llm.Response {
	raw, err := json.Marshal(copilotTriage{Findings: findings})
	if err != nil {
		panic(err)
	}

	return stopResp(string(raw), 0.01)
}

// copilotReviewOnHead is a completed review on the head SHA every Copilot gate
// test pins.
func copilotReviewOnHead(body string, comments ...ReviewComment) *CopilotReview {
	return &CopilotReview{CommitID: copilotHeadSHA, Body: body, Comments: comments}
}

// copilotHeadSHA is the PR head every Copilot gate test scripts, so a review
// carrying this CommitID is a review of the code the gate is holding.
const copilotHeadSHA = "head-sha"

// swallowedErrorComment is a Copilot comment on a real defect; renamingComment
// is the style nit the triage model rejects.
var (
	swallowedErrorComment = ReviewComment{
		Path: "internal/api/handler.go",
		Body: "This error is swallowed - the caller can never see it.",
	}
	renamingComment = ReviewComment{
		Path: "README.md",
		Body: "Consider rewording this sentence.",
	}
)

// TestCopilotGate_AlreadyRequestedWaitsAndPassesOnCleanReview: a review that is
// already requested is never re-requested, and a clean one passes the gate after
// a single triage call.
func TestCopilotGate_AlreadyRequestedWaitsAndPassesOnCleanReview(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{copilotReviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Clean", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "RequestCopilotReview:"+gatePRURL),
		"a review already requested must not be re-requested; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client), "one triage call, no fix round")
	assert.True(t, ops.loggedContains("Copilot review addressed"),
		"the card records why the gate passed; logs=%v", ops.recorded())

	body := ops.lastBody()
	assert.Contains(t, body, "## Copilot Review", "the triage round is recorded on the card; body=%q", body)
	assert.Contains(t, body, "- Copilot rounds used: 0/3", "a clean review spends no fix round")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_CleanPassWritesSatisfiedMarker: the zero-valid-findings pass
// exit is one of the two addressed exits that must persist the satisfied
// marker, so a re-trigger never re-requests a review this run already got and
// judged clean.
func TestCopilotGate_CleanPassWritesSatisfiedMarker(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{copilotReviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Clean", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	gatesSection := extractSection(ops.lastBody(), gatesSectionHeading)
	assert.Contains(t, gatesSection, "- Copilot gate: satisfied", "gatesSection=%q", gatesSection)
}

// cancelingGates wraps a fakeGates and cancels the run's context right after
// its first Checks call - reproducing the routine container-teardown
// cancellation ciGate's poll loop can hit while checks are still pending (see
// sleepGate's doc comment).
type cancelingGates struct {
	*fakeGates

	cancel context.CancelFunc
	once   sync.Once
}

func (g *cancelingGates) Checks(ctx context.Context, prURL string) ([]CheckResult, error) {
	checks, err := g.fakeGates.Checks(ctx, prURL)
	g.once.Do(g.cancel)

	return checks, err
}

// TestCopilotGate_SatisfiedMarkerSurvivesCIGateCancellation: the Copilot gate's
// pass must be durable the instant it happens, not only once the whole
// pr_gates phase finishes - a context cancellation while the CI gate is still
// polling pending checks must not cost the satisfied marker its only write.
func TestCopilotGate_SatisfiedMarkerSurvivesCIGateCancellation(t *testing.T) {
	ops := &fakeOps{}
	base := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{copilotReviewOnHead("LGTM")},
		checks:    [][]CheckResult{{{Name: "build", Bucket: "pending"}}},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	tc := copilotGateContext("Clean then torn down", "body")
	tc.AwaitCI = true

	o := prGateRun(ops, base, &fakeGit{}, client, tc, 0)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	o.d.PRGates = &cancelingGates{fakeGates: base, cancel: cancel}

	err := runPRGates(ctx, o)

	require.ErrorIs(t, err, context.Canceled,
		"a torn-down container's ciGate returns the raw context error, never a park")

	assert.Contains(t, ops.lastBody(), "- Copilot gate: satisfied",
		"the Copilot gate's pass must survive a CI-gate cancellation; body=%q", ops.lastBody())
}

// TestCopilotGate_RequestsWhenAbsent: with no review in flight the gate asks for
// one BEFORE it starts waiting - otherwise it would wait out the timeout on a
// review nobody requested.
func TestCopilotGate_RequestsWhenAbsent(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)

	ops := &fakeOps{}
	gates := &fakeGates{
		headSHA: copilotHeadSHA,
		reviews: []*CopilotReview{copilotReviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Not requested", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	request := indexOfCall(calls, "RequestCopilotReview:"+gatePRURL)
	wait := indexOfCall(calls, "CopilotReview:"+gatePRURL)

	require.GreaterOrEqual(t, request, 0, "the gate requests the missing review; calls=%v", calls)
	require.GreaterOrEqual(t, wait, 0, "the gate then waits for it; calls=%v", calls)
	assert.Less(t, request, wait, "the request must precede the wait; calls=%v", calls)
}

// TestCopilotGate_RequestFailsSkipsWithNote: Copilot being unavailable is not the
// card's fault - the gate records the VERBATIM gh error and proceeds, and the CI
// gate still runs. That log line is the only diagnostic an operator whose account
// has no Copilot access ever gets.
func TestCopilotGate_RequestFailsSkipsWithNote(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requestErr: errors.New("gh: HTTP 422: Copilot isn't available for this repository"),
		checks:     [][]CheckResult{{{Name: "build", Bucket: "pass"}}},
	}
	client := &planLLM{}

	tc := copilotGateContext("No Copilot here", "body")
	tc.AwaitCI = true

	o := prGateRun(ops, gates, &fakeGit{}, client, tc, 0)

	require.NoError(t, runPRGates(context.Background(), o), "an unavailable reviewer never parks the card")

	assert.True(t, ops.loggedContains("gh: HTTP 422: Copilot isn't available for this repository"),
		"the verbatim gh error is the post-ship debugging channel; logs=%v", ops.recorded())
	assert.Contains(t, gates.recorded(), "Checks:"+gatePRURL,
		"a skipped Copilot gate must not skip the CI gate; calls=%v", gates.recorded())
	assert.Zero(t, modelCallCount(client), "nothing to triage")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_SkipPathsDoNotWriteSatisfiedMarker: a request failure is
// Copilot unavailability, not an addressed review - the marker must stay
// unwritten so a re-trigger retries the request instead of skipping straight
// to CI.
func TestCopilotGate_SkipPathsDoNotWriteSatisfiedMarker(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requestErr: errors.New("gh: HTTP 422: Copilot isn't available for this repository"),
		checks:     [][]CheckResult{{{Name: "build", Bucket: "pass"}}},
	}
	client := &planLLM{}

	tc := copilotGateContext("No Copilot here", "body")
	tc.AwaitCI = true

	o := prGateRun(ops, gates, &fakeGit{}, client, tc, 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.NotContains(t, ops.lastBody(), "- Copilot gate: satisfied",
		"an unavailable reviewer must remain retryable; body=%q", ops.lastBody())
}

// TestCopilotGate_ReviewerNeverAppearsSkipsWithNote: the request API can return
// success without adding the reviewer. The gate re-checks, says so on the card,
// and proceeds instead of waiting out the full timeout on a review that is never
// coming.
func TestCopilotGate_ReviewerNeverAppearsSkipsWithNote(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)

	ops := &fakeOps{}
	gates := &fakeGates{requestSilentlyNoOps: true}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Silent no-op", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.True(t, ops.loggedContains("not added as a reviewer"),
		"the card names the silent failure; logs=%v", ops.recorded())
	assert.Equal(t, -1, indexOfCall(gates.recorded(), "CopilotReview:"+gatePRURL),
		"no wait loop runs when the reviewer never appeared; calls=%v", gates.recorded())
	assert.Zero(t, modelCallCount(client))
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_TimeoutProceeds: a review that never lands proceeds at the wait
// deadline. This gate never parks on a missing review - only on findings it could
// not get fixed.
func TestCopilotGate_TimeoutProceeds(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{requested: true, headSHA: copilotHeadSHA}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Never arrives", "body"), 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 10 * time.Millisecond

	require.NoError(t, runPRGates(context.Background(), o))

	assert.True(t, ops.loggedContains("did not arrive in time"),
		"the card records the timeout; logs=%v", ops.recorded())
	assert.GreaterOrEqual(t, len(gates.recorded()), 2, "the gate polled while it waited")
	assert.Zero(t, modelCallCount(client), "no review means nothing to triage")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_ValidFindingsFixedThenClean: the triage verdict decides which
// comments are real - the valid one funds a fix round and a re-request, the nit
// is recorded and dropped - and the next review comes back clean.
func TestCopilotGate_ValidFindingsFixedThenClean(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			copilotReviewOnHead("2 suggestions", swallowedErrorComment, renamingComment),
			copilotReviewOnHead("LGTM"),
		},
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(
			copilotFinding{
				File: "internal/api/handler.go", Issue: "the write error is dropped",
				Valid: true, Reason: "the caller cannot tell the write failed",
			},
			copilotFinding{
				File: "README.md", Issue: "wording could be clearer",
				Valid: false, Reason: "style preference, not a defect",
			},
		),
		stopResp("coder: returned the write error", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, git, client, copilotGateContext("Two comments", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	require.Equal(t, 3, modelCallCount(client), "triage, one fix, and the re-triage of the clean review")
	assert.Contains(t, git.recorded(), "Push:cm/card-1", "the fix is pushed; git=%v", git.recorded())

	fixPrompt := promptOfCall(client, 1)

	assert.Contains(t, fixPrompt, "internal/api/handler.go", "the fix run is fed the valid finding")
	assert.NotContains(t, fixPrompt, "wording could be clearer",
		"an invalid finding must never reach the coder")
	assert.Contains(t, gates.recorded(), "RequestCopilotReview:"+gatePRURL,
		"the fixed head is sent back for re-review; calls=%v", gates.recorded())

	body := ops.lastBody()
	assert.Contains(t, body, "## Copilot Review", "round 1 uses the bare heading")
	assert.Contains(t, body, "- VALID internal/api/handler.go: the write error is dropped")
	assert.Contains(t, body, "- INVALID README.md: wording could be clearer")
	assert.Contains(t, body, "## Copilot Review (Round 2)", "later rounds are numbered; body=%q", body)
	assert.Contains(t, body, "- Copilot rounds used: 1/3")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_DedupesRepeatedComments: Copilot re-posts comments it already
// made. A repeat is filtered before triage, so an already-fixed finding never
// buys a second fix round.
func TestCopilotGate_DedupesRepeatedComments(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			copilotReviewOnHead("one suggestion", swallowedErrorComment),
			copilotReviewOnHead("one suggestion", swallowedErrorComment),
		},
	}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "the write error is dropped",
			Valid: true, Reason: "the caller cannot tell the write failed",
		}),
		stopResp("coder: returned the write error", 0.02),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Repeat", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 2, modelCallCount(client),
		"one triage and one fix: the repeated comment is filtered before a second triage")
	assert.True(t, ops.loggedContains("already triaged"),
		"the card says why the second review passed; logs=%v", ops.recorded())
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 1/3", "exactly one round was spent")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)

	gatesSection := extractSection(ops.lastBody(), gatesSectionHeading)
	assert.Contains(t, gatesSection, "Status: passed")
	assert.NotContains(t, gatesSection, "the write error is dropped",
		"round 1's findings must not linger under a passed status; section=%q", gatesSection)
}

// TestCopilotGate_DedupeSurvivesResume: the dedupe keys live on the card, so a
// re-triggered run in a fresh container does not re-triage - or re-fix - a
// comment an earlier run already handled. The seeded body is written by
// recordCopilotRound itself, so the recorded line shape and the key read back out
// of it cannot drift apart. The comment is deliberately multi-line and longer
// than the key: flattening and truncation have to round-trip too.
func TestCopilotGate_DedupeSurvivesResume(t *testing.T) {
	wrapped := ReviewComment{
		Path: "internal/api/handler.go",
		Body: "This error is swallowed - the caller\ncan never see it, and the write is\nreported as a success.",
	}

	seed := &fakeOps{}
	recorder := prGateRun(seed, &fakeGates{}, &fakeGit{}, &planLLM{},
		copilotGateContext("Parked", "Task body."), 0)

	recorder.recordCopilotRound(context.Background(), 1, []ReviewComment{wrapped},
		[]copilotFinding{{
			File: "internal/api/handler.go", Issue: "the write error is dropped",
			Valid: true, Reason: "the caller cannot tell the write failed",
		}}, false)

	body := seed.lastBody()
	require.Contains(t, body, "### Comments triaged", "the parked run recorded its dedupe keys; body=%q", body)

	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{copilotReviewOnHead("one suggestion", wrapped)},
	}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Resumed", body), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Zero(t, modelCallCount(client),
		"a comment triaged before the park is never triaged again")
	assert.True(t, ops.loggedContains("already triaged"),
		"the card says why the resumed gate passed; logs=%v", ops.recorded())
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_SatisfiedMarkerSkipsOnResume: a resumed card whose body
// already carries the satisfied marker skips the Copilot gate entirely - no
// reviewer check, no request, no wait - and goes straight to the CI gate. The
// seeded body is written by recordGates itself, the same write-then-read
// discipline TestCopilotGate_DedupeSurvivesResume uses.
func TestCopilotGate_SatisfiedMarkerSkipsOnResume(t *testing.T) {
	seed := &fakeOps{}
	recorder := prGateRun(seed, &fakeGates{}, &fakeGit{}, &planLLM{},
		copilotGateContext("Earlier run", "Task body."), 0)

	recorder.recordGates(context.Background(), gatesState{CopilotSatisfied: true, Status: "passed"})

	body := seed.lastBody()
	require.Contains(t, body, "- Copilot gate: satisfied",
		"the seeded body records the earlier addressed review; body=%q", body)

	ops := &fakeOps{}
	gates := &fakeGates{
		checks: [][]CheckResult{{{Name: "build", Bucket: "pass"}}},
	}
	client := &planLLM{}

	tc := copilotGateContext("Resumed", body)
	tc.AwaitCI = true

	o := prGateRun(ops, gates, &fakeGit{}, client, tc, 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "CopilotRequested:"+gatePRURL),
		"no reviewer check on a resumed, satisfied gate; calls=%v", calls)
	assert.Equal(t, -1, indexOfCall(calls, "RequestCopilotReview:"+gatePRURL),
		"no re-request on a resumed, satisfied gate; calls=%v", calls)
	assert.Equal(t, -1, indexOfCall(calls, "CopilotReview:"+gatePRURL),
		"no wait on a resumed, satisfied gate; calls=%v", calls)
	assert.Contains(t, calls, "Checks:"+gatePRURL, "the CI gate still runs; calls=%v", calls)
	assert.Zero(t, modelCallCount(client), "nothing to triage on a satisfied gate")
	assert.True(t, ops.loggedContains("addressed in an earlier run"),
		"a card log line mentions the earlier run; logs=%v", ops.recorded())
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_ThreeRoundsThenPark: a reviewer that keeps finding new real
// defects outlives the fix budget - three rounds, then the card parks in review
// with the open findings named.
func TestCopilotGate_ThreeRoundsThenPark(t *testing.T) {
	freshReview := func(n int) *CopilotReview {
		return copilotReviewOnHead(fmt.Sprintf("round %d", n), ReviewComment{
			Path: fmt.Sprintf("internal/pkg/file%d.go", n),
			Body: fmt.Sprintf("Defect number %d: this branch cannot be reached.", n),
		})
	}

	freshVerdict := func(n int) llm.Response {
		return copilotVerdict(copilotFinding{
			File: fmt.Sprintf("internal/pkg/file%d.go", n), Issue: fmt.Sprintf("unreachable branch %d", n),
			Valid: true, Reason: "the guard above already returns",
		})
	}

	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{freshReview(1), freshReview(2), freshReview(3), freshReview(4)},
	}
	client := &planLLM{responses: []llm.Response{
		freshVerdict(1), stopResp("coder: round 1", 0.01),
		freshVerdict(2), stopResp("coder: round 2", 0.01),
		freshVerdict(3), stopResp("coder: round 3", 0.01),
		freshVerdict(4),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Endless", "body"), 0)

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "3 rounds")
	assert.Equal(t, 7, modelCallCount(client), "four triages and three fixes - the cap is 3, not 4")

	body := ops.lastBody()
	assert.Contains(t, body, "- Copilot rounds used: 3/3")
	assert.Contains(t, body, "- Status: parked:")
	assert.Contains(t, body, "internal/pkg/file4.go",
		"the human needs the findings that are still open; body=%q", body)
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"),
		"a parked card must NOT reach done")
}

// TestCopilotGate_BudgetParkDuringTriage: a budget that runs out before the
// triage call parks the card in review rather than failing the run - the work is
// pushed and the PR stands as the human finds it.
func TestCopilotGate_BudgetParkDuringTriage(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{copilotReviewOnHead("one suggestion", swallowedErrorComment)},
	}
	client := &planLLM{}

	tc := copilotGateContext("Broke", "body")
	tc.ReportedCostUSD = 5.0

	o := prGateRun(ops, gates, &fakeGit{}, client, tc, 1.0)

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)

	var budget *BudgetExceededError

	require.NotErrorAs(t, err, &budget,
		"gate-phase budget exhaustion parks in review; it never surfaces as the budget error")

	assert.Contains(t, parked.Reason, "budget")
	assert.Contains(t, parked.Reason, "Copilot", "the park reason names the gate that ran out")
	assert.Zero(t, modelCallCount(client), "the budget is checked before the triage model runs")
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"))
}

// TestCopilotGate_UnreadableVerdictTakesCommentsAtFaceValue: a triage response
// that is not the JSON we asked for must never ship past the review. The gate
// says so verbatim on the card and treats every comment as a finding - the
// conservative reading, since a wasted fix round costs less than a missed defect.
func TestCopilotGate_UnreadableVerdictTakesCommentsAtFaceValue(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			copilotReviewOnHead("one suggestion", swallowedErrorComment),
			copilotReviewOnHead("LGTM"),
		},
	}
	client := &planLLM{responses: []llm.Response{
		stopResp("Looks mostly fine to me, though I did not check the handler.", 0.01),
		stopResp("coder: returned the write error", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Junk verdict", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.True(t, ops.loggedContains("could not be read"),
		"the card records the unreadable verdict; logs=%v", ops.recorded())
	assert.Equal(t, 3, modelCallCount(client), "the unreadable verdict still funds the fix round")
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 1/3")
	assert.Contains(t, ops.lastBody(), "- VALID internal/api/handler.go:",
		"the untriaged comment is recorded as taken at face value; body=%q", ops.lastBody())
}

// TestCopilotGate_UnreadableVerdictWithNoComments: a body-only review (zero
// line comments) paired with an unreadable triage verdict must not be
// recorded as "the reviewer raised nothing to address" - that phrasing
// claims a clean, judged review, but nothing here was actually judged. The
// gate still passes: there is nothing to fix regardless of why.
func TestCopilotGate_UnreadableVerdictWithNoComments(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{copilotReviewOnHead("LGTM overall")},
	}
	client := &planLLM{responses: []llm.Response{
		stopResp("garbage, not JSON", 0.01),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Body-only review", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.True(t, ops.loggedContains("could not be read"),
		"the card records the unreadable verdict; logs=%v", ops.recorded())
	assert.False(t, ops.loggedContains("treating all 0 comment(s) as findings"),
		"there is nothing to treat as a finding when there are no comments; logs=%v", ops.recorded())
	assert.Contains(t, ops.lastBody(),
		"The triage verdict could not be read and the review left no line comments to take at face value; nothing was judged.",
		"body=%q", ops.lastBody())
	assert.NotContains(t, ops.lastBody(), "The reviewer raised nothing to address.",
		"body=%q", ops.lastBody())
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0,
		"a body-only review with an unreadable verdict has nothing to fix, so the gate passes")
}

// TestCopilotGate_EmptyVerdictWithCommentsTakesThemAtFaceValue: a verdict that
// parses but judges nothing is not a verdict of "no defects" - the prompt asks
// for one entry per comment, invalid ones included. The comments stand rather
// than shipping past the gate unjudged.
func TestCopilotGate_EmptyVerdictWithCommentsTakesThemAtFaceValue(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			copilotReviewOnHead("one suggestion", swallowedErrorComment),
			copilotReviewOnHead("LGTM"),
		},
	}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(), // parses cleanly, judges nothing
		stopResp("coder: returned the write error", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Silent verdict", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.True(t, ops.loggedContains("judged none of the 1 comment"),
		"the card says the comment was never judged; logs=%v", ops.recorded())
	assert.Equal(t, 3, modelCallCount(client), "the unjudged comment still funds the fix round")
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 1/3")
}

// nonEmpty normalizes an empty slice to nil so table cases can leave the want
// field unset.
func nonEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}

	return s
}
