package runkind

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const analysisToolName = "report_analysis"

// analysisTool is the analysis's terminal tool. A call whose arguments decode
// as a protocol.AnalyseResult that findingFault accepts ends the run with them
// on Result.CompletionArgs; any other call is refused, so the model can
// correct it.
type analysisTool struct{}

func (analysisTool) Name() string { return analysisToolName }

// Schema mirrors protocol.AnalyseResult.
func (analysisTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        analysisToolName,
			Description: "Report the friction findings, none if the run shows none. Calling it ends the run; make no other call after it.",
			Parameters: json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"findings": %s
	},
	"required": ["findings"]
}`, findingsSchema)),
		},
	}
}

func (analysisTool) Execute(_ context.Context, args map[string]any) (tools.Result, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return tools.Result{}, fmt.Errorf("encode analysis: %w", err)
	}

	if _, err := parseAnalysis(raw); err != nil {
		return tools.Result{}, err
	}

	return tools.Result{Text: "reported"}, nil
}

func (analysisTool) Terminal() bool { return true }

// parseAnalysis decodes a report_analysis call's arguments and refuses a
// report findingFault faults; unknown fields are ignored and null findings
// become an empty list.
func parseAnalysis(args json.RawMessage) (protocol.AnalyseResult, error) {
	var r protocol.AnalyseResult
	if err := json.Unmarshal(args, &r); err != nil {
		return protocol.AnalyseResult{}, fmt.Errorf("parse analysis: %w", err)
	}

	for i, f := range r.Findings {
		if err := findingFault(i, f); err != nil {
			return protocol.AnalyseResult{}, fmt.Errorf("invalid analysis: %w", err)
		}
	}

	if r.Findings == nil {
		r.Findings = []protocol.FrictionFinding{}
	}

	return r, nil
}
