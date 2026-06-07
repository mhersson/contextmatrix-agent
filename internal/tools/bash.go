package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

type BashTool struct{ root string }

func NewBashTool(root string) BashTool { return BashTool{root: root} }

func (t BashTool) Name() string { return "bash" }

func (t BashTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        "bash",
		Description: "Run a shell command in the workspace root and return combined stdout+stderr. Non-zero exits are returned as output, not as a hard failure.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"command":{"type":"string","description":"the shell command to run"},
				"timeout_seconds":{"type":"integer","description":"optional timeout in seconds (default 30)"}
			},
			"required":["command"]
		}`),
	}}
}

func (t BashTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	command, err := requireString(args, "command")
	if err != nil {
		return "", err
	}
	timeout := optInt(args, "timeout_seconds", 30)

	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, "bash", "-c", command)
	cmd.Dir = t.root
	out, runErr := cmd.CombinedOutput()
	res := string(out)
	if cctx.Err() == context.DeadlineExceeded {
		res += fmt.Sprintf("\n[command timed out after %ds]", timeout)
	} else if runErr != nil {
		res += fmt.Sprintf("\n[command exited with error: %v]", runErr)
	}
	return res, nil
}
