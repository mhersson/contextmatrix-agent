package attempt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventLine marshals one harness event exactly as the emitter writes it:
// a single JSON object followed by a newline.
func eventLine(t *testing.T, seq int) []byte {
	t.Helper()

	ev := events.Event{
		Seq:  seq,
		Kind: events.ToolCallKind,
		Time: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		Data: map[string]any{"tool": "bash", "input": map[string]any{"cmd": "go test ./..."}},
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	return append(b, '\n')
}

func TestWriterStampsOrdinal(t *testing.T) {
	var buf bytes.Buffer

	line := eventLine(t, 7)

	n, err := NewWriter(&buf, 2).Write(line)
	require.NoError(t, err)
	assert.Equal(t, len(line), n, "the writer reports every input byte consumed")

	out := buf.String()
	assert.True(t, strings.HasSuffix(out, "\n"), "the forwarded line keeps its terminator")
	assert.Equal(t, 1, strings.Count(out, "\n"), "one event in, one line out")

	var got map[string]any

	require.NoError(t, json.Unmarshal([]byte(out), &got))

	assert.InDelta(t, 2.0, got[Field], 1e-9)
	assert.EqualValues(t, 7, got["seq"])
	assert.Equal(t, string(events.ToolCallKind), got["kind"])
	assert.Equal(t, "2026-08-25T12:00:00Z", got["time"])
	assert.Equal(t, map[string]any{"tool": "bash", "input": map[string]any{"cmd": "go test ./..."}}, got["data"])
}

func TestWriterPassesThroughAtFirstAttempt(t *testing.T) {
	for _, ordinal := range []int{-1, 0, 1} {
		var buf bytes.Buffer

		line := eventLine(t, 1)

		n, err := NewWriter(&buf, ordinal).Write(line)
		require.NoError(t, err)
		assert.Equal(t, len(line), n)
		assert.Equal(t, string(line), buf.String(), "ordinal %d must forward the stream untouched", ordinal)
	}
}

func TestWriterForwardsNonJSONUnchanged(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"stderr slog line", `time=2026-08-25T12:00:00Z level=INFO msg="claimed card"`},
		{"prose", "==== run started 2026-08-25T12:00:00Z container=abcdef012345 ===="},
		{"truncated object", `{"seq":1,"kind":"usage"`},
		{"json array", `[1,2,3]`},
		{"json null", `null`},
		{"json string", `"hello"`},
		{"json number", `42`},
		{"object with trailing garbage", `{"seq":1} and then some`},
		{"empty line", ``},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer

			in := tc.line + "\n"

			n, err := NewWriter(&buf, 3).Write([]byte(in))
			require.NoError(t, err)
			assert.Equal(t, len(in), n)
			assert.Equal(t, in, buf.String(), "unparsable input must be forwarded byte for byte")
		})
	}
}

func TestWriterBuffersLineSplitAcrossWrites(t *testing.T) {
	var buf bytes.Buffer

	w := NewWriter(&buf, 4)
	line := eventLine(t, 9)
	split := len(line) / 2

	n, err := w.Write(line[:split])
	require.NoError(t, err)
	assert.Equal(t, split, n)
	assert.Empty(t, buf.String(), "a partial line is held until its newline arrives")

	n, err = w.Write(line[split:])
	require.NoError(t, err)
	assert.Equal(t, len(line)-split, n)

	var got map[string]any

	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.InDelta(t, 4.0, got[Field], 1e-9)
	assert.EqualValues(t, 9, got["seq"], "the two halves reassemble into one intact event")
}

func TestWriterHandlesManyLinesAndATrailingPartial(t *testing.T) {
	var buf bytes.Buffer

	w := NewWriter(&buf, 2)

	first, second := eventLine(t, 1), eventLine(t, 2)

	batch := append(append([]byte{}, first...), second...)
	batch = append(batch, []byte(`{"seq":3,"kind":"usage"`)...)

	n, err := w.Write(batch)
	require.NoError(t, err)
	assert.Equal(t, len(batch), n)
	assert.Equal(t, 2, strings.Count(buf.String(), "\n"), "only the two complete lines are forwarded")

	_, err = w.Write([]byte("}\n"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 3)

	for i, l := range lines {
		var got map[string]any

		require.NoError(t, json.Unmarshal([]byte(l), &got), "line %d", i)
		assert.InDelta(t, 2.0, got[Field], 1e-9, "line %d", i)
		assert.EqualValues(t, i+1, got["seq"])
	}
}

func TestWriterDoesNotMutateCallerBuffer(t *testing.T) {
	var buf bytes.Buffer

	line := eventLine(t, 5)
	original := string(line)

	_, err := NewWriter(&buf, 2).Write(line)
	require.NoError(t, err)
	assert.Equal(t, original, string(line), "the caller's slice is never written through")
}

func TestWriterPreservesNumericPrecision(t *testing.T) {
	var buf bytes.Buffer

	in := `{"seq":1,"kind":"usage","data":{"tokens":9007199254740993,"cost":0.00012500}}` + "\n"

	_, err := NewWriter(&buf, 2).Write([]byte(in))
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "9007199254740993", "a large integer must survive the round trip exactly")
	assert.Contains(t, out, "0.00012500", "a decimal must survive the round trip exactly")
}

func TestEntriesFromTwoAttemptsSeparableOnCollidingSeq(t *testing.T) {
	var first, second bytes.Buffer

	line := eventLine(t, 1)

	_, err := NewWriter(&first, 1).Write(line)
	require.NoError(t, err)

	_, err = NewWriter(&second, 2).Write(line)
	require.NoError(t, err)

	var a, b map[string]any

	require.NoError(t, json.Unmarshal(first.Bytes(), &a))
	require.NoError(t, json.Unmarshal(second.Bytes(), &b))

	assert.Equal(t, a["seq"], b["seq"], "the two runs collide on the sequence number")
	assert.Nil(t, a[Field], "attempt 1 needs no mark, so its absence is the separator")
	assert.InDelta(t, 2.0, b[Field], 1e-9)
}

// The transcript stream has more than one writer on it (the event emitter and
// the mob seat sink), so a line must arrive whole no matter who else is
// writing. Run under -race this also covers the held partial line.
func TestWriterKeepsLinesIntactUnderConcurrentWriters(t *testing.T) {
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)

	w := NewWriter(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()

		return buf.Write(p)
	}), 3)

	const writers, perWriter = 8, 25

	var wg sync.WaitGroup

	for i := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := range perWriter {
				line := []byte(fmt.Sprintf(`{"seq":%d,"kind":"tool_call","writer":%d}`+"\n", j+1, i))

				_, err := w.Write(line)
				assert.NoError(t, err)
			}
		}()
	}

	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, writers*perWriter)

	for _, l := range lines {
		var got map[string]any

		require.NoError(t, json.Unmarshal([]byte(l), &got), "every line is one intact event: %q", l)
		assert.InDelta(t, 3.0, got[Field], 1e-9)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
