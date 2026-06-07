package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

type WriteTool struct{ root string }

func NewWriteTool(root string) WriteTool { return WriteTool{root: root} }

func (t WriteTool) Name() string { return "write" }

func (t WriteTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        "write",
		Description: "Create or overwrite a file with exact content, returning a compact diff. Set create_dirs to make missing parent directories.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"file path relative to the workspace root"},
				"content":{"type":"string","description":"the full new file content"},
				"create_dirs":{"type":"boolean","description":"optional; create missing parent directories"}
			},
			"required":["path","content"]
		}`),
	}}
}

func (t WriteTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	rel, err := requireString(args, "path")
	if err != nil {
		return "", err
	}
	content, err := requireString(args, "content")
	if err != nil {
		return "", err
	}
	createDirs := optBool(args, "create_dirs", false)

	abs, err := resolveInRoot(t.root, rel)
	if err != nil {
		return "", err
	}
	old, readErr := os.ReadFile(abs)
	existed := readErr == nil

	if createDirs {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", fmt.Errorf("create parent dirs: %w", err)
		}
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", err
	}
	return summarizeWrite(rel, string(old), content, existed), nil
}

func summarizeWrite(rel, oldContent, newContent string, existed bool) string {
	if !existed {
		return fmt.Sprintf("created %s (%d lines)", rel, len(splitLines(newContent)))
	}
	if oldContent == newContent {
		return fmt.Sprintf("wrote %s (unchanged)", rel)
	}
	return fmt.Sprintf("wrote %s\n%s", rel, prefixSuffixDiff(oldContent, newContent))
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // drop the trailing empty from a final newline
	}
	return lines
}

// prefixSuffixDiff renders a compact diff by trimming common leading/trailing
// lines and showing the changed middle as -old / +new. Not a minimal-edit diff,
// but correct and concise for typical single-region edits.
func prefixSuffixDiff(oldContent, newContent string) string {
	o, n := splitLines(oldContent), splitLines(newContent)
	p := 0
	for p < len(o) && p < len(n) && o[p] == n[p] {
		p++
	}
	s := 0
	for s < len(o)-p && s < len(n)-p && o[len(o)-1-s] == n[len(n)-1-s] {
		s++
	}
	var lines []string
	for _, line := range o[p : len(o)-s] {
		lines = append(lines, "-"+line)
	}
	for _, line := range n[p : len(n)-s] {
		lines = append(lines, "+"+line)
	}
	if len(lines) == 0 {
		return "(no line-level changes)"
	}
	return strings.Join(lines, "\n")
}
