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
	"github.com/mhersson/contextmatrix-agent/internal/metrics"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-backendkit/frames"
	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const (
	// correlationHeader carries the client's trace ID. The agent backend reads
	// it on /trigger and threads it into executor.LaunchSpec.CorrelationID (the
	// container label) so host and worker logs stitch to the same CM trace.
	correlationHeader = "X-Correlation-ID"

	// skillsMountPathEnv is the in-container path the executor mounts the skills
	// dir at (must match executor.skillsMountPath). Passed to the worker as
	// CMX_TASK_SKILLS_DIR.
	skillsMountPathEnv = "/run/cm-skills"
)

// StatusReporter reports a task's worker-status transition back to
// ContextMatrix. *callback.Client satisfies it; tests supply a fake.
type StatusReporter interface {
	ReportStatus(ctx context.Context, cardID, project, status, message string) error
}

// AutonomousVerifier confirms a card's autonomous flag before /promote writes
// the control frame - fail closed. *callback.Client satisfies it; tests supply
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
// fake. Nil disables per-run provisioning - the run then mounts the shared
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
	// it via worker_image.
	BaseImage string

	// MCPURL and MCPAPIKey wire the worker's MCP client back to ContextMatrix.
	// MCPAPIKey is the fallback when the payload omits mcp_api_key.
	MCPURL    string
	MCPAPIKey string

	// SecretsHostDir is the fallback host directory bind-mounted read-only into
	// a container at /run/cm-secrets when the launch spec is built for a payload
	// without a git token (such specs are rejected by admitAndLaunch before
	// launch). Admitted runs always mount their per-run credential directory
	// instead. Empty disables the mount.
	SecretsHostDir string

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
	APIKey string

	// MetricsToken is the static bearer token AdminAuth accepts on the admin
	// /metrics route as an alternative to the signed-GET HMAC. Empty keeps
	// the route HMAC-only.
	MetricsToken string

	Skew          time.Duration
	MaxConcurrent int

	Executor       executor.Executor
	Tracker        *executor.Tracker
	Hub            *logbridge.Hub
	Reporter       StatusReporter
	Verifier       AutonomousVerifier
	SkillsResolver SkillsResolver
	Credentials    CredentialProvisioner

	// Images lists the node's tagged images for GET /images. Nil disables the
	// endpoint (500 internal).
	Images webhookcore.ImageLister

	// ImageListFilters are the per-tag substring filters applied to GET
	// /images responses. The serve layer always supplies at least the family
	// default; an empty slice yields an empty list, never "everything".
	ImageListFilters []string

	LaunchEnv LaunchEnv

	Replay *webhookcore.ReplayCache
	Dedup  *DedupCache

	Draining *atomic.Bool

	// KeepaliveInterval overrides the SSE heartbeat period. Zero uses the
	// package default; tests shrink it.
	KeepaliveInterval time.Duration

	// Metrics is the Prometheus bundle. Nil disables request instrumentation.
	Metrics *metrics.Metrics

	Logger *slog.Logger
}

// Server is the agent backend's HTTP surface. The embedded *webhookcore.Core
// provides the shared transport surface (HMAC auth, the drain gate, request
// metrics, the SSE /logs stream, and the health/readiness/images probes); the
// Server adds the agent-specific lifecycle handlers. It owns no goroutines
// beyond the per-trigger launch goroutines it spawns; the replay janitor and
// the executor supervision live in their respective owners.
type Server struct {
	*webhookcore.Core

	maxConcurrent int

	executor       executor.Executor
	tracker        *executor.Tracker
	hub            *logbridge.Hub
	reporter       StatusReporter
	verifier       AutonomousVerifier
	skillsResolver SkillsResolver
	credentials    CredentialProvisioner

	launchEnv LaunchEnv

	dedup *DedupCache

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
	// and launch-failure teardown interleave with - and clobber - the winner's
	// per-run credentials. Keyed like stdinMu; entries are likewise never
	// reclaimed (one bare mutex per card ever seen).
	launchMu sync.Map // map[string]*sync.Mutex
}

// NewServer wires a Server from its dependencies. It builds the transport Core
// first (which defaults the skew, replay cache, and draining flag when the
// caller leaves them nil) then wires the agent-specific fields. The dedup cache
// is defaulted here so a bare Config still yields a usable server (tests rely on
// this).
func NewServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	coreCfg := webhookcore.CoreConfig{
		APIKey:            cfg.APIKey,
		MetricsToken:      cfg.MetricsToken,
		Skew:              cfg.Skew,
		Replay:            cfg.Replay,
		Draining:          cfg.Draining,
		KeepaliveInterval: cfg.KeepaliveInterval,
		Logger:            logger,
		Hub:               cfg.Hub,
		LogsFilterParam:   "project",
		LogsFilterAttr:    "project_filter",
		Tracker:           cfg.Tracker,
		MaxConcurrent:     cfg.MaxConcurrent,
		Images:            cfg.Images,
		ImageListFilters:  cfg.ImageListFilters,
	}
	if cfg.Metrics != nil {
		coreCfg.Metrics = cfg.Metrics.Metrics
	}

	dedup := cfg.Dedup
	if dedup == nil {
		dedup = NewDedupCache(10*time.Minute, 4096)
	}

	return &Server{
		Core:           webhookcore.NewCore(coreCfg),
		maxConcurrent:  cfg.MaxConcurrent,
		executor:       cfg.Executor,
		tracker:        cfg.Tracker,
		hub:            cfg.Hub,
		reporter:       cfg.Reporter,
		verifier:       cfg.Verifier,
		skillsResolver: cfg.SkillsResolver,
		credentials:    cfg.Credentials,
		launchEnv:      cfg.LaunchEnv,
		dedup:          dedup,
		logger:         logger,
	}
}

// Routes returns the mux with every webhook route mounted. The mutating
// lifecycle routes are gated on drain; /kill, /stop-all, /logs, /containers,
// /images, /health, /readyz stay reachable during shutdown so operators can
// read state and stop work. The transport middlewares and the
// health/readiness/images/logs handlers are promoted from the embedded Core.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /trigger", s.RecordMetrics(s.Auth(s.DrainGate(s.handleTrigger))))
	mux.HandleFunc("POST /kill", s.RecordMetrics(s.Auth(s.handleKill)))
	mux.HandleFunc("POST /stop-all", s.RecordMetrics(s.Auth(s.handleStopAll)))
	mux.HandleFunc("POST /message", s.RecordMetrics(s.Auth(s.DrainGate(s.handleMessage))))
	mux.HandleFunc("POST /promote", s.RecordMetrics(s.Auth(s.DrainGate(s.handlePromote))))
	mux.HandleFunc("POST /end-session", s.RecordMetrics(s.Auth(s.DrainGate(s.handleEndSession))))
	mux.HandleFunc("GET /containers", s.RecordMetrics(s.Auth(s.handleContainers)))
	mux.HandleFunc("GET /images", s.RecordMetrics(s.Auth(s.HandleImages)))
	mux.HandleFunc("GET /logs", s.RecordMetrics(s.Auth(s.HandleLogs)))
	mux.HandleFunc("GET /health", s.RecordMetrics(s.HandleHealth))
	mux.HandleFunc("GET /readyz", s.RecordMetrics(s.HandleReadyz))

	return mux
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
	if !webhookcore.Decode(w, r, &payload) {
		return
	}

	// Capacity pre-check: refuse before we spawn the launch goroutine so the
	// 429 is synchronous. The executor re-checks under its own lock at admission
	// time (ErrCapacity), so a race that slips past here still fails closed.
	if s.maxConcurrent > 0 && s.tracker.Count() >= s.maxConcurrent {
		s.logger.Warn("trigger rejected: at capacity",
			"project", payload.Project, "card_id", payload.CardID, "limit", s.maxConcurrent)
		webhookcore.WriteError(w, http.StatusTooManyRequests, protocol.CodeLimitReached, "concurrency limit reached")

		return
	}

	if payload.TaskSkills != nil {
		if err := validateTaskSkills(*payload.TaskSkills); err != nil {
			s.logger.Warn("trigger rejected: invalid task_skills",
				"project", payload.Project, "card_id", payload.CardID, "error", err)
			webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "invalid task_skills")

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

	// Correlation ID travels in the X-Correlation-ID header, not the trigger
	// body. Fall back to the card ID so the worker always has a non-empty
	// trace key.
	correlationID := r.Header.Get(correlationHeader)
	if correlationID == "" {
		correlationID = payload.CardID
	}

	spec := s.buildLaunchSpec(payload, correlationID, skillsDir)

	// Respond 202 first, then launch asynchronously: /trigger must return fast,
	// and the eventual outcome is carried by the running/failed status callback.
	webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})

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
// loser sees the card already tracked and fails closed HERE - before it can
// provision or reach the executor. Proceeding to the executor on a tracked card
// would race the asynchronous tracker.Remove on the old run's exit: if Remove
// lands between the check and Launch, the executor would admit the duplicate - a
// container mounting a per-run dir nothing provisioned, which the old run's
// Teardown then deletes. Rejecting up front is the only race-free outcome; the
// winner's refresh loop and mounted run dir stay intact.
//
// Credential-availability guards: ContextMatrix provisions both credentials
// per run - there is no local fallback. A payload missing the git token or the
// llm_endpoint is fail-closed rejected here, before any provisioning or
// executor call, rather than launched with a dud mount or an empty LLM
// endpoint.
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

	if p.GitToken == "" {
		s.logger.Warn("trigger rejected: no git credential source",
			"project", project, "card_id", cardID)

		return "CM did not provision a git token"
	}

	if p.LLMEndpoint == nil {
		s.logger.Warn("trigger rejected: no llm_endpoint credential source",
			"project", project, "card_id", cardID)

		return "CM did not provision an llm_endpoint"
	}

	provisioned := false

	// The fail-closed guard above already rejected empty-GitToken payloads.
	if s.credentials != nil {
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

// runEndpoint maps the payload's llm_endpoint - plus the mob session guest
// specs, which carry bearer tokens and therefore ride the secrets file - into
// the per-run credential file's shape. Guests are set regardless of the LLM
// endpoint so an endpoint-less payload still delivers them.
func (s *Server) runEndpoint(p protocol.TriggerPayload) secrets.EndpointSecrets {
	e := secrets.EndpointSecrets{MobGuests: s.mobGuestsJSON(p)}

	if p.LLMEndpoint == nil {
		return e
	}

	e.APIKey = p.LLMEndpoint.APIKey
	e.BaseURL = p.LLMEndpoint.BaseURL
	e.Type = p.LLMEndpoint.Type

	return e
}

// mobGuestsJSON compact-encodes the payload's guest specs for the per-run
// secrets file. Empty or unmarshalable guests yield "" so the key is omitted
// - a delivery problem degrades to a guestless discussion, never a failed run.
func (s *Server) mobGuestsJSON(p protocol.TriggerPayload) string {
	if p.Mob == nil || len(p.Mob.Guests) == 0 {
		return ""
	}

	b, err := json.Marshal(p.Mob.Guests)
	if err != nil {
		s.logger.Warn("failed to marshal mob guests; discussion runs without guests",
			"project", p.Project, "card_id", p.CardID, "error", err)

		return ""
	}

	return string(b)
}

// buildLaunchSpec folds the trigger payload together with the static LaunchEnv
// into a fully-resolved executor.LaunchSpec. The image override, repo URL, and
// base branch come from the payload; the CM_*/CMX_* env contract is assembled
// here.
func (s *Server) buildLaunchSpec(p protocol.TriggerPayload, correlationID, skillsDir string) executor.LaunchSpec {
	image := s.launchEnv.BaseImage
	if p.WorkerImage != "" {
		image = p.WorkerImage
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

	if p.Mob != nil && p.Mob.Participants >= 2 {
		env = append(env, "CM_MOB_PARTICIPANTS="+strconv.Itoa(p.Mob.Participants))

		if len(p.Mob.Phases) > 0 {
			env = append(env, "CM_MOB_PHASES="+strings.Join(p.Mob.Phases, ","))
		}

		if p.Mob.Rounds > 0 {
			env = append(env, "CM_MOB_ROUNDS="+strconv.Itoa(p.Mob.Rounds))
		}

		if p.Mob.BudgetFactor > 0 {
			env = append(env, "CM_MOB_BUDGET_FACTOR="+formatFloat(p.Mob.BudgetFactor))
		}

		if p.Mob.ExecuteCheckpoints {
			env = append(env, "CM_MOB_EXECUTE_CHECKPOINTS=true")

			if p.Mob.CheckpointMinTier != "" {
				env = append(env, "CM_MOB_CHECKPOINT_MIN_TIER="+p.Mob.CheckpointMinTier)
			}

			if p.Mob.CheckpointRounds > 0 {
				env = append(env, "CM_MOB_CHECKPOINT_ROUNDS="+strconv.Itoa(p.Mob.CheckpointRounds))
			}
		}
	}

	// CM-provisioned credentials (git token + LLM values) travel via a per-run
	// secrets FILE (written and refreshed by the credential provisioner, mounted
	// at /run/cm-secrets), never plain container env - the worker reads them
	// only from that file, and plain env would go stale as the refresh loop
	// rewrites the token. A spec built for a payload without a git token keeps
	// the static SecretsHostDir fallback, but admitAndLaunch rejects such
	// payloads before launch.
	secretsHostDir := s.launchEnv.SecretsHostDir
	if s.credentials != nil && p.GitToken != "" {
		secretsHostDir = s.credentials.HostDir(p.Project, p.CardID)
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
	// intentionally left alone here - candidate verifies run serially in the
	// judge phase.
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

// ---- kill -------------------------------------------------------------------

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	var payload protocol.KillPayload
	if !webhookcore.Decode(w, r, &payload) {
		return
	}

	if _, ok := s.tracker.Get(payload.Project, payload.CardID); !ok {
		webhookcore.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

		return
	}

	if err := s.executor.Kill(r.Context(), payload.Project, payload.CardID); err != nil {
		if errors.Is(err, executor.ErrNotFound) {
			webhookcore.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

			return
		}

		s.logger.Error("kill failed", "project", payload.Project, "card_id", payload.CardID, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "kill failed")

		return
	}

	webhookcore.WriteJSON(w, http.StatusOK, protocol.SuccessResponse{OK: true})
}

// ---- stop-all ---------------------------------------------------------------

func (s *Server) handleStopAll(w http.ResponseWriter, r *http.Request) {
	var payload protocol.StopAllPayload
	if !webhookcore.Decode(w, r, &payload) {
		return
	}

	stopResults, err := s.executor.StopAll(r.Context(), payload.Project)
	if err != nil {
		s.logger.Error("stop-all failed", "project", payload.Project, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "stop-all failed")

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

	webhookcore.WriteJSON(w, status, protocol.StopAllResponse{
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
	if !webhookcore.Decode(w, r, &payload) {
		return
	}

	// Retried request whose first attempt already DELIVERED: return a cached ack
	// without re-writing the user frame to stdin. The record happens only after a
	// successful write below, so a 404 or a failed write never poisons the cache -
	// a retry then re-attempts delivery instead of getting a false duplicate ack.
	if s.dedup.Contains(payload.Project, payload.CardID, payload.MessageID) {
		webhookcore.WriteJSON(w, http.StatusOK, protocol.SuccessResponse{
			OK:        true,
			Message:   "duplicate message acknowledged",
			MessageID: payload.MessageID,
		})

		return
	}

	run, ok := s.tracker.Get(payload.Project, payload.CardID)
	if !ok {
		webhookcore.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

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
			webhookcore.WriteError(w, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge, "message content too large")

			return
		}

		s.logger.Error("message stdin write failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "write failed")

		return
	}

	// Delivered: publish to the chat stream and record the message_id so a retry
	// is deduped, then clear the awaiting flag and touch the run.
	publishUser(s.hub, payload.Project, payload.CardID, payload.Content)
	s.dedup.Record(payload.Project, payload.CardID, payload.MessageID)
	s.tracker.SetAwaiting(payload.Project, payload.CardID, false)
	s.tracker.Touch(payload.Project, payload.CardID)

	webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{
		OK:        true,
		MessageID: payload.MessageID,
	})
}

// publishUser emits a "user"-type log entry directly to the hub. It is NOT
// redacted - user content comes from the human and is displayed verbatim.
func publishUser(hub *logbridge.Hub, project, cardID, content string) {
	hub.Publish(protocol.LogEntry{
		Timestamp: time.Now(),
		Project:   project,
		CardID:    cardID,
		Type:      "user",
		Content:   content,
	})
}

// ---- promote ----------------------------------------------------------------

func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	var payload protocol.PromotePayload
	if !webhookcore.Decode(w, r, &payload) {
		return
	}

	run, ok := s.tracker.Get(payload.Project, payload.CardID)
	if !ok {
		webhookcore.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

		return
	}

	// Fail closed: verify the card's autonomous flag against ContextMatrix
	// before writing the promote frame. Any error means "do not promote".
	autonomous, err := s.verifier.VerifyAutonomous(r.Context(), payload.Project, payload.CardID)
	if err != nil {
		s.logger.Error("verify-autonomous failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		webhookcore.WriteError(w, http.StatusBadGateway, protocol.CodeUpstreamFailure, "upstream verification failed")

		return
	}

	if !autonomous {
		webhookcore.WriteError(w, http.StatusConflict, protocol.CodeConflict, "card is not autonomous")

		return
	}

	mu := s.stdinLock(payload.Project, payload.CardID)
	mu.Lock()
	err = frames.Write(run.Stdin, frames.Frame{Type: frames.TypePromote})
	mu.Unlock()

	if err != nil {
		s.logger.Error("promote stdin write failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "write failed")

		return
	}

	s.hub.Publish(protocol.LogEntry{
		Timestamp: time.Now(),
		Project:   payload.Project,
		CardID:    payload.CardID,
		Type:      "system",
		Content:   "promoted to autonomous mode",
	})

	webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})
}

// ---- end-session ------------------------------------------------------------

func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	var payload protocol.EndSessionPayload
	if !webhookcore.Decode(w, r, &payload) {
		return
	}

	run, ok := s.tracker.Get(payload.Project, payload.CardID)
	if !ok {
		// Idempotent: nothing to end is a success, not a 404.
		webhookcore.WriteJSON(w, http.StatusOK, protocol.SuccessResponse{OK: true})

		return
	}

	mu := s.stdinLock(payload.Project, payload.CardID)
	mu.Lock()
	err := frames.Write(run.Stdin, frames.Frame{Type: frames.TypeEndSession})
	mu.Unlock()

	if err != nil {
		s.logger.Error("end-session stdin write failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "write failed")

		return
	}

	webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})
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

	webhookcore.WriteJSON(w, http.StatusOK, protocol.ListContainersResponse{OK: true, Containers: items})
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

// validateTaskSkills enforces the task_skills contract: at most 64 entries,
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
