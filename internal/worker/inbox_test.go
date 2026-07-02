package worker

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/frames"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestInbox creates an Inbox, starts Pump in a background goroutine, and
// returns counters for onPromote and onEndSession callbacks.
func newTestInbox(t *testing.T, hitl bool) (*Inbox, *io.PipeWriter, *atomic.Int32, *atomic.Int32) {
	t.Helper()

	pr, pw := io.Pipe()

	var promotes, ends atomic.Int32

	in := NewInbox(hitl, func() { promotes.Add(1) }, func() { ends.Add(1) })
	go in.Pump(pr)

	return in, pw, &promotes, &ends
}

// sendFrame writes one frame to pw and asserts no error.
func sendFrame(t *testing.T, pw *io.PipeWriter, f frames.Frame) {
	t.Helper()
	require.NoError(t, frames.Write(pw, f))
}

// TestHITLWaitBlocksUntilUserMessage verifies that in HITL mode Wait blocks
// until a user_message frame arrives.
func TestHITLWaitBlocksUntilUserMessage(t *testing.T) {
	t.Parallel()

	in, pw, _, _ := newTestInbox(t, true)
	defer pw.Close()

	ctx := context.Background()
	got := make(chan harness.UserMessage, 1)
	waitErr := make(chan error, 1)

	go func() {
		um, err := in.Wait(ctx)
		got <- um

		waitErr <- err
	}()

	// Give the goroutine time to block before sending.
	time.Sleep(10 * time.Millisecond)

	sendFrame(t, pw, frames.Frame{Type: frames.TypeUserMessage, Content: "hello", MessageID: "m1"})

	select {
	case um := <-got:
		assert.Equal(t, "hello", um.Content)
		assert.Equal(t, "m1", um.MessageID)
		require.NoError(t, <-waitErr)
	case <-time.After(time.Second):
		t.Fatal("Wait did not unblock after user_message frame")
	}
}

// TestAutonomousModeWaitReturnsErrInboxClosed verifies that in autonomous
// mode (hitl=false) Wait returns ErrInboxClosed immediately on an empty queue.
func TestAutonomousModeWaitReturnsErrInboxClosed(t *testing.T) {
	t.Parallel()

	in, pw, _, _ := newTestInbox(t, false)
	defer pw.Close()

	ctx := context.Background()
	_, err := in.Wait(ctx)
	assert.ErrorIs(t, err, harness.ErrInboxClosed)
}

// TestAutonomousModeQueuedMessageDeliveredBeforeClose verifies that in
// autonomous mode a message pumped before Wait is still returned, not dropped.
func TestAutonomousModeQueuedMessageDeliveredBeforeClose(t *testing.T) {
	t.Parallel()

	in, pw, _, _ := newTestInbox(t, false)
	defer pw.Close()

	// Pump a message and wait for it to land in pending before calling Wait.
	sendFrame(t, pw, frames.Frame{Type: frames.TypeUserMessage, Content: "queued", MessageID: "q1"})

	// Poll until the message is queued (Pump is async).
	require.Eventually(t, func() bool {
		in.mu.Lock()
		n := len(in.pending)
		in.mu.Unlock()

		return n == 1
	}, time.Second, 5*time.Millisecond)

	// Wait should return the queued message even though the inbox is pre-closed.
	ctx := context.Background()
	um, err := in.Wait(ctx)
	require.NoError(t, err)
	assert.Equal(t, "queued", um.Content)
	assert.Equal(t, "q1", um.MessageID)

	// Subsequent Wait should return ErrInboxClosed.
	_, err = in.Wait(ctx)
	assert.ErrorIs(t, err, harness.ErrInboxClosed)
}

// TestPromoteFrameUnblocksWaitAndFiresCallback verifies that a promote frame
// causes a blocked Wait to return ErrInboxClosed and fires the onPromote
// callback.
func TestPromoteFrameUnblocksWaitAndFiresCallback(t *testing.T) {
	t.Parallel()

	in, pw, promotes, _ := newTestInbox(t, true)
	defer pw.Close()

	ctx := context.Background()
	waitErr := make(chan error, 1)

	go func() {
		_, err := in.Wait(ctx)
		waitErr <- err
	}()

	time.Sleep(10 * time.Millisecond)

	sendFrame(t, pw, frames.Frame{Type: frames.TypePromote})

	select {
	case err := <-waitErr:
		require.ErrorIs(t, err, harness.ErrInboxClosed)
	case <-time.After(time.Second):
		t.Fatal("Wait did not unblock after promote frame")
	}

	require.Eventually(t, func() bool { return promotes.Load() == 1 }, time.Second, 5*time.Millisecond)
}

// TestEndSessionFrameFiresCallbackAndContextCancelUnblocksWait verifies that
// an end_session frame fires onEndSession and that cancelling ctx (simulating
// what the worker does on end-session) unblocks a waiting Wait with ctx.Err().
func TestEndSessionFrameFiresCallbackAndContextCancelUnblocksWait(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	in, pw, _, ends := newTestInbox(t, true)
	defer pw.Close()

	waitErr := make(chan error, 1)

	go func() {
		_, err := in.Wait(ctx)
		waitErr <- err
	}()

	time.Sleep(10 * time.Millisecond)

	sendFrame(t, pw, frames.Frame{Type: frames.TypeEndSession})

	// Callback must fire.
	require.Eventually(t, func() bool { return ends.Load() == 1 }, time.Second, 5*time.Millisecond)

	// Worker would cancel ctx upon receiving the end-session callback; simulate.
	cancel()

	select {
	case err := <-waitErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Wait did not unblock after ctx cancel")
	}
}

// TestStdinEOFTriggersEndSession verifies that closing the pipe (EOF) is
// treated like end_session: onEndSession fires and a blocked Wait unblocks
// when ctx is cancelled.
func TestStdinEOFTriggersEndSession(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in, pw, _, ends := newTestInbox(t, true)

	waitErr := make(chan error, 1)

	go func() {
		_, err := in.Wait(ctx)
		waitErr <- err
	}()

	time.Sleep(10 * time.Millisecond)

	// Close the pipe — EOF on the reader side.
	pw.Close()

	require.Eventually(t, func() bool { return ends.Load() == 1 }, time.Second, 5*time.Millisecond)

	cancel()

	select {
	case err := <-waitErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Wait did not unblock after EOF + ctx cancel")
	}
}

// TestDrainReturnsQueuedMessagesInOrder verifies that Drain returns all queued
// messages in arrival order and leaves the queue empty.
func TestDrainReturnsQueuedMessagesInOrder(t *testing.T) {
	t.Parallel()

	in, pw, _, _ := newTestInbox(t, true)
	defer pw.Close()

	sendFrame(t, pw, frames.Frame{Type: frames.TypeUserMessage, Content: "first", MessageID: "1"})
	sendFrame(t, pw, frames.Frame{Type: frames.TypeUserMessage, Content: "second", MessageID: "2"})
	sendFrame(t, pw, frames.Frame{Type: frames.TypeUserMessage, Content: "third", MessageID: "3"})

	// Wait until all three messages are queued.
	require.Eventually(t, func() bool {
		in.mu.Lock()
		n := len(in.pending)
		in.mu.Unlock()

		return n == 3
	}, time.Second, 5*time.Millisecond)

	msgs := in.Drain()
	require.Len(t, msgs, 3)
	assert.Equal(t, "first", msgs[0].Content)
	assert.Equal(t, "second", msgs[1].Content)
	assert.Equal(t, "third", msgs[2].Content)

	// Second Drain should be empty.
	assert.Empty(t, in.Drain())
}
