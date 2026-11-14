package runkind

import (
	"os"
	"path/filepath"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkBranch(t *testing.T) {
	tests := []struct {
		name  string
		card  string
		heads []string
		want  string
	}{
		{"plain branch wins", "ABC-12", []string{"cm/abc-12", "cm/abc-123", "cm/abc-12-PROJ-7"}, "cm/abc-12"},
		{"the one keyed branch", "ABC-12", []string{"cm/abc-123", "cm/abc-12-PROJ-7"}, "cm/abc-12-PROJ-7"},
		{"a key with an underscore", "ABC-12", []string{"cm/abc-12-MY_PROJ-7"}, "cm/abc-12-MY_PROJ-7"},
		{"not a key", "ABC-12", []string{"cm/abc-12-notakey"}, ""},
		{"two keyed branches", "ABC-12", []string{"cm/abc-12-PROJ-7", "cm/abc-12-PROJ-8"}, ""},
		{"another card's branch", "ABC-12", []string{"cm/abc-123"}, ""},
		{"no heads", "ABC-12", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, workBranch(tt.card, tt.heads))
		})
	}
}

func TestCardFileID(t *testing.T) {
	tests := []struct {
		name, file, want string
	}{
		{"frontmatter id", "---\nid: ABC-13\ntitle: x\n---\nbody", "ABC-13"},
		{"no frontmatter", "no frontmatter", ""},
		{"no id", "---\ntitle: x\n---\nbody", ""},
		{"unclosed frontmatter", "---\nid: ABC-13\n", ""},
		{"invalid yaml", "---\nid: [ABC-13\n---\n", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cardFileID(tt.file))
		})
	}
}

func TestWriteInputs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "investigation")
	in := protocol.InvestigateInput{
		Card:     "---\nid: ABC-12\n---\ncard body\n",
		Subtasks: []string{"---\nid: ABC-13\n---\nfirst\n", "no frontmatter\n"},
		Siblings: []string{"---\nid: ABC-20\n---\nsibling\n", "---\nid: ../../escape\n---\nbad id\n"},
	}

	require.NoError(t, writeInputs(root, in.Card, in.Subtasks, in.Siblings))

	want := map[string]string{
		"card.md":            in.Card,
		"subtasks/ABC-13.md": in.Subtasks[0],
		"subtasks/2.md":      in.Subtasks[1],
		"siblings/ABC-20.md": in.Siblings[0],
		"siblings/2.md":      in.Siblings[1],
	}

	for rel, body := range want {
		path := filepath.Join(root, rel)

		got, err := os.ReadFile(path)
		require.NoError(t, err, rel)
		assert.Equal(t, body, string(got), rel)

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o222, "%s is read-only", rel)
	}

	assert.NoFileExists(t, filepath.Join(root, "run.log"))
	assert.NoFileExists(t, filepath.Join(filepath.Dir(root), "escape.md"))
}

func TestWriteInputsWithoutSubtasksOrSiblings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "investigation")
	require.NoError(t, writeInputs(root, "card\n", nil, nil))
	assert.FileExists(t, filepath.Join(root, "card.md"))
}
