package runkind

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/cardid"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"gopkg.in/yaml.v3"
)

// writeInputs writes the investigation's card files under root, read-only:
// card.md, and subtasks/<id>.md and siblings/<id>.md, named <n>.md (1-based)
// when a file has no card ID. The run log stays where serve mounted it.
func writeInputs(root string, in protocol.InvestigateInput) error {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", root, err)
	}

	if err := writeReadOnly(filepath.Join(root, "card.md"), in.Card); err != nil {
		return err
	}

	if err := writeCards(filepath.Join(root, "subtasks"), in.Subtasks); err != nil {
		return err
	}

	return writeCards(filepath.Join(root, "siblings"), in.Siblings)
}

func writeCards(dir string, files []string) error {
	if len(files) == 0 {
		return nil
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	for i, f := range files {
		// The ID names a file, so only a card ID's safe shape is used.
		name := cardFileID(f)
		if !cardid.Valid(name) {
			name = strconv.Itoa(i + 1)
		}

		if err := writeReadOnly(filepath.Join(dir, name+".md"), f); err != nil {
			return err
		}
	}

	return nil
}

func writeReadOnly(path, body string) error {
	if err := os.WriteFile(path, []byte(body), 0o444); err != nil { //nolint:gosec // G306: read-only card files, no secrets
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// cardFileID is the id of a card file's YAML frontmatter; "" when absent.
func cardFileID(file string) string {
	rest, ok := strings.CutPrefix(file, "---\n")
	if !ok {
		return ""
	}

	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return ""
	}

	var fm struct {
		ID string `yaml:"id"`
	}

	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return ""
	}

	return fm.ID
}
