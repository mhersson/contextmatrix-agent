package harness

import (
	"context"
	"errors"
)

// ErrInboxClosed signals no further human input will arrive: the harness
// treats a natural stop as done. A promoted HITL run and an autonomous run
// both surface it from Wait.
var ErrInboxClosed = errors.New("inbox closed")

// UserMessage is one human turn injected mid-run.
type UserMessage struct {
	Content   string
	MessageID string
}

// Inbox feeds human input into a running loop. Drain never blocks; Wait
// blocks until a message arrives, the inbox closes (ErrInboxClosed), or ctx
// is done (ctx.Err()). A nil Inbox on Config preserves autonomous behavior
// exactly. Implementations must be safe for Drain and Wait to be called from
// the run loop concurrently with a producer goroutine feeding messages.
type Inbox interface {
	Drain() []UserMessage
	Wait(ctx context.Context) (UserMessage, error)
}
