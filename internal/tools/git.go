package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

type GitTool struct{ root string }

func NewGitTool(root string) GitTool { return GitTool{root: root} }

func (t GitTool) Name() string { return "git" }

// readonlyGitSubcommands is the allowlist. Mutating git (commit/push/rebase/…)
// is a deterministic orchestrator action, never a model tool.
var readonlyGitSubcommands = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "branch": true,
}

func (t GitTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        "git",
		Description: "Run a read-only git command (status, diff, log, show, branch) in the workspace and return its output.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"subcommand":{"type":"string","enum":["status","diff","log","show","branch"],"description":"read-only git subcommand"},
				"args":{"type":"array","items":{"type":"string"},"description":"optional extra arguments, e.g. [\"--stat\"]"}
			},
			"required":["subcommand"]
		}`),
	}}
}

func (t GitTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	sub, err := requireString(args, "subcommand")
	if err != nil {
		return "", err
	}

	if !readonlyGitSubcommands[sub] {
		return "", fmt.Errorf("git subcommand %q is not allowed (read-only: status, diff, log, show, branch)", sub)
	}

	cmdArgs := append([]string{sub}, optStringSlice(args, "args")...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = t.root
	cmd.Env = ScrubbedEnv(nil)

	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return string(out), nil // surface git's own message as output, not a hard failure
		}

		return "", fmt.Errorf("git failed: %v", err)
	}

	return string(out), nil
}
