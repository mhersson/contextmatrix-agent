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

// TaskContext is the subset of get_task_context the worker and orchestrator
// need. Parent, siblings, and project config from the server response are
// intentionally ignored.
type TaskContext struct {
	// Base fields — populated for all cards.
	CardID      string
	Title       string
	Description string
	State       string

	// Type and Labels classify the card (bug vs feature vs maintenance) so the
	// orchestrator's plan phase can pick a pre-plan branch. Populated from the
	// card JSON get_task_context already returns.
	Type   string
	Labels []string

	// Orchestrator fields — populated for autonomous cards (may be zero-valued
	// for cards that were created before the C2 phase fields were added).
	Phase             string
	Autonomous        bool
	CreatePR          bool
	BaseBranch        string
	ReviewAttempts    int
	ModelOrchestrator string
	ModelCoder        string
	ModelReviewer     string
	// ReportedCostUSD is seeded from token_usage.estimated_cost_usd at context
	// fetch time. The orchestrator uses this as the budget ledger's starting
	// value so prior sub-agent spend is included in cost accounting.
	ReportedCostUSD float64
}

// SubtaskState is the per-card state summary returned by SubtaskStates.
type SubtaskState struct {
	CardID string
	Title  string
	State  string
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

// GetTaskContext fetches the card context, parsing the card portion of the
// get_task_context response. include_images is forced false: the worker reads
// card text, not inline image bytes.
func (c *Client) GetTaskContext(ctx context.Context, cardID string) (TaskContext, error) {
	text, err := c.call(ctx, "get_task_context", map[string]any{
		"card_id":        cardID,
		"include_images": false,
	})
	if err != nil {
		return TaskContext{}, err
	}

	// Mirrors the server's get_task_context output: {card, parent, siblings,
	// config}. Only the card is parsed; parent/siblings/config are ignored.
	var payload struct {
		Card struct {
			ID                string   `json:"id"`
			Title             string   `json:"title"`
			Body              string   `json:"body"`
			State             string   `json:"state"`
			Type              string   `json:"type"`
			Labels            []string `json:"labels"`
			Phase             string   `json:"phase"`
			Autonomous        bool     `json:"autonomous"`
			CreatePR          bool     `json:"create_pr"`
			BaseBranch        string   `json:"base_branch"`
			ReviewAttempts    int      `json:"review_attempts"`
			ModelOrchestrator string   `json:"model_orchestrator"`
			ModelCoder        string   `json:"model_coder"`
			ModelReviewer     string   `json:"model_reviewer"`
			TokenUsage        *struct {
				EstimatedCostUSD float64 `json:"estimated_cost_usd"`
			} `json:"token_usage"`
		} `json:"card"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return TaskContext{}, fmt.Errorf("parse task context: %w", err)
	}

	tc := TaskContext{
		CardID:            payload.Card.ID,
		Title:             payload.Card.Title,
		Description:       payload.Card.Body,
		State:             payload.Card.State,
		Type:              payload.Card.Type,
		Labels:            payload.Card.Labels,
		Phase:             payload.Card.Phase,
		Autonomous:        payload.Card.Autonomous,
		CreatePR:          payload.Card.CreatePR,
		BaseBranch:        payload.Card.BaseBranch,
		ReviewAttempts:    payload.Card.ReviewAttempts,
		ModelOrchestrator: payload.Card.ModelOrchestrator,
		ModelCoder:        payload.Card.ModelCoder,
		ModelReviewer:     payload.Card.ModelReviewer,
	}
	if payload.Card.TokenUsage != nil {
		tc.ReportedCostUSD = payload.Card.TokenUsage.EstimatedCostUSD
	}

	return tc, nil
}

// Heartbeat reports liveness for the card.
func (c *Client) Heartbeat(ctx context.Context, cardID string) error {
	_, err := c.call(ctx, "heartbeat", map[string]any{"card_id": cardID})

	return err
}

// ReportUsage records token usage for cost tracking. model may be empty, in
// which case it is omitted and the server applies its default. actualCostUSD
// is the authoritative provider-reported cost; it is omitted when zero so
// the server uses its rate table.
func (c *Client) ReportUsage(ctx context.Context, cardID, model string, promptTokens, completionTokens int64, actualCostUSD float64) error {
	args := map[string]any{
		"card_id":           cardID,
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
	}
	if model != "" {
		args["model"] = model
	}

	if actualCostUSD != 0 {
		args["actual_cost_usd"] = actualCostUSD
	}

	_, err := c.call(ctx, "report_usage", args)

	return err
}

// ReportPush records that work was pushed to the given git branch. prURL may
// be empty when no PR was created; it is omitted from the wire in that case.
func (c *Client) ReportPush(ctx context.Context, cardID, branch, prURL string) error {
	args := map[string]any{
		"card_id": cardID,
		"branch":  branch,
	}
	if prURL != "" {
		args["pr_url"] = prURL
	}

	_, err := c.call(ctx, "report_push", args)

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

// CreateCard creates a new subtask card under parent in the given project.
// body is the markdown description. dependsOn is the list of card IDs this
// card depends on; nil omits the field. Returns the server-assigned card ID.
func (c *Client) CreateCard(ctx context.Context, project, parent, title, body string, dependsOn []string) (string, error) {
	args := map[string]any{
		"project":  project,
		"parent":   parent,
		"title":    title,
		"body":     body,
		"type":     "task",
		"priority": "medium",
	}
	if len(dependsOn) > 0 {
		args["depends_on"] = dependsOn
	}

	text, err := c.call(ctx, "create_card", args)
	if err != nil {
		return "", err
	}

	// create_card returns the full board.Card JSON; extract the id field.
	var card struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &card); err != nil {
		return "", fmt.Errorf("parse create_card response: %w", err)
	}

	return card.ID, nil
}

// SetPhase sets the orchestrator phase on the card via update_card.
func (c *Client) SetPhase(ctx context.Context, cardID, phase string) error {
	_, err := c.call(ctx, "update_card", map[string]any{
		"card_id": cardID,
		"phase":   phase,
	})

	return err
}

// TransitionCard changes the card's state via transition_card.
func (c *Client) TransitionCard(ctx context.Context, cardID, state string) error {
	_, err := c.call(ctx, "transition_card", map[string]any{
		"card_id":   cardID,
		"new_state": state,
	})

	return err
}

// StartReview atomically transitions the card to review via start_review. The
// skill payload the server returns is intentionally not surfaced — the
// orchestrator drives its own review flow.
func (c *Client) StartReview(ctx context.Context, cardID string) error {
	_, err := c.call(ctx, "start_review", map[string]any{"card_id": cardID})

	return err
}

// IncrementReviewAttempts increments the review_attempts counter and returns
// the new count.
func (c *Client) IncrementReviewAttempts(ctx context.Context, cardID string) (int, error) {
	text, err := c.call(ctx, "increment_review_attempts", map[string]any{"card_id": cardID})
	if err != nil {
		return 0, err
	}

	// increment_review_attempts returns {card: <board.Card>}.
	var payload struct {
		Card struct {
			ReviewAttempts int `json:"review_attempts"`
		} `json:"card"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return 0, fmt.Errorf("parse increment_review_attempts response: %w", err)
	}

	return payload.Card.ReviewAttempts, nil
}

// SubtaskStates returns the per-card state for every subtask of the given
// parent card. It uses list_cards with a parent filter so the orchestrator
// can determine which subtasks are done without loading the full card bodies.
// project is mandatory: list_cards declares it as a required schema field
// with no card-ID resolution fallback.
func (c *Client) SubtaskStates(ctx context.Context, project, parentID string) ([]SubtaskState, error) {
	text, err := c.call(ctx, "list_cards", map[string]any{
		"project": project,
		"parent":  parentID,
	})
	if err != nil {
		return nil, err
	}

	// list_cards returns {cards: [...]}.
	var payload struct {
		Cards []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			State string `json:"state"`
		} `json:"cards"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, fmt.Errorf("parse list_cards response: %w", err)
	}

	states := make([]SubtaskState, 0, len(payload.Cards))
	for _, card := range payload.Cards {
		states = append(states, SubtaskState{
			CardID: card.ID,
			Title:  card.Title,
			State:  card.State,
		})
	}

	return states, nil
}

// AddLog appends a status_update activity log entry to the card.
func (c *Client) AddLog(ctx context.Context, cardID, message string) error {
	_, err := c.call(ctx, "add_log", map[string]any{
		"card_id": cardID,
		"action":  "status_update",
		"message": message,
	})

	return err
}
