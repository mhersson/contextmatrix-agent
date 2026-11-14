package runkind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const reportToolName = "report_investigation"

// reportTool is the investigation's terminal tool. A call whose arguments
// decode as a protocol.InvestigateResult that reportFault accepts ends the run
// with them on Result.CompletionArgs; any other call is refused, so the model
// can correct it.
type reportTool struct{}

func (reportTool) Name() string { return reportToolName }

// Schema mirrors protocol.InvestigateResult.
func (reportTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        reportToolName,
			Description: "Report the investigation. Calling it ends the run; make no other call after it.",
			Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"diagnosis": {"type": "string", "description": "Why the run stopped, for a human to read. Names any check a verify fix drops, and why."},
		"cause": {"type": "string", "enum": ["card", "model", "environment", "ecosystem"]},
		"fix": {
			"type": "object",
			"description": "Only with cause card. Set at least one field.",
			"properties": {
				"section": {"type": "string", "description": "The ## Sentinel section's content, without its heading and with no H2 line."},
				"verify": {
					"type": "object",
					"description": "Replaces the card's verify fields whole.",
					"properties": {
						"command": {"type": "string"},
						"timeout_seconds": {"type": "integer"},
						"env": {"type": "array", "items": {"type": "string"}}
					}
				},
				"retire_subtasks": {"type": "array", "items": {"type": "string"}, "description": "IDs of unstarted subtasks among the inputs."}
			}
		},
		"forward_to": {"type": "array", "items": {"type": "string"}, "description": "IDs of sibling inputs the same fix suits."},
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"kind": {"type": "string", "enum": ["guidance", "code", "card_advice", "project_verify", "hard_card", "ecosystem"]},
					"title": {"type": "string", "description": "code and ecosystem findings only."},
					"text": {"type": "string"},
					"evidence": {"type": "string", "description": "What in the run shows it. No card IDs."}
				},
				"required": ["kind", "text", "evidence"]
			}
		}
	},
	"required": ["diagnosis", "cause"]
}`),
		},
	}
}

func (reportTool) Execute(_ context.Context, args map[string]any) (tools.Result, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return tools.Result{}, fmt.Errorf("encode report: %w", err)
	}

	if _, err := parseReport(raw); err != nil {
		return tools.Result{}, err
	}

	return tools.Result{Text: "reported"}, nil
}

func (reportTool) Terminal() bool { return true }

// parseReport decodes a report call's arguments and refuses a report
// reportFault faults; unknown fields are ignored.
func parseReport(args json.RawMessage) (protocol.InvestigateResult, error) {
	var r protocol.InvestigateResult
	if err := json.Unmarshal(args, &r); err != nil {
		return protocol.InvestigateResult{}, fmt.Errorf("parse report: %w", err)
	}

	if err := reportFault(r); err != nil {
		return protocol.InvestigateResult{}, fmt.Errorf("invalid report: %w", err)
	}

	return r, nil
}

var (
	diagnoses = []string{
		protocol.DiagnosisCard, protocol.DiagnosisModel, protocol.DiagnosisEnvironment, protocol.DiagnosisEcosystem,
	}
	findingKinds = []string{
		protocol.FindingGuidance, protocol.FindingCode, protocol.FindingCardAdvice,
		protocol.FindingProjectVerify, protocol.FindingHardCard, protocol.FindingEcosystem,
	}
)

// reportFault is the first value in r that ContextMatrix would reject with the
// whole status, or nil. The fix and forward_to are left alone: ContextMatrix
// treats a bad one as no fix.
func reportFault(r protocol.InvestigateResult) error {
	if strings.TrimSpace(r.Diagnosis) == "" {
		return errors.New("diagnosis is empty")
	}

	if !slices.Contains(diagnoses, r.Cause) {
		return fmt.Errorf("cause %q is not one of %s", r.Cause, strings.Join(diagnoses, ", "))
	}

	for i, f := range r.Findings {
		if !slices.Contains(findingKinds, f.Kind) {
			return fmt.Errorf("finding %d: kind %q is not one of %s", i+1, f.Kind, strings.Join(findingKinds, ", "))
		}

		if strings.TrimSpace(f.Text) == "" {
			return fmt.Errorf("finding %d: text is empty", i+1)
		}
	}

	return nil
}
