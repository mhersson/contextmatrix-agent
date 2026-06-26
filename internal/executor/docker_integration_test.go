package executor

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/metrics"
)

// integrationGuard skips the test unless CMX_TEST_DOCKER is set. These tests
// require a reachable Docker daemon and pull the alpine image.
func integrationGuard(t *testing.T) {
	t.Helper()

	if os.Getenv("CMX_TEST_DOCKER") == "" {
		t.Skip("set CMX_TEST_DOCKER=1 to run docker integration tests")
	}
}

const alpineImage = "alpine:3"

// exitRecorder collects onExit callbacks so a test can wait for the exit and
// assert the code.
type exitRecorder struct {
	mu    sync.Mutex
	done  chan struct{}
	once  sync.Once
	code  int64
	fired bool
}

func newExitRecorder() *exitRecorder {
	return &exitRecorder{done: make(chan struct{})}
}

func (r *exitRecorder) onExit(_, _ string, code int64) {
	r.mu.Lock()
	r.code = code
	r.fired = true
	r.mu.Unlock()
	r.once.Do(func() { close(r.done) })
}

func (r *exitRecorder) wait(t *testing.T, d time.Duration) int64 {
	t.Helper()

	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()

		return r.code
	case <-time.After(d):
		t.Fatalf("onExit did not fire within %s", d)

		return 0
	}
}

// logCollector accumulates onLog lines.
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCollector) onLog(_, _ string, line []byte, _ bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lines = append(c.lines, string(line))
}

func (c *logCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.lines...)
}

func newTestExecutor(t *testing.T, cfg Config) *DockerExecutor {
	t.Helper()

	cli, err := NewClient()
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })

	cfg.Docker = cli

	if cfg.Tracker == nil {
		cfg.Tracker = NewTracker(8)
	}

	if cfg.PullPolicy == "" {
		cfg.PullPolicy = PullIfNotPresent
	}

	return NewDockerExecutor(cfg)
}

func TestIntegration_LaunchEchoAndExit(t *testing.T) {
	integrationGuard(t)

	exits := newExitRecorder()
	logs := &logCollector{}
	m := metrics.New()

	exec := newTestExecutor(t, Config{
		ContainerTimeout: 30 * time.Second,
		OnExit:           exits.onExit,
		OnLog:            logs.onLog,
		Metrics:          m,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		project = "demo"
		card    = "ECHO-1"
	)

	spec := LaunchSpec{
		CardID:        card,
		Project:       project,
		Image:         alpineImage,
		MemoryBytes:   256 * 1024 * 1024,
		PidsLimit:     128,
		CorrelationID: "corr-echo",
		Cmd:           []string{"sh", "-c", "read line; echo got:$line; sleep 1"},
	}

	require.NoError(t, exec.Launch(ctx, spec))

	run, ok := exec.tracker.Get(project, card)
	require.True(t, ok, "run must be tracked after launch")

	// Inspect: the container must carry the agent labels.
	info, err := exec.docker.ContainerInspect(ctx, run.ContainerID)
	require.NoError(t, err)
	assert.Equal(t, "true", info.Config.Labels[labelAgent])
	assert.Equal(t, card, info.Config.Labels[labelCardID])
	assert.Equal(t, project, info.Config.Labels[labelProject])
	assert.Equal(t, "corr-echo", info.Config.Labels[labelCorrelationID])

	// Drive stdin: the container reads one line, echoes it, then exits 0.
	_, err = run.Stdin.Write([]byte("hello\n"))
	require.NoError(t, err)

	code := exits.wait(t, 30*time.Second)
	assert.Equal(t, int64(0), code, "clean exit")

	// Verify that waitAndCleanup observed exactly one container_duration sample.
	// The observation happens before onExit fires, so by the time wait() returns
	// the histogram is already populated.
	assert.Equal(t, 1, testutil.CollectAndCount(m.ContainerDuration), "one container_duration series after one exit")

	mfs, err := m.Registry.Gather()
	require.NoError(t, err)

	var durationFound bool

	for _, mf := range mfs {
		if mf.GetName() != "cm_agent_container_duration_seconds" {
			continue
		}

		require.Len(t, mf.GetMetric(), 1, "exactly one outcome label combination")

		var outcomeVal string

		for _, lp := range mf.GetMetric()[0].GetLabel() {
			if lp.GetName() == "outcome" {
				outcomeVal = lp.GetValue()
			}
		}

		assert.Equal(t, metrics.OutcomeSuccess, outcomeVal, "outcome label must be success")
		assert.Equal(t, uint64(1), mf.GetMetric()[0].GetHistogram().GetSampleCount(), "histogram sample count must be 1")

		durationFound = true

		break
	}

	require.True(t, durationFound, "cm_agent_container_duration_seconds family must be present")

	// Pump captured the echoed line.
	assert.Eventually(t, func() bool {
		for _, l := range logs.snapshot() {
			if l == "got:hello" {
				return true
			}
		}

		return false
	}, 5*time.Second, 50*time.Millisecond, "expected pump to capture got:hello")

	// Container removed, tracker empty.
	assert.Eventually(t, func() bool {
		return exec.tracker.Count() == 0
	}, 5*time.Second, 50*time.Millisecond)

	_, err = exec.docker.ContainerInspect(ctx, run.ContainerID)
	assert.Error(t, err, "container must be removed after exit")
}

func TestIntegration_ContainerTimeout(t *testing.T) {
	integrationGuard(t)

	exits := newExitRecorder()

	exec := newTestExecutor(t, Config{
		ContainerTimeout: 300 * time.Millisecond,
		OnExit:           exits.onExit,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	require.NoError(t, exec.Launch(ctx, LaunchSpec{
		CardID:      "TIMEOUT-1",
		Project:     "demo",
		Image:       alpineImage,
		MemoryBytes: 256 * 1024 * 1024,
		PidsLimit:   128,
		Cmd:         []string{"sleep", "60"},
	}))

	code := exits.wait(t, 30*time.Second)
	assert.Equal(t, int64(-1), code, "timeout kill reports -1")

	assert.Eventually(t, func() bool {
		return exec.tracker.Count() == 0
	}, 5*time.Second, 50*time.Millisecond)
}

func TestIntegration_IdleWatchdogKillsSilentContainer(t *testing.T) {
	integrationGuard(t)

	exits := newExitRecorder()

	exec := newTestExecutor(t, Config{
		ContainerTimeout: 60 * time.Second,
		IdleTimeout:      200 * time.Millisecond,
		PollInterval:     50 * time.Millisecond,
		OnExit:           exits.onExit,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	require.NoError(t, exec.Launch(ctx, LaunchSpec{
		CardID:      "IDLE-1",
		Project:     "demo",
		Image:       alpineImage,
		MemoryBytes: 256 * 1024 * 1024,
		PidsLimit:   128,
		Cmd:         []string{"sleep", "60"},
	}))

	// Silent container: no output, idle watchdog must kill it well before the
	// container timeout. The watchdog SIGKILLs independently, so ContainerWait
	// returns a normal result with code 137 (128 + SIGKILL) via the wait path
	// rather than the timeout path's synthetic -1.
	code := exits.wait(t, 10*time.Second)
	assert.Equal(t, int64(137), code, "idle SIGKILL surfaces 137 via the wait path")

	assert.Eventually(t, func() bool {
		return exec.tracker.Count() == 0
	}, 5*time.Second, 50*time.Millisecond)
}

func TestIntegration_IdleWatchdogSuspendedWhileAwaiting(t *testing.T) {
	integrationGuard(t)

	exits := newExitRecorder()

	exec := newTestExecutor(t, Config{
		ContainerTimeout: 60 * time.Second,
		IdleTimeout:      200 * time.Millisecond,
		PollInterval:     50 * time.Millisecond,
		OnExit:           exits.onExit,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		project = "demo"
		card    = "AWAIT-1"
	)

	require.NoError(t, exec.Launch(ctx, LaunchSpec{
		CardID:      card,
		Project:     project,
		Image:       alpineImage,
		MemoryBytes: 256 * 1024 * 1024,
		PidsLimit:   128,
		Cmd:         []string{"sleep", "60"},
	}))

	// Mark awaiting-human BEFORE the idle window elapses; the watchdog must
	// suspend and leave the silent container running.
	exec.tracker.SetAwaiting(project, card, true)

	// Wait well past the idle window; the container must survive.
	time.Sleep(1 * time.Second)

	select {
	case <-exits.done:
		t.Fatal("container was killed while awaiting human input")
	default:
	}

	assert.Equal(t, 1, exec.tracker.Count(), "awaiting container still tracked")

	// Clean up: clearing awaiting lets the idle watchdog reap it.
	run, ok := exec.tracker.Get(project, card)
	require.True(t, ok)

	require.NoError(t, exec.docker.ContainerKill(ctx, run.ContainerID, "SIGKILL"))

	// SIGKILL of a running process yields exit code 137 (128 + SIGKILL); the
	// wait branch (not the timeout branch) surfaces it, so it is NOT -1.
	code := exits.wait(t, 10*time.Second)
	assert.Equal(t, int64(137), code, "SIGKILL surfaces 137 via the wait path")

	assert.Eventually(t, func() bool {
		return exec.tracker.Count() == 0
	}, 5*time.Second, 50*time.Millisecond)
}

// stopAllSweep is referenced to keep the container import in use when only a
// subset of tests compile; it exercises StopAll over a fresh launch.
func TestIntegration_StopAllAndCleanupOrphans(t *testing.T) {
	integrationGuard(t)

	exits := newExitRecorder()

	exec := newTestExecutor(t, Config{
		ContainerTimeout: 60 * time.Second,
		OnExit:           exits.onExit,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	require.NoError(t, exec.Launch(ctx, LaunchSpec{
		CardID:      "STOP-1",
		Project:     "demo",
		Image:       alpineImage,
		MemoryBytes: 256 * 1024 * 1024,
		PidsLimit:   128,
		Cmd:         []string{"sleep", "60"},
	}))

	killed, err := exec.StopAll(ctx, "demo")
	require.NoError(t, err)
	assert.Len(t, killed, 1)

	code := exits.wait(t, 30*time.Second)
	assert.Equal(t, int64(137), code, "SIGKILL surfaces 137 via the wait path")

	assert.Eventually(t, func() bool {
		return exec.tracker.Count() == 0
	}, 5*time.Second, 50*time.Millisecond)

	// CleanupOrphans is a no-op now (nothing labeled remains) but must not error.
	require.NoError(t, exec.CleanupOrphans(ctx))

	left, err := exec.docker.ContainerList(ctx, container.ListOptions{All: true})
	require.NoError(t, err)

	for _, c := range left {
		assert.NotEqual(t, "true", c.Labels[labelAgent], "no agent container should remain")
	}
}
