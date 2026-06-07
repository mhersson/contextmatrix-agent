package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveInRoot resolves p (relative to root, or absolute), guaranteeing the
// result stays within root even across symlinks. The returned path is based on
// the (unresolved) clean root so callers see a stable workspace-relative path,
// while containment is checked against the symlink-resolved location. Symlinks
// on the deepest existing ancestor are resolved, so not-yet-created files
// (write/edit targets) still validate.
func resolveInRoot(root, p string) (string, error) {
	rootClean := filepath.Clean(root)
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(rootClean, p)
	}
	abs = filepath.Clean(abs)

	rootResolved := evalOrSelf(rootClean)
	resolved := resolveExisting(abs)
	if resolved != rootResolved && !strings.HasPrefix(resolved, rootResolved+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes workspace root", p)
	}
	return abs, nil
}

// evalOrSelf returns EvalSymlinks(p), or p unchanged if it cannot be resolved.
func evalOrSelf(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// resolveExisting walks up to the deepest existing ancestor of abs, resolves
// its symlinks, then rejoins the non-existent suffix.
func resolveExisting(abs string) string {
	suffix := ""
	cur := abs
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if suffix == "" {
				return resolved
			}
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs // reached the filesystem root without resolving anything
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}
