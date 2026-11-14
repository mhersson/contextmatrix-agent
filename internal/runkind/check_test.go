package runkind

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrialCandidate(t *testing.T) {
	cands := []orchestrator.VerifyCandidate{
		{Argv: []string{"./run", "test"}, Display: "./run test"},
		{Argv: []string{"bash", "-c", "cd a && ./run test"}, Display: "cd a && ./run test"},
	}

	tests := []struct {
		name    string
		verify  string
		current string
		want    orchestrator.VerifyCandidate
		ok      bool
	}{
		{
			name:    "an unchanged command runs under the shell",
			verify:  "./run lint && ./run test",
			current: "./run lint &&  ./run test",
			want:    orchestrator.VerifyCandidate{Argv: verifyexec.ShellArgv("./run lint && ./run test"), Display: "./run lint && ./run test"},
			ok:      true,
		},
		{name: "a changed command runs as its candidate", verify: "./run  test", current: "./ci/verify", want: cands[0], ok: true},
		{name: "a candidate that is not plain argv is rejected", verify: "cd a && ./run test"},
		{name: "an undeclared command is rejected", verify: "./other", current: "./ci/verify"},
		{name: "an empty command is rejected", verify: "", current: "./ci/verify"},
		{name: "a blank command never matches a blank current", verify: "  ", current: " "},
		{name: "a re-segmented command is not unchanged", verify: "./run lint\n./run test", current: "./run lint ./run test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := protocol.CheckInput{Verify: &protocol.VerifyConfig{Command: tt.verify}, CurrentCommand: tt.current}

			got, ok := trialCandidate(in, cands)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSameCommand(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"./run  test", " ./run test\n", true},
		{"./run lint\n\n./run test", "./run lint\n  ./run  test", true},
		{"./run test", "./run tests", false},
		{"./run test", "./run", false},
		{"./run lint ./run test", "./run lint\n./run test", false},
		{"./run lint\n./run test", "./run test\n./run lint", false},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, sameCommand(tt.a, tt.b), "%q vs %q", tt.a, tt.b)
	}
}

// trialFake records its calls and returns a fixed trial.
type trialFake struct {
	calls   int
	timeout time.Duration
	env     []string
	got     orchestrator.VerifyCandidate
	block   func()
}

func (f *trialFake) trial(_ context.Context, _ string, c orchestrator.VerifyCandidate, timeout time.Duration, env []string) (string, string, error) {
	f.calls++
	f.got, f.timeout, f.env = c, timeout, env

	if f.block != nil {
		f.block()
	}

	return protocol.TrialFinished, "1 failed", nil
}

func fakeDeps(cands []orchestrator.VerifyCandidate, f *trialFake) checkDeps {
	return checkDeps{
		candidates:    func(string) []orchestrator.VerifyCandidate { return cands },
		trial:         f.trial,
		mask:          func(s string) string { return s },
		progressEvery: time.Minute,
	}
}

func discardEmitter() *events.Emitter {
	return events.NewEmitter(io.Discard, io.Discard)
}

func TestCheckWithoutVerifyOnlyLists(t *testing.T) {
	f := &trialFake{}
	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", "test"}, Display: "./run test"}}

	res, err := check(context.Background(), t.TempDir(), protocol.CheckInput{}, fakeDeps(cands, f), discardEmitter())
	require.NoError(t, err)
	assert.Equal(t, []string{"./run test"}, res.Candidates)
	assert.Empty(t, res.Trial)
	assert.Zero(t, f.calls)

	res, err = check(context.Background(), t.TempDir(), protocol.CheckInput{}, fakeDeps(nil, f), discardEmitter())
	require.NoError(t, err)
	require.NotNil(t, res.Candidates)
	assert.Empty(t, res.Candidates)

	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.JSONEq(t, `{"candidates":[]}`, string(b))
}

func TestCheckRejectedRunsNothing(t *testing.T) {
	f := &trialFake{}
	in := protocol.CheckInput{Verify: &protocol.VerifyConfig{Command: "./other"}, CurrentCommand: "./ci/verify"}

	res, err := check(context.Background(), t.TempDir(), in, fakeDeps(nil, f), discardEmitter())
	require.NoError(t, err)
	assert.Equal(t, protocol.TrialRejected, res.Trial)
	assert.Zero(t, f.calls)
}

func TestCheckTrialsTheAcceptedCommandOnce(t *testing.T) {
	f := &trialFake{}
	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", "test"}, Display: "./run test", Wrapper: true}}
	in := protocol.CheckInput{Verify: &protocol.VerifyConfig{Command: "./run test", TimeoutSeconds: 900, Env: []string{"CHECK_TOOL_HOME"}}}

	t.Setenv("CHECK_TOOL_HOME", "/opt/tool")

	res, err := check(context.Background(), t.TempDir(), in, fakeDeps(cands, f), discardEmitter())
	require.NoError(t, err)
	assert.Equal(t, protocol.TrialFinished, res.Trial)
	assert.Equal(t, 1, f.calls)
	assert.Equal(t, cands[0], f.got)
	assert.Equal(t, 15*time.Minute, f.timeout)
	assert.Equal(t, []string{"CHECK_TOOL_HOME=/opt/tool"}, f.env)
}

func TestCheckFailsOnACanceledTrial(t *testing.T) {
	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", "test"}, Display: "./run test"}}
	d := fakeDeps(cands, &trialFake{})
	d.trial = func(ctx context.Context, _ string, _ orchestrator.VerifyCandidate, _ time.Duration, _ []string) (string, string, error) {
		return "", "", context.Canceled
	}

	in := protocol.CheckInput{Verify: &protocol.VerifyConfig{Command: "./run test"}}

	_, err := check(context.Background(), t.TempDir(), in, d, discardEmitter())
	require.ErrorIs(t, err, context.Canceled)
}

// eventLog is a concurrency-safe transcript sink that signals once it has seen
// want lines of kind.
type eventLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	kind  string
	want  int
	seen  int
	ready chan struct{}
}

func (l *eventLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if bytes.Contains(p, []byte(`"kind":"`+l.kind+`"`)) {
		l.seen++
		if l.seen == l.want {
			close(l.ready)
		}
	}

	return l.buf.Write(p)
}

func (l *eventLog) kinds(t *testing.T) []string {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()

	var out []string

	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		var ev struct {
			Kind string `json:"kind"`
		}

		require.NoError(t, json.Unmarshal([]byte(line), &ev))

		out = append(out, ev.Kind)
	}

	return out
}

func TestCheckEmitsProgressDuringTheTrial(t *testing.T) {
	log := &eventLog{kind: string(progressKind), want: 3, ready: make(chan struct{})}
	f := &trialFake{block: func() {
		select {
		case <-log.ready:
		case <-time.After(10 * time.Second):
		}
	}}

	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", "test"}, Display: "./run test"}}
	in := protocol.CheckInput{Verify: &protocol.VerifyConfig{Command: "./run test"}}

	d := fakeDeps(cands, f)
	d.progressEvery = 10 * time.Millisecond

	_, err := check(context.Background(), t.TempDir(), in, d, events.NewEmitter(io.Discard, log))
	require.NoError(t, err)

	kinds := log.kinds(t)
	require.NotEmpty(t, kinds)
	assert.Equal(t, string(trialKind), kinds[len(kinds)-1], "the trial event comes last")
	assert.GreaterOrEqual(t, len(kinds)-1, 3)

	for _, k := range kinds[:len(kinds)-1] {
		assert.Equal(t, string(progressKind), k)
	}
}

func TestCheckTrialEventCarriesTheOutcomeAndOutput(t *testing.T) {
	var buf bytes.Buffer

	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", "test"}, Display: "./run test"}}
	in := protocol.CheckInput{Verify: &protocol.VerifyConfig{Command: "./run test"}}

	_, err := check(context.Background(), t.TempDir(), in, fakeDeps(cands, &trialFake{}), events.NewEmitter(io.Discard, &buf))
	require.NoError(t, err)

	var ev struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}

	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &ev))
	assert.Equal(t, string(trialKind), ev.Kind)
	assert.Equal(t, protocol.TrialFinished, ev.Data["trial"])
	assert.Equal(t, "1 failed", ev.Data["output"])
	assert.Equal(t, "./run test", ev.Data["command"])
}
