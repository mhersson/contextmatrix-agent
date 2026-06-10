package eval

import "fmt"

// DefaultTasks returns the curated task library for a role ("coder" | "reviewer" |
// "all"/""). Task metadata lives here; file contents live under fixtures/.
func DefaultTasks(role string) ([]Task, error) {
	coder := []Task{
		CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./..."},
		CoderTask{name: "reverse", fixture: "fixtures/coder/reverse", check: "go test ./..."},
		CoderTask{name: "fizzbuzz", fixture: "fixtures/coder/fizzbuzz", check: "go test ./..."},
		CoderTask{name: "dedup", fixture: "fixtures/coder/dedup", check: "go test ./..."},
		CoderTask{name: "stats", fixture: "fixtures/coder/stats", check: "go test ./..."},
		CoderTask{name: "meanfloor", fixture: "fixtures/coder/meanfloor", check: "go test ./..."},
		CoderTask{name: "calc", fixture: "fixtures/coder/calc", check: "go test ./..."},
		CoderTask{name: "safecount", fixture: "fixtures/coder/safecount", check: "CGO_ENABLED=1 go test -race ./..."},
		CoderTask{name: "lru", fixture: "fixtures/coder/lru", check: "go test ./..."},
		CoderTask{name: "truncate", fixture: "fixtures/coder/truncate", check: "go test ./..."},
	}
	reviewer := []Task{
		ReviewerTask{name: "offbyone", fixture: "fixtures/reviewer/offbyone", wantApprove: false, plantedSymbol: "Last"},
		ReviewerTask{name: "clean_guard", fixture: "fixtures/reviewer/clean_guard", wantApprove: true},
		ReviewerTask{name: "ignored_err", fixture: "fixtures/reviewer/ignored_err", wantApprove: false, plantedSymbol: "Balance"},
		ReviewerTask{name: "flipped_cond", fixture: "fixtures/reviewer/flipped_cond", wantApprove: false, plantedSymbol: "Allow"},
		ReviewerTask{name: "resource_leak", fixture: "fixtures/reviewer/resource_leak", wantApprove: false, plantedSymbol: "Load"},
		ReviewerTask{name: "clean_contains", fixture: "fixtures/reviewer/clean_contains", wantApprove: true},
		ReviewerTask{name: "clean_clamp", fixture: "fixtures/reviewer/clean_clamp", wantApprove: true},
		ReviewerTask{name: "clean_close", fixture: "fixtures/reviewer/clean_close", wantApprove: true},
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
