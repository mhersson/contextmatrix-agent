package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	snapshotMaxFiles      = 200 // cap on tracked-file entries listed in the snapshot
	snapshotReadmeLines   = 50  // README head lines included in the snapshot
	snapshotGreenfieldMax = 3   // <= this many tracked files reads as effectively empty
)

// snapshotManifests are the build-manifest basenames the snapshot reports the
// presence of, so planners learn the repo's toolchain without a discovery turn.
var snapshotManifests = []string{"go.mod", "package.json", "Makefile", "pyproject.toml", "Cargo.toml"}

// repoSnapshot renders a bounded picture of the workspace for planning
// discussions: the tracked file list (capped), the README head, and which
// build manifests exist — so planners and seats spend zero turns discovering
// repo shape, and a greenfield repo is stated outright instead of being
// triple-discovered by every seat. Returns "" when the root is empty or not a
// git repo (the briefing then simply omits the block, no worse than before).
func repoSnapshot(root string) string {
	if root == "" {
		return ""
	}

	files, ok := gitTrackedFiles(root)
	if !ok {
		return ""
	}

	var b strings.Builder

	b.WriteString("REPO SNAPSHOT (authoritative; do not spend turns re-verifying this)\n")

	if len(files) <= snapshotGreenfieldMax {
		fmt.Fprintf(&b, "This repository is effectively EMPTY — tracked files: %s. Treat the task as greenfield; do not explore for existing structure.\n",
			strings.Join(files, ", "))
	} else {
		fmt.Fprintf(&b, "Tracked files (%d):\n", len(files))

		shown := files
		if len(shown) > snapshotMaxFiles {
			shown = shown[:snapshotMaxFiles]
		}

		b.WriteString(strings.Join(shown, "\n"))

		if len(files) > snapshotMaxFiles {
			fmt.Fprintf(&b, "\n… and %d more", len(files)-snapshotMaxFiles)
		}

		b.WriteString("\n")
		b.WriteString(manifestLine(files))
		b.WriteString("\n")
	}

	if head := readmeHead(root, snapshotReadmeLines); head != "" {
		b.WriteString("\n=== README.md (head) ===\n" + head + "\n")
	}

	return b.String()
}

// repoSnapshotBlock is the prompt-ready snapshot for this run's workspace,
// redacted the same way newRun redacts the grounding block (defense-in-depth:
// the README head reaches the LLM endpoint, so a secret in it must be scrubbed
// before it leaves the worker). "" when the workspace is not a git repo — the
// briefing then omits the block. Non-empty results are wrapped in surrounding
// blank lines so the block sits apart from the grounding above and the task
// text below.
func (o *run) repoSnapshotBlock() string {
	snap := repoSnapshot(o.d.Cfg.Workspace)
	if snap == "" {
		return ""
	}

	if o.d.Redact != nil {
		snap = o.d.Redact(snap)
	}

	return "\n" + snap + "\n"
}

// gitTrackedFiles enumerates every tracked file in root's git index via a single
// `git ls-files -z` (all tracked files, no pathspec), mirroring
// gitTrackedNested's direct-exec pattern. Returned paths are slash-separated and
// relative to root. ok is false when root is not a git repo or git is
// unavailable (ls-files exits non-zero), signalling graceful degradation.
func gitTrackedFiles(root string) ([]string, bool) {
	cmd := exec.CommandContext(context.Background(), "git", "-C", root,
		"ls-files", "-z") //nolint:gosec // fixed args; root is the workspace path

	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}

	var files []string

	for rel := range strings.SplitSeq(string(out), "\x00") {
		if rel != "" {
			files = append(files, rel)
		}
	}

	return files, true
}

// manifestLine reports which build manifests are present at the repo root among
// snapshotManifests, so the plan can name the real toolchain. "none found" when
// the repo carries none of them.
func manifestLine(files []string) string {
	var present []string

	for _, m := range snapshotManifests {
		if slices.Contains(files, m) {
			present = append(present, m)
		}
	}

	if len(present) == 0 {
		return "Build manifests present: none found"
	}

	return "Build manifests present: " + strings.Join(present, ", ")
}

// readmeHead returns the first n lines of the workspace's README.md, or "" when
// it is absent or unreadable.
func readmeHead(root string, n int) string {
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		return ""
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}

	return strings.Join(lines, "\n")
}
