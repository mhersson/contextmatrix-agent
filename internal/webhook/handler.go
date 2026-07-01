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
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const (
	// maxRequestBodyBytes caps the body the auth middleware reads before HMAC
	// verification. ContextMatrix caps /message content well under this; a
	// larger body is a misbehaving or hostile client.
	maxRequestBodyBytes = 1 << 20 // 1 MiB

	// correlationHeader carries the client's trace ID. The agent backend reads
	// it on /trigger and threads it into the container as CM_CORRELATION_ID so
	// runner and worker logs stitch to the same CM trace.
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
	// container at /run/cm-secrets. Empty disables the mount.
	SecretsHostDir string

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
		launchEnv:         cfg.LaunchEnv,
		replay:            replay,
		dedup:             dedup,
		draining:          draining,
		keepaliveInterval: cfg.KeepaliveInterval,
		metrics:           cfg.Metrics,
		logger:            logger,
	}
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

	go s.launch(spec, payload.Project, payload.CardID) //nolint:gosec // G118: launch is fire-and-forget and outlives the request; a request-scoped context would cancel the worker
}

// launch runs on a detached context so the container's lifecycle outlives the
// HTTP request that triggered it. It reports running on success and failed on a
// launch error.
func (s *Server) launch(spec executor.LaunchSpec, project, cardID string) {
	ctx := context.Background()

	if err := s.executor.Launch(ctx, spec); err != nil {
		s.logger.Error("launch failed", "project", project, "card_id", cardID, "error", err)

		if rerr := s.reporter.ReportStatus(ctx, cardID, project, "failed", "launch failed"); rerr != nil {
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
		"CM_CORRELATION_ID=" + correlationID,
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

	if skillsDir != "" {
		env = append(env, "CMX_TASK_SKILLS_DIR="+skillsMountPathEnv)

		if p.TaskSkills != nil {
			env = append(env, "CM_TASK_SKILLS_SET=1")
			env = append(env, "CM_TASK_SKILLS="+strings.Join(*p.TaskSkills, ","))
		}
	}

	env = append(env, s.launchEnv.WorkerExtraEnv...)

	return executor.LaunchSpec{
		CardID:         p.CardID,
		Project:        p.Project,
		Image:          image,
		Env:            env,
		SecretsHostDir: s.launchEnv.SecretsHostDir,
		SkillsHostDir:  skillsDir,
		MemoryBytes:    s.launchEnv.MemoryBytes,
		PidsLimit:      s.launchEnv.PidsLimit,
		CorrelationID:  correlationID,
		MCPURL:         s.launchEnv.MCPURL,
	}
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

	s.hub.PublishUser(payload.Project, payload.CardID, payload.Content)

	mu := s.stdinLock(payload.Project, payload.CardID)
	mu.Lock()
	err := frames.Write(run.Stdin, frames.Frame{
		Type:      frames.TypeUserMessage,
		Content:   payload.Content,
		MessageID: payload.MessageID,
	})
	mu.Unlock()

	if err != nil {
		s.logger.Error("message stdin write failed",
			"project", payload.Project, "card_id", payload.CardID, "error", err)
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "write failed")

		return
	}

	// Delivered: record the message_id now so a retry is deduped, then clear the
	// awaiting flag and touch the run.
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
