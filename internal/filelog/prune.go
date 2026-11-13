package filelog

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Prune removes card log files under <dir>/<project>/ last modified before
// now-maxAge and returns how many it removed. A file with an open writer is
// never removed. The sweep holds the logger lock, so it cannot interleave with
// Begin opening a card's file; a failure on one file is warned and skipped.
func (l *Logger) Prune(now time.Time, maxAge time.Duration) int {
	if !l.enabled() {
		return 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	open := make(map[string]bool, len(l.files))
	for _, cf := range l.files {
		open[cf.path] = true
	}

	// fs.Glob over the dir keeps metacharacters in a configured log_dir literal.
	matches, err := fs.Glob(os.DirFS(l.dir), "*/*.log")
	if err != nil {
		l.logger.Warn("filelog: prune glob failed", "error", err)

		return 0
	}

	paths := make([]string, len(matches))
	for i, m := range matches {
		paths[i] = filepath.Join(l.dir, m)
	}

	cutoff := now.Add(-maxAge)
	removed := 0

	for _, p := range paths {
		if open[p] {
			continue
		}

		info, err := os.Stat(p)
		if err != nil {
			l.logger.Warn("filelog: prune stat failed", "path", p, "error", err)

			continue
		}

		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}

		if err := os.Remove(p); err != nil {
			l.logger.Warn("filelog: prune remove failed", "path", p, "error", err)

			continue
		}

		removed++
	}

	return removed
}
