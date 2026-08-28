package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/attempt"
	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/filelog"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-agent/internal/webhook"
	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

func TestComposeMCPURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{"plain", "http://cm:8080", "http://cm:8080/mcp"},
		{"trailing slash trimmed", "http://cm:8080/", "http://cm:8080/mcp"},
		{"multiple trailing slashes trimmed", "http://cm:8080///", "http://cm:8080/mcp"},
		{"path base", "http://cm:8080/api", "http://cm:8080/api/mcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, composeMCPURL(tt.base))
		})
	}
}

func TestExitStatus(t *testing.T) {
	tests := []struct {
		name        string
		code        int64
		cause       executor.ExitCause
		wantStatus  string
		wantMessage string
	}{
		{"zero is completed", 0, executor.ExitNormal, "completed", ""},
		{"nonzero is failed", 1, executor.ExitNormal, "failed", "worker exited with code 1"},
		{"timeout sentinel is failed", -1, executor.ExitTimeout, "failed", "worker exited with code -1"},
		{"wait failure sentinel is failed", -1, executor.ExitWaitFailure, "failed", "worker exited with code -1"},
		{"idle kill is failed", 137, executor.ExitIdleTimeout, "failed", "worker exited with code 137"},
		{"requested kill is failed", 137, executor.ExitKilled, "failed", "worker exited with code 137"},
		{
			"a daemon error is failed even carrying a zero code",
			0, executor.ExitDaemonError,
			"failed", "worker exit status unknown: the container wait returned a daemon error",
		},
		{
			"a daemon error does not quote the code it arrived with",
			1, executor.ExitDaemonError,
			"failed", "worker exit status unknown: the container wait returned a daemon error",
		},
		{
			"an idle-kill cause carrying a zero code is failed",
			0, executor.ExitIdleTimeout,
			"failed", "worker exit status unknown: the run ended by a kill or a timeout rather than on its own terms",
		},
		{
			"a requested kill carrying a zero code is failed",
			0, executor.ExitKilled,
			"failed", "worker exit status unknown: the run ended by a kill or a timeout rather than on its own terms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, message := exitStatus(tt.code, tt.cause)
			assert.Equal(t, tt.wantStatus, status)
			assert.Equal(t, tt.wantMessage, message)
		})
	}
}

func TestLaunchEnvMCPURL(t *testing.T) {
	t.Run("container override wins for MCP base", func(t *testing.T) {
		cfg := &config.ServiceConfig{
			ContextMatrixURL:          "http://public:8080",
			ContainerContextMatrixURL: "http://internal:8080",
		}
		env := launchEnv(cfg, "/secrets/shared")
		assert.Equal(t, "http://internal:8080/mcp", env.MCPURL)
		assert.Equal(t, "/secrets/shared", env.SecretsHostDir)
	})

	t.Run("falls back to public URL when no container override", func(t *testing.T) {
		cfg := &config.ServiceConfig{ContextMatrixURL: "http://public:8080"}
		env := launchEnv(cfg, "/secrets/shared")
		assert.Equal(t, "http://public:8080/mcp", env.MCPURL)
	})

	t.Run("forwards worker knobs", func(t *testing.T) {
		cfg := &config.ServiceConfig{
			ContextMatrixURL:      "http://public:8080",
			MCPAPIKey:             "mcp-key",
			BaseImage:             "img@sha256:abc",
			ContainerMemoryBytes:  1234,
			ContainerPidsLimit:    99,
			BashTimeoutMaxSeconds: 700,
			ToolOutputMaxBytes:    40000,
			DefaultModel:          "deepseek/deepseek-v4",
			ReviewAttemptsCap:     5,
			SelectorTierBars:      map[string]float64{"critical": 0.95},
		}
		env := launchEnv(cfg, "/secrets/shared")
		assert.Equal(t, "mcp-key", env.MCPAPIKey)
		assert.Equal(t, "img@sha256:abc", env.BaseImage)
		assert.Equal(t, int64(1234), env.MemoryBytes)
		assert.Equal(t, int64(99), env.PidsLimit)
		assert.Equal(t, 700, env.BashTimeoutMaxSeconds)
		assert.Equal(t, 40000, env.ToolOutputMaxBytes)
		assert.Equal(t, "deepseek/deepseek-v4", env.DefaultModel)
		assert.Equal(t, 5, env.ReviewAttemptsCap)
		assert.Equal(t, map[string]float64{"critical": 0.95}, env.SelectorTierBars)
	})

	t.Run("forwards compaction settings", func(t *testing.T) {
		cfg := &config.ServiceConfig{
			ContextMatrixURL: "http://public:8080",
			Compaction: config.CompactionConfig{
				Enabled:         true,
				Threshold:       0.8,
				KeepRecentTurns: 4,
			},
		}
		env := launchEnv(cfg, "/secrets/shared")
		assert.True(t, env.CompactionEnabled)
		assert.InDelta(t, 0.8, env.CompactionThreshold, 1e-9)
		assert.Equal(t, 4, env.CompactionKeepRecentTurns)
	})
}

func TestFlattenEnv(t *testing.T) {
	t.Run("nil map yields nil", func(t *testing.T) {
		assert.Nil(t, flattenEnv(nil))
	})

	t.Run("renders KEY=VALUE pairs", func(t *testing.T) {
		got := flattenEnv(map[string]string{"FOO": "bar", "BAZ": "qux"})
		sort.Strings(got)
		assert.Equal(t, []string{"BAZ=qux", "FOO=bar"}, got)
	})
}

type stubReporter struct{ status, message string }

func (s *stubReporter) ReportStatus(_ context.Context, _, _, status, message string) error {
	s.status = status
	s.message = message

	return nil
}

func TestOnContainerExitClosesLogFile(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	files := filelog.New(dir, logger)
	files.Begin("proj", "CARD-1", "abcdef012345")
	files.Write("proj", "CARD-1", []byte("a line"), false)

	creds := secrets.NewRunCredentials(t.TempDir(), "http://cm", "key", logger)
	rep := &stubReporter{}

	// A minimal bridge + registry so onContainerExit compiles.
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, nil)
	bridge := logbridge.NewBridge(logbridge.BridgeConfig{Hub: hub})
	registry := newSessionSecretTee(logbridge.NewRedactorRegistry(bridge))

	onExit := onContainerExit(rep, creds, files, registry, bridge, logger)
	onExit("proj", "CARD-1", 0, executor.ExitNormal, 1, "corr-1")

	data, err := os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "a line")
	assert.Contains(t, string(data), "==== run ended ")
	assert.Contains(t, string(data), "exit=0")
	assert.Equal(t, "completed", rep.status)

	// The file is closed and forgotten: a further Write does not reach it.
	files.Write("proj", "CARD-1", []byte("after close"), false)

	after, err := os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)
	assert.NotContains(t, string(after), "after close")
}

func TestServeCommandRegistered(t *testing.T) {
	root := NewRootCmd()

	var found bool

	for _, c := range root.Commands() {
		if c.Name() == "serve" {
			found = true

			break
		}
	}

	assert.True(t, found, "serve command should be registered on root")
}

// The file logger is the only durable record of a card's prior runs, and the
// webhook server is what puts the ordinal in the container env - but the two
// never meet unless serve wires them together. This pins that seam: the
// counter the webhook config accepts is the one that counts real run headers.
func TestNextAttemptWiresFileLoggerIntoWebhookConfig(t *testing.T) {
	dir := t.TempDir()
	files := filelog.New(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cfg := webhook.Config{NextAttempt: files.NextAttempt}
	require.NotNil(t, cfg.NextAttempt)

	assert.Equal(t, 1, cfg.NextAttempt("proj", "CARD-1"))

	files.Begin("proj", "CARD-1", "abcdef012345")
	files.End("proj", "CARD-1", 0, "normal")

	assert.Equal(t, 2, cfg.NextAttempt("proj", "CARD-1"))
}

// exitHarness wires the collaborators onContainerExit needs plus a hub
// subscriber, so a test can read both surfaces a terminal event lands on.
// registry is exported so a test can register a session key before firing
// onExit and then check whether it survived - the only way to pin that
// onExit's removal call actually targets the id a real registration used.
type exitHarness struct {
	dir      string
	files    *filelog.Logger
	rep      *stubReporter
	stream   <-chan protocol.LogEntry
	registry *sessionSecretTee
	onExit   func(project, cardID string, exitCode int64, cause executor.ExitCause, attempt int, correlationID string)
}

func newExitHarness(t *testing.T, logDir string) *exitHarness {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	files := filelog.New(logDir, logger)
	creds := secrets.NewRunCredentials(t.TempDir(), "http://cm", "key", logger)
	rep := &stubReporter{}

	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, nil)
	bridge := logbridge.NewBridge(logbridge.BridgeConfig{Hub: hub, MapExtra: agentMapExtra})
	registry := newSessionSecretTee(logbridge.NewRedactorRegistry(bridge))

	id, ch := hub.Subscribe("proj")

	t.Cleanup(func() { hub.Unsubscribe(id) })

	return &exitHarness{
		dir:      logDir,
		files:    files,
		rep:      rep,
		stream:   ch,
		registry: registry,
		onExit:   onContainerExit(rep, creds, files, registry, bridge, logger),
	}
}

func (h *exitHarness) transcript(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(h.dir, "proj", "card-1.log"))
	require.NoError(t, err)

	return string(data)
}

// drainStream collects everything the hub already holds without blocking.
func drainStream(ch <-chan protocol.LogEntry) []protocol.LogEntry {
	var got []protocol.LogEntry

	for {
		select {
		case e := <-ch:
			got = append(got, e)
		default:
			return got
		}
	}
}

// findRunEnd returns the decoded run_end transcript line and its byte offset.
func findRunEnd(t *testing.T, s string) (map[string]any, int) {
	t.Helper()

	for _, line := range strings.Split(s, "\n") {
		if !strings.Contains(line, `"`+runEndKind+`"`) {
			continue
		}

		var ev map[string]any

		require.NoError(t, json.Unmarshal([]byte(line), &ev), "the terminal line must be valid JSONL")

		return ev, strings.Index(s, line)
	}

	t.Fatalf("no %s line in the transcript:\n%s", runEndKind, s)

	return nil, 0
}

// TestExitEmitsTerminalEventAndFooterFromOneCall pins the whole point of the
// event: a run that died leaves a transcript and a live stream that say how it
// ended, and the two cannot disagree because one call writes both.
func TestExitEmitsTerminalEventAndFooterFromOneCall(t *testing.T) {
	h := newExitHarness(t, t.TempDir())

	h.files.Begin("proj", "CARD-1", "abcdef012345")
	h.files.Write("proj", "CARD-1", []byte(`{"seq":1,"kind":"state_change"}`), false)

	h.onExit("proj", "CARD-1", -1, executor.ExitTimeout, 2, "corr-1")

	s := h.transcript(t)

	ev, runEndAt := findRunEnd(t, s)
	data, ok := ev["data"].(map[string]any)
	require.True(t, ok, "the terminal line carries a data payload")
	assert.InDelta(t, -1, data["exit_code"], 0)
	assert.Equal(t, string(executor.ExitTimeout), data["cause"])

	footerAt := strings.Index(s, "==== run ended ")
	require.NotEqual(t, -1, footerAt)
	assert.Less(t, runEndAt, footerAt, "the terminal event must land before the footer closes the file")
	assert.Contains(t, s, "cause="+string(executor.ExitTimeout), "the footer names the same cause")

	entries := drainStream(h.stream)
	require.Len(t, entries, 1, "exactly one terminal entry reaches the live stream")
	assert.Equal(t, "CARD-1", entries[0].CardID)
	assert.Contains(t, entries[0].Content, "-1")
	assert.Contains(t, entries[0].Content, string(executor.ExitTimeout))
}

// TestOnContainerExit_RemovalTargetsExactRegisteredID pins the link a bare
// project/cardID revert at either onContainerExit's removal call or
// onTokenRefreshHook's registration call would silently break. It registers a
// key exactly the way production code does - webhook.SessionID for the
// initial registration (what addSessionSecrets uses) and the real
// onTokenRefreshHook for a mid-run rotation - then fires the real onExit
// closure and checks removal by behavior (Redact no longer masks a key once
// it is gone), not by re-deriving the id inside the test. A second run's key,
// registered under a different correlation id, must stay masked - untouched
// by run 1's exit. Reverting onContainerExit's removal call to the bare
// project+"/"+cardID id fails this test: it would target an id nothing was
// ever registered under (production registration always includes the
// correlation id), so run 1's own keys would still be masked after its exit.
// Reverting onTokenRefreshHook alone means the rotated token registers under
// an id removal never targets, so it alone would still be masked afterward.
func TestOnContainerExit_RemovalTargetsExactRegisteredID(t *testing.T) {
	h := newExitHarness(t, t.TempDir())

	// Simulates addSessionSecrets registering run 1's initial secret, then
	// onTokenRefreshHook registering a mid-run rotated token - both under the
	// same correlation id a real admission would use.
	h.registry.AddSessionKey(webhook.SessionID("proj", "CARD-1", "corr-1"), "PLACEHOLDER-RUN1-SECRET")
	onTokenRefreshHook(h.registry)("proj", "CARD-1", "corr-1", "PLACEHOLDER-RUN1-ROTATED")

	// A second run of the same card, registered under a different
	// correlation id, must be unaffected by run 1's exit.
	h.registry.AddSessionKey(webhook.SessionID("proj", "CARD-1", "corr-2"), "PLACEHOLDER-RUN2-SECRET")

	h.files.Begin("proj", "CARD-1", "abcdef012345")
	h.onExit("proj", "CARD-1", 0, executor.ExitNormal, 1, "corr-1")

	line := []byte("PLACEHOLDER-RUN1-SECRET PLACEHOLDER-RUN1-ROTATED PLACEHOLDER-RUN2-SECRET")
	redacted := string(h.registry.Redact(line))

	// Removed keys are no longer masked - Redact leaves them literal.
	assert.Contains(t, redacted, "PLACEHOLDER-RUN1-SECRET",
		"run 1's initial secret must be gone from the registry after its own exit")
	assert.Contains(t, redacted, "PLACEHOLDER-RUN1-ROTATED",
		"run 1's rotated token, registered under the same correlation id, must also be gone")

	// A still-registered key stays masked.
	assert.NotContains(t, redacted, "PLACEHOLDER-RUN2-SECRET",
		"run 2's secret, registered under a different correlation id, must still be masked")
	assert.Equal(t, 1, strings.Count(redacted, "[REDACTED]"), "exactly run 2's one still-registered secret is masked")
}

// TestTerminalEventCarriesTheAttemptOrdinal keeps a restarted container's
// terminal event separable from the run it replaced. The container stamps its
// own lines; a line the host writes has to carry the ordinal itself or it
// reads as attempt 1.
func TestTerminalEventCarriesTheAttemptOrdinal(t *testing.T) {
	t.Run("second attempt is stamped", func(t *testing.T) {
		h := newExitHarness(t, t.TempDir())
		h.files.Begin("proj", "CARD-1", "abcdef012345")
		h.onExit("proj", "CARD-1", 0, executor.ExitNormal, 2, "corr-1")

		ev, _ := findRunEnd(t, h.transcript(t))
		assert.InDelta(t, 2, ev[attempt.Field], 0)
	})

	t.Run("first attempt is left unmarked", func(t *testing.T) {
		h := newExitHarness(t, t.TempDir())
		h.files.Begin("proj", "CARD-1", "abcdef012345")
		h.onExit("proj", "CARD-1", 0, executor.ExitNormal, 1, "corr-1")

		ev, _ := findRunEnd(t, h.transcript(t))
		assert.NotContains(t, ev, attempt.Field, "an absent ordinal already reads as the first attempt")
	})
}

// TestFooterAndTerminalEventWrittenOnEveryExitPath: a run that ends any of the
// three ways closes both surfaces, and the cause distinguishes them.
func TestFooterAndTerminalEventWrittenOnEveryExitPath(t *testing.T) {
	tests := []struct {
		name       string
		exitCode   int64
		cause      executor.ExitCause
		wantStatus string
	}{
		{"normal", 0, executor.ExitNormal, "completed"},
		{"timeout", -1, executor.ExitTimeout, "failed"},
		{"wait failure", -1, executor.ExitWaitFailure, "failed"},
		{"daemon-flagged wait", 0, executor.ExitDaemonError, "failed"},
		{"idle watchdog kill", 137, executor.ExitIdleTimeout, "failed"},
		{"requested kill", 137, executor.ExitKilled, "failed"},
		{"idle watchdog kill carrying zero", 0, executor.ExitIdleTimeout, "failed"},
		{"requested kill carrying zero", 0, executor.ExitKilled, "failed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newExitHarness(t, t.TempDir())
			h.files.Begin("proj", "CARD-1", "abcdef012345")
			h.onExit("proj", "CARD-1", tc.exitCode, tc.cause, 1, "corr-1")

			s := h.transcript(t)

			ev, _ := findRunEnd(t, s)
			data, _ := ev["data"].(map[string]any)
			assert.Equal(t, string(tc.cause), data["cause"])
			assert.Contains(t, s, "==== run ended ")
			assert.Contains(t, s, "cause="+string(tc.cause))
			assert.Equal(t, tc.wantStatus, h.rep.status)
			require.Len(t, drainStream(h.stream), 1)
		})
	}
}

// TestExitPathSurvivesAnUnwritableTranscript: the terminal event is
// best-effort. With file logging off there is nowhere to write it, and the
// status callback - the only way CM learns the run finished - must still run
// with the original status and message.
func TestExitPathSurvivesAnUnwritableTranscript(t *testing.T) {
	h := newExitHarness(t, "") // an empty dir disables the file logger entirely

	h.onExit("proj", "CARD-1", 137, executor.ExitTimeout, 1, "corr-1")

	assert.Equal(t, "failed", h.rep.status)
	assert.Equal(t, "worker exited with code 137", h.rep.message)
	require.Len(t, drainStream(h.stream), 1, "the live stream still gets the terminal event")
}
