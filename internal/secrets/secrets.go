// Package secrets stages worker secrets on the host and reads them in the
// container. The host side mirrors the runner: a shared env file, rewritten
// before the GitHub token expires, bind-mounted read-only into workers.
package secrets

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// TokenGenerator matches the githubauth generator surface so *githubauth.AppProvider
// and *githubauth.PATProvider satisfy it without an adapter.
type TokenGenerator interface {
	GenerateToken(ctx context.Context) (token string, expiresAt time.Time, err error)
}

// Source holds key-value pairs parsed from an env file.
type Source struct{ vals map[string]string }

// Open parses a KEY=value env file. Blank lines and lines beginning with '#'
// are ignored. Values may contain '=' characters.
func Open(path string) (*Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open env file: %w", err)
	}

	defer func() { _ = f.Close() }()

	vals := make(map[string]string)
	sc := bufio.NewScanner(f)

	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		vals[k] = v
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan env file: %w", err)
	}

	return &Source{vals: vals}, nil
}

// Get returns the value for key, or "" if not present.
func (s *Source) Get(key string) string {
	return s.vals[key]
}

// For returns the same source. The per-run parameter is reserved for a future
// multi-user mode; it is intentionally ignored today.
func (s *Source) For(_ string) *Source { return s }

// WriteEnvFile writes vals to path atomically (write-tmp + rename).
// The directory is created with mode 0700; the file is written with mode 0600.
// Lines are written in deterministic order: OPENROUTER_API_KEY first, then
// CM_GIT_TOKEN, to make content predictable for tests and operators.
func WriteEnvFile(path string, vals map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}

	// Build content in fixed order.
	var sb strings.Builder

	for _, k := range []string{"OPENROUTER_API_KEY", "CM_GIT_TOKEN"} {
		if v, ok := vals[k]; ok {
			sb.WriteString(k)
			sb.WriteByte('=')
			sb.WriteString(v)
			sb.WriteByte('\n')
		}
	}

	// Any other keys in sorted order — map iteration is randomized, and the
	// output must be byte-identical across rewrites.
	known := map[string]bool{"OPENROUTER_API_KEY": true, "CM_GIT_TOKEN": true}
	for _, k := range slices.Sorted(maps.Keys(vals)) {
		if !known[k] {
			sb.WriteString(k)
			sb.WriteByte('=')
			sb.WriteString(vals[k])
			sb.WriteByte('\n')
		}
	}

	// Write to a temp file in the same dir so rename is atomic on Linux.
	tmp, err := os.CreateTemp(dir, ".env-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	tmpPath := tmp.Name()

	if _, err := tmp.WriteString(sb.String()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return fmt.Errorf("write env file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("chmod env file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("rename env file: %w", err)
	}

	return nil
}

// Refresher writes the env file immediately on Run, then rewrites it
// refreshBefore ahead of each token expiry. OPENROUTER_API_KEY is static and
// persists across every rewrite.
type Refresher struct {
	path          string
	openRouterKey string
	gen           TokenGenerator
	logger        *slog.Logger
	refreshBefore time.Duration // default 10m; override in tests
	minSleep      time.Duration // floor on sleep; default 30s; override in tests
}

const (
	defaultRefreshBefore = 10 * time.Minute
	defaultMinSleep      = 30 * time.Second
	retryBackoff         = 5 * time.Second
)

// NewRefresher constructs a Refresher. Pass nil for logger to use the default.
func NewRefresher(path, openRouterKey string, gen TokenGenerator, logger *slog.Logger) *Refresher {
	if logger == nil {
		logger = slog.Default()
	}

	return &Refresher{
		path:          path,
		openRouterKey: openRouterKey,
		gen:           gen,
		logger:        logger,
		refreshBefore: defaultRefreshBefore,
		minSleep:      defaultMinSleep,
	}
}

// Run writes the env file immediately, then rewrites it ahead of each expiry.
// Blocks until ctx is done; returns nil on clean shutdown.
func (r *Refresher) Run(ctx context.Context) error {
	for {
		token, expiresAt, err := r.gen.GenerateToken(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			r.logger.Error("generate token failed, retrying", "error", err, "backoff", retryBackoff)

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retryBackoff):
				continue
			}
		}

		vals := map[string]string{
			"CM_GIT_TOKEN": token,
		}
		if r.openRouterKey != "" {
			vals["OPENROUTER_API_KEY"] = r.openRouterKey
		}

		if err := WriteEnvFile(r.path, vals); err != nil {
			r.logger.Error("write env file failed", "error", err)
			// Don't crash; stale file still has the previous token.
		} else {
			r.logger.Info("env file written", "expires_at", expiresAt)
		}

		sleep := time.Until(expiresAt) - r.refreshBefore
		if sleep < r.minSleep {
			sleep = r.minSleep
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sleep):
		}
	}
}
