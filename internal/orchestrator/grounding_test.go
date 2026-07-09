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

// gitInit initialises a repo isolated from the developer's global gitignore, so
// only the repo's own rules and its tracked index decide what ls-files sees.
func gitInit(t *testing.T, dir string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	require.NoError(t, exec.Command("git", "-C", dir, "init", "-q").Run())
	require.NoError(t, exec.Command("git", "-C", dir, "config", "core.excludesFile", "/dev/null").Run())
}

// gitAddAll stages the whole tree so the nested files become tracked — the
// listing is built from git's tracked index, so a file must be added (not merely
// present) to appear.
func gitAddAll(t *testing.T, dir string) {
	t.Helper()

	require.NoError(t, exec.Command("git", "-C", dir, "add", "-A").Run())
}

func TestDiscoverGrounding(t *testing.T) {
	t.Run("empty or missing root yields nil", func(t *testing.T) {
		root, nested := discoverGrounding("")
		assert.Nil(t, root)
		assert.Nil(t, nested)

		root, nested = discoverGrounding(filepath.Join(t.TempDir(), "nope"))
		assert.Nil(t, root)
		assert.Nil(t, nested)
	})

	t.Run("root AGENTS.md preferred over CLAUDE.md", func(t *testing.T) {
		root := t.TempDir()
		writeNestedFile(t, root, "AGENTS.md", "# agents")
		writeNestedFile(t, root, "CLAUDE.md", "# claude")

		doc, nested := discoverGrounding(root)
		require.NotNil(t, doc)
		assert.Equal(t, "AGENTS.md", doc.name)
		assert.Equal(t, ".", doc.relDir)
		assert.Contains(t, doc.content, "# agents")
		assert.Empty(t, nested)
	})

	t.Run("root CLAUDE.md used as fallback when no AGENTS.md", func(t *testing.T) {
		root := t.TempDir()
		writeNestedFile(t, root, "CLAUDE.md", "# claude")

		doc, _ := discoverGrounding(root)
		require.NotNil(t, doc)
		assert.Equal(t, "CLAUDE.md", doc.name)
	})

	t.Run("git workspace: root injected, nested listed shallow to deep, gitignored excluded", func(t *testing.T) {
		root := t.TempDir()
		gitInit(t, root)
		// The repo's own .gitignore is the agnostic source of truth for which trees
		// to exclude — grounding names no ecosystem directory.
		writeNestedFile(t, root, ".gitignore", "node_modules/\n")
		writeNestedFile(t, root, "CLAUDE.md", "# root rules")
		writeNestedFile(t, root, "internal/api/AGENTS.md", "# api rules")
		writeNestedFile(t, root, "web/CLAUDE.md", "# web rules")
		// Gitignored: `git add -A` refuses it, so it never enters the tracked index
		// and cannot reach the listing — no name-based knowledge of "node_modules".
		writeNestedFile(t, root, "node_modules/pkg/CLAUDE.md", "# vendored")
		gitAddAll(t, root)

		doc, nested := discoverGrounding(root)
		require.NotNil(t, doc)
		assert.Equal(t, "CLAUDE.md", doc.name)
		assert.Contains(t, doc.content, "# root rules")

		// Nested docs are ENUMERATED as paths, shallow → deep, never read.
		require.Equal(t, []string{"web/CLAUDE.md", filepath.ToSlash("internal/api/AGENTS.md")}, nested)

		// The rendered block injects the root content but only LISTS nested paths.
		block := groundingBlock(doc, nested)
		assert.Contains(t, block, "# root rules")
		assert.Contains(t, block, "web/CLAUDE.md")
		assert.NotContains(t, block, "# web rules", "a nested doc's content must never be injected")
	})

	t.Run("committed third-party tree is listed, its content never injected", func(t *testing.T) {
		// REGRESSION PIN. A repo that COMMITS its dependency tree (Go vendor/, a
		// checked-in node_modules) carries third-party AGENTS.md/CLAUDE.md files that
		// are TRACKED, not gitignored — a gitignore-only prune would walk them and
		// inject a foreign library's instructions as if they were the target repo's
		// own rules. The trade this pins: a committed third-party instruction file
		// appears at most as a PATH in the listing; its content is never injected
		// into the block every phase presents as "the target repo's own instructions".
		root := t.TempDir()
		gitInit(t, root)
		writeNestedFile(t, root, "AGENTS.md", "# first-party rules")
		writeNestedFile(t, root, "vendor/github.com/x/CLAUDE.md", "# THIRD-PARTY do not inject")
		gitAddAll(t, root)

		doc, nested := discoverGrounding(root)
		require.NotNil(t, doc)
		assert.Contains(t, nested, "vendor/github.com/x/CLAUDE.md", "a tracked nested doc is enumerated as a path")

		block := groundingBlock(doc, nested)
		assert.NotContains(t, block, "THIRD-PARTY do not inject",
			"a committed third-party instruction file must never be injected as the repo's own")
	})

	t.Run("non-git fallback: root injected, nested listed, dot-dirs and depth pruned", func(t *testing.T) {
		root := t.TempDir() // no git init: exercises the WalkDir fallback
		writeNestedFile(t, root, "CLAUDE.md", "# root rules")
		writeNestedFile(t, root, "svc/CLAUDE.md", "# svc rules")
		// Hidden dot-dir: always skipped (VCS/tooling/build state).
		writeNestedFile(t, root, ".cache/CLAUDE.md", "# hidden")
		// Deeper than the depth cap: skipped before any read.
		writeNestedFile(t, root, "a/b/c/d/e/CLAUDE.md", "# too deep")

		doc, nested := discoverGrounding(root)
		require.NotNil(t, doc)
		assert.Contains(t, doc.content, "# root rules")

		assert.Contains(t, nested, "svc/CLAUDE.md")

		for _, p := range nested {
			assert.NotContains(t, p, ".cache", "a hidden dot-dir must not appear in the listing")
			assert.NotEqual(t, filepath.ToSlash("a/b/c/d/e/CLAUDE.md"), p, "a depth-5 dir must not be listed")
		}
	})

	t.Run("listing is capped at groundingMaxDocs", func(t *testing.T) {
		root := t.TempDir()

		for i := range groundingMaxDocs + 5 {
			writeNestedFile(t, root, filepath.Join(fmt.Sprintf("pkg%02d", i), "CLAUDE.md"), "# content")
		}

		_, nested := discoverGrounding(root)
		assert.Len(t, nested, groundingMaxDocs)
	})
}

func TestReadGroundingDirRejectsOutOfTreeSymlink(t *testing.T) {
	// A repo-committed root instruction file symlinked OUT of the workspace (in
	// production: /proc/self/environ, /run/cm-secrets/env) must not be read into
	// the grounding block — that would smuggle secrets, unredacted, into every
	// model prompt.
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.env")
	require.NoError(t, os.WriteFile(outside, []byte("CM_MCP_API_KEY=supersecret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "AGENTS.md")))

	doc, _ := discoverGrounding(root)

	assert.Nil(t, doc, "the root instruction file escapes the workspace; nothing grounded")

	if doc != nil {
		assert.NotContains(t, doc.content, "supersecret",
			"out-of-tree symlinked instruction file must not leak into grounding")
	}
}

func TestReadGroundingDirFollowsInRepoSymlink(t *testing.T) {
	// A legitimate in-repo symlink for the root doc (AGENTS.md -> an in-tree file)
	// is followed and read (containment allows targets that stay within the
	// workspace); only escaping symlinks are rejected.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "rules.md"), []byte("# real rules"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(root, "rules.md"), filepath.Join(root, "AGENTS.md")))

	doc, _ := discoverGrounding(root)
	require.NotNil(t, doc, "in-tree symlinked root instruction file must be grounded")
	assert.Equal(t, "AGENTS.md", doc.name)
	assert.Contains(t, doc.content, "# real rules", "in-repo symlink must be followed")
}

func TestGroundingTruncationIsRuneSafe(t *testing.T) {
	// A 3-byte rune straddling the cap boundary must not be split into invalid UTF-8.
	root := t.TempDir()
	writeNestedFile(t, root, "AGENTS.md", strings.Repeat("a", groundingByteCap-1)+"…tail")

	doc, _ := discoverGrounding(root)
	require.NotNil(t, doc)
	assert.True(t, utf8.ValidString(doc.content), "truncated grounding must be valid UTF-8")
	assert.Contains(t, doc.content, "[... truncated at 64 KB ...]")
}

func TestGroundingBlock(t *testing.T) {
	assert.Empty(t, groundingBlock(nil, nil))

	root := &groundingDoc{relDir: ".", name: "CLAUDE.md", content: "# root rules"}
	nested := []string{"web/CLAUDE.md", "internal/api/AGENTS.md"}
	block := groundingBlock(root, nested)

	assert.Contains(t, block, "REPO GROUNDING")
	assert.Contains(t, block, "=== ./CLAUDE.md ===")
	assert.Contains(t, block, "# root rules")

	// Nested files are named as PATHS with a read-on-demand instruction — their
	// content is NOT part of the block.
	assert.Contains(t, block, "web/CLAUDE.md")
	assert.Contains(t, block, "internal/api/AGENTS.md")
	assert.Contains(t, block, "read that directory's instruction file first")

	// The root divider (injected content) must precede the nested listing.
	rootDiv := strings.Index(block, "=== ./CLAUDE.md ===")
	listing := strings.Index(block, "Directory-specific instruction files exist at:")
	assert.Less(t, rootDiv, listing, "root content should precede the nested listing")

	// A root-only block carries no listing sentence.
	assert.NotContains(t, groundingBlock(root, nil), "Directory-specific instruction files")
}
