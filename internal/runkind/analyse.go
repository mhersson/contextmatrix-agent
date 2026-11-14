package runkind

import (
	"context"
	"errors"
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/events"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// analyseMaxTurns is the analysis's turn cap.
const analyseMaxTurns = 40

var errNoAnalysis = errors.New("the run ended without " + analysisToolName)

// analyse runs one read-only model pass over a finished run's inputs and
// returns its friction findings. The usage is returned on every path that ran
// the model.
func analyse(
	ctx context.Context,
	ws string,
	spec Spec,
	in protocol.AnalyseInput,
	d modelDeps,
	emit *events.Emitter,
) (protocol.AnalyseResult, []protocol.RunUsage, error) {
	if d.executor == nil {
		return protocol.AnalyseResult{}, nil, errNoModelAccess
	}

	branch, err := fetchWorkBranch(ctx, spec, ws)
	if err != nil {
		return protocol.AnalyseResult{}, nil, err
	}

	guidance, err := fetchGuidanceBranch(ctx, spec, ws)
	if err != nil {
		return protocol.AnalyseResult{}, nil, err
	}

	// The read roots must exist before the tools are built.
	if err := writeInputs(spec.InputsDir, in.Card, in.Subtasks, nil); err != nil {
		return protocol.AnalyseResult{}, nil, fmt.Errorf("write inputs: %w", err)
	}

	task := d.redact(analysePrompt(orchestrator.Grounding(ctx, ws), ws, spec.InputsDir, spec.RunLogPath, in, branch, guidance))

	res, usage, err := runModel(ctx, ws, spec, d, emit, task, analysisTool{}, analyseMaxTurns)
	if err != nil {
		return protocol.AnalyseResult{}, usage, err
	}

	if res.CompletionArgs == nil {
		return protocol.AnalyseResult{}, usage, errNoAnalysis
	}

	r, err := parseAnalysis(res.CompletionArgs)

	return r, usage, err
}
