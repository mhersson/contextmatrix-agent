package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
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

	rels, err := t.list(ctx, pattern, searchPath)
	if err != nil {
		return "", err
	}
	if len(rels) == 0 {
		return "no matches", nil
	}
	return strings.Join(rels, "\n"), nil
}

// list returns the workspace-relative paths matching pattern, preferring fd and
// falling back to rg. Both honor .gitignore (fd natively; rg via globViaRg).
func (t GlobTool) list(ctx context.Context, pattern, searchPath string) ([]string, error) {
	if bin := fdBinary(); bin != "" {
		return t.globViaFd(ctx, bin, pattern, searchPath)
	}
	if _, err := exec.LookPath("rg"); err == nil {
		return t.globViaRg(ctx, pattern, searchPath)
	}
	return nil, fmt.Errorf("glob requires fd or rg on PATH")
}

// globViaFd uses fd's native glob + .gitignore handling. fd exits 0 even when
// nothing matches, so any non-zero exit is a real error (not "no matches").
func (t GlobTool) globViaFd(ctx context.Context, bin, pattern, searchPath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, bin, "--glob", "--type", "f", pattern, searchPath)
	cmd.Dir = t.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("fd glob failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return relLines(string(out), t.root), nil
}

// globViaRg lists files with `rg --files` (which honors .gitignore — unlike
// `rg --glob`, which overrides ignore rules) and applies the glob in Go. rg
// exits 1 when it finds no files (treated as no matches); exit >= 2 is an error.
func (t GlobTool) globViaRg(ctx context.Context, pattern, searchPath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "rg", "--files", searchPath)
	cmd.Dir = t.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil // rg: no files found
		}
		return nil, fmt.Errorf("rg --files failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return filterByGlob(relLines(string(out), t.root), pattern), nil
}

// filterByGlob keeps the entries matching pattern, mirroring fd's default glob
// semantics with stdlib filepath.Match: a pattern WITHOUT a path separator
// matches the base name; a leading "**/" is stripped and base-name matched; a
// pattern WITH a separator matches the relative path. Note: filepath.Match does
// not support "**" as a recursive wildcard — it matches exactly one path
// component, so a pattern like "internal/**/*.go" matches one level deep but
// misses deeper nesting (e.g. "internal/a/b/c.go"). This is a fallback-only
// limitation; fd (the primary) handles them natively.
func filterByGlob(rels []string, pattern string) []string {
	pattern = strings.TrimPrefix(pattern, "**/")
	matchBase := !strings.Contains(pattern, "/")
	var out []string
	for _, rel := range rels {
		name := rel
		if matchBase {
			name = filepath.Base(rel)
		}
		if ok, _ := filepath.Match(pattern, name); ok {
			out = append(out, rel)
		}
	}
	return out
}

// relLines splits command output into trimmed, workspace-relative paths.
func relLines(out, root string) []string {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	var rels []string
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		rels = append(rels, strings.TrimPrefix(ln, root+"/"))
	}
	return rels
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
