package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/mhersson/contextmatrix-agent/internal/metrics"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerConfig_StdinAndImage(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
	})

	assert.Equal(t, "alpine:3", cfg.Image)
	assert.True(t, cfg.OpenStdin, "OpenStdin must be set so control frames can be written")
	assert.True(t, cfg.AttachStdin)
	assert.False(t, cfg.StdinOnce, "stdin stays open for the container's life")
	assert.False(t, cfg.Tty)
}

func TestContainerConfig_Labels(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
	})

	assert.Equal(t, "true", cfg.Labels[labelAgent])
	assert.Equal(t, "ABC-1", cfg.Labels[labelCardID])
	assert.Equal(t, "demo", cfg.Labels[labelProject])
	// Correlation ID label is omitted when empty.
	_, ok := cfg.Labels[labelCorrelationID]
	assert.False(t, ok)
}

func TestContainerConfig_CorrelationIDLabel(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		CardID:        "ABC-1",
		Project:       "demo",
		Image:         "alpine:3",
		CorrelationID: "corr-123",
	})

	assert.Equal(t, "corr-123", cfg.Labels[labelCorrelationID])
}

func TestContainerConfig_EnvPassthrough(t *testing.T) {
	env := []string{"FOO=bar", "BAZ=qux"}

	cfg, _ := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
		Env:     env,
	})

	assert.Equal(t, env, cfg.Env)
}

func TestContainerConfig_CACertMountAndEnv(t *testing.T) {
	cfg, host := containerConfig(LaunchSpec{
		CardID:         "ABC-1",
		Project:        "demo",
		Image:          "alpine:3",
		Env:            []string{"FOO=bar"},
		CACertHostFile: "/etc/cm/extra-ca.pem",
	})

	assert.Contains(t, host.Binds, "/etc/cm/extra-ca.pem:/run/cm-ca/ca.crt:ro")
	assert.Contains(t, cfg.Env, "CMX_CA_CERT_FILE=/run/cm-ca/ca.crt")
	assert.Contains(t, cfg.Env, "FOO=bar", "pre-existing env is preserved")

	// The git/gh CA vars are deliberately NOT set at the container level - the
	// harness scrubs subprocess env, so they would be dead and misleading. git/gh
	// get them on their explicit subprocess env instead.
	for _, e := range cfg.Env {
		assert.NotContains(t, e, "GIT_SSL_CAINFO")
		assert.NotContains(t, e, "GH_CA_BUNDLE")
		assert.NotContains(t, e, "SSL_CERT_FILE")
	}
}

func TestContainerConfig_NoCACertByDefault(t *testing.T) {
	cfg, host := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
		Env:     []string{"FOO=bar"},
	})

	for _, b := range host.Binds {
		assert.NotContains(t, b, "/run/cm-ca/")
	}

	for _, e := range cfg.Env {
		assert.NotContains(t, e, "CMX_CA_CERT_FILE")
	}
}

func TestContainerConfig_HostConfigResourcesAndHardening(t *testing.T) {
	const (
		mem  = int64(8 * 1024 * 1024 * 1024)
		pids = int64(512)
	)

	_, host := containerConfig(LaunchSpec{
		CardID:         "ABC-1",
		Project:        "demo",
		Image:          "alpine:3",
		SecretsHostDir: "/srv/cm/secrets/demo",
		MemoryBytes:    mem,
		PidsLimit:      pids,
	})

	assert.Equal(t, mem, host.Memory)

	require.NotNil(t, host.PidsLimit)
	assert.Equal(t, pids, *host.PidsLimit)

	require.NotNil(t, host.Init)
	assert.True(t, *host.Init, "docker-init must be PID 1 so orphaned children are reaped")

	assert.Equal(t, []string{"ALL"}, []string(host.CapDrop))
	assert.Equal(t, []string{"no-new-privileges"}, host.SecurityOpt)
	assert.Equal(t, []string{"/srv/cm/secrets/demo:/run/cm-secrets:ro"}, host.Binds)
}

func TestContainerConfig_RunsAsNonRoot(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
	})

	assert.Equal(t, "1000:1000", cfg.User)
}

func TestContainerConfig_NoBindsWhenSecretsDirEmpty(t *testing.T) {
	_, host := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
	})

	assert.Empty(t, host.Binds)
}

func TestContainerName_Sanitized(t *testing.T) {
	tests := []struct {
		name    string
		project string
		cardID  string
		want    string
	}{
		{
			name:    "lowercases card id",
			project: "demo",
			cardID:  "ABC-1",
			want:    "cm-agent-demo-abc-1",
		},
		{
			name:    "sanitizes disallowed chars in project",
			project: "my/proj@v2",
			cardID:  "ABC-1",
			want:    "cm-agent-my-proj-v2-abc-1",
		},
		{
			name:    "spaces become dashes",
			project: "team alpha",
			cardID:  "X-9",
			want:    "cm-agent-team-alpha-x-9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, containerName(tt.project, tt.cardID))
		})
	}
}

func TestContainerName_MatchesDockerCharset(t *testing.T) {
	got := containerName("my/proj@v2", "ABC-1")
	assert.Regexp(t, `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`, got)
}

// DockerExecutor must satisfy the Executor seam.
var _ Executor = (*DockerExecutor)(nil)

func TestLineWriter_SplitsOnNewline(t *testing.T) {
	var got []string

	w := newLineWriter(func(line []byte) {
		got = append(got, string(line))
	})

	n, err := w.Write([]byte("alpha\nbeta\n"))
	require.NoError(t, err)
	assert.Equal(t, len("alpha\nbeta\n"), n)
	assert.Equal(t, []string{"alpha", "beta"}, got)
}

func TestLineWriter_PartialLineHeldUntilFlush(t *testing.T) {
	var got []string

	w := newLineWriter(func(line []byte) {
		got = append(got, string(line))
	})

	_, _ = w.Write([]byte("hel"))
	_, _ = w.Write([]byte("lo\nwor"))

	assert.Equal(t, []string{"hello"}, got, "complete line emitted, partial held")

	w.Flush()
	assert.Equal(t, []string{"hello", "wor"}, got, "flush emits the trailing partial line")
}

func TestLineWriter_FlushOnEmptyBufferIsNoop(t *testing.T) {
	called := false

	w := newLineWriter(func([]byte) { called = true })
	w.Flush()

	assert.False(t, called)
}

func TestLineWriter_TrimsCarriageReturn(t *testing.T) {
	var got []string

	w := newLineWriter(func(line []byte) {
		got = append(got, string(line))
	})

	_, _ = w.Write([]byte("windows\r\nline\r\n"))

	assert.Equal(t, []string{"windows", "line"}, got)
}

func TestLineWriter_BoundsLongLine(t *testing.T) {
	var got []byte

	w := newLineWriter(func(line []byte) {
		got = append([]byte(nil), line...)
	})

	huge := bytes.Repeat([]byte("x"), scannerBufferMax+4096)
	_, _ = w.Write(huge)
	w.Flush()

	assert.LessOrEqual(t, len(got), scannerBufferMax,
		"line buffer must not grow past the cap")
}

func TestContainerConfigMountsSkillsReadOnly(t *testing.T) {
	_, host := containerConfig(LaunchSpec{
		CardID:         "CARD-1",
		Project:        "proj",
		Image:          "img",
		SecretsHostDir: "/host/secrets",
		SkillsHostDir:  "/host/skills",
	})

	assert.Contains(t, host.Binds, "/host/secrets:/run/cm-secrets:ro")
	assert.Contains(t, host.Binds, "/host/skills:/run/cm-skills:ro", "skills dir is bound read-only")
}

func TestContainerConfigNoSkillsBindWhenUnset(t *testing.T) {
	_, host := containerConfig(LaunchSpec{CardID: "CARD-1", Project: "proj", Image: "img"})

	for _, b := range host.Binds {
		assert.NotContains(t, b, "/run/cm-skills", "no skills bind when SkillsHostDir is empty")
	}
}

func TestNewDockerExecutor_WiresOnStart(t *testing.T) {
	called := false
	e := NewDockerExecutor(Config{
		OnStart: func(_, _, _ string) { called = true },
	})

	require.NotNil(t, e.onStart)

	e.onStart("p", "c", "id")
	assert.True(t, called)
}

type fakeWaiter struct{ exits bool }

func (f *fakeWaiter) ContainerWait(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	wc := make(chan container.WaitResponse, 1)
	ec := make(chan error, 1)

	if f.exits {
		wc <- container.WaitResponse{StatusCode: 0}
	} else {
		go func() {
			<-ctx.Done()

			ec <- ctx.Err()
		}()
	}

	return wc, ec
}

func TestWaitForSelfExitExited(t *testing.T) {
	assert.True(t, waitForSelfExit(t.Context(), &fakeWaiter{exits: true}, "c1", time.Second))
}

func TestWaitForSelfExitTimesOut(t *testing.T) {
	start := time.Now()

	assert.False(t, waitForSelfExit(t.Context(), &fakeWaiter{exits: false}, "c1", 20*time.Millisecond))
	assert.Less(t, time.Since(start), time.Second, "must give up at the grace bound")
}

func TestImageSummaries_SkipsDanglingAndMapsFields(t *testing.T) {
	in := []image.Summary{
		{
			RepoTags:    []string{"contextmatrix-agent-worker:go-node"},
			RepoDigests: []string{"contextmatrix-agent-worker@sha256:abc"},
			Created:     1750000000,
			Size:        2_560_000_000,
		},
		{RepoTags: nil, RepoDigests: []string{"orphan@sha256:def"}},     // dangling: skipped
		{RepoTags: []string{"<none>:<none>"}},                           // dangling tag form: skipped
		{RepoTags: []string{"other:latest", "<none>:<none>"}, Size: 42}, // <none> pruned, image kept
	}

	got := imageSummaries(in)

	require.Len(t, got, 2)
	assert.Equal(t, webhookcore.ImageSummary{
		Tags:      []string{"contextmatrix-agent-worker:go-node"},
		Digests:   []string{"contextmatrix-agent-worker@sha256:abc"},
		CreatedAt: 1750000000,
		SizeBytes: 2_560_000_000,
	}, got[0])
	assert.Equal(t, []string{"other:latest"}, got[1].Tags)
	assert.Equal(t, int64(42), got[1].SizeBytes)
}

func TestWaitForPumpDrain_ReturnsWhenPumpCloses(t *testing.T) {
	pumpDone := make(chan struct{})
	close(pumpDone)

	assert.True(t, waitForPumpDrain(pumpDone, time.Second))
}

// TestWaitForPumpDrain_TimesOut pins the bound that keeps a pump which never
// drains - a hijacked attach connection the daemon leaves open - from stranding
// the exit callback, the credential teardown and the session-secret cleanup.
func TestWaitForPumpDrain_TimesOut(t *testing.T) {
	pumpDone := make(chan struct{}) // never closed

	start := time.Now()

	assert.False(t, waitForPumpDrain(pumpDone, 20*time.Millisecond))
	assert.Less(t, time.Since(start), time.Second, "the wait must be bounded, not blocked")
}

// exitCall is one recorded onExit invocation.
type exitCall struct {
	exitCode      int64
	cause         ExitCause
	attempt       int
	correlationID string
}

// stubDocker fakes the slice of the Docker API the supervision goroutines
// touch. The embedded interface is nil: any method the test does not set is a
// panic, so an unexpected daemon call fails loudly instead of silently.
type stubDocker struct {
	client.APIClient

	waitFn func(ctx context.Context) (<-chan container.WaitResponse, <-chan error)

	mu      sync.Mutex
	killed  []string
	removed []string
}

func (s *stubDocker) ContainerWait(
	ctx context.Context, _ string, _ container.WaitCondition,
) (<-chan container.WaitResponse, <-chan error) {
	return s.waitFn(ctx)
}

func (s *stubDocker) ContainerKill(_ context.Context, id, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.killed = append(s.killed, id)

	return nil
}

func (s *stubDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.removed = append(s.removed, id)

	return nil
}

func (s *stubDocker) ContainerCreate(
	_ context.Context, _ *container.Config, _ *container.HostConfig,
	_ *network.NetworkingConfig, _ *ocispec.Platform, _ string,
) (container.CreateResponse, error) {
	return container.CreateResponse{ID: "container-1"}, nil
}

func (s *stubDocker) ContainerAttach(
	_ context.Context, _ string, _ container.AttachOptions,
) (types.HijackedResponse, error) {
	local, remote := net.Pipe()
	_ = remote.Close()

	return types.HijackedResponse{Conn: local, Reader: bufio.NewReader(bytes.NewReader(nil))}, nil
}

func (s *stubDocker) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	return nil
}

// exitsWith yields a wait result carrying code, the shape of a container that
// ran to completion on its own.
func exitsWith(code int64) func(context.Context) (<-chan container.WaitResponse, <-chan error) {
	return func(context.Context) (<-chan container.WaitResponse, <-chan error) {
		wc := make(chan container.WaitResponse, 1)
		wc <- container.WaitResponse{StatusCode: code}

		return wc, make(chan error, 1)
	}
}

// flaggedBy yields a wait result the daemon flagged with an error. The status
// code in such a response is not trustworthy, and the container was never
// killed by this process.
func flaggedBy(message string) func(context.Context) (<-chan container.WaitResponse, <-chan error) {
	return func(context.Context) (<-chan container.WaitResponse, <-chan error) {
		wc := make(chan container.WaitResponse, 1)
		wc <- container.WaitResponse{Error: &container.WaitExitError{Message: message}}

		return wc, make(chan error, 1)
	}
}

// hangsUntilDeadline never yields a result, so the container timeout fires and
// waitCtx reports DeadlineExceeded on errCh - the timeout-kill path.
func hangsUntilDeadline(ctx context.Context) (<-chan container.WaitResponse, <-chan error) {
	ec := make(chan error, 1)

	go func() {
		<-ctx.Done()

		ec <- ctx.Err()
	}()

	return make(chan container.WaitResponse), ec
}

// failsImmediately yields a wait error while the context is still live - the
// wait-failure path, which shares its exit code with the timeout kill.
func failsImmediately(context.Context) (<-chan container.WaitResponse, <-chan error) {
	ec := make(chan error, 1)
	ec <- errors.New("daemon connection lost")

	return make(chan container.WaitResponse), ec
}

// TestWaitAndCleanupReportsExitCause pins every way a run can end that the
// exit code alone cannot carry: both kill paths report -1, both recorded kills
// report a normal wait with the SIGKILL code, and a wait the daemon flagged
// reports a status code that is not trustworthy. Only the cause separates them.
func TestWaitAndCleanupReportsExitCause(t *testing.T) {
	tests := []struct {
		name     string
		waitFn   func(context.Context) (<-chan container.WaitResponse, <-chan error)
		reason   string
		wantCode int64
		wantCaus ExitCause
		wantKill bool
	}{
		{"normal exit", exitsWith(0), "", 0, ExitNormal, false},
		{"non-zero exit", exitsWith(7), "", 7, ExitNormal, false},
		{"timeout kill", hangsUntilDeadline, "", -1, ExitTimeout, true},
		{"wait failure", failsImmediately, "", -1, ExitWaitFailure, true},
		{"daemon-flagged wait", flaggedBy("no such container"), "", 0, ExitDaemonError, false},
		{"idle watchdog kill", exitsWith(137), metrics.OutcomeIdleTimeout, 137, ExitIdleTimeout, false},
		{"requested kill", exitsWith(137), metrics.OutcomeKilled, 137, ExitKilled, false},
		{
			"the container timeout outranks a recorded kill",
			hangsUntilDeadline, metrics.OutcomeKilled, -1, ExitTimeout, true,
		},
		{
			"a recorded kill outranks a daemon-flagged wait",
			flaggedBy("no such container"), metrics.OutcomeIdleTimeout, 0, ExitIdleTimeout, false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			docker := &stubDocker{waitFn: tc.waitFn}
			got := make(chan exitCall, 1)
			tracker := NewTracker(1)

			if tc.reason != "" {
				require.True(t, tracker.AddIfUnderLimit(&Run{Project: "proj", CardID: "CARD-1"}))
				tracker.SetReason("proj", "CARD-1", tc.reason)
			}

			e := NewDockerExecutor(Config{
				Docker:           docker,
				Tracker:          tracker,
				ContainerTimeout: 30 * time.Millisecond,
				Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
				OnExit: func(_, _ string, code int64, cause ExitCause, attempt int, correlationID string) {
					got <- exitCall{exitCode: code, cause: cause, attempt: attempt, correlationID: correlationID}
				},
			})

			conn, other := net.Pipe()

			t.Cleanup(func() { _ = other.Close() })

			pumpDone := make(chan struct{})
			close(pumpDone)

			e.waitAndCleanup("proj", "CARD-1", "cid-1", 1, "corr-1", time.Now(),
				types.HijackedResponse{Conn: conn}, make(chan struct{}), pumpDone,
				slog.New(slog.NewTextHandler(io.Discard, nil)))

			select {
			case call := <-got:
				assert.Equal(t, tc.wantCode, call.exitCode)
				assert.Equal(t, tc.wantCaus, call.cause)
			default:
				t.Fatal("onExit never fired")
			}

			docker.mu.Lock()
			defer docker.mu.Unlock()

			assert.Equal(t, tc.wantKill, len(docker.killed) == 1, "kill expectation")
			assert.Equal(t, []string{"cid-1"}, docker.removed, "the container is always removed")
		})
	}
}

// TestLaunchCarriesTheAttemptOrdinalToOnExit pins the hop that keeps a
// host-written terminal event separable per container run: the ordinal the
// launch spec carries is the one the exit callback reports. Without it every
// run's terminal event reads as attempt 1.
func TestLaunchCarriesTheAttemptOrdinalToOnExit(t *testing.T) {
	docker := &stubDocker{waitFn: exitsWith(0)}
	got := make(chan exitCall, 1)

	e := NewDockerExecutor(Config{
		Docker:           docker,
		Tracker:          NewTracker(1),
		PullPolicy:       PullNever,
		ContainerTimeout: time.Second,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnExit: func(_, _ string, code int64, cause ExitCause, attempt int, correlationID string) {
			got <- exitCall{exitCode: code, cause: cause, attempt: attempt, correlationID: correlationID}
		},
	})

	require.NoError(t, e.Launch(t.Context(), LaunchSpec{
		Project: "proj",
		CardID:  "CARD-1",
		Image:   "alpine:3",
		Attempt: 3,
	}))

	select {
	case call := <-got:
		assert.Equal(t, 3, call.attempt)
		assert.Equal(t, ExitNormal, call.cause)
	case <-time.After(5 * time.Second):
		t.Fatal("onExit never fired")
	}
}

// TestLaunchCarriesTheCorrelationIDToOnExit pins the other half of the
// per-run identity the exit callback needs: unlike the attempt ordinal (which
// collapses to 1 for every run when file logging is disabled), the
// correlation id is always distinct per admitted trigger, so it is what a
// caller must use to tell a stale run's exit apart from a fresh one racing in
// behind it.
func TestLaunchCarriesTheCorrelationIDToOnExit(t *testing.T) {
	docker := &stubDocker{waitFn: exitsWith(0)}
	got := make(chan exitCall, 1)

	e := NewDockerExecutor(Config{
		Docker:           docker,
		Tracker:          NewTracker(1),
		PullPolicy:       PullNever,
		ContainerTimeout: time.Second,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnExit: func(_, _ string, code int64, cause ExitCause, attempt int, correlationID string) {
			got <- exitCall{exitCode: code, cause: cause, attempt: attempt, correlationID: correlationID}
		},
	})

	require.NoError(t, e.Launch(t.Context(), LaunchSpec{
		Project:       "proj",
		CardID:        "CARD-1",
		Image:         "alpine:3",
		CorrelationID: "trace-xyz",
	}))

	select {
	case call := <-got:
		assert.Equal(t, "trace-xyz", call.correlationID)
	case <-time.After(5 * time.Second):
		t.Fatal("onExit never fired")
	}
}
