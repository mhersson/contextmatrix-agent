package runkind

import (
	"context"
	"errors"
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/events"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// investigateMaxTurns is the investigation's turn cap.
const investigateMaxTurns = 80

var errNoReport = errors.New("the run ended without " + reportToolName)

// investigate runs one read-only model pass over the stopped run's inputs and
// returns its report. The usage is returned on every path that ran the model.
func investigate(
	ctx context.Context,
	ws string,
	spec Spec,
	in protocol.InvestigateInput,
	d modelDeps,
	emit *events.Emitter,
) (protocol.InvestigateResult, []protocol.RunUsage, error) {
	if d.executor == nil {
		return protocol.InvestigateResult{}, nil, errNoModelAccess
	}

	branch, err := fetchWorkBranch(ctx, spec, ws)
	if err != nil {
		return protocol.InvestigateResult{}, nil, err
	}

	// The read roots must exist before the tools are built.
	if err := writeInputs(spec.InputsDir, in.Card, in.Subtasks, in.Siblings); err != nil {
		return protocol.InvestigateResult{}, nil, fmt.Errorf("write inputs: %w", err)
	}

	task := d.redact(investigatePrompt(orchestrator.Grounding(ctx, ws), ws, spec.InputsDir, spec.RunLogPath, in, branch))

	res, usage, err := runModel(ctx, ws, spec, d, emit, task, reportTool{}, investigateMaxTurns)
	if err != nil {
		return protocol.InvestigateResult{}, usage, err
	}

	if res.CompletionArgs == nil {
		return protocol.InvestigateResult{}, usage, errNoReport
	}

	r, err := parseReport(res.CompletionArgs)

	return r, usage, err
}
