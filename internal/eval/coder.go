package eval

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
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
	// writable lists the workspace-relative files (".txt" stripped) the model is
	// meant to edit, e.g. ["sumlist.go"]. Every OTHER provisioned file — hidden
	// tests, go.mod, and shared helpers like lru/list.go — is hashed and re-verified
	// before go test (a whitelist, not a blacklist: anything not explicitly writable
	// is protected). Empty means nothing is writable, so any edit is tampering.
	writable []string
}

// tamperedDetailPrefix marks a Verdict that failed because the run tampered with
// protected fixture files rather than because the solution was wrong. runner.go keys
// Outcome.Tampered on this prefix so the merge step can distinguish the two failures.
const tamperedDetailPrefix = "tampered:"

func (t CoderTask) Name() string        { return t.name }
func (t CoderTask) Role() registry.Role { return registry.RoleCoder }

func (t CoderTask) Prompt() string {
	return "A test in this workspace is failing. Implement the missing code so the tests pass, then run them with bash to confirm. Reply with a short confirmation and no tool call when green."
}

func (t CoderTask) Provision(dir string) error { return copyEmbedded(t.fixture, dir) }

func (t CoderTask) Check(ctx context.Context, dir string, _ harness.Result) (harness.Verdict, error) {
	// A coder run is only honest if the model changed nothing but its designated
	// solution file(s). Everything else — hidden tests, go.mod, shared helpers — is
	// hashed from the tamper-proof embedded fixture and re-verified before go test.
	// Two attack surfaces: editing a protected file, and ADDING a new *_test.go that
	// short-circuits the hidden test (e.g. a TestMain that exits 0). Both fail the run.
	writable := make(map[string]bool, len(t.writable))
	for _, w := range t.writable {
		writable[w] = true
	}

	recorded, err := recordFixtureHashes(t.fixture, writable)
	if err != nil {
		return harness.Verdict{}, fmt.Errorf("hash fixture %s: %w", t.fixture, err)
	}

	edited, err := verifyFixtureIntegrity(dir, recorded)
	if err != nil {
		return harness.Verdict{}, fmt.Errorf("verify integrity %s: %w", t.fixture, err)
	}

	added, err := addedTestFiles(dir, recorded)
	if err != nil {
		return harness.Verdict{}, fmt.Errorf("scan added tests %s: %w", t.fixture, err)
	}

	if len(edited) > 0 || len(added) > 0 {
		// Restore edited/deleted originals and remove added cheat tests so the failure
		// is attributable to tampering — not to broken tests — and a later inspection
		// or re-run sees the clean fixture.
		if err := restoreFixtureFiles(t.fixture, dir, edited); err != nil {
			return harness.Verdict{}, fmt.Errorf("restore tampered files %s: %w", t.fixture, err)
		}

		if err := removeFiles(dir, added); err != nil {
			return harness.Verdict{}, fmt.Errorf("remove added tests %s: %w", t.fixture, err)
		}

		// Fresh slice: edited was already handed to restoreFixtureFiles, so appending
		// into its backing array would alias what that call saw.
		altered := make([]string, 0, len(edited)+len(added))
		altered = append(altered, edited...)
		altered = append(altered, added...)

		return harness.Verdict{
			OK:     false,
			Detail: fmt.Sprintf("%s protected fixture files altered: %s", tamperedDetailPrefix, strings.Join(altered, ", ")),
		}, nil
	}

	return runCommand(ctx, dir, t.check)
}

// recordFixtureHashes returns the sha256 of every PROTECTED file in the embedded
// fixture under root, keyed by workspace-relative path (".txt" suffix stripped to
// match what Provision wrote). Protected = every fixture file except those named in
// writable (the model's solution file[s]). This whitelist covers hidden tests,
// go.mod/go.sum, and shared helpers without enumerating file types. The embedded
// fixture is the tamper-proof source of truth for what those files must contain.
func recordFixtureHashes(root string, writable map[string]bool) (map[string][32]byte, error) {
	hashes := map[string][32]byte{}

	err := fs.WalkDir(fixturesFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		rel = strings.TrimSuffix(rel, ".txt")
		if writable[rel] {
			return nil
		}

		data, err := fixturesFS.ReadFile(p)
		if err != nil {
			return err
		}

		hashes[rel] = sha256.Sum256(data)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return hashes, nil
}

// verifyFixtureIntegrity re-hashes the protected fixture files before verification.
// A model that edits them has not solved the task: the run fails and the originals
// are restored so the failure is attributable to tampering, not to broken tests. It
// returns the workspace-relative paths whose on-disk content no longer matches the
// recorded hash (including any that were deleted), sorted for a stable detail message.
func verifyFixtureIntegrity(dir string, recorded map[string][32]byte) (tampered []string, err error) {
	for rel, want := range recorded {
		data, readErr := os.ReadFile(filepath.Join(dir, rel))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				tampered = append(tampered, rel) // deletion is tampering too

				continue
			}

			return nil, readErr
		}

		if sha256.Sum256(data) != want {
			tampered = append(tampered, rel)
		}
	}

	sort.Strings(tampered)

	return tampered, nil
}

// addedTestFiles returns workspace-relative `*_test.go` paths present on disk but
// absent from recorded (i.e. not shipped in the fixture). Adding a hidden test is the
// one way a model can cheat without editing a protected file: a new TestMain that
// exits 0 pre-empts the real test. Added NON-test files are ignored — Go forbids
// duplicate symbols so they cannot shadow the solution, and the imports the hidden
// test exercises are fixed in the (protected) test file.
func addedTestFiles(dir string, recorded map[string][32]byte) ([]string, error) {
	var added []string

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}

		if _, ok := recorded[rel]; !ok {
			added = append(added, rel)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(added)

	return added, nil
}

// restoreFixtureFiles rewrites the named protected files in dir from their embedded
// originals (re-applying the ".txt" strip), leaving the workspace with clean tests.
func restoreFixtureFiles(root, dir string, rels []string) error {
	for _, rel := range rels {
		// embed.FS keys are always slash-separated regardless of OS; use path.Join,
		// not filepath.Join, so this resolves correctly off-Linux too.
		data, err := fixturesFS.ReadFile(path.Join(root, rel+".txt"))
		if err != nil {
			return fmt.Errorf("read embedded original %s: %w", rel, err)
		}

		out := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}

		if err := os.WriteFile(out, data, 0o644); err != nil {
			return err
		}
	}

	return nil
}

// removeFiles deletes the named workspace-relative files (added cheat tests, which
// have no embedded original to restore).
func removeFiles(dir string, rels []string) error {
	for _, rel := range rels {
		if err := os.Remove(filepath.Join(dir, rel)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return nil
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
