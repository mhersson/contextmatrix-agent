// Package filelog writes raw worker-container output to one append file per
// card on the agent host. It is the durable counterpart to the ephemeral /logs
// SSE stream: a faithful, human-readable capture of everything a container
// printed (stdout JSONL transcript + stderr slog), as `docker logs -f` would
// show it. Logging failures are warned and swallowed so a run never fails
// because its log could not be written.
package filelog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// runHeaderPrefix opens the header Begin writes once per container run.
// NextAttempt counts lines starting with it, so writer and counter share this
// one definition and cannot drift apart.
const runHeaderPrefix = "==== run started "

// causeSuperseded is the footer cause for a run whose own correlation id
// still had an open entry when its Begin was replayed: its End never fired,
// so nothing observed how it ended. It is deliberately not one of the causes
// an exit path reports, so a reader never mistakes an unobserved run for a
// timeout or a wait failure.
const causeSuperseded = "superseded"

// Logger writes per-card container output to <dir>/<project>/<cardID>.log.
// A nil *Logger, or one built with an empty dir, disables every operation.
// Open writers are keyed by (project, cardID, correlationID), so two runs of
// one card - a re-trigger racing the previous run's drain - append to the
// same file through their own writers and each End footers only its own.
type Logger struct {
	dir    string
	logger *slog.Logger

	mu    sync.Mutex           // guards files
	files map[string]*cardFile // key: runKey(project, cardID, correlationID)
}

// cardFile is one open per-card log file plus the metadata the footer needs.
// Its own mutex guards writes/close so concurrent cards never serialize on the
// manager lock. path is retained for the warn logs footerAndClose writes on
// failure.
type cardFile struct {
	mu          sync.Mutex
	f           *os.File
	path        string
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

// runKey composes the in-memory open-writer id: the sanitized per-card key
// plus the run's correlation id, mirroring webhook.SessionID's shape. The
// correlation id is minted by webhook.handleTrigger (cardID plus a short
// random suffix, never a filesystem input), so it needs no sanitizing for a
// map key.
func runKey(project, cardID, correlationID string) string {
	return key(project, cardID) + "/" + correlationID
}

// path is the on-disk log path for a card: <dir>/<project>/<cardID>.log, each
// segment sanitized so an untrusted project/cardID cannot escape the root.
func (l *Logger) path(project, cardID string) string {
	return filepath.Join(l.dir, sanitize(project), sanitize(cardID)+".log")
}

// Begin opens (append) the card's log file and writes a run header. A writer
// already open under this run's own id is footered as superseded first.
func (l *Logger) Begin(project, cardID, containerID, correlationID string) {
	if !l.enabled() {
		return
	}

	p := l.path(project, cardID)

	keepKey := runKey(project, cardID, correlationID)

	l.closeCard(keepKey, -1, causeSuperseded) // supersede this run id's still-open entry, if any

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		l.logger.Warn("filelog: mkdir failed", "project", project, "card_id", cardID, "error", err)

		return
	}

	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		l.logger.Warn("filelog: open failed", "project", project, "card_id", cardID, "error", err)

		return
	}

	header := fmt.Sprintf(runHeaderPrefix+"%s container=%s ====\n",
		time.Now().UTC().Format(time.RFC3339), shortID(containerID))
	// Best-effort header: on failure keep capturing the run's output anyway.
	if _, err := f.WriteString(header); err != nil {
		l.logger.Warn("filelog: write header failed", "project", project, "card_id", cardID, "error", err)
	}

	l.mu.Lock()
	l.files[runKey(project, cardID, correlationID)] = &cardFile{f: f, path: p, containerID: containerID}
	l.mu.Unlock()
}

// Write appends one line (with a trailing newline) to the run's file. No-op if
// the run has no open file - a stale run id writing after its own End can never
// reach another run's writer, because writers are keyed by correlation id.
func (l *Logger) Write(project, cardID, correlationID string, line []byte, _ bool) {
	if !l.enabled() {
		return
	}

	l.mu.Lock()
	cf := l.files[runKey(project, cardID, correlationID)]
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

// closeCard writes the run footer with exitCode and cause, closes the file,
// and forgets the entry. No-op if the key has no open file.
func (l *Logger) closeCard(k string, exitCode int64, cause string) {
	l.mu.Lock()
	cf := l.files[k]
	delete(l.files, k)
	l.mu.Unlock()

	if cf == nil {
		return
	}

	l.footerAndClose(cf, exitCode, cause)
}

// footerAndClose writes the run footer and closes the file. It is a no-op on
// an already-closed handle.
func (l *Logger) footerAndClose(cf *cardFile, exitCode int64, cause string) {
	cf.mu.Lock()
	defer cf.mu.Unlock()

	if cf.closed {
		return
	}

	footer := fmt.Sprintf("==== run ended %s container=%s exit=%d cause=%s ====\n",
		time.Now().UTC().Format(time.RFC3339), shortID(cf.containerID), exitCode, cause)
	if _, err := cf.f.WriteString(footer); err != nil {
		l.logger.Warn("filelog: write footer failed", "path", cf.path, "error", err)
	}

	if err := cf.f.Close(); err != nil {
		l.logger.Warn("filelog: close failed", "path", cf.path, "error", err)
	}

	cf.closed = true
}

// End writes a run footer naming the exit code and how the run ended, closes
// the file, and forgets the entry. The cause is what separates the two kill
// paths, which share exit code -1. No-op if the run has no open file - in
// particular, a stale run's late End cannot close a re-trigger's writer,
// because the two sit under different keys.
func (l *Logger) End(project, cardID, correlationID string, exitCode int64, cause string) {
	if !l.enabled() {
		return
	}

	l.closeCard(runKey(project, cardID, correlationID), exitCode, cause)
}

// NextAttempt returns the ordinal of the run about to start for this card:
// one more than the number of runs its log already records. A card with no log,
// an unreadable log, or a disabled logger is on its first attempt. The count
// comes off the file rather than memory so it survives a restart of this
// process, which is one of the ways a card ends up with a second attempt in the
// first place.
func (l *Logger) NextAttempt(project, cardID string) int {
	if !l.enabled() {
		return 1
	}

	f, err := os.Open(l.path(project, cardID))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			l.logger.Warn("filelog: open for attempt count failed",
				"project", project, "card_id", cardID, "error", err)
		}

		return 1
	}

	defer f.Close() //nolint:errcheck // read-only handle

	count, err := countRunHeaders(f)
	if err != nil {
		l.logger.Warn("filelog: attempt count truncated by a read error",
			"project", project, "card_id", cardID, "error", err)
	}

	return count + 1
}

// countRunHeaders counts the lines in r that begin with runHeaderPrefix.
//
// It scans fixed chunks and carries the last few bytes of each into the next,
// so a header split across a chunk boundary is still counted exactly once, and
// so the scan is not bounded by line length: a transcript line runs to whatever
// the container printed, far past any single-line scan buffer. Matching on a
// preceding newline anchors the count to real line starts, and a JSON string
// cannot contain a raw newline, so log content quoting the header text is not
// miscounted.
func countRunHeaders(r io.Reader) (int, error) {
	const chunk = 32 << 10

	needle := []byte("\n" + runHeaderPrefix)
	buf := make([]byte, chunk+len(needle))

	// A synthetic leading newline makes a header at the very start of the file
	// match the same needle as every later one.
	buf[0] = '\n'
	carry := 1
	count := 0

	for {
		n, err := r.Read(buf[carry:])
		if n > 0 {
			window := buf[:carry+n]
			count += bytes.Count(window, needle)

			// An occurrence cannot fit entirely inside the carry, so nothing is
			// counted twice.
			carry = min(len(needle)-1, len(window))
			copy(buf, window[len(window)-carry:])
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return count, nil
			}

			return count, fmt.Errorf("read log: %w", err)
		}
	}
}

// sanitize lower-cases s and replaces every character outside [a-z0-9_-] with
// '-', so a project or card ID becomes a single safe path segment. Excluding
// '.' makes "." and ".." collapse to dashes, defeating path traversal; an empty
// string becomes "-" so it never collapses the path via filepath.Join.
func sanitize(s string) string {
	s = strings.ToLower(s)
	if s == "" {
		return "-"
	}

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
