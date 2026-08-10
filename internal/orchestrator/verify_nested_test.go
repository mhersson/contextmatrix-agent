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
	pomXML      = "<project></project>\n"
	npmPkg      = `{"name":"x","scripts":{"test":"vitest run"}}`
	goMod       = "module example.com/x\n"
	gradleBuild = "plugins {}\n"
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

	t.Run("mvnw without pom.xml resolves nothing", func(t *testing.T) {
		// The emitted command references the pom (-f rel/pom.xml); a wrapper-only
		// module would probe-pass and then false-fail at runtime, so the maven
		// row requires the pom itself and this module falls through undetected.
		stubTools(t, "mvn", "java")

		ws := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(ws, "backend"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(ws, "backend", "mvnw"), []byte("#!/bin/sh\n"), 0o755))

		det := detectNested(ws)
		assert.Nil(t, det.Argv)
		assert.Empty(t, det.Marker, "nested Maven falls through to the model-proposal tier without a pom")
	})

	t.Run("gradlew without build script resolves nothing", func(t *testing.T) {
		// The emitted command references the build file (-p rel); a wrapper-only
		// module would probe-pass and then false-fail at runtime, so the gradle
		// row requires build.gradle(.kts) and this module falls through undetected.
		stubTools(t, "gradle", "java")

		ws := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(ws, "svc"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(ws, "svc", "gradlew"), []byte("#!/bin/sh\n"), 0o755))

		det := detectNested(ws)
		assert.Nil(t, det.Argv)
		assert.Empty(t, det.Marker, "nested Gradle falls through to the model-proposal tier without a build script")
	})

	t.Run("system gradle preferred over wrapper", func(t *testing.T) {
		stubTools(t, "gradle", "java")

		ws := t.TempDir()
		mkModule(t, ws, "svc", "build.gradle", gradleBuild)
		require.NoError(t, os.WriteFile(filepath.Join(ws, "svc", "gradlew"), []byte("#!/bin/sh\n"), 0o755))

		det := detectNested(ws)
		assert.Equal(t, []string{"gradle", "-p", "svc", "test"}, det.Argv,
			"the wrapper bootstraps its own distribution and dies on minimal images; a runnable system gradle wins")
	})

	t.Run("gradle wrapper fallback when no system gradle", func(t *testing.T) {
		stubTools(t, "java") // no gradle on PATH

		ws := t.TempDir()
		mkModule(t, ws, "svc", "build.gradle", gradleBuild)
		require.NoError(t, os.WriteFile(filepath.Join(ws, "svc", "gradlew"), []byte("#!/bin/sh\n"), 0o755))

		det := detectNested(ws)
		assert.Equal(t, []string{"svc/gradlew", "-p", "svc", "test"}, det.Argv)
	})

	t.Run("unresolved gradle module carries the park marker", func(t *testing.T) {
		// build.gradle.kts pins the other half of hasGradleBuildFile's OR.
		stubTools(t, "gradle") // java missing: the JVM probe fails

		ws := t.TempDir()
		mkModule(t, ws, "svc", "build.gradle.kts", gradleBuild)

		det := detectNested(ws)
		assert.Nil(t, det.Argv)
		assert.Equal(t, "gradle project (in svc/)", det.Marker)
		assert.Contains(t, det.Reason, "java")
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

	t.Run("exactly the cap still composes", func(t *testing.T) {
		// The composing side of the boundary: nestedModuleCap modules are AT the
		// cap, not over it, so an off-by-one in the decline arm is visible here.
		stubTools(t, "go", "bash")

		ws := t.TempDir()
		for _, d := range []string{"a", "b", "c", "d"} {
			mkModule(t, ws, d, "go.mod", goMod)
		}

		det := detectNested(ws)
		assert.Equal(t,
			"go test -C a ./... && go test -C b ./... && go test -C c ./... && go test -C d ./...",
			det.Display)
		assert.Empty(t, det.Marker)
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

	t.Run("nested module with no toolchain carries the park marker", func(t *testing.T) {
		// nestedProbe's failure return - the path every go/cargo/npm/dotnet row
		// takes. Maven and Gradle have their own resolvers, so without this the
		// park-instead-of-proceed rule is pinned for JVM modules only.
		stubTools(t) // empty PATH: go does not resolve

		ws := t.TempDir()
		mkModule(t, ws, "svc", "go.mod", goMod)

		det := detectNested(ws)
		assert.Nil(t, det.Argv)
		assert.Equal(t, "go.mod (in svc/)", det.Marker)
		assert.Contains(t, det.Reason, "go")
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
