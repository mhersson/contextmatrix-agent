package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/filelog"
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

// SectionSource copies one run's section of a card's durable log.
// *filelog.Logger satisfies it.
type SectionSource interface {
	CopySection(project, cardID, runID string, w io.Writer) error
}

// handleRunKind admits a sandboxed run kind, for a card or a project: validate, refuse at
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

	spec := s.buildRunKindSpec(p, requestRunID(r, runKindRunBase(p)))

	// Registered before the 202 so a kind-scoped /kill that follows it always
	// finds the admission. Detached from the request: the launch outlives it.
	adm, ctx := s.beginAdmission(context.WithoutCancel(r.Context()), spec.Key())

	webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})

	go s.launchRunKind(ctx, adm, p, spec) //nolint:gosec // G118: the launch outlives the request on its admission context, detached from the request and bounded by the launch timeout
}

// runKindRunBase is the run ID base: the card and kind, or the project and kind
// for a project-level kind that has no card.
func runKindRunBase(p protocol.RunKindPayload) string {
	if p.CardID == "" {
		return p.Project + "-" + p.Kind
	}

	return p.CardID + "-" + p.Kind
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

	failure, stopped := s.admitRunKind(ctx, adm, req, p)
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
func (s *Server) admitRunKind(ctx context.Context, adm *admission, req launchRequest, p protocol.RunKindPayload) (launchFailure, bool) {
	dir, failure := s.stageRunKind(req, p)
	if failure.failed() {
		s.logger.Error("run input staging failed",
			"project", req.Key.Project, "card_id", req.Key.CardID, "kind", req.Key.Kind, "reason", failure.Message)

		if s.endAdmission(adm) {
			return launchFailure{}, true
		}

		return failure, false
	}

	req.Spec.InputHostDir = dir

	return s.admitAndLaunch(ctx, adm, req)
}

// stageRunKind writes the kind's input into the run's input dir, and for
// investigate and analyse the stopped run's log section beside it.
func (s *Server) stageRunKind(req launchRequest, p protocol.RunKindPayload) (string, launchFailure) {
	runID := req.Spec.CorrelationID

	dir, err := s.stageRunInput(runID, runKindInput(p))
	if err != nil {
		s.logger.Error("stage run input", "run_id", runID, "error", err)

		return "", infraFailure("run input staging failed")
	}

	if !usesModel(p.Kind) {
		return dir, launchFailure{}
	}

	switch err := s.stageRunLog(dir, p); {
	case errors.Is(err, filelog.ErrNoSection):
		return "", launchFailure{Message: "run log not found", Cause: protocol.StopCauseError}
	case err != nil:
		s.logger.Error("stage run log", "run_id", runID, "error", err)

		return "", infraFailure("run log staging failed")
	}

	return dir, launchFailure{}
}

// stageRunLog streams the stopped run's whole log section into run.log in dir.
func (s *Server) stageRunLog(dir string, p protocol.RunKindPayload) (err error) {
	if s.sections == nil {
		return errors.New("no section source")
	}

	f, err := os.OpenFile(filepath.Join(dir, runinput.RunLogFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create run log: %w", err)
	}

	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close run log: %w", cerr)
		}
	}()

	return s.sections.CopySection(p.Project, p.CardID, runLogID(p), f)
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
	switch p.Kind {
	case protocol.RunKindCheck:
		return p.Check
	case protocol.RunKindAnalyse:
		return p.Analyse
	case protocol.RunKindGuidancePR:
		return p.GuidancePR
	}

	return p.Investigate
}
