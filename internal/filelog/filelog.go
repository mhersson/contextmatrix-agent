// Package filelog writes raw worker-container output to one append file per
// card on the agent host. It is the durable counterpart to the ephemeral /logs
// SSE stream: a faithful, human-readable capture of everything a container
// printed (stdout JSONL transcript + stderr slog), as `docker logs -f` would
// show it. Logging failures are warned and swallowed so a run never fails
// because its log could not be written.
package filelog

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Logger writes per-card container output to <dir>/<project>/<cardID>.log.
// A nil *Logger, or one built with an empty dir, disables every operation, so
// callers can wire it unconditionally. Safe for concurrent use across cards.
type Logger struct {
	dir    string
	logger *slog.Logger

	mu    sync.Mutex           // guards files only
	files map[string]*cardFile // key: sanitize(project)/sanitize(cardID)
}

// cardFile is one open per-card log file plus the metadata the footer needs.
// Its own mutex guards writes/close so concurrent cards never serialize on the
// manager lock.
type cardFile struct {
	mu          sync.Mutex
	f           *os.File
	containerID string
	closed      bool
}

// New builds a Logger rooted at dir. An empty dir disables file logging.
func New(dir string, logger *slog.Logger) *Logger {
	if logger == nil {
		logger = slog.Default()
	}

	return &Logger{
		dir:    dir,
		logger: logger,
		files:  make(map[string]*cardFile),
	}
}

func (l *Logger) enabled() bool {
	return l != nil && l.dir != ""
}

func key(project, cardID string) string {
	return sanitize(project) + "/" + sanitize(cardID)
}

// path is the on-disk log path for a card: <dir>/<project>/<cardID>.log, each
// segment sanitized so an untrusted project/cardID cannot escape the root.
func (l *Logger) path(project, cardID string) string {
	return filepath.Join(l.dir, sanitize(project), sanitize(cardID)+".log")
}

// Begin opens (append) the card's log file and writes a run header. Call once
// per container run, before any Write.
func (l *Logger) Begin(project, cardID, containerID string) {
	if !l.enabled() {
		return
	}

	p := l.path(project, cardID)

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		l.logger.Warn("filelog: mkdir failed", "project", project, "card_id", cardID, "error", err)

		return
	}

	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		l.logger.Warn("filelog: open failed", "project", project, "card_id", cardID, "error", err)

		return
	}

	header := fmt.Sprintf("==== run started %s container=%s ====\n",
		time.Now().UTC().Format(time.RFC3339), shortID(containerID))
	if _, err := f.WriteString(header); err != nil {
		l.logger.Warn("filelog: write header failed", "project", project, "card_id", cardID, "error", err)
	}

	l.mu.Lock()
	l.files[key(project, cardID)] = &cardFile{f: f, containerID: containerID}
	l.mu.Unlock()
}

// Write appends one line (with a trailing newline) to the card's file. No-op
// if the card has no open file.
func (l *Logger) Write(project, cardID string, line []byte, _ bool) {
	if !l.enabled() {
		return
	}

	l.mu.Lock()
	cf := l.files[key(project, cardID)]
	l.mu.Unlock()

	if cf == nil {
		return
	}

	cf.mu.Lock()
	defer cf.mu.Unlock()

	if cf.closed {
		return
	}

	// Copy into a fresh buffer so we never mutate the caller's slice (the tee
	// also passes line on to the log bridge).
	buf := make([]byte, len(line)+1)
	copy(buf, line)
	buf[len(line)] = '\n'

	if _, err := cf.f.Write(buf); err != nil {
		l.logger.Warn("filelog: write failed", "project", project, "card_id", cardID, "error", err)
	}
}

// End writes a run footer, closes the file, and forgets the card. No-op if the
// card has no open file.
func (l *Logger) End(project, cardID string, exitCode int64) {
	if !l.enabled() {
		return
	}

	k := key(project, cardID)

	l.mu.Lock()
	cf := l.files[k]
	delete(l.files, k)
	l.mu.Unlock()

	if cf == nil {
		return
	}

	cf.mu.Lock()
	defer cf.mu.Unlock()

	footer := fmt.Sprintf("==== run ended %s container=%s exit=%d ====\n",
		time.Now().UTC().Format(time.RFC3339), shortID(cf.containerID), exitCode)
	if _, err := cf.f.WriteString(footer); err != nil {
		l.logger.Warn("filelog: write footer failed", "project", project, "card_id", cardID, "error", err)
	}

	if err := cf.f.Close(); err != nil {
		l.logger.Warn("filelog: close failed", "project", project, "card_id", cardID, "error", err)
	}

	cf.closed = true
}

// sanitize lower-cases s and replaces every character outside [a-z0-9_-] with
// '-', so a project or card ID becomes a single safe path segment. Excluding
// '.' makes "." and ".." collapse to dashes, defeating path traversal.
func sanitize(s string) string {
	s = strings.ToLower(s)

	var b strings.Builder

	b.Grow(len(s))

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	return b.String()
}

// shortID truncates a Docker container ID to 12 chars for the run header.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}
