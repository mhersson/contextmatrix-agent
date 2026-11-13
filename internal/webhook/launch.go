package webhook

import (
	"context"
	"errors"
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// launchRequest is everything one run needs to be admitted and launched: its
// key, the resolved spec, the credentials CM provisioned, and any extra values
// the host must mask. Secrets are extra masked values beyond the credentials
// above (a card run's resolved MCP key and mob guest tokens).
type launchRequest struct {
	Key               executor.RunKey
	Spec              executor.LaunchSpec
	GitToken          string
	GitTokenExpiresAt string
	LLM               *protocol.LLMEndpoint
	MobGuests         string
	Secrets           []string
}

// launchFailure is a launch that did not happen: a client-safe message and the
// stop cause reported with it. The zero value means launched.
type launchFailure struct {
	Message string
	Cause   string
}

func (f launchFailure) failed() bool { return f.Message != "" }

// cardLaunchRequest builds the launch request for a card trigger.
func (s *Server) cardLaunchRequest(p protocol.TriggerPayload, spec executor.LaunchSpec) launchRequest {
	mcpKey := s.launchEnv.MCPAPIKey
	if p.MCPAPIKey != "" {
		mcpKey = p.MCPAPIKey
	}

	extra := []string{mcpKey}

	if p.Mob != nil {
		for _, guest := range p.Mob.Guests {
			extra = append(extra, guest.Token)
		}
	}

	return launchRequest{
		Key:               spec.Key(),
		Spec:              spec,
		GitToken:          p.GitToken,
		GitTokenExpiresAt: p.GitTokenExpiresAt,
		LLM:               p.LLMEndpoint,
		MobGuests:         s.mobGuestsJSON(p),
		Secrets:           extra,
	}
}

// launch provisions the per-run credential file, launches the container, and
// reports running on success and failed on a provisioning or launch error,
// including the launch timeout. A stopped admission reports nothing. Status
// callbacks run outside the admission lock so a slow ContextMatrix response
// cannot serialize later triggers for the card.
func (s *Server) launch(ctx context.Context, adm *admission, req launchRequest) {
	defer adm.cancel()

	project, cardID := req.Key.Project, req.Key.CardID

	failure, stopped := s.admitAndLaunch(ctx, adm, req)
	close(adm.done)

	if stopped {
		return
	}

	// ctx may have expired; the callback client bounds its own requests.
	status := protocol.StatusCallbackPayload{CardID: cardID, Project: project, RunID: req.Spec.CorrelationID}

	if failure.failed() {
		status.WorkerStatus = "failed"
		status.Message = failure.Message
		status.StopCause = failure.Cause

		if err := s.reporter.ReportStatus(context.Background(), status); err != nil {
			s.logger.Error("report failed-status callback failed",
				"project", project, "card_id", cardID, "error", err)
		}

		return
	}

	status.WorkerStatus = "running"

	if err := s.reporter.ReportStatus(context.Background(), status); err != nil {
		s.logger.Error("report running-status callback failed",
			"project", project, "card_id", cardID, "error", err)
	}
}

// admission is one trigger's launch in flight, from before its 202 until
// admitAndLaunch returns. A /kill or /stop-all that finds it sets stopped and
// cancels its context; the launch then aborts, or kills the run it has just
// launched, and reports nothing. ContextMatrix records the card as killed once
// the stop answers and writes any later status over that, so neither running
// nor a failure may follow.
type admission struct {
	key    executor.RunKey
	cancel context.CancelFunc

	// stopped is guarded by Server.admissionsMu.
	stopped bool

	// done is closed once admitAndLaunch has returned.
	done chan struct{}
}

// beginAdmission registers a trigger's admission and returns its context,
// which the launch timeout and a stop both end.
func (s *Server) beginAdmission(parent context.Context, k executor.RunKey) (*admission, context.Context) {
	ctx, cancel := context.WithTimeout(parent, launchTimeout)
	adm := &admission{key: k, cancel: cancel, done: make(chan struct{})}

	s.admissionsMu.Lock()
	s.admissions[adm] = struct{}{}
	s.admissionsMu.Unlock()

	return adm, ctx
}

// endAdmission deregisters adm and reports whether a stop reached it. The flag
// is read under the lock a stop sets it under, so a stop either lands here or
// finds no admission and kills the tracked run instead.
func (s *Server) endAdmission(adm *admission) bool {
	s.admissionsMu.Lock()
	defer s.admissionsMu.Unlock()

	delete(s.admissions, adm)

	return adm.stopped
}

// stopAdmissions stops every admission in flight that match selects, then
// waits, bounded by ctx, for each to return, so none launches or reports
// after the caller has killed the card's tracked runs and answered.
func (s *Server) stopAdmissions(ctx context.Context, match func(executor.RunKey) bool) {
	var stopping []*admission

	s.admissionsMu.Lock()

	for adm := range s.admissions {
		if match(adm.key) {
			adm.stopped = true
			adm.cancel()
			stopping = append(stopping, adm)
		}
	}

	s.admissionsMu.Unlock()

	for _, adm := range stopping {
		select {
		case <-adm.done:
		case <-ctx.Done():
			s.logger.Warn("stopped admission still running when the stop answered",
				"project", adm.key.Project, "card_id", adm.key.CardID)

			return
		}
	}
}

// admitAndLaunch provisions the per-run credentials and launches the container,
// serialized per card. Returns the zero launchFailure on success or a client-safe failure;
// stopped reports that a stop reached the admission, whose launched run it then
// kills, and the caller reports nothing.
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
// endpoint. A server without a usable LLM proxy fails closed the same way: the
// worker gets a per-run proxy token, never the real key, and a run that does
// not launch has its token revoked. A claude_code run is the exception: its
// worker is staged the subscription token.
func (s *Server) admitAndLaunch(ctx context.Context, adm *admission, req launchRequest) (failure launchFailure, stopped bool) {
	mu := s.launchLock(req.Key)
	mu.Lock()
	defer mu.Unlock()

	failure = s.provisionAndLaunch(ctx, req)

	if !failure.failed() && s.afterLaunch != nil {
		s.afterLaunch()
	}

	if !s.endAdmission(adm) {
		return failure, false
	}

	if !failure.failed() {
		s.stopLaunchedRun(req)
	}

	s.logger.Info("launch stopped", "project", req.Key.Project, "card_id", req.Key.CardID)

	return launchFailure{}, true
}

// stopLaunchedRun kills a run whose admission was stopped after the executor
// had launched it. Its token is revoked first so it cannot reach the LLM
// during the kill grace; its session secrets stay registered until its exit
// path removes them after the output drains, as for any run.
func (s *Server) stopLaunchedRun(req launchRequest) {
	s.llmProxy.Revoke(req.Spec.CorrelationID)

	if s.credentials != nil {
		s.credentials.Teardown(req.Key.Project, req.Key.Name(), req.Spec.CorrelationID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()

	if err := s.executor.Kill(ctx, req.Key); err != nil && !errors.Is(err, executor.ErrNotFound) {
		s.logger.Error("kill of a stopped launch failed",
			"project", req.Key.Project, "card_id", req.Key.CardID, "error", err)
	}
}

// provisionAndLaunch is admitAndLaunch's admission: the guards, the per-run
// provisioning and the launch. The caller holds the card's admission lock.
func (s *Server) provisionAndLaunch(ctx context.Context, req launchRequest) launchFailure {
	project, cardID := req.Key.Project, req.Key.CardID
	spec := req.Spec

	if _, tracked := s.tracker.Get(req.Key); tracked {
		s.logger.Warn("trigger rejected: card already running",
			"project", project, "card_id", cardID)

		return infraFailure("card already running")
	}

	if req.GitToken == "" {
		s.logger.Warn("trigger rejected: no git credential source",
			"project", project, "card_id", cardID)

		return infraFailure("CM did not provision a git token")
	}

	if req.LLM == nil {
		s.logger.Warn("trigger rejected: no llm_endpoint credential source",
			"project", project, "card_id", cardID)

		return infraFailure("CM did not provision an llm_endpoint")
	}

	claude := req.LLM.Type == protocol.LLMEndpointTypeClaudeCode

	if claude && req.LLM.OAuthToken == "" {
		s.logger.Warn("trigger rejected: claude_code endpoint without an oauth token",
			"project", project, "card_id", cardID)

		return infraFailure("CM did not provision a Claude Code oauth token")
	}

	if s.llmProxy == nil {
		s.logger.Error("trigger rejected: LLM proxy not configured",
			"project", project, "card_id", cardID)

		return infraFailure("LLM proxy not configured")
	}

	// The timeout or a stop can land while the admission waits for the lock.
	if ctx.Err() != nil {
		return s.contextFailure(ctx)
	}

	// A claude_code run is never proxied: the claude binary holds the
	// subscription token itself, so Register is skipped and the token stays "".
	var (
		token string
		err   error
	)

	if !claude {
		token, err = s.llmProxy.Register(spec.CorrelationID, *req.LLM)
		if err != nil {
			s.logger.Error("LLM proxy registration failed",
				"project", project, "card_id", cardID, "error", err)

			return infraFailure("LLM proxy registration failed")
		}
	}

	provisioned := false

	// The fail-closed guard above already rejected empty-GitToken payloads.
	if s.credentials != nil {
		staged := protocol.LLMEndpoint{Type: req.LLM.Type, BaseURL: s.launchEnv.LLMBaseURL, APIKey: token}
		if claude {
			staged = protocol.LLMEndpoint{Type: req.LLM.Type, OAuthToken: req.LLM.OAuthToken, Effort: req.LLM.Effort}
		}

		if err := s.credentials.Provision(project, req.Key.Name(), spec.CorrelationID, req.GitToken, req.GitTokenExpiresAt, s.runEndpoint(req, staged)); err != nil {
			s.llmProxy.Revoke(spec.CorrelationID)
			s.logger.Error("provision per-run credentials failed",
				"project", project, "card_id", cardID, "error", err)

			return infraFailure("credential provisioning failed")
		}

		provisioned = true
	}

	s.addSessionSecrets(req, token)

	// A stop or the timeout that landed during provisioning skips the launch
	// and takes the failure path's teardown.
	err = ctx.Err()
	if err == nil {
		err = s.executor.Launch(ctx, spec)
	}

	if err != nil {
		// Tear down only what THIS launch provisioned: a duplicate that skipped
		// provisioning must not touch the winner's credentials.
		if provisioned {
			s.credentials.Teardown(project, req.Key.Name(), spec.CorrelationID)
		}

		s.llmProxy.Revoke(spec.CorrelationID)
		s.removeSessionSecrets(project, cardID, spec.CorrelationID)

		s.logger.Error("launch failed", "project", project, "card_id", cardID, "error", err)

		return s.contextFailure(ctx)
	}

	return launchFailure{}
}

// contextFailure is the client-safe message for a launch that did not happen;
// the launch timeout names itself so a stalled pull reads as one.
func (s *Server) contextFailure(ctx context.Context) launchFailure {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return infraFailure(fmt.Sprintf("launch timed out after %s", launchTimeout))
	}

	return infraFailure("launch failed")
}

func infraFailure(message string) launchFailure {
	return launchFailure{Message: message, Cause: protocol.StopCauseInfrastructure}
}

// addSessionSecrets registers every secret a run receives in the session
// secret registry under SessionID(project, cardID, correlationID): the git
// token, the LLM proxy token, the endpoint's own keys, and the request's extra
// Secrets (a card run's resolved MCP API key and mob guest bearer tokens). In
// endpoint mode the real LLM endpoint key never reaches the container but stays
// registered as defence in depth; a claude_code worker holds the subscription
// token, which is registered too. Registration happens before Launch so no
// container output can precede it; removeSessionSecrets unregisters on
// container exit and on a failed launch, so a re-trigger never inherits stale
// keys. It is nil-safe against a nil registry and a nil endpoint, and
// AddSessionKey ignores empty values.
//
// Operator-supplied worker_extra_env is deliberately not registered. Those
// values are appended verbatim to the container environment and AGENTS.md
// already declares them readable by model-run commands and unredacted in
// transcripts, so masking only the /logs copy would be a false assurance.
// (The chat backend does register its worker_extra_env LLM_API_KEY, which is
// why the two backends differ here.)
func (s *Server) addSessionSecrets(req launchRequest, llmToken string) {
	if s.sessionRegistry == nil {
		return
	}

	id := SessionID(req.Key.Project, req.Key.CardID, req.Spec.CorrelationID)

	s.sessionRegistry.AddSessionKey(id, req.GitToken)
	s.sessionRegistry.AddSessionKey(id, llmToken)

	if req.LLM != nil {
		s.sessionRegistry.AddSessionKey(id, req.LLM.APIKey)
		s.sessionRegistry.AddSessionKey(id, req.LLM.OAuthToken)
	}

	for _, secret := range req.Secrets {
		s.sessionRegistry.AddSessionKey(id, secret)
	}
}

// removeSessionSecrets unregisters SessionID(project, cardID, correlationID)
// from the secret registry. Nil-tolerant: a nil registry makes this a no-op.
func (s *Server) removeSessionSecrets(project, cardID, correlationID string) {
	if s.sessionRegistry == nil {
		return
	}

	s.sessionRegistry.RemoveSessionKey(SessionID(project, cardID, correlationID))
}

// runEndpoint maps the LLM endpoint the worker is to call - the proxy's, never
// the payload's, except a claude_code run, which stages the subscription
// token - plus the payload's mob session guest specs, which carry
// bearer tokens and therefore ride the secrets file, into the per-run
// credential file's shape.
func (s *Server) runEndpoint(req launchRequest, llm protocol.LLMEndpoint) secrets.EndpointSecrets {
	return secrets.EndpointSecrets{
		APIKey:     llm.APIKey,
		BaseURL:    llm.BaseURL,
		Type:       llm.Type,
		OAuthToken: llm.OAuthToken,
		Effort:     llm.Effort,
		MobGuests:  req.MobGuests,
	}
}
