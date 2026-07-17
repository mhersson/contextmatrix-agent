// Package secrets stages worker secrets on the host and reads them in the
// container. The host side stages one env file per run from CM-provisioned
// credentials, rewritten before the git token expires, bind-mounted read-only
// into the worker.
package secrets

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

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

// EndpointSecrets is the static (non-rotating) LLM endpoint config staged into
// the worker secrets file on every rewrite.
type EndpointSecrets struct {
	APIKey  string
	BaseURL string
	Type    string
	// MobGuests is the compact JSON guest-spec list ([]protocol.GuestSpec on
	// the wire) for mob session discussions. Guests carry bearer tokens, so
	// they ride the secrets file - and living here (not a one-off append)
	// they survive every token-refresh rewrite. Empty = no guests; key
	// omitted.
	MobGuests string
}

// endpointVals assembles the worker env map from a git token and the (optional)
// LLM endpoint values. Empty endpoint fields are omitted so they never appear in
// the env file. Used by the per-run refresher.
func endpointVals(token string, e EndpointSecrets) map[string]string {
	vals := map[string]string{"CM_GIT_TOKEN": token}

	if e.APIKey != "" {
		vals["LLM_API_KEY"] = e.APIKey
	}

	if e.BaseURL != "" {
		vals["LLM_BASE_URL"] = e.BaseURL
	}

	if e.Type != "" {
		vals["LLM_TYPE"] = e.Type
	}

	if e.MobGuests != "" {
		vals["CM_MOB_GUESTS"] = e.MobGuests
	}

	return vals
}

// WriteEnvFile writes vals to path atomically (write-tmp + rename).
// The directory is created with mode 0700; the file is written with mode 0600.
// Lines are written in deterministic order: LLM_API_KEY, LLM_BASE_URL, LLM_TYPE,
// CM_GIT_TOKEN first, then any extra keys in sorted order - output is
// byte-identical across rewrites.
func WriteEnvFile(path string, vals map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}

	// Build content in fixed order.
	var sb strings.Builder

	for _, k := range []string{"LLM_API_KEY", "LLM_BASE_URL", "LLM_TYPE", "CM_GIT_TOKEN"} {
		if v, ok := vals[k]; ok {
			sb.WriteString(k)
			sb.WriteByte('=')
			sb.WriteString(v)
			sb.WriteByte('\n')
		}
	}

	// Any other keys in sorted order - map iteration is randomized, and the
	// output must be byte-identical across rewrites.
	known := map[string]bool{"LLM_API_KEY": true, "LLM_BASE_URL": true, "LLM_TYPE": true, "CM_GIT_TOKEN": true}
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

// Refresh-loop timing shared with the per-run refresher (runrefresher.go).
const (
	defaultRefreshBefore = 10 * time.Minute
	defaultMinSleep      = 30 * time.Second
	defaultRetryBackoff  = 5 * time.Second
)
