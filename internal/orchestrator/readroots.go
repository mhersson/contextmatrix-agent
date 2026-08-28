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
// cardID attributes the line to a run and is part of the dedup key: every
// caller building tools for the same run must pass the SAME cardID (the
// worker's Config.CardID, threaded to the review panel too) or their lines
// for an identical declaration will never collapse. Nothing is logged when
// no roots were declared - matches the behavior of the logReadOnlyRoots
// function this replaces. Logs at warn when anything was dropped, info
// otherwise.
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
