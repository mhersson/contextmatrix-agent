package kata

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyProducesFailingThenPassingKata(t *testing.T) {
	dest := t.TempDir()
	require.NoError(t, Copy(dest))

	assert.FileExists(t, filepath.Join(dest, "go.mod"))
	assert.FileExists(t, filepath.Join(dest, "rle.go"))
	assert.FileExists(t, filepath.Join(dest, "rle_test.go"))

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	// Skeleton must FAIL.
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dest
	require.Error(t, cmd.Run(), "skeleton kata should fail")

	// A correct implementation must PASS — proves the kata is well-formed.
	good := `package kata
import "strconv"
func Encode(s string) string {
	if s == "" { return "" }
	r := []rune(s)
	out := ""
	count := 1
	for i := 1; i <= len(r); i++ {
		if i < len(r) && r[i] == r[i-1] {
			count++
			continue
		}
		out += strconv.Itoa(count) + string(r[i-1])
		count = 1
	}
	return out
}`
	require.NoError(t, os.WriteFile(filepath.Join(dest, "rle.go"), []byte(good), 0o644))

	cmd = exec.Command("go", "test", "./...")
	cmd.Dir = dest
	require.NoError(t, cmd.Run(), "correct impl should pass")
}
