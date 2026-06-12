package cmclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAgentID = "cmx-agent-cmx-001"

// recorder captures the arguments every stub tool received, keyed by tool name.
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
// IsError surfacing path.
func addStub(server *mcp.Server, rec *recorder, name, cannedText string, fail bool) {
	mcp.AddTool(server, &mcp.Tool{Name: name}, func(_ context.Context, _ *mcp.CallToolRequest, in genericInput) (*mcp.CallToolResult, any, error) {
		rec.record(name, in)

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
// shape: {card, parent, siblings, config}. Our client only reads the card
// portion, so parent/siblings/config carry filler the client must ignore.
const taskContextPayload = `{
  "card": {
    "id": "CMX-001",
    "title": "Wire up the widget",
    "body": "Connect the widget to the gizmo and verify the blinkenlights.",
    "state": "in_progress"
  },
  "parent": {"id": "CMX-000", "title": "Epic"},
  "siblings": [{"id": "CMX-002", "title": "Sibling"}],
  "config": {"name": "demo", "prefix": "CMX"}
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

	tc, err := c.GetTaskContext(context.Background(), "CMX-001")
	require.NoError(t, err)

	args, ok := rec.get("get_task_context")
	require.True(t, ok, "get_task_context stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	// include_images must be sent as an explicit false.
	require.Contains(t, args, "include_images")
	assert.Equal(t, false, args["include_images"])

	// Parsed from the card portion of the canned payload.
	assert.Equal(t, "CMX-001", tc.CardID)
	assert.Equal(t, "Wire up the widget", tc.Title)
	assert.Equal(t, "Connect the widget to the gizmo and verify the blinkenlights.", tc.Description)
	assert.Equal(t, "in_progress", tc.State)
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

	require.NoError(t, c.ReportUsage(context.Background(), "CMX-001", "some/model", 100, 50))

	args, ok := rec.get("report_usage")
	require.True(t, ok, "report_usage stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	assert.Equal(t, "some/model", args["model"])
	// Token counts must reach the wire as numbers. JSON numbers decode to
	// float64 in a map[string]any, so compare by numeric value.
	assert.EqualValues(t, 100, args["prompt_tokens"])
	assert.EqualValues(t, 50, args["completion_tokens"])
}

func TestReportUsage_OmitsEmptyModel(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.ReportUsage(context.Background(), "CMX-001", "", 1, 2))

	args, ok := rec.get("report_usage")
	require.True(t, ok)
	assert.NotContains(t, args, "model", "empty model should be omitted")
}

func TestReportPush(t *testing.T) {
	rec := newRecorder()
	c := newTestClient(t, rec, "")

	require.NoError(t, c.ReportPush(context.Background(), "CMX-001", "cm/cmx-001"))

	args, ok := rec.get("report_push")
	require.True(t, ok, "report_push stub should have been called")
	assert.Equal(t, "CMX-001", args["card_id"])
	assert.Equal(t, testAgentID, args["agent_id"])
	assert.Equal(t, "cm/cmx-001", args["branch"])
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

func TestGetTaskContext_MalformedJSON(t *testing.T) {
	rec := newRecorder()
	server := mcp.NewServer(&mcp.Implementation{Name: "stub-cm", Version: "0.0.0"}, nil)
	addStub(server, rec, "get_task_context", "not json", false)
	ts := serveBearer(t, server)

	c, err := New(context.Background(), ts.URL, "test-key", testAgentID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	_, err = c.GetTaskContext(context.Background(), "CMX-001")
	require.Error(t, err, "non-JSON payload must surface as a parse error")
	assert.Contains(t, err.Error(), "parse task context")
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
