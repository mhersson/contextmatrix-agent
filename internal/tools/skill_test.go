package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSkill creates <root>/<name>/SKILL.md with the given description + body.
func writeSkill(t *testing.T, root, name, desc, body string) {
	t.Helper()

	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
}

func TestSkillToolMenuAllWhenUnset(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-development", "Use when writing Go.", "GO BODY")
	writeSkill(t, root, "documentation", "Use when writing docs.", "DOC BODY")

	st, ok := NewSkillTool(root, nil, false, nil)
	require.True(t, ok, "a populated dir with no subset yields a usable tool")

	desc := st.Schema().Function.Description
	assert.Contains(t, desc, "go-development", "menu lists every skill when no subset is set")
	assert.Contains(t, desc, "documentation")
	assert.Contains(t, desc, "Use when writing Go.", "menu carries descriptions for model-driven selection")
}

func TestSkillToolMenuRespectsSubset(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-development", "Use when writing Go.", "GO BODY")
	writeSkill(t, root, "documentation", "Use when writing docs.", "DOC BODY")

	st, ok := NewSkillTool(root, []string{"go-development"}, true, nil)
	require.True(t, ok)

	desc := st.Schema().Function.Description
	assert.Contains(t, desc, "go-development")
	assert.NotContains(t, desc, "documentation", "subset excludes unlisted skills")
}

func TestSkillToolEmptyMenuReturnsNotOk(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-development", "Use when writing Go.", "GO BODY")

	_, ok := NewSkillTool(root, []string{}, true, nil)
	assert.False(t, ok, "an explicit empty subset yields no tool")

	_, ok = NewSkillTool(t.TempDir(), nil, false, nil)
	assert.False(t, ok, "an empty dir yields no tool")
}

func TestSkillToolLoadReturnsBodyAndFiresOnEngage(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-development", "Use when writing Go.", "GO BODY CONTENT")

	var engaged []string

	st, ok := NewSkillTool(root, nil, false, func(_ context.Context, name string) error {
		engaged = append(engaged, name)

		return nil
	})
	require.True(t, ok)

	out, err := st.Execute(context.Background(), map[string]any{"skill": "go-development"})
	require.NoError(t, err)
	assert.Contains(t, out, "GO BODY CONTENT", "load returns the SKILL.md body")
	assert.Equal(t, []string{"go-development"}, engaged, "loading a skill fires onEngage once")
}

func TestSkillToolUnknownSkillReturnsGuidanceNotError(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-development", "Use when writing Go.", "GO BODY")

	st, ok := NewSkillTool(root, nil, false, nil)
	require.True(t, ok)

	out, err := st.Execute(context.Background(), map[string]any{"skill": "nope"})
	require.NoError(t, err, "an unknown skill is guidance, not a turn-ending error")
	assert.Contains(t, out, "go-development", "guidance lists the available skills")
}

func TestSkillToolRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-development", "Use when writing Go.", "GO BODY")
	// A secret outside the skills root.
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(root), "secret"), []byte("TOPSECRET"), 0o644))

	st, ok := NewSkillTool(root, nil, false, nil)
	require.True(t, ok)

	// A traversal via the optional file argument must be rejected (resolveInRoot
	// jails reads to the skill dir); it must never read outside it.
	_, err := st.Execute(context.Background(), map[string]any{"skill": "go-development", "file": "../../secret"})
	require.Error(t, err, "path traversal must be rejected")
}

func TestSkillToolLoadsSupportingFile(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "tdd", "Use for TDD.", "MAIN")
	require.NoError(t, os.WriteFile(filepath.Join(root, "tdd", "anti-patterns.md"), []byte("ANTIPATTERNS"), 0o644))

	st, ok := NewSkillTool(root, nil, false, nil)
	require.True(t, ok)

	out, err := st.Execute(context.Background(), map[string]any{"skill": "tdd", "file": "anti-patterns.md"})
	require.NoError(t, err)
	assert.Contains(t, out, "ANTIPATTERNS", "the optional file arg loads a supporting file within the skill dir")
}

func TestSkillToolName(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-development", "d", "b")
	st, _ := NewSkillTool(root, nil, false, nil)
	assert.Equal(t, "skill", st.Name())
	assert.Equal(t, "skill", st.Schema().Function.Name)
	assert.Contains(t, string(st.Schema().Function.Parameters), `"skill"`, "schema declares the skill param")
}

func TestSkillToolDescriptionBoundedToFrontmatter(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "go-development")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	// Frontmatter has the real description; the BODY also has a description: line
	// that must be ignored (the scan must stop at the closing ---).
	content := "---\nname: go-development\ndescription: REAL frontmatter desc.\n---\n\n## Notes\ndescription: BODY decoy that must be ignored\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))

	st, ok := NewSkillTool(root, nil, false, nil)
	require.True(t, ok)

	desc := st.Schema().Function.Description
	assert.Contains(t, desc, "REAL frontmatter desc.", "description comes from frontmatter")
	assert.NotContains(t, desc, "BODY decoy", "scan stops at the closing --- and ignores the body")
}

func TestParseSkillDescriptionStopsAtClosingDelimiter(t *testing.T) {
	// Frontmatter has NO description; a decoy description: lives in the body.
	// The old continue-based scan leaked the body line ("decoy", true); the
	// fixed break-at-closing-delimiter scan must return ("", false).
	data := []byte("---\nname: x\n---\n\n## Body\ndescription: decoy that must not be read\n")

	desc, ok := parseSkillDescription(data)
	assert.False(t, ok, "no frontmatter description must yield ok=false")
	assert.Empty(t, desc, "the body description: line must not be read once past the closing ---")
}

func TestSkillToolMenuText(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-development", "Use when writing Go.", "B")
	writeSkill(t, root, "documentation", "Use when writing docs.", "B")

	st, ok := NewSkillTool(root, nil, false, nil)
	require.True(t, ok)

	menu := st.MenuText()
	assert.Contains(t, menu, "- go-development: Use when writing Go.\n")
	assert.Contains(t, menu, "- documentation: Use when writing docs.\n")
	// Schema reuses MenuText, so its description must contain exactly those lines.
	assert.Contains(t, st.Schema().Function.Description, menu)
}
