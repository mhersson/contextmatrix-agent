package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// groundingDoc is one discovered repo-instruction file (AGENTS.md, or
// CLAUDE.md as fallback) with its capped content, labelled by directory.
type groundingDoc struct {
	relDir  string // path relative to the workspace root; "." for the root
	name    string // "AGENTS.md" or "CLAUDE.md"
	content string
}

const (
	groundingByteCap  = 64 * 1024 // 64 KB per file
	groundingMaxDocs  = 24        // count cap (protects monorepos)
	groundingMaxDepth = 4         // directory levels below root to walk
)

// gitIgnoresDir reports whether the target repo's own ignore rules exclude path.
// It is the language-agnostic source of truth for "don't descend here":
// node_modules, vendor, target, build, dist and friends are skipped because the
// repo gitignores them, not because grounding names any ecosystem. Best-effort —
// if root is not a git repo (or git is unavailable) it returns false and the
// directory is walked.
func gitIgnoresDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == "" {
		return false
	}

	// check-ignore exits 0 when path is ignored, 1 when not, 128 on error (e.g.
	// not a git repo). Only a clean exit 0 means "ignored". Grounding's walk is
	// uncancellable, so a background context is consistent here.
	cmd := exec.CommandContext(context.Background(), "git", "-C", root, "check-ignore", "-q", "--", rel) //nolint:gosec // fixed args; rel is a workspace-relative walk entry

	return cmd.Run() == nil
}

// discoverGrounding walks the workspace root and returns the repo's
// instruction files (AGENTS.md preferred over CLAUDE.md per directory),
// root first then shallow to deep. Best-effort: a missing or non-directory
// root yields nil and the run proceeds ungrounded.
func discoverGrounding(root string) []groundingDoc {
	if root == "" {
		return nil
	}

	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	var docs []groundingDoc

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}

		if !d.IsDir() {
			return nil
		}

		// Skip hidden dot-directories (.git, .worktrees, .venv, .gradle, .next,
		// ...) and anything the repo's own .gitignore excludes (node_modules,
		// vendor, target, build, ...). This names NO ecosystem directory: the
		// repo's gitignore is the language-agnostic source of truth, exactly as in
		// the commit-staging guard. The depth and count caps below bound the rest.
		if path != root && (strings.HasPrefix(d.Name(), ".") || gitIgnoresDir(root, path)) {
			return filepath.SkipDir
		}

		if groundingDepth(root, path) > groundingMaxDepth {
			return filepath.SkipDir
		}

		if doc, ok := readGroundingDir(root, path); ok {
			docs = append(docs, doc)
		}

		return nil
	})

	sortGroundingDocs(docs)

	if len(docs) > groundingMaxDocs {
		slog.Warn("grounding: too many instruction files; truncating",
			"found", len(docs), "kept", groundingMaxDocs)

		docs = docs[:groundingMaxDocs]
	}

	return docs
}

// readGroundingDir returns the chosen instruction file for dir, AGENTS.md
// preferred over CLAUDE.md. ok is false when neither exists (or the candidate
// escapes the workspace / is not a regular file).
func readGroundingDir(root, dir string) (groundingDoc, bool) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		content, ok := readGroundingFile(root, filepath.Join(dir, name))
		if !ok {
			continue
		}

		rel, err := filepath.Rel(root, dir)
		if err != nil {
			rel = "."
		}

		return groundingDoc{relDir: rel, name: name, content: content}, true
	}

	return groundingDoc{}, false
}

// readGroundingFile reads path with the real target constrained to the workspace
// root and the read bounded before allocation. In-repo symlinks (CLAUDE.md ->
// AGENTS.md) are followed; a repo-committed symlink escaping the tree (->
// /proc/self/environ, -> /run/cm-secrets/env) or a non-regular file (device,
// FIFO) yields ok=false, so a poisoned repo cannot smuggle secrets or OOM the
// worker via the grounding block that seeds every model prompt.
func readGroundingFile(root, path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}

	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}

	rel, err := filepath.Rel(rootReal, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}

	f, err := os.Open(resolved) //nolint:gosec // path resolved and confined to the workspace root above
	if err != nil {
		return "", false
	}

	defer f.Close() //nolint:errcheck // read-only

	if info, serr := f.Stat(); serr != nil || !info.Mode().IsRegular() {
		return "", false
	}

	// Bound the read BEFORE allocation: a multi-GB committed file (or a symlink to
	// /dev/zero) must not OOM the worker before the 64 KB cap is applied.
	data, err := io.ReadAll(io.LimitReader(f, groundingByteCap+1))
	if err != nil {
		return "", false
	}

	if len(data) > groundingByteCap {
		return truncateGroundingUTF8(data, groundingByteCap) + "\n[... truncated at 64 KB ...]", true
	}

	return string(data), true
}

// truncateGroundingUTF8 returns the first limit bytes of data backed off to the
// nearest UTF-8 rune boundary, so a fixed-offset cut never splits a multi-byte
// rune (which would emit U+FFFD in the grounding prompt).
func truncateGroundingUTF8(data []byte, limit int) string {
	if limit >= len(data) {
		return string(data)
	}

	for limit > 0 && data[limit]&0xC0 == 0x80 {
		limit--
	}

	return string(data[:limit])
}

func groundingDepth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}

	return strings.Count(rel, string(os.PathSeparator)) + 1
}

func sortGroundingDocs(docs []groundingDoc) {
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].relDir == "." {
			return docs[j].relDir != "."
		}

		if docs[j].relDir == "." {
			return false
		}

		di := strings.Count(docs[i].relDir, string(os.PathSeparator))

		dj := strings.Count(docs[j].relDir, string(os.PathSeparator))
		if di != dj {
			return di < dj
		}

		return docs[i].relDir < docs[j].relDir
	})
}

// groundingBlock renders discovered instruction files as a prompt block.
// Empty input yields "" so phases inject nothing when no files exist.
func groundingBlock(docs []groundingDoc) string {
	if len(docs) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("REPO GROUNDING — the target repository's own instructions; " +
		"follow them, they override generic defaults.\n")

	for _, d := range docs {
		var label string

		if d.relDir != "." {
			label = filepath.ToSlash(d.relDir) + "/" + d.name
		} else {
			label = "./" + d.name
		}

		b.WriteString("\n=== " + label + " ===\n")
		b.WriteString(d.content)
		b.WriteString("\n")
	}

	return b.String()
}
