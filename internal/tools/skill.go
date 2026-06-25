package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

// skillNamePattern restricts a skill directory name to the same safe charset
// ContextMatrix and the runner enforce. Defense-in-depth before any path join.
var skillNamePattern = regexpMustCompileSkill()

// skillEntry is one available skill in the menu.
type skillEntry struct {
	name        string
	description string
}

// SkillTool is a read-only, model-driven engagement tool: it surfaces the
// available task-skills by name+description and loads a chosen SKILL.md on
// demand, firing onEngage so the engagement is reported. It reads only files
// under dir (the read-only skills mount), so it is safe in both the write and
// read-only registries.
type SkillTool struct {
	dir      string
	menu     []skillEntry
	onEngage func(ctx context.Context, name string) error
}

// NewSkillTool scans dir for skills (subdirectories containing SKILL.md), reads
// each name+description, applies the 3-state subset filter, and returns the tool
// plus ok=false when the resulting menu is empty (the caller then omits the
// tool, keeping no-skills runs byte-identical). allowedSet mirrors
// CM_TASK_SKILLS_SET: false → offer all; true → offer only names in allowed
// (possibly none).
func NewSkillTool(dir string, allowed []string, allowedSet bool, onEngage func(ctx context.Context, name string) error) (SkillTool, bool) {
	menu := scanSkills(dir)

	if allowedSet {
		want := make(map[string]bool, len(allowed))
		for _, a := range allowed {
			want[a] = true
		}

		filtered := menu[:0]
		for _, e := range menu {
			if want[e.name] {
				filtered = append(filtered, e)
			}
		}

		menu = filtered
	}

	if len(menu) == 0 {
		return SkillTool{}, false
	}

	return SkillTool{dir: dir, menu: menu, onEngage: onEngage}, true
}

func (t SkillTool) Name() string { return "skill" }

// MenuText returns the available skills as one "- name: description" line each,
// newline-terminated. Single source for both the tool's schema menu and the
// orchestrator's prompt-injected skill list, so the two cannot drift.
func (t SkillTool) MenuText() string {
	var b strings.Builder
	for _, e := range t.menu {
		b.WriteString("- ")
		b.WriteString(e.name)
		b.WriteString(": ")
		b.WriteString(e.description)
		b.WriteString("\n")
	}

	return b.String()
}

func (t SkillTool) Schema() llm.Tool {
	var b strings.Builder
	b.WriteString("Engage a specialist task-skill before doing role/language work. ")
	b.WriteString("Call this with the skill name to load its full instructions; ")
	b.WriteString("optionally pass 'file' to load a supporting file the skill references. ")
	b.WriteString("Available skills:\n")
	b.WriteString(t.MenuText())

	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        "skill",
		Description: b.String(),
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"skill":{"type":"string","description":"the name of the skill to load (one of the available skills listed above)"},
				"file":{"type":"string","description":"optional supporting file within the skill to load instead of SKILL.md"}
			},
			"required":["skill"]
		}`),
	}}
}

func (t SkillTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	name, err := requireString(args, "skill")
	if err != nil {
		return "", err
	}

	if !t.inMenu(name) {
		// Unknown name: return the menu as guidance rather than a turn-ending
		// error, so the model can correct itself.
		return "unknown skill " + name + ". " + t.menuText(), nil
	}

	file, _ := args["file"].(string)
	if file == "" {
		body, rerr := os.ReadFile(filepath.Join(t.dir, name, "SKILL.md")) //nolint:gosec // name is menu-validated
		if rerr != nil {
			return "", fmt.Errorf("read skill %q: %w", name, rerr)
		}

		// Engagement = loading the skill itself. Best-effort: a report failure
		// must not fail the load (CM dedups; the model still has the skill).
		if t.onEngage != nil {
			_ = t.onEngage(ctx, name)
		}

		return string(body), nil
	}

	// Supporting file: jail it to the skill's own directory.
	abs, rerr := resolveInRoot(filepath.Join(t.dir, name), file)
	if rerr != nil {
		return "", rerr
	}

	data, rerr := os.ReadFile(abs) //nolint:gosec // abs is resolveInRoot-jailed to the skill dir
	if rerr != nil {
		return "", fmt.Errorf("read skill file %q/%q: %w", name, file, rerr)
	}

	return string(data), nil
}

func (t SkillTool) inMenu(name string) bool {
	for _, e := range t.menu {
		if e.name == name {
			return true
		}
	}

	return false
}

func (t SkillTool) menuText() string {
	names := make([]string, 0, len(t.menu))
	for _, e := range t.menu {
		names = append(names, e.name)
	}

	return "Available skills: " + strings.Join(names, ", ") + "."
}

// scanSkills returns the skills under dir (subdirectories with a parseable
// SKILL.md), sorted by name. A missing dir or an unreadable/invalid skill is
// skipped silently — skills are advisory, never fatal.
func scanSkills(dir string) []skillEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	out := make([]skillEntry, 0, len(entries))

	for _, e := range entries {
		if !e.IsDir() || !skillNamePattern.MatchString(e.Name()) {
			continue
		}

		data, rerr := os.ReadFile(filepath.Join(dir, e.Name(), "SKILL.md")) //nolint:gosec // dir is the configured skills mount
		if rerr != nil {
			continue
		}

		desc, ok := parseSkillDescription(data)
		if !ok {
			continue
		}

		out = append(out, skillEntry{name: e.Name(), description: desc})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })

	return out
}

// parseSkillDescription extracts the single-line `description:` from a SKILL.md
// YAML frontmatter block without pulling in a YAML dependency. Returns ok=false
// if there is no frontmatter or no description.
func parseSkillDescription(data []byte) (string, bool) {
	s := string(data)
	s = strings.TrimPrefix(s, "\uFEFF")

	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", false
	}

	// Bound the scan to the frontmatter block (up to the closing ---).
	rest := s

	inFrontmatter := false

	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimRight(line, "\r")

		if !inFrontmatter {
			inFrontmatter = true // first line is the opening ---

			continue
		}

		if strings.TrimSpace(line) == "---" {
			break // closing delimiter — stop; do not scan the body
		}

		if after, ok := strings.CutPrefix(line, "description:"); ok {
			return strings.TrimSpace(after), true
		}
	}

	return "", false
}

func regexpMustCompileSkill() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
}
