// Package attempt stamps a per-card attempt ordinal onto the run transcript.
//
// The harness event sequence restarts at 1 for every container run, so when a
// card's container is restarted the card's durable log holds two runs whose
// sequence numbers overlap completely and nothing in the envelope tells them
// apart. The ordinal counts a card's container runs - a notion the harness,
// which sees one loop invocation, does not have - so it is stamped here, on
// this side of the boundary. NewWriter treats an ordinal of 1 or less as the
// first attempt and stamps nothing, so a transcript written before the field
// existed and a first-attempt transcript written after are indistinguishable
// on the wire - both simply carry no attempt field.
//
// The work command's main transcript stream stamps via the harness emitter's
// own envelope-field option instead of this package's writer; the writer here
// remains the stamping point for the seat-debug funnel, which has no emitter
// of its own to configure.
package attempt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Field is the transcript envelope key carrying the ordinal.
const Field = "attempt"

// NewWriter returns a writer that stamps ordinal onto every complete JSON
// object line it forwards. An ordinal of 1 or less is the first attempt and
// needs no mark, so w is returned unwrapped.
//
// The transform is line-oriented and non-destructive: a line that is not
// exactly one JSON object is forwarded byte for byte, and a line split across
// Write calls is held until its newline arrives. Writes are serialized: the
// transcript stream has more than one writer on it, and the held partial line
// is shared state that a raw file handle did not have.
func NewWriter(w io.Writer, ordinal int) io.Writer {
	if ordinal <= 1 {
		return w
	}

	return &writer{w: w, ordinal: ordinal}
}

type writer struct {
	w       io.Writer
	ordinal int

	mu sync.Mutex

	pending []byte // a line whose newline has not arrived yet
	out     []byte // reused so each line leaves in a single Write
}

func (a *writer) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	rest := p

	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			a.pending = append(a.pending, rest...)

			return len(p), nil
		}

		line := rest[:i]
		rest = rest[i+1:]

		if len(a.pending) > 0 {
			a.pending = append(a.pending, line...)
			line = a.pending
		}

		// Copy into out rather than appending to line: line can alias the
		// caller's buffer, which a transform must never write through. The
		// terminator goes out with the line so the stream stays line-atomic
		// for whoever else writes to it.
		a.out = append(a.out[:0], a.stamp(line)...)
		a.out = append(a.out, '\n')

		a.pending = a.pending[:0]

		if _, err := a.w.Write(a.out); err != nil {
			return len(p) - len(rest), err
		}
	}
}

// stamp returns line with the ordinal inserted, or line untouched when it is
// not exactly one JSON object. Numbers decode as json.Number so the re-encoded
// line keeps the digits it arrived with.
func (a *writer) stamp(line []byte) []byte {
	if len(line) == 0 || line[0] != '{' {
		return line
	}

	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()

	var entry map[string]any

	if err := dec.Decode(&entry); err != nil || entry == nil {
		return line
	}

	// Content after the object means this line is not one event: forward it
	// rather than re-encode a partial reading of it.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return line
	}

	entry[Field] = a.ordinal

	b, err := json.Marshal(entry)
	if err != nil {
		return line
	}

	return b
}
