// Package frames defines the JSON Lines control protocol written to a worker
// container's stdin by the host service and read by the work command. It is
// internal to this repo — both ends are our code — so it is NOT part of
// contextmatrix-protocol. Unknown frame types are skipped for forward
// compatibility.
package frames

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

const (
	TypeUserMessage = "user_message"
	TypePromote     = "promote"
	TypeEndSession  = "end_session"

	// maxLine bounds one frame; /message content is capped at 8 KiB by
	// ContextMatrix, so 64 KiB is generous headroom.
	maxLine = 64 * 1024
)

type Frame struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

// Write encodes one frame as a single JSON line. The host service is the
// sole writer per container stdin, so atomicity beyond a single Write call
// is not required.
func Write(w io.Writer, f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}

	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}

	return nil
}

type Reader struct{ sc *bufio.Scanner }

func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxLine)

	return &Reader{sc: sc}
}

// Next returns the next known frame, skipping malformed lines and unknown
// types. io.EOF when the stream ends. A line exceeding maxLine fails the
// scanner and returns a non-EOF error — a hard stop, unlike shorter
// malformed lines which are skipped.
func (r *Reader) Next() (Frame, error) {
	for r.sc.Scan() {
		var f Frame
		if err := json.Unmarshal(r.sc.Bytes(), &f); err != nil {
			continue
		}

		switch f.Type {
		case TypeUserMessage, TypePromote, TypeEndSession:
			return f, nil
		default:
			continue
		}
	}

	if err := r.sc.Err(); err != nil {
		return Frame{}, fmt.Errorf("read frame: %w", err)
	}

	return Frame{}, io.EOF
}
