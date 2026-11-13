package webhook

import (
	"context"
	"crypto/rand"
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
	"github.com/mhersson/contextmatrix-backendkit/llmproxy"
	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const (
	// correlationHeader carries ContextMatrix's trace ID, fresh on every
	// attempt. requestRunID appends it to the run id it mints, which
	// becomes executor.LaunchSpec.CorrelationID (the container label), so host
	// and worker logs stitch to the same CM trace. That run id is also what
	// SessionID keys redaction sessions by (see addSessionSecrets and
	// removeSessionSecrets): distinct ids for two runs of the same card let a
	// stale run's exit remove only its own redaction keys.
	correlationHeader = "X-Correlation-ID"

	// skillsMountPathEnv is the in-container path the executor mounts the skills
	// dir at (must match executor.skillsMountPath). Passed to the worker as
	// CMX_TASK_SKILLS_DIR.
	skillsMountPathEnv = "/run/cm-skills"

	// launchTimeout bounds one trigger's admission, image pull included. It is
	// sized for a cold pull of the full worker image, several gigabytes, over a
	// slow link; a launch still running past it has stalled, and failing it
	// frees the card's admission lock.
	launchTimeout = 15 * time.Minute

	// killTimeout bounds a kill made on its own context: the executor's
	// self-exit grace plus the SIGKILL round trip, with headroom. Stop-all
	// kills concurrently, so the run count does not stretch it.
	killTimeout = 10 * time.Second
)

// StatusReporter reports a task's worker-status transition back to
// ContextMatrix. *callback.Client satisfies it; tests supply a fake.
type StatusReporter interface {
	ReportStatus(ctx context.Context, p protocol.StatusCallbackPayload) error
}

// SessionSecretRegistry manages per-session secrets registered with the log
// bridge redactor so CM-provisioned credentials appearing on worker stderr or
// unparsable stdout are masked on the /logs SSE stream. AddSessionKey must
// ignore empty keys so callers may register unconditionally.
//
// *logbridge.RedactorRegistry satisfies it; tests supply a fake.
type SessionSecretRegistry interface {
	AddSessionKey(id, key string)
	RemoveSessionKey(id string)
}

// LLMProxy keeps each run's real LLM key on serve: Register returns the
// stand-in token the worker presents, Revoke ends it. *llmproxy.Proxy
// satisfies it; tests supply a fake.
type LLMProxy interface {
	http.Handler
	Register(runKey string, up protocol.LLMEndpoint) (string, error)
	Revoke(runKey string)
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
	// error. correlationID identifies this run for OnTokenRefresh callers.
	Provision(project, cardID, correlationID, token, expiresAt string, endpoint secrets.EndpointSecrets) error
	// Teardown stops this run's refresh loop and removes the directory its
	// own handle carried. Keyed by correlation id: a stale run's teardown
	// against a successor run's state is a no-op. Idempotent.
	Teardown(project, cardID, correlationID string)
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

	// LLMBaseURL is the container-reachable LLM proxy URL (<serve>/llm), staged
	// as the worker's LLM base URL in place of the upstream one.
	LLMBaseURL string

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

	// ContainerTimeoutSeconds is the service config's container_timeout,
	// passed to the worker as CMX_CONTAINER_TIMEOUT_SECONDS so a phase that
	// must park before serve's hard kill knows its deadline. Zero is omitted
	// (the worker treats absence as "unknown").
	ContainerTimeoutSeconds int

	// BashTimeoutMaxSeconds, ToolOutputMaxBytes, DefaultModel, and
	// ReasoningEffort are the CMX_* worker knobs. Zero/empty values are omitted
	// so the worker applies its own defaults.
	BashTimeoutMaxSeconds int
	ToolOutputMaxBytes    int
	DefaultModel          string
	ReasoningEffort       string

	// ReplayReasoning is relayed as CMX_REPLAY_REASONING=true and omitted when false.
	ReplayReasoning bool

	// MaxCardCost is the cumulative USD ceiling per card passed as
	// CMX_MAX_CARD_COST. Zero is omitted (worker applies its own default).
	MaxCardCost float64

	// CompactionEnabled, CompactionThreshold, and CompactionKeepRecentTurns
	// configure the worker harness loop's in-window compaction. When disabled
	// (the default) the CMX_COMPACTION_* vars are omitted so the worker keeps the
	// hard context_limit stop.
	CompactionEnabled         bool
	CompactionThreshold       float64
	CompactionKeepRecentTurns int

	// ReviewAttemptsCap is the configured review-round budget forwarded to
	// workers as CMX_REVIEW_ATTEMPTS_CAP. Zero means unset and is omitted, so
	// the worker applies config.DefaultReviewAttemptsCap. The serve config
	// validates the range, so anything non-zero reaching here is in bounds.
	ReviewAttemptsCap int

	// The pr_gates knobs, relayed as CMX_GATES_*. Zero seconds are omitted so
	// the worker applies its defaults; the thread-replies flag is always sent.
	GatesPollIntervalSeconds       int
	GatesCIWaitTimeoutSeconds      int
	GatesCopilotWaitTimeoutSeconds int
	GatesCopilotThreadReplies      bool

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
	SessionSecrets SessionSecretRegistry

	// LLMProxy holds each run's real LLM key behind a stand-in token. Nil fails
	// every model run closed, so a wiring slip can never stage the real key.
	LLMProxy LLMProxy

	// RunInputs stages each run kind's input dir. Nil leaves /run-kind
	// unmounted, as on an agent without run kinds.
	RunInputs RunInputStore

	// Images lists the node's tagged images for GET /images. Nil disables the
	// endpoint (500 internal).
	Images webhookcore.ImageLister

	// ImageListFilters are the per-tag substring filters applied to GET
	// /images responses. The serve layer always supplies at least the family
	// default; an empty slice yields an empty list, never "everything".
	ImageListFilters []string

	// Build is copied into every /health answer.
	Build protocol.BuildInfo

	// ImageLabels reads LaunchEnv.BaseImage's revision label for /health; nil
	// leaves worker_image_revision out.
	ImageLabels webhookcore.ImageLabeler

	LaunchEnv LaunchEnv

	// NextAttempt returns the ordinal of the run about to start for a card: 1
	// the first time, 2 for a container that replaces it, and so on. It travels
	// to the worker as CMX_ATTEMPT and is stamped on every transcript line,
	// which is the only thing that separates a restarted run's events from the
	// earlier ones - the event sequence itself restarts at 1. Nil reads as
	// "first attempt", so a server built without one still launches.
	NextAttempt func(project, cardID string) int

	Replay *webhookcore.ReplayCache
	Dedup  *webhookcore.DedupCache

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
	llmProxy       LLMProxy
	runInputs      RunInputStore

	launchEnv LaunchEnv

	// nextAttempt counts the card's prior container runs. Nil is the first
	// attempt.
	nextAttempt func(project, cardID string) int

	dedup *webhookcore.DedupCache

	logger *slog.Logger

	// stdinMu serializes control-frame writes to each run's stdin. The executor
	// documents Run.Stdin as single-writer; webhook handlers run on independent
	// HTTP goroutines, so a per-run mutex keeps frame bytes from interleaving on
	// the wire. Keyed by project+"/"+cardID. Given the low message volume a
	// per-key lock is cheaper than a global one under concurrency.
	stdinMu sync.Map // map[string]*sync.Mutex

	// launchMu serializes per-run admission (credential provisioning + executor
	// launch). /trigger has no request dedup, so two concurrent triggers for one
	// run can both reach launch; without serialization the loser's provisioning
	// and launch-failure teardown interleave with - and clobber - the winner's
	// per-run credentials. Keyed by executor.RunKey; entries are never
	// reclaimed (one bare mutex per run key ever seen).
	launchMu sync.Map // map[executor.RunKey]*sync.Mutex

	// afterLaunch, when set, runs between a successful launch and the
	// admission's stop check: a test synchronization hook, never set in
	// production.
	afterLaunch func()

	// admissionsMu guards admissions and each admission's stopped flag. It is a
	// leaf lock, taken under launchMu but never the other way round.
	admissionsMu sync.Mutex
	admissions   map[*admission]struct{}

	// sessionRegistry manages per-session secrets registered with the log bridge
	// redactor. Nil-tolerant: a nil value makes all calls a no-op.
	sessionRegistry SessionSecretRegistry
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
		Build:             cfg.Build,
		// Only the default image is inspected, never a payload's worker_image.
		WorkerImageRevision: webhookcore.ImageRevisionHook(cfg.ImageLabels, cfg.LaunchEnv.BaseImage),
	}
	if cfg.Metrics != nil {
		coreCfg.Metrics = cfg.Metrics.Metrics
	}

	dedup := cfg.Dedup
	if dedup == nil {
		dedup = webhookcore.NewDedupCache(10*time.Minute, 4096)
	}

	return &Server{
		Core:            webhookcore.NewCore(coreCfg),
		maxConcurrent:   cfg.MaxConcurrent,
		executor:        cfg.Executor,
		tracker:         cfg.Tracker,
		hub:             cfg.Hub,
		reporter:        cfg.Reporter,
		verifier:        cfg.Verifier,
		skillsResolver:  cfg.SkillsResolver,
		credentials:     cfg.Credentials,
		llmProxy:        cfg.LLMProxy,
		runInputs:       cfg.RunInputs,
		sessionRegistry: cfg.SessionSecrets,
		launchEnv:       cfg.LaunchEnv,
		nextAttempt:     cfg.NextAttempt,
		dedup:           dedup,
		logger:          logger,
		admissions:      make(map[*admission]struct{}),
	}
}

// Routes returns the mux with every webhook route mounted. The mutating
// lifecycle routes are gated on drain; /kill, /stop-all, /logs, /containers,
// /images, /health, /readyz stay reachable during shutdown so operators can
// read state and stop work. The LLM proxy sits outside Auth and DrainGate: it
// authenticates workers by their run token, and workers parking during a drain
// still need it. The transport middlewares and the health/readiness/images/logs
// handlers are promoted from the embedded Core.
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

	if s.runInputs != nil {
		mux.HandleFunc("POST /run-kind", s.RecordMetrics(s.Auth(s.DrainGate(s.handleRunKind))))
	}

	if s.llmProxy != nil {
		mux.HandleFunc(llmproxy.DefaultPrefix+"/", s.RecordMetrics(s.llmProxy.ServeHTTP))
	}

	return mux
}

// stdinLock returns the per-run mutex for project/cardID, creating it on first
// use. Callers Lock/Unlock around frames.Write to honour Run.Stdin's
// single-writer contract. Entries are deliberately never reclaimed: one bare
// mutex per card ever seen is a tiny, process-bounded footprint, and skipping
// deletion avoids delete/recreate races with in-flight writers.
func (s *Server) stdinLock(project, cardID string) *sync.Mutex {
	v, _ := s.stdinMu.LoadOrStore(cardKey(project, cardID), &sync.Mutex{})

	return v.(*sync.Mutex)
}

// launchLock returns the per-run admission mutex, creating it on first use.
// See the launchMu field comment for why launch serializes per run.
func (s *Server) launchLock(k executor.RunKey) *sync.Mutex {
	v, _ := s.launchMu.LoadOrStore(k, &sync.Mutex{})

	return v.(*sync.Mutex)
}

// cardKey keys the per-card locks and the message dedup scope. CM-validated
// project names and card IDs carry no NUL, which the dedup cache requires.
func cardKey(project, cardID string) string {
	return project + "/" + cardID
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

	spec := s.buildLaunchSpec(payload, requestRunID(r, payload.CardID), skillsDir)

	// Registered before the 202 so a /kill that follows it always finds the
	// admission. Detached from the request: the launch outlives it.
	adm, ctx := s.beginAdmission(context.WithoutCancel(r.Context()), spec.Key())

	// Respond 202 first, then launch asynchronously: /trigger must return fast,
	// and the eventual outcome is carried by the running/failed status callback.
	webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})

	go s.launch(ctx, adm, s.cardLaunchRequest(payload, spec)) //nolint:gosec // G118: the launch outlives the request on its admission context, detached from the request and bounded by the launch timeout
}

// SessionID composes the id AddSessionKey/RemoveSessionKey register secrets
// under: project, card ID, and correlationID together. Folding in the
// correlation id (rather than just project/cardID) is what makes a re-trigger
// of the same card a structurally different session bucket from the run it
// replaced - a stale run's RemoveSessionKey then cannot reach a fresh run's
// keys no matter how the two overlap in time. Exported so internal/cli/serve.go
// (which wires the OnTokenRefresh and container-exit paths against the same
// registry) composes ids through this single definition instead of a second,
// independently-maintained copy that could drift from it.
func SessionID(project, cardID, correlationID string) string {
	return project + "/" + cardID + "/" + correlationID
}

// requestRunID is mintRunID(base) plus the X-Correlation-ID header: the
// minted part keeps every run's per-run keying unique whatever a caller sends.
func requestRunID(r *http.Request, base string) string {
	id := mintRunID(base)
	if supplied := r.Header.Get(correlationHeader); supplied != "" {
		id += "-" + supplied
	}

	return id
}

// mintRunID appends a short random suffix to base, so every run gets a fresh
// id; crypto/rand.Text cannot fail.
func mintRunID(base string) string {
	return base + "-" + strings.ToLower(rand.Text()[:8])
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

	if p.MaxCapability {
		env = append(env, "CM_MAX_CAPABILITY=true")
	}

	if p.POC {
		env = append(env, "CM_POC=true")
	}

	// Emitted only when set: an older worker image ignores the pair and fails
	// the clone of a missing base.
	if p.CreateBaseBranch {
		env = append(env, "CM_CREATE_BASE_BRANCH=true", "CM_BASE_BRANCH_FROM="+p.BaseBranchFrom)
	}

	// A container that replaces a dead one writes into the same per-card log,
	// with an event sequence that starts over at 1. The ordinal is what tells
	// the two apart. The first attempt is left unmarked in the container env:
	// the worker reads an absent value as 1. The spec carries the ordinal
	// either way, because the host stamps its own terminal event with it.
	ordinal := 1
	if s.nextAttempt != nil {
		ordinal = s.nextAttempt(p.Project, p.CardID)
	}

	if ordinal > 1 {
		env = append(env, "CMX_ATTEMPT="+strconv.Itoa(ordinal))
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

	// The run's credentials (git token + LLM proxy values) travel via a per-run
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

	env = append(env, s.workerKnobEnv()...)

	if s.launchEnv.ReplayReasoning {
		env = append(env, "CMX_REPLAY_REASONING=true")
	}

	if s.launchEnv.MaxCardCost != 0 {
		env = append(env, "CMX_MAX_CARD_COST="+formatFloat(s.launchEnv.MaxCardCost))
	}

	if s.launchEnv.CompactionEnabled {
		env = append(
			env,
			"CMX_COMPACTION_ENABLED=true",
			"CMX_COMPACTION_THRESHOLD="+formatFloat(s.launchEnv.CompactionThreshold),
			"CMX_COMPACTION_KEEP_RECENT_TURNS="+strconv.Itoa(s.launchEnv.CompactionKeepRecentTurns),
		)
	}

	if s.launchEnv.ReviewAttemptsCap > 0 {
		env = append(env, "CMX_REVIEW_ATTEMPTS_CAP="+strconv.Itoa(s.launchEnv.ReviewAttemptsCap))
	}

	if s.launchEnv.GatesPollIntervalSeconds > 0 {
		env = append(env, "CMX_GATES_POLL_INTERVAL_SECONDS="+strconv.Itoa(s.launchEnv.GatesPollIntervalSeconds))
	}

	if s.launchEnv.GatesCIWaitTimeoutSeconds > 0 {
		env = append(env, "CMX_GATES_CI_WAIT_TIMEOUT_SECONDS="+strconv.Itoa(s.launchEnv.GatesCIWaitTimeoutSeconds))
	}

	if s.launchEnv.GatesCopilotWaitTimeoutSeconds > 0 {
		env = append(env, "CMX_GATES_COPILOT_WAIT_TIMEOUT_SECONDS="+strconv.Itoa(s.launchEnv.GatesCopilotWaitTimeoutSeconds))
	}

	env = append(env, "CMX_GATES_COPILOT_THREAD_REPLIES="+strconv.FormatBool(s.launchEnv.GatesCopilotThreadReplies))

	env = append(env, s.selectionEnv(executor.RunKey{Project: p.Project, CardID: p.CardID}, p.Selection)...)

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
		Attempt:        ordinal,
	}
}

// selectionEnv renders a run's selection context as CMX_SELECTION. A nil one,
// or one that fails to encode, yields none and the worker uses its default model.
func (s *Server) selectionEnv(k executor.RunKey, sel *protocol.SelectionContext) []string {
	if sel == nil {
		return nil
	}

	b, err := json.Marshal(sel)
	if err != nil {
		s.logger.Warn("failed to marshal selection context; container will use default model",
			"project", k.Project, "card_id", k.CardID, "kind", k.Kind, "error", err)

		return nil
	}

	return []string{"CMX_SELECTION=" + string(b)}
}

// workerKnobEnv is the CMX_* knobs card runs and run kinds both take. Zero and
// empty values are omitted so the worker applies its own defaults.
func (s *Server) workerKnobEnv() []string {
	var env []string

	if s.launchEnv.DefaultModel != "" {
		env = append(env, "CMX_DEFAULT_MODEL="+s.launchEnv.DefaultModel)
	}

	if s.launchEnv.ReasoningEffort != "" {
		env = append(env, "CMX_REASONING_EFFORT="+s.launchEnv.ReasoningEffort)
	}

	if s.launchEnv.BashTimeoutMaxSeconds > 0 {
		env = append(env, "CMX_BASH_TIMEOUT_MAX_SECONDS="+strconv.Itoa(s.launchEnv.BashTimeoutMaxSeconds))
	}

	if s.launchEnv.ToolOutputMaxBytes > 0 {
		env = append(env, "CMX_TOOL_OUTPUT_MAX_BYTES="+strconv.Itoa(s.launchEnv.ToolOutputMaxBytes))
	}

	if s.launchEnv.ContainerTimeoutSeconds > 0 {
		env = append(env, "CMX_CONTAINER_TIMEOUT_SECONDS="+strconv.Itoa(s.launchEnv.ContainerTimeoutSeconds))
	}

	return env
}

// ---- kill -------------------------------------------------------------------

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	var payload protocol.KillPayload
	if !webhookcore.Decode(w, r, &payload) {
		return
	}

	// An empty kind is the card run only; the key compares the kind too, so a
	// run kind's admission and container are never touched by it.
	k := executor.RunKey{Project: payload.Project, CardID: payload.CardID, Kind: payload.Kind}

	s.stopAdmissions(r.Context(), func(adm executor.RunKey) bool {
		return adm == k
	})

	// Idempotent: ContextMatrix reads any status >= 400 as a failed stop, and a
	// card with no tracked container is already stopped.
	if err := s.executor.Kill(r.Context(), k); err != nil &&
		!errors.Is(err, executor.ErrNotFound) {
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

	// Detached so a caller that gives up mid-stop does not cut the kills short.
	// One killTimeout covers two steps in turn, each about one kill grace: the
	// wait for stopped admissions, one of which may be killing a run it had
	// just launched, then the kills of the tracked runs.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), killTimeout)
	defer cancel()

	// An empty project stops every project, as StopAll reads it.
	s.stopAdmissions(ctx, func(k executor.RunKey) bool {
		return payload.Project == "" || k.Project == payload.Project
	})

	stopResults, err := s.executor.StopAll(ctx, payload.Project)
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

	run, ok := s.tracker.Get(executor.RunKey{Project: payload.Project, CardID: payload.CardID})
	if !ok {
		webhookcore.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

		return
	}

	// One per-card lock spans the check-and-record, the write and the rollback.
	// A retry arriving mid-write waits for it, then either finds the delivered
	// record (duplicate ack) or, after a failed write rolled it back, delivers.
	scope := cardKey(payload.Project, payload.CardID)
	mu := s.stdinLock(payload.Project, payload.CardID)
	mu.Lock()

	if s.dedup.CheckAndRecord(scope, payload.MessageID) {
		mu.Unlock()

		webhookcore.WriteJSON(w, http.StatusOK, protocol.SuccessResponse{
			OK:        true,
			Message:   "duplicate message acknowledged",
			MessageID: payload.MessageID,
		})

		return
	}

	err := frames.Write(run.Stdin, frames.Frame{
		Type:      frames.TypeUserMessage,
		Content:   payload.Content,
		MessageID: payload.MessageID,
	})
	if err != nil {
		s.dedup.Rollback(scope, payload.MessageID)
	}

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

	publishUser(s.hub, payload.Project, payload.CardID, payload.Content)
	k := executor.RunKey{Project: payload.Project, CardID: payload.CardID}
	s.tracker.SetAwaiting(k, false)
	s.tracker.Touch(k)

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

	run, ok := s.tracker.Get(executor.RunKey{Project: payload.Project, CardID: payload.CardID})
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

	if err := s.EndSession(payload.Project, payload.CardID); err != nil {
		if errors.Is(err, executor.ErrNotFound) {
			// Idempotent: nothing to end is a success, not a 404.
			webhookcore.WriteJSON(w, http.StatusOK, protocol.SuccessResponse{OK: true})

			return
		}

		s.logger.Error("end-session stdin write failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "write failed")

		return
	}

	webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})
}

// EndSession writes an end_session frame to the card's tracked run under the
// per-card stdin lock; the worker then pushes its WIP, releases the card and
// exits. Serve shutdown shares this path with /end-session. It returns
// executor.ErrNotFound when no run is tracked.
func (s *Server) EndSession(project, cardID string) error {
	run, ok := s.tracker.Get(executor.RunKey{Project: project, CardID: cardID})
	if !ok {
		return fmt.Errorf("end session: %w: %s/%s", executor.ErrNotFound, project, cardID)
	}

	mu := s.stdinLock(project, cardID)
	mu.Lock()
	defer mu.Unlock()

	if err := frames.Write(run.Stdin, frames.Frame{Type: frames.TypeEndSession}); err != nil {
		return fmt.Errorf("write end_session frame: %w", err)
	}

	return nil
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
			Kind:        run.Kind,
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
