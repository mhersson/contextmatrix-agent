package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/runinput"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// RunInputStore owns each run kind's input dir on the host. *runinput.Store
// satisfies it; Remove is idempotent.
type RunInputStore interface {
	Create(runID string) (string, error)
	Remove(runID string)
}

// handleRunKind admits a sandboxed run kind for one card: validate, refuse at
// capacity, then 202 and an asynchronous launch, as /trigger does.
func (s *Server) handleRunKind(w http.ResponseWriter, r *http.Request) {
	var p protocol.RunKindPayload
	if !webhookcore.Decode(w, r, &p) {
		return
	}

	if field := runKindInvalidField(p); field != "" {
		s.logger.Warn("run kind rejected: invalid field",
			"project", p.Project, "card_id", p.CardID, "kind", p.Kind, "field", field)
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "invalid "+field)

		return
	}

	if s.maxConcurrent > 0 && s.tracker.Count() >= s.maxConcurrent {
		s.logger.Warn("run kind rejected: at capacity",
			"project", p.Project, "card_id", p.CardID, "kind", p.Kind, "limit", s.maxConcurrent)
		webhookcore.WriteError(w, http.StatusTooManyRequests, protocol.CodeLimitReached, "concurrency limit reached")

		return
	}

	spec := s.buildRunKindSpec(p, requestRunID(r, p.CardID+"-"+p.Kind))

	// Registered before the 202 so a kind-scoped /kill that follows it always
	// finds the admission. Detached from the request: the launch outlives it.
	adm, ctx := s.beginAdmission(context.WithoutCancel(r.Context()), spec.Key())

	webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})

	go s.launchRunKind(ctx, adm, p, spec) //nolint:gosec // G118: the launch outlives the request on its admission context, detached from the request and bounded by the launch timeout
}

// launchRunKind stages the run's inputs, then admits and launches it through
// the shared launch path. A run kind sends no running status: a launched one
// reports only at its exit, which also removes its input dir. A run that does
// not launch has its dir removed here and, unless a stop reached it, sends its
// one failed status.
func (s *Server) launchRunKind(ctx context.Context, adm *admission, p protocol.RunKindPayload, spec executor.LaunchSpec) {
	defer adm.cancel()

	req := launchRequest{
		Key:         spec.Key(),
		Spec:        spec,
		GitToken:    p.GitToken,
		LLM:         p.LLMEndpoint,
		LLMRequired: usesModel(p.Kind),
	}

	failure, stopped := s.admitRunKind(ctx, adm, req, runKindInput(p))
	close(adm.done)

	if failure.failed() || stopped {
		s.runInputs.Remove(spec.CorrelationID)
	}

	if failure.failed() {
		s.reportFailure(req, failure)
	}
}

// admitRunKind stages the run's input dir and admits the run with it mounted.
// It returns as admitAndLaunch does.
func (s *Server) admitRunKind(ctx context.Context, adm *admission, req launchRequest, input any) (launchFailure, bool) {
	dir, err := s.stageRunInput(req.Spec.CorrelationID, input)
	if err != nil {
		s.logger.Error("run input staging failed",
			"project", req.Key.Project, "card_id", req.Key.CardID, "kind", req.Key.Kind, "error", err)

		if s.endAdmission(adm) {
			return launchFailure{}, true
		}

		return infraFailure("run input staging failed"), false
	}

	req.Spec.InputHostDir = dir

	return s.admitAndLaunch(ctx, adm, req)
}

// stageRunInput creates the run's input dir and writes the kind's input into
// it.
func (s *Server) stageRunInput(runID string, input any) (string, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode run input: %w", err)
	}

	dir, err := s.runInputs.Create(runID)
	if err != nil {
		return "", fmt.Errorf("create run input dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, runinput.InputFile), b, 0o600); err != nil {
		return "", fmt.Errorf("write run input: %w", err)
	}

	return dir, nil
}

// runKindInput is the input the payload's kind runs on.
func runKindInput(p protocol.RunKindPayload) any {
	if p.Kind == protocol.RunKindCheck {
		return p.Check
	}

	return p.Investigate
}
