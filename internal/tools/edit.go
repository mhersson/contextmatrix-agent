package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

type EditTool struct{ root string }

func NewEditTool(root string) EditTool { return EditTool{root: root} }

func (t EditTool) Name() string { return "edit" }

func (t EditTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        "edit",
		Description: "Replace an exact substring in a file. Fails if old_string is absent, or appears more than once unless replace_all is true.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"file path relative to the workspace root"},
				"old_string":{"type":"string","description":"exact text to replace"},
				"new_string":{"type":"string","description":"replacement text"},
				"replace_all":{"type":"boolean","description":"optional; replace every occurrence"}
			},
			"required":["path","old_string","new_string"]
		}`),
	}}
}

func (t EditTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	rel, err := requireString(args, "path")
	if err != nil {
		return "", err
	}
	oldStr, err := requireString(args, "old_string")
	if err != nil {
		return "", err
	}
	newStr, err := requireString(args, "new_string")
	if err != nil {
		return "", err
	}
	replaceAll := optBool(args, "replace_all", false)

	abs, err := resolveInRoot(t.root, rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	content := string(b)
	n := strings.Count(content, oldStr)
	if n == 0 {
		return "", fmt.Errorf("old_string not found in %s", rel)
	}
	if n > 1 && !replaceAll {
		return "", fmt.Errorf("old_string appears %d times in %s; set replace_all or provide a unique string", n, rel)
	}
	if replaceAll {
		content = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		content = strings.Replace(content, oldStr, newStr, 1)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", rel, n), nil
}
