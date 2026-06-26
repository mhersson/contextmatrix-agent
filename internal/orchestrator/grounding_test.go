package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeNestedFile(t *testing.T, dir, rel, content string) {
	t.Helper()

	p := filepath.Join(dir, rel)

	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

func TestDiscoverGrounding(t *testing.T) {
	t.Run("empty or missing root yields nil", func(t *testing.T) {
		assert.Nil(t, discoverGrounding(""))
		assert.Nil(t, discoverGrounding(filepath.Join(t.TempDir(), "nope")))
	})

	t.Run("AGENTS.md preferred over CLAUDE.md in the same dir", func(t *testing.T) {
		root := t.TempDir()
		writeNestedFile(t, root, "AGENTS.md", "# agents")
		writeNestedFile(t, root, "CLAUDE.md", "# claude")

		docs := discoverGrounding(root)
		require.Len(t, docs, 1)
		assert.Equal(t, "AGENTS.md", docs[0].name)
		assert.Equal(t, ".", docs[0].relDir)
		assert.Contains(t, docs[0].content, "# agents")
	})

	t.Run("CLAUDE.md used as fallback when no AGENTS.md", func(t *testing.T) {
		root := t.TempDir()
		writeNestedFile(t, root, "CLAUDE.md", "# claude")

		docs := discoverGrounding(root)
		require.Len(t, docs, 1)
		assert.Equal(t, "CLAUDE.md", docs[0].name)
	})

	t.Run("nested files discovered, root first then shallow to deep", func(t *testing.T) {
		root := t.TempDir()
		writeNestedFile(t, root, "CLAUDE.md", "# root")
		writeNestedFile(t, root, "web/CLAUDE.md", "# web")
		writeNestedFile(t, root, "internal/api/AGENTS.md", "# api")

		docs := discoverGrounding(root)
		require.Len(t, docs, 3)
		assert.Equal(t, ".", docs[0].relDir)
		assert.Equal(t, "web", docs[1].relDir)
		assert.Equal(t, filepath.Join("internal", "api"), docs[2].relDir)
	})

	t.Run("file over 64KB is truncated with a marker", func(t *testing.T) {
		root := t.TempDir()
		writeNestedFile(t, root, "CLAUDE.md", strings.Repeat("x", 70*1024))

		docs := discoverGrounding(root)
		require.Len(t, docs, 1)
		assert.LessOrEqual(t, len(docs[0].content), groundingByteCap+64)
		assert.Contains(t, docs[0].content, "[... truncated at 64 KB ...]")
	})

	t.Run("excluded dirs are not walked", func(t *testing.T) {
		root := t.TempDir()
		writeNestedFile(t, root, "CLAUDE.md", "# root")
		writeNestedFile(t, root, "node_modules/pkg/CLAUDE.md", "# vendored")
		writeNestedFile(t, root, ".git/CLAUDE.md", "# git")

		docs := discoverGrounding(root)
		require.Len(t, docs, 1)
		assert.Equal(t, ".", docs[0].relDir)
	})

	t.Run("dirs deeper than max depth are not walked", func(t *testing.T) {
		root := t.TempDir()
		writeNestedFile(t, root, "CLAUDE.md", "# root")
		writeNestedFile(t, root, "a/b/c/d/e/CLAUDE.md", "# too deep")

		tooDeep := filepath.Join("a", "b", "c", "d", "e")
		docs := discoverGrounding(root)

		for _, doc := range docs {
			assert.NotEqual(t, tooDeep, doc.relDir, "depth-5 dir should be skipped")
		}
	})

	t.Run("count cap limits results to groundingMaxDocs", func(t *testing.T) {
		root := t.TempDir()

		for i := range 25 {
			writeNestedFile(t, root, filepath.Join(fmt.Sprintf("pkg%02d", i), "CLAUDE.md"), "# content")
		}

		docs := discoverGrounding(root)
		assert.Len(t, docs, groundingMaxDocs)
	})
}
