// Package kata provides a small, deterministic coding task (run-length
// encoding) used to exercise the agent loop end-to-end.
package kata

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed template/*.txt
var templateFS embed.FS

// Copy writes the kata (go.mod, rle.go skeleton, rle_test.go) into dest.
// In dest the skeleton test FAILS until Encode is implemented.
func Copy(dest string) error {
	entries, err := templateFS.ReadDir("template")
	if err != nil {
		return err
	}

	for _, e := range entries {
		data, err := templateFS.ReadFile("template/" + e.Name())
		if err != nil {
			return err
		}

		// go.mod.txt -> go.mod
		name := strings.TrimSuffix(e.Name(), ".txt")
		if err := os.WriteFile(filepath.Join(dest, name), data, 0o644); err != nil { //nolint:gosec // G306: fixture copied into a workspace; repo files keep standard modes
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	return nil
}
