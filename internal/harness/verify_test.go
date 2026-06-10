package harness

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyEmitsAndReturnsVerdict(t *testing.T) {
	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)
	v, err := Verify(context.Background(), emit, func(ctx context.Context) (Verdict, error) {
		return Verdict{OK: false, Detail: "2 tests failing"}, nil
	})
	require.NoError(t, err)
	assert.False(t, v.OK)
	assert.Equal(t, "2 tests failing", v.Detail)
	assert.Contains(t, transcript.String(), "verification")
	assert.Contains(t, transcript.String(), "2 tests failing")
}

func TestVerifyNilCheckIsNoop(t *testing.T) {
	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)
	v, err := Verify(context.Background(), emit, nil)
	require.NoError(t, err)
	assert.False(t, v.OK)
	assert.Empty(t, strings.TrimSpace(transcript.String()))
}

func TestVerifyPropagatesError(t *testing.T) {
	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)
	_, err := Verify(context.Background(), emit, func(ctx context.Context) (Verdict, error) {
		return Verdict{}, errors.New("check blew up")
	})
	require.Error(t, err)
	// The error path still emits a verification event carrying the error detail.
	assert.Contains(t, transcript.String(), "verification")
	assert.Contains(t, transcript.String(), "check blew up")
}
