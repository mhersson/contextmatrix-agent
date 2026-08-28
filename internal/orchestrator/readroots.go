package orchestrator

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-harness/tools"
)

// ReadRootsLog dedupes identical read-only-roots log lines for one run. The
// worker constructs a single instance in runFSM and threads it through every
// construction site (including via Deps.ReadRootsLog for the review panel),
// so its dedupe window matches the run's lifetime rather than the process's:
// readOnlyToolsWithRoots backs two Deps fields built from one declaration,
// and PlanTools / WriteToolsForDir are closures the orchestrator can invoke
// again later in the same run. Best-of-N candidates build their registries
// concurrently (see runCandidate), so the mutex is required, not defensive.
type ReadRootsLog struct {
	mu     sync.Mutex
	logged map[string]struct{}
}

// NewReadRootsLog returns a fresh, empty tracker.
func NewReadRootsLog() *ReadRootsLog {
	return &ReadRootsLog{logged: make(map[string]struct{})}
}

// Log records one read/grep/glob tool construction's sanitizeReadRoots
// outcome: the extra read-only roots that survived, and for each one
// dropped, why. The harness drops a widening root silently (see
// tools.ReadRoots in the harness's jail.go), so this is the only place an
// operator whose declared prefix is wrong for the image sees it.
//
// cardID attributes the line to a run when the caller has one; pass "" where
// none is threaded through (the review panel builds its tools as a free
// function with no card identity available) and the workspace still
// identifies the line. Nothing is logged when no roots were declared -
// matches the behavior of the logReadOnlyRoots function this replaces. Logs
// at warn when anything was dropped, info otherwise.
//
// A nil receiver performs no dedup - every call logs. Production always
// threads a real instance (via runFSM); this only matters for a caller that
// has no run-scoped tracker to hand in, where an occasional repeat line is
// the honest cost of not tracking it.
func (l *ReadRootsLog) Log(cardID, workspace string, rr tools.ReadRoots) {
	if len(rr.Effective) == 0 && len(rr.Dropped) == 0 {
		return
	}

	if l != nil {
		key := fmt.Sprintf("%s\x00%s\x00%+v", cardID, workspace, rr)

		l.mu.Lock()

		_, already := l.logged[key]
		if !already {
			l.logged[key] = struct{}{}
		}

		l.mu.Unlock()

		if already {
			return
		}
	}

	sep := string(os.PathListSeparator)

	dropped := make([]string, len(rr.Dropped))
	for i, d := range rr.Dropped {
		dropped[i] = fmt.Sprintf("%s(%s)", d.Root, d.Reason)
	}

	attrs := []any{"workspace", workspace, "effective", strings.Join(rr.Effective, sep), "dropped", strings.Join(dropped, sep)}
	if cardID != "" {
		attrs = append([]any{"card_id", cardID}, attrs...)
	}

	if len(dropped) > 0 {
		slog.Warn("read-only roots resolved", attrs...)
	} else {
		slog.Info("read-only roots resolved", attrs...)
	}
}
