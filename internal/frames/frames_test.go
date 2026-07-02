package frames

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	var sb strings.Builder

	require.NoError(t, Write(&sb, Frame{Type: TypeUserMessage, Content: "hi there", MessageID: "m1"}))
	require.NoError(t, Write(&sb, Frame{Type: TypePromote}))
	require.NoError(t, Write(&sb, Frame{Type: TypeEndSession}))

	r := NewReader(strings.NewReader(sb.String()))

	f1, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, Frame{Type: TypeUserMessage, Content: "hi there", MessageID: "m1"}, f1)

	f2, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, TypePromote, f2.Type)

	f3, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, TypeEndSession, f3.Type)

	_, err = r.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestReaderSkipsGarbageAndUnknownTypes(t *testing.T) {
	in := "not json\n{\"type\":\"future_thing\"}\n{\"type\":\"user_message\",\"content\":\"ok\"}\n"
	r := NewReader(strings.NewReader(in))

	f, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, "ok", f.Content) // garbage + unknown types skipped, not fatal
}

func TestReaderOversizedLineIsFatal(t *testing.T) {
	in := strings.Repeat("a", MaxLine+1) + "\n"
	r := NewReader(strings.NewReader(in))

	_, err := r.Next()
	require.Error(t, err)
	require.NotErrorIs(t, err, io.EOF)
}

func TestWriteRejectsOversizedFrame(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	err := Write(&buf, Frame{Type: TypeUserMessage, Content: strings.Repeat("x", MaxLine)})

	require.ErrorIs(t, err, ErrFrameTooLarge)
	assert.Zero(t, buf.Len(), "nothing must be written on an oversized frame")
}
