package eval

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

//go:embed fixtures
var fixturesFS embed.FS

// CoderTask provisions a skeleton with a failing hidden test; success = the check
// command exits 0 after the agent's edits.
type CoderTask struct {
	name    string
	fixture string // path within fixturesFS, e.g. "fixtures/coder/sumlist"
	check   string // shell command run in the workspace, e.g. "go test ./..."
}

func (t CoderTask) Name() string        { return t.name }
func (t CoderTask) Role() registry.Role { return registry.RoleCoder }

func (t CoderTask) Prompt() string {
	return "A test in this workspace is failing. Implement the missing code so the tests pass, then run them with bash to confirm. Reply with a short confirmation and no tool call when green."
}

func (t CoderTask) Provision(dir string) error { return copyEmbedded(t.fixture, dir) }

func (t CoderTask) Check(ctx context.Context, dir string, _ harness.Result) (harness.Verdict, error) {
	return runCommand(ctx, dir, t.check)
}

// copyEmbedded writes every file under root (in fixturesFS) into dest, stripping a
// trailing ".txt" (go.mod.txt -> go.mod) so fixture Go files stay out of the
// parent module's build.
func copyEmbedded(root, dest string) error {
	return fs.WalkDir(fixturesFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		data, err := fixturesFS.ReadFile(p)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		out := filepath.Join(dest, strings.TrimSuffix(rel, ".txt"))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}

		return os.WriteFile(out, data, 0o644)
	})
}

// runCommand runs a shell command in dir; exit 0 = OK, non-zero exit = not-OK with
// the combined output as detail (other errors propagate).
func runCommand(ctx context.Context, dir, command string) (harness.Verdict, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return harness.Verdict{OK: false, Detail: strings.TrimSpace(string(out))}, nil
		}

		return harness.Verdict{}, fmt.Errorf("check command: %w", err)
	}

	return harness.Verdict{OK: true, Detail: strings.TrimSpace(string(out))}, nil
}
