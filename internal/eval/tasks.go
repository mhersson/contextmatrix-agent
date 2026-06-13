package eval

import "fmt"

// DefaultTasks returns the curated task library for a role ("coder" | "reviewer" |
// "all"/""). Task metadata lives here; file contents live under fixtures/.
func DefaultTasks(role string) ([]Task, error) {
	// writable names the single solution file per fixture; the integrity guard
	// protects every OTHER provisioned file (hidden test, go.mod, and lru's shared
	// list.go helper). lru's writable is lru.go ONLY — list.go is part of the harness.
	coder := []Task{
		CoderTask{name: "sumlist", fixture: "fixtures/coder/sumlist", check: "go test ./...", writable: []string{"sumlist.go"}},
		CoderTask{name: "reverse", fixture: "fixtures/coder/reverse", check: "go test ./...", writable: []string{"reverse.go"}},
		CoderTask{name: "fizzbuzz", fixture: "fixtures/coder/fizzbuzz", check: "go test ./...", writable: []string{"fizzbuzz.go"}},
		CoderTask{name: "dedup", fixture: "fixtures/coder/dedup", check: "go test ./...", writable: []string{"dedup.go"}},
		CoderTask{name: "stats", fixture: "fixtures/coder/stats", check: "go test ./...", writable: []string{"stats.go"}},
		CoderTask{name: "meanfloor", fixture: "fixtures/coder/meanfloor", check: "go test ./...", writable: []string{"meanfloor.go"}},
		CoderTask{name: "calc", fixture: "fixtures/coder/calc", check: "go test ./...", writable: []string{"calc.go"}},
		CoderTask{name: "safecount", fixture: "fixtures/coder/safecount", check: "CGO_ENABLED=1 go test -race ./...", writable: []string{"safecount.go"}},
		CoderTask{name: "lru", fixture: "fixtures/coder/lru", check: "go test ./...", writable: []string{"lru.go"}},
		CoderTask{name: "truncate", fixture: "fixtures/coder/truncate", check: "go test ./...", writable: []string{"truncate.go"}},
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
