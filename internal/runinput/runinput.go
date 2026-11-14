// Package runinput stages a run kind's inputs on the serve host: one directory
// per run, bind-mounted read-only into its container. The inputs are not
// secrets, so they live on disk under the log dir, never beside the env file.
package runinput

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
)

const (
	// MountPath is where a run's input dir is mounted inside its container.
	MountPath = "/run/cm-input"
	// InputFile holds the JSON of the kind's input.
	InputFile = "input.json"
	// RunLogFile holds the stopped run's log section an investigation reads.
	RunLogFile = "run.log"
)

var unsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Store owns the per-run input dirs under one root.
type Store struct {
	root   string
	logger *slog.Logger
}

// New returns a Store rooted at root, which it creates on first use. A nil
// logger uses the default.
func New(root string, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}

	return &Store{root: root, logger: logger}
}

func (s *Store) dir(runID string) string {
	return filepath.Join(s.root, unsafe.ReplaceAllString(runID, "-"))
}

// Create makes the run's fresh 0700 dir and returns its path. It fails when the
// dir exists, so a run never inherits another's files.
func (s *Store) Create(runID string) (string, error) {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return "", fmt.Errorf("create run inputs root: %w", err)
	}

	dir := s.dir(runID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create run input dir: %w", err)
	}

	return dir, nil
}

// Remove deletes the run's dir. Idempotent.
func (s *Store) Remove(runID string) {
	if err := os.RemoveAll(s.dir(runID)); err != nil {
		s.logger.Warn("remove run input dir failed", "run_id", runID, "error", err)
	}
}

// CleanupOrphans removes the whole root. Call it once at start: a fresh
// process tracks no runs, so every dir under it is a leftover.
func (s *Store) CleanupOrphans() error {
	if err := os.RemoveAll(s.root); err != nil {
		return fmt.Errorf("sweep run input dirs: %w", err)
	}

	return nil
}
