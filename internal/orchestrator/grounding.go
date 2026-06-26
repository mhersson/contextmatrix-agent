package orchestrator

import (
	"log/slog"
	"os"
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

var groundingSkipDirs = map[string]struct{}{
	".git": {}, "node_modules": {}, "vendor": {}, "dist": {},
	"build": {}, ".next": {}, "target": {}, ".worktrees": {},
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

		if _, skip := groundingSkipDirs[d.Name()]; skip && path != root {
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
// preferred over CLAUDE.md. ok is false when neither exists.
func readGroundingDir(root, dir string) (groundingDoc, bool) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}

		content := string(data)
		if len(data) > groundingByteCap {
			content = string(data[:groundingByteCap]) + "\n[... truncated at 64 KB ...]"
		}

		rel, err := filepath.Rel(root, dir)
		if err != nil {
			rel = "."
		}

		return groundingDoc{relDir: rel, name: name, content: content}, true
	}

	return groundingDoc{}, false
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
