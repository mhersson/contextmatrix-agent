package runkind

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// instructionFile returns the repo-relative path of the file grounding reads:
// root AGENTS.md, else root CLAUDE.md, else a new AGENTS.md. A symlink there is
// followed to its target.
func instructionFile(ws string) (string, error) {
	_, aErr := os.Lstat(filepath.Join(ws, "AGENTS.md"))
	_, cErr := os.Lstat(filepath.Join(ws, "CLAUDE.md"))
	name := guidanceFile(aErr == nil, cErr == nil)

	info, err := os.Lstat(filepath.Join(ws, name))

	switch {
	case errors.Is(err, os.ErrNotExist):
		return name, nil
	case err != nil:
		return "", fmt.Errorf("inspect %s: %w", name, err)
	case info.Mode().IsRegular():
		return name, nil
	case info.Mode()&os.ModeSymlink != 0:
		return linkTarget(ws, name)
	default:
		return "", fmt.Errorf("%s is not a regular file", name)
	}
}

// linkTarget resolves the symlink name in ws to its target's slash-separated
// path in the repo. The target must be a regular file inside the repo, outside
// .git, with no symlink on its path.
func linkTarget(ws, name string) (string, error) {
	root, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}

	dest, err := os.Readlink(filepath.Join(root, name))
	if err != nil {
		return "", fmt.Errorf("read link %s: %w", name, err)
	}

	if !filepath.IsAbs(dest) {
		dest = filepath.Join(root, dest)
	}

	dest = filepath.Clean(dest)

	rel, err := filepath.Rel(root, dest)
	if err != nil {
		return "", fmt.Errorf("%s: relate link to the repo: %w", name, err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s links outside the repo", name)
	}

	rel = filepath.ToSlash(rel)
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return "", fmt.Errorf("%s links into .git", name)
	}

	resolved, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return "", fmt.Errorf("%s links to a missing file: %w", name, err)
	}

	if resolved != dest {
		return "", fmt.Errorf("%s links through a link", name)
	}

	info, err := os.Lstat(dest)
	if err != nil {
		return "", fmt.Errorf("stat %s link target: %w", name, err)
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s links to something that is not a regular file", name)
	}

	return rel, nil
}
