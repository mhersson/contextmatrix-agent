package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

type GlobTool struct{ root string }

func NewGlobTool(root string) GlobTool { return GlobTool{root: root} }

func (t GlobTool) Name() string { return "glob" }

func (t GlobTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        "glob",
		Description: "List files matching a glob pattern (e.g. *.go), honoring .gitignore. Optionally restrict to a subpath.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"pattern":{"type":"string","description":"glob pattern, e.g. *.go or *_test.go"},
				"path":{"type":"string","description":"optional subpath under the workspace root to search"}
			},
			"required":["pattern"]
		}`),
	}}
}

func (t GlobTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	pattern, err := requireString(args, "pattern")
	if err != nil {
		return "", err
	}
	searchPath := t.root
	if rel := optString(args, "path", ""); rel != "" {
		abs, err := resolveInRoot(t.root, rel)
		if err != nil {
			return "", err
		}
		searchPath = abs
	}

	var cmd *exec.Cmd
	if bin := fdBinary(); bin != "" {
		cmd = exec.CommandContext(ctx, bin, "--glob", "--type", "f", pattern, searchPath)
	} else if _, lookErr := exec.LookPath("rg"); lookErr == nil {
		cmd = exec.CommandContext(ctx, "rg", "--files", "--glob", pattern, searchPath)
	} else {
		return "", fmt.Errorf("glob requires fd or rg on PATH")
	}
	cmd.Dir = t.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		// rg exits 1 when no files match; fd exits 0 with empty output.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return "no matches", nil
		}
		return "", fmt.Errorf("glob failed: %v: %s", err, string(out))
	}
	return formatGlob(string(out), t.root), nil
}

func formatGlob(out, root string) string {
	s := strings.TrimSpace(strings.ReplaceAll(out, root+"/", ""))
	if s == "" {
		return "no matches"
	}
	return s
}

// fdBinary returns the fd executable name on PATH ("fd" or Debian's "fdfind"),
// or "" if neither is present.
func fdBinary() string {
	for _, name := range []string{"fd", "fdfind"} {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}
