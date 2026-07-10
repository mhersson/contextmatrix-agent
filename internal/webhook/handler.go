package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/frames"
	"github.com/mhersson/contextmatrix-agent/internal/logbridge"
	"github.com/mhersson/contextmatrix-agent/internal/metrics"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const (
	// maxRequestBodyBytes caps the body the auth middleware reads before HMAC
	// verification. ContextMatrix caps /message content well under this; a
	// larger body is a misbehaving or hostile client.
	maxRequestBodyBytes = 1 << 20 // 1 MiB

	// correlationHeader carries the client's trace ID. The agent backend reads
	// it on /trigger and threads it into executor.LaunchSpec.CorrelationID (the
	// container label) so runner and worker logs stitch to the same CM trace.
	correlationHeader = "X-Correlation-ID"

	// skillsMountPathEnv is the in-container path the executor mounts the skills
	// dir at (must match executor.skillsMountPath). Passed to the worker as
	// CMX_TASK_SKILLS_DIR.
	skillsMountPathEnv = "/run/cm-skills"
)

// StatusReporter reports a task's runner-status transition back to
// ContextMatrix. *callback.Client satisfies it; tests supply a fake.
type StatusReporter interface {
	ReportStatus(ctx context.Context, cardID, project, status, message string) error
}

// AutonomousVerifier confirms a card's autonomous flag before /promote writes
// the control frame — fail closed. *callback.Client satisfies it; tests supply
// a fake.
type AutonomousVerifier interface {
	VerifyAutonomous(ctx context.Context, project, cardID string) (bool, error)
}

// SkillsResolver resolves the host directory holding the task-skills the worker
// container mounts. *taskskills.Resolver satisfies it; tests supply a fake.
// Resolve is best-effort: a non-nil error means "no skills this run".
type SkillsResolver interface {
	Resolve(ctx context.Context) (hostDir string, err error)
}

// CredentialProvisioner stages a per-run credential file (the payload git token
// plus LLM endpoint values) and refreshes the git token from ContextMatrix until
// the run is torn down. *secrets.RunCredentials satisfies it; tests supply a
// fake. Nil disables per-run provisioning — the run then mounts the shared
// secrets file (the fallback path).
type CredentialProvisioner interface {
	// HostDir is the per-run directory bind-mounted read-only at /run/cm-secrets.
	// Keyed by (project, cardID) so the same card ID across projects stays isolated.
	HostDir(project, cardID string) string
	// Provision writes the initial per-run env file and starts the refresh loop
	// (when the token carries an expiry). Synchronous initial write; returns its
	// error.
	Provision(project, cardID, token, expiresAt string, endpoint secrets.EndpointSecrets) error
	// Teardown stops the refresh loop and removes the run directory. Idempotent.
	Teardown(project, cardID string)
}

// LaunchEnv carries the static, per-process inputs the handler folds together
// with each TriggerPayload to compose an executor.LaunchSpec. Per-request
// fields (card ID, project, repo URL, base branch, model, interactive,
// correlation ID, image override) come from the payload; everything here is
// fixed at serve time.
type LaunchEnv struct {
	// BaseImage is the worker image launched when the payload does not override
	// it via runner_image.
	BaseImage string

	// MCPURL and MCPAPIKey wire the worker's MCP client back to ContextMatrix.
	// MCPAPIKey is the fallback when the payload omits mcp_api_key.
	MCPURL    string
	MCPAPIKey string

	// SecretsHostDir is the host directory bind-mounted read-only into each
	// container at /run/cm-secrets. Empty disables the mount. Used when a run has
	// no CM-provisioned git token (the shared, process-wide secrets file); a run
	// with a payload token mounts its per-run directory instead.
	SecretsHostDir string

	// SharedSecretsAvailable is true only when the shared, process-wide secrets
	// refresher is running — i.e. local github config is present, so the
	// SecretsHostDir mount actually carries a git token. False when github is
	// unconfigured (CM-provisioned credentials are the only source): a trigger
	// with no payload git token then has no fallback at all, and
	// admitAndLaunch fail-closes it rather than launching a container with a
	// dud mount.
	SharedSecretsAvailable bool

	// DefaultLLMEndpoint is the local-config LLM endpoint staged into a per-run
	// credential file when the trigger payload omits its own llm_endpoint (the
	// compat-window fallback). The shared secrets file already carries these
	// values for the no-token path.
	DefaultLLMEndpoint secrets.EndpointSecrets

	// CACertFile is the host path to an optional extra-CA PEM, bind-mounted
	// read-only into each container at /run/cm-ca/ca.crt. Empty disables it.
	CACertFile string

	// MemoryBytes and PidsLimit are the per-container resource caps.
	MemoryBytes int64
	PidsLimit   int64

	// BashTimeoutMaxSeconds, ToolOutputMaxBytes, DefaultModel, and
	// ReasoningEffort are the CMX_* worker knobs. Zero/empty values are omitted
	// so the worker applies its own defaults.
	BashTimeoutMaxSeconds int
	ToolOutputMaxBytes    int
	DefaultModel          string
	ReasoningEffort       string

	// MaxCardCost is the cumulative USD ceiling per card passed as
	// CMX_MAX_CARD_COST. Zero is omitted (worker applies its own default).
	MaxCardCost float64

	// SelectorPriceHeadroom is the best-value band multiplier passed as
	// CMX_SELECTOR_PRICE_HEADROOM. Zero is omitted (worker applies its own default).
	SelectorPriceHeadroom float64

	// CompactionEnabled, CompactionThreshold, and CompactionKeepRecentTurns
	// configure the worker harness loop's in-window compaction. When disabled
	// (the default) the CMX_COMPACTION_* vars are omitted so the worker keeps the
	// hard context_limit stop.
	CompactionEnabled         bool
	CompactionThreshold       float64
	CompactionKeepRecentTurns int

	// WorkerExtraEnv is appended verbatim to every container's environment
	// (KEY=VALUE strings). Used for operator-supplied passthrough.
	WorkerExtraEnv []string
}

// Config carries the dependencies NewServer needs. Pointers may be shared with
// the serve layer; the server does not take ownership of their lifecycles.
type Config struct {
	APIKey        string
	Skew          time.Duration
	MaxConcurrent int

	Executor       executor.Executor
	Tracker        *executor.Tracker
	Hub            *logbridge.Hub
	Reporter       StatusReporter
	Verifier       AutonomousVerifier
	SkillsResolver SkillsResolver
	Credentials    CredentialProvisioner

	LaunchEnv LaunchEnv

	Replay *ReplayCache
	Dedup  *DedupCache

	Draining *atomic.Bool

	// KeepaliveInterval overrides the SSE heartbeat period. Zero uses the
	// package default; tests shrink it.
	KeepaliveInterval time.Duration

	// Metrics is the Prometheus bundle. Nil disables request instrumentation.
	Metrics *metrics.Metrics

	Logger *slog.Logger
}

// Server is the agent backend's HTTP surface. It owns no goroutines beyond the
// per-trigger launch goroutines it spawns; the replay janitor and the executor
// supervision live in their respective owners.
type Server struct {
	apiKey        string
	skew          time.Duration
	maxConcurrent int

	executor       executor.Executor
	tracker        *executor.Tracker
	hub            *logbridge.Hub
	reporter       StatusReporter
	verifier       AutonomousVerifier
	skillsResolver SkillsResolver
	credentials    CredentialProvisioner

	launchEnv LaunchEnv

	replay *ReplayCache
	dedup  *DedupCache

	draining *atomic.Bool

	// keepaliveInterval is the SSE comment heartbeat period. Zero means the
	// package default. Tests shrink it; production leaves it unset.
	keepaliveInterval time.Duration

	metrics *metrics.Metrics

	logger *slog.Logger

	// stdinMu serializes control-frame writes to each run's stdin. The executor
	// documents Run.Stdin as single-writer; webhook handlers run on independent
	// HTTP goroutines, so a per-run mutex keeps frame bytes from interleaving on
	// the wire. Keyed by project+"/"+cardID. Given the low message volume a
	// per-key lock is cheaper than a global one under concurrency.
	stdinMu sync.Map // map[string]*sync.Mutex

	// launchMu serializes per-card admission (credential provisioning + executor
	// launch). /trigger has no request dedup, so two concurrent triggers for one
	// card can both reach launch; without serialization the loser's provisioning
	// and launch-failure teardown interleave with — and clobber — the winner's
	// per-run credentials. Keyed like stdinMu; entries are likewise never
	// reclaimed (one bare mutex per card ever seen).
	launchMu sync.Map // map[string]*sync.Mutex

	// sseShutdown is closed by CloseSSE at drain so every in-flight /logs handler
	// returns promptly (an SSE stream never idles, so http.Server.Shutdown would
	// otherwise block the full timeout). Guarded by sseShutdownOnce for idempotency.
	sseShutdown     chan struct{}
	sseShutdownOnce sync.Once

	// credentialFallbackWarnOnce guards the compat-window deprecation warning
	// (see warnCredentialFallbackOnce) so it logs once per process, not once per
	// trigger.
	credentialFallbackWarnOnce sync.Once
}

// NewServer wires a Server from its dependencies. The replay cache, dedup
// cache, and draining flag are created if the caller leaves them nil so a bare
// Config still yields a usable server (tests rely on this).
func NewServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	skew := cfg.Skew
	if skew == 0 {
		skew = protocol.DefaultMaxClockSkew
	}

	replay := cfg.Replay
	if replay == nil {
		replay = NewReplayCache(skew, 4096)
	}

	dedup := cfg.Dedup
	if dedup == nil {
		dedup = NewDedupCache(10*time.Minute, 4096)
	}

	draining := cfg.Draining
	if draining == nil {
		draining = &atomic.Bool{}
	}

	return &Server{
		apiKey:            cfg.APIKey,
		skew:              skew,
		maxConcurrent:     cfg.MaxConcurrent,
		executor:          cfg.Executor,
		tracker:           cfg.Tracker,
		hub:               cfg.Hub,
		reporter:          cfg.Reporter,
		verifier:          cfg.Verifier,
		skillsResolver:    cfg.SkillsResolver,
		credentials:       cfg.Credentials,
		launchEnv:         cfg.LaunchEnv,
		replay:            replay,
		dedup:             dedup,
		draining:          draining,
		keepaliveInterval: cfg.KeepaliveInterval,
		metrics:           cfg.Metrics,
		logger:            logger,
		sseShutdown:       make(chan struct{}),
	}
}

// CloseSSE unblocks every in-flight /logs SSE handler. Wire it via
// httpServer.RegisterOnShutdown so SIGTERM drain returns promptly. Idempotent.
func (s *Server) CloseSSE() {
	s.sseShutdownOnce.Do(func() { close(s.sseShutdown) })
}

// Routes returns the mux with every webhook route mounted. The mutating
// lifecycle routes are gated on drain; /kill, /stop-all, /logs, /containers,
// /health, /readyz stay reachable during shutdown so operators can read state
// and stop work.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /trigger", s.recordMetrics(s.auth(s.drainGate(s.handleTrigger))))
	mux.HandleFunc("POST /kill", s.recordMetrics(s.auth(s.handleKill)))
	mux.HandleFunc("POST /stop-all", s.recordMetrics(s.auth(s.handleStopAll)))
	mux.HandleFunc("POST /message", s.recordMetrics(s.auth(s.drainGate(s.handleMessage))))
	mux.HandleFunc("POST /promote", s.recordMetrics(s.auth(s.drainGate(s.handlePromote))))
	mux.HandleFunc("POST /end-session", s.recordMetrics(s.auth(s.drainGate(s.handleEndSession))))
	mux.HandleFunc("GET /containers", s.recordMetrics(s.auth(s.handleContainers)))
	mux.HandleFunc("GET /logs", s.recordMetrics(s.auth(s.handleLogs)))
	mux.HandleFunc("GET /health", s.recordMetrics(s.handleHealth))
	mux.HandleFunc("GET /readyz", s.recordMetrics(s.handleReadyz))

	return mux
}

// AdminAuth exposes the HMAC verifier for the admin /metrics endpoint, which
// the serve layer mounts on a separate loopback listener. It reuses the same
// signed-GET verification, replay cache, and skew as the webhook routes — the
// agent-backend signed-GET HMAC is real auth, preserved here.
func (s *Server) AdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return s.auth(next)
}

// stdinLock returns the per-run mutex for project/cardID, creating it on first
// use. Callers Lock/Unlock around frames.Write to honour Run.Stdin's
// single-writer contract. Entries are deliberately never reclaimed: one bare
// mutex per card ever seen is a tiny, process-bounded footprint, and skipping
// deletion avoids delete/recreate races with in-flight writers.
func (s *Server) stdinLock(project, cardID string) *sync.Mutex {
	k := project + "/" + cardID
	v, _ := s.stdinMu.LoadOrStore(k, &sync.Mutex{})

	return v.(*sync.Mutex)
}

// launchLock returns the per-card admission mutex, creating it on first use.
// See the launchMu field comment for why launch serializes per card.
func (s *Server) launchLock(project, cardID string) *sync.Mutex {
	k := project + "/" + cardID
	v, _ := s.launchMu.LoadOrStore(k, &sync.Mutex{})

	return v.(*sync.Mutex)
}

// ---- trigger ----------------------------------------------------------------

func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	var payload protocol.TriggerPayload
	if !s.decode(w, r, &payload) {
		return
	}

	// Capacity pre-check: refuse before we spawn the launch goroutine so the
	// 429 is synchronous. The executor re-checks under its own lock at admission
	// time (ErrCapacity), so a race that slips past here still fails closed.
	if s.maxConcurrent > 0 && s.tracker.Count() >= s.maxConcurrent {
		s.logger.Warn("trigger rejected: at capacity",
			"project", payload.Project, "card_id", payload.CardID, "limit", s.maxConcurrent)
		writeError(w, http.StatusTooManyRequests, protocol.CodeLimitReached, "concurrency limit reached")

		return
	}

	if payload.TaskSkills != nil {
		if err := validateTaskSkills(*payload.TaskSkills); err != nil {
			s.logger.Warn("trigger rejected: invalid task_skills",
				"project", payload.Project, "card_id", payload.CardID, "error", err)
			writeError(w, http.StatusBadRequest, protocol.CodeInvalidField, "invalid task_skills")

			return
		}
	}

	// Resolve the skills dir best-effort: a failure means this run runs without
	// skills (advisory), and the next trigger retries.
	skillsDir := ""

	if s.skillsResolver != nil {
		if dir, err := s.skillsResolver.Resolve(r.Context()); err != nil {
			s.logger.Warn("task-skills unavailable this run",
				"project", payload.Project, "card_id", payload.CardID, "error", err)
		} else {
			skillsDir = dir
		}
	}

	// Correlation ID travels in the X-Correlation-ID header (runner parity), not
	// the trigger body. Fall back to the card ID so the worker always has a
	// non-empty trace key.
	correlationID := r.Header.Get(correlationHeader)
	if correlationID == "" {
		correlationID = payload.CardID
	}

	spec := s.buildLaunchSpec(payload, correlationID, skillsDir)

	// Respond 202 first, then launch asynchronously: /trigger must return fast,
	// and the eventual outcome is carried by the running/failed status callback.
	writeJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})

	go s.launch(spec, payload) //nolint:gosec // G118: launch is fire-and-forget and outlives the request; a request-scoped context would cancel the worker
}

// launch runs on a detached context so the container's lifecycle outlives the
// HTTP request that triggered it. It provisions the per-run credential file when
// the payload carried a git token, launches the container, and reports running
// on success and failed on a provisioning or launch error. Status callbacks run
// outside the admission lock so a slow ContextMatrix response cannot serialize
// later triggers for the card.
func (s *Server) launch(spec executor.LaunchSpec, p protocol.TriggerPayload) {
	ctx := context.Background()
	project, cardID := p.Project, p.CardID

	if failure := s.admitAndLaunch(ctx, spec, p); failure != "" {
		if rerr := s.reporter.ReportStatus(ctx, cardID, project, "failed", failure); rerr != nil {
			s.logger.Error("report failed-status callback failed",
				"project", project, "card_id", cardID, "error", rerr)
		}

		return
	}

	if err := s.reporter.ReportStatus(ctx, cardID, project, "running", ""); err != nil {
		s.logger.Error("report running-status callback failed",
			"project", project, "card_id", cardID, "error", err)
	}
}

// admitAndLaunch provisions the per-run credentials and launches the container,
// serialized per card. Returns "" on success or a client-safe failure message.
//
// The serialization plus the early tracked-run rejection close the
// duplicate-trigger window: two concurrent triggers for one card both reach
// admission (no request dedup, and the capacity pre-check does not serialize
// same-card admission), but under the lock the winner is admitted first and the
// loser sees the card already tracked and fails closed HERE — before it can
// provision or reach the executor. Proceeding to the executor on a tracked card
// would race the asynchronous tracker.Remove on the old run's exit: if Remove
// lands between the check and Launch, the executor would admit the duplicate — a
// container mounting a per-run dir nothing provisioned, which the old run's
// Teardown then deletes. Rejecting up front is the only race-free outcome; the
// winner's refresh loop and mounted run dir stay intact.
//
// Credential-availability guards: github and llm_endpoint are both optional
// local config now that ContextMatrix can provision either per run, but a run
// needs at least ONE source for each. A payload missing a credential AND
// lacking the corresponding local fallback is fail-closed rejected here —
// before any provisioning or executor call — rather than launched with a dud
// mount or an empty LLM endpoint.
func (s *Server) admitAndLaunch(ctx context.Context, spec executor.LaunchSpec, p protocol.TriggerPayload) string {
	project, cardID := p.Project, p.CardID

	mu := s.launchLock(project, cardID)
	mu.Lock()
	defer mu.Unlock()

	if _, tracked := s.tracker.Get(project, cardID); tracked {
		s.logger.Warn("trigger rejected: card already running",
			"project", project, "card_id", cardID)

		return "card already running"
	}

	if p.GitToken == "" && !s.launchEnv.SharedSecretsAvailable {
		s.logger.Warn("trigger rejected: no git credential source",
			"project", project, "card_id", cardID)

		return "CM did not provision a git token and no local github config exists"
	}

	if p.LLMEndpoint == nil && s.launchEnv.DefaultLLMEndpoint.APIKey == "" {
		s.logger.Warn("trigger rejected: no llm_endpoint credential source",
			"project", project, "card_id", cardID)

		return "CM did not provision an llm_endpoint and no local llm_endpoint config exists"
	}

	provisioned := false

	// Two credential delivery channels (see buildLaunchSpec): a git-token payload
	// provisions the per-run FILE here; an llm-only payload rides per-launch env
	// (env-first read) with no per-run file, so it is not provisioned.
	if s.credentials != nil && p.GitToken != "" {
		if err := s.credentials.Provision(project, cardID, p.GitToken, p.GitTokenExpiresAt, s.runEndpoint(p)); err != nil {
			s.logger.Error("provision per-run credentials failed",
				"project", project, "card_id", cardID, "error", err)

			return "credential provisioning failed"
		}

		provisioned = true
	}

	if err := s.executor.Launch(ctx, spec); err != nil {
		// Tear down only what THIS launch provisioned: a duplicate that skipped
		// provisioning must not touch the winner's credentials.
		if provisioned {
			s.credentials.Teardown(project, cardID)
		}

		s.logger.Error("launch failed", "project", project, "card_id", cardID, "error", err)

		return "launch failed"
	}

	return ""
}

// runEndpoint resolves the LLM endpoint staged into the per-run credential file:
// the payload's llm_endpoint when present, else the local-config default (the
// compat-window fallback). Gating the per-run file on the git token alone means
// a git-token-only payload still gets a working LLM endpoint from local config.
// admitAndLaunch's llm_endpoint guard runs before this is ever called, so by
// the time we get here p.LLMEndpoint != nil or DefaultLLMEndpoint.APIKey != ""
// — the fully-empty result this used to describe as "intentional" is no
// longer reachable in production.
func (s *Server) runEndpoint(p protocol.TriggerPayload) secrets.EndpointSecrets {
	if p.LLMEndpoint != nil {
		return secrets.EndpointSecrets{
			APIKey:  p.LLMEndpoint.APIKey,
			BaseURL: p.LLMEndpoint.BaseURL,
			Type:    p.LLMEndpoint.Type,
		}
	}

	return s.launchEnv.DefaultLLMEndpoint
}

// buildLaunchSpec folds the trigger payload together with the static LaunchEnv
// into a fully-resolved executor.LaunchSpec. The image override, repo URL, and
// base branch come from the payload; the CM_*/CMX_* env contract is assembled
// here.
func (s *Server) buildLaunchSpec(p protocol.TriggerPayload, correlationID, skillsDir string) executor.LaunchSpec {
	image := s.launchEnv.BaseImage
	if p.RunnerImage != "" {
		image = p.RunnerImage
	}

	mcpKey := s.launchEnv.MCPAPIKey
	if p.MCPAPIKey != "" {
		mcpKey = p.MCPAPIKey
	}

	env := []string{
		"CM_CARD_ID=" + p.CardID,
		"CM_PROJECT=" + p.Project,
		"CM_REPO_URL=" + p.RepoURL,
		"CM_BASE_BRANCH=" + p.BaseBranch,
		"CM_INTERACTIVE=" + boolEnv(p.Interactive),
		"CM_MODEL=" + p.Model,
		"CM_MCP_URL=" + s.launchEnv.MCPURL,
		"CM_MCP_API_KEY=" + mcpKey,
	}

	if p.BestOfN > 1 {
		env = append(env, "CM_BEST_OF_N="+strconv.Itoa(p.BestOfN))
	}

	// Compat window: CM-provisioned credentials in the trigger payload override
	// the shared-secrets values for this run over two delivery channels. When a
	// git token is present, ALL credentials (git token + LLM values) travel via a
	// per-run secrets FILE (written and refreshed by the credential provisioner,
	// mounted at /run/cm-secrets), never plain container env — the worker reads
	// them only from that file, and plain env would go stale as the refresh loop
	// rewrites the token. When the payload carries an llm_endpoint but NO git
	// token, the run keeps the shared mount (it still needs the rotating shared
	// git token) and the LLM values ride per-launch container env instead, which
	// the worker resolves env-first-then-file. A payload from a pre-multi-user CM
	// carries neither field: the run mounts the shared secrets file staged from
	// local github/llm_endpoint config, and this process logs the deprecation
	// warning once (not per trigger).
	secretsHostDir := s.launchEnv.SecretsHostDir
	if s.credentials != nil && p.GitToken != "" {
		secretsHostDir = s.credentials.HostDir(p.Project, p.CardID)
	}

	// LLM-only payload: deliver the payload's LLM endpoint values as per-launch
	// container env (all three, even when empty — an empty BaseURL means "the
	// type's canonical default"). The worker reads LLM_* env-first-then-file, so
	// these override the shared file's values without disturbing the rotating git
	// token the shared mount still provides.
	if p.LLMEndpoint != nil && p.GitToken == "" {
		env = append(env,
			"LLM_API_KEY="+p.LLMEndpoint.APIKey,
			"LLM_BASE_URL="+p.LLMEndpoint.BaseURL,
			"LLM_TYPE="+p.LLMEndpoint.Type,
		)
	}

	if p.GitToken == "" || p.LLMEndpoint == nil {
		s.warnCredentialFallbackOnce()
	}

	if s.launchEnv.BashTimeoutMaxSeconds > 0 {
		env = append(env, "CMX_BASH_TIMEOUT_MAX_SECONDS="+strconv.Itoa(s.launchEnv.BashTimeoutMaxSeconds))
	}

	if s.launchEnv.ToolOutputMaxBytes > 0 {
		env = append(env, "CMX_TOOL_OUTPUT_MAX_BYTES="+strconv.Itoa(s.launchEnv.ToolOutputMaxBytes))
	}

	if s.launchEnv.DefaultModel != "" {
		env = append(env, "CMX_DEFAULT_MODEL="+s.launchEnv.DefaultModel)
	}

	if s.launchEnv.ReasoningEffort != "" {
		env = append(env, "CMX_REASONING_EFFORT="+s.launchEnv.ReasoningEffort)
	}

	if s.launchEnv.MaxCardCost != 0 {
		env = append(env, "CMX_MAX_CARD_COST="+formatFloat(s.launchEnv.MaxCardCost))
	}

	if s.launchEnv.SelectorPriceHeadroom != 0 {
		env = append(env, "CMX_SELECTOR_PRICE_HEADROOM="+formatFloat(s.launchEnv.SelectorPriceHeadroom))
	}

	if s.launchEnv.CompactionEnabled {
		env = append(env,
			"CMX_COMPACTION_ENABLED=true",
			"CMX_COMPACTION_THRESHOLD="+formatFloat(s.launchEnv.CompactionThreshold),
			"CMX_COMPACTION_KEEP_RECENT_TURNS="+strconv.Itoa(s.launchEnv.CompactionKeepRecentTurns),
		)
	}

	if p.Selection != nil {
		if b, err := json.Marshal(p.Selection); err == nil {
			env = append(env, "CMX_SELECTION="+string(b))
		} else {
			s.logger.Warn("failed to marshal selection context; container will use default model",
				"project", p.Project, "card_id", p.CardID, "error", err)
		}
	}

	if p.Verify != nil {
		if b, err := json.Marshal(p.Verify); err == nil {
			env = append(env, "CMX_VERIFY="+string(b))
		} else {
			s.logger.Warn("failed to marshal verify config; container will detect the verify command",
				"project", p.Project, "card_id", p.CardID, "error", err)
		}
	}

	if skillsDir != "" {
		env = append(env, "CMX_TASK_SKILLS_DIR="+skillsMountPathEnv)

		if p.TaskSkills != nil {
			env = append(env, "CM_TASK_SKILLS_SET=1")
			env = append(env, "CM_TASK_SKILLS="+strings.Join(*p.TaskSkills, ","))
		}
	}

	env = append(env, s.launchEnv.WorkerExtraEnv...)

	// Best-of-N races N candidate implementations concurrently, so the pids
	// cap (a per-container ceiling) scales with N; the memory limit is
	// intentionally left alone here — candidate verifies are serialized in a
	// later task.
	pids := s.launchEnv.PidsLimit
	if p.BestOfN > 1 && pids > 0 {
		pids *= int64(p.BestOfN)
	}

	return executor.LaunchSpec{
		CardID:         p.CardID,
		Project:        p.Project,
		Image:          image,
		Env:            env,
		SecretsHostDir: secretsHostDir,
		SkillsHostDir:  skillsDir,
		CACertHostFile: s.launchEnv.CACertFile,
		MemoryBytes:    s.launchEnv.MemoryBytes,
		PidsLimit:      pids,
		CorrelationID:  correlationID,
		MCPURL:         s.launchEnv.MCPURL,
	}
}

// warnCredentialFallbackOnce logs the compat-window deprecation warning the
// first time a trigger payload omits a CM-provisioned credential. Guarded by
// credentialFallbackWarnOnce so a long-lived serve process logs it once,
// regardless of how many triggers fall back to local config.
func (s *Server) warnCredentialFallbackOnce() {
	s.credentialFallbackWarnOnce.Do(func() {
		s.logger.Warn("CM did not provision credentials; using local github/llm_endpoint config " +
			"— this fallback is deprecated")
	})
}

// ---- kill -------------------------------------------------------------------

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	var payload protocol.KillPayload
	if !s.decode(w, r, &payload) {
		return
	}

	if _, ok := s.tracker.Get(payload.Project, payload.CardID); !ok {
		writeError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

		return
	}

	if err := s.executor.Kill(r.Context(), payload.Project, payload.CardID); err != nil {
		if errors.Is(err, executor.ErrNotFound) {
			writeError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

			return
		}

		s.logger.Error("kill failed", "project", payload.Project, "card_id", payload.CardID, "error", err)
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "kill failed")

		return
	}

	writeJSON(w, http.StatusOK, protocol.SuccessResponse{OK: true})
}

// ---- stop-all ---------------------------------------------------------------

func (s *Server) handleStopAll(w http.ResponseWriter, r *http.Request) {
	var payload protocol.StopAllPayload
	if !s.decode(w, r, &payload) {
		return
	}

	stopResults, err := s.executor.StopAll(r.Context(), payload.Project)
	if err != nil {
		s.logger.Error("stop-all failed", "project", payload.Project, "error", err)
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "stop-all failed")

		return
	}

	var stopped, failed int

	results := make([]protocol.CardKillResult, 0, len(stopResults))

	for _, sr := range stopResults {
		ckr := protocol.CardKillResult{
			CardID:  sr.Run.CardID,
			Project: sr.Run.Project,
			OK:      sr.Err == nil,
		}

		if sr.Err != nil {
			ckr.Error = sr.Err.Error()
			failed++
		} else {
			stopped++
		}

		results = append(results, ckr)
	}

	status := http.StatusOK
	if failed > 0 {
		status = http.StatusMultiStatus
	}

	writeJSON(w, status, protocol.StopAllResponse{
		OK:      failed == 0,
		Total:   len(stopResults),
		Stopped: stopped,
		Failed:  failed,
		Results: results,
	})
}

// ---- message ----------------------------------------------------------------

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var payload protocol.MessagePayload
	if !s.decode(w, r, &payload) {
		return
	}

	// Retried request whose first attempt already DELIVERED: return a cached ack
	// without re-writing the user frame to stdin. The record happens only after a
	// successful write below, so a 404 or a failed write never poisons the cache —
	// a retry then re-attempts delivery instead of getting a false duplicate ack.
	if s.dedup.Contains(payload.Project, payload.CardID, payload.MessageID) {
		writeJSON(w, http.StatusOK, protocol.SuccessResponse{
			OK:        true,
			Message:   "duplicate message acknowledged",
			MessageID: payload.MessageID,
		})

		return
	}

	run, ok := s.tracker.Get(payload.Project, payload.CardID)
	if !ok {
		writeError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

		return
	}

	mu := s.stdinLock(payload.Project, payload.CardID)
	mu.Lock()
	err := frames.Write(run.Stdin, frames.Frame{
		Type:      frames.TypeUserMessage,
		Content:   payload.Content,
		MessageID: payload.MessageID,
	})
	mu.Unlock()

	if err != nil {
		if errors.Is(err, frames.ErrFrameTooLarge) {
			s.logger.Warn("message rejected: frame too large",
				"project", payload.Project, "card_id", payload.CardID)
			writeError(w, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge, "message content too large")

			return
		}

		s.logger.Error("message stdin write failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "write failed")

		return
	}

	// Delivered: publish to the chat stream and record the message_id so a retry
	// is deduped, then clear the awaiting flag and touch the run.
	s.hub.PublishUser(payload.Project, payload.CardID, payload.Content)
	s.dedup.Record(payload.Project, payload.CardID, payload.MessageID)
	s.tracker.SetAwaiting(payload.Project, payload.CardID, false)
	s.tracker.Touch(payload.Project, payload.CardID)

	writeJSON(w, http.StatusAccepted, protocol.SuccessResponse{
		OK:        true,
		MessageID: payload.MessageID,
	})
}

// ---- promote ----------------------------------------------------------------

func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	var payload protocol.PromotePayload
	if !s.decode(w, r, &payload) {
		return
	}

	run, ok := s.tracker.Get(payload.Project, payload.CardID)
	if !ok {
		writeError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

		return
	}

	// Fail closed: verify the card's autonomous flag against ContextMatrix
	// before writing the promote frame. Any error means "do not promote".
	autonomous, err := s.verifier.VerifyAutonomous(r.Context(), payload.Project, payload.CardID)
	if err != nil {
		s.logger.Error("verify-autonomous failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		writeError(w, http.StatusBadGateway, protocol.CodeUpstreamFailure, "upstream verification failed")

		return
	}

	if !autonomous {
		writeError(w, http.StatusConflict, protocol.CodeConflict, "card is not autonomous")

		return
	}

	mu := s.stdinLock(payload.Project, payload.CardID)
	mu.Lock()
	err = frames.Write(run.Stdin, frames.Frame{Type: frames.TypePromote})
	mu.Unlock()

	if err != nil {
		s.logger.Error("promote stdin write failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "write failed")

		return
	}

	s.hub.Publish(protocol.LogEntry{
		Timestamp: time.Now(),
		Project:   payload.Project,
		CardID:    payload.CardID,
		Type:      "system",
		Content:   "promoted to autonomous mode",
	})

	writeJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})
}

// ---- end-session ------------------------------------------------------------

func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	var payload protocol.EndSessionPayload
	if !s.decode(w, r, &payload) {
		return
	}

	run, ok := s.tracker.Get(payload.Project, payload.CardID)
	if !ok {
		// Idempotent: nothing to end is a success, not a 404.
		writeJSON(w, http.StatusOK, protocol.SuccessResponse{OK: true})

		return
	}

	mu := s.stdinLock(payload.Project, payload.CardID)
	mu.Lock()
	err := frames.Write(run.Stdin, frames.Frame{Type: frames.TypeEndSession})
	mu.Unlock()

	if err != nil {
		s.logger.Error("end-session stdin write failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "write failed")

		return
	}

	writeJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})
}

// ---- containers -------------------------------------------------------------

func (s *Server) handleContainers(w http.ResponseWriter, _ *http.Request) {
	runs := s.tracker.List()

	items := make([]protocol.ContainerListItem, 0, len(runs))
	for _, run := range runs {
		items = append(items, protocol.ContainerListItem{
			ContainerID: run.ContainerID,
			CardID:      run.CardID,
			Project:     run.Project,
			State:       "running",
			StartedAt:   run.StartedAt.UTC().Format(time.RFC3339),
			Tracked:     true,
		})
	}

	writeJSON(w, http.StatusOK, protocol.ListContainersResponse{OK: true, Containers: items})
}

// ---- health / readyz --------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{
		OK:                true,
		RunningContainers: s.tracker.Count(),
		MaxConcurrent:     s.maxConcurrent,
	})
}

// readyResponse is the /readyz body. It is a custom shape (not ErrorResponse)
// so the readiness probe stays self-describing for orchestrators.
type readyResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if s.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, readyResponse{OK: false, Reason: "draining"})

		return
	}

	writeJSON(w, http.StatusOK, readyResponse{OK: true})
}

// ---- decode + write helpers -------------------------------------------------

// decode unmarshals the (already auth-verified) request body into v. The body
// was re-injected by the auth middleware, so a normal read suffices. On a JSON
// error it writes a 400 and returns false.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidJSON, "invalid JSON")

		return false
	}

	return true
}

// writeJSON marshals v and writes it with the given status. A marshal failure
// falls back to a fixed internal-error body so the client always gets
// well-formed JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")

	body, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"code":"internal","message":"response marshal failed"}`))

		return
	}

	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError serialises a protocol.ErrorResponse. msg must be a fixed,
// client-safe string, never raw err.Error() text.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, protocol.ErrorResponse{OK: false, Code: code, Message: msg})
}

// boolEnv renders a bool as the worker's "true"/"" env convention
// (CM_INTERACTIVE == "true").
func boolEnv(b bool) string {
	if b {
		return "true"
	}

	return ""
}

// formatFloat renders a float64 as a compact string without unnecessary
// trailing zeros (e.g. 5.0 -> "5", 1.5 -> "1.5"). strconv.FormatFloat with
// 'f' and -1 precision does this correctly.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

const maxTaskSkills = 64

var taskSkillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// validateTaskSkills mirrors the runner's ValidateTaskSkills: at most 64 entries,
// each a safe skill name. CM validates too, so this is a should-never-happen
// guard against a malformed payload.
func validateTaskSkills(skills []string) error {
	if len(skills) > maxTaskSkills {
		return fmt.Errorf("too many task_skills: %d > %d", len(skills), maxTaskSkills)
	}

	for _, s := range skills {
		if !taskSkillNamePattern.MatchString(s) {
			return fmt.Errorf("invalid task_skill name %q", s)
		}
	}

	return nil
}
