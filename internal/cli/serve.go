package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-agent/internal/attempt"
	"github.com/mhersson/contextmatrix-agent/internal/callback"
	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/filelog"
	"github.com/mhersson/contextmatrix-agent/internal/metrics"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-agent/internal/webhook"
	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-backendkit/taskskills"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	"github.com/mhersson/contextmatrix-harness/redact"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const (
	// httpShutdownTimeout bounds the graceful HTTP drain after draining flips.
	httpShutdownTimeout = 10 * time.Second
	// callbackShutdownTimeout bounds each per-container kill + status callback
	// during shutdown so one slow ContextMatrix response cannot starve the rest.
	callbackShutdownTimeout = 10 * time.Second
	// onExitTimeout bounds the detached status callback fired when a container
	// exits. The supervision goroutine that calls it has no request context.
	onExitTimeout = 30 * time.Second

	// staticSecretsID is the reserved pseudo-session id under which
	// process-lifetime config-level secrets (MCP API key, agent API key) are
	// registered with the session secret registry. Nothing ever removes them,
	// so they survive every per-run add and remove.
	staticSecretsID = "__static__"
)

func newServeCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the agent task backend: host ContextMatrix lifecycle webhooks and launch worker containers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), configPath)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", defaultServeConfigPath(),
		"path to the service config file")

	return cmd
}

// defaultServeConfigPath resolves the XDG config path
// (~/.config/contextmatrix-agent/serve.yaml). A failure to resolve the user
// config dir falls back to the bare filename so LoadService still yields
// defaults+env.
func defaultServeConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "serve.yaml"
	}

	return filepath.Join(dir, "contextmatrix-agent", "serve.yaml")
}

func runServe(ctx context.Context, configPath string) error {
	cfg, err := config.LoadService(configPath)
	if err != nil {
		return fmt.Errorf("load service config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid service config: %w", err)
	}

	logger := newServeLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	mx := metrics.New()

	skillsCache := filepath.Join(cfg.SecretsDir, "task-skills-cache")
	skillsResolver := taskskills.NewResolver(cfg.ContextMatrixURL, cfg.APIKey, skillsCache, "/api/agent/task-skills-source")

	// Per-run credentials: every admitted trigger carries a CM-provisioned git
	// token; its credentials are staged into
	// <secrets_dir>/runs/<project>/<card_id>/env and refreshed from CM until
	// the run is torn down. There is no local credential source - a payload
	// without CM-provisioned credentials is fail-closed rejected by the
	// webhook launch guards.
	credentials := secrets.NewRunCredentials(cfg.SecretsDir, cfg.ContextMatrixURL, cfg.APIKey, logger)

	// Per-card container-output logs. Empty cfg.LogDir disables the feature; the
	// returned logger no-ops every call, so wiring below stays unconditional.
	files := filelog.New(cfg.LogDir, logger)

	docker, err := executor.NewClient()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}

	tracker := executor.NewTracker(cfg.MaxConcurrent)
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, dropAdapter{mx: mx})

	cbClient := callback.New(cfg.ContextMatrixURL, cfg.APIKey, logger).WithMetrics(mx)

	bridge := logbridge.NewBridge(logbridge.BridgeConfig{
		Hub:                  hub,
		Redactor:             nil, // set dynamically via RedactorRegistry below
		OnAwaiting:           func(k logbridge.Key, v bool) { tracker.SetAwaiting(k.Project, k.CardID, v) },
		SurfaceAwaitingHuman: true,
		MapExtra:             agentMapExtra,
	})

	// RedactorRegistry composes the bridge's redactor from all registered
	// per-session keys and atomically swaps it on every mutation. Register
	// the process-lifetime config-level secrets first under a reserved id
	// so they survive every per-run add and remove (the trap fix).
	//
	// registry tees every AddSessionKey/RemoveSessionKey call to the
	// backendkit RedactorRegistry above (unchanged - still drives the SSE
	// bridge) and to a second, file-log-only redactor built from the
	// identical key set, so the durable per-card log can mask the same
	// secrets on the full raw line - not just the derived Content field the
	// SSE bridge redacts. See sessionSecretTee.
	registry := newSessionSecretTee(logbridge.NewRedactorRegistry(bridge))
	registry.AddSessionKey(staticSecretsID, cfg.MCPAPIKey)
	registry.AddSessionKey(staticSecretsID, cfg.APIKey)

	// Wire the token-refresh hook so every rotated git token is added to the
	// redactor set. Appending is correct - both the original and the rotated
	// token can appear in output.
	credentials.OnTokenRefresh = onTokenRefreshHook(registry)

	exec := executor.NewDockerExecutor(executor.Config{
		Docker:           docker,
		Tracker:          tracker,
		PullPolicy:       cfg.ImagePullPolicy,
		ContainerTimeout: cfg.ContainerTimeout,
		IdleTimeout:      cfg.IdleOutputTimeout,
		PollInterval:     cfg.IdleWatchdogInterval,
		OnStart: func(project, cardID, containerID, correlationID string) {
			files.Begin(project, cardID, containerID, correlationID)
		},
		OnLog:   containerLogSink(bridge, files, registry),
		OnExit:  onContainerExit(cbClient, credentials, files, registry, bridge, logger),
		Logger:  logger,
		Metrics: mx,
	})

	// Force-remove any agent-labeled containers left by a previous process before
	// we start serving - a labeled container in a fresh process is an orphan.
	if err := exec.CleanupOrphans(ctx); err != nil {
		logger.Warn("orphan cleanup failed", "error", err)
	}

	// Likewise sweep leftover per-run secret files: a fresh process tracks no
	// runs, so any run dir on disk is stale secret material from a previous one.
	if err := credentials.CleanupOrphans(); err != nil {
		logger.Warn("per-run secrets cleanup failed", "error", err)
	}

	var draining atomic.Bool

	replay := webhookcore.NewReplayCache(cfg.ReplaySkew, cfg.ReplayCacheSize)
	dedup := webhook.NewDedupCache(cfg.MessageDedupTTL, cfg.MessageDedupCacheSize)

	srv := webhook.NewServer(webhook.Config{
		APIKey:           cfg.APIKey,
		MetricsToken:     cfg.MetricsToken,
		Skew:             cfg.ReplaySkew,
		MaxConcurrent:    cfg.MaxConcurrent,
		Executor:         exec,
		Tracker:          tracker,
		Hub:              hub,
		Reporter:         cbClient,
		Verifier:         cbClient,
		SkillsResolver:   skillsResolver,
		Credentials:      credentials,
		SessionSecrets:   registry,
		Images:           exec,
		ImageListFilters: cfg.ImageListFilters,
		LaunchEnv:        launchEnv(cfg, filepath.Join(cfg.SecretsDir, "shared")),
		NextAttempt:      files.NextAttempt,
		Replay:           replay,
		Dedup:            dedup,
		Draining:         &draining,
		Logger:           logger,
		Metrics:          mx,
	})

	// The replay janitor sweeps expired entries on the cache's own interval.
	stopJanitor := replay.StartJanitor()
	defer stopJanitor()

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Unblock in-flight /logs SSE streams when Shutdown starts; otherwise
	// http.Server.Shutdown waits the full httpShutdownTimeout on a stream that
	// never goes idle.
	httpServer.RegisterOnShutdown(srv.CloseSSE)

	adminSrv := buildAdminServer(cfg, srv, mx, logger)

	stopGauge := startRunningContainersGauge(tracker, mx, logger, 30*time.Second)
	defer stopGauge()

	serverErr := make(chan error, 1)

	go func() {
		logger.Info("agent service listening", "addr", httpServer.Addr)

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	if adminSrv != nil {
		go func() {
			logger.Info("admin server listening", "addr", adminSrv.Addr)

			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("admin server error", "error", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server error: %w", err)
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig.String())
	case <-ctx.Done():
		logger.Info("context cancelled, shutting down")
	}

	gracefulShutdown(httpServer, adminSrv, exec, tracker, cbClient, &draining, logger)
	cbClient.Close()
	logger.Info("agent service stopped")

	return nil
}

// gracefulShutdown drains the HTTP listener, kills every tracked worker, and
// reports each as failed to ContextMatrix. Structure:
//  1. flip draining so /readyz returns 503 and mutating routes refuse new work
//  2. Shutdown the HTTP server with a bounded budget
//  3. Shutdown the admin server if enabled
//  4. for each tracked run: Kill the container and report "failed"
func gracefulShutdown(
	httpServer *http.Server,
	adminServer *http.Server,
	exec executor.Executor,
	tracker *executor.Tracker,
	reporter webhook.StatusReporter,
	draining *atomic.Bool,
	logger *slog.Logger,
) {
	draining.Store(true)
	logger.Info("draining: readyz now returns 503, mutating routes refuse new work")

	httpCtx, httpCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer httpCancel()

	if err := httpServer.Shutdown(httpCtx); err != nil {
		logger.Error("http server shutdown error", "error", err)
	}

	if adminServer != nil {
		adminCtx, adminCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)

		if err := adminServer.Shutdown(adminCtx); err != nil {
			logger.Error("admin server shutdown error", "error", err)
		}

		adminCancel()
	}

	for _, run := range tracker.List() {
		logger.Info("killing container on shutdown", "project", run.Project, "card_id", run.CardID)

		// Kill's self-exit grace window applies here too, bounded by this
		// per-container killCtx (callbackShutdownTimeout).
		killCtx, killCancel := context.WithTimeout(context.Background(), callbackShutdownTimeout)
		if err := exec.Kill(killCtx, run.Project, run.CardID); err != nil &&
			!errors.Is(err, executor.ErrNotFound) {
			logger.Warn("failed to kill container on shutdown",
				"project", run.Project, "card_id", run.CardID, "error", err)
		}

		killCancel()

		cbCtx, cbCancel := context.WithTimeout(context.Background(), callbackShutdownTimeout)
		if err := reporter.ReportStatus(cbCtx, run.CardID, run.Project, "failed", "service shutting down"); err != nil {
			logger.Warn("failed to report shutdown status",
				"project", run.Project, "card_id", run.CardID, "error", err)
		}

		cbCancel()
	}
}

// onTokenRefreshHook builds the RunCredentials.OnTokenRefresh callback: it
// registers a freshly-minted git token with the session-secret registry
// under webhook.SessionID(project, cardID, correlationID) - the exact id
// addSessionSecrets registered the run's other secrets under (see
// internal/webhook/handler.go), so the rotated token joins its own run's
// redaction bucket rather than a different one. Extracted to a named
// function (rather than an inline closure in runServe) so it is directly
// unit-testable.
func onTokenRefreshHook(registry *sessionSecretTee) func(project, cardID, correlationID, token string) {
	return func(project, cardID, correlationID, token string) {
		registry.AddSessionKey(webhook.SessionID(project, cardID, correlationID), token)
	}
}

// sessionSecretTee tees every session-secret registration to backendkit's
// logbridge.RedactorRegistry (unchanged - still drives the SSE bridge's
// per-field redaction) and to a second, locally-held redactor built from the
// identical key set via the same redact.New primitive the registry uses
// internally. AddSessionKey/RemoveSessionKey forward the same arguments to
// both sides, so the active key set cannot drift between them - this is not
// a second, independently-tracked secret set, just a second reader of the
// one set of registrations. containerLogSink uses Redact to mask the durable
// per-card file log on the full raw line, catching secrets in JSON fields the
// SSE bridge's Content-only redaction never inspects.
//
// Follow-up (ledgered): once backendkit exposes the bridge's redactor (or a
// redact-and-return BridgeLine) in a tagged release, this tee can be deleted
// in favor of reusing that single result directly.
type sessionSecretTee struct {
	sse *logbridge.RedactorRegistry

	mu      sync.Mutex
	session map[string][]string
	file    atomic.Pointer[redact.Redactor]
}

// newSessionSecretTee wraps sse, an already-constructed RedactorRegistry, so
// callers keep wiring the SSE side exactly as before.
func newSessionSecretTee(sse *logbridge.RedactorRegistry) *sessionSecretTee {
	return &sessionSecretTee{sse: sse, session: make(map[string][]string)}
}

// AddSessionKey registers key under sessionID on both the SSE registry and
// the file-log redactor. An empty key is ignored on the file-log side too,
// matching RedactorRegistry's contract, so callers may register
// unconditionally.
func (t *sessionSecretTee) AddSessionKey(sessionID, key string) {
	t.sse.AddSessionKey(sessionID, key)

	if key == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.session[sessionID] = append(t.session[sessionID], key)
	t.rebuild()
}

// RemoveSessionKey forgets sessionID's secrets on both the SSE registry and
// the file-log redactor. Idempotent: removing an unregistered session is a
// no-op on the file-log side too.
func (t *sessionSecretTee) RemoveSessionKey(sessionID string) {
	t.sse.RemoveSessionKey(sessionID)

	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.session[sessionID]; !ok {
		return
	}

	delete(t.session, sessionID)
	t.rebuild()
}

// rebuild composes every registered key into a fresh redactor and swaps it
// into t.file, or clears t.file to nil when nothing is registered - Redact's
// short-circuit and the "nothing registered yet" startup state both key off
// that nil, a single atomic read with no separate flag to fall out of sync
// with it. The caller holds t.mu.
func (t *sessionSecretTee) rebuild() {
	all := make([]string, 0, len(t.session))
	for _, keys := range t.session {
		all = append(all, keys...)
	}

	if len(all) == 0 {
		t.file.Store(nil)

		return
	}

	t.file.Store(redact.New(all))
}

// Redact masks every currently-registered secret in line. Nothing registered
// yet (t.file is nil) skips the copy through string(line)/Apply/[]byte(...)
// entirely and returns line as-is - the common case is every line taking that
// round trip for zero matches, since most output carries no secret.
func (t *sessionSecretTee) Redact(line []byte) []byte {
	r := t.file.Load()
	if r == nil {
		return line
	}

	return []byte(r.Apply(string(line)))
}

// containerLogSink returns the executor OnLog callback: it fans out one
// worker output line to the live SSE bridge and the durable per-card file
// log. The file log receives the line masked by registry's tee redactor -
// the identical secret set the SSE bridge redacts from - applied to the full
// raw line for both the stdout and stderr arms, so a CM-provisioned secret
// never reaches disk in plaintext even in JSON fields the SSE bridge's
// per-field redaction does not inspect.
func containerLogSink(
	bridge *logbridge.Bridge,
	files *filelog.Logger,
	registry *sessionSecretTee,
) func(project, cardID, correlationID string, line []byte, stderr bool) {
	return func(project, cardID, correlationID string, line []byte, stderr bool) {
		// Bridge to the live /logs SSE stream first so the interactive
		// stream is never gated on the durable-log disk write. BridgeLine
		// does not mutate line and Write copies it, so the order is safe.
		bridge.BridgeLine(logbridge.Key{Project: project, CardID: cardID}, line, stderr)
		files.Write(project, cardID, correlationID, registry.Redact(line), stderr)
	}
}

// onContainerExit builds the executor OnExit hook: it tears down the run's
// per-run credentials (stop the refresh loop, remove the run dir), maps the
// container exit code to a worker-status, and reports it to ContextMatrix on a
// bounded detached context (the supervision goroutine carries no request ctx).
// waitAndCleanup is the single funnel every container exits through, so this is
// the teardown seam for the per-run refresh loop.
//
// Ordering invariant: the terminal run_end event is written BEFORE files.End,
// because files.End closes this run's log writer and a Write after it is a
// no-op - the event would reach the live stream and never the durable
// transcript.
//
// Ordering invariant: both files.End and this exit-path Teardown run BEFORE the
// exit status callback below, and that callback is what gates CM's re-triggers
// (CM learns the run finished only from it). files.End footers and closes this
// run's own writer first - keyed by the run's correlation id, so it can only
// ever close the writer this run's Begin opened, never a re-trigger's - before
// the status callback can let CM admit a new run for the same card.
//
// The tracker.Remove -> this callback window is NOT negligible: waitAndCleanup
// (internal/executor/docker.go) clears the tracker entry BEFORE the pump-drain
// wait (up to pumpDrainTimeout, 5s), and this callback - which runs Teardown
// and the session-secret removal below - fires only once that wait completes.
// Nothing but the tracker entry gates admission, so CM can and does re-trigger
// inside that window; a re-trigger racing in here is the normal case this file
// defends against, not an unreachable edge case. files.End shares the same
// window, and the same defense: the durable log's open writers are keyed by
// the run's correlation id (the same triple webhook.SessionID keys redaction
// sessions by), so this stale run's End is a no-op against a re-trigger's
// already-open writer.
//
// credentials.Teardown stays keyed by plain project/cardID: RunCredentials
// tracks one live handle per card, and Provision's own re-provision-displaces
// design (see its doc comment) means a re-trigger's Provision landing inside
// this window already replaces the stale run's handle with its own before this
// stale Teardown call runs. Teardown then finds and stops the RE-TRIGGER's
// handle, not the original run's - at worst the re-trigger loses its own
// freshly-provisioned credential directory to this call, a loud, self-inflicted
// failure (the new run fails fast on a missing secrets file), never a leaked or
// cross-run token. That narrower risk is unchanged by this task; only the
// session-secret registry removal below is now scoped to avoid the equivalent
// cross-run failure mode.
//
// Session-secret removal keying: the registry removal below uses
// webhook.SessionID(project, cardID, correlationID) - the run's own
// correlation id, forwarded here from the executor's OnExit callback - so it
// does NOT share credentials.Teardown's exposure above. Before this fix, both
// this call and addSessionSecrets composed the bare project/cardID id, so a
// stale run's removal here would strip a re-trigger's still-live redaction
// keys during the same window. Keying by correlationID makes the two calls
// target different map entries, so a stale exit can only ever remove its own
// run's keys.
func onContainerExit(
	reporter webhook.StatusReporter,
	credentials *secrets.RunCredentials,
	files *filelog.Logger,
	registry *sessionSecretTee,
	bridge *logbridge.Bridge,
	logger *slog.Logger,
) func(project, cardID string, exitCode int64, cause executor.ExitCause, ordinal int, correlationID string) {
	return func(project, cardID string, exitCode int64, cause executor.ExitCause, ordinal int, correlationID string) {
		emitRunEnd(bridge, files, project, cardID, correlationID, exitCode, cause, ordinal, logger)

		files.End(project, cardID, correlationID, exitCode, string(cause))
		credentials.Teardown(project, cardID)

		// Remove the session secrets after credential teardown but before
		// the status callback so a re-trigger does not inherit stale keys.
		registry.RemoveSessionKey(webhook.SessionID(project, cardID, correlationID))

		status, message := exitStatus(exitCode, cause)

		ctx, cancel := context.WithTimeout(context.Background(), onExitTimeout)
		defer cancel()

		if err := reporter.ReportStatus(ctx, cardID, project, status, message); err != nil {
			logger.Error("report exit status callback failed",
				"project", project, "card_id", cardID, "status", status, "error", err)
		}
	}
}

// runEndKind is the transcript event kind the host writes when a container run
// ends. Every other event on the stream comes from inside the container, so a
// run that dies leaves output that simply stops mid-sentence; this line is what
// says a run ended and how. It goes to both surfaces the container's own output
// goes to - the live SSE stream and the durable per-card transcript - because
// the two have different readers.
const runEndKind = "run_end"

// emitRunEnd writes the terminal event to the live stream and the durable
// transcript. It is best-effort throughout: it returns nothing and never
// short-circuits, so a failure to describe the exit cannot change the exit
// path or mask the cause the caller is reporting.
func emitRunEnd(
	bridge *logbridge.Bridge,
	files *filelog.Logger,
	project, cardID, correlationID string,
	exitCode int64,
	cause executor.ExitCause,
	ordinal int,
	logger *slog.Logger,
) {
	line, err := json.Marshal(runEndEvent(exitCode, cause, ordinal))
	if err != nil {
		logger.Warn("could not build the run_end event",
			"project", project, "card_id", cardID, "error", err)

		return
	}

	// Same order as the output tee: the live stream is never gated on a disk
	// write, BridgeLine does not mutate line and Write copies it.
	bridge.BridgeLine(logbridge.Key{Project: project, CardID: cardID}, line, false)
	files.Write(project, cardID, correlationID, line, false)
}

// runEndEvent builds the terminal line in the envelope the worker's own events
// use, so a transcript reader parses it exactly like every other line.
//
// It carries no seq: the sequence counter belongs to the container, and this
// line is written after the container is gone. The harness envelope declares
// seq without omitempty, so a consumer decoding into that struct reads zero
// here and must take this line in file order rather than sort by sequence,
// which would file the end of the run at its start.
//
// The attempt ordinal is stamped only past the first run, matching what the
// container does with its own lines, where an absent ordinal already reads as
// attempt 1. Without it a restarted card's terminal event would be filed under
// the run it replaced.
func runEndEvent(exitCode int64, cause executor.ExitCause, ordinal int) map[string]any {
	ev := map[string]any{
		"kind": runEndKind,
		"time": time.Now().UTC(),
		"data": map[string]any{
			"exit_code": exitCode,
			"cause":     string(cause),
		},
	}

	if ordinal > 1 {
		ev[attempt.Field] = ordinal
	}

	return ev
}

// exitStatus maps a container exit code and the way the run ended to a
// ContextMatrix worker-status and a human-readable message. Exit 0 is
// "completed" only when the cause is ExitNormal, the one case where the
// container ended on its own terms and the code carries its usual meaning; any
// other cause arriving with a zero code means the code is not evidence of
// success, so it is "failed" too. Any non-zero code is "failed", with the code
// carried in the message for the operator.
//
// A daemon-flagged wait is called out separately. The status code that arrives
// with a daemon error comes from the same response the daemon could not
// complete, so it is not a run outcome and must not be read as one,
// whatever the value. The message names the daemon rather than leaving the
// operator to guess.
//
// For the other zero-code cases the message says the exit status is unknown
// because the run ended by a kill or a timeout rather than on its own terms -
// it does not quote the meaningless zero or claim a specific cause it cannot
// name.
//
// `failed` is not certainly accurate here either - the work may well have
// succeeded - but it is honest about what is known, and it fails in the safe
// direction: a run recorded as failed gets looked at, while one recorded as a
// clean finish does not.
func exitStatus(exitCode int64, cause executor.ExitCause) (status, message string) {
	if cause == executor.ExitDaemonError {
		return "failed", "worker exit status unknown: the container wait returned a daemon error"
	}

	if exitCode == 0 && cause != executor.ExitNormal {
		return "failed", "worker exit status unknown: the run ended by a kill or a timeout rather than on its own terms"
	}

	if exitCode == 0 {
		return "completed", ""
	}

	return "failed", fmt.Sprintf("worker exited with code %d", exitCode)
}

// launchEnv assembles the static per-process LaunchEnv folded into each
// container. The MCP URL base seen from containers is the container-specific
// override when set, else the public ContextMatrix URL; "/mcp" is appended to
// form the full endpoint the worker's CM_MCP_URL expects.
func launchEnv(cfg *config.ServiceConfig, secretsHostDir string) webhook.LaunchEnv {
	base := cfg.ContainerContextMatrixURL
	if base == "" {
		base = cfg.ContextMatrixURL
	}

	return webhook.LaunchEnv{
		BaseImage:                 cfg.BaseImage,
		MCPURL:                    composeMCPURL(base),
		MCPAPIKey:                 cfg.MCPAPIKey,
		SecretsHostDir:            secretsHostDir,
		CACertFile:                cfg.CACertFile,
		MemoryBytes:               cfg.ContainerMemoryBytes,
		PidsLimit:                 cfg.ContainerPidsLimit,
		ContainerTimeoutSeconds:   int(cfg.ContainerTimeout.Seconds()),
		BashTimeoutMaxSeconds:     cfg.BashTimeoutMaxSeconds,
		ToolOutputMaxBytes:        cfg.ToolOutputMaxBytes,
		DefaultModel:              cfg.DefaultModel,
		ReasoningEffort:           cfg.ReasoningEffort,
		MaxCardCost:               cfg.MaxCardCost,
		SelectorPriceHeadroom:     cfg.SelectorPriceHeadroom,
		SelectorTierBars:          cfg.SelectorTierBars,
		ReviewAttemptsCap:         cfg.ReviewAttemptsCap,
		CompactionEnabled:         cfg.Compaction.Enabled,
		CompactionThreshold:       cfg.Compaction.Threshold,
		CompactionKeepRecentTurns: cfg.Compaction.KeepRecentTurns,
		WorkerExtraEnv:            flattenEnv(cfg.WorkerExtraEnv),
	}
}

// composeMCPURL builds the full MCP endpoint URL the worker connects to:
// <base>/mcp, with any trailing slash on the base trimmed so we never emit a
// double slash.
func composeMCPURL(base string) string {
	return strings.TrimRight(base, "/") + "/mcp"
}

// flattenEnv renders a KEY:VALUE map into the KEY=VALUE slice the container
// environment expects. Order is unspecified; the worker reads by key.
func flattenEnv(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}

	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}

	return out
}

// newServeLogger builds a JSON slog logger at the level named by lvl
// (debug|info|warn|error; default info on an empty or unrecognised value).
func newServeLogger(lvl string) *slog.Logger {
	var level slog.Level

	switch strings.ToLower(lvl) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// dropAdapter bridges logbridge.DropObserver to the Prometheus broadcaster-drops
// counter without forcing logbridge to import Prometheus.
type dropAdapter struct{ mx *metrics.Metrics }

func (a dropAdapter) ObserveDrop() {
	if a.mx == nil {
		return
	}

	a.mx.BroadcasterDropsTotal.Inc()
}

// agentMapExtra is the agent's MapExtra hook for the log bridge: the arms for
// the event kinds the agent emits that the shared bridge does not know. A kind
// no arm claims returns ok=false so it falls through to the shared default
// skip - seat sub-run events ("seat_debug") included, keeping them off the live
// stream by construction.
func agentMapExtra(kind string, data map[string]any) (protocol.LogEntry, bool, bool) {
	switch kind {
	case "discussion":
		return discussionMapExtra(data)
	case "gate_progress":
		return gateProgressMapExtra(data)
	case runEndKind:
		return runEndMapExtra(data)
	default:
		return protocol.LogEntry{}, false, false
	}
}

// discussionMapExtra maps a mob-session discussion event to a speaker-labeled
// text entry carrying the briefing, round utterances, moderator notices, or
// synthesis.
func discussionMapExtra(data map[string]any) (protocol.LogEntry, bool, bool) {
	return protocol.LogEntry{
		Type:    "text",
		Content: mapStr(data, "content"),
		Agent:   mapStr(data, "agent"),
		Model:   mapStr(data, "model"),
	}, false, true
}

// gateProgressMapExtra maps one pr_gates poll to a system entry.
//
// A poll marked repeat=true is dropped - returned unclaimed, which the bridge's
// default arm turns into a skip. The gate emits on EVERY poll so the
// serve-side idle watchdog keeps seeing output while it waits, but a status
// identical to the one already on screen is not worth a transcript row: a
// gate can sit on the same counts for many minutes, and printing them each
// time buries the polls that actually moved. The dropped polls are still in
// the durable run log, which records the worker's raw output.
func gateProgressMapExtra(data map[string]any) (protocol.LogEntry, bool, bool) {
	if repeat, _ := data["repeat"].(bool); repeat {
		return protocol.LogEntry{}, false, false
	}

	return protocol.LogEntry{
		Type:    "system",
		Content: mapStr(data, "status"),
	}, false, true
}

// runEndMapExtra maps the host's terminal event to a system entry, so a live
// stream that would otherwise stop mid-sentence closes with a line naming the
// exit code and the cause. Without an arm here the shared bridge's default skip
// drops the kind and the stream still ends in silence.
func runEndMapExtra(data map[string]any) (protocol.LogEntry, bool, bool) {
	return protocol.LogEntry{
		Type: "system",
		Content: fmt.Sprintf("run ended: exit=%d cause=%s",
			mapInt64(data, "exit_code"), mapStr(data, "cause")),
	}, false, true
}

// mapInt64: JSON numbers decode as float64, which is the shape the bridge
// hands over.
func mapInt64(data map[string]any, key string) int64 {
	switch n := data[key].(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	default:
		return 0
	}
}

// mapStr reads a string field out of an event's data payload, returning "" when
// it is absent or not a string.
func mapStr(data map[string]any, key string) string {
	s, _ := data[key].(string)

	return s
}

// buildAdminServer returns the admin HTTP server serving Prometheus /metrics
// behind HMAC (plus an optional static bearer token), or nil when admin_port
// is 0. Binds to admin_bind_addr, loopback by default; a non-loopback bind is
// allowed for external scrapers and logged as a warning.
func buildAdminServer(
	cfg *config.ServiceConfig,
	srv *webhook.Server,
	mx *metrics.Metrics,
	logger *slog.Logger,
) *http.Server {
	if cfg.AdminPort == 0 {
		logger.Info("admin endpoints disabled (admin_port=0)")

		return nil
	}

	bind := cfg.AdminBindAddr
	if bind == "" {
		bind = "127.0.0.1"
	}

	if bind != "127.0.0.1" && bind != "localhost" && bind != "::1" {
		logger.Warn("admin server bound to non-loopback address - metrics exposed; restrict via firewall",
			"addr", bind, "port", cfg.AdminPort)
	}

	mux := http.NewServeMux()
	metricsHandler := promhttp.HandlerFor(mx.Registry, promhttp.HandlerOpts{})
	mux.HandleFunc("GET /metrics", srv.AdminAuth(metricsHandler.ServeHTTP))

	metricsAuth := "hmac"
	if cfg.MetricsToken != "" {
		metricsAuth = "hmac+bearer"
	}

	logger.Info("admin endpoints registered", "port", cfg.AdminPort, "metrics_auth", metricsAuth)

	return &http.Server{
		Addr:              net.JoinHostPort(bind, strconv.Itoa(cfg.AdminPort)),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// startRunningContainersGauge polls tracker.Count() on a ticker and publishes
// it to the running-containers gauge. Returns an idempotent stop function. A
// non-positive interval disables the poller.
func startRunningContainersGauge(
	tracker *executor.Tracker,
	mx *metrics.Metrics,
	logger *slog.Logger,
	interval time.Duration,
) func() {
	if interval <= 0 {
		logger.Warn("running-containers gauge disabled: non-positive interval", "interval", interval)

		return func() {}
	}

	stop := make(chan struct{})

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				mx.RunningContainers.Set(float64(tracker.Count()))
			}
		}
	}()

	var once sync.Once

	return func() { once.Do(func() { close(stop) }) }
}
