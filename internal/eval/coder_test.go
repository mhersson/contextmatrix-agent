package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoderTaskProvisionAndCheck(t *testing.T) {
	task := CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./...", writable: []string{"sumlist.go"}}
	dir := t.TempDir()
	require.NoError(t, task.Provision(dir))

	// Skeleton is provisioned and the hidden test fails as shipped.
	assert.FileExists(t, filepath.Join(dir, "sumlist.go"))
	assert.FileExists(t, filepath.Join(dir, "go.mod"))
	v, err := task.Check(context.Background(), dir, harnessZero())
	require.NoError(t, err)
	assert.False(t, v.OK, "unimplemented skeleton should fail its test")

	// Implementing Sum makes the check pass.
	impl := "package sumlist\n\nfunc Sum(xs []int) int {\n\ttotal := 0\n\tfor _, x := range xs {\n\t\ttotal += x\n\t}\n\treturn total\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sumlist.go"), []byte(impl), 0o644))
	v, err = task.Check(context.Background(), dir, harnessZero())
	require.NoError(t, err)
	assert.True(t, v.OK, "implemented code should pass: %s", v.Detail)
}

// TestCoderTaskCheckFailsOnTamperedTest proves the integrity guard: a model that
// edits the hidden test (here, overwriting it with a vacuous always-pass test
// between provisioning and verification) must NOT score a pass. Check re-hashes the
// provisioned test files; on mismatch it fails the run with a "tampered" marker and
// restores the originals so the failure is attributable to tampering, not the model.
func TestCoderTaskCheckFailsOnTamperedTest(t *testing.T) {
	task := CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./...", writable: []string{"sumlist.go"}}
	dir := t.TempDir()
	require.NoError(t, task.Provision(dir))

	// Solve the task legitimately so a non-tampered run would pass...
	impl := "package sumlist\n\nfunc Sum(xs []int) int {\n\ttotal := 0\n\tfor _, x := range xs {\n\t\ttotal += x\n\t}\n\treturn total\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sumlist.go"), []byte(impl), 0o644))

	// ...then cheat by overwriting the hidden test with a vacuous one that always
	// passes regardless of Sum's correctness.
	testPath := filepath.Join(dir, "sumlist_test.go")
	cheat := "package sumlist\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {}\n"
	require.NoError(t, os.WriteFile(testPath, []byte(cheat), 0o644))

	v, err := task.Check(context.Background(), dir, harnessZero())
	require.NoError(t, err)
	assert.False(t, v.OK, "a run that edits the hidden test must not pass")
	assert.Contains(t, v.Detail, "tampered", "outcome detail must mark the run as tampered: %q", v.Detail)

	// The guard must restore the original test from the embedded fixture so a later
	// inspection or re-run sees clean tests, not the model's vacuous cheat.
	restored, err := os.ReadFile(testPath)
	require.NoError(t, err)
	assert.NotContains(t, string(restored), "func TestSum(t *testing.T) {}", "original test must be restored")
	assert.Contains(t, string(restored), "Sum(c.in)", "restored test must be the embedded original")
}

// TestCoderTaskCheckFailsOnTamperedGoMod proves the guard also covers go.mod: a model
// could weaken the module declaration to dodge a failing build/test, so go.mod is
// hashed and restored alongside the hidden tests.
func TestCoderTaskCheckFailsOnTamperedGoMod(t *testing.T) {
	task := CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./...", writable: []string{"sumlist.go"}}
	dir := t.TempDir()
	require.NoError(t, task.Provision(dir))

	impl := "package sumlist\n\nfunc Sum(xs []int) int {\n\ttotal := 0\n\tfor _, x := range xs {\n\t\ttotal += x\n\t}\n\treturn total\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sumlist.go"), []byte(impl), 0o644))

	// Rewrite go.mod (here, a trailing comment) — still builds/tests fine, so absent
	// the guard the run would pass; the guard must catch the edit regardless.
	gomodPath := filepath.Join(dir, "go.mod")
	require.NoError(t, os.WriteFile(gomodPath, []byte("module sumlist\n\ngo 1.26 // tampered\n"), 0o644))

	v, err := task.Check(context.Background(), dir, harnessZero())
	require.NoError(t, err)
	assert.False(t, v.OK, "a run that edits go.mod must not pass")
	assert.Contains(t, v.Detail, "tampered", "outcome detail must mark the run as tampered: %q", v.Detail)
}

// TestCoderTaskCheckHonestRunUnaffected guards against false positives: a run that
// touches only the solution file passes exactly as before, with no tamper marker.
func TestCoderTaskCheckHonestRunUnaffected(t *testing.T) {
	task := CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./...", writable: []string{"sumlist.go"}}
	dir := t.TempDir()
	require.NoError(t, task.Provision(dir))

	impl := "package sumlist\n\nfunc Sum(xs []int) int {\n\ttotal := 0\n\tfor _, x := range xs {\n\t\ttotal += x\n\t}\n\treturn total\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sumlist.go"), []byte(impl), 0o644))

	v, err := task.Check(context.Background(), dir, harnessZero())
	require.NoError(t, err)
	assert.True(t, v.OK, "an honest run should pass: %s", v.Detail)
	assert.NotContains(t, v.Detail, "tampered", "honest run must not be flagged as tampered")
}

// TestCoderTaskCheckFailsOnAddedTestFile closes the universal bypass: a model can
// not pre-empt the hidden test by ADDING a new *_test.go (here a TestMain that exits
// 0 before any real test runs). The guard scans the workspace for test files absent
// from the embedded fixture and fails the run; the recorded-hash sweep alone misses
// them because they never existed at provision time.
func TestCoderTaskCheckFailsOnAddedTestFile(t *testing.T) {
	task := CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./...", writable: []string{"sumlist.go"}}
	dir := t.TempDir()
	require.NoError(t, task.Provision(dir))

	// Deliberately WRONG solution; only the cheat test would make it "pass".
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sumlist.go"), []byte("package sumlist\n\nfunc Sum(xs []int) int { return -999 }\n"), 0o644))

	cheat := "package sumlist\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(0) }\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zz_cheat_test.go"), []byte(cheat), 0o644))

	v, err := task.Check(context.Background(), dir, harnessZero())
	require.NoError(t, err)
	assert.False(t, v.OK, "adding a hidden test must not let a wrong solution pass")
	assert.Contains(t, v.Detail, "tampered", "outcome detail must mark the run as tampered: %q", v.Detail)
}

// TestCoderTaskCheckFailsOnEditedHelper closes the live-fixture bypass: lru/ ships a
// list.go helper that lru.go delegates eviction/recency to. Only lru.go is writable;
// editing the helper to neuter the test must fail. Under a blacklist (test+go.mod
// only) list.go was freely editable.
func TestCoderTaskCheckFailsOnEditedHelper(t *testing.T) {
	task := CoderTask{name: "lru", fixture: "fixtures/coder/lru", check: "go test ./...", writable: []string{"lru.go"}}
	dir := t.TempDir()
	require.NoError(t, task.Provision(dir))

	// Edit the shared helper (the model's solution file is lru.go, not list.go).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "list.go"), []byte("package lru\n\ntype node struct{ key, value int }\n"), 0o644))

	v, err := task.Check(context.Background(), dir, harnessZero())
	require.NoError(t, err)
	assert.False(t, v.OK, "editing a protected helper must not pass")
	assert.Contains(t, v.Detail, "tampered", "outcome detail must mark the run as tampered: %q", v.Detail)

	// The helper is restored from the embedded original.
	restored, err := os.ReadFile(filepath.Join(dir, "list.go"))
	require.NoError(t, err)
	assert.Contains(t, string(restored), "moveToFront", "the original helper must be restored")
}

// TestCoderTaskCheckAddedNonTestFileAllowed guards against a false positive from the
// added-test scan: adding a NON-test .go file is legitimate (Go forbids duplicate
// symbols so it cannot shadow the solution, and the test's imports are fixed in the
// protected test file). An honest run that splits its solution across files passes.
func TestCoderTaskCheckAddedNonTestFileAllowed(t *testing.T) {
	task := CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./...", writable: []string{"sumlist.go"}}
	dir := t.TempDir()
	require.NoError(t, task.Provision(dir))

	// Correct solution split across the writable file and an added helper file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sumlist.go"), []byte("package sumlist\n\nfunc Sum(xs []int) int { return total(xs) }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "helper.go"), []byte("package sumlist\n\nfunc total(xs []int) int {\n\ts := 0\n\tfor _, x := range xs {\n\t\ts += x\n\t}\n\treturn s\n}\n"), 0o644))

	v, err := task.Check(context.Background(), dir, harnessZero())
	require.NoError(t, err)
	assert.True(t, v.OK, "adding a non-test helper file is legitimate: %s", v.Detail)
	assert.NotContains(t, v.Detail, "tampered")
}

// TestVerifyFixtureIntegrity exercises the integrity helper directly: a clean
// workspace reports no tampering, a content edit and a deletion both report the
// affected path, and the returned list is sorted. Under the whitelist model every
// non-writable file is recorded — tests, go.mod, AND helpers like list.go.
func TestVerifyFixtureIntegrity(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, CoderTask{fixture: "fixtures/coder/sumlist"}.Provision(dir))

	recorded, err := recordFixtureHashes("fixtures/coder/sumlist", map[string]bool{"sumlist.go": true})
	require.NoError(t, err)
	require.Contains(t, recorded, "sumlist_test.go", "the hidden test must be recorded")
	require.Contains(t, recorded, "go.mod", "go.mod must be recorded")
	require.NotContains(t, recorded, "sumlist.go", "the writable solution file must NOT be protected")

	// Clean workspace: nothing tampered.
	tampered, err := verifyFixtureIntegrity(dir, recorded)
	require.NoError(t, err)
	assert.Empty(t, tampered)

	// Edit the test and delete go.mod: both surface, sorted.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sumlist_test.go"), []byte("package sumlist\n"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(dir, "go.mod")))
	tampered, err = verifyFixtureIntegrity(dir, recorded)
	require.NoError(t, err)
	assert.Equal(t, []string{"go.mod", "sumlist_test.go"}, tampered)
}

// TestRecordFixtureHashesProtectsHelper asserts the whitelist records non-writable
// helper source (lru/list.go) while leaving the writable solution (lru.go) free.
func TestRecordFixtureHashesProtectsHelper(t *testing.T) {
	recorded, err := recordFixtureHashes("fixtures/coder/lru", map[string]bool{"lru.go": true})
	require.NoError(t, err)
	assert.Contains(t, recorded, "list.go", "the shared helper must be protected")
	assert.Contains(t, recorded, "lru_test.go", "the hidden test must be protected")
	assert.Contains(t, recorded, "go.mod", "go.mod must be protected")
	assert.NotContains(t, recorded, "lru.go", "the writable solution file must NOT be protected")
}

// TestAddedTestFiles flags on-disk *_test.go absent from the recorded fixture and
// ignores both expected test files and added non-test files.
func TestAddedTestFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, CoderTask{fixture: "fixtures/coder/sumlist"}.Provision(dir))

	recorded, err := recordFixtureHashes("fixtures/coder/sumlist", map[string]bool{"sumlist.go": true})
	require.NoError(t, err)

	// No added test files yet (sumlist_test.go is expected/recorded).
	added, err := addedTestFiles(dir, recorded)
	require.NoError(t, err)
	assert.Empty(t, added)

	// An added non-test file is fine; an added *_test.go is flagged.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "helper.go"), []byte("package sumlist\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zz_cheat_test.go"), []byte("package sumlist\n"), 0o644))
	added, err = addedTestFiles(dir, recorded)
	require.NoError(t, err)
	assert.Equal(t, []string{"zz_cheat_test.go"}, added)
}

func harnessZero() harness.Result { return harness.Result{} }
