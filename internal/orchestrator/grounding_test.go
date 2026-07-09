package orchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeNestedFile(t *testing.T, dir, rel, content string) {
	t.Helper()

	p := filepath.Join(dir, rel)

	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

func gitInit(t *testing.T, dir string) {
	t.Helper()

	require.NoError(t, exec.Command("git", "-C", dir, "init", "-q").Run())
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

	t.Run("hidden and gitignored dirs are not walked", func(t *testing.T) {
		root := t.TempDir()
		gitInit(t, root)
		// The repo's own .gitignore is the agnostic source of truth for which
		// non-hidden dirs to skip — grounding names no ecosystem directory.
		writeNestedFile(t, root, ".gitignore", "node_modules/\n")
		writeNestedFile(t, root, "CLAUDE.md", "# root")
		// Skipped because the repo ignores it, not because grounding knows the
		// name "node_modules".
		writeNestedFile(t, root, "node_modules/pkg/CLAUDE.md", "# vendored")
		// Hidden dot-dir: always skipped (VCS/tooling/build state).
		writeNestedFile(t, root, ".cache/CLAUDE.md", "# hidden")
		// An ordinary, non-ignored subdir IS walked.
		writeNestedFile(t, root, "svc/CLAUDE.md", "# tracked subdir")

		docs := discoverGrounding(root)
		require.Len(t, docs, 2)

		relDirs := []string{docs[0].relDir, docs[1].relDir}
		assert.Contains(t, relDirs, ".")
		assert.Contains(t, relDirs, "svc")
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

func TestReadGroundingDirRejectsOutOfTreeSymlink(t *testing.T) {
	// A repo-committed instruction file symlinked OUT of the workspace (in
	// production: /proc/self/environ, /run/cm-secrets/env) must not be read into
	// the grounding block — that would smuggle secrets, unredacted, into every
	// model prompt.
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.env")
	require.NoError(t, os.WriteFile(outside, []byte("CM_MCP_API_KEY=supersecret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "AGENTS.md")))

	docs := discoverGrounding(root)

	for _, d := range docs {
		assert.NotContains(t, d.content, "supersecret",
			"out-of-tree symlinked instruction file must not leak into grounding")
	}

	assert.Empty(t, docs, "the only instruction file escapes the workspace; nothing grounded")
}

func TestReadGroundingDirFollowsInRepoSymlink(t *testing.T) {
	// The common in-repo CLAUDE.md -> AGENTS.md symlink is legitimate and must
	// still be read (containment allows targets that stay within the workspace).
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# real rules"), 0o644))
	sub := filepath.Join(root, "pkg")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.Symlink(filepath.Join(root, "AGENTS.md"), filepath.Join(sub, "CLAUDE.md")))

	var found bool

	for _, d := range discoverGrounding(root) {
		if d.relDir == "pkg" {
			found = true

			assert.Contains(t, d.content, "# real rules", "in-repo symlink must be followed")
		}
	}

	assert.True(t, found, "in-tree symlinked instruction file must be grounded")
}

func TestGroundingTruncationIsRuneSafe(t *testing.T) {
	// A 3-byte rune straddling the cap boundary must not be split into invalid UTF-8.
	root := t.TempDir()
	writeNestedFile(t, root, "AGENTS.md", strings.Repeat("a", groundingByteCap-1)+"…tail")

	docs := discoverGrounding(root)
	require.Len(t, docs, 1)
	assert.True(t, utf8.ValidString(docs[0].content), "truncated grounding must be valid UTF-8")
	assert.Contains(t, docs[0].content, "[... truncated at 64 KB ...]")
}

func TestGroundingBlock(t *testing.T) {
	assert.Empty(t, groundingBlock(nil))

	docs := []groundingDoc{
		{relDir: ".", name: "CLAUDE.md", content: "# root rules"},
		{relDir: "web", name: "CLAUDE.md", content: "# web rules"},
	}
	block := groundingBlock(docs)

	assert.Contains(t, block, "REPO GROUNDING")
	assert.Contains(t, block, "=== ./CLAUDE.md ===")
	assert.Contains(t, block, "# root rules")
	assert.Contains(t, block, "=== web/CLAUDE.md ===")
	assert.Contains(t, block, "# web rules")

	// Root divider must precede the web divider.
	rootDiv := strings.Index(block, "=== ./CLAUDE.md ===")
	webDiv := strings.Index(block, "=== web/CLAUDE.md ===")

	assert.Less(t, rootDiv, webDiv, "root divider should appear before web divider")

	// Each doc's content must appear after its own divider and before the next.
	rootContent := strings.Index(block, "# root rules")
	webContent := strings.Index(block, "# web rules")

	assert.Less(t, rootDiv, rootContent, "root content should follow root divider")
	assert.Less(t, rootContent, webDiv, "root content should precede web divider")
	assert.Less(t, webDiv, webContent, "web content should follow web divider")
}
