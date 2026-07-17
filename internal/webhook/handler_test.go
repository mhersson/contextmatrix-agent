package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-backendkit/frames"
	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- signing helpers --------------------------------------------------------

const testAPIKey = "test-secret-key"

// signReq stamps r with a valid HMAC signature for the given key over
// METHOD\nURI\nTS.BODY using the supplied timestamp.
func signReq(t *testing.T, r *http.Request, key string, body []byte, ts string) {
	t.Helper()

	sig := protocol.SignPayloadWithTimestamp(key, r.Method, r.URL.RequestURI(), body, ts)
	r.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	r.Header.Set(protocol.TimestampHeader, ts)
}

// nowTS returns the current Unix second as a string.
func nowTS() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

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

	// Mirror the real executor's fail-closed admission: a duplicate card key (or
	// a full tracker) is refused with ErrCapacity and the launch never happens.
	if f.tracker != nil {
		admitted := f.tracker.AddIfUnderLimit(&executor.Run{
			ContainerID: "ctr-" + spec.CardID,
			CardID:      spec.CardID,
			Project:     spec.Project,
			StartedAt:   time.Now(),
			Stdin:       &nopWriteCloser{},
		})
		if !admitted {
			return executor.ErrCapacity
		}
	}

	f.launched = append(f.launched, spec)

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

// quietLogger silences slog output so tests that exercise the deprecation
// warning and launch/provision error paths keep pristine output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCredentials records per-run credential provisioning and teardown, and can
// inject a Provision error. It satisfies CredentialProvisioner.
type fakeCredentials struct {
	mu sync.Mutex

	provisionErr error
	provisions   []provisionCall
	teardowns    [][2]string // project, cardID
}

type provisionCall struct {
	project, cardID, token, expiresAt string
	endpoint                          secrets.EndpointSecrets
}

func (f *fakeCredentials) HostDir(project, cardID string) string {
	return "/secrets/runs/" + project + "/" + cardID
}

func (f *fakeCredentials) Provision(project, cardID, token, expiresAt string, endpoint secrets.EndpointSecrets) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.provisionErr != nil {
		return f.provisionErr
	}

	f.provisions = append(f.provisions, provisionCall{project, cardID, token, expiresAt, endpoint})

	return nil
}

func (f *fakeCredentials) Teardown(project, cardID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.teardowns = append(f.teardowns, [2]string{project, cardID})
}

func (f *fakeCredentials) provisionCalls() []provisionCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]provisionCall, len(f.provisions))
	copy(out, f.provisions)

	return out
}

func (f *fakeCredentials) teardownCalls() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([][2]string, len(f.teardowns))
	copy(out, f.teardowns)

	return out
}

// fakeImageLister returns a fixed set of image summaries or an error. It
// satisfies webhookcore.ImageLister.
type fakeImageLister struct {
	summaries []webhookcore.ImageSummary
	err       error
}

func (f *fakeImageLister) ListImages(_ context.Context) ([]webhookcore.ImageSummary, error) {
	return f.summaries, f.err
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
	images   *fakeImageLister
	draining *atomic.Bool
}

func newHarness(t *testing.T, maxConcurrent int) *harness {
	t.Helper()

	tracker := executor.NewTracker(maxConcurrent)
	exec := &fakeExecutor{tracker: tracker}
	reporter := &fakeReporter{}
	verifier := &fakeVerifier{autonomous: true}
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, nil)
	images := &fakeImageLister{}
	draining := &atomic.Bool{}

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
		Images:           images,
		ImageListFilters: []string{"contextmatrix-agent"},
		Draining:         draining,
	})

	return &harness{
		server:   server,
		exec:     exec,
		tracker:  tracker,
		reporter: reporter,
		verifier: verifier,
		hub:      hub,
		images:   images,
		draining: draining,
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

// provisionedPayload returns a trigger payload carrying both CM-provisioned
// credentials, satisfying the fail-closed guards in admitAndLaunch. Tests that
// exercise the guards build credential-less payloads inline on purpose.
func provisionedPayload(cardID string) protocol.TriggerPayload {
	return protocol.TriggerPayload{
		CardID:   cardID,
		Project:  "proj",
		RepoURL:  "https://github.com/org/repo",
		GitToken: "cm-git-token",
		LLMEndpoint: &protocol.LLMEndpoint{
			Type:    "openai",
			BaseURL: "https://llm.example/v1",
			APIKey:  "cm-llm-key",
		},
	}
}

// ---- trigger ----------------------------------------------------------------

func TestTrigger_AcceptsAndLaunches(t *testing.T) {
	h := newHarness(t, 4)

	payload := provisionedPayload("PROJ-001")
	payload.BaseBranch = "main"
	payload.Model = "some-model"
	payload.Interactive = true

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

	payload := provisionedPayload("PROJ-002")
	payload.WorkerImage = "override:image"
	payload.MCPAPIKey = "payload-mcp-key"

	w := h.do(t, http.MethodPost, "/trigger", payload)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Eventually(t, func() bool {
		return len(h.exec.launchedSpecs()) == 1
	}, time.Second, 5*time.Millisecond)

	spec := h.exec.launchedSpecs()[0]
	assert.Equal(t, "override:image", spec.Image)
	assert.Contains(t, spec.Env, "CM_MCP_API_KEY=payload-mcp-key")
}

// TestTrigger_WorkerImageWireTag pins the decode side of the protocol v0.8.0
// rename: a raw trigger body carrying "worker_image" (not the pre-rename
// "runner_image") must drive the launch spec's image. Building the body from a
// raw JSON string rather than marshalling a TriggerPayload is the point - it
// exercises the actual wire tag, which a shared-struct round-trip cannot.
func TestTrigger_WorkerImageWireTag(t *testing.T) {
	h := newHarness(t, 4)

	const body = `{` +
		`"card_id":"PROJ-002",` +
		`"project":"proj",` +
		`"repo_url":"https://github.com/org/repo",` +
		`"git_token":"cm-git-token",` +
		`"worker_image":"override:image",` +
		`"llm_endpoint":{"type":"openai","base_url":"https://llm.example/v1","api_key":"cm-llm-key"}` +
		`}`

	ts := nowTS()
	r := httptest.NewRequest(http.MethodPost, "/trigger", strings.NewReader(body))
	signReq(t, r, testAPIKey, []byte(body), ts)

	w := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(w, r)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Eventually(t, func() bool {
		return len(h.exec.launchedSpecs()) == 1
	}, time.Second, 5*time.Millisecond)

	assert.Equal(t, "override:image", h.exec.launchedSpecs()[0].Image)
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

	w := h.do(t, http.MethodPost, "/trigger", provisionedPayload("PROJ-003"))
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

// TestMessage_PublishesUnredactedUserEntry pins publishUser's only remaining
// coverage: a delivered message publishes a Type "user" LogEntry to the hub
// carrying the payload's project/card/content verbatim - including content
// that looks like a secret, since user-authored content is never redacted
// (only worker-emitted content passes through the redactor).
func TestMessage_PublishesUnredactedUserEntry(t *testing.T) {
	h := newHarness(t, 4)
	h.addRun("PROJ-001", "proj")

	_, ch := h.hub.Subscribe("")

	const secretLooking = "here is my token: sk-live-abc123def456, do not redact it"

	payload := protocol.MessagePayload{
		CardID:    "PROJ-001",
		Project:   "proj",
		Content:   secretLooking,
		MessageID: "m1",
	}

	w := h.do(t, http.MethodPost, "/message", payload)
	require.Equal(t, http.StatusAccepted, w.Code)

	select {
	case entry := <-ch:
		assert.Equal(t, "user", entry.Type)
		assert.Equal(t, "proj", entry.Project)
		assert.Equal(t, "PROJ-001", entry.CardID)
		assert.Equal(t, secretLooking, entry.Content, "user content must be published verbatim, never redacted")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the published user log entry")
	}
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

// TestMessage_OversizedContentRejected proves a frame that would exceed
// frames.MaxLine is refused before it reaches the container: the handler
// maps frames.ErrFrameTooLarge to 413 CodeTooLarge and stdin stays empty.
func TestMessage_OversizedContentRejected(t *testing.T) {
	h := newHarness(t, 4)
	stdin := h.addRun("PROJ-001", "proj")

	payload := protocol.MessagePayload{
		CardID:    "PROJ-001",
		Project:   "proj",
		Content:   strings.Repeat("x", frames.MaxLine),
		MessageID: "m1",
	}

	w := h.do(t, http.MethodPost, "/message", payload)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Equal(t, protocol.CodeTooLarge, decodeErr(t, w).Code)
	assert.Empty(t, stdin.String(), "oversized frame must not reach the container stdin")
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

	// Retry with the SAME message_id: must NOT be deduped - it must deliver.
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

// ---- images -----------------------------------------------------------------

func TestImages_FiltersPerTagAndMaps(t *testing.T) {
	h := newHarness(t, 4)
	h.images.summaries = []webhookcore.ImageSummary{
		{
			Tags:      []string{"contextmatrix-agent-worker:go-node"},
			Digests:   []string{"contextmatrix-agent-worker@sha256:abc"},
			CreatedAt: 1750000000,
			SizeBytes: 2_560_000_000,
		},
		{Tags: []string{"harbor.example/apps/contextmatrix:latest"}}, // no matching tag: dropped
		{Tags: []string{"contextmatrix-agent-worker:dev", "unrelated:tag"}},
	}

	w := h.do(t, http.MethodGet, "/images", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp protocol.ListImagesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.True(t, resp.OK)
	require.Len(t, resp.Images, 2)
	assert.Equal(t, []string{"contextmatrix-agent-worker:go-node"}, resp.Images[0].Tags)
	assert.Equal(t, []string{"contextmatrix-agent-worker@sha256:abc"}, resp.Images[0].Digests)
	assert.Equal(t, int64(1750000000), resp.Images[0].Created)
	assert.Equal(t, int64(2_560_000_000), resp.Images[0].Size)
	// Non-matching tag pruned from the mixed image.
	assert.Equal(t, []string{"contextmatrix-agent-worker:dev"}, resp.Images[1].Tags)
}

func TestImages_DockerErrorReturns502Generic(t *testing.T) {
	h := newHarness(t, 4)
	h.images.err = errors.New("daemon exploded: secret detail")

	w := h.do(t, http.MethodGet, "/images", nil)
	require.Equal(t, http.StatusBadGateway, w.Code)

	var resp protocol.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, protocol.CodeUpstreamFailure, resp.Code)
	assert.NotContains(t, resp.Message, "secret detail")
}

func TestImages_RequiresSignature(t *testing.T) {
	h := newHarness(t, 4)

	r := httptest.NewRequest(http.MethodGet, "/images", nil)
	w := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestImages_NilListerReturns500(t *testing.T) {
	server := NewServer(Config{APIKey: testAPIKey})

	r := httptest.NewRequest(http.MethodGet, "/images", nil)
	signReq(t, r, testAPIKey, nil, nowTS())

	w := httptest.NewRecorder()
	server.Routes().ServeHTTP(w, r)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ---- health / readyz --------------------------------------------------------

func TestHealth_Unauthenticated(t *testing.T) {
	h := newHarness(t, 7)
	h.addRun("PROJ-001", "proj")

	// No signing - /health is unauthenticated.
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

	h.draining.Store(true)

	r2 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w2 := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(w2, r2)
	require.Equal(t, http.StatusServiceUnavailable, w2.Code)
	assert.Contains(t, w2.Body.String(), "draining")
}

// ---- drain gate on mutating routes -----------------------------------------

func TestDrainGate_TriggerRefusedWhileDraining(t *testing.T) {
	h := newHarness(t, 4)
	h.draining.Store(true)

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
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, nil)

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

	w := h.do(t, http.MethodPost, "/trigger", provisionedPayload("PROJ-010"))
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Eventually(t, func() bool {
		return len(h.exec.launchedSpecs()) == 1
	}, time.Second, 5*time.Millisecond)

	spec := h.exec.launchedSpecs()[0]
	assert.Contains(t, spec.Env, "CMX_MAX_CARD_COST=5", "max_card_cost must be formatted without trailing decimal")
	assert.Contains(t, spec.Env, "CMX_SELECTOR_PRICE_HEADROOM=1.5", "selector_price_headroom must appear")
}

func TestBuildLaunchSpec_VerifyEnvEmitted(t *testing.T) {
	// A verify config on the payload is JSON-marshalled into CMX_VERIFY.
	h := newHarness(t, 4)

	payload := provisionedPayload("PROJ-011")
	payload.Verify = &protocol.VerifyConfig{
		Command:        "cargo test",
		TimeoutSeconds: 900,
		Env:            []string{"JAVA_HOME"},
	}

	w := h.do(t, http.MethodPost, "/trigger", payload)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Eventually(t, func() bool {
		return len(h.exec.launchedSpecs()) == 1
	}, time.Second, 5*time.Millisecond)

	spec := h.exec.launchedSpecs()[0]
	assert.Contains(t, spec.Env,
		`CMX_VERIFY={"command":"cargo test","timeout_seconds":900,"env":["JAVA_HOME"]}`)
}

func TestBuildLaunchSpec_VerifyEnvAbsentWhenNil(t *testing.T) {
	// No verify config -> no CMX_VERIFY var at all (consumers detect instead).
	h := newHarness(t, 4)

	w := h.do(t, http.MethodPost, "/trigger", provisionedPayload("PROJ-012"))
	require.Equal(t, http.StatusAccepted, w.Code)

	require.Eventually(t, func() bool {
		return len(h.exec.launchedSpecs()) == 1
	}, time.Second, 5*time.Millisecond)

	spec := h.exec.launchedSpecs()[0]
	for _, e := range spec.Env {
		assert.NotContains(t, e, "CMX_VERIFY=")
	}
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

func sliceP(s ...string) *[]string { return new(s) }

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

	w := h.do(t, http.MethodPost, "/trigger", provisionedPayload("PROJ-011"))
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
	// worker keeps the hard context_limit stop - behavior-neutral.
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

// TestBuildLaunchSpec_BestOfN pins the CM_BEST_OF_N env emission and pids-limit
// scaling: BestOfN > 1 emits the env var and multiplies PidsLimit by N;
// BestOfN <= 1 (the default) emits nothing and leaves PidsLimit unchanged. The
// memory limit is intentionally left alone here - candidate verifies run
// serially in the judge phase.
func TestBuildLaunchSpec_BestOfN(t *testing.T) {
	newServerWithPids := func(pids int64) *Server {
		return NewServer(Config{
			APIKey:    "k",
			Executor:  &fakeExecutor{},
			Tracker:   executor.NewTracker(1),
			LaunchEnv: LaunchEnv{BaseImage: "img", MCPURL: "http://mcp", PidsLimit: pids},
		})
	}

	t.Run("emits env and scales pids when greater than 1", func(t *testing.T) {
		s := newServerWithPids(128)

		spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p", BestOfN: 3}, "corr", "")

		assert.Contains(t, spec.Env, "CM_BEST_OF_N=3")
		assert.Equal(t, int64(384), spec.PidsLimit, "pids limit must scale by N")
	})

	t.Run("omits env and leaves pids unchanged when zero", func(t *testing.T) {
		s := newServerWithPids(128)

		spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p", BestOfN: 0}, "corr", "")

		for _, e := range spec.Env {
			assert.NotContains(t, e, "CM_BEST_OF_N", "BestOfN 0 must not emit the env var")
		}

		assert.Equal(t, int64(128), spec.PidsLimit)
	})

	t.Run("does not scale an unset (zero) pids limit", func(t *testing.T) {
		s := newServerWithPids(0)

		spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p", BestOfN: 3}, "corr", "")

		assert.Contains(t, spec.Env, "CM_BEST_OF_N=3")
		assert.Equal(t, int64(0), spec.PidsLimit, "an unset pids limit (uncapped) must stay unset")
	})
}

// ---- CM-provisioned credentials -----------------------------------------------

// TestBuildLaunchSpec_CredentialDeliveryMatrix pins that credentials never ride
// plain container env, whatever the payload shape: the git token and LLM values
// travel only via the per-run secrets file the credential provisioner mounts.
// Payloads missing either credential still get a spec built (buildLaunchSpec
// runs pre-guard) but are rejected by admitAndLaunch before launch.
func TestBuildLaunchSpec_CredentialDeliveryMatrix(t *testing.T) {
	llm := &protocol.LLMEndpoint{Type: "openai", BaseURL: "https://llm.example/v1", APIKey: "cm-llm-key"}

	tests := []struct {
		name    string
		payload protocol.TriggerPayload
	}{
		{
			name:    "git token and llm endpoint: both ride the per-run file",
			payload: protocol.TriggerPayload{CardID: "C1", Project: "p", GitToken: "cm-git-token", LLMEndpoint: llm},
		},
		{
			name:    "git token only (guard-rejected pre-launch)",
			payload: protocol.TriggerPayload{CardID: "C1", Project: "p", GitToken: "cm-git-token"},
		},
		{
			name:    "llm endpoint only (guard-rejected pre-launch)",
			payload: protocol.TriggerPayload{CardID: "C1", Project: "p", LLMEndpoint: llm},
		},
		{
			name:    "neither (guard-rejected pre-launch)",
			payload: protocol.TriggerPayload{CardID: "C1", Project: "p"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{
				APIKey:      "k",
				Executor:    &fakeExecutor{},
				Tracker:     executor.NewTracker(1),
				Credentials: &fakeCredentials{},
				LaunchEnv:   LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
				Logger:      quietLogger(),
			})

			spec := s.buildLaunchSpec(tc.payload, "corr", "")

			for _, e := range spec.Env {
				assert.NotContains(t, e, "CM_GIT_TOKEN", "git token must never leak into plain env")
				assert.NotContains(t, e, "LLM_API_KEY", "llm key must not leak into plain env")
				assert.NotContains(t, e, "LLM_BASE_URL", "llm base url must not leak into plain env")
				assert.NotContains(t, e, "LLM_TYPE", "llm type must not leak into plain env")
			}
		})
	}
}

// TestBuildLaunchSpec_PerRunMountWhenTokenPresent verifies the mount routing: a
// payload with a git token mounts the per-run directory; without one it mounts
// the shared secrets dir (a spec the launch guard then rejects pre-launch).
func TestBuildLaunchSpec_PerRunMountWhenTokenPresent(t *testing.T) {
	creds := &fakeCredentials{}

	s := NewServer(Config{
		APIKey:      "k",
		Executor:    &fakeExecutor{},
		Tracker:     executor.NewTracker(1),
		Credentials: creds,
		LaunchEnv:   LaunchEnv{BaseImage: "img", MCPURL: "http://mcp", SecretsHostDir: "/secrets/shared"},
		Logger:      quietLogger(),
	})

	withToken := s.buildLaunchSpec(protocol.TriggerPayload{
		CardID:   "C1",
		Project:  "p",
		GitToken: "cm-git-token",
	}, "corr", "")
	assert.Equal(t, creds.HostDir("p", "C1"), withToken.SecretsHostDir,
		"a payload token mounts the per-run dir")

	withoutToken := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C2", Project: "p"}, "corr", "")
	assert.Equal(t, "/secrets/shared", withoutToken.SecretsHostDir,
		"no payload token mounts the shared dir")
}

// TestBuildLaunchSpec_SharedMountWhenNoProvisioner verifies that without a
// credential provisioner wired, every run mounts the shared dir even if a token
// is present (defensive: the provisioner is the only per-run writer).
func TestBuildLaunchSpec_SharedMountWhenNoProvisioner(t *testing.T) {
	s := NewServer(Config{
		APIKey:    "k",
		Executor:  &fakeExecutor{},
		Tracker:   executor.NewTracker(1),
		LaunchEnv: LaunchEnv{BaseImage: "img", MCPURL: "http://mcp", SecretsHostDir: "/secrets/shared"},
		Logger:    quietLogger(),
	})

	spec := s.buildLaunchSpec(protocol.TriggerPayload{
		CardID:   "C1",
		Project:  "p",
		GitToken: "cm-git-token",
	}, "corr", "")

	assert.Equal(t, "/secrets/shared", spec.SecretsHostDir)
}

// TestLaunch_ProvisionsPerRunCredentials verifies launch provisions the per-run
// credentials (payload token + expiry + resolved endpoint) before starting the
// container, and does not tear them down on a successful launch.
func TestLaunch_ProvisionsPerRunCredentials(t *testing.T) {
	tracker := executor.NewTracker(1)
	creds := &fakeCredentials{}
	reporter := &fakeReporter{}

	s := NewServer(Config{
		APIKey:      "k",
		Executor:    &fakeExecutor{tracker: tracker},
		Tracker:     tracker,
		Reporter:    reporter,
		Credentials: creds,
		LaunchEnv:   LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
		Logger:      quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:            "C1",
		Project:           "p",
		GitToken:          "cm-git-token",
		GitTokenExpiresAt: "2026-07-05T12:00:00Z",
		LLMEndpoint:       &protocol.LLMEndpoint{Type: "openai", BaseURL: "https://llm.example/v1", APIKey: "cm-llm-key"},
	}
	spec := s.buildLaunchSpec(payload, "corr", "")

	s.launch(spec, payload)

	calls := creds.provisionCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, provisionCall{
		project:   "p",
		cardID:    "C1",
		token:     "cm-git-token",
		expiresAt: "2026-07-05T12:00:00Z",
		endpoint:  secrets.EndpointSecrets{Type: "openai", BaseURL: "https://llm.example/v1", APIKey: "cm-llm-key"},
	}, calls[0])
	assert.Empty(t, creds.teardownCalls(), "no teardown on a successful launch")
	assert.Equal(t, [][3]string{{"C1", "running", ""}}, reporter.statuses())
}

// TestLaunch_TearsDownPerRunCredentialsOnLaunchFailure verifies a launch error
// tears down the just-provisioned per-run credentials and reports failed.
func TestLaunch_TearsDownPerRunCredentialsOnLaunchFailure(t *testing.T) {
	creds := &fakeCredentials{}
	reporter := &fakeReporter{}

	s := NewServer(Config{
		APIKey:      "k",
		Executor:    &fakeExecutor{launchErr: errors.New("boom")},
		Tracker:     executor.NewTracker(1),
		Reporter:    reporter,
		Credentials: creds,
		LaunchEnv:   LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
		Logger:      quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:            "C1",
		Project:           "p",
		GitToken:          "cm-git-token",
		GitTokenExpiresAt: "2026-07-05T12:00:00Z",
		LLMEndpoint:       &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
	}
	s.launch(s.buildLaunchSpec(payload, "corr", ""), payload)

	assert.Len(t, creds.provisionCalls(), 1)
	assert.Equal(t, [][2]string{{"p", "C1"}}, creds.teardownCalls(),
		"a launch failure must tear down the per-run credentials")
	assert.Equal(t, [][3]string{{"C1", "failed", "launch failed"}}, reporter.statuses())
}

// TestLaunch_ProvisionFailureReportsFailedWithoutLaunching verifies a provision
// error short-circuits: the container is never started and the run reports failed.
func TestLaunch_ProvisionFailureReportsFailedWithoutLaunching(t *testing.T) {
	creds := &fakeCredentials{provisionErr: errors.New("disk full")}
	exec := &fakeExecutor{}
	reporter := &fakeReporter{}

	s := NewServer(Config{
		APIKey:      "k",
		Executor:    exec,
		Tracker:     executor.NewTracker(1),
		Reporter:    reporter,
		Credentials: creds,
		LaunchEnv:   LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
		Logger:      quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:            "C1",
		Project:           "p",
		GitToken:          "cm-git-token",
		GitTokenExpiresAt: "2026-07-05T12:00:00Z",
		LLMEndpoint:       &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
	}
	s.launch(s.buildLaunchSpec(payload, "corr", ""), payload)

	assert.Empty(t, exec.launchedSpecs(), "container must not launch when provisioning fails")
	assert.Equal(t, [][3]string{{"C1", "failed", "credential provisioning failed"}}, reporter.statuses())
}

// TestLaunch_DuplicateTriggerDoesNotClobberWinnerCredentials pins the
// duplicate-trigger race: /trigger has no request dedup and the capacity
// pre-check does not serialize same-card admission, so two launches for one
// card can both reach admission. The loser must be rejected up front (the card
// is already tracked) WITHOUT provisioning, launching, or touching the winner's
// per-run credentials - before this fix the loser skipped provisioning but
// still reached the executor, and a tracker.Remove landing in that window could
// admit it against an unprovisioned run dir the winner's Teardown then deleted.
func TestLaunch_DuplicateTriggerDoesNotClobberWinnerCredentials(t *testing.T) {
	// Capacity 2 so the duplicate key, not raw capacity, rejects the loser: the
	// executor's AddIfUnderLimit enforces one container per card.
	tracker := executor.NewTracker(2)
	creds := &fakeCredentials{}
	reporter := &fakeReporter{}

	s := NewServer(Config{
		APIKey:      "k",
		Executor:    &fakeExecutor{tracker: tracker},
		Tracker:     tracker,
		Reporter:    reporter,
		Credentials: creds,
		LaunchEnv:   LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
		Logger:      quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:            "C1",
		Project:           "p",
		GitToken:          "cm-git-token",
		GitTokenExpiresAt: "2026-07-05T12:00:00Z",
		LLMEndpoint:       &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
	}
	spec := s.buildLaunchSpec(payload, "corr", "")

	s.launch(spec, payload) // winner: provisions and launches
	s.launch(spec, payload) // duplicate: must be rejected up front, before provisioning

	assert.Len(t, creds.provisionCalls(), 1,
		"the duplicate must not re-provision over the winner")
	assert.Empty(t, creds.teardownCalls(),
		"the rejected duplicate must not tear down the winner's credentials")
	assert.Equal(t, [][3]string{{"C1", "running", ""}, {"C1", "failed", "card already running"}},
		reporter.statuses())
}

// TestLaunch_ConcurrentDuplicateTriggersSerialized drives the same duplicate
// race with real concurrency (meaningful under -race): whichever interleaving
// the scheduler picks, exactly one trigger provisions and runs, the other fails
// closed, and the winner's credentials are never torn down.
func TestLaunch_ConcurrentDuplicateTriggersSerialized(t *testing.T) {
	tracker := executor.NewTracker(2)
	creds := &fakeCredentials{}
	reporter := &fakeReporter{}

	s := NewServer(Config{
		APIKey:      "k",
		Executor:    &fakeExecutor{tracker: tracker},
		Tracker:     tracker,
		Reporter:    reporter,
		Credentials: creds,
		LaunchEnv:   LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
		Logger:      quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:            "C1",
		Project:           "p",
		GitToken:          "cm-git-token",
		GitTokenExpiresAt: "2026-07-05T12:00:00Z",
		LLMEndpoint:       &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
	}
	spec := s.buildLaunchSpec(payload, "corr", "")

	var wg sync.WaitGroup

	for range 2 {
		wg.Go(func() {
			s.launch(spec, payload)
		})
	}

	wg.Wait()

	assert.Len(t, creds.provisionCalls(), 1, "exactly one trigger provisions")
	assert.Empty(t, creds.teardownCalls(), "the loser must not tear down the winner")

	statuses := reporter.statuses()
	require.Len(t, statuses, 2)

	var running, failed int

	for _, st := range statuses {
		switch st[1] {
		case "running":
			running++
		case "failed":
			failed++
		}
	}

	assert.Equal(t, 1, running, "exactly one trigger reports running")
	assert.Equal(t, 1, failed, "exactly one trigger reports failed")
}

// ---- credential-availability guards (fail-closed) ---------------------------
//
// These pin the fail-closed behavior: ContextMatrix provisions both
// credentials per run, and a trigger missing either must never reach the
// executor. Rejection surfaces exactly like a launch or provisioning failure -
// a "failed" status callback, not a synchronous HTTP error - since the guard
// lives in admitAndLaunch, downstream of the 202 Accepted response.

// TestLaunch_GitTokenAloneDoesNotSatisfyLLMGuard proves the two guards are
// independent: a payload git token covers guard 1 but not guard 2, so the run
// must still be rejected (by the LLM guard).
func TestLaunch_GitTokenAloneDoesNotSatisfyLLMGuard(t *testing.T) {
	tracker := executor.NewTracker(1)
	exec := &fakeExecutor{tracker: tracker}
	reporter := &fakeReporter{}

	s := NewServer(Config{
		APIKey:      "k",
		Executor:    exec,
		Tracker:     tracker,
		Reporter:    reporter,
		Credentials: &fakeCredentials{},
		LaunchEnv:   LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"}, // no local fallback at all
		Logger:      quietLogger(),
	})

	payload := protocol.TriggerPayload{CardID: "C1", Project: "p", GitToken: "cm-git-token"}
	s.launch(s.buildLaunchSpec(payload, "corr", ""), payload)

	assert.Empty(t, exec.launchedSpecs())
	assert.Equal(t,
		[][3]string{{"C1", "failed", "CM did not provision an llm_endpoint"}},
		reporter.statuses())
}

// TestLaunch_LLMEndpointAloneDoesNotSatisfyGitTokenGuard mirrors the previous
// test from the other side: a payload llm_endpoint covers guard 2 but not
// guard 1, so the run must still be rejected (by the git-token guard).
func TestLaunch_LLMEndpointAloneDoesNotSatisfyGitTokenGuard(t *testing.T) {
	tracker := executor.NewTracker(1)
	exec := &fakeExecutor{tracker: tracker}
	reporter := &fakeReporter{}

	s := NewServer(Config{
		APIKey:    "k",
		Executor:  exec,
		Tracker:   tracker,
		Reporter:  reporter,
		LaunchEnv: LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"}, // no local fallback at all
		Logger:    quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:      "C1",
		Project:     "p",
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
	}
	s.launch(s.buildLaunchSpec(payload, "corr", ""), payload)

	assert.Empty(t, exec.launchedSpecs())
	assert.Equal(t,
		[][3]string{{"C1", "failed", "CM did not provision a git token"}},
		reporter.statuses())
}

// TestLaunch_GuardsDoNotFireWhenPayloadProvisionsBoth proves a fully-provisioned
// payload launches: the payload itself carries both credentials, which is the
// only credential source there is.
func TestLaunch_GuardsDoNotFireWhenPayloadProvisionsBoth(t *testing.T) {
	tracker := executor.NewTracker(1)
	creds := &fakeCredentials{}
	reporter := &fakeReporter{}

	s := NewServer(Config{
		APIKey:      "k",
		Executor:    &fakeExecutor{tracker: tracker},
		Tracker:     tracker,
		Reporter:    reporter,
		Credentials: creds,
		LaunchEnv:   LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"}, // deliberately no local fallback
		Logger:      quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:      "C1",
		Project:     "p",
		GitToken:    "cm-git-token",
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
	}
	s.launch(s.buildLaunchSpec(payload, "corr", ""), payload)

	assert.Len(t, creds.provisionCalls(), 1, "a fully-provisioned payload must launch regardless of local config")
	assert.Equal(t, [][3]string{{"C1", "running", ""}}, reporter.statuses())
}

// TestTrigger_RejectsAndReportsFailedWhenNoCredentialSourceAtAll drives the
// guard through the full HTTP /trigger endpoint (not just s.launch directly):
// the response is still 202 Accepted - rejection surfaces only via the async
// status callback - and no container is ever launched.
func TestTrigger_RejectsAndReportsFailedWhenNoCredentialSourceAtAll(t *testing.T) {
	tracker := executor.NewTracker(4)
	exec := &fakeExecutor{tracker: tracker}
	reporter := &fakeReporter{}
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, nil)

	server := NewServer(Config{
		APIKey:        testAPIKey,
		Skew:          protocol.DefaultMaxClockSkew,
		MaxConcurrent: 4,
		Executor:      exec,
		Tracker:       tracker,
		Hub:           hub,
		Reporter:      reporter,
		Verifier:      &fakeVerifier{autonomous: true},
		LaunchEnv: LaunchEnv{
			BaseImage: "base:image",
			MCPURL:    "http://cm:8080/mcp",
		},
		Logger: quietLogger(),
	})
	h := &harness{server: server, exec: exec, tracker: tracker, reporter: reporter, hub: hub}

	payload := protocol.TriggerPayload{CardID: "PROJ-020", Project: "proj", RepoURL: "r"}
	w := h.do(t, http.MethodPost, "/trigger", payload)

	require.Equal(t, http.StatusAccepted, w.Code, "the guard rejects asynchronously, not via the HTTP response")

	require.Eventually(t, func() bool {
		for _, c := range h.reporter.statuses() {
			if c[1] == "failed" {
				return true
			}
		}

		return false
	}, time.Second, 5*time.Millisecond)

	statuses := h.reporter.statuses()
	require.Len(t, statuses, 1)
	assert.Equal(t, "CM did not provision a git token", statuses[0][2])
	assert.Empty(t, h.exec.launchedSpecs(), "no container launch when the payload provisions nothing")
}

// TestBuildLaunchSpec_Mob pins the CM_MOB_* env emission, mirroring the
// Best-of-N pattern: scalar knobs ride plain env; guest specs (bearer tokens
// inside) never do - they travel only via the per-run secrets file.
func TestBuildLaunchSpec_Mob(t *testing.T) {
	newServer := func() *Server {
		return NewServer(Config{
			APIKey:    "k",
			Executor:  &fakeExecutor{},
			Tracker:   executor.NewTracker(1),
			LaunchEnv: LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
		})
	}

	t.Run("emits scalar env when mob is on", func(t *testing.T) {
		s := newServer()

		spec := s.buildLaunchSpec(protocol.TriggerPayload{
			CardID: "C1", Project: "p",
			Mob: &protocol.MobSpec{
				Participants: 3,
				Phases:       []string{"plan", "review"},
				Rounds:       2,
				BudgetFactor: 0.75,
				Guests:       []protocol.GuestSpec{{Name: "laptop", URL: "http://10.0.0.5:8484", Token: "guest-secret"}},
			},
		}, "corr", "")

		assert.Contains(t, spec.Env, "CM_MOB_PARTICIPANTS=3")
		assert.Contains(t, spec.Env, "CM_MOB_PHASES=plan,review")
		assert.Contains(t, spec.Env, "CM_MOB_ROUNDS=2")
		assert.Contains(t, spec.Env, "CM_MOB_BUDGET_FACTOR=0.75")

		for _, e := range spec.Env {
			assert.NotContains(t, e, "guest-secret", "guest tokens must never ride plain container env")
			assert.NotContains(t, e, "CM_MOB_GUESTS", "guest specs must never ride plain container env")
		}
	})

	t.Run("omits all mob env when absent", func(t *testing.T) {
		s := newServer()

		spec := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "C1", Project: "p"}, "corr", "")

		for _, e := range spec.Env {
			assert.NotContains(t, e, "CM_MOB", "no mob env for a non-mob run")
		}
	})

	t.Run("omits mob env when participants below two", func(t *testing.T) {
		s := newServer()

		spec := s.buildLaunchSpec(protocol.TriggerPayload{
			CardID: "C1", Project: "p",
			Mob: &protocol.MobSpec{Participants: 1},
		}, "corr", "")

		for _, e := range spec.Env {
			assert.NotContains(t, e, "CM_MOB", "participants < 2 is off")
		}
	})
}

// TestRunEndpointCarriesMobGuests pins the guest delivery seam: guest specs
// land in EndpointSecrets.MobGuests so the per-run secrets writer (initial
// Provision AND every refresh rewrite) emits the CM_MOB_GUESTS line.
func TestRunEndpointCarriesMobGuests(t *testing.T) {
	s := NewServer(Config{
		APIKey:    "k",
		Executor:  &fakeExecutor{},
		Tracker:   executor.NewTracker(1),
		LaunchEnv: LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
	})

	t.Run("guests marshal alongside the llm endpoint", func(t *testing.T) {
		e := s.runEndpoint(protocol.TriggerPayload{
			LLMEndpoint: &protocol.LLMEndpoint{Type: "openai", BaseURL: "https://llm.example/v1", APIKey: "key"},
			Mob: &protocol.MobSpec{
				Participants: 3,
				Guests:       []protocol.GuestSpec{{Name: "laptop", URL: "http://10.0.0.5:8484", Token: "guest-secret"}},
			},
		})

		assert.Equal(t, "key", e.APIKey)
		assert.JSONEq(t,
			`[{"name":"laptop","url":"http://10.0.0.5:8484","token":"guest-secret"}]`,
			e.MobGuests)
	})

	t.Run("guests survive a nil llm endpoint", func(t *testing.T) {
		e := s.runEndpoint(protocol.TriggerPayload{
			Mob: &protocol.MobSpec{
				Participants: 2,
				Guests:       []protocol.GuestSpec{{Name: "laptop", URL: "http://10.0.0.5:8484"}},
			},
		})

		assert.Empty(t, e.APIKey)
		assert.NotEmpty(t, e.MobGuests)
	})

	t.Run("no guests yields empty string", func(t *testing.T) {
		e := s.runEndpoint(protocol.TriggerPayload{Mob: &protocol.MobSpec{Participants: 3}})
		assert.Empty(t, e.MobGuests)
	})
}

func TestBuildLaunchSpecMobCheckpointEnv(t *testing.T) {
	s := newSkillsTestServer(fakeSkillsResolver{})

	spec := s.buildLaunchSpec(protocol.TriggerPayload{
		CardID: "C1", Project: "p",
		Mob: &protocol.MobSpec{
			Participants: 3, Phases: []string{"plan", "execute"},
			ExecuteCheckpoints: true, CheckpointMinTier: "simple", CheckpointRounds: 3,
		},
	}, "corr", "")

	assert.Contains(t, spec.Env, "CM_MOB_EXECUTE_CHECKPOINTS=true")
	assert.Contains(t, spec.Env, "CM_MOB_CHECKPOINT_MIN_TIER=simple")
	assert.Contains(t, spec.Env, "CM_MOB_CHECKPOINT_ROUNDS=3")
}

func TestBuildLaunchSpecMobCheckpointEnvAbsentWhenOff(t *testing.T) {
	s := newSkillsTestServer(fakeSkillsResolver{})

	spec := s.buildLaunchSpec(protocol.TriggerPayload{
		CardID: "C1", Project: "p",
		Mob: &protocol.MobSpec{Participants: 3, Phases: []string{"plan"}},
	}, "corr", "")

	for _, e := range spec.Env {
		assert.NotContains(t, e, "CM_MOB_EXECUTE_CHECKPOINTS")
		assert.NotContains(t, e, "CM_MOB_CHECKPOINT_")
	}
}

// ---- logs --------------------------------------------------------------

// TestLogs_MountAndAuth is a smoke test for the /logs route as mounted through
// the real Routes() mux: the backendkit suite pins the SSE mechanics
// (keepalive, filtering, ...) in isolation, but only this repo's own mux and
// middleware chain can prove /logs is actually wired behind Auth here. A
// signed GET streams the SSE preamble over a real connection (a
// ResponseRecorder cannot observe a still-streaming handler, so this needs
// httptest.NewServer); an unsigned GET is rejected outright.
func TestLogs_MountAndAuth(t *testing.T) {
	h := newHarness(t, 4)

	ts := httptest.NewServer(h.server.Routes())
	defer ts.Close()
	defer h.server.CloseSSE()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/logs", nil)
	require.NoError(t, err)

	sigTS := nowTS()
	sig := protocol.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs", nil, sigTS)
	req.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	req.Header.Set(protocol.TimestampHeader, sigTS)

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // unblocked via ctx cancel below
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	buf := make([]byte, len(": connected\n\n"))
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	assert.Equal(t, ": connected\n\n", string(buf), "body must start with the SSE connected preamble")

	cancel() // unblock the still-streaming handler so the test exits promptly

	unsigned := httptest.NewRequest(http.MethodGet, "/logs", nil)
	w := httptest.NewRecorder()
	h.server.Routes().ServeHTTP(w, unsigned)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "unsigned GET /logs must be rejected by Auth")
}
