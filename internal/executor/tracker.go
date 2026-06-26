// Package executor launches and supervises one Docker container per card. The
// Tracker gates concurrency and records per-run state (awaiting-human, last
// output activity) that the watchdogs consult; DockerExecutor owns the
// container lifecycle behind the Executor interface so a future
// KubernetesExecutor can slot in without touching the serve layer.
package executor

import (
	"io"
	"sync"
	"time"
)

// Run is the in-memory record of one live container. ContainerID is the Docker
// ID; Stdin is the attached stdin stream (control frames flow over it for the
// container's whole life) and is nil in unit tests that exercise the tracker
// without Docker. Stdin is single-writer: callers must serialize writes per
// run — concurrent writers (e.g. webhook handlers on separate HTTP goroutines)
// would interleave frame bytes on the wire.
type Run struct {
	ContainerID string
	CardID      string
	Project     string
	StartedAt   time.Time
	Stdin       io.WriteCloser
}

// Tracker is the concurrency gate and run registry. It is safe for concurrent
// use. Keys are project+"/"+cardID, enforcing one container per card.
type Tracker struct {
	mu           sync.Mutex
	byKey        map[string]*Run
	max          int
	awaiting     map[string]bool
	lastActivity map[string]time.Time
	reason       map[string]string
}

// NewTracker returns a Tracker that admits at most maxConcurrent runs.
func NewTracker(maxConcurrent int) *Tracker {
	return &Tracker{
		byKey:        make(map[string]*Run),
		max:          maxConcurrent,
		awaiting:     make(map[string]bool),
		lastActivity: make(map[string]time.Time),
		reason:       make(map[string]string),
	}
}

func key(project, cardID string) string {
	return project + "/" + cardID
}

// AddIfUnderLimit registers r unless the tracker is at capacity or the run's
// key is already present (one container per card). It returns true when the run
// was admitted, false when it was refused.
func (t *Tracker) AddIfUnderLimit(r *Run) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := key(r.Project, r.CardID)
	if _, exists := t.byKey[k]; exists {
		return false
	}

	if len(t.byKey) >= t.max {
		return false
	}

	t.byKey[k] = r

	return true
}

// Get returns the run for project/cardID and whether it is tracked.
func (t *Tracker) Get(project, cardID string) (*Run, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	r, ok := t.byKey[key(project, cardID)]

	return r, ok
}

// Remove deletes the run and all auxiliary state for project/cardID. It is
// idempotent: removing an absent key is a no-op.
func (t *Tracker) Remove(project, cardID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := key(project, cardID)
	delete(t.byKey, k)
	delete(t.awaiting, k)
	delete(t.lastActivity, k)
	delete(t.reason, k)
}

// List returns a snapshot slice of the tracked runs. Mutating the slice does
// not affect the tracker, but the *Run elements are shared pointers, not deep
// copies — mutating a Run's fields is visible to every other holder.
func (t *Tracker) List() []*Run {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]*Run, 0, len(t.byKey))
	for _, r := range t.byKey {
		out = append(out, r)
	}

	return out
}

// Count returns the number of tracked runs.
func (t *Tracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.byKey)
}

// SetAwaiting records whether project/cardID is awaiting human input. While
// awaiting, the idle watchdog suspends so a human-blocked container is not
// killed for silence.
func (t *Tracker) SetAwaiting(project, cardID string, v bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.awaiting[key(project, cardID)] = v
}

// Awaiting reports whether project/cardID is awaiting human input. Unknown keys
// report false.
func (t *Tracker) Awaiting(project, cardID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.awaiting[key(project, cardID)]
}

// Touch records output activity for project/cardID, resetting the idle timer.
func (t *Tracker) Touch(project, cardID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastActivity[key(project, cardID)] = time.Now()
}

// LastActivity returns the time of the last recorded output for project/cardID,
// or the zero time if none has been recorded.
func (t *Tracker) LastActivity(project, cardID string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.lastActivity[key(project, cardID)]
}

// SetReason records why a container is being terminated so waitAndCleanup can
// label container_duration_seconds. Call it before the SIGKILL it explains. It
// no-ops for an untracked run so a dead key never carries a stale reason.
func (t *Tracker) SetReason(project, cardID, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := key(project, cardID)
	if _, ok := t.byKey[k]; !ok {
		return
	}

	t.reason[k] = reason
}

// Reason returns the recorded termination reason for project/cardID, or "" if
// none was recorded.
func (t *Tracker) Reason(project, cardID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.reason[key(project, cardID)]
}
