package tools

// ReadOnlyTools returns the read-only tool set for a workspace root: read, grep,
// glob, and the read-only git tool — no write/edit/bash. Safe for parallel
// subagent fan-out over a shared workspace.
func ReadOnlyTools(root string) []Tool {
	return []Tool{
		NewReadTool(root),
		NewGrepTool(root),
		NewGlobTool(root),
		NewGitTool(root),
	}
}

// NewReadOnlyRegistry builds a Registry containing only ReadOnlyTools(root).
func NewReadOnlyRegistry(root string) *Registry {
	return NewRegistry(ReadOnlyTools(root)...)
}
