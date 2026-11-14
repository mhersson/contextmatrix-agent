package runkind

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstructionFileTarget(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		links map[string]string
		want  string
		fail  bool
	}{
		{"regular AGENTS.md", map[string]string{"AGENTS.md": "x"}, nil, "AGENTS.md", false},
		{"only CLAUDE.md", map[string]string{"CLAUDE.md": "x"}, nil, "CLAUDE.md", false},
		{"neither", nil, nil, "AGENTS.md", false},
		{"link to a sibling", map[string]string{"CLAUDE.md": "x"}, map[string]string{"AGENTS.md": "CLAUDE.md"}, "CLAUDE.md", false},
		{"link into a directory", map[string]string{"docs/AGENTS.md": "x"}, map[string]string{"AGENTS.md": "docs/AGENTS.md"}, "docs/AGENTS.md", false},
		{"link through a dot-dot that stays inside", map[string]string{"docs/a.md": "x"}, map[string]string{"AGENTS.md": "docs/../docs/a.md"}, "docs/a.md", false},
		{"escaping link", nil, map[string]string{"AGENTS.md": "../outside.md"}, "", true},
		{"absolute link outside", nil, map[string]string{"AGENTS.md": "/etc/hostname"}, "", true},
		{"missing target", nil, map[string]string{"AGENTS.md": "nope.md"}, "", true},
		{"directory target", map[string]string{"docs/x.md": "x"}, map[string]string{"AGENTS.md": "docs"}, "", true},
		{"chained link", map[string]string{"CLAUDE.md": "x"}, map[string]string{"AGENTS.md": "link.md", "link.md": "CLAUDE.md"}, "", true},
		{"link on the target's path", map[string]string{"real/a.md": "x"}, map[string]string{"AGENTS.md": "dir/a.md", "dir": "real"}, "", true},
		{"target inside .git", nil, map[string]string{"AGENTS.md": ".git/config"}, "", true},
		{"root name is a directory", map[string]string{"AGENTS.md/x": "x"}, nil, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := filepath.Join(t.TempDir(), "ws")
			require.NoError(t, os.MkdirAll(filepath.Join(ws, ".git"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(ws, ".git", "config"), []byte("x"), 0o644))

			for name, body := range tc.files {
				require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(ws, name)), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644))
			}

			for name, target := range tc.links {
				require.NoError(t, os.Symlink(target, filepath.Join(ws, name)))
			}

			got, err := instructionFile(ws)
			if tc.fail {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
