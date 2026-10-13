package orchestrator

import (
	"sync"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
)

// modelProgressKind is the liveness event a streaming model turn emits. The
// container's output is the serve-side idle watchdog's only proof of life, and
// a long reasoning turn is otherwise silent until it ends. Not a harness kind:
// the log bridge has no arm for it, so it stays off the live stream and never
// touches the awaiting flag, while the durable run log keeps it.
const modelProgressKind = "model_progress"

// modelProgressInterval is the most often a run emits the marker: far inside
// any workable idle timeout, coarse enough that a long turn adds only a few
// lines to the run log.
const modelProgressInterval = 30 * time.Second

// progressMarker turns streamed model fragments into a throttled liveness
// signal. Fragments are raw model output - unredacted, and a secret can span
// two of them - so the marker never reads them and its event carries no data.
// One marker serves the whole run: mob seats and Best-of-N candidates stream
// concurrently, and the watchdog needs one signal per container, not one per
// model run.
type progressMarker struct {
	emit *events.Emitter
	now  func() time.Time

	mu   sync.Mutex
	last time.Time
}

func newProgressMarker(emit *events.Emitter, now func() time.Time) *progressMarker {
	return &progressMarker{emit: emit, now: now, last: now()}
}

// onDelta is the harness OnDelta callback. It runs synchronously on the model
// run's goroutine, so it does no I/O beyond one throttled event line.
func (p *progressMarker) onDelta(llm.Delta) {
	now := p.now()

	p.mu.Lock()

	due := now.Sub(p.last) >= modelProgressInterval
	if due {
		p.last = now
	}

	p.mu.Unlock()

	if due {
		p.emit.Emit(events.Kind(modelProgressKind), nil)
	}
}
