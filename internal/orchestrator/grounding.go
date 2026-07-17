package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// groundingDoc is the workspace root's instruction file (AGENTS.md, or CLAUDE.md
// as fallback) with its capped content. Only the root doc is ever read into the
// grounding block; nested files are enumerated as paths, never as content.
type groundingDoc struct {
	name    string // "AGENTS.md" or "CLAUDE.md"
	content string
}

const (
	groundingByteCap  = 64 * 1024 // 64 KB for the root doc
	groundingMaxDocs  = 24        // listing count cap (protects monorepos)
	groundingMaxDepth = 4         // directory levels below root to list
)

// discoverGrounding returns the repo's instruction files split into two tiers:
// the workspace root's own AGENTS.md/CLAUDE.md (read in full, to be INJECTED) and
// the relative paths of nested instruction files (ENUMERATED only, never read).
// The split closes a poisoning vector: a repo that COMMITS a dependency tree (Go
// vendor/, a checked-in node_modules) carries third-party AGENTS.md/CLAUDE.md
// files that are tracked, not gitignored - injecting their content would present
// a foreign library's instructions to every phase as "the target repo's own
// rules". Listing a nested doc's path lets the model read it on demand without
// that masquerade. Best-effort: a missing or non-directory root yields nils and
// the run proceeds ungrounded.
func discoverGrounding(root string) (*groundingDoc, []string) {
	if root == "" {
		return nil, nil
	}

	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, nil
	}

	var rootDoc *groundingDoc

	if doc, ok := readGroundingDir(root); ok {
		rootDoc = &doc
	}

	// Nested docs: one subprocess in a git workspace (tracked files only, so
	// gitignored and untracked trees are structurally excluded); a filesystem
	// walk otherwise. Both feed the same post-filter for uniform semantics.
	candidates, ok := gitTrackedNested(root)
	if !ok {
		candidates = walkNested(root)
	}

	return rootDoc, filterNested(candidates)
}

// gitTrackedNested enumerates the workspace's nested instruction files from git's
// tracked index in a single `git ls-files` - the pathspecs require a leading
// directory so the root doc is excluded (it is discovered separately). ok is
// false when root is not a git repo or git is unavailable (ls-files exits
// non-zero), signalling the caller to fall back to a filesystem walk. Returned
// paths are slash-separated and relative to root; filtering happens in
// filterNested.
func gitTrackedNested(root string) ([]string, bool) {
	cmd := exec.CommandContext(context.Background(), "git", "-C", root,
		"ls-files", "-z", "--", "*/AGENTS.md", "*/CLAUDE.md") //nolint:gosec // fixed args; root is the workspace path

	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}

	var rels []string

	for rel := range strings.SplitSeq(string(out), "\x00") {
		if rel != "" {
			rels = append(rels, rel)
		}
	}

	return rels, true
}

// walkNested is the non-git fallback: a filesystem walk enumerating nested
// AGENTS.md/CLAUDE.md files. The depth check runs FIRST (the cheapest gate,
// before any dot-name or file work), dot-directories are skipped, and hits feed
// the same listing as the git path. The root doc is discovered by the caller, so
// the root directory contributes no listing entry.
func walkNested(root string) []string {
	var rels []string

	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || !d.IsDir() {
			return nil
		}

		if groundingDepth(root, p) > groundingMaxDepth {
			return filepath.SkipDir
		}

		if p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}

		if p == root {
			return nil
		}

		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			if fi, err := os.Lstat(filepath.Join(p, name)); err == nil && fi.Mode().IsRegular() {
				if rel, rerr := filepath.Rel(root, p); rerr == nil {
					rels = append(rels, filepath.ToSlash(filepath.Join(rel, name)))
				}
			}
		}

		return nil
	})

	return rels
}

// filterNested reduces raw candidate instruction-file paths (relative to root,
// slash-separated) to the enumerated listing: entries with a dot-directory
// component are dropped, entries below groundingMaxDepth are dropped, AGENTS.md
// wins over CLAUDE.md within a directory, the result is sorted shallow → deep,
// and it is capped at groundingMaxDocs.
func filterNested(candidates []string) []string {
	chosen := map[string]string{} // directory -> selected file name

	for _, rel := range candidates {
		rel = filepath.ToSlash(rel)
		name := path.Base(rel)

		if name != "AGENTS.md" && name != "CLAUDE.md" {
			continue
		}

		dir := path.Dir(rel)
		if dir == "." || dir == "" || hasDotComponent(dir) || nestedDepth(dir) > groundingMaxDepth {
			continue
		}

		if cur, ok := chosen[dir]; !ok || (cur == "CLAUDE.md" && name == "AGENTS.md") {
			chosen[dir] = name
		}
	}

	listed := make([]string, 0, len(chosen))
	for dir, name := range chosen {
		listed = append(listed, dir+"/"+name)
	}

	sortNested(listed)

	if len(listed) > groundingMaxDocs {
		slog.Warn("grounding: too many nested instruction files; truncating listing",
			"found", len(listed), "kept", groundingMaxDocs)

		listed = listed[:groundingMaxDocs]
	}

	return listed
}

// hasDotComponent reports whether any segment of a slash-separated directory
// starts with a dot (.git, .venv, .next, ...), so hidden trees never appear in
// the listing even when git tracks a file inside one.
func hasDotComponent(dir string) bool {
	for seg := range strings.SplitSeq(dir, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}

	return false
}

// nestedDepth is the directory-level count of a non-root, slash-separated dir
// ("svc" -> 1, "internal/api" -> 2), matching groundingDepth's convention.
func nestedDepth(dir string) int {
	return strings.Count(dir, "/") + 1
}

// sortNested orders the listing shallow → deep, then lexically, so the closest
// (most general) instruction files are named first.
func sortNested(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool {
		di := strings.Count(path.Dir(paths[i]), "/")

		dj := strings.Count(path.Dir(paths[j]), "/")
		if di != dj {
			return di < dj
		}

		return paths[i] < paths[j]
	})
}

// readGroundingDir returns the chosen instruction file for the workspace root,
// AGENTS.md preferred over CLAUDE.md. ok is false when neither exists (or the
// candidate escapes the workspace / is not a regular file).
func readGroundingDir(root string) (groundingDoc, bool) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		content, ok := readGroundingFile(root, filepath.Join(root, name))
		if !ok {
			continue
		}

		return groundingDoc{name: name, content: content}, true
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

// groundingBlock renders the root instruction file plus the nested-file listing
// as a prompt block. The root doc's content is injected verbatim; nested files
// are named as PATHS with an instruction to read them on demand - never their
// content, so a committed third-party doc cannot pose as the repo's own rules.
// No root doc and an empty listing yields "" so phases inject nothing.
func groundingBlock(root *groundingDoc, nested []string) string {
	if root == nil && len(nested) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("REPO GROUNDING - the target repository's own instructions; " +
		"follow them, they override generic defaults.\n")

	if root != nil {
		b.WriteString("\n=== ./" + root.name + " ===\n")
		b.WriteString(root.content)
		b.WriteString("\n")
	}

	if len(nested) > 0 {
		b.WriteString("\nDirectory-specific instruction files exist at: " +
			strings.Join(nested, ", ") + ".\n")
		b.WriteString("Before working on files under one of these directories, read that " +
			"directory's instruction file first.\n")
	}

	return b.String()
}
