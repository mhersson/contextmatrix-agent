package mob

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// errTurnTimeout marks one seat turn exceeding its deadline. The seat is
// absent for the round and rejoins the next one on a replacement task.
var errTurnTimeout = errors.New("mob: turn deadline exceeded")

// cancelBudget bounds the best-effort CancelTask issued after a timeout.
const cancelBudget = 2 * time.Second

// closeBudget bounds the best-effort close message per seat.
const closeBudget = 5 * time.Second

// seatHandle is the moderator's side of one seat's A2A conversation.
type seatHandle struct {
	name     string
	lens     string
	guest    bool
	client   *a2aclient.Client
	taskID   a2a.TaskID
	deadline time.Duration
	absent   bool // absent this round (timeout/error); may rejoin next round
	// dead marks a seat permanently gone (open failed). A failed dial never
	// returns a handle from dialSeat/dialGuest; the engine constructs the
	// dead placeholder itself and reads it when skipping the seat across
	// rounds.
	dead bool

	// bearer authenticates every call: the loopback server token for
	// internal seats, the registry token for guests.
	bearer string
	// lastCost is the most recent successful turn's USD cost, read off the
	// utterance metadata (internal seats only; guests never set it).
	lastCost float64
}

// dialSeat connects to one internal seat endpoint. a2a-go builds the
// transport lazily, so failures surface on the first call, not here.
func dialSeat(ctx context.Context, name, lens, endpoint, bearer string) (*seatHandle, error) {
	client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{
		a2a.NewAgentInterface(endpoint, a2a.TransportProtocolJSONRPC),
	}, a2aclient.WithJSONRPCTransport(http.DefaultClient))
	if err != nil {
		return nil, fmt.Errorf("mob: dial seat %s: %w", name, err)
	}

	return &seatHandle{
		name:     name,
		lens:     lens,
		client:   client,
		deadline: internalTurnDeadline,
		bearer:   bearer,
	}, nil
}

// dialGuest resolves {url}/.well-known/agent-card.json and connects from the
// card. A registered-but-unreachable guest fails here — the engine records
// it as a dead seat and the discussion proceeds on quorum.
func dialGuest(ctx context.Context, g GuestSeat) (*seatHandle, error) {
	var opts []agentcard.ResolveOption
	if g.Token != "" {
		opts = append(opts, agentcard.WithRequestHeader("Authorization", "Bearer "+g.Token))
	}

	card, err := agentcard.NewResolver(http.DefaultClient).Resolve(ctx, g.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("mob: resolve guest %s card: %w", g.Name, err)
	}

	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(http.DefaultClient))
	if err != nil {
		return nil, fmt.Errorf("mob: dial guest %s: %w", g.Name, err)
	}

	return &seatHandle{
		name:     "guest-" + g.Name,
		guest:    true,
		client:   client,
		deadline: guestTurnDeadline,
		bearer:   g.Token,
	}, nil
}

// authCtx attaches the handle's bearer as an Authorization service param —
// the JSON-RPC transport sends it as an HTTP header.
func (h *seatHandle) authCtx(ctx context.Context) context.Context {
	if h.bearer == "" {
		return ctx
	}

	return a2aclient.AttachServiceParams(ctx, a2aclient.ServiceParams{
		"Authorization": {"Bearer " + h.bearer},
	})
}

// sendTurn sends one round message and waits for the utterance. body is the
// rendered delta (or the full snapshot for a replacement task); round >= 0.
// On success the handle's taskID is recorded/refreshed and the truncated
// utterance returned. On the deadline expiring: best-effort CancelTask
// (detached context, 2 s budget), clear the task so the seat rejoins next
// round on a replacement task, mark absent, return errTurnTimeout.
func (h *seatHandle) sendTurn(ctx context.Context, round int, body string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, h.deadline)
	defer cancel()

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(body))
	setControl(msg, control{Kind: controlRound, Round: round})

	if h.taskID != "" {
		msg.TaskID = h.taskID
	}

	result, err := h.client.SendMessage(h.authCtx(callCtx), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		if callCtx.Err() != nil {
			// a2a-go executions run on a detached context: the client-side
			// deadline did NOT stop the seat's runner. Cancel explicitly so
			// it stops burning tokens.
			h.cancelInFlight()
			h.taskID = ""
			h.absent = true

			return "", errTurnTimeout
		}

		h.absent = true

		return "", fmt.Errorf("mob: send turn to %s: %w", h.name, err)
	}

	task, ok := result.(*a2a.Task)
	if !ok {
		h.absent = true

		return "", fmt.Errorf("mob: seat %s returned %T, want *a2a.Task", h.name, result)
	}

	if task.Status.State.Terminal() {
		// failed/canceled/rejected: the task can carry no further messages.
		h.taskID = ""
		h.absent = true

		return "", fmt.Errorf("mob: seat %s task ended in state %s", h.name, task.Status.State)
	}

	if task.Status.Message == nil {
		h.absent = true

		return "", fmt.Errorf("mob: seat %s returned no utterance", h.name)
	}

	h.taskID = task.ID
	h.lastCost = costFrom(task.Status.Message)
	h.absent = false

	return truncateUtterance(messageText(task.Status.Message)), nil
}

// cancelInFlight issues a best-effort CancelTask for the current task on a
// detached context. A first-turn timeout has no task ID yet (the blocking
// SendMessage never returned one) — that orphaned run is bounded by the seat
// harness's MaxTurns and MaxCostUSD instead.
func (h *seatHandle) cancelInFlight() {
	if h.taskID == "" {
		return
	}

	cctx, cancel := context.WithTimeout(context.Background(), cancelBudget)
	defer cancel()

	_, _ = h.client.CancelTask(h.authCtx(cctx), &a2a.CancelTaskRequest{ID: h.taskID}) //nolint:errcheck // best-effort
}

// closeSeat sends the close control message on the handle's task (errors
// ignored — close is a courtesy) and destroys the client.
func (h *seatHandle) closeSeat(ctx context.Context) {
	if h.client == nil {
		return
	}

	if h.taskID != "" {
		cctx, cancel := context.WithTimeout(ctx, closeBudget)

		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("The discussion is closed. Thank you."))
		setControl(msg, control{Kind: controlClose})
		msg.TaskID = h.taskID

		_, _ = h.client.SendMessage(h.authCtx(cctx), &a2a.SendMessageRequest{Message: msg}) //nolint:errcheck // best-effort close

		cancel()
	}

	_ = h.client.Destroy() //nolint:errcheck // in-process cleanup
}
