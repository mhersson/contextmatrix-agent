// Package worker orchestrates one card-scoped run inside a container: stdin
// control frames, ContextMatrix card operations over MCP, the harness loop,
// and the end-of-run git workflow.
package worker

import (
	"context"
	"io"
	"sync"

	"github.com/mhersson/contextmatrix-backendkit/frames"
	"github.com/mhersson/contextmatrix-harness/harness"
)

// Inbox adapts stdin control frames to harness.Inbox. In autonomous mode (or
// after a promote frame) Wait reports ErrInboxClosed so a natural stop means
// done; in HITL mode Wait blocks for the next human turn.
type Inbox struct {
	mu      sync.Mutex
	pending []harness.UserMessage
	closed  bool // autonomous, or promoted
	signal  chan struct{}
	onProm  func()
	onEnd   func()
}

// NewInbox constructs an Inbox. hitl=false means autonomous (pre-closed).
// onPromote fires when a promote frame arrives; onEndSession fires on
// end_session or EOF.
//
// Liveness contract: end_session and EOF fire onEndSession and stop the pump
// WITHOUT closing the inbox or waking waiters. The onEndSession callback MUST
// cancel the context passed to Wait, or a parked Wait hangs forever.
func NewInbox(hitl bool, onPromote, onEndSession func()) *Inbox {
	return &Inbox{
		closed: !hitl,
		signal: make(chan struct{}, 1),
		onProm: onPromote,
		onEnd:  onEndSession,
	}
}

// Pump reads frames until EOF or error. Run it in a goroutine; EOF or
// end_session triggers onEndSession (closing stdin ends the session).
func (in *Inbox) Pump(r io.Reader) {
	fr := frames.NewReader(r, frames.TypeUserMessage, frames.TypePromote, frames.TypeEndSession)

	for {
		f, err := fr.Next()
		if err != nil { // io.EOF or a scanner error: session over either way
			in.onEnd()

			return
		}

		switch f.Type {
		case frames.TypeUserMessage:
			in.mu.Lock()
			in.pending = append(in.pending, harness.UserMessage{Content: f.Content, MessageID: f.MessageID})
			in.mu.Unlock()
			in.ping()
		case frames.TypePromote:
			in.mu.Lock()
			in.closed = true
			in.mu.Unlock()
			in.onProm()
			in.ping()
		case frames.TypeEndSession:
			in.onEnd()

			return
		}
	}
}

// ping sends a non-blocking signal to wake a waiting Wait call.
func (in *Inbox) ping() {
	select {
	case in.signal <- struct{}{}:
	default:
	}
}

// Drain returns all queued messages in order and empties the queue. Never
// blocks.
func (in *Inbox) Drain() []harness.UserMessage {
	in.mu.Lock()
	defer in.mu.Unlock()

	out := in.pending
	in.pending = nil

	return out
}

// Wait blocks until a message is available, the inbox is closed
// (ErrInboxClosed), or ctx is done (ctx.Err()). Queued messages are always
// delivered before ErrInboxClosed, even in autonomous or post-promote mode.
func (in *Inbox) Wait(ctx context.Context) (harness.UserMessage, error) {
	for {
		in.mu.Lock()
		if len(in.pending) > 0 {
			um := in.pending[0]
			in.pending = in.pending[1:]
			in.mu.Unlock()

			return um, nil
		}

		closed := in.closed
		in.mu.Unlock()

		if closed {
			return harness.UserMessage{}, harness.ErrInboxClosed
		}

		select {
		case <-ctx.Done():
			return harness.UserMessage{}, ctx.Err()
		case <-in.signal:
		}
	}
}
