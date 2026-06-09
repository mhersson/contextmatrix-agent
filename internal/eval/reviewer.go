package eval

import (
	"bufio"
	"context"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// ReviewerTask provisions a workspace with a change in REVIEW.diff. The reviewer
// runs read-only and must emit a structured verdict. Success = correct
// APPROVE/REQUEST_CHANGES and, for a planted bug, citing the planted symbol.
type ReviewerTask struct {
	name          string
	fixture       string
	wantApprove   bool   // true for a clean diff
	plantedSymbol string // symbol the reviewer must cite for a mutant (empty when clean)
}

func (t ReviewerTask) Name() string        { return t.name }
func (t ReviewerTask) Role() registry.Role { return registry.RoleReviewer }

func (t ReviewerTask) Provision(dir string) error { return copyEmbedded(t.fixture, dir) }

func (t ReviewerTask) Prompt() string {
	return strings.TrimSpace(`
Review the change in REVIEW.diff against the surrounding code. Use the read-only
tools (read, grep, glob) to inspect files. Decide whether the change is correct.

End your reply with EXACTLY one verdict line, and if requesting changes, one
defect line citing where the problem is:
VERDICT: APPROVE
or
VERDICT: REQUEST_CHANGES
DEFECT: <file>:<symbol-or-line> - <one-sentence description>

Do not call any tool in the turn that contains the verdict.`)
}

func (t ReviewerTask) Check(_ context.Context, _ string, res harness.Result) (harness.Verdict, error) {
	approve, defect, ok := parseVerdict(res.Output)
	if !ok {
		return harness.Verdict{OK: false, Detail: "no parseable VERDICT line"}, nil
	}
	if t.wantApprove {
		return harness.Verdict{OK: approve, Detail: "clean diff; approve=" + boolStr(approve)}, nil
	}
	caught := !approve && strings.Contains(strings.ToLower(defect), strings.ToLower(t.plantedSymbol))
	return harness.Verdict{OK: caught, Detail: "mutant; defect=" + defect}, nil
}

// parseVerdict scans output for the last VERDICT: line and an optional DEFECT:
// line. Returns approve, the defect text, and whether a verdict was found.
func parseVerdict(output string) (approve bool, defect string, ok bool) {
	sc := bufio.NewScanner(strings.NewReader(output))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "VERDICT:"):
			approve = strings.HasPrefix(strings.TrimSpace(upper[len("VERDICT:"):]), "APPROVE")
			ok = true
		case strings.HasPrefix(upper, "DEFECT:"):
			defect = strings.TrimSpace(line[len("DEFECT:"):])
		}
	}
	return approve, defect, ok
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
