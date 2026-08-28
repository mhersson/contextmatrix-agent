package orchestrator

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-harness/tools"
)

// readRootsLogMu and readRootsLogged dedupe identical read-only-roots log
// lines within one process. The three construction sites that call
// LogReadRootsOutcome can each rebuild read/grep/glob tools from the SAME
// declaration more than once per run: readOnlyToolsWithRoots backs two Deps
// fields built from one call, and PlanTools / WriteToolsForDir are closures
// the orchestrator can invoke again later in the run. Keying on the full
// (identity, outcome) pair - not just the inputs - means a genuinely
// different outcome (a different workspace, or a declaration that resolved
// differently) still gets its own line. Best-of-N candidates build their
// registries concurrently (see runCandidate), so this needs a mutex.
var (
	readRootsLogMu  sync.Mutex
	readRootsLogged = map[string]struct{}{}
)

// LogReadRootsOutcome logs one read/grep/glob tool construction's
// sanitizeReadRoots outcome: the extra read-only roots that survived, and for
// each one dropped, why. The harness drops a widening root silently (see
// tools.ReadRoots in the harness's jail.go), so this is the only place an
// operator whose declared prefix is wrong for the image sees it.
//
// cardID attributes the line to a run when the caller has one; pass "" where
// none is threaded through (the review panel builds its tools as a free
// function with no card identity available) and the workspace still
// identifies the line. Nothing is logged when no roots were declared -
// matches the behavior of the logReadOnlyRoots function this replaces. Logs
// at warn when anything was dropped, info otherwise.
func LogReadRootsOutcome(cardID, workspace string, rr tools.ReadRoots) {
	if len(rr.Effective) == 0 && len(rr.Dropped) == 0 {
		return
	}

	key := fmt.Sprintf("%s\x00%s\x00%+v", cardID, workspace, rr)

	readRootsLogMu.Lock()

	_, already := readRootsLogged[key]
	if !already {
		readRootsLogged[key] = struct{}{}
	}

	readRootsLogMu.Unlock()

	if already {
		return
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
