package orchestrator

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// finishTool is the terminating tool the coder, document, and fix models call to
// end their run. It implements tools.Terminal, so a successful call ends
// harness.Run with the call's arguments surfaced on Result.CompletionArgs.
type finishTool struct{}

// NewFinishTool returns the stateless finish tool for the write toolset.
func NewFinishTool() tools.Tool { return finishTool{} }

func (finishTool) Name() string { return "finish" }

func (finishTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "finish",
			Description: `Call this when your work is complete. Provide the conventional-commit message summarizing your change (e.g. "feat(api): add health endpoint"). Calling finish ends the run; make no further tool calls after it.`,
			Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"commit_message": {
			"type": "string",
			"description": "The conventional-commit message for this change."
		}
	},
	"required": ["commit_message"]
}`),
		},
	}
}

// Execute is a no-op: finish only carries the terminating signal and the commit
// message (surfaced by the harness on Result.CompletionArgs). It never fails, so
// it always terminates the run; consumers fall back to a title/default when the
// message is empty.
func (finishTool) Execute(_ context.Context, _ map[string]any) (tools.Result, error) {
	return tools.Result{Text: "done"}, nil
}

// Terminal marks finish as the run-ending tool.
func (finishTool) Terminal() bool { return true }

// finishArgs is the structured payload of a finish tool call.
type finishArgs struct {
	CommitMessage string `json:"commit_message"`
}

// finishCommitMessage returns the trimmed commit_message from a finish call's
// arguments (harness.Result.CompletionArgs), or "" when the args are absent,
// empty, or unparseable.
func finishCommitMessage(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}

	var fa finishArgs
	if err := json.Unmarshal(args, &fa); err != nil {
		return ""
	}

	return strings.TrimSpace(fa.CommitMessage)
}
