// Package cmclient is the worker's card-operations client: a minimal MCP
// client over Streamable HTTP against ContextMatrix's /mcp endpoint. Card
// progress stays on MCP by design — the webhook channel is lifecycle-only.
package cmclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Client is a card-operations client bound to one agent identity. Every method
// is a typed wrapper over a single MCP tool call; all calls carry the agent ID.
type Client struct {
	session *mcp.ClientSession
	agentID string
}

// bearerTransport injects a static Authorization: Bearer header on every
// outbound request. The SDK's streamable transport exposes no header hook
// directly, but it does accept a custom *http.Client, so we wrap its
// RoundTripper.
type bearerTransport struct {
	apiKey string
	base   http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone so we never mutate a caller-shared request.
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.apiKey)

	return t.base.RoundTrip(r)
}

// New connects to the ContextMatrix MCP endpoint. mcpURL e.g.
// http://host:8080/mcp; apiKey goes out as a bearer header on every request.
// agentID is the worker's identity (convention: cmx-agent-<card-id-lower>).
func New(ctx context.Context, mcpURL, apiKey, agentID string) (*Client, error) {
	httpClient := &http.Client{
		Transport: &bearerTransport{apiKey: apiKey, base: http.DefaultTransport},
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:   mcpURL,
		HTTPClient: httpClient,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "contextmatrix-agent-worker", Version: "0.1.0"}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to mcp endpoint: %w", err)
	}

	return &Client{session: session, agentID: agentID}, nil
}

// Close ends the MCP session. The underlying SDK session Close is idempotent
// and concurrency safe, so double-close is safe.
func (c *Client) Close() error {
	if err := c.session.Close(); err != nil {
		return fmt.Errorf("close mcp session: %w", err)
	}

	return nil
}

// TaskContext is the subset of get_task_context the worker needs. Parent,
// siblings, and project config from the server response are intentionally
// ignored.
type TaskContext struct {
	CardID      string
	Title       string
	Description string
	State       string
}

// call invokes a tool and surfaces both transport errors and tool-level
// IsError results as Go errors. It returns the result's first text content so
// callers can parse structured payloads. The caller's args map is never
// mutated; agent_id is injected into a fresh copy.
func (c *Client) call(ctx context.Context, tool string, args map[string]any) (string, error) {
	withAgent := make(map[string]any, len(args)+1)
	for k, v := range args {
		withAgent[k] = v
	}

	withAgent["agent_id"] = c.agentID

	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      tool,
		Arguments: withAgent,
	})
	if err != nil {
		return "", fmt.Errorf("call %s: %w", tool, err)
	}

	text := firstText(result)
	if result.IsError {
		return "", fmt.Errorf("call %s: %s", tool, text)
	}

	return text, nil
}

// firstText returns the text of the first TextContent in the result, or an
// empty string if none is present. The ContextMatrix server returns structured
// tool payloads as JSON text content, so this is where structured results land.
func firstText(result *mcp.CallToolResult) string {
	for _, content := range result.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			return tc.Text
		}
	}

	return ""
}

// ClaimCard claims the card for this client's agent.
func (c *Client) ClaimCard(ctx context.Context, cardID string) error {
	_, err := c.call(ctx, "claim_card", map[string]any{"card_id": cardID})

	return err
}

// GetTaskContext fetches the card context, parsing card_id/title/description/
// state from the card portion of the response. include_images is forced false:
// the worker reads card text, not inline image bytes.
func (c *Client) GetTaskContext(ctx context.Context, cardID string) (TaskContext, error) {
	text, err := c.call(ctx, "get_task_context", map[string]any{
		"card_id":        cardID,
		"include_images": false,
	})
	if err != nil {
		return TaskContext{}, err
	}

	// Mirrors the server's get_task_context output: {card, parent, siblings,
	// config}. Only the card is parsed.
	var payload struct {
		Card struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Body  string `json:"body"`
			State string `json:"state"`
		} `json:"card"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return TaskContext{}, fmt.Errorf("parse task context: %w", err)
	}

	return TaskContext{
		CardID:      payload.Card.ID,
		Title:       payload.Card.Title,
		Description: payload.Card.Body,
		State:       payload.Card.State,
	}, nil
}

// Heartbeat reports liveness for the card.
func (c *Client) Heartbeat(ctx context.Context, cardID string) error {
	_, err := c.call(ctx, "heartbeat", map[string]any{"card_id": cardID})

	return err
}

// ReportUsage records token usage for cost tracking. model may be empty, in
// which case it is omitted and the server applies its default.
func (c *Client) ReportUsage(ctx context.Context, cardID, model string, promptTokens, completionTokens int64) error {
	args := map[string]any{
		"card_id":           cardID,
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
	}
	if model != "" {
		args["model"] = model
	}

	_, err := c.call(ctx, "report_usage", args)

	return err
}

// ReportPush records that work was pushed to the given git branch.
func (c *Client) ReportPush(ctx context.Context, cardID, branch string) error {
	_, err := c.call(ctx, "report_push", map[string]any{
		"card_id": cardID,
		"branch":  branch,
	})

	return err
}

// CompleteTask marks the card done with the given summary.
func (c *Client) CompleteTask(ctx context.Context, cardID, summary string) error {
	_, err := c.call(ctx, "complete_task", map[string]any{
		"card_id": cardID,
		"summary": summary,
	})

	return err
}

// ReleaseCard releases this client's claim on the card.
func (c *Client) ReleaseCard(ctx context.Context, cardID string) error {
	_, err := c.call(ctx, "release_card", map[string]any{"card_id": cardID})

	return err
}
