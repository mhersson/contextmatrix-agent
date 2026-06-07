package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveInRoot resolves p (relative to root, or absolute) and guarantees the
// result stays within root, rejecting traversal escapes.
func resolveInRoot(root, p string) (string, error) {
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)
	rootClean := filepath.Clean(root)
	if abs != rootClean && !strings.HasPrefix(abs, rootClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes workspace root", p)
	}
	return abs, nil
}
