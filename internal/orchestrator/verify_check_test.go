package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrialOutcome(t *testing.T) {
	tests := []struct {
		name string
		res  verifyResult
		err  error
		want string
	}{
		{"passed", verifyResult{Status: verifyPassed}, nil, protocol.TrialFinished},
		{"failed", verifyResult{Status: verifyFailed}, nil, protocol.TrialFinished},
		{"timeout", verifyResult{Status: verifySkipped, Skip: skipTimeout}, nil, protocol.TrialTimeout},
		{"exhausted", verifyResult{Status: verifySkipped, Skip: skipExhausted}, nil, protocol.TrialInfrastructure},
		{"tool missing", verifyResult{Status: verifySkipped, Skip: skipToolMissing}, nil, protocol.TrialToolchain},
		{"runtime missing", verifyResult{}, &ToolchainMissingError{Tier: "runtime"}, protocol.TrialInfrastructure},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, trialOutcome(tt.res, tt.err))
		})
	}
}

func TestClampVerifyTimeout(t *testing.T) {
	tests := []struct {
		seconds int
		want    time.Duration
	}{
		{0, 10 * time.Minute},
		{10, 30 * time.Second},
		{900, 15 * time.Minute},
		{9000, 2 * time.Hour},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, ClampVerifyTimeout(tt.seconds))
	}
}

func TestTrialVerifyRunsTheCommandOnce(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := "#!/bin/sh\necho x >> " + counter + "\nexit " + "${CODE:-1}" + "\n"

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ci"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ci", "verify"), []byte(script), 0o755))

	c := VerifyCandidate{Argv: []string{"./ci/verify"}, Display: "./ci/verify"}

	trial, _, err := TrialVerify(context.Background(), dir, c, time.Minute, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, protocol.TrialFinished, trial)

	data, err := os.ReadFile(counter)
	require.NoError(t, err)
	assert.Equal(t, "x\n", string(data), "a failing command runs exactly once")

	trial, _, err = TrialVerify(context.Background(), dir, c, time.Minute, []string{"CODE=127"}, nil)
	require.NoError(t, err)
	assert.Equal(t, protocol.TrialToolchain, trial)

	trial, _, err = TrialVerify(context.Background(), dir,
		VerifyCandidate{Argv: []string{"sleep", "5"}}, 100*time.Millisecond, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, protocol.TrialTimeout, trial)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = TrialVerify(ctx, dir, c, time.Minute, nil, nil)
	require.Error(t, err)
}

func TestVerifyCandidatesListsEveryRunnableRow(t *testing.T) {
	t.Run("two rows resolve", func(t *testing.T) {
		stubTools(t, "go", "cargo")

		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module example.com/x\n")
		writeFile(t, dir, "Cargo.toml", "[package]\nname=\"x\"\n")

		got := VerifyCandidates(dir)
		require.Len(t, got, 2)
		assert.Equal(t, []string{"go", "test", "./..."}, got[0].Argv)
		assert.Equal(t, "cargo test", got[1].Display)
		assert.Equal(t, "go test ./...", detectVerifyCommand(dir).Display, "the gate keeps only the first")
	})

	t.Run("wrapper first", func(t *testing.T) {
		stubTools(t, "make", "go")

		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "test:\n\t./run-tests.sh\n")
		writeFile(t, dir, "go.mod", "module example.com/x\n")

		got := VerifyCandidates(dir)
		require.Len(t, got, 2)
		assert.True(t, got[0].Wrapper)
		assert.False(t, got[1].Wrapper)
	})

	t.Run("empty workspace is non-nil", func(t *testing.T) {
		stubTools(t, "go")

		got := VerifyCandidates(t.TempDir())
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("unresolved root marker contributes nothing", func(t *testing.T) {
		stubTools(t, "go")

		dir := t.TempDir()
		writeFile(t, dir, "Cargo.toml", "[package]\nname=\"x\"\n")

		assert.Empty(t, VerifyCandidates(dir))
	})
}
