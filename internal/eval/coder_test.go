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
	task := CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./..."}
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

func harnessZero() harness.Result { return harness.Result{} }
