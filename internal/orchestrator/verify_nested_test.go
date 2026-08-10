package orchestrator

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkModule creates ws/<dir>/<name> with content, creating the module dir.
func mkModule(t *testing.T, ws, dir, name, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(ws, dir), 0o755))
	writeFile(t, filepath.Join(ws, dir), name, content)
}

const (
	pomXML = "<project></project>\n"
	npmPkg = `{"name":"x","scripts":{"test":"vitest run"}}`
	goMod  = "module example.com/x\n"
)

func TestDetectNested(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probing is POSIX-only")
	}

	t.Run("maven and npm compose in lexical order", func(t *testing.T) {
		stubTools(t, "mvn", "java", "npm", "bash")

		ws := t.TempDir()
		mkModule(t, ws, "backend", "pom.xml", pomXML)
		mkModule(t, ws, "frontend", "package.json", npmPkg)

		det := detectNested(ws)
		assert.Equal(t, "mvn -q -f backend/pom.xml test && npm --prefix frontend test", det.Display)
		assert.Equal(t, []string{"bash", "-c", "set -o pipefail; mvn -q -f backend/pom.xml test && npm --prefix frontend test"}, det.Argv)
		assert.False(t, det.Wrapper)
		assert.Empty(t, det.Marker)
		assert.Empty(t, det.Notes)
	})

	t.Run("single module stays plain argv", func(t *testing.T) {
		stubTools(t, "mvn", "java")

		ws := t.TempDir()
		mkModule(t, ws, "backend", "pom.xml", pomXML)

		det := detectNested(ws)
		assert.Equal(t, []string{"mvn", "-q", "-f", "backend/pom.xml", "test"}, det.Argv)
		assert.Equal(t, "mvn -q -f backend/pom.xml test", det.Display)
	})

	t.Run("system mvn preferred over wrapper", func(t *testing.T) {
		stubTools(t, "mvn", "java")

		ws := t.TempDir()
		mkModule(t, ws, "backend", "pom.xml", pomXML)
		require.NoError(t, os.WriteFile(filepath.Join(ws, "backend", "mvnw"), []byte("#!/bin/sh\n"), 0o755))

		det := detectNested(ws)
		assert.Equal(t, []string{"mvn", "-q", "-f", "backend/pom.xml", "test"}, det.Argv,
			"the wrapper bootstraps its own Maven and dies on minimal images; a runnable system mvn wins")
	})

	t.Run("wrapper fallback when no system mvn", func(t *testing.T) {
		stubTools(t, "java") // no mvn on PATH

		ws := t.TempDir()
		mkModule(t, ws, "backend", "pom.xml", pomXML)
		require.NoError(t, os.WriteFile(filepath.Join(ws, "backend", "mvnw"), []byte("#!/bin/sh\n"), 0o755))

		det := detectNested(ws)
		assert.Equal(t, []string{"backend/mvnw", "-q", "-f", "backend/pom.xml", "test"}, det.Argv)
	})

	t.Run("unresolved lone module carries the park marker", func(t *testing.T) {
		stubTools(t, "mvn") // java missing: the JVM probe fails

		ws := t.TempDir()
		mkModule(t, ws, "backend", "pom.xml", pomXML)

		det := detectNested(ws)
		assert.Nil(t, det.Argv)
		assert.Equal(t, "maven project (in backend/)", det.Marker)
		assert.Contains(t, det.Reason, "java")
	})

	t.Run("partial coverage notes the uncovered module", func(t *testing.T) {
		stubTools(t, "npm") // no mvn, no java: the backend cannot resolve

		ws := t.TempDir()
		mkModule(t, ws, "backend", "pom.xml", pomXML)
		mkModule(t, ws, "frontend", "package.json", npmPkg)

		det := detectNested(ws)
		assert.Equal(t, []string{"npm", "--prefix", "frontend", "test"}, det.Argv)
		assert.Empty(t, det.Marker, "a resolved command must not also park")
		require.Len(t, det.Notes, 1)
		assert.Contains(t, det.Notes[0], "backend/")
		assert.Contains(t, det.Notes[0], "NOT covered")
	})

	t.Run("module cap declines with a diagnostic", func(t *testing.T) {
		stubTools(t, "go")

		ws := t.TempDir()
		for _, d := range []string{"a", "b", "c", "d", "e"} {
			mkModule(t, ws, d, "go.mod", goMod)
		}

		det := detectNested(ws)
		assert.Nil(t, det.Argv)
		assert.Equal(t, "nested modules", det.Marker)
		assert.Contains(t, det.Reason, "5 nested modules")
	})

	t.Run("dependency and hidden dirs are ignored", func(t *testing.T) {
		stubTools(t, "npm", "mvn", "java")

		ws := t.TempDir()
		mkModule(t, ws, "node_modules", "package.json", npmPkg)
		mkModule(t, ws, ".hidden", "pom.xml", pomXML)

		det := detectNested(ws)
		assert.Nil(t, det.Argv)
		assert.Empty(t, det.Marker)
	})

	t.Run("nested go module scoped with -C", func(t *testing.T) {
		stubTools(t, "go")

		ws := t.TempDir()
		mkModule(t, ws, "svc", "go.mod", goMod)

		det := detectNested(ws)
		assert.Equal(t, []string{"go", "test", "-C", "svc", "./..."}, det.Argv)
	})

	t.Run("nested pytest deliberately not detected", func(t *testing.T) {
		stubTools(t, "pytest")

		ws := t.TempDir()
		mkModule(t, ws, "api", "pytest.ini", "[pytest]\n")

		det := detectNested(ws)
		assert.Nil(t, det.Argv)
		assert.Empty(t, det.Marker, "nested Python falls through to the model-proposal tier")
	})
}

func TestDetectVerifyCommandFallsBackToNested(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probing is POSIX-only")
	}

	t.Run("root empty scans nested", func(t *testing.T) {
		stubTools(t, "go")

		ws := t.TempDir()
		mkModule(t, ws, "svc", "go.mod", goMod)

		det := detectVerifyCommand(ws)
		assert.Equal(t, []string{"go", "test", "-C", "svc", "./..."}, det.Argv)
	})

	t.Run("root unresolved marker beats nested scan", func(t *testing.T) {
		// A root Cargo.toml with no cargo must keep its Tier-4 park; a runnable
		// nested module must not paper over the implicated root toolchain.
		stubTools(t, "go") // no cargo

		ws := t.TempDir()
		writeFile(t, ws, "Cargo.toml", "[package]\nname=\"x\"\n")
		mkModule(t, ws, "svc", "go.mod", goMod)

		det := detectVerifyCommand(ws)
		assert.Nil(t, det.Argv)
		assert.Equal(t, "Cargo.toml", det.Marker)
	})
}
