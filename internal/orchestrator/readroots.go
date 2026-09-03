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

// Log records one tool construction's sanitizeReadRoots outcome: surviving
// roots and, per dropped root, why - warn when anything was dropped, info
// otherwise, nothing when no roots were declared. Deduped per (cardID,
// workspace, outcome), so every caller for one run must pass the same cardID.
// A nil receiver logs without dedup.
func (l *ReadRootsLog) Log(cardID, workspace string, rr tools.ReadRoots) {
	if len(rr.Effective) == 0 && len(rr.Dropped) == 0 {
		return
	}

	if l != nil {
		key := readRootsKey(cardID, workspace, rr)

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

// readRootsKey builds an explicit, unambiguous dedup key for one (identity,
// outcome) pair: every field - cardID, workspace, each effective root, each
// dropped root paired with its reason - is its own element in a NUL-joined
// list, rather than leaning on a struct's default %v formatting (which is
// readable but not a documented, collision-safe encoding).
func readRootsKey(cardID, workspace string, rr tools.ReadRoots) string {
	parts := make([]string, 0, 2+len(rr.Effective)+2*len(rr.Dropped))
	parts = append(parts, cardID, workspace)
	parts = append(parts, rr.Effective...)

	for _, d := range rr.Dropped {
		parts = append(parts, d.Root, string(d.Reason))
	}

	return strings.Join(parts, "\x00")
}
