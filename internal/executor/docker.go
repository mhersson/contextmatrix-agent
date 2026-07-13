package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mhersson/contextmatrix-agent/internal/metrics"
)

// Container labels. The agent label marks every container this executor owns so
// the boot-time orphan sweep and CM's reconcile sweep can find them by filter.
const (
	labelAgent         = "contextmatrix.agent"
	labelCardID        = "contextmatrix.card_id"
	labelProject       = "contextmatrix.project"
	labelCorrelationID = "contextmatrix.correlation_id"
)

// secretsMountPath is where the per-project secrets dir is mounted read-only
// inside the container.
const secretsMountPath = "/run/cm-secrets" //nolint:gosec // path, not a credential

// skillsMountPath is where the resolved task-skills dir is mounted read-only
// inside the container. The worker reads it via CMX_TASK_SKILLS_DIR.
const skillsMountPath = "/run/cm-skills" //nolint:gosec // path, not a credential

// caCertMountPath is where the optional extra-CA PEM is mounted read-only
// inside the container. The worker reads it via CMX_CA_CERT_FILE and threads
// it onto its git/gh subprocesses.
const caCertMountPath = "/run/cm-ca/ca.crt" //nolint:gosec // path, not a credential

// scannerBufferMax bounds the per-line buffer of the stdout/stderr pump so a
// pathological container cannot pin the host heap with one unbounded line.
const scannerBufferMax = 1 << 20 // 1 MiB

// killGraceTimeout is how long Kill waits for a container to exit on its
// own before SIGKILL. The terminal-state cleanup fires the moment a card
// completes; killing the worker mid-teardown records exit 137 for a
// successful run and labels its duration metric "killed".
const killGraceTimeout = 3 * time.Second

// Image pull policies.
const (
	PullNever        = "never"
	PullIfNotPresent = "if-not-present"
	PullAlways       = "always"
)

// Sentinel errors callers match with errors.Is.
var (
	// ErrCapacity is returned by Launch when the tracker is already at its
	// concurrency limit. The created container is removed before returning.
	ErrCapacity = errors.New("executor: at capacity")
	// ErrNotFound is returned by Kill when no run is tracked for the key.
	ErrNotFound = errors.New("executor: container not found")
)

// containerWaiter is the one-method slice of the Docker API Kill needs to
// observe a self-exit, narrow so tests can fake it.
type containerWaiter interface {
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
}

// waitForSelfExit reports whether the container left the running state on
// its own within grace. Wait errors and the grace timeout both report
// false — the caller then kills.
func waitForSelfExit(ctx context.Context, docker containerWaiter, containerID string, grace time.Duration) bool {
	waitCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()

	waitCh, errCh := docker.ContainerWait(waitCtx, containerID, container.WaitConditionNotRunning)

	select {
	case <-waitCh:
		return true
	case <-errCh:
		return false
	}
}

// LaunchSpec is the fully-resolved description of one container to launch. The
// caller has already applied any payload image override and assembled Env and
// the secrets bind source.
type LaunchSpec struct {
	CardID, Project string
	Image           string // payload override already applied by the caller
	Env             []string
	SecretsHostDir  string // bind source; mounted read-only at /run/cm-secrets
	SkillsHostDir   string // bind source; mounted read-only at /run/cm-skills (empty = no skills)
	CACertHostFile  string // bind source; mounted read-only at /run/cm-ca/ca.crt (empty = no extra CA)
	MemoryBytes     int64
	PidsLimit       int64
	CorrelationID   string

	// MCPURL is the CM MCP endpoint the worker connects to. Its hostname is
	// pinned into the container's /etc/hosts (see buildExtraHosts) so a name
	// that only resolves on the host stays reachable inside the container.
	MCPURL string

	// Cmd overrides the image entrypoint command. Used by integration tests to
	// run a deterministic command against a stock image; harmless in production
	// where it is left nil and the worker image's own entrypoint runs.
	Cmd []string
}

// StopResult is the outcome of killing one tracked run in a StopAll sweep. Err
// is nil when the kill succeeded or the container was already gone.
type StopResult struct {
	Run *Run
	Err error
}

// Executor is the seam a future KubernetesExecutor implements. The serve layer
// depends on this interface, not on DockerExecutor.
type Executor interface {
	Launch(ctx context.Context, spec LaunchSpec) error
	Kill(ctx context.Context, project, cardID string) error
	List(ctx context.Context) ([]*Run, error)
	StopAll(ctx context.Context, project string) ([]StopResult, error)
	CleanupOrphans(ctx context.Context) error
}

var containerNameRe = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// containerName builds a Docker-legal container name from project and cardID.
// The card ID is lowercased and any character outside Docker's allowed set
// [a-zA-Z0-9_.-] is replaced with a dash.
func containerName(project, cardID string) string {
	name := strings.ToLower(fmt.Sprintf("cm-agent-%s-%s", project, cardID))

	return containerNameRe.ReplaceAllString(name, "-")
}

// containerConfig is the pure mapping from a LaunchSpec to the Docker create
// configs. It performs no I/O so it is fully unit-testable without a daemon.
func containerConfig(spec LaunchSpec) (*container.Config, *container.HostConfig) {
	labels := map[string]string{
		labelAgent:   "true",
		labelCardID:  spec.CardID,
		labelProject: spec.Project,
	}
	if spec.CorrelationID != "" {
		labels[labelCorrelationID] = spec.CorrelationID
	}

	cfg := &container.Config{
		Image:       spec.Image,
		Env:         spec.Env,
		Labels:      labels,
		Cmd:         spec.Cmd,
		User:        "1000:1000",
		OpenStdin:   true,
		AttachStdin: true,
		StdinOnce:   false,
		// Tty defaults to false; the stdcopy demux below requires a multiplexed
		// (non-TTY) stream.
	}

	pidsLimit := spec.PidsLimit

	host := &container.HostConfig{
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		Resources: container.Resources{
			Memory:    spec.MemoryBytes,
			PidsLimit: &pidsLimit,
		},
	}

	if spec.SecretsHostDir != "" {
		host.Binds = append(host.Binds, spec.SecretsHostDir+":"+secretsMountPath+":ro")
	}

	if spec.SkillsHostDir != "" {
		host.Binds = append(host.Binds, spec.SkillsHostDir+":"+skillsMountPath+":ro")
	}

	if spec.CACertHostFile != "" {
		// Mount the operator's extra CA read-only and point the worker at it via
		// CMX_CA_CERT_FILE — the only CA var set at the container level. The
		// worker reads it for its own outbound TLS (LLM, MCP) and threads the path
		// onto its git/gh subprocesses explicitly. GIT_SSL_CAINFO / GH_CA_BUNDLE /
		// SSL_CERT_FILE are deliberately NOT set here: the harness scrubs
		// subprocess env, so they would be dead, and advertising them would wrongly
		// imply git/gh inherit the container env.
		host.Binds = append(host.Binds, spec.CACertHostFile+":"+caCertMountPath+":ro")
		cfg.Env = append(cfg.Env, "CMX_CA_CERT_FILE="+caCertMountPath)
	}

	return cfg, host
}

// DockerExecutor launches one container per card via the Docker SDK. It owns
// the tracker and the per-run supervision goroutines (output pump, wait +
// cleanup, idle watchdog). Dependencies are injected; there is no global state.
type DockerExecutor struct {
	docker     client.APIClient
	tracker    *Tracker
	pullPolicy string

	// resolver resolves the MCP hostname into the container's ExtraHosts.
	// Defaulted to net.DefaultResolver; swappable in tests.
	resolver hostResolver

	containerTimeout time.Duration
	idleTimeout      time.Duration
	pollInterval     time.Duration

	onStart func(project, cardID, containerID string)
	onExit  func(project, cardID string, exitCode int64)
	onLog   func(project, cardID string, line []byte, stderr bool)

	logger  *slog.Logger
	metrics *metrics.Metrics
}

// Config carries the DockerExecutor dependencies. onStart, onExit, and onLog
// may be nil (the executor no-ops them) but the serve layer wires all three.
type Config struct {
	Docker     client.APIClient
	Tracker    *Tracker
	PullPolicy string

	ContainerTimeout time.Duration
	IdleTimeout      time.Duration
	PollInterval     time.Duration

	OnStart func(project, cardID, containerID string)
	OnExit  func(project, cardID string, exitCode int64)
	OnLog   func(project, cardID string, line []byte, stderr bool)

	Logger *slog.Logger
	// Metrics is the Prometheus bundle. Nil disables container-duration
	// observation.
	Metrics *metrics.Metrics
}

// NewDockerExecutor wires a DockerExecutor from its dependencies.
func NewDockerExecutor(cfg Config) *DockerExecutor {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &DockerExecutor{
		docker:           cfg.Docker,
		tracker:          cfg.Tracker,
		pullPolicy:       cfg.PullPolicy,
		resolver:         net.DefaultResolver,
		containerTimeout: cfg.ContainerTimeout,
		idleTimeout:      cfg.IdleTimeout,
		pollInterval:     cfg.PollInterval,
		onStart:          cfg.OnStart,
		onExit:           cfg.OnExit,
		onLog:            cfg.OnLog,
		logger:           logger,
		metrics:          cfg.Metrics,
	}
}

// NewClient builds a Docker API client from the environment with API version
// negotiation. Returned as the concrete *client.Client; consumers depend on the
// client.APIClient interface.
func NewClient() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("new docker client: %w", err)
	}

	return cli, nil
}

// Launch pulls (per policy), creates, attaches stdin+stdout+stderr, admits the
// run to the tracker, starts the container, and spawns the supervision
// goroutines. On any failure after create, the container is removed so nothing
// leaks. ErrCapacity is returned when the tracker is full.
func (e *DockerExecutor) Launch(ctx context.Context, spec LaunchSpec) error {
	log := e.logger.With("project", spec.Project, "card_id", spec.CardID)

	if err := e.pull(ctx, spec.Image, log); err != nil {
		return fmt.Errorf("pull image %q: %w", spec.Image, err)
	}

	cfg, host := containerConfig(spec)
	host.ExtraHosts = buildExtraHosts(e.resolver, spec.MCPURL, log)
	name := containerName(spec.Project, spec.CardID)

	resp, err := e.docker.ContainerCreate(ctx, cfg, host, &network.NetworkingConfig{}, &ocispec.Platform{}, name)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	// Attach BEFORE start so no early output is missed and stdin is ready for
	// the control frames (message/promote/end_session) the serve layer writes
	// over the run.
	attach, err := e.docker.ContainerAttach(ctx, resp.ID, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		e.removeContainer(ctx, resp.ID, log)

		return fmt.Errorf("attach container: %w", err)
	}

	run := &Run{
		ContainerID: resp.ID,
		CardID:      spec.CardID,
		Project:     spec.Project,
		StartedAt:   time.Now(),
		Stdin:       attach.Conn,
	}

	if !e.tracker.AddIfUnderLimit(run) {
		attach.Close()
		e.removeContainer(ctx, resp.ID, log)

		return ErrCapacity
	}

	if err := e.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		e.tracker.Remove(spec.Project, spec.CardID)
		attach.Close()
		e.removeContainer(ctx, resp.ID, log)

		return fmt.Errorf("start container: %w", err)
	}

	// Seed the idle clock so a container that exits before any output is not
	// flagged idle retroactively.
	e.tracker.Touch(spec.Project, spec.CardID)

	// Signal the run started now that the container ID and card are both known,
	// before the pump starts, so a per-card log header precedes any output.
	if e.onStart != nil {
		e.onStart(spec.Project, spec.CardID, resp.ID)
	}

	done := make(chan struct{})

	go e.pump(spec.Project, spec.CardID, attach.Reader, log)
	go e.runIdleWatchdog(spec.Project, spec.CardID, resp.ID, done, log)
	// waitAndCleanup deliberately runs on a detached context: the container's
	// supervision must outlive the request ctx that triggered Launch, otherwise
	// a returned webhook handler would cancel a still-running container's wait
	// and cleanup. The container timeout is the bound.
	//nolint:gosec // G118: detached ctx is intentional; container outlives the request
	go e.waitAndCleanup(spec.Project, spec.CardID, resp.ID, run.StartedAt, attach, done, log)

	log.Info("container launched", "container_id", truncateID(resp.ID), "name", name)

	return nil
}

// pump demultiplexes the attach reader into stdout/stderr line streams, calling
// onLog and tracker.Touch for every completed line.
func (e *DockerExecutor) pump(project, cardID string, r io.Reader, log *slog.Logger) {
	stdoutW := newLineWriter(func(line []byte) {
		e.tracker.Touch(project, cardID)

		if e.onLog != nil {
			e.onLog(project, cardID, line, false)
		}
	})
	stderrW := newLineWriter(func(line []byte) {
		e.tracker.Touch(project, cardID)

		if e.onLog != nil {
			e.onLog(project, cardID, line, true)
		}
	})

	if _, err := stdcopy.StdCopy(stdoutW, stderrW, r); err != nil &&
		!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		log.Debug("output pump ended", "error", err)
	}

	stdoutW.Flush()
	stderrW.Flush()
}

// waitAndCleanup blocks on ContainerWait under the container timeout, kills on
// timeout, force-removes the container, observes its duration by outcome,
// clears the tracker entry, closes the attach connection, signals the watchdog
// via done, and fires onExit.
func (e *DockerExecutor) waitAndCleanup(
	project, cardID, containerID string,
	startedAt time.Time,
	attach types.HijackedResponse,
	done chan struct{},
	log *slog.Logger,
) {
	defer close(done)
	defer attach.Close()

	exitCode := int64(0)
	timedOut := false

	waitCtx, cancel := context.WithTimeout(context.Background(), e.containerTimeout)
	defer cancel()

	waitCh, errCh := e.docker.ContainerWait(waitCtx, containerID, container.WaitConditionNotRunning)

	select {
	case res := <-waitCh:
		exitCode = res.StatusCode
		if res.Error != nil {
			log.Warn("container wait reported error", "error", res.Error.Message)
		}
	case err := <-errCh:
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			log.Warn("container timed out, killing", "timeout", e.containerTimeout)

			timedOut = true
		} else {
			log.Warn("container wait failed, killing", "error", err)
		}

		e.kill(containerID, log)

		exitCode = -1
	}

	rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rmCancel()

	if err := e.docker.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true}); err != nil {
		log.Warn("failed to remove container", "container_id", truncateID(containerID), "error", err)
	}

	if e.metrics != nil {
		outcome := resolveOutcome(timedOut, e.tracker.Reason(project, cardID), exitCode)
		e.metrics.ContainerDuration.WithLabelValues(outcome).Observe(time.Since(startedAt).Seconds())
	}

	e.tracker.Remove(project, cardID)

	log.Info("container exited", "exit_code", exitCode)

	if e.onExit != nil {
		e.onExit(project, cardID, exitCode)
	}
}

// runIdleWatchdog kills a container that produces no output for longer than the
// idle timeout, unless the run is awaiting human input. It stops when done is
// closed by waitAndCleanup. Disabled when idleTimeout or pollInterval <= 0.
func (e *DockerExecutor) runIdleWatchdog(project, cardID, containerID string, done <-chan struct{}, log *slog.Logger) {
	if e.idleTimeout <= 0 || e.pollInterval <= 0 {
		return
	}

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if e.tracker.Awaiting(project, cardID) {
				continue
			}

			last := e.tracker.LastActivity(project, cardID)
			if last.IsZero() || time.Since(last) <= e.idleTimeout {
				continue
			}

			log.Warn("container hit idle timeout, killing",
				"idle_timeout", e.idleTimeout,
				"last_activity", last.Format(time.RFC3339Nano),
			)
			e.tracker.SetReason(project, cardID, metrics.OutcomeIdleTimeout)
			e.kill(containerID, log)

			return
		}
	}
}

// Kill stops the tracked container for project/cardID. It first waits up to
// killGraceTimeout for the container to exit on its own; if it does, Kill
// returns nil without sending SIGKILL and without recording a kill reason.
// Only when the grace window elapses without a self-exit does Kill record
// the kill reason and send SIGKILL. Removal is handled by waitAndCleanup.
// Returns ErrNotFound when no run is tracked.
func (e *DockerExecutor) Kill(ctx context.Context, project, cardID string) error {
	run, ok := e.tracker.Get(project, cardID)
	if !ok {
		return fmt.Errorf("%w: %s/%s", ErrNotFound, project, cardID)
	}

	// Give the worker a short window to exit on its own; the reason is only
	// recorded (and SIGKILL sent) when it does not.
	if waitForSelfExit(ctx, e.docker, run.ContainerID, killGraceTimeout) {
		return nil
	}

	e.tracker.SetReason(project, cardID, metrics.OutcomeKilled)

	if err := e.docker.ContainerKill(ctx, run.ContainerID, "SIGKILL"); err != nil {
		return fmt.Errorf("kill container %s/%s: %w", project, cardID, err)
	}

	return nil
}

// List returns a snapshot of the currently tracked runs.
func (e *DockerExecutor) List(_ context.Context) ([]*Run, error) {
	return e.tracker.List(), nil
}

// StopAll kills every tracked run, filtered to project when non-empty (empty
// project means all), and returns a per-card outcome for every run attempted.
// Failures are included in the results (Err != nil) rather than swallowed, so
// the caller can surface partial failures to the CM operator. Runs are killed
// serially and each Kill inherits the self-exit grace window, so stopping N
// still-running containers can block up to N x killGraceTimeout — accepted,
// since N is small and this is the operator bulk-stop path, not a hot path.
func (e *DockerExecutor) StopAll(ctx context.Context, project string) ([]StopResult, error) {
	var results []StopResult

	for _, run := range e.tracker.List() {
		if project != "" && run.Project != project {
			continue
		}

		err := e.Kill(ctx, run.Project, run.CardID)
		if errors.Is(err, ErrNotFound) {
			err = nil // already gone is a success
		}

		if err != nil {
			e.logger.Warn("stop-all kill failed",
				"project", run.Project, "card_id", run.CardID, "error", err)
		}

		results = append(results, StopResult{Run: run, Err: err})
	}

	return results, nil
}

// CleanupOrphans force-removes every agent-labeled container found at boot.
// Anything matching is orphaned by definition — the tracker is empty in a fresh
// process, so a labeled container is a leftover from a previous run. This
// assumes exclusive ownership of contextmatrix.agent-labeled containers on the
// daemon; a second executor process sharing the Docker daemon would have its
// live containers swept.
func (e *DockerExecutor) CleanupOrphans(ctx context.Context) error {
	containers, err := e.docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelAgent+"=true")),
	})
	if err != nil {
		return fmt.Errorf("list orphan containers: %w", err)
	}

	for _, ctr := range containers {
		log := e.logger.With(
			"container_id", truncateID(ctr.ID),
			"card_id", ctr.Labels[labelCardID],
			"project", ctr.Labels[labelProject],
		)
		log.Info("removing orphan container")

		if err := e.docker.ContainerRemove(ctx, ctr.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Warn("failed to remove orphan container", "error", err)
		}
	}

	return nil
}

// ImageSummary is one tagged image in the node's local image store, in
// executor-neutral form. Name filtering is the webhook layer's policy; the
// executor reports everything tagged.
type ImageSummary struct {
	Tags      []string
	Digests   []string
	CreatedAt int64
	SizeBytes int64
}

// ListImages returns the tagged images present in the node's local image
// store. Dangling images (no repo tags) are skipped.
func (e *DockerExecutor) ListImages(ctx context.Context) ([]ImageSummary, error) {
	summaries, err := e.docker.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("image list: %w", err)
	}

	return imageSummaries(summaries), nil
}

// imageSummaries maps Docker image summaries to ImageSummary, dropping
// dangling images and the "<none>:<none>" placeholder tag Docker reports for
// them.
func imageSummaries(in []image.Summary) []ImageSummary {
	out := make([]ImageSummary, 0, len(in))

	for _, s := range in {
		tags := make([]string, 0, len(s.RepoTags))

		for _, tag := range s.RepoTags {
			if tag != "<none>:<none>" {
				tags = append(tags, tag)
			}
		}

		if len(tags) == 0 {
			continue
		}

		out = append(out, ImageSummary{
			Tags:      tags,
			Digests:   s.RepoDigests,
			CreatedAt: s.Created,
			SizeBytes: s.Size,
		})
	}

	return out
}

// pull applies the executor's image pull policy: never skips, if-not-present
// pulls only when the image is absent locally, always pulls unconditionally.
func (e *DockerExecutor) pull(ctx context.Context, img string, log *slog.Logger) error {
	switch e.pullPolicy {
	case PullNever:
		return nil
	case PullIfNotPresent:
		if _, err := e.docker.ImageInspect(ctx, img); err == nil {
			log.Debug("image present locally, skipping pull", "image", img)

			return nil
		}
	case PullAlways:
		// Fall through to pull.
	default:
		return fmt.Errorf("unknown image pull policy %q", e.pullPolicy)
	}

	reader, err := e.docker.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull: %w", err)
	}

	defer func() { _ = reader.Close() }()

	// Drain the progress stream so the pull completes before create.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("drain image pull: %w", err)
	}

	return nil
}

// kill best-effort SIGKILLs a container by ID using a bounded detached context.
func (e *DockerExecutor) kill(containerID string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := e.docker.ContainerKill(ctx, containerID, "SIGKILL"); err != nil {
		log.Warn("failed to kill container", "container_id", truncateID(containerID), "error", err)
	}
}

// removeContainer force-removes a created-but-not-supervised container on a
// launch failure path, using a bounded detached context so a cancelled launch
// ctx cannot turn cleanup into a no-op.
func (e *DockerExecutor) removeContainer(_ context.Context, containerID string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		log.Warn("failed to remove container after launch failure",
			"container_id", truncateID(containerID), "error", err)
	}
}

func truncateID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}

// lineWriter is an io.Writer that splits its input on newlines and invokes emit
// once per complete line (without the trailing newline). The final unterminated
// line is delivered by Flush. Per-line length is bounded by scannerBufferMax.
type lineWriter struct {
	emit func(line []byte)
	buf  bytes.Buffer
}

func newLineWriter(emit func(line []byte)) *lineWriter {
	return &lineWriter{emit: emit}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)

	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			w.appendBounded(p)

			break
		}

		w.appendBounded(p[:idx])
		w.flushLine()

		p = p[idx+1:]
	}

	return n, nil
}

// appendBounded appends to the line buffer, dropping bytes past the cap so one
// runaway line cannot grow the buffer without bound.
func (w *lineWriter) appendBounded(p []byte) {
	if room := scannerBufferMax - w.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}

		w.buf.Write(p)
	}
}

func (w *lineWriter) flushLine() {
	line := make([]byte, w.buf.Len())
	copy(line, w.buf.Bytes())
	w.buf.Reset()

	line = bytes.TrimRight(line, "\r")
	w.emit(line)
}

// Flush emits any buffered partial line. Safe to call when the buffer is empty.
func (w *lineWriter) Flush() {
	if w.buf.Len() > 0 {
		w.flushLine()
	}
}
