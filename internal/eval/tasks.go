package eval

import "fmt"

// DefaultTasks returns the curated task library for a role ("coder" | "reviewer" |
// "all"/""). Task metadata lives here; file contents live under fixtures/.
func DefaultTasks(role string) ([]Task, error) {
	coder := []Task{
		CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./..."},
	}
	reviewer := []Task{
		ReviewerTask{name: "offbyone", fixture: "fixtures/reviewer/offbyone", wantApprove: false, plantedSymbol: "Last"},
		ReviewerTask{name: "clean_guard", fixture: "fixtures/reviewer/clean_guard", wantApprove: true},
	}
	switch role {
	case "coder":
		return coder, nil
	case "reviewer":
		return reviewer, nil
	case "all", "":
		return append(coder, reviewer...), nil
	default:
		return nil, fmt.Errorf("unknown role %q (want coder|reviewer|all)", role)
	}
}
