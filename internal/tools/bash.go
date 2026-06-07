package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"syscall"
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

	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = t.root
	// New process group so we can signal the whole tree (the child is the
	// group leader: pgid == child pid). Plain ctx-cancel leaves grandchildren.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start command: %w", err)
	}
	pgid := cmd.Process.Pid

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()

	select {
	case <-timer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck
		<-done
		return buf.String() + fmt.Sprintf("\n[command timed out after %ds]", timeout), nil
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck
		<-done
		return buf.String(), ctx.Err()
	case werr := <-done:
		res := buf.String()
		if werr != nil {
			res += fmt.Sprintf("\n[command exited with error: %v]", werr)
		}
		return res, nil
	}
}
