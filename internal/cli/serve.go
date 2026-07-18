package cli

import (
	"context"
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
	redactor := redact.New([]string{cfg.MCPAPIKey, cfg.APIKey})

	cbClient := callback.New(cfg.ContextMatrixURL, cfg.APIKey, logger).WithMetrics(mx)

	bridge := logbridge.NewBridge(logbridge.BridgeConfig{
		Hub:                  hub,
		Redactor:             redactor,
		OnAwaiting:           func(k logbridge.Key, v bool) { tracker.SetAwaiting(k.Project, k.CardID, v) },
		SurfaceAwaitingHuman: true,
		MapExtra:             discussionMapExtra,
	})

	exec := executor.NewDockerExecutor(executor.Config{
		Docker:           docker,
		Tracker:          tracker,
		PullPolicy:       cfg.ImagePullPolicy,
		ContainerTimeout: cfg.ContainerTimeout,
		IdleTimeout:      cfg.IdleOutputTimeout,
		PollInterval:     cfg.IdleWatchdogInterval,
		OnStart:          files.Begin,
		OnLog: func(project, cardID string, line []byte, stderr bool) {
			// Bridge to the live /logs SSE stream first so the interactive
			// stream is never gated on the durable-log disk write. BridgeLine
			// does not mutate line and Write copies it, so the order is safe.
			bridge.BridgeLine(logbridge.Key{Project: project, CardID: cardID}, line, stderr)
			files.Write(project, cardID, line, stderr)
		},
		OnExit:  onContainerExit(cbClient, credentials, files, logger),
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
		Images:           exec,
		ImageListFilters: cfg.ImageListFilters,
		LaunchEnv:        launchEnv(cfg, filepath.Join(cfg.SecretsDir, "shared")),
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

// onContainerExit builds the executor OnExit hook: it tears down the run's
// per-run credentials (stop the refresh loop, remove the run dir), maps the
// container exit code to a worker-status, and reports it to ContextMatrix on a
// bounded detached context (the supervision goroutine carries no request ctx).
// waitAndCleanup is the single funnel every container exits through, so this is
// the teardown seam for the per-run refresh loop.
//
// Ordering invariant: both files.End and this exit-path Teardown run BEFORE the
// exit status callback below, and that callback is what gates CM's re-triggers
// (CM learns the run finished only from it). files.End footers and closes the
// per-card log first, so the log is closed before the status callback can let CM
// admit a new run for the same card. Likewise for Teardown: under the normal
// flow the tracker.Remove → Teardown window is already closed before CM can
// re-trigger, and it cannot be hit. A re-trigger racing in out of band inside
// that microsecond window would at worst lose its own fresh provisioning to this
// Teardown - a loud, self-inflicted failure - never a leaked or cross-run token.
func onContainerExit(
	reporter webhook.StatusReporter,
	credentials *secrets.RunCredentials,
	files *filelog.Logger,
	logger *slog.Logger,
) func(project, cardID string, exitCode int64) {
	return func(project, cardID string, exitCode int64) {
		files.End(project, cardID, exitCode)
		credentials.Teardown(project, cardID)

		status, message := exitStatus(exitCode)

		ctx, cancel := context.WithTimeout(context.Background(), onExitTimeout)
		defer cancel()

		if err := reporter.ReportStatus(ctx, cardID, project, status, message); err != nil {
			logger.Error("report exit status callback failed",
				"project", project, "card_id", cardID, "status", status, "error", err)
		}
	}
}

// exitStatus maps a container exit code to a ContextMatrix worker-status and a
// human-readable message. Exit 0 is "completed"; anything else is "failed",
// with the code carried in the message for the operator.
func exitStatus(exitCode int64) (status, message string) {
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
		BashTimeoutMaxSeconds:     cfg.BashTimeoutMaxSeconds,
		ToolOutputMaxBytes:        cfg.ToolOutputMaxBytes,
		DefaultModel:              cfg.DefaultModel,
		ReasoningEffort:           cfg.ReasoningEffort,
		MaxCardCost:               cfg.MaxCardCost,
		SelectorPriceHeadroom:     cfg.SelectorPriceHeadroom,
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

// discussionMapExtra is the agent's mob-session "discussion" arm, supplied to
// the log bridge as its MapExtra hook: it maps a discussion event to a
// speaker-labeled text entry carrying the briefing, round utterances, moderator
// notices, or synthesis. Non-discussion kinds return ok=false so they fall
// through to the shared default skip - seat sub-run events ("seat_debug")
// included, keeping them off the live stream by construction.
func discussionMapExtra(kind string, data map[string]any) (protocol.LogEntry, bool, bool) {
	if kind != "discussion" {
		return protocol.LogEntry{}, false, false
	}

	str := func(k string) string {
		s, _ := data[k].(string)

		return s
	}

	return protocol.LogEntry{
		Type:    "text",
		Content: str("content"),
		Agent:   str("agent"),
		Model:   str("model"),
	}, false, true
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
