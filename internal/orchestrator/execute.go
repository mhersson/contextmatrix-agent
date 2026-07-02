package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/harness"
)

// commitMarker is the line prefix the coder appends to its final message to
// hand off a conventional commit summary. The orchestrator (not the coder)
// performs the commit, so this is the only channel for the message.
const commitMarker = "COMMIT:"

// estimateTokens approximates the prompt budget for window fitting: chars/4
// (the rough bytes-per-token rule) plus a fixed overhead covering the system
// prompt, the tool schemas, and headroom for the conversation that follows.
func estimateTokens(prompt string) int { return len(prompt)/4 + 24000 }

// runExecute is the execute phase: subtasks run SEQUENTIALLY in dependency
// order over a single shared workspace (no parallel writers). Each subtask gets
// a fresh-context coder harness with the full write toolset; code commits and
// pushes after every subtask. The budget ledger is checked before every
// model-bearing step.
func runExecute(ctx context.Context, o *run) error {
	ordered, err := topoOrder(o.subtasks)
	if err != nil {
		return fmt.Errorf("order subtasks: %w", err)
	}

	for _, sub := range ordered {
		if err := o.executeSubtask(ctx, sub); err != nil {
			return err
		}
	}

	return nil
}

// executeSubtask runs one subtask end to end: skip-if-done, budget check, claim,
// model resolution, coder harness run, usage accounting, commit, push, complete.
func (o *run) executeSubtask(ctx context.Context, sub subtaskRef) error {
	d := o.d

	// Resume: a subtask already completed in a prior run is not re-run.
	if sub.State == "done" {
		slog.Info("execute: skipping completed subtask", "card_id", sub.ID)

		return nil
	}

	// Budget gate BEFORE claiming, so a parked subtask is never owned.
	if err := o.ledger.Check(); err != nil {
		return err
	}

	// Claim conflicts mean another agent owns the subtask — abort the run rather
	// than skip, because the workspace is shared and we cannot safely proceed
	// without ownership of the card we are about to build on.
	if err := d.Ops.ClaimCard(ctx, sub.ID); err != nil {
		return fmt.Errorf("claim subtask %s: %w", sub.ID, err)
	}

	if err := o.executeClaimed(ctx, sub); err != nil {
		// The run is aborting (or parking) while we still own the subtask:
		// release it, or it stays claimed until CM's stall sweep mislabels a
		// deliberately-parked run as crashed 30 minutes later.
		o.releaseSubtask(ctx, sub.ID)

		return err
	}

	return nil
}

// subtaskHeartbeatInterval matches the worker's parent-card cadence (5m against
// CM's default 30m heartbeat_timeout). A var so tests can shrink it.
var subtaskHeartbeatInterval = 5 * time.Minute

// executeClaimed is the owned span of a subtask: coder run, commit, push,
// complete. A heartbeat goroutine covers the whole span — CM's stall sweep
// reclaims ANY claimed card whose last_heartbeat exceeds the timeout, the
// parent-card heartbeat does not cover subtask claims, and a coder run is
// wall-clock unbounded. The deferred stop cancels the goroutine AND waits for
// it to actually exit on every exit path (complete, error, park), so it can
// never outlive the claim — or this function's return.
func (o *run) executeClaimed(ctx context.Context, sub subtaskRef) error {
	d := o.d

	stopHeartbeat := startSubtaskHeartbeat(ctx, d.Ops, sub.ID)
	defer stopHeartbeat()

	prompt := fmt.Sprintf(coderPrompt, o.skillEngage(), o.grounding, sub.Title, subtaskBody(sub), o.tc.Title, o.tc.Description)

	res, err := o.runCoder(ctx, sub, prompt)
	if err != nil {
		return err
	}

	commitMsg, ok := extractCommitLine(res.Output)
	if !ok {
		commitMsg = sanitizeTitle(sub.Title)
	}

	committed, err := d.Git.CommitWithMessage(ctx, commitMsg)
	if err != nil {
		return fmt.Errorf("commit subtask %s: %w", sub.ID, err)
	}

	// Push after every subtask so each unit of work is durable and the next
	// subtask builds on a pushed base. A clean tree (nothing committed) skips the
	// push but still completes the card. A push failure aborts the run — the
	// spend has already been reported, so retry/resume must not double-charge.
	if committed {
		if err := o.pushBranch(ctx); err != nil {
			return fmt.Errorf("push after subtask %s: %w", sub.ID, err)
		}
	}

	if err := d.Ops.CompleteTask(ctx, sub.ID, subtaskSummary(res.Output, sub.Title)); err != nil {
		return fmt.Errorf("complete subtask %s: %w", sub.ID, err)
	}

	return nil
}

// startSubtaskHeartbeat ticks ops.Heartbeat for cardID on
// subtaskHeartbeatInterval until the returned stop func is called. Unlike the
// worker's parent-card startHeartbeat (which only cancels and does not wait),
// stop here BLOCKS until the goroutine has actually exited: executeClaimed
// must never return while the goroutine could still be running, or a package
// var read (subtaskHeartbeatInterval) could outlive the caller's stack frame.
func startSubtaskHeartbeat(ctx context.Context, ops Ops, cardID string) func() {
	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		heartbeatLoop(hbCtx, ops, cardID)
	}()

	return func() {
		cancel()
		<-done
	}
}

// heartbeatLoop ticks ops.Heartbeat for cardID on subtaskHeartbeatInterval
// until ctx is canceled. Mirrors the worker's parent-card heartbeat: failures
// are logged, never fatal — a transient MCP hiccup must not abort a healthy run.
func heartbeatLoop(ctx context.Context, ops Ops, cardID string) {
	ticker := time.NewTicker(subtaskHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := ops.Heartbeat(ctx, cardID); err != nil {
				slog.Warn("subtask heartbeat failed", "card_id", cardID, "error", err)
			}
		}
	}
}

// releaseSubtask best-effort releases a claimed subtask on an error exit.
// WithoutCancel: the release must still go out when the run context is the
// thing that died (end_session/kill). An already-unclaimed card
// (ErrCardNotClaimed) is a benign no-op, mirroring the worker's releaseQuietly.
func (o *run) releaseSubtask(ctx context.Context, cardID string) {
	if err := o.d.Ops.ReleaseCard(context.WithoutCancel(ctx), cardID); err != nil &&
		!errors.Is(err, cmclient.ErrCardNotClaimed) {
		slog.Warn("release subtask failed", "card_id", cardID, "error", err)
	}
}

// runCoder runs the subtask's coder harness with in-run recovery from a
// harness-incapable model: it resolves the coder model (skipping any model
// already excluded this run), logs the pick, runs the harness, and accounts for
// spend on every attempt. If the model proves incapable (*IncapableError) it
// blacklists/excludes it via recoverIncapable and RE-SELECTS the next-best model
// for the SAME subtask — the incapable model wrote nothing, so re-running is
// clean (no git reset). The loop is bounded by recoverIncapable's per-card cap:
// once exhausted it returns the wrapped park error. Any non-incapable run error
// (transport, context limit, budget) is returned immediately, unwrapped of the
// recovery loop. Returns the successful run's result.
func (o *run) runCoder(ctx context.Context, sub subtaskRef, prompt string) (harness.Result, error) {
	d := o.d
	cfg := d.Cfg

	// At most one initial attempt plus reselectCap re-selections; recoverIncapable
	// is the authoritative bound (it errors at the cap), the +1 is a belt-and-braces
	// ceiling so a logic slip can never spin.
	for attempt := 0; attempt <= reselectCap; attempt++ {
		model := o.resolveCoderModel(sub, prompt)

		_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory selection record
			fmt.Sprintf("coder model %s selected for subtask %q (tier=%s)", model, sub.Title, tierOf(sub)))

		res, err := o.runModel(ctx, d.WriteTools, prompt, model)

		// Account for spend even on a transport error / partial run, then report
		// the model actually used (falling back to the resolved slug when the
		// provider did not echo one). The incapable attempt is charged too — it
		// burned tokens before tripping.
		o.ledger.Spend(res.TotalCostUSD)

		usedModel := res.ModelUsed
		if usedModel == "" {
			usedModel = model
		}

		// Record the resolved coder slug so the review panel excludes it: a capable
		// model must not review its own code. This runs BEFORE the incapable check
		// below, so an incapable model (which produced no code) is also recorded
		// here — harmless, and it keeps that model out of its own review via this
		// set plus o.excluded. Keyed on the slug we configured, which is what
		// SelectReviewPanel's Exclude set compares against. newRun initializes the
		// map unconditionally.
		o.coderModels[model] = true

		if reportErr := d.Ops.ReportUsage(ctx, sub.ID, usedModel,
			res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
			slog.Warn("execute: report usage failed", "card_id", sub.ID, "error", reportErr)
		}

		var ie *IncapableError
		if errors.As(err, &ie) {
			// recoverIncapable blacklists + excludes the model and returns an error
			// only when the per-card re-selection cap is exhausted — park then.
			if rerr := o.recoverIncapable(ctx, ie); rerr != nil {
				return res, rerr
			}

			// Re-select (the failed model is now excluded) and re-run the SAME
			// subtask: a clean restart since the incapable model committed nothing.
			continue
		}

		if err != nil {
			return res, fmt.Errorf("coder run for %s: %w", sub.ID, err)
		}

		return res, nil
	}

	// Unreachable in practice: recoverIncapable errors at the cap before the loop
	// can exhaust its iterations. Defensive guard against an infinite loop.
	return harness.Result{}, fmt.Errorf("coder for %s: re-selection loop exhausted", sub.ID)
}

// pushBranch pushes the card branch after a commit. On a FRESH run that found a
// stale remote branch (o.staleRemoteTip != ""), the FIRST push overwrites it
// with a force-with-lease against the recorded tip — per spec §5.1, a fresh run
// owns its card branch and reclaims a stale one at first push. Every push after
// that (firstPushDone) is plain, because the branch is now ours and a plain push
// fast-forwards. A run with no stale branch (staleRemoteTip == "", the normal
// case, including all resume runs which never record a tip) always uses a plain
// push. Shared by the execute and document phases.
func (o *run) pushBranch(ctx context.Context) error {
	branch := o.d.Cfg.Branch

	// Every exit marks the first push as attempted: the lease is a one-shot
	// overwrite, never to be repeated with a stale expected tip.
	defer func() { o.firstPushDone = true }()

	if !o.firstPushDone && o.staleRemoteTip != "" {
		if err := o.d.Git.ForcePushWithLease(ctx, branch, o.staleRemoteTip); err != nil {
			return fmt.Errorf("lease push %q: %w", branch, err)
		}

		return nil
	}

	if err := o.d.Git.Push(ctx, branch); err != nil {
		return fmt.Errorf("push %q: %w", branch, err)
	}

	return nil
}

// resolveCoderModel picks the coder model for a subtask: the card's coder pin
// when it is catalog-resolvable, else the best-value complexity selection for
// the subtask's tier and a real window estimate of the coder prompt.
func (o *run) resolveCoderModel(sub subtaskRef, prompt string) string {
	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		// A pinned model is returned even if it is in o.excluded: we never override
		// an explicit operator pin with an auto-selected substitute. A pinned model
		// that is harness-incapable therefore keeps being re-selected, exhausts the
		// re-selection cap, and parks — the blacklist still records it.
		return o.tc.ModelCoder
	}

	spec := o.d.Registry.SelectByComplexity(registry.SelectInput{
		Role:      registry.RoleCoder,
		Tier:      tierOf(sub),
		EstTokens: estimateTokens(prompt),
		Exclude:   o.excluded,
	})

	return spec.Model
}

// subtaskBody returns the description text for a subtask: the planner's
// description (file lists, acceptance criteria) on the fresh-plan path. The
// title fallback exists for resume-loaded refs, which legitimately lack bodies
// (SubtaskStates carries no body field) — it is not the primary path.
func subtaskBody(sub subtaskRef) string {
	if sub.Body != "" {
		return sub.Body
	}

	return sub.Title
}

// tierOf maps a subtask's planner tier string to a registry.Tier. An empty or
// unrecognised tier defaults to moderate: conservative, since under-selecting a
// model for real work is worse than slightly over-paying.
func tierOf(sub subtaskRef) registry.Tier {
	switch sub.Tier {
	case "simple":
		return registry.TierSimple
	case "complex":
		return registry.TierComplex
	case "critical":
		return registry.TierCritical
	default:
		return registry.TierModerate
	}
}

// extractCommitLine scans the coder's final output for the last COMMIT: line and
// returns the trimmed conventional-commit summary after the marker. A missing
// marker or an empty summary returns ("", false) so the caller falls back to the
// sanitized subtask title.
func extractCommitLine(output string) (string, bool) {
	var (
		found string
		ok    bool
	)

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, commitMarker) {
			continue
		}

		msg := strings.TrimSpace(strings.TrimPrefix(trimmed, commitMarker))
		if msg != "" {
			found, ok = msg, true // keep scanning: the LAST valid line wins
		}
	}

	return found, ok
}

// sanitizeTitle builds the fallback commit message from a subtask title when the
// coder omits a usable COMMIT line. Format: lowercase "feat: <title>" — a sane,
// conventional-ish default. A blank title yields "feat: untitled".
func sanitizeTitle(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		t = "untitled"
	}

	return "feat: " + t
}

// subtaskSummary derives the complete_task summary: the first non-empty line of
// the coder's handoff, falling back to the subtask title.
func subtaskSummary(output, title string) string {
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}

	return title
}

// topoOrder returns the subtasks in dependency order via Kahn's algorithm:
// dependencies precede dependents, and among nodes that are simultaneously ready
// the original creation order is preserved (deterministic). A dependency cycle
// returns an error — the planner forbids cycles, but a resume-loaded set might
// not, so the guard is defensive. Dependency IDs not present in the set are
// ignored (already-done prerequisites from a prior run do not block scheduling).
func topoOrder(subs []subtaskRef) ([]subtaskRef, error) {
	index := make(map[string]int, len(subs))
	for i, s := range subs {
		index[s.ID] = i
	}

	// indegree counts only in-set dependencies; out-of-set deps are satisfied.
	indegree := make([]int, len(subs))
	dependents := make([][]int, len(subs))

	for i, s := range subs {
		for _, dep := range s.DependsOnIDs {
			j, ok := index[dep]
			if !ok {
				continue
			}

			indegree[i]++
			dependents[j] = append(dependents[j], i)
		}
	}

	// Seed the ready set in creation order so ties are deterministic.
	var ready []int

	for i := range subs {
		if indegree[i] == 0 {
			ready = append(ready, i)
		}
	}

	ordered := make([]subtaskRef, 0, len(subs))

	for len(ready) > 0 {
		// Pop the lowest original index among the ready nodes: preserves creation
		// order among simultaneously-ready siblings.
		pick := 0
		for k, idx := range ready {
			if idx < ready[pick] {
				pick = k
			}
		}

		i := ready[pick]
		ready = append(ready[:pick], ready[pick+1:]...)
		ordered = append(ordered, subs[i])

		for _, dep := range dependents[i] {
			indegree[dep]--
			if indegree[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}

	if len(ordered) != len(subs) {
		return nil, fmt.Errorf("subtask dependency cycle detected (%d of %d schedulable)", len(ordered), len(subs))
	}

	return ordered, nil
}
