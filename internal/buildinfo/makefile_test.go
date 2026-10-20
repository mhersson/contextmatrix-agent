package buildinfo

import (
	"bufio"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-protocol/changelog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Makefile's git log args and -X paths are a wire contract that
// changelog.Parse and the linker consume, so these tests pin its text.

type pkgMarker struct{}

func makefileLines(t *testing.T) []string {
	t.Helper()

	f, err := os.Open("../../Makefile")
	require.NoError(t, err)

	t.Cleanup(func() { _ = f.Close() })

	var lines []string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}

	require.NoError(t, sc.Err())

	return lines
}

// historyCommand returns the history recipe's git invocation, up to its
// output redirect.
func historyCommand(t *testing.T) []string {
	t.Helper()

	for _, line := range makefileLines(t) {
		if !strings.HasPrefix(line, "\tgit log") {
			continue
		}

		var cmd []string

		for field := range strings.FieldsSeq(line) {
			if field == ">" {
				break
			}

			cmd = append(cmd, field)
		}

		return cmd
	}

	t.Fatal("no git log recipe line in the Makefile")

	return nil
}

// variable returns the value of the Makefile variable name, trimmed.
func variable(t *testing.T, name string) string {
	t.Helper()

	for _, line := range makefileLines(t) {
		key, value, ok := strings.Cut(line, ":=")
		if ok && strings.TrimSpace(key) == name {
			return strings.TrimSpace(value)
		}
	}

	t.Fatalf("no %s := line in the Makefile", name)

	return ""
}

func TestMakefile_HistoryRecipeRunsGitLogArgs(t *testing.T) {
	assert.Equal(t, append([]string{"git"}, changelog.GitLogArgs...), historyCommand(t))
}

func TestMakefile_LdflagsStampBuildinfoVars(t *testing.T) {
	assert.Equal(t, reflect.TypeFor[pkgMarker]().PkgPath(), variable(t, "BUILDINFO"))

	ldflags := variable(t, "LDFLAGS")
	for _, name := range []string{"version", "commit", "buildTime"} {
		assert.Contains(t, ldflags, "-X $(BUILDINFO)."+name+"=")
	}
}
