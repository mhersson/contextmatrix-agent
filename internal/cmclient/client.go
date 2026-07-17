// Package cmclient is the worker's card-operations client: a minimal MCP
// client over Streamable HTTP against ContextMatrix's /mcp endpoint. Card
// progress stays on MCP by design - the webhook channel is lifecycle-only.
package cmclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrCardNotClaimed is returned by ReleaseCard when ContextMatrix reports the
// card is already unclaimed. Best-effort callers (the worker's cleanup release)
// treat it as a benign no-op: the card is already in the desired released state.
var ErrCardNotClaimed = errors.New("card is not claimed")

// Client is a card-operations client bound to one agent identity. Every method
// is a typed wrapper over a single MCP tool call; all calls carry the agent ID.
type Client struct {
	agentID string

	mu      sync.Mutex
	session *mcp.ClientSession
	dial    func(ctx context.Context) (*mcp.ClientSession, error)
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

// Option configures New.
type Option func(*options)

type options struct {
	base http.RoundTripper
}

// WithBaseTransport sets the RoundTripper the bearer transport wraps. The
// default is http.DefaultTransport; a caller supplies a CA-augmented transport
// so the MCP connection shares the same trust store as the worker's other
// clients. A nil transport is ignored (the default is kept).
func WithBaseTransport(rt http.RoundTripper) Option {
	return func(o *options) {
		if rt != nil {
			o.base = rt
		}
	}
}

// newTransport builds the streamable transport. DisableStandaloneSSE: the
// worker registers no server->client handlers (NewClient gets nil options), so
// the standalone GET stream carries nothing - while its SDK-side retry counter
// only resets on event-ID progress, meaning any 6 idle closes over the session
// lifetime (proxy idle timeouts, CM redeploys, blips) would otherwise
// deterministically poison the whole session.
func newTransport(mcpURL string, httpClient *http.Client) *mcp.StreamableClientTransport {
	return &mcp.StreamableClientTransport{
		Endpoint:             mcpURL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}
}

// New connects to the ContextMatrix MCP endpoint. mcpURL e.g.
// http://host:8080/mcp; apiKey goes out as a bearer header on every request.
// agentID is the worker's identity (convention: cmx-agent-<card-id-lower>).
func New(ctx context.Context, mcpURL, apiKey, agentID string, opts ...Option) (*Client, error) {
	o := options{base: http.DefaultTransport}
	for _, opt := range opts {
		opt(&o)
	}

	httpClient := &http.Client{
		Transport: &bearerTransport{apiKey: apiKey, base: o.base},
	}

	dial := func(ctx context.Context) (*mcp.ClientSession, error) {
		client := mcp.NewClient(&mcp.Implementation{Name: "contextmatrix-agent-worker", Version: "0.1.0"}, nil)

		session, err := client.Connect(ctx, newTransport(mcpURL, httpClient), nil)
		if err != nil {
			return nil, fmt.Errorf("connect to mcp endpoint: %w", err)
		}

		return session, nil
	}

	session, err := dial(ctx)
	if err != nil {
		return nil, err
	}

	return &Client{session: session, agentID: agentID, dial: dial}, nil
}

// Close ends the MCP session. The underlying SDK session Close is idempotent
// and concurrency safe, so double-close is safe. The mutex serialises Close
// against a concurrent reconnect swapping c.session.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.session.Close(); err != nil {
		return fmt.Errorf("close mcp session: %w", err)
	}

	return nil
}

// callTool is the single session chokepoint: on a poisoned session
// (ErrConnectionClosed from the SDK's client/server-closing states, or
// ErrSessionMissing when CM restarted and forgot the session) it re-dials a
// fresh session ONCE and retries the call once. Tool-level IsError results
// (which come back as err==nil here) and context cancellation never trigger a
// re-dial. Single-flight: concurrent callers piggyback on the first re-dial via
// the c.session == sess guard.
func (c *Client) callTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	c.mu.Lock()
	sess := c.session
	c.mu.Unlock()

	result, err := sess.CallTool(ctx, params)
	if err == nil || (!errors.Is(err, mcp.ErrConnectionClosed) && !errors.Is(err, mcp.ErrSessionMissing)) {
		return result, err
	}

	c.mu.Lock()
	if c.session == sess {
		fresh, derr := c.dial(ctx)
		if derr != nil {
			c.mu.Unlock()

			return nil, fmt.Errorf("reconnect after poisoned session: %w", errors.Join(err, derr))
		}

		_ = sess.Close()
		c.session = fresh

		slog.Warn("cmclient: mcp session poisoned; reconnected with a fresh session", "tool", params.Name)
	}

	sess = c.session
	c.mu.Unlock()

	return sess.CallTool(ctx, params)
}

// ImageBlob is one inline card image fetched over MCP.
type ImageBlob struct {
	MIME string
	Data []byte
}

// TaskContext is the subset of get_task_context the worker and orchestrator
// need. Parent, siblings, and project config from the server response are
// intentionally ignored.
type TaskContext struct {
	// Base fields - populated for all cards.
	Title       string
	Description string
	State       string

	// Type and Labels classify the card (bug vs feature vs maintenance) so the
	// orchestrator's plan phase can pick a pre-plan branch. Populated from the
	// card JSON get_task_context already returns.
	Type   string
	Labels []string

	// Orchestrator fields - populated for autonomous cards; zero-valued when the
	// card JSON omits them.
	Phase             string
	Autonomous        bool
	CreatePR          bool
	ReviewAttempts    int
	ModelOrchestrator string
	ModelCoder        string
	ModelReviewer     string
	// ReportedCostUSD is seeded from token_usage.estimated_cost_usd at context
	// fetch time. The orchestrator uses this as the budget ledger's starting
	// value so prior sub-agent spend is included in cost accounting.
	ReportedCostUSD float64

	// Images are the primary card body's inline images, fetched with
	// include_images:true. nil when the card has none.
	Images []ImageBlob
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
	maps.Copy(withAgent, args)

	withAgent["agent_id"] = c.agentID

	result, err := c.callTool(ctx, &mcp.CallToolParams{
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

// callFull invokes a tool and returns the full result so callers can read
// non-text content (e.g. ImageContent). Mirrors call's agent_id injection and
// IsError surfacing.
func (c *Client) callFull(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	withAgent := make(map[string]any, len(args)+1)
	maps.Copy(withAgent, args)

	withAgent["agent_id"] = c.agentID

	result, err := c.callTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: withAgent})
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", tool, err)
	}

	if result.IsError {
		return nil, fmt.Errorf("call %s: %s", tool, firstText(result))
	}

	return result, nil
}

// cardImages extracts inline ImageContent blocks from an MCP result, skipping
// any blob with empty data or MIME type (and warning about each skip).
func cardImages(result *mcp.CallToolResult) []ImageBlob {
	var out []ImageBlob

	for _, content := range result.Content {
		ic, ok := content.(*mcp.ImageContent)
		if !ok {
			continue
		}

		if len(ic.Data) == 0 {
			slog.Warn("skipping card image blob: empty data")

			continue
		}

		if ic.MIMEType == "" {
			slog.Warn("skipping card image blob: empty MIME type")

			continue
		}

		out = append(out, ImageBlob{MIME: ic.MIMEType, Data: ic.Data})
	}

	return out
}

// ClaimCard claims the card for this client's agent.
func (c *Client) ClaimCard(ctx context.Context, cardID string) error {
	_, err := c.call(ctx, "claim_card", map[string]any{"card_id": cardID})

	return err
}

// GetTaskContext fetches the card context, parsing the card portion of the
// get_task_context response. Pass includeImages=true to request inline image
// blobs alongside the text payload (orchestrator bootstrap); pass false for
// callers that read only scalar fields and do not use the images (worker
// bootstrap).
func (c *Client) GetTaskContext(ctx context.Context, cardID string, includeImages bool) (TaskContext, error) {
	result, err := c.callFull(ctx, "get_task_context", map[string]any{
		"card_id":        cardID,
		"include_images": includeImages,
	})
	if err != nil {
		return TaskContext{}, err
	}

	text := firstText(result)

	// Mirrors the server's get_task_context output: {card, parent, siblings,
	// config}. Only the card is parsed; parent/siblings/config are ignored.
	var payload struct {
		Card struct {
			Title             string   `json:"title"`
			Body              string   `json:"body"`
			State             string   `json:"state"`
			Type              string   `json:"type"`
			Labels            []string `json:"labels"`
			Phase             string   `json:"phase"`
			Autonomous        bool     `json:"autonomous"`
			CreatePR          bool     `json:"create_pr"`
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
		Title:             payload.Card.Title,
		Description:       payload.Card.Body,
		State:             payload.Card.State,
		Type:              payload.Card.Type,
		Labels:            payload.Card.Labels,
		Phase:             payload.Card.Phase,
		Autonomous:        payload.Card.Autonomous,
		CreatePR:          payload.Card.CreatePR,
		ReviewAttempts:    payload.Card.ReviewAttempts,
		ModelOrchestrator: payload.Card.ModelOrchestrator,
		ModelCoder:        payload.Card.ModelCoder,
		ModelReviewer:     payload.Card.ModelReviewer,
		Images:            cardImages(result),
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

// ModelOutcome is one Best-of-N candidate's result, reported on the parent
// card via ReportModelOutcomes.
type ModelOutcome struct {
	Model       string  `json:"model"`
	Result      string  `json:"result"` // win | loss | failed
	VerifyPass  bool    `json:"verify_pass"`
	CostUSD     float64 `json:"cost_usd"`
	NCandidates int     `json:"n_candidates"`
	JudgeModel  string  `json:"judge_model,omitempty"`
}

// ReportModelOutcomes records per-candidate Best-of-N results on the parent
// card via the report_model_outcome tool.
func (c *Client) ReportModelOutcomes(ctx context.Context, cardID string, outcomes []ModelOutcome) error {
	rows := make([]map[string]any, 0, len(outcomes))
	for _, o := range outcomes {
		row := map[string]any{
			"model":        o.Model,
			"result":       o.Result,
			"verify_pass":  o.VerifyPass,
			"cost_usd":     o.CostUSD,
			"n_candidates": o.NCandidates,
		}
		if o.JudgeModel != "" {
			row["judge_model"] = o.JudgeModel
		}

		rows = append(rows, row)
	}

	_, err := c.call(ctx, "report_model_outcome", map[string]any{
		"card_id":  cardID,
		"outcomes": rows,
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

// ReleaseCard releases this client's claim on the card. A release that finds the
// card already unclaimed returns ErrCardNotClaimed (wrapped), so best-effort
// callers can treat the redundant release as a benign no-op.
func (c *Client) ReleaseCard(ctx context.Context, cardID string) error {
	_, err := c.call(ctx, "release_card", map[string]any{"card_id": cardID})

	return classifyReleaseError(err)
}

// classifyReleaseError maps ContextMatrix's "card is not claimed" release error
// to the typed ErrCardNotClaimed sentinel, preserving the original message in the
// chain. Other errors pass through unchanged; nil stays nil.
func classifyReleaseError(err error) error {
	if err == nil {
		return nil
	}

	if strings.Contains(err.Error(), ErrCardNotClaimed.Error()) {
		return fmt.Errorf("%w: %w", ErrCardNotClaimed, err)
	}

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

// UpdateCardBody replaces the card body via update_card. The orchestrator uses
// it to record the run's history (diagnosis, plan, review rounds) on the parent
// card. Only the body field is sent, so a concurrent phase update is not
// clobbered - CM patches only the provided fields.
func (c *Client) UpdateCardBody(ctx context.Context, cardID, body string) error {
	_, err := c.call(ctx, "update_card", map[string]any{
		"card_id": cardID,
		"body":    body,
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
// skill payload the server returns is intentionally not surfaced - the
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

// BlacklistModel reports a harness-incapable model so CM never auto-selects it.
func (c *Client) BlacklistModel(ctx context.Context, cardID, model, reason string) error {
	_, err := c.call(ctx, "report_incapable_model", map[string]any{
		"model_slug":     model,
		"reason":         reason,
		"sample_card_id": cardID,
	})

	return err
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

// RecordSkillEngaged appends a "skill_engaged" activity log entry via the MCP
// add_log tool. ContextMatrix records it as a deduped skill_engaged entry
// (RecordSkillEngaged/skillNameOf parse "engaged <skill>" from the message) and
// publishes card.log_added for the dashboard - the agent's report path, distinct
// from AddLog's status_update. The call helper injects agent_id.
func (c *Client) RecordSkillEngaged(ctx context.Context, cardID, skillName string) error {
	_, err := c.call(ctx, "add_log", map[string]any{
		"card_id": cardID,
		"action":  "skill_engaged",
		"message": "engaged " + skillName,
	})

	return err
}
