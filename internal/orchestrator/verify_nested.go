package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
)

// One level of nesting: a monorepo with backend/pom.xml + frontend/package.json
// and no root markers previously resolved NOTHING, so the review gate ran with
// no machine verification at all. detectNested scans first-level subdirectories
// with the same marker priorities and emits WORKSPACE-ROOTED scoped commands
// (mvn -f, npm --prefix, go test -C, ...), composing several modules with &&.

// nestedModuleCap bounds how many nested modules one composed command covers.
// Beyond it detection declines with a diagnostic marker: a sprawling
// multi-module repo needs an operator-declared command, not a guessed one.
const nestedModuleCap = 4

// nestedDirNameRe gates scanned directory names. It doubles as the injection
// guard: a composed command is executed via bash -c, and a name matching this
// pattern cannot carry shell metacharacters, spaces, or a leading dash or dot,
// so hidden directories are excluded for free.
var nestedDirNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// nestedSkipDirs are first-level directories never scanned: dependency and
// output trees whose markers describe vendored code, not the repo's modules.
var nestedSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, "out": true,
}

// nestedRow mirrors detectRow one directory down: present inspects the module
// directory itself; resolve emits a workspace-rooted scoped argv so the
// command runs from the workspace root without a cd.
type nestedRow struct {
	present func(dir string) bool
	resolve func(ws, rel string) (argv []string, reason string)
	marker  string
}

// nestedRows is the nested marker table, in the same priority order as
// detectRows. pytest is deliberately absent: `pytest <dir>` exits 5 on a
// module with no collectable tests (a false failure), and rootdir/conftest
// resolution from the workspace root is not predictable enough for a
// code-chosen command - the Tier-3 model proposal stays the sanctioned
// fallback for nested Python, matching the root walk's bare-pyproject rule.
var nestedRows = []nestedRow{
	{marker: "go.mod", present: hasFile("go.mod"), resolve: nestedProbe(func(rel string) []string {
		return []string{"go", "test", "-C", rel, "./..."}
	})},
	{marker: "Cargo.toml", present: hasFile("Cargo.toml"), resolve: nestedProbe(func(rel string) []string {
		return []string{"cargo", "test", "--manifest-path", rel + "/Cargo.toml"}
	})},
	{marker: "package.json", present: hasRealNPMTestScript, resolve: nestedProbe(func(rel string) []string {
		return []string{"npm", "--prefix", rel, "test"}
	})},
	// The nested command references the build file (-p rel), so a wrapper-only
	// module with no build.gradle(.kts) falls through to the model-proposal
	// tier instead of probe-passing and then false-failing at runtime.
	{marker: "gradle project", present: hasGradleBuildFile, resolve: resolveNestedGradle},
	// The nested command references the pom (-f rel/pom.xml), so a
	// wrapper-only module with no pom.xml falls through to the model-proposal
	// tier instead of probe-passing and then false-failing at runtime.
	{marker: "maven project", present: hasFile("pom.xml"), resolve: resolveNestedMaven},
	{marker: ".NET project file", present: hasDotnetProject, resolve: nestedProbe(func(rel string) []string {
		return []string{"dotnet", "test", rel}
	})},
}

// nestedProbe adapts a scoped-argv builder into a nested resolve func: the
// argv's leading program is probed from the workspace (PATH plus the JVM
// rule), mirroring probeArgvReason at the root.
func nestedProbe(build func(rel string) []string) func(ws, rel string) ([]string, string) {
	return func(ws, rel string) ([]string, string) {
		argv := build(rel)
		if err := verifyexec.Probe(ws, argv); err != nil {
			return nil, err.Error()
		}

		return argv, ""
	}
}

// resolveNestedMaven prefers the system mvn over the module wrapper -
// REVERSED from the root-level resolveMaven. A wrapper invoked on a minimal
// image passes the probe and then dies bootstrapping its own Maven
// distribution; a system mvn that probes runnable actually runs. The wrapper
// remains the fallback when no system mvn is installed.
func resolveNestedMaven(ws, rel string) ([]string, string) {
	argv := []string{"mvn", "-q", "-f", rel + "/pom.xml", "test"}

	sysErr := verifyexec.Probe(ws, argv)
	if sysErr == nil {
		return argv, ""
	}

	if !execFileExists(filepath.Join(ws, rel, "mvnw")) {
		return nil, sysErr.Error()
	}

	wargv := []string{rel + "/mvnw", "-q", "-f", rel + "/pom.xml", "test"}
	if err := verifyexec.Probe(ws, wargv); err != nil {
		return nil, err.Error()
	}

	return wargv, ""
}

// resolveNestedGradle mirrors resolveNestedMaven's system-first preference.
func resolveNestedGradle(ws, rel string) ([]string, string) {
	argv := []string{"gradle", "-p", rel, "test"}

	sysErr := verifyexec.Probe(ws, argv)
	if sysErr == nil {
		return argv, ""
	}

	if !execFileExists(filepath.Join(ws, rel, "gradlew")) {
		return nil, sysErr.Error()
	}

	wargv := []string{rel + "/gradlew", "-p", rel, "test"}
	if err := verifyexec.Probe(ws, wargv); err != nil {
		return nil, err.Error()
	}

	return wargv, ""
}

// hasGradleBuildFile reports whether dir declares a Gradle build script -
// build.gradle or build.gradle.kts. Unlike the root-level hasGradleProject, an
// executable gradlew alone does not qualify here: the nested command
// references the build file (-p dir), so a wrapper-only module would
// otherwise probe-pass and then false-fail at runtime pointing at a
// nonexistent build script.
func hasGradleBuildFile(dir string) bool {
	return fileExists(filepath.Join(dir, "build.gradle")) || fileExists(filepath.Join(dir, "build.gradle.kts"))
}

// detectNested is the one-level fallback walk, called only when the root walk
// found neither a command nor an unresolved marker. For each eligible
// first-level directory the first nestedRow whose marker is present decides
// that module (same first-match semantics as the root). Every module that
// resolves joins the command; a module whose toolchain cannot run becomes the
// Tier-4 diagnostic marker when NOTHING resolves, and a plan note when
// something else did - partial coverage must be visible, never silent.
func detectNested(workspace string) detection {
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return detection{}
	}

	var (
		det      detection
		displays []string
		single   []string
		modules  int
	)

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || nestedSkipDirs[name] || !nestedDirNameRe.MatchString(name) {
			continue
		}

		dir := filepath.Join(workspace, name)

		for _, row := range nestedRows {
			if !row.present(dir) {
				continue
			}

			modules++

			argv, reason := row.resolve(workspace, name)
			if argv != nil {
				displays = append(displays, strings.Join(argv, " "))
				single = argv
			} else {
				if det.Marker == "" {
					det.Marker = fmt.Sprintf("%s (in %s/)", row.marker, name)
					det.Reason = reason
				}

				det.Notes = append(det.Notes, fmt.Sprintf(
					"nested module %s/ declares a %s but its toolchain cannot run (%s) - it is NOT covered by the verify gate",
					name, row.marker, reason))
			}

			break // first matching row decides the module, as at the root
		}
	}

	switch {
	case modules > nestedModuleCap:
		return detection{
			Marker: "nested modules",
			Reason: fmt.Sprintf("%d nested modules detected - declare a verify command that covers them", modules),
		}
	case len(displays) == 0:
		return det // nothing runnable: Marker/Reason park at Tier 4, or stay empty
	case len(displays) == 1:
		return detection{Argv: single, Display: displays[0], Notes: det.Notes}
	default:
		joined := strings.Join(displays, " && ")

		return detection{Argv: verifyexec.ShellArgv(joined), Display: joined, Notes: det.Notes}
	}
}
