package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
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
	// queue is seeded, and the last one sticks once it is exhausted); a nil
	// entry is a read that found no review, which is how a test scripts the
	// gate's head probe coming back empty. reRequestErr, counted against
	// requests, scripts a failure on the SECOND and later RequestCopilotReview
	// calls only - the re-request after a fix round - independent of
	// requestErr, which always applies to the first.
	requested            bool
	requestErr           error
	reRequestErr         error
	requestSilentlyNoOps bool
	reviews              []*CopilotReview
	requests             int

	// requestedAfterChecks scripts a pending request appearing late (a ruleset,
	// or a listing that lagged the request): once CopilotRequested has been
	// called this many times it answers true regardless of requested. Zero
	// disables the knob.
	requestedAfterChecks int
	requestedCalls       int

	// confirmAfterRequests scripts a request that is silently dropped N times
	// and then takes: every RequestCopilotReview call after the Nth is
	// confirmed by its response body and flips requested. Zero disables the
	// knob. It takes precedence over requestSilentlyNoOps so the first request
	// can stay a silent no-op while a later retry confirms.
	confirmAfterRequests int

	// requestedAfterRequests scripts a listing that lags the request: once
	// RequestCopilotReview has been called this many times, CopilotRequested
	// answers true regardless of requested. Zero disables the knob, and it
	// wins over requestedAfterChecks so the first request's pre-check and
	// re-check read false while a retry's follow-up check reads true.
	requestedAfterRequests int

	// holdReviewsUntilChecks reproduces a review that lands DURING the CI wait:
	// while it is set and Checks has never been polled, CopilotReview reads as
	// "no review yet" without consuming the queue, so the Copilot wait times out
	// and the first scripted review only becomes visible once the CI gate ran.
	holdReviewsUntilChecks bool
	checksPolled           bool

	logs string

	// logsDelay holds every FailureLogs call, so a test can make a fix round
	// outlast the gate deadline.
	logsDelay time.Duration

	// Thread write-back scripting: threads is what ReviewThreads returns;
	// replyErr/resolveErr script write failures. ReviewThreads returns the
	// backing slice DELIBERATELY: the writer's in-place IsResolved/ReplyCount
	// updates persist into later fetches, modeling GitHub's own persistence -
	// tests depend on that, so never "fix" this to return copies.
	threads    []ReviewThread
	threadsErr error
	replyErr   error
	resolveErr error

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

	f.checksPolled = true

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

	f.requestedCalls++

	if f.requestedAfterRequests > 0 && f.requests >= f.requestedAfterRequests {
		return true, nil
	}

	if f.requestedAfterChecks > 0 && f.requestedCalls >= f.requestedAfterChecks {
		return true, nil
	}

	return f.requested, nil
}

func (f *fakeGates) RequestCopilotReview(_ context.Context, prURL string) (bool, error) {
	f.record("RequestCopilotReview:" + prURL)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests++

	if f.requestErr != nil {
		return false, f.requestErr
	}

	if f.requests > 1 && f.reRequestErr != nil {
		return false, f.reRequestErr
	}

	if f.requestSilentlyNoOps && (f.confirmAfterRequests == 0 || f.requests <= f.confirmAfterRequests) {
		return false, nil
	}

	if f.confirmAfterRequests > 0 && f.requests > f.confirmAfterRequests {
		f.requested = true

		return true, nil
	}

	f.requested = true

	return true, nil
}

func (f *fakeGates) ReviewThreads(_ context.Context, prURL string) ([]ReviewThread, error) {
	f.record("ReviewThreads:" + prURL)

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.threads, f.threadsErr
}

func (f *fakeGates) ReplyToReviewComment(_ context.Context, prURL string, commentID int64, body string) error {
	f.record(fmt.Sprintf("Reply:%s:%d:%s", prURL, commentID, body))

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.replyErr
}

func (f *fakeGates) ResolveReviewThread(_ context.Context, threadID string) error {
	f.record("Resolve:" + threadID)

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.resolveErr
}

func (f *fakeGates) CopilotReview(_ context.Context, prURL string) (*CopilotReview, error) {
	f.record("CopilotReview:" + prURL)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.holdReviewsUntilChecks && !f.checksPolled {
		return nil, nil
	}

	if len(f.reviews) == 0 {
		return nil, nil
	}

	idx := min(f.r, len(f.reviews)-1)
	f.r++

	return f.reviews[idx], nil
}

func (f *fakeGates) FailureLogs(_ context.Context, prURL string, _ []CheckResult) (string, error) {
	f.record("FailureLogs:" + prURL)

	time.Sleep(f.logsDelay)

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
	assert.Equal(t, 20*time.Minute, zero.copilotWait())

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

// TestGatePoller_ReservedKeysWinOverFields: a caller's fields must never
// overwrite the poller's reserved keys (gate, status, repeat). Copy fields
// first, set reserved keys afterward - matching gateNote's convention.
func TestGatePoller_ReservedKeysWinOverFields(t *testing.T) {
	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)
	p := &gatePoller{gate: "ci"}

	p.poll(emit, "CI checks: 1 passed, 0 pending, 0 failed", map[string]any{
		"gate":   "spoofed-ci",
		"status": "spoofed-status",
		"repeat": true,
		"reason": "kept",
	})

	var ev struct {
		Kind string `json:"kind"`
		Data struct {
			Gate   string `json:"gate"`
			Status string `json:"status"`
			Repeat bool   `json:"repeat"`
			Reason string `json:"reason"`
		} `json:"data"`
	}

	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(transcript.String())), &ev))

	assert.Equal(t, gateProgressKind, ev.Kind)
	assert.Equal(t, "ci", ev.Data.Gate, "reserved key 'gate' must win over caller fields")
	assert.Equal(t, "CI checks: 1 passed, 0 pending, 0 failed", ev.Data.Status,
		"reserved key 'status' must win over caller fields")
	assert.False(t, ev.Data.Repeat, "reserved key 'repeat' must win over caller fields (no heartbeat history)")
	assert.Equal(t, "kept", ev.Data.Reason, "non-reserved fields still ride along")
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

// TestPRGates_DecisionsAreEmittedAsGateProgressEvents: gate verdicts used to
// reach only the card activity log, so a worker run log said nothing about
// which gates ran or how the Copilot gate exited. Every decision now rides the
// gate_progress channel (repeat=false, so the serve transcript shows it) and
// the durable run log carries it. Decisions carry decision=true, which
// gateProgressStatuses filters out - the poll-only assertions elsewhere stay
// exact - so this test reads them through gateDecisionStatuses.
func TestPRGates_DecisionsAreEmittedAsGateProgressEvents(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{reviewOnHead("LGTM")},
		checks:    [][]CheckResult{{{Name: "ci", Bucket: "pass"}}},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	tc := copilotGateContext("Events", "body")
	tc.AwaitCI = true

	var transcript bytes.Buffer

	o := prGateRun(ops, gates, &fakeGit{}, client, tc, 0)
	o.d.Emit = events.NewEmitter(nil, &transcript)

	require.NoError(t, runPRGates(context.Background(), o))

	shown := strings.Join(gateDecisionStatuses(t, &transcript), "\n")
	assert.Contains(t, shown, "pr_gates: entering - await_ci=true await_copilot_review=true")
	assert.Contains(t, shown, "pr_url="+gatePRURL)
	assert.Contains(t, shown, "pr_gates: Copilot review addressed")
	assert.Contains(t, shown, "pr_gates: CI green")
	assert.Contains(t, shown, "pr_gates: passed")
}

// TestGateNoteReservedKeysWinOverFields: every gate_progress consumer keys off
// gate/status/repeat/decision, so a caller's context fields ride alongside them
// and can never redefine them.
func TestGateNoteReservedKeysWinOverFields(t *testing.T) {
	var transcript bytes.Buffer

	o := &run{d: Deps{
		Ops:  &fakeOps{},
		Emit: events.NewEmitter(nil, &transcript),
		Cfg:  Config{CardID: "CARD-1"},
	}}

	o.gateNote(context.Background(), "ci", "pr_gates: CI green", map[string]any{
		"gate":     "spoofed",
		"status":   "spoofed",
		"repeat":   true,
		"decision": false,
		"reason":   "kept",
	})

	var ev struct {
		Kind string `json:"kind"`
		Data struct {
			Gate     string `json:"gate"`
			Status   string `json:"status"`
			Repeat   bool   `json:"repeat"`
			Decision bool   `json:"decision"`
			Reason   string `json:"reason"`
		} `json:"data"`
	}

	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(transcript.String())), &ev))

	assert.Equal(t, gateProgressKind, ev.Kind)
	assert.Equal(t, "ci", ev.Data.Gate)
	assert.Equal(t, "pr_gates: CI green", ev.Data.Status)
	assert.False(t, ev.Data.Repeat, "a decision is never a repeat")
	assert.True(t, ev.Data.Decision, "a decision is never demoted to a poll")
	assert.Equal(t, "kept", ev.Data.Reason, "non-reserved fields still ride along")
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

// TestCIGate_PermanentPollErrorParksImmediately: a poll failure the seam marks
// permanent parks the gate on that very poll with the verbatim gh text, instead
// of looping to the wait deadline and parking blind.
func TestCIGate_PermanentPollErrorParksImmediately(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checksErr: &PermanentPollError{Err: "gh run list: exit status 1: unknown flag: --commit"}}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, ciGateContext("Old gh", "body"), 0)
	o.d.Cfg.GatesCIWaitTimeout = 45 * time.Minute // the park must not wait for this

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "could not be read")
	assert.Contains(t, ops.lastBody(), "unknown flag: --commit", "the card detail names the gh failure verbatim")
	assert.Equal(t, []string{"Checks:" + gatePRURL}, gates.recorded(), "one poll, no retry loop")
	assert.Zero(t, modelCallCount(client))
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"))
}

// TestCIGate_FixRoundReserveFollowsTheObservedCycle: once the gate has watched
// one CI cycle settle, a fix round needs that cycle plus a coder allowance left
// on the clock - the fixed 5-minute floor alone would start a round a 9-minute
// CI can never finish inside the wait.
func TestCIGate_FixRoundReserveFollowsTheObservedCycle(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{{failingCheck()}}}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, ciGateContext("Slow CI", "body"), 0)
	o.d.Cfg.GatesCIWaitTimeout = 10 * time.Minute // above the 5-minute floor, below 9m + allowance
	o.ciObservedSettle = 9 * time.Minute

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "fix round")
	assert.Zero(t, modelCallCount(client), "no coder run whose CI cycle cannot fit in the wait")
	assert.Contains(t, ops.lastBody(), "- CI rounds used: 0/3")
}

// TestCIGate_ObservesTheSettleCycleAfterTheFirstPoll: the cycle is measured
// from gate start to the first settled poll AFTER the first read - the first
// poll reflects state from before the gate started, not a cycle it waited out.
func TestCIGate_ObservesTheSettleCycleAfterTheFirstPoll(t *testing.T) {
	cases := []struct {
		name     string
		checks   [][]CheckResult
		observed bool
	}{
		{
			name: "a run that settles while the gate watches is measured",
			checks: [][]CheckResult{
				{failingCheck(), {Name: "slow", Bucket: "pending"}}, // red, still running
				{failingCheck(), {Name: "slow", Bucket: "pass"}},    // settled: one cycle observed
				{{Name: "build", Bucket: "pass"}, {Name: "slow", Bucket: "pass"}},
			},
			observed: true,
		},
		{
			name:     "checks already settled on the first poll are not a cycle the gate waited through",
			checks:   [][]CheckResult{{{Name: "build", Bucket: "pass"}}},
			observed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakeOps{}
			gates := &fakeGates{checks: tc.checks}
			client := &planLLM{responses: []llm.Response{stopResp("coder: fixed", 0.01)}}

			o := prGateRun(ops, gates, &fakeGit{committed: true}, client, ciGateContext("Cycle", "body"), 0)

			require.NoError(t, runPRGates(context.Background(), o))
			assert.Equal(t, tc.observed, o.ciObservedSettle > 0, "observed=%v", o.ciObservedSettle)
		})
	}
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

// gateProgressStatuses decodes the statuses of every gate_progress POLL the
// emitter wrote, keeping only the ones the log bridge would SHOW (repeat=false).
// Decisions (decision=true) ride the same event kind but are not polls, so they
// are filtered out here and read through gateDecisionStatuses instead.
func gateProgressStatuses(t *testing.T, transcript *bytes.Buffer) []string {
	t.Helper()

	return gateProgressLines(t, transcript, false)
}

// gateDecisionStatuses decodes the statuses of every gate_progress DECISION the
// emitter wrote - the gateNote lines, not the poll heartbeat.
func gateDecisionStatuses(t *testing.T, transcript *bytes.Buffer) []string {
	t.Helper()

	return gateProgressLines(t, transcript, true)
}

func gateProgressLines(t *testing.T, transcript *bytes.Buffer, decisions bool) []string {
	t.Helper()

	var shown []string

	for line := range strings.SplitSeq(strings.TrimSpace(transcript.String()), "\n") {
		if line == "" {
			continue
		}

		var ev struct {
			Kind string `json:"kind"`
			Data struct {
				Status   string `json:"status"`
				Repeat   bool   `json:"repeat"`
				Decision bool   `json:"decision"`
			} `json:"data"`
		}

		require.NoError(t, json.Unmarshal([]byte(line), &ev))

		if ev.Kind == gateProgressKind && !ev.Data.Repeat && ev.Data.Decision == decisions {
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

// TestCIGate_WaitsForTheRunToSettleBeforeFixing: a check that fails while its
// siblings are still running must NOT open a fix round yet. gh reads the
// failure log from the run's log archive, which GitHub publishes only once
// every job in the run has finished, so a round started here gets a digest with
// no failure output in it - and a sibling that fails a minute later would cost
// a second round to fix what this one could have covered in the same pass.
func TestCIGate_WaitsForTheRunToSettleBeforeFixing(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		checks: [][]CheckResult{
			{failingCheck(), {Name: "slow", Bucket: "pending"}},
			{failingCheck(), {Name: "slow", Bucket: "pending"}},
			{failingCheck(), {Name: "slow", Bucket: "pass"}},
			{{Name: "build", Bucket: "pass"}, {Name: "slow", Bucket: "pass"}},
		},
		logs: "build failed: undefined: helper",
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{stopResp("coder: fixed the build", 0.05)}}

	o := prGateRun(ops, gates, git, client, ciGateContext("Red while pending", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 1, modelCallCount(client),
		"exactly one fix round ran, and only once the run had settled")
	assert.Contains(t, gates.recorded(), "FailureLogs:"+gatePRURL)
}

// TestCIGate_RunThatNeverSettlesParksInsteadOfFixing: the settle wait has no
// escape hatch on purpose. A run that never finishes is a run whose log never
// becomes readable, so the fix round the gate would buy by giving up early is
// exactly the blind one the wait exists to prevent. It parks for a human
// instead, and spends no rounds getting there.
func TestCIGate_RunThatNeverSettlesParksInsteadOfFixing(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{
		{failingCheck(), {Name: "hung", Bucket: "pending"}},
	}}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, ciGateContext("Hung sibling", "body"), 0)
	o.d.Cfg.GatesCIWaitTimeout = 10 * time.Millisecond

	var parked *GatesParkedError

	require.ErrorAs(t, runPRGates(context.Background(), o), &parked)
	assert.Equal(t, "CI still red at the wait deadline", parked.Reason)
	assert.Equal(t, 0, modelCallCount(client), "no round is spent on a digest that cannot be read")

	body := ops.lastBody()
	assert.Contains(t, body, "- CI rounds used: 0/3")
}

// TestCIGate_FixRoundCarriesTheCIFailureNote: the fix coder is otherwise told
// only the run's verify command, so a lint or format failure sends it hunting a
// defect in logic that is not there. Its prompt must carry both the digest and
// the note that CI runs a broader suite than verify.
func TestCIGate_FixRoundCarriesTheCIFailureNote(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		checks: [][]CheckResult{
			{failingCheck()},
			{{Name: "build", Bucket: "pass"}},
		},
		logs: "tools_test.go:166:1: File is not properly formatted (goimports)",
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{stopResp("coder: fixed the formatting", 0.05)}}

	o := prGateRun(ops, gates, git, client, ciGateContext("Lint red", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))
	require.Equal(t, 1, modelCallCount(client), "exactly one fix round ran")

	prompt := promptOfCall(client, 0)
	assert.Contains(t, prompt, "tools_test.go:166:1", "the digest reaches the fix coder")
	assert.Contains(t, prompt, "broader check suite",
		"the fix coder is told CI runs more than the verify command")
	assert.Contains(t, prompt, "the log could not be fetched",
		"the fix coder is told what to do with a digest that names no failure")
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

	// Passing on either empty poll would have ended the gate after two
	// Checks calls; waiting for the new head's run takes all four.
	assert.GreaterOrEqual(t, countCalls(gates.recorded(), "Checks:"+gatePRURL), 4,
		"the gate waited through both empty polls; calls=%v", gates.recorded())
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

	// Reading the empty first poll as "no CI" would have passed the gate on
	// one Checks call; the resumed memory makes it wait for the second.
	assert.GreaterOrEqual(t, countCalls(gates.recorded(), "Checks:"+gatePRURL), 2,
		"the gate waited for the resumed head's run; calls=%v", gates.recorded())
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
	assert.Contains(t, ops.lastBody(), "API rate limit exceeded",
		"the last poll error is the only diagnostic the human gets; body=%q", ops.lastBody())
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

// TestCIGate_FixRoundPastDeadlineRepollsBeforeParking: a fix round that outlasts
// the wait deadline must not park on the buckets it was started from - the
// failures the fix just addressed. The gate re-polls the new head first and
// parks on what that poll says: the new run pending, or not registered yet.
func TestCIGate_FixRoundPastDeadlineRepollsBeforeParking(t *testing.T) {
	cases := []struct {
		name       string
		postFix    []CheckResult
		wantReason string
	}{
		{"new run pending", []CheckResult{{Name: "build", Bucket: "pending"}}, "CI still pending at the wait deadline"},
		{"new run not registered yet", nil, "no checks had registered on the current head at the wait deadline"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkFixRoundReserve(t, 0) // a millisecond-scale gate still funds its round

			ops := &fakeOps{}
			gates := &fakeGates{
				checks:    [][]CheckResult{{failingCheck()}, tc.postFix}, // red: one fix round; then the new head
				logsDelay: 40 * time.Millisecond,                         // the round outlasts the 20ms deadline
			}
			client := &planLLM{responses: []llm.Response{stopResp("coder: fixed it", 0.01)}}

			o := prGateRun(ops, gates, &fakeGit{committed: true}, client, ciGateContext("Slow fix", "body"), 0)
			o.d.Cfg.GatesCIWaitTimeout = 20 * time.Millisecond

			err := runPRGates(context.Background(), o)

			var parked *GatesParkedError

			require.ErrorAs(t, err, &parked)
			assert.Equal(t, 1, modelCallCount(client), "the fix round ran")
			assert.Equal(t, tc.wantReason, parked.Reason,
				"the verdict reads the post-fix poll, not the red one the fix was started from")
			assert.NotContains(t, ops.lastBody(), "https://github.test/acme/repo/actions/runs/42/job/7",
				"the fixed round's failing check must not be listed as current; body=%q", ops.lastBody())
		})
	}
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

// shrinkCopilotGrace shortens the grace window an unconfirmed review request
// gets, so the skip branch is reached in milliseconds.
func shrinkCopilotGrace(t *testing.T, d time.Duration) {
	t.Helper()

	prev := gatesCopilotGraceWait
	gatesCopilotGraceWait = d

	t.Cleanup(func() { gatesCopilotGraceWait = prev })
}

// shrinkCopilotGraceRetries re-times the in-grace review-request retries, so
// they fire (or are proven dropped) in milliseconds instead of minutes. The
// poller helper: the first offset above the shrunk window fires nothing, the
// second fires once.
func shrinkCopilotGraceRetries(t *testing.T, first, second time.Duration) {
	t.Helper()

	prev := gatesCopilotGraceRetries
	gatesCopilotGraceRetries = []time.Duration{first, second}

	t.Cleanup(func() { gatesCopilotGraceRetries = prev })
}

// copilotVerdict scripts one triage response: the strict JSON the gate asks for.
func copilotVerdict(findings ...copilotFinding) llm.Response {
	raw, err := json.Marshal(copilotTriage{Findings: findings})
	if err != nil {
		panic(err)
	}

	return stopResp(string(raw), 0.01)
}

// reviewOnHead is a completed review on the head SHA every Copilot gate
// test pins.
func reviewOnHead(body string, comments ...ReviewComment) *CopilotReview {
	return &CopilotReview{CommitID: copilotHeadSHA, Body: body, Comments: comments}
}

// copilotHeadSHA is the PR head every Copilot gate test scripts, so a review
// carrying this CommitID is a review of the code the gate is holding.
const copilotHeadSHA = "head-sha"

// swallowedErrorComment is a Copilot comment on a real defect; renamingComment
// is the style nit the triage model rejects.
var (
	swallowedErrorComment = ReviewComment{
		ID:   901,
		Path: "internal/api/handler.go",
		Body: "This error is swallowed - the caller can never see it.",
	}
	renamingComment = ReviewComment{
		ID:   902,
		Path: "README.md",
		Body: "Consider rewording this sentence.",
	}
)

// threadOf builds the review thread rooted at a comment, the shape
// ReviewThreads would report for it.
func threadOf(id string, c ReviewComment, extra ...int64) ReviewThread {
	return ReviewThread{
		ThreadID:   id,
		CommentIDs: append([]int64{c.ID}, extra...),
		ReplyCount: len(extra),
		RootPath:   c.Path,
		RootBody:   c.Body,
	}
}

// TestCopilotGate_ExistingReviewOnHeadSkipsTheRequest: a re-trigger or a slow
// earlier run finds Copilot's review already on the PR head. The gate must
// read it, never re-request (a paid duplicate), and never wait for a review it
// already has.
func TestCopilotGate_ExistingReviewOnHeadSkipsTheRequest(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: false,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{reviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Already reviewed", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "RequestCopilotReview:"+gatePRURL),
		"an existing review on the head must not be re-requested; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client), "the existing review is triaged once")
	assert.Contains(t, ops.lastBody(), "- Copilot gate: satisfied")
}

// TestCopilotGate_AlreadyRequestedWaitsAndPassesOnCleanReview: a review that is
// already requested is never re-requested, and a clean one passes the gate after
// a single triage call.
func TestCopilotGate_AlreadyRequestedWaitsAndPassesOnCleanReview(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		// Empty on the probe, so this test still covers the already-requested
		// wait path rather than the probe shortcut.
		reviews: []*CopilotReview{nil, reviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Clean", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "RequestCopilotReview:"+gatePRURL),
		"a review already requested must not be re-requested; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client), "one triage call, no fix round")

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
		reviews:   []*CopilotReview{reviewOnHead("LGTM")},
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
		reviews:   []*CopilotReview{reviewOnHead("LGTM")},
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
		// The gate probes for a review already on the head before it requests
		// one, so "no review in flight" is a nil first read; the review the
		// gate then waits for is the second.
		reviews: []*CopilotReview{nil, reviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Not requested", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	probe := indexOfCall(calls, "CopilotReview:"+gatePRURL)
	request := indexOfCall(calls, "RequestCopilotReview:"+gatePRURL)

	require.GreaterOrEqual(t, request, 0, "the gate requests the missing review; calls=%v", calls)
	require.GreaterOrEqual(t, probe, 0, "the gate probes the head before requesting; calls=%v", calls)
	assert.Less(t, probe, request, "the probe must precede the request; calls=%v", calls)
	assert.Greater(t, countCalls(calls, "CopilotReview:"+gatePRURL), 1,
		"the gate then waits for the review it requested; calls=%v", calls)
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

	assert.Contains(t, gates.recorded(), "Checks:"+gatePRURL,
		"a skipped Copilot gate must not skip the CI gate; calls=%v", gates.recorded())
	assert.Zero(t, modelCallCount(client), "nothing to triage")
	assert.Equal(t, 1, countCalls(gates.recorded(), "CopilotReview:"+gatePRURL),
		"proven unavailability skips the late probe after CI - only the pre-request probe reads the PR; calls=%v", gates.recorded())
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_RequestFailsStillWaits: a generic request failure (e.g. the
// GraphQL login-resolution error gh hits when the reviewer login cannot be
// resolved) does NOT prove Copilot cannot review - the repo may use automatic
// review assignment, so a review is coming anyway. The gate must still enter the
// wait loop, pick up the repo-automated review, triage it, and pass - and it
// must not record the 'unavailable ... gate skipped' line the old code wrote on
// every request failure.
func TestCopilotGate_RequestFailsStillWaits(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requestErr: errors.New("gh: GraphQL: Could not resolve user with login 'copilot-pull-request-reviewer[bot]'. (requestReviewsByLogin)"),
		headSHA:    copilotHeadSHA,
		// Nothing on the head when the gate probes, so the failing request is
		// the path under test.
		reviews: []*CopilotReview{nil, reviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Auto-review", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Greater(t, countCalls(calls, "CopilotReview:"+gatePRURL), 1,
		"a failed request must not skip the wait - the gate still polls for a repo-automated review; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client), "the arrived review is triaged")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_RequestFailsStillWaits_422FromMalformedRequest: a bare 422
// (e.g. from a malformed requested_reviewers payload) must NOT be treated as
// proven Copilot unavailability, because the message may be a request-format
// error rather than a "Copilot cannot review" response. The gate must still
// enter the wait loop and pick up a repo-automated review.
func TestCopilotGate_RequestFailsStillWaits_422FromMalformedRequest(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		// A 422 error that does not say "Copilot isn't available" - e.g. the
		// REST endpoint's response to a malformed request body.
		requestErr: errors.New("gh: HTTP 422: Unprocessable Entity: reviewers field is invalid"),
		headSHA:    copilotHeadSHA,
		// Nothing on the head when the gate probes, so the failing request is
		// the path under test.
		reviews: []*CopilotReview{nil, reviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Auto-review", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Greater(t, countCalls(calls, "CopilotReview:"+gatePRURL), 1,
		"a bare 422 must not skip the wait - the gate still polls for a repo-automated review; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client), "the arrived review is triaged")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_RequestFailsStillWaitsOutTimeout: with a generic request
// failure and no repo-automated review to pick up, the gate waits out its
// deadline and proceeds via the existing 'did not arrive in time' skip -
// proving a request failure still never parks. The skip writes no satisfied
// marker, so a re-trigger retries the request.
func TestCopilotGate_RequestFailsStillWaitsOutTimeout(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requestErr: errors.New("gh: GraphQL: Could not resolve user with login 'copilot-pull-request-reviewer[bot]'. (requestReviewsByLogin)"),
	}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Auto-review", "body"), 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 10 * time.Millisecond

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Greater(t, countCalls(gates.recorded(), "CopilotReview:"+gatePRURL), 1,
		"the gate polled for a review while it waited out the deadline; calls=%v", gates.recorded())
	assert.Contains(t, ops.lastBody(), "did not arrive in time",
		"the wait-out skip line, not the unavailability one; body=%q", ops.lastBody())
	assert.NotContains(t, ops.lastBody(), "- Copilot gate: satisfied",
		"a skipped gate stays retryable; body=%q", ops.lastBody())
	assert.Zero(t, modelCallCount(client), "no review means nothing to triage")
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

// TestCopilotGate_UnconfirmedRequestGraceCatchesReview: a request that
// succeeds without the reviewer showing up is not proof no review is coming -
// a ruleset may deliver one on its own - so the gate waits a grace window,
// and a review that lands inside it is triaged like any other.
func TestCopilotGate_UnconfirmedRequestGraceCatchesReview(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)

	ops := &fakeOps{}
	gates := &fakeGates{
		requestSilentlyNoOps: true,
		headSHA:              copilotHeadSHA,
		// Nothing on the head when the gate probes: the request path this test
		// is about only runs when the probe comes back empty.
		reviews: []*CopilotReview{nil, reviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Silent no-op", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Greater(t, countCalls(calls, "CopilotReview:"+gatePRURL), 1,
		"the gate must enter the wait loop; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client), "the arrived review is triaged")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_UnconfirmedRequestSkipsAfterGrace: when the review request
// is accepted but the reviewer never appears - and no review arrives in the
// grace window - the gate skips with a note saying the request did not take,
// instead of burning the full copilot_wait and blaming a slow review.
func TestCopilotGate_UnconfirmedRequestSkipsAfterGrace(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)
	shrinkCopilotGrace(t, 5*time.Millisecond)

	ops := &fakeOps{}
	gates := &fakeGates{
		requestSilentlyNoOps: true,
		headSHA:              copilotHeadSHA,
		// No review, ever: the zero reviews queue answers every poll with nil.
	}
	client := &planLLM{}

	// Far longer than the grace window, short enough that a regression to the
	// full wait fails this test quickly rather than hanging it.
	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Dropped request", "body"), 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 300 * time.Millisecond

	require.NoError(t, runPRGates(context.Background(), o))

	// The skip reason is persisted on the gates section, and production reads
	// it back (isCopilotUnavailable). Burning the full wait instead would
	// record the timeout skip here, so this is what tells the two apart.
	assert.Contains(t, ops.lastBody(), "the reviewer was never added and no review arrived",
		"the grace skip is what the card records, not a wait that ran out; body=%q", ops.lastBody())

	calls := gates.recorded()
	assert.Greater(t, countCalls(calls, "CopilotReview:"+gatePRURL), 1,
		"the gate kept reading the PR: proven unavailability stops after the single pre-request probe; calls=%v", calls)
	assert.Equal(t, 0, modelCallCount(client), "no review, nothing to triage")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0,
		"the gate passes; Copilot being unreachable never parks the card")
}

// TestCopilotGate_GraceUpgradesWhenRequestListedLate: a pending request that
// appears during the grace window - a ruleset adding the reviewer on its own,
// or a listing that lagged - proves a review is coming, so the gate upgrades
// from the grace window to the full wait and triages the review it delivers.
func TestCopilotGate_GraceUpgradesWhenRequestListedLate(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)
	shrinkCopilotGrace(t, 5*time.Millisecond)

	// The review sits behind more empty polls than the grace window can make
	// before its deadline, so only the upgrade to the full wait reaches it -
	// a grace window left to run out would skip instead.
	reviews := make([]*CopilotReview, 12)
	reviews[len(reviews)-1] = reviewOnHead("LGTM")

	ops := &fakeOps{}
	gates := &fakeGates{
		requestSilentlyNoOps: true,
		headSHA:              copilotHeadSHA,
		// Call 1 is the pre-check, call 2 the post-request re-check - both
		// false, so the gate enters the grace window - and call 3 is the first
		// grace poll, where the pending request finally shows.
		requestedAfterChecks: 3,
		reviews:              reviews,
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Late listing", "body"), 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 2 * time.Second

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 1, modelCallCount(client),
		"the grace window did not skip: the full wait it upgraded to delivered a review, and it was triaged")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_GraceRetryLandsUpgradesToFullWait: a request dropped at
// first is retried inside the grace window. When a retry lands - confirmed by
// the response body, or listed on the follow-up re-check - the gate upgrades
// to the full wait exactly as it does for a request the API listed late.
func TestCopilotGate_GraceRetryLandsUpgradesToFullWait(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)
	shrinkCopilotGrace(t, 40*time.Millisecond)
	shrinkCopilotGraceRetries(t, 8*time.Millisecond, 16*time.Millisecond)

	// The review sits behind more empty polls than the grace window can make
	// before its deadline, so only the upgrade to the full wait reaches it -
	// a grace window left to run out would skip instead.
	reviews := make([]*CopilotReview, 20)
	reviews[len(reviews)-1] = reviewOnHead("LGTM")

	ops := &fakeOps{}
	gates := &fakeGates{
		// The first request is a silent no-op; the SECOND - the in-grace
		// retry - is body-confirmed.
		requestSilentlyNoOps: true,
		confirmAfterRequests: 1,
		headSHA:              copilotHeadSHA,
		reviews:              reviews,
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Retry lands", "body"), 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 2 * time.Second

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, 2, countCalls(calls, "RequestCopilotReview:"+gatePRURL),
		"the dropped request is re-issued exactly once inside the grace window; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client),
		"the grace window did not skip: the full wait it upgraded to delivered a review, and it was triaged")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_GraceRetryListedLateUpgradesToFullWait: a retry whose
// confirmation does not come through the response body still counts as landed
// when the follow-up CopilotRequested re-check lists the reviewer - the same
// two-step confirmation the first request gets.
func TestCopilotGate_GraceRetryListedLateUpgradesToFullWait(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)
	shrinkCopilotGrace(t, 40*time.Millisecond)
	shrinkCopilotGraceRetries(t, 8*time.Millisecond, 16*time.Millisecond)

	// CopilotRequested stays false while only the dropped first request is on
	// the books and starts answering true once the in-grace retry has been
	// issued, so the pending listing shows up on the retry's own re-check.
	// The review sits behind more empty polls than the grace window can make,
	// so only the upgrade to the full wait reaches it - a grace window left
	// to run out would skip instead.
	reviews := make([]*CopilotReview, 20)
	reviews[len(reviews)-1] = reviewOnHead("LGTM")

	ops := &fakeOps{}
	gates := &fakeGates{
		requestSilentlyNoOps: true,
		headSHA:              copilotHeadSHA,
		// The listing lags the request: CopilotRequested reads false while
		// only the first request exists, and true once the in-grace retry is
		// on the books - so the retry's body-silent response gets its
		// confirmation from its follow-up re-check.
		requestedAfterRequests: 2,
		reviews:                reviews,
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Retry listed late", "body"), 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 2 * time.Second

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, 2, countCalls(calls, "RequestCopilotReview:"+gatePRURL),
		"the dropped request is re-issued exactly once inside the grace window; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client),
		"the grace window did not skip: the full wait it upgraded to delivered a review, and it was triaged")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_GraceRetriesDroppedFailOpenQuietly: when every in-grace
// retry is silently dropped too, the gate keeps today's fail-open outcome -
// the same skip note, no extra card notes, no decision events - and the total
// grace duration stays bounded by the grace window.
func TestCopilotGate_GraceRetriesDroppedFailOpenQuietly(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)
	shrinkCopilotGrace(t, 40*time.Millisecond)
	shrinkCopilotGraceRetries(t, 8*time.Millisecond, 16*time.Millisecond)

	var transcript bytes.Buffer

	ops := &fakeOps{}
	gates := &fakeGates{
		requestSilentlyNoOps: true,
		headSHA:              copilotHeadSHA,
	}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Every retry dropped", "body"), 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 300 * time.Millisecond
	o.d.Emit = events.NewEmitter(nil, &transcript)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, 3, countCalls(calls, "RequestCopilotReview:"+gatePRURL),
		"both in-grace retries fire, and nothing re-requests past the deadline; calls=%v", calls)
	assert.Equal(t, 0, modelCallCount(client), "no review, nothing to triage")

	// The fail-open shape: the same four decision events the gate emits
	// without any retry - entering, the unconfirmed note, the skip note,
	// passed - and nothing from the retries themselves.
	assert.Len(t, gateDecisionStatuses(t, &transcript), 4,
		"retries emit no decision events; decisions=%v", gateDecisionStatuses(t, &transcript))

	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0,
		"the gate passes; Copilot being unreachable never parks the card")
}

// TestCopilotGate_GraceRetry422SkipsUnavailable: a retry that proves Copilot
// cannot review the repository surfaces the verbatim 422 reason and takes the
// existing unavailability skip - the same shape as the first request's 422.
func TestCopilotGate_GraceRetry422SkipsUnavailable(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)
	shrinkCopilotGrace(t, 40*time.Millisecond)
	shrinkCopilotGraceRetries(t, 8*time.Millisecond, 16*time.Millisecond)

	ops := &fakeOps{}
	gates := &fakeGates{
		requestSilentlyNoOps: true,
		headSHA:              copilotHeadSHA,
		// The first request is a silent no-op; the SECOND - the in-grace
		// retry - answers the proven unavailability 422.
		reRequestErr: errors.New("HTTP 422: Copilot isn't available for this repository"),
	}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Retry 422", "body"), 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 300 * time.Millisecond

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, 2, countCalls(calls, "RequestCopilotReview:"+gatePRURL),
		"the 422 stops the retry ladder at the first retry; calls=%v", calls)
	assert.Contains(t, ops.lastBody(), "Copilot review unavailable",
		"the proven-unavailability skip line, not a wait-out; body=%q", ops.lastBody())
	assert.Equal(t, 0, modelCallCount(client), "no review, nothing to triage")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0,
		"the gate skips rather than parks; Copilot being unreachable never parks the card")
}

// TestCopilotGate_ConfirmedRequestSkipsRecheck: when the POST's response body
// already lists the bot as a requested reviewer, the sleep-and-recheck round
// trip buys nothing - the only CopilotRequested call is the pre-check.
func TestCopilotGate_ConfirmedRequestSkipsRecheck(t *testing.T) {
	// No recheck shrink on purpose: if the code regresses into the recheck
	// path, the full 10s pause makes this test conspicuously slow rather than
	// silently green.
	ops := &fakeOps{}
	gates := &fakeGates{
		headSHA: copilotHeadSHA,
		reviews: []*CopilotReview{nil, reviewOnHead("LGTM")},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Confirmed request", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, 1, countCalls(calls, "CopilotRequested:"+gatePRURL),
		"a body-confirmed request needs no re-check; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client), "the arrived review is triaged")
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

	assert.Equal(t, 1, countCalls(gates.recorded(), "CopilotRequested:"+gatePRURL),
		"a listed reviewer waits the full window: no grace window, which re-reads the listing; calls=%v", gates.recorded())
	assert.GreaterOrEqual(t, len(gates.recorded()), 2, "the gate polled while it waited")
	assert.Zero(t, modelCallCount(client), "no review means nothing to triage")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestPRGates_CIGreenKeepsTheCopilotOutcome: the CI gate's green exit must not
// erase the Copilot gate's recorded outcome from the PR Gates section - that
// line is the only place on the card body that says what happened to the
// Copilot review.
func TestPRGates_CIGreenKeepsTheCopilotOutcome(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		checks:    [][]CheckResult{{{Name: "ci", Bucket: "pass"}}},
	}
	client := &planLLM{}

	tc := copilotGateContext("Timeout then green", "body")
	tc.AwaitCI = true

	o := prGateRun(ops, gates, &fakeGit{}, client, tc, 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 5 * time.Millisecond

	require.NoError(t, runPRGates(context.Background(), o))

	body := ops.lastBody()
	assert.Contains(t, body, "- Status: passed")
	assert.Contains(t, body, "Copilot review did not arrive in time",
		"the Copilot outcome survives the CI green write; body=%q", body)
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
			reviewOnHead("2 suggestions", swallowedErrorComment, renamingComment),
			reviewOnHead("LGTM"),
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

// TestCopilotGate_ThreadWriteBack: the triage verdicts reach the PR. The
// INVALID comment gets a reply carrying the dismissal reason and its thread
// is resolved; the VALID comment gets a reply after the fix round pushes,
// citing the new head, and its thread stays open for the re-review to
// confirm.
func TestCopilotGate_ThreadWriteBack(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		threads: []ReviewThread{
			threadOf("RT_VALID", swallowedErrorComment),
			threadOf("RT_INVALID", renamingComment),
		},
		reviews: []*CopilotReview{
			reviewOnHead("2 suggestions", swallowedErrorComment, renamingComment),
			reviewOnHead("LGTM"),
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
	o.d.Cfg.GatesCopilotThreadReplies = true

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()

	invalidReply := indexOfCallPrefix(calls, fmt.Sprintf("Reply:%s:%d:", gatePRURL, renamingComment.ID))
	require.GreaterOrEqual(t, invalidReply, 0, "the INVALID comment gets a reply; calls=%v", calls)
	assert.Contains(t, calls[invalidReply], "style preference, not a defect",
		"the dismissal reasoning is the reply body")
	assert.GreaterOrEqual(t, indexOfCall(calls, "Resolve:RT_INVALID"), 0,
		"a dismissed thread is resolved; calls=%v", calls)

	validReply := indexOfCallPrefix(calls, fmt.Sprintf("Reply:%s:%d:", gatePRURL, swallowedErrorComment.ID))
	require.GreaterOrEqual(t, validReply, 0, "the VALID comment gets a reply; calls=%v", calls)
	assert.Contains(t, calls[validReply], "the caller cannot tell the write failed")
	assert.Contains(t, calls[validReply], copilotHeadSHA, "the VALID reply cites the fixed head")

	// The clean re-review (the second CopilotReview read) confirms the fix
	// and only THEN may the VALID thread resolve - never on the push alone.
	validResolve := indexOfCall(calls, "Resolve:RT_VALID")
	require.GreaterOrEqual(t, validResolve, 0, "the confirmed fix resolves the thread; calls=%v", calls)
	assert.Greater(t, validResolve, nthIndexOfCall(calls, "CopilotReview:"+gatePRURL, 2),
		"a VALID thread is never resolved on the fix push alone; calls=%v", calls)

	assert.Contains(t, git.recorded(), "Push:cm/card-1", "the fix is pushed")
}

// TestCopilotGate_ConfirmedFixResolvesOnReReview: Copilot re-posts every
// comment it still holds open, so a re-review that stops repeating a VALID
// comment is the all-clear - only then is its thread resolved.
func TestCopilotGate_ConfirmedFixResolvesOnReReview(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		threads:   []ReviewThread{threadOf("RT_VALID", swallowedErrorComment)},
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "dropped error",
			Valid: true, Reason: "the caller cannot tell the write failed",
		}),
		stopResp("coder: fixed", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, git, client, copilotGateContext("Confirmed fix", "body"), 0)
	o.d.Cfg.GatesCopilotThreadReplies = true

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	resolve := indexOfCall(calls, "Resolve:RT_VALID")
	require.GreaterOrEqual(t, resolve, 0, "the clean re-review confirms the fix; calls=%v", calls)
	assert.Greater(t, resolve, nthIndexOfCall(calls, "CopilotReview:"+gatePRURL, 2),
		"the resolve is earned by the re-review, not the fix push; calls=%v", calls)
}

// nthIndexOfCall returns the index of the n-th (1-based) occurrence of name,
// or -1.
func nthIndexOfCall(calls []string, name string, n int) int {
	seen := 0

	for i, c := range calls {
		if c == name {
			seen++

			if seen == n {
				return i
			}
		}
	}

	return -1
}

// TestCopilotGate_RepeatedValidCommentResolvesNothing: a re-review that
// repeats the VALID comment proves the fix did NOT land - the thread stays
// open and funds another fix round instead.
func TestCopilotGate_RepeatedValidCommentResolvesNothing(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		threads:   []ReviewThread{threadOf("RT_VALID", swallowedErrorComment, 950)},
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", swallowedErrorComment),
			reviewOnHead("still there", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "dropped error",
			Valid: true, Reason: "real",
		}),
		stopResp("coder: fix one", 0.02),
		stopResp("coder: fix two", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, git, client, copilotGateContext("Repeat", "body"), 0)
	o.d.Cfg.GatesCopilotThreadReplies = true

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	resolve := indexOfCall(calls, "Resolve:RT_VALID")
	thirdRead := nthIndexOfCall(calls, "CopilotReview:"+gatePRURL, 3)
	require.GreaterOrEqual(t, thirdRead, 0, "the gate read the repeat and the final clean review; calls=%v", calls)
	require.GreaterOrEqual(t, resolve, 0, "the final clean review still confirms; calls=%v", calls)
	assert.Greater(t, resolve, thirdRead,
		"the repeat round must not resolve; only the clean review does; calls=%v", calls)
	assert.Equal(t, 1, countCalls(calls, "Resolve:RT_VALID"))
}

// TestCopilotGate_ThreadWriteBackOffByZeroValue: a Deps built without the
// knob writes nothing - no thread listing, no replies, no resolves.
func TestCopilotGate_ThreadWriteBackOffByZeroValue(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		threads:   []ReviewThread{threadOf("RT_1", swallowedErrorComment)},
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", renamingComment),
		},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict(
		copilotFinding{File: "README.md", Issue: "wording", Valid: false, Reason: "style"},
	)}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Knob off", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	for _, call := range gates.recorded() {
		assert.NotContains(t, call, "ReviewThreads:", "no thread reads with the knob off")
		assert.NotContains(t, call, "Reply:", "no replies with the knob off")
		assert.NotContains(t, call, "Resolve:", "no resolves with the knob off")
	}
}

// TestCopilotGate_ThreadWithReplyIsNotRepliedAgain: a thread that already
// carries a reply is never replied to twice - the crash window between
// posting a reply and recording the verdict line would otherwise double-post
// on resume - but a dismissed thread still resolves.
func TestCopilotGate_ThreadWithReplyIsNotRepliedAgain(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		threads:   []ReviewThread{threadOf("RT_1", renamingComment, 950)},
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", renamingComment),
		},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict(
		copilotFinding{File: "README.md", Issue: "wording", Valid: false, Reason: "style"},
	)}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Replied already", "body"), 0)
	o.d.Cfg.GatesCopilotThreadReplies = true

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	assert.Equal(t, -1, indexOfCallPrefix(calls, "Reply:"),
		"an answered thread gets no second reply; calls=%v", calls)
	assert.GreaterOrEqual(t, indexOfCall(calls, "Resolve:RT_1"), 0,
		"the dismissal still resolves the thread; calls=%v", calls)
}

// TestCopilotGate_ThreadFoundByDedupeKeyWhenIDMisses: a comment whose REST id
// is absent from the thread listing (a legacy zero-ID value, or a null
// GraphQL databaseId) still reaches its thread through the card's dedupe key
// - the documented fallback, and the only path serving such comments.
func TestCopilotGate_ThreadFoundByDedupeKeyWhenIDMisses(t *testing.T) {
	legacy := ReviewComment{Path: renamingComment.Path, Body: renamingComment.Body} // ID 0

	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		// The thread's comment ids do not contain the comment's (zero) id, so
		// only the RootPath/RootBody key can match it.
		threads: []ReviewThread{{
			ThreadID:   "RT_KEY",
			CommentIDs: []int64{777},
			RootPath:   legacy.Path,
			RootBody:   legacy.Body,
		}},
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", legacy),
		},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict(
		copilotFinding{File: "README.md", Issue: "wording", Valid: false, Reason: "style"},
	)}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Key fallback", "body"), 0)
	o.d.Cfg.GatesCopilotThreadReplies = true

	require.NoError(t, runPRGates(context.Background(), o))

	calls := gates.recorded()
	reply := indexOfCallPrefix(calls, fmt.Sprintf("Reply:%s:%d:", gatePRURL, 777))
	require.GreaterOrEqual(t, reply, 0, "the reply lands on the key-matched thread's root; calls=%v", calls)
	assert.GreaterOrEqual(t, indexOfCall(calls, "Resolve:RT_KEY"), 0, "and the dismissal resolves it")
}

// TestCopilotGate_ThreadWriteBackFailureNeverParks: a failing reply is one
// card note; the gate still passes and the card completes.
func TestCopilotGate_ThreadWriteBackFailureNeverParks(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		threads:   []ReviewThread{threadOf("RT_1", renamingComment)},
		replyErr:  errors.New("HTTP 403: Resource not accessible"),
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", renamingComment),
		},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict(
		copilotFinding{File: "README.md", Issue: "wording", Valid: false, Reason: "style"},
	)}}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Reply fails", "body"), 0)
	o.d.Cfg.GatesCopilotThreadReplies = true

	require.NoError(t, runPRGates(context.Background(), o))

	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0,
		"a write failure never parks the card")
	assert.Equal(t, -1, indexOfCall(gates.recorded(), "Resolve:RT_1"),
		"an unanswered thread is not resolved; the reply failed first")
}

// TestCopilotGate_ReRequestNotTakingSkipsAfterGrace: the re-request after a
// fix round can be accepted and silently dropped exactly like the first
// request. When the reviewer never appears and no review lands in the grace
// window, the gate passes with a note naming the dropped re-request - it does
// not burn the full copilot_wait on a re-review nothing requested.
func TestCopilotGate_ReRequestNotTakingSkipsAfterGrace(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)
	shrinkCopilotGrace(t, 5*time.Millisecond)

	ops := &fakeOps{}
	gates := &fakeGates{
		requestSilentlyNoOps: true,
		headSHA:              copilotHeadSHA,
		// A review is already on the head, so the gate goes straight to triage;
		// the only RequestCopilotReview call is the re-request after the fix.
		// The nil entry then answers every later poll: no re-review ever lands.
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", swallowedErrorComment),
			nil,
		},
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{File: "internal/api/handler.go", Issue: "dropped error", Valid: true, Reason: "real"}),
		stopResp("coder: fixed", 0.02),
	}}

	o := prGateRun(ops, gates, git, client, copilotGateContext("Dropped re-request", "body"), 0)
	// Far longer than the grace window, short enough that a regression to the
	// full wait fails this test quickly rather than hanging it.
	o.d.Cfg.GatesCopilotWaitTimeout = 300 * time.Millisecond

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 2, modelCallCount(client), "triage and the fix; there is no re-review to re-triage")
	assert.Contains(t, git.recorded(), "Push:cm/card-1", "the fix is pushed; git=%v", git.recorded())
	// The persisted gates section carries the skip the grace window took;
	// burning the full wait on the re-review would record the timeout skip
	// instead, which is the regression this separates out.
	assert.Contains(t, ops.lastBody(), "fix pushed but the Copilot re-review request did not take",
		"the card records the dropped re-request, not a wait that ran out; body=%q", ops.lastBody())
	assert.NotContains(t, ops.lastBody(), "- Copilot gate: satisfied",
		"an unreviewed fix must not be recorded as a satisfied gate")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0,
		"the gate passes; the fix is already pushed")
}

// TestCopilotGate_ReRequestFailureStillWaitsForTheAutoReview: on a ruleset
// repo Copilot re-reviews every push by itself and the explicit re-request is
// the fragile step. A generic re-request failure must not pass the gate with
// the fix unreviewed - it records the error and waits, exactly like a failed
// first request does.
func TestCopilotGate_ReRequestFailureStillWaitsForTheAutoReview(t *testing.T) {
	shrinkCopilotRecheck(t, time.Millisecond)

	ops := &fakeOps{}
	gates := &fakeGates{
		requested:    false,
		headSHA:      copilotHeadSHA,
		reRequestErr: errors.New("HTTP 422: Reviews may only be requested from collaborators"),
		// Nothing on the head when the gate probes, so the first
		// RequestCopilotReview call is the initial request (which succeeds);
		// the second - after the fix round - is the one reRequestErr targets.
		reviews: []*CopilotReview{
			nil,
			reviewOnHead("1 suggestion", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{File: "internal/api/handler.go", Issue: "dropped error", Valid: true, Reason: "real"}),
		stopResp("coder: fixed", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, git, client, copilotGateContext("Re-request fails", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 3, modelCallCount(client), "triage, fix, and the re-triage of the auto re-review")
	assert.Contains(t, ops.lastBody(), "- Copilot gate: satisfied")
}

// TestCopilotGate_DedupesRepeatedComments: Copilot re-posts a comment it
// already made. A comment triaged VALID is still open when it comes back
// verbatim - the fix round did not actually resolve it - so the repeat buys
// another fix round rather than the already-triaged pass.
func TestCopilotGate_DedupesRepeatedComments(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			reviewOnHead("one suggestion", swallowedErrorComment),
			reviewOnHead("one suggestion", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
	}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "the write error is dropped",
			Valid: true, Reason: "the caller cannot tell the write failed",
		}),
		stopResp("coder: round 1 fix", 0.02),
		stopResp("coder: round 2 fix", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Repeat", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 4, modelCallCount(client),
		"two triages (round 1 and the final clean re-review) and two fixes; the repeat itself spends no triage call")
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 2/3", "the repeat bought a second round")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)

	gatesSection := extractSection(ops.lastBody(), gatesSectionHeading)
	assert.Contains(t, gatesSection, "Status: passed")
	assert.NotContains(t, gatesSection, "the write error is dropped",
		"a resolved round's findings must not linger under a passed status; section=%q", gatesSection)
}

// TestCopilotGate_DedupesRepeatedInvalidComments: a comment triaged INVALID in
// round 1, still repeated after the fix round (the VALID comment beside it is
// gone from the re-review, since the fix resolved it), is correctly read as
// already triaged - unlike a repeated VALID finding
// (TestCopilotGate_DedupesRepeatedComments), it never buys another round.
func TestCopilotGate_DedupesRepeatedInvalidComments(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			reviewOnHead("2 suggestions", swallowedErrorComment, renamingComment),
			reviewOnHead("1 suggestion", renamingComment),
		},
	}
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
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Repeat invalid only", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 2, modelCallCount(client),
		"one triage and one fix: the repeated INVALID comment is filtered before a second triage")
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 1/3", "exactly one round was spent")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_RepeatedValidFindingBuysAnotherFixRound: round 1 triages one
// VALID comment and one INVALID comment side by side. The fix round does not
// actually resolve the VALID one, and Copilot re-posts BOTH verbatim on the
// re-review. The repeat must spend no triage call, must not take the
// already-triaged pass, and must fund a second fix round on the still-open
// VALID finding alone.
func TestCopilotGate_RepeatedValidFindingBuysAnotherFixRound(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			reviewOnHead("2 suggestions", swallowedErrorComment, renamingComment),
			reviewOnHead("2 suggestions", swallowedErrorComment, renamingComment),
			reviewOnHead("LGTM"),
		},
	}
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
		stopResp("coder: round 1 fix", 0.02),
		stopResp("coder: round 2 fix", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Repeated valid finding", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 4, modelCallCount(client),
		"two triages (round 1 and the final clean re-review) and two fixes - the repeat spends no triage call")

	body := ops.lastBody()
	assert.Contains(t, body, "- Copilot rounds used: 2/3", "the repeated VALID finding bought a second fix round")

	history := strings.Join(ops.bodyUpdates, "\n===\n")
	assert.Contains(t, history, copilotRepeatedReason,
		"the second round's finding is recorded as reopened, not freshly triaged; history=%q", history)
	assert.NotContains(t, history, "## Copilot Review (Round 2)",
		"nothing fresh was triaged on the repeat, so it writes no new Copilot Review section; history=%q", history)
	assert.Contains(t, history, "## Copilot Review (Round 3)",
		"the clean re-review is the next triage round after the skipped repeat; history=%q", history)

	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_RepeatedValidFindingParksAtRoundsCap: a VALID finding that
// Copilot keeps re-posting after every fix never gets waved through as
// already triaged - it spends the gate's rounds cap and parks instead of
// passing.
func TestCopilotGate_RepeatedValidFindingParksAtRoundsCap(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			reviewOnHead("one suggestion", swallowedErrorComment),
			reviewOnHead("one suggestion", swallowedErrorComment),
			reviewOnHead("one suggestion", swallowedErrorComment),
		},
	}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "the write error is dropped",
			Valid: true, Reason: "the caller cannot tell the write failed",
		}),
		stopResp("coder: round 1 fix", 0.01),
		stopResp("coder: round 2 fix", 0.01),
		stopResp("coder: round 3 fix", 0.01),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Never resolved", "body"), 0)

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "3 rounds")
	assert.Equal(t, 4, modelCallCount(client), "one triage and three fixes - the cap is 3, not 4")

	body := ops.lastBody()
	assert.Contains(t, body, "- Copilot rounds used: 3/3")
	assert.Contains(t, body, "- Status: parked:")
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"),
		"a parked card must NOT reach done")
}

// TestCopilotGate_DedupeSurvivesResume: the dedupe keys live on the card, so a
// re-triggered run in a fresh container does not re-triage - or re-fix - a
// comment an earlier run already triaged INVALID. The seeded body is written by
// recordCopilotRound itself, so the recorded line shape and the key read back out
// of it cannot drift apart. The comment is deliberately multi-line and longer
// than the key: flattening and truncation have to round-trip too. A VALID
// finding's resume behaviour - buying another fix round instead - is
// TestCopilotGate_ResumedValidFindingBuysFixRound.
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
			Valid: false, Reason: "already covered by the retry wrapper - not a defect",
		}}, false)

	body := seed.lastBody()
	require.Contains(t, body, "### Comments triaged", "the parked run recorded its dedupe keys; body=%q", body)

	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{reviewOnHead("one suggestion", wrapped)},
	}
	client := &planLLM{}

	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("Resumed", body), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Zero(t, modelCallCount(client),
		"a comment triaged before the park is never triaged again")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_ResumedValidFindingBuysFixRound: the sibling of
// TestCopilotGate_DedupeSurvivesResume for a finding an earlier run triaged
// VALID. The fix round that finding was supposed to buy never happened before
// the park, so its repeat on resume must fund a fresh fix round rather than
// take the already-triaged pass - the round trip a re-triggered card needs.
func TestCopilotGate_ResumedValidFindingBuysFixRound(t *testing.T) {
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
		reviews: []*CopilotReview{
			reviewOnHead("one suggestion", wrapped),
			reviewOnHead("LGTM"),
		},
	}
	client := &planLLM{responses: []llm.Response{
		stopResp("coder: fixed on resume", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Resumed", body), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 2, modelCallCount(client),
		"the repeat spends no triage call - only the fix round and the final clean re-triage")
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 1/3", "the resumed repeat bought a fix round")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestUnseenComments_FiltersAnAlreadyTriagedInvalidComment guards the rename
// from a value test to a membership test: a comment triaged INVALID stores
// false, and unseenComments must still treat it as already triaged rather than
// reading the false value as "not seen".
func TestUnseenComments_FiltersAnAlreadyTriagedInvalidComment(t *testing.T) {
	c := ReviewComment{Path: "internal/api/handler.go", Body: "Consider renaming this variable for clarity."}
	triaged := map[string]bool{copilotCommentKey(c.Path, c.Body): false}

	fresh := unseenComments([]ReviewComment{c}, triaged)

	assert.Empty(t, fresh, "an INVALID-valued entry must still be filtered as already triaged; fresh=%v", fresh)
}

// TestCopilotCommentVerdicts covers the single pairing rule between a review
// comment and the triage verdict for it: index pairing when the counts match
// (the triage prompt asks for one entry per comment, in order), otherwise an OR
// of the Valid flag over every finding naming the comment's path, and VALID for
// a comment no finding names at all - an unjudged comment must never read as
// safe to ignore.
func TestCopilotCommentVerdicts(t *testing.T) {
	commentA := ReviewComment{Path: "a.go", Body: "issue in a"}
	commentB := ReviewComment{Path: "b.go", Body: "issue in b"}

	tests := []struct {
		name     string
		comments []ReviewComment
		findings []copilotFinding
		want     []bool
	}{
		{
			name:     "counts match: paired by index regardless of file name",
			comments: []ReviewComment{commentA, commentB},
			findings: []copilotFinding{
				{File: "z.go", Valid: true},
				{File: "y.go", Valid: false},
			},
			want: []bool{true, false},
		},
		{
			name:     "counts differ: OR across every finding sharing the comment's path",
			comments: []ReviewComment{commentA},
			findings: []copilotFinding{
				{File: "a.go", Valid: false},
				{File: "a.go", Valid: true},
			},
			want: []bool{true},
		},
		{
			name:     "counts differ: a comment no finding names reads VALID",
			comments: []ReviewComment{commentA, commentB},
			findings: []copilotFinding{
				{File: "a.go", Valid: false},
			},
			want: []bool{false, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, copilotCommentVerdicts(tt.comments, tt.findings))
		})
	}
}

// TestCopilotCommentVerdicts_RoundTripThroughRecordAndTriaged drives
// recordCopilotRound with one VALID and one INVALID finding - one comment body
// deliberately multi-line and longer than copilotKeyBodyChars, as
// TestCopilotGate_DedupeSurvivesResume does - then feeds the recorded body back
// through copilotTriagedComments and asserts both keys read back with the right
// verdict.
func TestCopilotCommentVerdicts_RoundTripThroughRecordAndTriaged(t *testing.T) {
	longComment := ReviewComment{
		Path: "internal/api/handler.go",
		Body: "This error is swallowed - the caller\ncan never see it, and the write is\n" +
			"reported as a success, which is worse than raising nothing at all.",
	}
	nitComment := ReviewComment{
		Path: "internal/api/style.go",
		Body: "Consider renaming this variable for clarity.",
	}

	seed := &fakeOps{}
	recorder := prGateRun(seed, &fakeGates{}, &fakeGit{}, &planLLM{},
		copilotGateContext("Parked", "Task body."), 0)

	recorder.recordCopilotRound(context.Background(), 1,
		[]ReviewComment{longComment, nitComment},
		[]copilotFinding{
			{File: longComment.Path, Issue: "the write error is dropped", Valid: true, Reason: "the caller cannot tell the write failed"},
			{File: nitComment.Path, Issue: "cosmetic only", Valid: false, Reason: "not a defect"},
		}, false)

	body := seed.lastBody()

	triaged := copilotTriagedComments(body)

	validVerdict, ok := triaged[copilotCommentKey(longComment.Path, longComment.Body)]
	require.True(t, ok, "the VALID comment's key is present; triaged=%v", triaged)
	assert.True(t, validVerdict, "the VALID comment reads back as triaged VALID")

	invalidVerdict, ok := triaged[copilotCommentKey(nitComment.Path, nitComment.Body)]
	require.True(t, ok, "the INVALID comment's key is present; triaged=%v", triaged)
	assert.False(t, invalidVerdict, "the INVALID comment reads back as triaged INVALID")
}

// TestRecordCopilotRound_PathCannotForgeAVerdictLine: a comment path is
// flattened before it is written, the same as the body, so a path carrying a
// newline cannot inject a second record line that overwrites a real finding's
// verdict when the section is read back on resume.
func TestRecordCopilotRound_PathCannotForgeAVerdictLine(t *testing.T) {
	genuine := ReviewComment{Path: "internal/api/handler.go", Body: "the write error is dropped"}
	forging := ReviewComment{
		Path: "internal/api/style.go\n- INVALID internal/api/handler.go",
		Body: "the write error is dropped",
	}

	seed := &fakeOps{}
	recorder := prGateRun(seed, &fakeGates{}, &fakeGit{}, &planLLM{},
		copilotGateContext("Parked", "Task body."), 0)

	recorder.recordCopilotRound(context.Background(), 1,
		[]ReviewComment{genuine, forging},
		[]copilotFinding{
			{File: genuine.Path, Issue: "the write error is dropped", Valid: true, Reason: "the caller cannot tell the write failed"},
			{File: forging.Path, Issue: "cosmetic only", Valid: false, Reason: "not a defect"},
		}, false)

	triaged := copilotTriagedComments(seed.lastBody())

	require.Len(t, triaged, 2, "one entry per comment, none forged; triaged=%v", triaged)

	realVerdict, ok := triaged[copilotCommentKey(genuine.Path, genuine.Body)]
	require.True(t, ok, "the genuine comment's key is present; triaged=%v", triaged)
	assert.True(t, realVerdict, "the genuine comment's VALID verdict survives a forging neighbour")

	forgingVerdict, ok := triaged[copilotCommentKey(forging.Path, forging.Body)]
	require.True(t, ok, "the forging comment round-trips under its own flattened key; triaged=%v", triaged)
	assert.False(t, forgingVerdict, "the forging comment reads back with its own INVALID verdict")
}

// TestCopilotTriagedComments_LegacyVerdictLessLinesReadAsValid: a
// "### Comments triaged" block written before verdicts were recorded still
// parses, and every entry reads as VALID - the conservative reading for a
// comment whose verdict was never recorded.
func TestCopilotTriagedComments_LegacyVerdictLessLinesReadAsValid(t *testing.T) {
	body := "## Copilot Review\n\n" +
		"- the write error is dropped\n\n" +
		"### " + copilotCommentsHeading + "\n\n" +
		"- internal/api/handler.go: the write error is dropped\n" +
		"- internal/api/style.go: consider renaming this variable\n"

	triaged := copilotTriagedComments(body)

	require.Len(t, triaged, 2, "triaged=%v", triaged)

	handlerVerdict, ok := triaged[copilotCommentKey("internal/api/handler.go", "the write error is dropped")]
	require.True(t, ok, "triaged=%v", triaged)
	assert.True(t, handlerVerdict, "a verdict-less legacy line reads as VALID")

	styleVerdict, ok := triaged[copilotCommentKey("internal/api/style.go", "consider renaming this variable")]
	require.True(t, ok, "triaged=%v", triaged)
	assert.True(t, styleVerdict, "a verdict-less legacy line reads as VALID")
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
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestCopilotGate_ThreeRoundsThenPark: a reviewer that keeps finding new real
// defects outlives the fix budget - three rounds, then the card parks in review
// with the open findings named.
func TestCopilotGate_ThreeRoundsThenPark(t *testing.T) {
	freshReview := func(n int) *CopilotReview {
		return reviewOnHead(fmt.Sprintf("round %d", n), ReviewComment{
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
		reviews:   []*CopilotReview{reviewOnHead("one suggestion", swallowedErrorComment)},
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
			reviewOnHead("one suggestion", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
	}
	client := &planLLM{responses: []llm.Response{
		stopResp("Looks mostly fine to me, though I did not check the handler.", 0.01),
		stopResp("coder: returned the write error", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Junk verdict", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

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
		reviews:   []*CopilotReview{reviewOnHead("LGTM overall")},
	}
	client := &planLLM{responses: []llm.Response{
		stopResp("garbage, not JSON", 0.01),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Body-only review", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

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
			reviewOnHead("one suggestion", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
	}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(), // parses cleanly, judges nothing
		stopResp("coder: returned the write error", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, &fakeGit{committed: true}, client, copilotGateContext("Silent verdict", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 3, modelCallCount(client), "the unjudged comment still funds the fix round")
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 1/3")
}

// TestPRGates_LateCopilotReviewIsTriagedAfterCI: the Copilot wait timed out,
// CI then took long enough for the review to land. The gate must not finish
// with an unread review on the head - one probe after CI catches it.
func TestPRGates_LateCopilotReviewIsTriagedAfterCI(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested:              true,
		headSHA:                copilotHeadSHA,
		holdReviewsUntilChecks: true,
		reviews:                []*CopilotReview{reviewOnHead("LGTM")},
		checks:                 [][]CheckResult{{{Name: "ci", Bucket: "pass"}}},
	}
	client := &planLLM{responses: []llm.Response{copilotVerdict()}}

	tc := copilotGateContext("Late review", "body")
	tc.AwaitCI = true

	o := prGateRun(ops, gates, &fakeGit{}, client, tc, 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 5 * time.Millisecond

	require.NoError(t, runPRGates(context.Background(), o))

	// The fake withholds the review until Checks runs, so the wait can only
	// have timed out and the probe that found it can only be the one after CI.
	assert.Greater(t, countCalls(gates.recorded(), "CopilotReview:"+gatePRURL), 1,
		"the wait polled to its deadline, then the late probe read the PR again; calls=%v", gates.recorded())
	assert.Equal(t, 1, modelCallCount(client), "the late review is triaged")
	assert.Contains(t, ops.lastBody(), "- Copilot gate: satisfied")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestPRGates_LateReviewFixRoundReRunsBothGates: a late review with a real
// finding funds a fix round, and the second pass re-runs both gates before the
// card completes - the Copilot gate (served by its head probe here, since the
// fake's head SHA does not move) and the CI gate.
func TestPRGates_LateReviewFixRoundReRunsBothGates(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested:              true,
		headSHA:                copilotHeadSHA,
		holdReviewsUntilChecks: true,
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
		checks: [][]CheckResult{{{Name: "ci", Bucket: "pass"}}},
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{File: "internal/api/handler.go", Issue: "dropped error", Valid: true, Reason: "real"}),
		stopResp("coder: fixed", 0.02),
		copilotVerdict(),
	}}

	tc := copilotGateContext("Late review with finding", "body")
	tc.AwaitCI = true

	o := prGateRun(ops, gates, git, client, tc, 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 5 * time.Millisecond

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 3, modelCallCount(client), "triage, fix, re-triage")
	assert.Contains(t, git.recorded(), "Push:cm/card-1")
	assert.GreaterOrEqual(t, strings.Count(strings.Join(gates.recorded(), " "), "Checks:"), 2,
		"CI is polled again after the fix push; calls=%v", gates.recorded())
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 1/3")
	assert.Contains(t, ops.lastBody(), "- Copilot gate: satisfied")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

// TestPRGates_ReEnteredCIGateRemembersEarlierChecks: the checks-were-seen memory
// also has to span a re-entry within one run. A late-review fix round pushes a
// new head and sends the gates round again, and the new head's run has not
// registered with CI yet - so the re-entered gate's first poll is empty. Without
// the run-level memory (st.CIRounds is still 0: the round the late check spent
// was a Copilot round) that empty poll reads as "this repo has no CI" and
// completes the card on a head nothing ever checked. The grace window is zeroed
// so only that memory can hold the gate.
func TestPRGates_ReEnteredCIGateRemembersEarlierChecks(t *testing.T) {
	shrinkNoChecksGrace(t, 0)

	ops := &fakeOps{}
	gates := &fakeGates{
		requested:              true,
		headSHA:                copilotHeadSHA,
		holdReviewsUntilChecks: true,
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
		checks: [][]CheckResult{
			{{Name: "ci", Bucket: "pass"}}, // the first head is green
			{},                             // the fixed head's run has not registered
			{{Name: "ci", Bucket: "pass"}}, // it registers, and it is green
		},
	}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{File: "internal/api/handler.go", Issue: "dropped error", Valid: true, Reason: "real"}),
		stopResp("coder: fixed", 0.02),
		copilotVerdict(),
	}}

	tc := copilotGateContext("Late review then an unregistered head", "body")
	tc.AwaitCI = true

	git := &fakeGit{committed: true}

	o := prGateRun(ops, gates, git, client, tc, 0)
	o.d.Cfg.GatesCopilotWaitTimeout = 5 * time.Millisecond

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Contains(t, git.recorded(), "Push:cm/card-1", "the late fix round pushed a new head")
	assert.GreaterOrEqual(t, strings.Count(strings.Join(gates.recorded(), " "), "Checks:"), 3,
		"the re-entered gate waited through the empty poll; calls=%v", gates.recorded())
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0,
		"the card still completes once the fixed head goes green")
}

// TestCopilotGate_FixRoundNoChangeParks: a Copilot fix round whose fix commits
// nothing parks with the no-change reason instead of cycling to the rounds cap,
// once the one-step retry the bar raise buys is already spent (see
// TestCopilotGate_NoChangeRaisesTheBarBeforeParking for that retry itself). The
// bar counter is shared with the review loop and the CI gate, so a card whose
// rounds already escalated has had a stronger fixer tried on it and parks here
// on the first no-op round. The fakeGit defaults to committed=false, producing
// a no-change outcome.
func TestCopilotGate_FixRoundNoChangeParks(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{reviewOnHead("1 suggestion", swallowedErrorComment)},
	}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "the write error is dropped",
			Valid: true, Reason: "the caller cannot tell the write failed",
		}),
		stopResp("coder: no-op fix", 0.01),
	}}

	// committed defaults to false -> runFix returns committed=false, err=nil
	o := prGateRun(ops, gates, &fakeGit{}, client, copilotGateContext("No change fix", "body"), 0)
	o.fixBarSteps = 1 // a review round already climbed the bar

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "Copilot fix produced no change",
		"the park reason names the no-change outcome; reason=%q", parked.Reason)

	calls := gates.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "RequestCopilotReview:"+gatePRURL),
		"a no-change fix must not re-request a review; calls=%v", calls)
	assert.Equal(t, 2, modelCallCount(client),
		"one triage call and one fix model call; client.calls=%v", client.models)
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"),
		"a parked card must NOT reach done")
}

// TestCopilotGate_NoChangeRaisesTheBarBeforeParking: a Copilot fix round that
// produced no change is QUALITY evidence, and one more round is already funded
// by gatesRoundsCap - so the gate spends it on a stronger fixer instead of
// parking without ever having tried one, mirroring the CI gate's rule (see
// TestCIGate_NoChangeRaisesTheBarBeforeParking). The repeated VALID finding on
// the unchanged head buys the second round without a fresh triage call.
func TestCopilotGate_NoChangeRaisesTheBarBeforeParking(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{reviewOnHead("1 suggestion", swallowedErrorComment)},
	}
	git := &fakeGit{committed: false}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "the write error is dropped",
			Valid: true, Reason: "the caller cannot tell the write failed",
		}),
		stopResp("coder: nothing to change", 0.01),
		stopResp("coder: still nothing", 0.01),
	}}

	o := prGateRun(ops, gates, git, client, copilotGateContext("No change", "body"), 0)
	// A pool with a second coder in it, so the round the bar raise buys has a
	// model to run: the default gate registry carries one, and round 2's pick
	// excludes round 1's failed fixer.
	o.d.Registry = reviewFixRegistry()

	var parked *GatesParkedError
	require.ErrorAs(t, runPRGates(context.Background(), o), &parked)
	assert.Contains(t, parked.Reason, "Copilot fix produced no change",
		"the park reason names the no-change outcome; reason=%q", parked.Reason)

	assert.Equal(t, 1, o.fixBarSteps, "the second round got a stronger fixer")
	assert.Zero(t, o.fixBudgetSteps, "a no-op round is not volume evidence")
	assert.Equal(t, 3, modelCallCount(client),
		"one triage call plus exactly one extra fix round was bought, not a spin")
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 2/3")
}

// TestCopilotGate_NoChangeRetrySkipsReRequest: the gateNoChangeRetry-funded
// retry works an unchanged head, so re-requesting the review would return the
// same findings verbatim - the retry round runs on the already-fetched
// findings instead, and the family extends
// TestCopilotGate_NoChangeRaisesTheBarBeforeParking with the request count.
// The self-bounding loop: the second no-change round finds the retry already
// spent and parks before any re-request is issued.
func TestCopilotGate_NoChangeRetrySkipsReRequest(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews:   []*CopilotReview{reviewOnHead("1 suggestion", swallowedErrorComment)},
	}
	git := &fakeGit{committed: false}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "the write error is dropped",
			Valid: true, Reason: "the caller cannot tell the write failed",
		}),
		stopResp("coder: nothing to change", 0.01),
		stopResp("coder: still nothing", 0.01),
	}}

	o := prGateRun(ops, gates, git, client, copilotGateContext("No change no re-request", "body"), 0)
	// A pool with a second coder in it, so the round the bar raise buys has a
	// model to run: the default gate registry carries one, and round 2's pick
	// excludes round 1's failed fixer.
	o.d.Registry = reviewFixRegistry()

	var parked *GatesParkedError

	require.ErrorAs(t, runPRGates(context.Background(), o), &parked)
	assert.Contains(t, parked.Reason, "Copilot fix produced no change",
		"the park reason names the no-change outcome; reason=%q", parked.Reason)

	assert.Zero(t, gates.requests,
		"a no-change retry works an unchanged head and must never re-request a review; calls=%v", gates.recorded())
	assert.Equal(t, 3, modelCallCount(client),
		"one triage call plus the retry fix round, with no re-triage of the same findings")
	assert.NotContains(t, git.recorded(), "Push:cm/card-1",
		"neither round committed anything, so nothing was pushed; git=%v", git.recorded())
	assert.Contains(t, ops.lastBody(), "- Copilot rounds used: 2/3", "the retry round ran on the same findings")
}

// TestCopilotGate_CommittedFixStillReRequests: the re-request skip is scoped
// to the funded no-change retry. A fix round that commits moves the head, so
// today's behavior holds verbatim - exactly one re-request goes out and the
// clean re-review of the new head passes the gate.
func TestCopilotGate_CommittedFixStillReRequests(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "the write error is dropped",
			Valid: true, Reason: "the caller cannot tell the write failed",
		}),
		stopResp("coder: returned the write error", 0.02),
		copilotVerdict(),
	}}

	o := prGateRun(ops, gates, git, client, copilotGateContext("Committed fix", "body"), 0)

	require.NoError(t, runPRGates(context.Background(), o))

	assert.Equal(t, 1, gates.requests,
		"a committed fix round re-requests the review exactly once; calls=%v", gates.recorded())
	assert.Contains(t, git.recorded(), "Push:cm/card-1", "the fix is pushed; git=%v", git.recorded())
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0,
		"the clean re-review completes the card")
}

// A Copilot fix round that PUSHED and then ran out of turns earns the re-review
// of its new head, the same rule the CI gate applies: the cap widens the next
// round rather than parking work that is already on the branch.
func TestCopilotGate_CappedFixRoundAfterPushReRequests(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{
		requested: true,
		headSHA:   copilotHeadSHA,
		reviews: []*CopilotReview{
			reviewOnHead("1 suggestion", swallowedErrorComment),
			reviewOnHead("LGTM"),
		},
	}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: slices.Concat(
		[]llm.Response{copilotVerdict(copilotFinding{
			File: "internal/api/handler.go", Issue: "the write error is dropped",
			Valid: true, Reason: "the caller cannot tell the write failed",
		})},
		burnResps(5), // the fix coder commits work but never lands finish
		[]llm.Response{copilotVerdict()},
	)}

	o := prGateRun(ops, gates, git, client, copilotGateContext("Capped fix", "body"), 0)
	o.d.Cfg.MaxTurns = 5

	require.NoError(t, runPRGates(context.Background(), o), "a pushed fix is not a reason to park")

	assert.Equal(t, 1, gates.requests, "the pushed head gets its re-review; calls=%v", gates.recorded())
	assert.Contains(t, git.recorded(), "Push:cm/card-1", "git=%v", git.recorded())
	assert.Equal(t, 1, o.fixBudgetSteps, "the cap widens the next fix round")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0, "calls=%v", ops.recorded())
}

// TestCIGate_FixRoundNoChangeParks: a CI fix round whose fix commits nothing
// parks with the no-change reason instead of cycling to the rounds cap - once
// the bar raise that buys a first no-op round one more attempt is spent. The
// bar counter is shared with the review loop, so a card whose review rounds
// already escalated has had a stronger fixer tried on it and parks here on the
// first no-op round.
func TestCIGate_FixRoundNoChangeParks(t *testing.T) {
	shrinkFixRoundReserve(t, 0) // a millisecond-scale gate still funds its round

	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{{failingCheck()}}}
	client := &planLLM{responses: []llm.Response{
		stopResp("coder: no-op fix", 0.01),
	}}

	// committed defaults to false -> runFix returns committed=false, err=nil
	o := prGateRun(ops, gates, &fakeGit{}, client, ciGateContext("No change CI fix", "body"), 0)
	o.fixBarSteps = 1 // a review round already climbed the bar

	err := runPRGates(context.Background(), o)

	var parked *GatesParkedError

	require.ErrorAs(t, err, &parked)
	assert.Contains(t, parked.Reason, "CI fix produced no change",
		"the park reason names the no-change outcome; reason=%q", parked.Reason)

	// The fix round fetches failure logs, runs the fix model, then parks - no re-poll.
	calls := gates.recorded()
	assert.Equal(t, []string{"Checks:" + gatePRURL}, calls[:1],
		"only the initial poll; no re-poll after the fix; calls=%v", calls)
	assert.Equal(t, 1, modelCallCount(client),
		"one fix model call; the CI gate has no triage phase")
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"),
		"a parked card must NOT reach done")
}

// nonEmpty normalizes an empty slice to nil so table cases can leave the want
// field unset.
func nonEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}

	return s
}

// With the fix counters monotone, a gate's runFix can exhaust its fix models
// and return *ReviewParkedError through the SHARED runFix. Every resource a
// gate can run out of maps to a park reason, so the gate parks with the PR
// standing rather than falling through to a hard error that ends the run. The
// fix path parks for more than one reason, so the card carries the one the
// error itself gives.
func TestGateResourceParkMatchesReviewParked(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"budget":                   {&BudgetExceededError{}, "budget"},
		"turn cap":                 {&MaxTurnsError{Model: "m", Turns: 45}, "turns"},
		"parked, fixers exhausted": {&ReviewParkedError{Reason: reviewParkedFixExhausted}, reviewParkedFixExhausted},
		"parked, none selectable":  {&ReviewParkedError{Reason: reviewParkedNoFixModel}, reviewParkedNoFixModel},
		"parked, no reason given":  {&ReviewParkedError{}, gatesFixParkFallbackReason},
		"unrelated":                {errors.New("boom"), ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, gateResourcePark(tc.err, "budget", "turns"))
		})
	}
}

// A capped CI fix round that LANDED a diff is worth another CI cycle: the poll
// loop will see a real new head. One that landed nothing is not - it would
// re-bucket the identical settled-red checks and burn the remaining rounds
// back to back with no CI feedback between them.
//
// The retry is not one-shot: a pushing round that keeps capping keeps buying
// cycles until gatesRoundsCap stops it, which is what bounds it. A round that
// pushes nothing parks on the first cap.
func TestCIGate_CappedFixRetriesOnlyWhenItPushed(t *testing.T) {
	for name, tc := range map[string]struct {
		committed bool
		// atTopRung seeds a card whose bar already opens the budget at the top
		// rung, so the width cannot move and only the budget term parks it.
		atTopRung  bool
		wantRounds int
		wantWider  bool
	}{
		"capped with a commit":   {committed: true, wantRounds: 3, wantWider: true},
		"capped with nothing":    {committed: false, wantRounds: 1},
		"capped at the top rung": {committed: true, atTopRung: true, wantRounds: 1},
	} {
		t.Run(name, func(t *testing.T) {
			ops := &fakeOps{}
			gates := &fakeGates{checks: [][]CheckResult{
				{failingCheck()}, {failingCheck()}, {failingCheck()}, {failingCheck()},
			}}
			git := &fakeGit{committed: tc.committed}
			// Every fix round burns its whole budget, so each returns max_turns.
			client := &planLLM{responses: burnResps(60)}

			o := prGateRun(ops, gates, git, client, ciGateContext("Capped", "body"), 0)
			o.d.Cfg.MaxTurns = 5

			if tc.atTopRung {
				o.cardSizing = sizing{Bar: registry.TierCritical, Budget: seedBudgetStep(registry.TierCritical)}
				o.fixBudgetSteps = 1 // one rung short of the counter's ceiling, but already at the width ceiling
			}

			var parked *GatesParkedError
			require.ErrorAs(t, runPRGates(context.Background(), o), &parked)

			body := ops.lastBody()
			assert.Contains(t, body, fmt.Sprintf("- CI rounds used: %d/3", tc.wantRounds))

			if tc.wantWider {
				assert.Positive(t, o.fixBudgetSteps, "a pushed capped round runs the next one wider")
			}

			if tc.atTopRung {
				assert.Equal(t, 1, o.fixBudgetSteps,
					"a round that could not run wider is not recorded as a widening")
			}
		})
	}
}

// A CI fix round that produced no change is QUALITY evidence, and one more
// round is already funded by gatesRoundsCap - so the gate spends it on a
// stronger fixer instead of parking without ever having tried one.
func TestCIGate_NoChangeRaisesTheBarBeforeParking(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{
		{failingCheck()}, {failingCheck()}, {failingCheck()}, {failingCheck()},
	}}
	git := &fakeGit{committed: false}
	client := &planLLM{responses: []llm.Response{
		stopResp("coder: nothing to change", 0.01),
		stopResp("coder: still nothing", 0.01),
	}}

	o := prGateRun(ops, gates, git, client, ciGateContext("No change", "body"), 0)
	// A pool with a second coder in it, so the round the bar raise buys has a
	// model to run: the default gate registry carries one, and round 2's pick
	// excludes round 1's failed fixer.
	o.d.Registry = reviewFixRegistry()

	var parked *GatesParkedError
	require.ErrorAs(t, runPRGates(context.Background(), o), &parked)

	assert.Equal(t, 1, o.fixBarSteps, "the second round got a stronger fixer")
	assert.Zero(t, o.fixBudgetSteps, "a no-op round is not volume evidence")
	assert.Equal(t, 2, modelCallCount(client), "exactly one extra round was bought, not a spin")
	assert.Contains(t, ops.lastBody(), "- CI rounds used: 2/3")
}

// A gate fix round can exhaust its fix models through the SHARED runFix: the
// no-change bar raise excludes the model that failed, and a pool with no other
// coder in it leaves the escalated round nothing to select. That park must reach
// the card as a park. A hard error here would end a run whose work is already
// pushed, instead of leaving the PR standing for a human.
func TestCIGate_ExhaustedFixModelsParksInsteadOfFailing(t *testing.T) {
	ops := &fakeOps{}
	gates := &fakeGates{checks: [][]CheckResult{
		{failingCheck()}, {failingCheck()}, {failingCheck()},
	}}
	// One coder model in the pool, and one scripted round: the second round never
	// reaches a model call.
	client := &planLLM{responses: []llm.Response{stopResp("coder: nothing to change", 0.01)}}

	o := prGateRun(ops, gates, &fakeGit{}, client, ciGateContext("Exhausted", "body"), 0)

	var parked *GatesParkedError
	require.ErrorAs(t, runPRGates(context.Background(), o), &parked,
		"an exhausted fix pool parks the gate; a hard error would end the run and abandon the PR")

	assert.Equal(t, reviewParkedNoFixModel, parked.Reason,
		"the card names what actually happened: nothing was selectable, not that a round had failed")
	assert.Equal(t, 1, modelCallCount(client), "the escalated round is refused before it spends a call")
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "TransitionCard:done"),
		"a parked card must NOT reach done")
}
