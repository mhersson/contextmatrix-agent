package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

type ReadTool struct{ root string }

func NewReadTool(root string) ReadTool { return ReadTool{root: root} }

func (t ReadTool) Name() string { return "read" }

func (t ReadTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        "read",
		Description: "Read a UTF-8 text file. Optionally start at a 1-based line offset and limit the number of lines returned.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"file path relative to the workspace root"},
				"offset":{"type":"integer","description":"optional 1-based first line to return"},
				"limit":{"type":"integer","description":"optional maximum number of lines"}
			},
			"required":["path"]
		}`),
	}}
}

func (t ReadTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	rel, err := requireString(args, "path")
	if err != nil {
		return "", err
	}
	abs, err := resolveInRoot(t.root, rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	offset := optInt(args, "offset", 0)
	limit := optInt(args, "limit", 0)
	if offset <= 0 && limit <= 0 {
		return string(b), nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	start := 0
	if offset > 0 {
		start = offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return strings.Join(lines[start:end], ""), nil
}
