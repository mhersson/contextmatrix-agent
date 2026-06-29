package cmclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyReleaseError(t *testing.T) {
	require.NoError(t, classifyReleaseError(nil))

	// CM surfaces the lock manager's "card is not claimed" text on a redundant
	// release; classifyReleaseError maps it to the typed sentinel.
	notClaimed := fmt.Errorf("call release_card: release card CARD-1: release card: %s", ErrCardNotClaimed.Error())
	got := classifyReleaseError(notClaimed)
	require.ErrorIs(t, got, ErrCardNotClaimed)
	assert.Contains(t, got.Error(), "CARD-1", "original message is preserved in the chain")

	other := errors.New("call release_card: connection refused")
	got2 := classifyReleaseError(other)
	require.NotErrorIs(t, got2, ErrCardNotClaimed, "unrelated errors are not the sentinel")
	assert.Equal(t, other, got2, "unrelated errors pass through unchanged")
}

const testAgentID = "cmx-agent-cmx-001"

// recorder captures the arguments every stub tool received, keyed by tool
// name. Only the LAST call per tool is kept — fine here, where every test
// invokes each tool at most once.
type recorder struct {
	mu    sync.Mutex
	calls map[string]map[string]any
}

func newRecorder() *recorder {
	return &recorder{calls: make(map[string]map[string]any)}
}

func (r *recorder) record(name string, args map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls[name] = args
}

func (r *recorder) get(name string) (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	args, ok := r.calls[name]

	return args, ok
}

// genericInput accepts any tool argument set so a single stub signature works
// for every card-operation tool. The SDK unmarshals the wire arguments into it.
type genericInput map[string]any

// addStub registers a tool that records its raw arguments and returns canned
// text content. When fail is true the tool returns an error, exercising the
// IsError surfacing path. required lists argument keys the stub rejects when
// absent, mirroring the real server's JSON-schema validation of non-omitempty
// input fields (e.g. list_cards' and create_card's required project).
func addStub(server *mcp.Server, rec *recorder, name, cannedText string, fail bool, required ...string) {
	mcp.AddTool(server, &mcp.Tool{Name: name}, func(_ context.Context, _ *mcp.CallToolRequest, in genericInput) (*mcp.CallToolResult, any, error) {
		rec.record(name, in)

		for _, key := range required {
			if _, ok := in[key]; !ok {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "missing required argument: " + key}},
				}, nil, nil
			}
		}

		if fail {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "stub failure: " + name}},
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: cannedText}},
		}, nil, nil
	})
}

// taskContextPayload mirrors the real ContextMatrix get_task_context result
// shape: {card, parent, siblings, config}. Our client reads the card portion
// including all orchestrator fields.
const taskContextPayload = `{
  "card": {
    "id": "CMX-001",
    "title": "Wire up the widget",
    "body": "Connect the widget to the gizmo and verify the blinkenlights.",
    "state": "in_progress",
    "type": "bug",
    "labels": ["bug", "backend"],
    "phase": "execute",
    "autonomous": true,
    "create_pr": true,
    "base_branch": "main",
    "review_attempts": 2,
    "model_orchestrator": "claude-opus-4-5",
    "model_coder": "claude-sonnet-4-5",
    "model_reviewer": "claude-opus-4-5",
    "token_usage": {
      "prompt_tokens": 1000,
      "completion_tokens": 200,
      "estimated_cost_usd": 0.0045
    }
  },
  "parent": {"id": "CMX-000", "title": "Epic"},
  "siblings": [{"id": "CMX-002", "title": "Sibling"}],
  "config": {"name": "demo", "prefix": "CMX"}
}`

// incrementReviewAttemptsPayload mirrors the increment_review_attempts result:
// {card: <board.Card>} with the new review_attempts value.
const incrementReviewAttemptsPayload = `{
  "card": {
    "id": "CMX-001",
    "title": "Wire up the widget",
    "state": "in_progress",
    "review_attempts": 3
  }
}`

// createCardPayload mirrors the create_card result: a full board.Card with
// server-generated id.
const createCardPayload = `{
  "id": "CMX-042",
  "title": "Implement widget interface",
  "state": "todo",
  "type": "subtask",
  "priority": "medium"
}`

// listCardsSubtaskPayload mirrors list_cards result for subtask queries:
// {cards: [...]}.
const listCardsSubtaskPayload = `{
  "cards": [
    {"id": "CMX-002", "title": "First subtask",  "state": "done"},
    {"id": "CMX-003", "title": "Second subtask", "state": "in_progress"},
    {"id": "CMX-004", "title": "Third subtask",  "state": "todo"}
  ]
}`

// serveBearer serves an MCP server over the SDK's streamable HTTP handler
// behind a bearer gate. A missing or wrong token must 401, which proves the
// client sends Authorization on every request (connect would otherwise fail).
func serveBearer(t *testing.T, server *mcp.Server) *httptest.Server {
	t.Helper()

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)

	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		handler.ServeHTTP(w, r)
	})

	ts := httptest.NewServer(guarded)
	t.Cleanup(ts.Close)

	return ts
}

// startStubServer stands up an in-process MCP server with stub card-operation
// tools behind the bearer gate. failTool, when non-empty, names the tool that
// returns an error.
func startStubServer(t *testing.T, rec *recorder, failTool string) *httptest.Server {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "stub-cm", Version: "0.0.0"}, nil)

	addStub(server, rec, "claim_card", `{"ok": true}`, failTool == "claim_card")
	addStub(server, rec, "get_task_context", taskContextPayload, failTool == "get_task_context")
	addStub(server, rec, "heartbeat", `{"ok": true}`, failTool == "heartbeat")
	addStub(server, rec, "report_usage", `{"ok": true}`, failTool == "report_usage")
	addStub(server, rec, "report_push", `{"ok": true}`, failTool == "report_push")
	addStub(server, rec, "complete_task", `{"ok": true}`, failTool == "complete_task")
	addStub(server, rec, "release_card", `{"ok": true}`, failTool == "release_card")
	addStub(server, rec, "create_card", createCardPayload, failTool == "create_card", "project")
	addStub(server, rec, "update_card", `{"id":"CMX-001","state":"in_progress"}`, failTool == "update_card")
	addStub(server, rec, "transition_card", `{"id":"CMX-001","state":"review"}`, failTool == "transition_card")
	addStub(server, rec, "start_review", `{"skill_name":"review-task","model":"opus","content":"review instructions","inline":true}`, failTool == "start_review")
	addStub(server, rec, "increment_review_attempts", incrementReviewAttemptsPayload, failTool == "increment_review_attempts")
	addStub(server, rec, "list_cards", listCardsSubtaskPayload, failTool == "list_cards", "project")
	addStub(server, rec, "add_log", `{"id":"CMX-001","state":"in_progress"}`, failTool == "add_log")
	addStub(server, rec, "report_incapable_model", `{"ok": true}`, failTool == "report_incapable_model")

	return serveBearer(t, server)
}

func newTestClient(t *testing.T, rec *recorder, failTool string) *Client {
	t.Helper()

	ts := startStubServer(t, rec, failTool)
	ctx := context.Background()

	c, err := New(ctx, ts.URL, "test-key", testAgentID)
	require.NoError(t, err, "New should connect with a valid bearer token")
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestNew_MissingBearerFails(t *testing.T) {
	ts := startStubServer(t, newRecorder(), "")

	// Wrong key must fail the connect handshake (the gate 401s it).
	_, err := New(context.Background(), ts.URL, "wrong-key", testAgentID)
	require.Error(t, err, "connect must fail without the correct bearer token")
}

func TestClaimCard(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.ClaimCard(context.Background(), "CMX-001"))

	args, ok := rec.get("claim_card")
	require.True(t, ok, "claim_card stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
}

func TestGetTaskContext(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	tc, err := c.GetTaskContext(context.Background(), "CMX-001", true)
	require.NoError(t, err)

	args, ok := rec.get("get_task_context")
	require.True(t, ok, "get_task_context stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	// include_images is now requested as true.
	require.Contains(t, args, "include_images")
	assert.Equal(t, true, args["include_images"])

	// Parsed from the card portion of the canned payload — base fields.
	assert.Equal(t, "CMX-001", tc.CardID)
	assert.Equal(t, "Wire up the widget", tc.Title)
	assert.Equal(t, "Connect the widget to the gizmo and verify the blinkenlights.", tc.Description)
	assert.Equal(t, "in_progress", tc.State)

	// Classification fields from the card JSON.
	assert.Equal(t, "bug", tc.Type)
	assert.Equal(t, []string{"bug", "backend"}, tc.Labels)

	// Orchestrator fields from the extended card JSON.
	assert.Equal(t, "execute", tc.Phase)
	assert.True(t, tc.Autonomous)
	assert.True(t, tc.CreatePR)
	assert.Equal(t, "main", tc.BaseBranch)
	assert.Equal(t, 2, tc.ReviewAttempts)
	assert.Equal(t, "claude-opus-4-5", tc.ModelOrchestrator)
	assert.Equal(t, "claude-sonnet-4-5", tc.ModelCoder)
	assert.Equal(t, "claude-opus-4-5", tc.ModelReviewer)
	assert.InDelta(t, 0.0045, tc.ReportedCostUSD, 1e-9)
}

func TestGetTaskContext_NoImages(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	tc, err := c.GetTaskContext(context.Background(), "CMX-001", true)
	require.NoError(t, err)
	assert.Empty(t, tc.Images) // canned stub returns text only
}

func TestGetTaskContext_IncludeImagesFalse(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	_, err := c.GetTaskContext(context.Background(), "CMX-001", false)
	require.NoError(t, err)

	args, ok := rec.get("get_task_context")
	require.True(t, ok, "get_task_context stub should have been called")
	// When includeImages=false the wire arg must carry false, not the
	// default JSON zero (missing key). The server honours the explicit value.
	assert.Equal(t, false, args["include_images"])
}

func TestGetTaskContext_ParsesImages(t *testing.T) {
	rec := newRecorder()
	server := mcp.NewServer(&mcp.Implementation{Name: "stub-cm", Version: "0.0.0"}, nil)

	png := []byte{0x89, 0x50, 0x4e, 0x47} // opaque bytes; the client does not decode

	mcp.AddTool(server, &mcp.Tool{Name: "get_task_context"},
		func(_ context.Context, _ *mcp.CallToolRequest, in genericInput) (*mcp.CallToolResult, any, error) {
			rec.record("get_task_context", in)

			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: `{"card":{"id":"CMX-001","title":"T","body":"see ![](/api/images/abc)","state":"todo"}}`},
				&mcp.ImageContent{Data: png, MIMEType: "image/png"},
			}}, nil, nil
		})

	ts := serveBearer(t, server)
	c, err := New(context.Background(), ts.URL, "test-key", testAgentID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	tc, err := c.GetTaskContext(context.Background(), "CMX-001", true)
	require.NoError(t, err)

	args, ok := rec.get("get_task_context")
	require.True(t, ok, "get_task_context stub should have been called")
	assert.Equal(t, true, args["include_images"])

	require.Len(t, tc.Images, 1)
	assert.Equal(t, "image/png", tc.Images[0].MIME)
	assert.Equal(t, png, tc.Images[0].Data)
}

func TestGetTaskContext_OrchestratorFieldsDefaultWhenAbsent(t *testing.T) {
	rec := newRecorder()
	server := mcp.NewServer(&mcp.Implementation{Name: "stub-cm", Version: "0.0.0"}, nil)
	// Minimal card: only base fields present.
	addStub(server, rec, "get_task_context", `{
		"card": {"id": "CMX-001", "title": "T", "body": "B", "state": "todo"},
		"config": {}
	}`, false)
	ts := serveBearer(t, server)

	c, err := New(context.Background(), ts.URL, "test-key", testAgentID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	tc, err := c.GetTaskContext(context.Background(), "CMX-001", true)
	require.NoError(t, err)

	assert.Empty(t, tc.Phase)
	assert.False(t, tc.Autonomous)
	assert.False(t, tc.CreatePR)
	assert.Empty(t, tc.BaseBranch)
	assert.Equal(t, 0, tc.ReviewAttempts)
	assert.Empty(t, tc.ModelOrchestrator)
	assert.Empty(t, tc.ModelCoder)
	assert.Empty(t, tc.ModelReviewer)
	assert.InDelta(t, 0.0, tc.ReportedCostUSD, 1e-9)
}

func TestHeartbeat(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.Heartbeat(context.Background(), "CMX-001"))

	args, ok := rec.get("heartbeat")
	require.True(t, ok, "heartbeat stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
}

func TestReportUsage(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.ReportUsage(context.Background(), "CMX-001", "some/model", 100, 50, 0.0))

	args, ok := rec.get("report_usage")
	require.True(t, ok, "report_usage stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	assert.Equal(t, "some/model", args["model"])
	// Token counts must reach the wire as numbers. JSON numbers decode to
	// float64 in a map[string]any, so compare by numeric value.
	assert.EqualValues(t, 100, args["prompt_tokens"])
	assert.EqualValues(t, 50, args["completion_tokens"])
	// Zero actual cost must be omitted from the wire.
	assert.NotContains(t, args, "actual_cost_usd", "zero cost must be omitted")
}

func TestReportUsage_ActualCostSentWhenNonZero(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.ReportUsage(context.Background(), "CMX-001", "some/model", 100, 50, 0.0123))

	args, ok := rec.get("report_usage")
	require.True(t, ok)
	require.Contains(t, args, "actual_cost_usd", "non-zero cost must be sent")
	assert.InDelta(t, 0.0123, args["actual_cost_usd"], 1e-9)
}

func TestReportUsage_OmitsEmptyModel(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.ReportUsage(context.Background(), "CMX-001", "", 1, 2, 0))

	args, ok := rec.get("report_usage")
	require.True(t, ok)
	assert.NotContains(t, args, "model", "empty model should be omitted")
}

func TestReportPush(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.ReportPush(context.Background(), "CMX-001", "cm/cmx-001", ""))

	args, ok := rec.get("report_push")
	require.True(t, ok, "report_push stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	assert.Equal(t, "cm/cmx-001", args["branch"])
	// Empty pr_url must be omitted.
	assert.NotContains(t, args, "pr_url", "empty pr_url must be omitted")
}

func TestReportPush_SendsPRURLWhenSet(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.ReportPush(context.Background(), "CMX-001", "cm/cmx-001", "https://github.com/org/repo/pull/42"))

	args, ok := rec.get("report_push")
	require.True(t, ok)
	assert.Equal(t, "https://github.com/org/repo/pull/42", args["pr_url"])
}

func TestCompleteTask(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.CompleteTask(context.Background(), "CMX-001", "did the thing"))

	args, ok := rec.get("complete_task")
	require.True(t, ok, "complete_task stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	assert.Equal(t, "did the thing", args["summary"])
}

func TestReleaseCard(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.ReleaseCard(context.Background(), "CMX-001"))

	args, ok := rec.get("release_card")
	require.True(t, ok, "release_card stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
}

// --- new methods ---

func TestCreateCard(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	id, err := c.CreateCard(context.Background(), "demo", "CMX-000", "Implement widget interface", "## Details\n\nDo the work.", []string{"CMX-010"})
	require.NoError(t, err)
	assert.Equal(t, "CMX-042", id)

	args, ok := rec.get("create_card")
	require.True(t, ok, "create_card stub should have been called")
	assert.Equal(t, "demo", args["project"])
	assert.Equal(t, "CMX-000", args["parent"])
	assert.Equal(t, "Implement widget interface", args["title"])
	assert.Equal(t, "## Details\n\nDo the work.", args["body"])
	assert.Equal(t, testAgentID, args["agent_id"])
	// depends_on must land in args as a slice.
	depRaw, hasDeps := args["depends_on"]
	require.True(t, hasDeps, "depends_on must be present")
	// JSON round-trip makes this []interface{}.
	deps, ok := depRaw.([]interface{})
	require.True(t, ok, "depends_on must be a slice")
	require.Len(t, deps, 1)
	assert.Equal(t, "CMX-010", deps[0])
}

func TestCreateCard_NilDependsOnOmitted(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	_, err := c.CreateCard(context.Background(), "demo", "CMX-000", "Title", "body", nil)
	require.NoError(t, err)

	args, ok := rec.get("create_card")
	require.True(t, ok)
	assert.NotContains(t, args, "depends_on", "nil depends_on must be omitted")
}

func TestCreateCard_IsError(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "create_card")

	_, err := c.CreateCard(context.Background(), "demo", "", "Title", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stub failure: create_card")
}

func TestSetPhase(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.SetPhase(context.Background(), "CMX-001", "execute"))

	args, ok := rec.get("update_card")
	require.True(t, ok, "update_card stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	assert.Equal(t, "execute", args["phase"])
}

func TestSetPhase_IsError(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "update_card")

	err := c.SetPhase(context.Background(), "CMX-001", "execute")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stub failure: update_card")
}

func TestTransitionCard(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.TransitionCard(context.Background(), "CMX-001", "review"))

	args, ok := rec.get("transition_card")
	require.True(t, ok, "transition_card stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	assert.Equal(t, "review", args["new_state"])
}

func TestTransitionCard_IsError(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "transition_card")

	err := c.TransitionCard(context.Background(), "CMX-001", "review")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stub failure: transition_card")
}

func TestStartReview(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.StartReview(context.Background(), "CMX-001"))

	args, ok := rec.get("start_review")
	require.True(t, ok, "start_review stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
}

func TestStartReview_IsError(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "start_review")

	err := c.StartReview(context.Background(), "CMX-001")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stub failure: start_review")
}

func TestIncrementReviewAttempts(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	count, err := c.IncrementReviewAttempts(context.Background(), "CMX-001")
	require.NoError(t, err)
	assert.Equal(t, 3, count)

	args, ok := rec.get("increment_review_attempts")
	require.True(t, ok, "increment_review_attempts stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
}

func TestIncrementReviewAttempts_IsError(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "increment_review_attempts")

	_, err := c.IncrementReviewAttempts(context.Background(), "CMX-001")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stub failure: increment_review_attempts")
}

func TestSubtaskStates(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	states, err := c.SubtaskStates(context.Background(), "demo", "CMX-001")
	require.NoError(t, err)
	require.Len(t, states, 3)

	assert.Equal(t, SubtaskState{CardID: "CMX-002", Title: "First subtask", State: "done"}, states[0])
	assert.Equal(t, SubtaskState{CardID: "CMX-003", Title: "Second subtask", State: "in_progress"}, states[1])
	assert.Equal(t, SubtaskState{CardID: "CMX-004", Title: "Third subtask", State: "todo"}, states[2])

	args, ok := rec.get("list_cards")
	require.True(t, ok, "list_cards stub should have been called")
	// list_cards declares project as a required schema field (no card-ID
	// resolution fallback) — it must always be on the wire.
	assert.Equal(t, "demo", args["project"])
	assert.Equal(t, "CMX-001", args["parent"])
	assert.Equal(t, testAgentID, args["agent_id"])
}

func TestSubtaskStates_IsError(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "list_cards")

	_, err := c.SubtaskStates(context.Background(), "demo", "CMX-001")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stub failure: list_cards")
}

func TestAddLog(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.AddLog(context.Background(), "CMX-001", "progress update"))

	args, ok := rec.get("add_log")
	require.True(t, ok, "add_log stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	assert.Equal(t, "status_update", args["action"])
	assert.Equal(t, "progress update", args["message"])
}

func TestAddLog_IsError(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "add_log")

	err := c.AddLog(context.Background(), "CMX-001", "progress update")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stub failure: add_log")
}

func TestBlacklistModel(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.BlacklistModel(context.Background(), "CARD-1", "bad/model", "cannot drive the tool loop"))

	args, ok := rec.get("report_incapable_model")
	require.True(t, ok, "report_incapable_model stub should have been called")
	assert.Equal(t, "bad/model", args["model_slug"])
	assert.Equal(t, "cannot drive the tool loop", args["reason"])
	assert.Equal(t, "CARD-1", args["sample_card_id"])
	// agent_id is injected by c.call — must match the client's configured identity.
	assert.Equal(t, testAgentID, args["agent_id"])
}

func TestBlacklistModel_IsError(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "report_incapable_model")

	err := c.BlacklistModel(context.Background(), "CARD-1", "bad/model", "cannot drive the tool loop")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stub failure: report_incapable_model")
}

// --- existing edge-case tests ---

// newClientWithStub builds a client against a server carrying a single stub
// tool returning rawText. Used to exercise parse-error paths with non-JSON
// payloads.
func newClientWithStub(t *testing.T, tool, rawText string) *Client {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "stub-cm", Version: "0.0.0"}, nil)
	addStub(server, newRecorder(), tool, rawText, false)
	ts := serveBearer(t, server)

	c, err := New(context.Background(), ts.URL, "test-key", testAgentID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestGetTaskContext_MalformedJSON(t *testing.T) {
	c := newClientWithStub(t, "get_task_context", "not json")

	_, err := c.GetTaskContext(context.Background(), "CMX-001", true)
	require.Error(t, err, "non-JSON payload must surface as a parse error")
	assert.Contains(t, err.Error(), "parse task context")
}

func TestIncrementReviewAttempts_MalformedJSON(t *testing.T) {
	c := newClientWithStub(t, "increment_review_attempts", "not json")

	_, err := c.IncrementReviewAttempts(context.Background(), "CMX-001")
	require.Error(t, err, "non-JSON payload must surface as a parse error")
	assert.Contains(t, err.Error(), "parse increment_review_attempts response")
}

func TestCreateCard_MalformedJSON(t *testing.T) {
	c := newClientWithStub(t, "create_card", "not json")

	_, err := c.CreateCard(context.Background(), "demo", "CMX-000", "Title", "body", nil)
	require.Error(t, err, "non-JSON payload must surface as a parse error")
	assert.Contains(t, err.Error(), "parse create_card response")
}

func TestSubtaskStates_MalformedJSON(t *testing.T) {
	c := newClientWithStub(t, "list_cards", "not json")

	_, err := c.SubtaskStates(context.Background(), "demo", "CMX-001")
	require.Error(t, err, "non-JSON payload must surface as a parse error")
	assert.Contains(t, err.Error(), "parse list_cards response")
}

// Pins the test harness itself: a stub registered with a required key must
// reject calls missing it, the way the real server's JSON-schema validation
// does. This is what makes the wire-arg assertions trustworthy — a refactor
// that drops a required arg fails loudly instead of silently returning the
// canned payload.
func TestAddStub_RejectsMissingRequiredKey(t *testing.T) {
	rec := newRecorder()
	server := mcp.NewServer(&mcp.Implementation{Name: "stub-cm", Version: "0.0.0"}, nil)
	addStub(server, rec, "list_cards", listCardsSubtaskPayload, false, "project")
	ts := serveBearer(t, server)

	c, err := New(context.Background(), ts.URL, "test-key", testAgentID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	// Bypass SubtaskStates (which always sends project) and call the tool
	// directly without it.
	_, err = c.call(context.Background(), "list_cards", map[string]any{"parent": "CMX-001"})
	require.Error(t, err, "stub must reject a call missing a required key")
	assert.Contains(t, err.Error(), "missing required argument: project")
}

func TestCall_DoesNotMutateCallerArgs(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	args := map[string]any{"card_id": "CMX-001"}
	_, err := c.call(context.Background(), "claim_card", args)
	require.NoError(t, err)

	assert.NotContains(t, args, "agent_id", "call must not mutate the caller's args map")

	// The wire still carried the injected agent identity.
	wireArgs, ok := rec.get("claim_card")
	require.True(t, ok)
	assert.Equal(t, testAgentID, wireArgs["agent_id"])
}

func TestToolError_SurfacesAsGoError(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "claim_card")

	err := c.ClaimCard(context.Background(), "CMX-001")
	require.Error(t, err, "a tool IsError result must surface as a Go error")
	assert.Contains(t, err.Error(), "stub failure: claim_card")
}

// Guard: the canned payload stays valid JSON so a parsing regression in the
// fixture is caught here rather than masquerading as a client bug.
func TestTaskContextPayloadIsValidJSON(t *testing.T) {
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(taskContextPayload), &v))
}

func TestIncrementReviewAttemptsPayloadIsValidJSON(t *testing.T) {
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(incrementReviewAttemptsPayload), &v))
}

func TestCreateCardPayloadIsValidJSON(t *testing.T) {
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(createCardPayload), &v))
}

func TestListCardsSubtaskPayloadIsValidJSON(t *testing.T) {
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(listCardsSubtaskPayload), &v))
}

func TestRecordSkillEngaged(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.RecordSkillEngaged(context.Background(), "CMX-001", "go-development"))

	args, ok := rec.get("add_log")
	require.True(t, ok, "RecordSkillEngaged must call the add_log tool")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, "skill_engaged", args["action"], "action must be skill_engaged so CM records a skill_engaged entry")
	assert.Equal(t, "engaged go-development", args["message"], "message must be 'engaged <skill>' so skillNameOf parses it")
	assert.Equal(t, testAgentID, args["agent_id"], "the call helper injects the per-card agent identity")
}
