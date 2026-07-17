package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// envProbes maps workspace marker files to the command that reports the
// relevant toolchain version. Ordered; probes sharing a binary run once.
var envProbes = []struct {
	marker string
	argv   []string
}{
	{"go.mod", []string{"go", "version"}},
	{"package.json", []string{"node", "--version"}},
	{"Cargo.toml", []string{"rustc", "--version"}},
	{"pyproject.toml", []string{"python3", "--version"}},
	{"requirements.txt", []string{"python3", "--version"}},
}

// envProbeTimeout bounds one toolchain probe; a hung binary must not stall
// the checkpoint.
const envProbeTimeout = 5 * time.Second

// environmentFacts renders the authoritative environment block for
// checkpoint briefings: the current UTC date plus the first output line of
// each toolchain probe whose marker file exists in the workspace.
// Best-effort by contract - a missing binary, probe error, or timeout
// silently omits that line; the header and date always render. It exists to
// ground discussion seats against knowledge-cutoff hallucinations ("version
// X does not exist") with facts verified on this container.
func environmentFacts(workspace string) string {
	var b strings.Builder

	b.WriteString("ENVIRONMENT (authoritative; verified on this container - do not dispute from memory)\n")
	b.WriteString("Date: " + time.Now().UTC().Format("2006-01-02"))

	probed := map[string]bool{}

	for _, p := range envProbes {
		if probed[p.argv[0]] {
			continue
		}

		if _, err := os.Stat(filepath.Join(workspace, p.marker)); err != nil {
			continue
		}

		probed[p.argv[0]] = true

		ctx, cancel := context.WithTimeout(context.Background(), envProbeTimeout)
		cmd := exec.CommandContext(ctx, p.argv[0], p.argv[1:]...) //nolint:gosec // G204: argv values are hardcoded in envProbes list
		// Probes must observe the workspace's own toolchain selection
		// (go toolchain directives, rustup/pyenv shims are cwd-sensitive).
		cmd.Dir = workspace
		// The go command re-execs the selected toolchain as a child sharing
		// stdout; the context kill only hits the parent, so without a wait
		// delay Output() could block on the orphan's pipe past the timeout.
		cmd.WaitDelay = 2 * time.Second

		out, err := cmd.Output()

		cancel()

		if err != nil {
			continue
		}

		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		if line != "" {
			b.WriteString("\n" + line)
		}
	}

	return b.String()
}
