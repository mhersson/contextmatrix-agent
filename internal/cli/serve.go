package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	githubauth "github.com/mhersson/contextmatrix-githubauth"
	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-agent/internal/callback"
	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/logbridge"
	"github.com/mhersson/contextmatrix-agent/internal/redact"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-agent/internal/webhook"
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

	provider, err := newTokenProvider(cfg.GitHub)
	if err != nil {
		return err
	}

	logger.Info("github token provider initialized", "auth_mode", cfg.GitHub.AuthMode)

	// Secrets refresher: writes <secrets_dir>/shared/env, rewritten ahead of each
	// token expiry. The worker reads /run/cm-secrets/env, which is <shared> bound
	// read-only into the container.
	sharedDir := filepath.Join(cfg.SecretsDir, "shared")
	envFile := filepath.Join(sharedDir, "env")
	refresher := secrets.NewRefresher(envFile, cfg.OpenRouterAPIKey, provider, logger)

	refreshCtx, refreshCancel := context.WithCancel(context.Background())
	defer refreshCancel()

	go func() {
		if err := refresher.Run(refreshCtx); err != nil {
			logger.Error("secrets refresher stopped with error", "error", err)
		}
	}()

	docker, err := executor.NewClient()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}

	tracker := executor.NewTracker(cfg.MaxConcurrent)
	hub := logbridge.NewHub()
	redactor := redact.New([]string{cfg.OpenRouterAPIKey, cfg.MCPAPIKey, cfg.APIKey, cfg.ArtificialAnalysisAPIKey})

	cbClient := callback.New(cfg.ContextMatrixURL, cfg.APIKey, logger)

	bridge := logbridge.New(hub, redactor, tracker.SetAwaiting)

	exec := executor.NewDockerExecutor(executor.Config{
		Docker:           docker,
		Tracker:          tracker,
		PullPolicy:       cfg.ImagePullPolicy,
		ContainerTimeout: cfg.ContainerTimeout,
		IdleTimeout:      cfg.IdleOutputTimeout,
		PollInterval:     cfg.IdleWatchdogInterval,
		OnLog:            bridge.BridgeLine,
		OnExit:           onContainerExit(cbClient, logger),
		Logger:           logger,
	})

	// Force-remove any agent-labeled containers left by a previous process before
	// we start serving — a labeled container in a fresh process is an orphan.
	if err := exec.CleanupOrphans(ctx); err != nil {
		logger.Warn("orphan cleanup failed", "error", err)
	}

	var draining atomic.Bool

	replay := webhook.NewReplayCache(cfg.ReplaySkew, cfg.ReplayCacheSize)
	dedup := webhook.NewDedupCache(cfg.MessageDedupTTL, cfg.MessageDedupCacheSize)

	srv := webhook.NewServer(webhook.Config{
		APIKey:        cfg.APIKey,
		Skew:          cfg.ReplaySkew,
		MaxConcurrent: cfg.MaxConcurrent,
		Executor:      exec,
		Tracker:       tracker,
		Hub:           hub,
		Reporter:      cbClient,
		Verifier:      cbClient,
		LaunchEnv:     launchEnv(cfg, sharedDir),
		Replay:        replay,
		Dedup:         dedup,
		Draining:      &draining,
		Logger:        logger,
	})

	// The replay janitor sweeps expired entries on the cache's own interval.
	stopJanitor := replay.StartJanitor()
	defer stopJanitor()

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)

	go func() {
		logger.Info("agent service listening", "addr", httpServer.Addr)

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

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

	gracefulShutdown(httpServer, exec, tracker, cbClient, &draining, logger)
	refreshCancel()
	logger.Info("agent service stopped")

	return nil
}

// gracefulShutdown drains the HTTP listener, kills every tracked worker, and
// reports each as failed to ContextMatrix. Mirrors the runner's structure:
//  1. flip draining so /readyz returns 503 and mutating routes refuse new work
//  2. Shutdown the HTTP server with a bounded budget
//  3. for each tracked run: Kill the container and report "failed"
func gracefulShutdown(
	httpServer *http.Server,
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

	for _, run := range tracker.List() {
		logger.Info("killing container on shutdown", "project", run.Project, "card_id", run.CardID)

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

// onContainerExit builds the executor OnExit hook: it maps the container exit
// code to a runner-status and reports it to ContextMatrix on a bounded detached
// context (the supervision goroutine carries no request ctx).
func onContainerExit(reporter webhook.StatusReporter, logger *slog.Logger) func(project, cardID string, exitCode int64) {
	return func(project, cardID string, exitCode int64) {
		status, message := exitStatus(exitCode)

		ctx, cancel := context.WithTimeout(context.Background(), onExitTimeout)
		defer cancel()

		if err := reporter.ReportStatus(ctx, cardID, project, status, message); err != nil {
			logger.Error("report exit status callback failed",
				"project", project, "card_id", cardID, "status", status, "error", err)
		}
	}
}

// exitStatus maps a container exit code to a ContextMatrix runner-status and a
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
		BaseImage:             cfg.BaseImage,
		MCPURL:                composeMCPURL(base),
		MCPAPIKey:             cfg.MCPAPIKey,
		SecretsHostDir:        secretsHostDir,
		MemoryBytes:           cfg.ContainerMemoryBytes,
		PidsLimit:             cfg.ContainerPidsLimit,
		BashTimeoutMaxSeconds: cfg.BashTimeoutMaxSeconds,
		ToolOutputMaxBytes:    cfg.ToolOutputMaxBytes,
		DefaultModel:          cfg.DefaultModel,
		MaxCardCost:           cfg.MaxCardCost,
		SelectorPriceHeadroom: cfg.SelectorPriceHeadroom,
		WorkerExtraEnv:        flattenEnv(cfg.WorkerExtraEnv),
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

// newTokenProvider selects the GitHub token provider per auth_mode, mirroring
// the runner: app -> NewAppProvider, pat -> NewPATProvider. No caching wrapper;
// the refresher mints ahead of expiry and the worker needs freshness at
// hand-off. Validate() has already rejected unknown modes, but this defends in
// depth.
func newTokenProvider(gh config.GitHubConfig) (secrets.TokenGenerator, error) {
	switch gh.AuthMode {
	case "app":
		p, err := githubauth.NewAppProvider(
			gh.App.AppID,
			gh.App.InstallationID,
			gh.App.PrivateKeyPath,
			githubauth.WithAPIBaseURL(gh.APIBaseURL),
		)
		if err != nil {
			return nil, fmt.Errorf("construct github app provider: %w", err)
		}

		return p, nil
	case "pat":
		p, err := githubauth.NewPATProvider(gh.PAT.Token)
		if err != nil {
			return nil, fmt.Errorf("construct github pat provider: %w", err)
		}

		return p, nil
	default:
		return nil, fmt.Errorf("unknown github auth_mode %q", gh.AuthMode)
	}
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
