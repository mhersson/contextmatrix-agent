// Package executor launches and supervises one Docker container per card run
// and per run kind of the card. The Tracker gates concurrency and records
// per-run state (awaiting-human, last output activity) that the watchdogs
// consult; DockerExecutor owns the container lifecycle behind the Executor
// interface so a future KubernetesExecutor can slot in without touching the
// serve layer.
package executor

import (
	"io"
	"sync"
	"time"
)

// Run is the in-memory record of one live container. ContainerID is the Docker
// ID; CorrelationID is the launch spec's per-trigger id, which keys the run's
// proxy token and other per-run state; Stdin is the attached stdin stream
// (control frames flow over it for the container's whole life) and is nil in
// unit tests that exercise the tracker without Docker. Stdin is single-writer:
// callers must serialize writes per run - concurrent writers (e.g. webhook
// handlers on separate HTTP goroutines) would interleave frame bytes on the
// wire.
type Run struct {
	ContainerID   string
	CardID        string
	Project       string
	Kind          string
	CorrelationID string
	StartedAt     time.Time
	Stdin         io.WriteCloser
}

// RunKey identifies one run: Kind is empty for a card run.
type RunKey struct {
	Project string
	CardID  string
	Kind    string
}

// Name is the segment that names the run's container, credentials dir and log
// file. A run kind's name can never equal a card ID, which ends in digits.
func (k RunKey) Name() string {
	if k.Kind == "" {
		return k.CardID
	}

	return k.CardID + "." + k.Kind
}

// Key returns the run's key.
func (r *Run) Key() RunKey {
	return RunKey{Project: r.Project, CardID: r.CardID, Kind: r.Kind}
}

// Tracker is the concurrency gate and run registry. It is safe for concurrent
// use. Keys are RunKeys, enforcing one container per card run and per run kind.
type Tracker struct {
	mu           sync.Mutex
	byKey        map[RunKey]*Run
	max          int
	awaiting     map[RunKey]bool
	lastActivity map[RunKey]time.Time
	reason       map[RunKey]string
}

// NewTracker returns a Tracker that admits at most maxConcurrent runs.
func NewTracker(maxConcurrent int) *Tracker {
	return &Tracker{
		byKey:        make(map[RunKey]*Run),
		max:          maxConcurrent,
		awaiting:     make(map[RunKey]bool),
		lastActivity: make(map[RunKey]time.Time),
		reason:       make(map[RunKey]string),
	}
}

// AddIfUnderLimit registers r unless the tracker is at capacity or the run's
// key is already present (one container per run key). It returns true when the run
// was admitted, false when it was refused.
func (t *Tracker) AddIfUnderLimit(r *Run) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := r.Key()
	if _, exists := t.byKey[k]; exists {
		return false
	}

	if len(t.byKey) >= t.max {
		return false
	}

	t.byKey[k] = r

	return true
}

// Get returns the run under k and whether it is tracked.
func (t *Tracker) Get(k RunKey) (*Run, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	r, ok := t.byKey[k]

	return r, ok
}

// Remove deletes the run under k and all its auxiliary state. It is
// idempotent: removing an absent key is a no-op.
func (t *Tracker) Remove(k RunKey) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.byKey, k)
	delete(t.awaiting, k)
	delete(t.lastActivity, k)
	delete(t.reason, k)
}

// List returns a snapshot slice of the tracked runs. Mutating the slice does
// not affect the tracker, but the *Run elements are shared pointers, not deep
// copies - mutating a Run's fields is visible to every other holder.
func (t *Tracker) List() []*Run {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]*Run, 0, len(t.byKey))
	for _, r := range t.byKey {
		out = append(out, r)
	}

	return out
}

func (t *Tracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.byKey)
}

// SetAwaiting records whether the run is awaiting human input. While
// awaiting, the idle watchdog suspends so a human-blocked container is not
// killed for silence.
//
// It no-ops for an untracked run, like Touch and SetReason: the exit path
// removes the run before the last entries it publishes reach the bridge, and
// the bridge clears awaiting on every entry it publishes, so an unguarded
// write would leave an entry behind for every completed run.
func (t *Tracker) SetAwaiting(k RunKey, v bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.byKey[k]; !ok {
		return
	}

	t.awaiting[k] = v
}

// Awaiting reports whether the run is awaiting human input. Unknown keys
// report false.
func (t *Tracker) Awaiting(k RunKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.awaiting[k]
}

// Touch records output activity for the run, resetting the idle timer.
// It no-ops for an untracked run, like SetReason: waitAndCleanup removes the
// run before the output pump has finished draining, and an unguarded write
// would resurrect a lastActivity entry that nothing reclaims.
func (t *Tracker) Touch(k RunKey) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.byKey[k]; !ok {
		return
	}

	t.lastActivity[k] = time.Now()
}

// LastActivity returns the time of the last recorded output for the run,
// or the zero time if none has been recorded.
func (t *Tracker) LastActivity(k RunKey) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.lastActivity[k]
}

// SetReason records why a container is being terminated so waitAndCleanup can
// label container_duration_seconds. Call it before the SIGKILL it explains. It
// no-ops for an untracked run so a dead key never carries a stale reason.
func (t *Tracker) SetReason(k RunKey, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.byKey[k]; !ok {
		return
	}

	t.reason[k] = reason
}

// Reason returns the recorded termination reason for the run, or "" if
// none was recorded.
func (t *Tracker) Reason(k RunKey) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.reason[k]
}
