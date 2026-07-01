package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- fakes ------------------------------------------------------------------

// fakeExecutor records calls and lets a test inject a Launch error. On a
// successful Launch it registers the run in the shared tracker so the handler's
// capacity checks and /containers reflect it, mirroring the real executor.
type fakeExecutor struct {
	mu sync.Mutex

	tracker *executor.Tracker

	launchErr      error
	launched       []executor.LaunchSpec
	killed         [][2]string // project, cardID
	stopAllArg     string
	stopAllResults []executor.StopResult
}

func (f *fakeExecutor) Launch(_ context.Context, spec executor.LaunchSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.launchErr != nil {
		return f.launchErr
	}

	f.launched = append(f.launched, spec)

	if f.tracker != nil {
		f.tracker.AddIfUnderLimit(&executor.Run{
			ContainerID: "ctr-" + spec.CardID,
			CardID:      spec.CardID,
			Project:     spec.Project,
			StartedAt:   time.Now(),
			Stdin:       &nopWriteCloser{},
		})
	}

	return nil
}

func (f *fakeExecutor) Kill(_ context.Context, project, cardID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.killed = append(f.killed, [2]string{project, cardID})

	if f.tracker != nil {
		f.tracker.Remove(project, cardID)
	}

	return nil
}

func (f *fakeExecutor) List(_ context.Context) ([]*executor.Run, error) {
	if f.tracker != nil {
		return f.tracker.List(), nil
	}

	return nil, nil
}

func (f *fakeExecutor) StopAll(_ context.Context, project string) ([]executor.StopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stopAllArg = project

	return f.stopAllResults, nil
}

func (f *fakeExecutor) CleanupOrphans(_ context.Context) error { return nil }

func (f *fakeExecutor) launchedSpecs() []executor.LaunchSpec {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]executor.LaunchSpec, len(f.launched))
	copy(out, f.launched)

	return out
}

// fakeReporter records status callbacks.
type fakeReporter struct {
	mu    sync.Mutex
	calls [][3]string // cardID, status, message
}

func (f *fakeReporter) ReportStatus(_ context.Context, cardID, _, status, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, [3]string{cardID, status, message})

	return nil
}

func (f *fakeReporter) statuses() [][3]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([][3]string, len(f.calls))
	copy(out, f.calls)

	return out
}

// fakeVerifier returns a fixed autonomous result or error.
type fakeVerifier struct {
	autonomous bool
	err        error
}

func (f *fakeVerifier) VerifyAutonomous(_ context.Context, _, _ string) (bool, error) {
	return f.autonomous, f.err
}

// nopWriteCloser captures stdin frame writes for assertions.
type nopWriteCloser struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (n *nopWriteCloser) Write(p []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.buf.Write(p)
}

func (n *nopWriteCloser) Close() error { return nil }

func (n *nopWriteCloser) String() string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.buf.String()
}

// errWriteCloser is a tracked run's stdin whose Write always fails, simulating a
// closed pipe / dead container so the /message handler hits its write-error path.
type errWriteCloser struct {
	err error
}

func (e *errWriteCloser) Write(_ []byte) (int, error) { return 0, e.err }

func (e *errWriteCloser) Close() error { return nil }

// ---- harness ----------------------------------------------------------------

type harness struct {
	server   *Server
	exec     *fakeExecutor
	tracker  *executor.Tracker
	reporter *fakeReporter
	verifier *fakeVerifier
	hub      *logbridge.Hub
}

func newHarness(t *testing.T, maxConcurrent int) *harness {
	t.Helper()

	tracker := executor.NewTracker(maxConcurrent)
	exec := &fakeExecutor{tracker: tracker}
	reporter := &fakeReporter{}
	verifier := &fakeVerifier{autonomous: true}
	hub := logbridge.NewHub()

	server := NewServer(Config{
		APIKey:        testAPIKey,
		Skew:          protocol.DefaultMaxClockSkew,
		MaxConcurrent: maxConcurrent,
		Executor:      exec,
		Tracker:       tracker,
		Hub:           hub,
		Reporter:      reporter,
		Verifier:      verifier,
		LaunchEnv: LaunchEnv{
			BaseImage: "base:image",
			MCPURL:    "http://cm:8080/mcp",
			MCPAPIKey: "cfg-mcp-key",
		},
	})

	return &harness{
		server:   server,
		exec:     exec,
		tracker:  tracker,
		reporter: reporter,
		verifier: verifier,
		hub:      hub,
	}
}

// do signs and dispatches a request through the server's mux.
func (h *harness) do(t *testing.T, method, target string, payload any) *httptest.ResponseRecorder {
	t.Helper()

	return h.doAt(t, method, target, payload, nowTS())
}

// doAt signs with an explicit timestamp so tests can issue two requests that the
// replay cache treats as distinct (a real retry carries a fresh timestamp and
// therefore a fresh signature).
func (h *harness) doAt(t *testing.T, method, target string, payload any, ts string) *httptest.ResponseRecorder {
	t.Helper()

	var body []byte

	if payload != nil {
		var err error

		body, err = json.Marshal(payload)
		require.NoError(t, err)
	}

	r := httptest.NewRequest(method, target, strings.NewReader(string(body)))
	signReq(t, r, testAPIKey, body, ts)

	w := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(w, r)

	return w
}

// addRun directly registers a tracked run with a capturable stdin, simulating a
// container the executor already launched.
func (h *harness) addRun(cardID, project string) *nopWriteCloser {
	stdin := &nopWriteCloser{}
	h.tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "ctr-" + cardID,
		CardID:      cardID,
		Project:     project,
		StartedAt:   time.Now(),
		Stdin:       stdin,
	})

	return stdin
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) protocol.ErrorResponse {
	t.Helper()

	var er protocol.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &er))

	return er
}

// ---- trigger ----------------------------------------------------------------

func TestTrigger_AcceptsAndLaunches(t *testing.T) {
	h := newHarness(t, 4)

	payload := protocol.TriggerPayload{
		CardID:      "PROJ-001",
		Project:     "proj",
		RepoURL:     "https://github.com/org/repo",
		BaseBranch:  "main",
		Model:       "some-model",
		Interactive: true,
	}

	w := h.do(t, http.MethodPost, "/trigger", payload)
	require.Equal(t, http.StatusAccepted, w.Code)

	var sr protocol.SuccessResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sr))
	assert.True(t, sr.OK)

	// Launch happens asynchronously: poll the fake.
	require.Eventually(t, func() bool {
		return len(h.exec.launchedSpecs()) == 1
	}, time.Second, 5*time.Millisecond)

	spec := h.exec.launchedSpecs()[0]
	assert.Equal(t, "PROJ-001", spec.CardID)
	assert.Equal(t, "proj", spec.Project)
	assert.Equal(t, "base:image", spec.Image)
	assert.Contains(t, spec.Env, "CM_CARD_ID=PROJ-001")
	assert.Contains(t, spec.Env, "CM_PROJECT=proj")
	assert.Contains(t, spec.Env, "CM_REPO_URL=https://github.com/org/repo")
	assert.Contains(t, spec.Env, "CM_BASE_BRANCH=main")
	assert.Contains(t, spec.Env, "CM_INTERACTIVE=true")
	assert.Contains(t, spec.Env, "CM_MODEL=some-model")
	assert.Contains(t, spec.Env, "CM_MCP_URL=http://cm:8080/mcp")
	assert.Contains(t, spec.Env, "CM_MCP_API_KEY=cfg-mcp-key")
	assert.Contains(t, spec.Env, "CM_CORRELATION_ID=PROJ-001")

	// running callback after successful launch.
	require.Eventually(t, func() bool {
		for _, c := range h.reporter.statuses() {
			if c[1] == "running" {
				return true
			}
		}

		return false
	}, time.Second, 5*time.Millisecond)
}

func TestTrigger_ImageAndMCPKeyOverride(t *testing.T) {
	h := newHarness(t, 4)

	payload := protocol.TriggerPayload{
		CardID:      "PROJ-002",
		Project:     "proj",
		RepoURL:     "https://github.com/org/repo",
		RunnerImage: "override:image",
		MCPAPIKey:   "payload-mcp-key",
	}

	w := h.do(t, http.MethodPost, "/trigger", payload)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Eventually(t, func() bool {
		return len(h.exec.launchedSpecs()) == 1
	}, time.Second, 5*time.Millisecond)

	spec := h.exec.launchedSpecs()[0]
	assert.Equal(t, "override:image", spec.Image)
	assert.Contains(t, spec.Env, "CM_MCP_API_KEY=payload-mcp-key")
}

func TestTrigger_CapacityReturns429(t *testing.T) {
	h := newHarness(t, 1)

	// Fill capacity.
	h.addRun("PROJ-001", "proj")

	payload := protocol.TriggerPayload{CardID: "PROJ-002", Project: "proj", RepoURL: "r"}
	w := h.do(t, http.MethodPost, "/trigger", payload)

	require.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, protocol.CodeLimitReached, decodeErr(t, w).Code)
	assert.Empty(t, h.exec.launchedSpecs(), "no launch on a full backend")
}

func TestTrigger_LaunchErrorReportsFailed(t *testing.T) {
	h := newHarness(t, 4)
	h.exec.launchErr = errors.New("boom")

	payload := protocol.TriggerPayload{CardID: "PROJ-003", Project: "proj", RepoURL: "r"}
	w := h.do(t, http.MethodPost, "/trigger", payload)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Eventually(t, func() bool {
		for _, c := range h.reporter.statuses() {
			if c[1] == "failed" {
				return true
			}
		}

		return false
	}, time.Second, 5*time.Millisecond)
}

// ---- kill -------------------------------------------------------------------

func TestKill_TrackedReturns200(t *testing.T) {
	h := newHarness(t, 4)
	h.addRun("PROJ-001", "proj")

	w := h.do(t, http.MethodPost, "/kill", protocol.KillPayload{CardID: "PROJ-001", Project: "proj"})

	require.Equal(t, http.StatusOK, w.Code)

	var sr protocol.SuccessResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sr))
	assert.True(t, sr.OK)

	h.exec.mu.Lock()
	require.Len(t, h.exec.killed, 1)
	assert.Equal(t, [2]string{"proj", "PROJ-001"}, h.exec.killed[0])
	h.exec.mu.Unlock()
}

func TestKill_UntrackedReturns404(t *testing.T) {
	h := newHarness(t, 4)

	w := h.do(t, http.MethodPost, "/kill", protocol.KillPayload{CardID: "ghost", Project: "proj"})

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, protocol.CodeNotFound, decodeErr(t, w).Code)
}

// ---- stop-all ---------------------------------------------------------------

func TestStopAll_ReturnsResults(t *testing.T) {
	h := newHarness(t, 4)
	h.exec.stopAllResults = []executor.StopResult{
		{Run: &executor.Run{CardID: "PROJ-001", Project: "proj"}, Err: nil},
		{Run: &executor.Run{CardID: "PROJ-002", Project: "proj"}, Err: nil},
	}

	w := h.do(t, http.MethodPost, "/stop-all", protocol.StopAllPayload{Project: "proj"})

	require.Equal(t, http.StatusOK, w.Code)

	var resp protocol.StopAllResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.True(t, resp.OK)
	assert.Equal(t, 2, resp.Total)
	assert.Equal(t, 2, resp.Stopped)
	assert.Equal(t, 0, resp.Failed)
	require.Len(t, resp.Results, 2)
	assert.Equal(t, "PROJ-001", resp.Results[0].CardID)
	assert.True(t, resp.Results[0].OK)

	h.exec.mu.Lock()
	assert.Equal(t, "proj", h.exec.stopAllArg)
	h.exec.mu.Unlock()
}

// TestStopAllReportsFailures: when one of two kills fails the handler must
// respond 207 Multi-Status with OK:false, Failed:1, Stopped:1, and a per-card
// Error on the failed entry.
func TestStopAllReportsFailures(t *testing.T) {
	h := newHarness(t, 4)
	h.exec.stopAllResults = []executor.StopResult{
		{Run: &executor.Run{CardID: "PROJ-001", Project: "proj"}, Err: nil},
		{Run: &executor.Run{CardID: "PROJ-002", Project: "proj"}, Err: errors.New("container not responding")},
	}

	w := h.do(t, http.MethodPost, "/stop-all", protocol.StopAllPayload{Project: "proj"})

	require.Equal(t, http.StatusMultiStatus, w.Code)

	var resp protocol.StopAllResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.False(t, resp.OK)
	assert.Equal(t, 2, resp.Total)
	assert.Equal(t, 1, resp.Stopped)
	assert.Equal(t, 1, resp.Failed)
	require.Len(t, resp.Results, 2)

	var failedResult *protocol.CardKillResult

	for i := range resp.Results {
		if !resp.Results[i].OK {
			failedResult = &resp.Results[i]
		}
	}

	require.NotNil(t, failedResult, "one result must have OK=false")
	assert.Equal(t, "PROJ-002", failedResult.CardID)
	assert.NotEmpty(t, failedResult.Error, "failed result must carry an Error")

	// The successful entry is counted in Stopped, not Failed.
	var okResult *protocol.CardKillResult

	for i := range resp.Results {
		if resp.Results[i].OK {
			okResult = &resp.Results[i]
		}
	}

	require.NotNil(t, okResult, "one result must have OK=true")
	assert.Equal(t, "PROJ-001", okResult.CardID)
}

// ---- message ----------------------------------------------------------------

func TestMessage_WritesFrameAnd202(t *testing.T) {
	h := newHarness(t, 4)
	stdin := h.addRun("PROJ-001", "proj")
	h.tracker.SetAwaiting("proj", "PROJ-001", true)

	payload := protocol.MessagePayload{
		CardID:    "PROJ-001",
		Project:   "proj",
		Content:   "hello worker",
		MessageID: "m1",
	}

	w := h.do(t, http.MethodPost, "/message", payload)
	require.Equal(t, http.StatusAccepted, w.Code)

	var frame struct {
		Type      string `json:"type"`
		Content   string `json:"content"`
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdin.String())), &frame))
	assert.Equal(t, "user_message", frame.Type)
	assert.Equal(t, "hello worker", frame.Content)
	assert.Equal(t, "m1", frame.MessageID)

	assert.False(t, h.tracker.Awaiting("proj", "PROJ-001"), "awaiting cleared after message")
}

func TestMessage_DuplicateReturnsCachedAck(t *testing.T) {
	h := newHarness(t, 4)
	stdin := h.addRun("PROJ-001", "proj")

	payload := protocol.MessagePayload{CardID: "PROJ-001", Project: "proj", Content: "hi", MessageID: "dup"}

	// Two distinct timestamps so the replay cache admits both; the dedup cache
	// (keyed on message_id) catches the second.
	t1 := strconv.FormatInt(time.Now().Add(-2*time.Second).Unix(), 10)
	t2 := strconv.FormatInt(time.Now().Unix(), 10)

	w1 := h.doAt(t, http.MethodPost, "/message", payload, t1)
	require.Equal(t, http.StatusAccepted, w1.Code)

	first := stdin.String()

	w2 := h.doAt(t, http.MethodPost, "/message", payload, t2)
	require.Equal(t, http.StatusOK, w2.Code, "duplicate ack is 200, not 202")

	assert.Equal(t, first, stdin.String(), "duplicate must not re-write stdin")
}

func TestMessage_UntrackedReturns404(t *testing.T) {
	h := newHarness(t, 4)

	w := h.do(t, http.MethodPost, "/message",
		protocol.MessagePayload{CardID: "ghost", Project: "proj", Content: "x", MessageID: "m"})

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, protocol.CodeNotFound, decodeErr(t, w).Code)
}

// TestMessage_WriteFailureRetryDelivers proves the dedup cache is not poisoned
// by a failed stdin write: the first attempt 500s, a same-message_id retry
// against a now-healthy run delivers the frame (no false duplicate ack).
func TestMessage_WriteFailureRetryDelivers(t *testing.T) {
	h := newHarness(t, 4)

	// First attempt: the tracked run's stdin errors on write.
	errStdin := &errWriteCloser{err: errors.New("pipe closed")}
	h.tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "ctr-PROJ-001",
		CardID:      "PROJ-001",
		Project:     "proj",
		StartedAt:   time.Now(),
		Stdin:       errStdin,
	})

	payload := protocol.MessagePayload{CardID: "PROJ-001", Project: "proj", Content: "hi", MessageID: "m1"}

	t1 := strconv.FormatInt(time.Now().Add(-2*time.Second).Unix(), 10)
	t2 := strconv.FormatInt(time.Now().Unix(), 10)

	w1 := h.doAt(t, http.MethodPost, "/message", payload, t1)
	require.Equal(t, http.StatusInternalServerError, w1.Code)
	assert.Equal(t, protocol.CodeInternal, decodeErr(t, w1).Code)

	// The container is replaced by a healthy one (same card) before the retry.
	h.tracker.Remove("proj", "PROJ-001")
	healthy := h.addRun("PROJ-001", "proj")

	// Retry with the SAME message_id: must NOT be deduped — it must deliver.
	w2 := h.doAt(t, http.MethodPost, "/message", payload, t2)
	require.Equal(t, http.StatusAccepted, w2.Code, "retry after failed write must deliver, not false-ack")

	var frame struct {
		Type      string `json:"type"`
		Content   string `json:"content"`
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(healthy.String())), &frame))
	assert.Equal(t, "user_message", frame.Type)
	assert.Equal(t, "hi", frame.Content)
	assert.Equal(t, "m1", frame.MessageID)
}

// TestMessage_NotFoundDoesNotPoisonDedup proves a 404 (untracked run) does not
// record the message_id: once the run appears, a same-message_id retry delivers.
func TestMessage_NotFoundDoesNotPoisonDedup(t *testing.T) {
	h := newHarness(t, 4)

	payload := protocol.MessagePayload{CardID: "PROJ-001", Project: "proj", Content: "hi", MessageID: "m1"}

	t1 := strconv.FormatInt(time.Now().Add(-2*time.Second).Unix(), 10)
	t2 := strconv.FormatInt(time.Now().Unix(), 10)

	// No run tracked yet → 404.
	w1 := h.doAt(t, http.MethodPost, "/message", payload, t1)
	require.Equal(t, http.StatusNotFound, w1.Code)

	// The container now exists; the retry with the same message_id must deliver.
	stdin := h.addRun("PROJ-001", "proj")

	w2 := h.doAt(t, http.MethodPost, "/message", payload, t2)
	require.Equal(t, http.StatusAccepted, w2.Code, "404 must not poison dedup; retry delivers")
	assert.Contains(t, stdin.String(), `"message_id":"m1"`)
}

// TestMessage_DuplicateAfterDeliveryAcksWithoutSecondFrame confirms the happy
// path still dedups: a delivered message is acked once with a frame, and a
// retry acks (200) without writing a second frame.
func TestMessage_DuplicateAfterDeliveryAcksWithoutSecondFrame(t *testing.T) {
	h := newHarness(t, 4)
	stdin := h.addRun("PROJ-001", "proj")

	payload := protocol.MessagePayload{CardID: "PROJ-001", Project: "proj", Content: "hi", MessageID: "dup"}

	t1 := strconv.FormatInt(time.Now().Add(-2*time.Second).Unix(), 10)
	t2 := strconv.FormatInt(time.Now().Unix(), 10)

	w1 := h.doAt(t, http.MethodPost, "/message", payload, t1)
	require.Equal(t, http.StatusAccepted, w1.Code)

	first := stdin.String()
	require.Contains(t, first, `"message_id":"dup"`)

	w2 := h.doAt(t, http.MethodPost, "/message", payload, t2)
	require.Equal(t, http.StatusOK, w2.Code, "duplicate ack is 200, not 202")

	assert.Equal(t, first, stdin.String(), "delivered duplicate must not re-write stdin")
}

// ---- promote ----------------------------------------------------------------

func TestPromote_AutonomousWritesFrame(t *testing.T) {
	h := newHarness(t, 4)
	stdin := h.addRun("PROJ-001", "proj")
	h.verifier.autonomous = true

	w := h.do(t, http.MethodPost, "/promote", protocol.PromotePayload{CardID: "PROJ-001", Project: "proj"})

	require.Equal(t, http.StatusAccepted, w.Code)
	assert.Contains(t, stdin.String(), `"type":"promote"`)
}

func TestPromote_NotAutonomousReturns409(t *testing.T) {
	h := newHarness(t, 4)
	stdin := h.addRun("PROJ-001", "proj")
	h.verifier.autonomous = false

	w := h.do(t, http.MethodPost, "/promote", protocol.PromotePayload{CardID: "PROJ-001", Project: "proj"})

	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, protocol.CodeConflict, decodeErr(t, w).Code)
	assert.Empty(t, stdin.String(), "no frame on a non-autonomous card")
}

func TestPromote_VerifyErrorReturns502(t *testing.T) {
	h := newHarness(t, 4)
	stdin := h.addRun("PROJ-001", "proj")
	h.verifier.err = errors.New("upstream down")

	w := h.do(t, http.MethodPost, "/promote", protocol.PromotePayload{CardID: "PROJ-001", Project: "proj"})

	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.Equal(t, protocol.CodeUpstreamFailure, decodeErr(t, w).Code)
	assert.Empty(t, stdin.String(), "fail closed: no frame when verification errors")
}

func TestPromote_UntrackedReturns404(t *testing.T) {
	h := newHarness(t, 4)

	w := h.do(t, http.MethodPost, "/promote", protocol.PromotePayload{CardID: "ghost", Project: "proj"})

	require.Equal(t, http.StatusNotFound, w.Code)
}

// ---- end-session ------------------------------------------------------------

func TestEndSession_TrackedWritesFrame(t *testing.T) {
	h := newHarness(t, 4)
	stdin := h.addRun("PROJ-001", "proj")

	w := h.do(t, http.MethodPost, "/end-session", protocol.EndSessionPayload{CardID: "PROJ-001", Project: "proj"})

	require.Equal(t, http.StatusAccepted, w.Code)
	assert.Contains(t, stdin.String(), `"type":"end_session"`)
}

func TestEndSession_UntrackedReturns200(t *testing.T) {
	h := newHarness(t, 4)

	w := h.do(t, http.MethodPost, "/end-session", protocol.EndSessionPayload{CardID: "ghost", Project: "proj"})

	require.Equal(t, http.StatusOK, w.Code, "idempotent: nothing to end is success")
}

// ---- containers -------------------------------------------------------------

func TestContainers_ListsTrackedRuns(t *testing.T) {
	h := newHarness(t, 4)
	h.addRun("PROJ-001", "proj")

	w := h.do(t, http.MethodGet, "/containers", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp protocol.ListContainersResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.True(t, resp.OK)
	require.Len(t, resp.Containers, 1)

	item := resp.Containers[0]
	assert.Equal(t, "ctr-PROJ-001", item.ContainerID)
	assert.Equal(t, "PROJ-001", item.CardID)
	assert.Equal(t, "proj", item.Project)
	assert.Equal(t, "running", item.State)
	assert.True(t, item.Tracked)

	_, err := time.Parse(time.RFC3339, item.StartedAt)
	assert.NoError(t, err, "StartedAt must be RFC3339")
}

// ---- health / readyz --------------------------------------------------------

func TestHealth_Unauthenticated(t *testing.T) {
	h := newHarness(t, 7)
	h.addRun("PROJ-001", "proj")

	// No signing — /health is unauthenticated.
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)

	var hr protocol.HealthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &hr))

	assert.True(t, hr.OK)
	assert.Equal(t, 1, hr.RunningContainers)
	assert.Equal(t, 7, hr.MaxConcurrent)
}

func TestReadyz_OKAndDraining(t *testing.T) {
	h := newHarness(t, 4)

	r1 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w1 := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(w1, r1)
	require.Equal(t, http.StatusOK, w1.Code)

	h.server.draining.Store(true)

	r2 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w2 := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(w2, r2)
	require.Equal(t, http.StatusServiceUnavailable, w2.Code)
	assert.Contains(t, w2.Body.String(), "draining")
}

// ---- drain gate on mutating routes -----------------------------------------

func TestDrainGate_TriggerRefusedWhileDraining(t *testing.T) {
	h := newHarness(t, 4)
	h.server.draining.Store(true)

	w := h.do(t, http.MethodPost, "/trigger",
		protocol.TriggerPayload{CardID: "PROJ-001", Project: "proj", RepoURL: "r"})

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, protocol.CodeDraining, decodeErr(t, w).Code)
	assert.Empty(t, h.exec.launchedSpecs())
}

// ---- budget env threading ---------------------------------------------------

// newHarnessWithBudget builds a Server with the budget knobs set.
func newHarnessWithBudget(t *testing.T, maxCardCost, headroom float64) *harness {
	t.Helper()

	tracker := executor.NewTracker(4)
	exec := &fakeExecutor{tracker: tracker}
	reporter := &fakeReporter{}
	verifier := &fakeVerifier{autonomous: true}
	hub := logbridge.NewHub()

	server := NewServer(Config{
		APIKey:        testAPIKey,
		Skew:          protocol.DefaultMaxClockSkew,
		MaxConcurrent: 4,
		Executor:      exec,
		Tracker:       tracker,
		Hub:           hub,
		Reporter:      reporter,
		Verifier:      verifier,
		LaunchEnv: LaunchEnv{
			BaseImage:             "base:image",
			MCPURL:                "http://cm:8080/mcp",
			MCPAPIKey:             "cfg-mcp-key",
			MaxCardCost:           maxCardCost,
			SelectorPriceHeadroom: headroom,
		},
	})

	return &harness{
		server:   server,
		exec:     exec,
		tracker:  tracker,
		reporter: reporter,
		verifier: verifier,
		hub:      hub,
	}
}

func TestBuildLaunchSpec_BudgetEnvEmitted(t *testing.T) {
	// When MaxCardCost and SelectorPriceHeadroom are non-zero, both CMX_* vars
	// must appear in the launched container env.
	h := newHarnessWithBudget(t, 5.0, 1.5)

	payload := protocol.TriggerPayload{
		CardID:  "PROJ-010",
		Project: "proj",
		RepoURL: "https://github.com/org/repo",
	}

	w := h.do(t, http.MethodPost, "/trigger", payload)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Eventually(t, func() bool {
		return len(h.exec.launchedSpecs()) == 1
	}, time.Second, 5*time.Millisecond)

	spec := h.exec.launchedSpecs()[0]
	assert.Contains(t, spec.Env, "CMX_MAX_CARD_COST=5", "max_card_cost must be formatted without trailing decimal")
	assert.Contains(t, spec.Env, "CMX_SELECTOR_PRICE_HEADROOM=1.5", "selector_price_headroom must appear")
}

// ---- task-skills threading --------------------------------------------------

type fakeSkillsResolver struct {
	dir string
	err error
}

func (f fakeSkillsResolver) Resolve(context.Context) (string, error) { return f.dir, f.err }

func newSkillsTestServer(resolver SkillsResolver) *Server {
	return NewServer(Config{
		APIKey:         "k",
		Executor:       &fakeExecutor{},
		Tracker:        executor.NewTracker(1),
		SkillsResolver: resolver,
		LaunchEnv:      LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
	})
}

func sliceP(s ...string) *[]string { return &s }

func TestBuildLaunchSpecThreadsSkillsSubset(t *testing.T) {
	s := newSkillsTestServer(fakeSkillsResolver{dir: "/host/skills"})

	spec := s.buildLaunchSpec(protocol.TriggerPayload{
		CardID: "C1", Project: "p", TaskSkills: sliceP("go-development", "documentation"),
	}, "corr", "/host/skills")

	assert.Equal(t, "/host/skills", spec.SkillsHostDir)
	assert.Contains(t, spec.Env, "CMX_TASK_SKILLS_DIR=/run/cm-skills")
	assert.Contains(t, spec.Env, "CM_TASK_SKILLS_SET=1")
	assert.Contains(t, spec.Env, "CM_TASK_SKILLS=go-development,documentation")
}

func TestBuildLaunchSpecNilSubsetOffersAll(t *testing.T) {
	s := newSkillsTestServer(fakeSkillsResolver{dir: "/host/skills"})

	spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p"}, "corr", "/host/skills")

	assert.Equal(t, "/host/skills", spec.SkillsHostDir)
	assert.Contains(t, spec.Env, "CMX_TASK_SKILLS_DIR=/run/cm-skills")

	for _, e := range spec.Env {
		assert.NotEqual(t, "CM_TASK_SKILLS_SET=1", e, "nil subset must not set CM_TASK_SKILLS_SET")
	}
}

func TestBuildLaunchSpecNoSkillsWhenDirEmpty(t *testing.T) {
	s := newSkillsTestServer(fakeSkillsResolver{dir: "", err: errors.New("unreachable")})

	spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p", TaskSkills: sliceP("go-development")}, "corr", "")

	assert.Empty(t, spec.SkillsHostDir)

	for _, e := range spec.Env {
		assert.NotContains(t, e, "CMX_TASK_SKILLS_DIR")
		assert.NotContains(t, e, "CM_TASK_SKILLS")
	}
}

func TestValidateTaskSkills(t *testing.T) {
	require.NoError(t, validateTaskSkills(nil))
	require.NoError(t, validateTaskSkills([]string{"go-development", "test-driven-development"}))
	require.Error(t, validateTaskSkills([]string{"../etc"}), "path-ish names rejected")
	require.Error(t, validateTaskSkills([]string{"UPPER"}), "uppercase rejected")

	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = "a"
	}

	require.Error(t, validateTaskSkills(tooMany), "more than 64 entries rejected")
}

func TestBuildLaunchSpec_BudgetEnvOmittedWhenZero(t *testing.T) {
	// When MaxCardCost and SelectorPriceHeadroom are zero, the CMX_* vars must
	// be omitted so workers apply their own defaults.
	h := newHarnessWithBudget(t, 0, 0)

	payload := protocol.TriggerPayload{
		CardID:  "PROJ-011",
		Project: "proj",
		RepoURL: "https://github.com/org/repo",
	}

	w := h.do(t, http.MethodPost, "/trigger", payload)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Eventually(t, func() bool {
		return len(h.exec.launchedSpecs()) == 1
	}, time.Second, 5*time.Millisecond)

	spec := h.exec.launchedSpecs()[0]

	for _, e := range spec.Env {
		assert.NotContains(t, e, "CMX_MAX_CARD_COST", "zero max_card_cost must not be emitted")
		assert.NotContains(t, e, "CMX_SELECTOR_PRICE_HEADROOM", "zero headroom must not be emitted")
	}
}

func TestBuildLaunchSpec_CompactionEnvEmittedWhenEnabled(t *testing.T) {
	// When compaction is enabled, all three CMX_COMPACTION_* vars must reach the
	// container env so the worker opts the harness loop into in-window compaction.
	s := NewServer(Config{
		APIKey:   "k",
		Executor: &fakeExecutor{},
		Tracker:  executor.NewTracker(1),
		LaunchEnv: LaunchEnv{
			BaseImage:                 "img",
			MCPURL:                    "http://mcp",
			CompactionEnabled:         true,
			CompactionThreshold:       0.8,
			CompactionKeepRecentTurns: 4,
		},
	})

	spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p"}, "corr", "")

	assert.Contains(t, spec.Env, "CMX_COMPACTION_ENABLED=true")
	assert.Contains(t, spec.Env, "CMX_COMPACTION_THRESHOLD=0.8")
	assert.Contains(t, spec.Env, "CMX_COMPACTION_KEEP_RECENT_TURNS=4")
}

// TestBuildLaunchSpecEmitsReasoningEffort pins the CMX_REASONING_EFFORT env
// threading: when LaunchEnv.ReasoningEffort is set the var must appear in the
// container env; when empty (the default) it must be absent so workers use their
// own default (no reasoning overhead).
func TestBuildLaunchSpecEmitsReasoningEffort(t *testing.T) {
	t.Run("emits when set", func(t *testing.T) {
		s := NewServer(Config{
			APIKey:   "k",
			Executor: &fakeExecutor{},
			Tracker:  executor.NewTracker(1),
			LaunchEnv: LaunchEnv{
				BaseImage:       "img",
				MCPURL:          "http://mcp",
				ReasoningEffort: "high",
			},
		})

		spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p"}, "corr", "")

		assert.Contains(t, spec.Env, "CMX_REASONING_EFFORT=high")
	})

	t.Run("omits when empty", func(t *testing.T) {
		s := NewServer(Config{
			APIKey:    "k",
			Executor:  &fakeExecutor{},
			Tracker:   executor.NewTracker(1),
			LaunchEnv: LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
		})

		spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p"}, "corr", "")

		for _, e := range spec.Env {
			assert.NotContains(t, e, "CMX_REASONING_EFFORT", "empty effort must not be emitted")
		}
	})
}

func TestBuildLaunchSpec_CompactionEnvOmittedWhenDisabled(t *testing.T) {
	// Disabled compaction (the default) emits no CMX_COMPACTION_* vars, so the
	// worker keeps the hard context_limit stop — behavior-neutral.
	s := NewServer(Config{
		APIKey:    "k",
		Executor:  &fakeExecutor{},
		Tracker:   executor.NewTracker(1),
		LaunchEnv: LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
	})

	spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p"}, "corr", "")

	for _, e := range spec.Env {
		assert.NotContains(t, e, "CMX_COMPACTION_", "disabled compaction must emit no CMX_COMPACTION_* env")
	}
}
