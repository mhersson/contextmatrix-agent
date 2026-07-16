package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// judgeDiffCap bounds a candidate diff carried whole into the judge prompt; a
// larger diff is summarized as a --stat so one huge candidate cannot blow the
// prompt window.
const judgeDiffCap = 40_000

// judgeVerifyTail is how much of a failing candidate's verify output (the tail)
// is carried into the judge prompt as the failure evidence.
const judgeVerifyTail = 2000

// judgePrompt asks the reviewer-role judge to pick the candidate to ship. The
// candidates are numbered 1-based by pool position (matching the verdict's
// winner index). It is filled with (pool size, task title, rendered sections).
const judgePrompt = `You are judging %d candidate implementations of the same plan for task %q.

For each candidate you get: the coder model, the verify result (passed, failed, or skipped when no gate could run), the verify output tail on failure, and the full diff (or a --stat summary when too large).

Pick the implementation that should ship: correctness first (verify result, plan coverage), then code quality and coherence, then parsimony (prefer the smaller diff when quality is equal). A skipped verify is inconclusive, not a pass — judge such candidates on the diff and plan coverage.

%s

Respond with ONLY a JSON object:
{"winner": <1-based candidate number>, "ranking": [<best to worst>], "rationale": "<2-4 sentences>", "notes": [{"candidate": <n>, "assessment": "<1 sentence>"}]}`

// judgeVerdict is the judge model's structured decision over the pool.
type judgeVerdict struct {
	Winner    int         `json:"winner"`
	Rationale string      `json:"rationale"`
	Notes     []judgeNote `json:"notes"`
}

// judgeNote is one candidate's per-candidate assessment from the judge.
type judgeNote struct {
	Candidate  int    `json:"candidate"`
	Assessment string `json:"assessment"`
}

// runJudge is the Best-of-N judge phase. It verifies every surviving candidate
// serially, eliminates the ones whose verify command fails when any candidate
// passes, and picks the winner: a single viable candidate is an auto-win (no
// model call); otherwise a complex-tier reviewer model reads the pool and emits
// a JSON verdict, mirroring the review synthesis call (two parse attempts, then
// a mechanical fallback to the first verifying candidate). For a normal run
// (BestOfN < 2, or no candidates) it is a strict no-op. Every path that picks a
// winner (auto-win, a parsed verdict, and the unparsable-verdict fallback)
// records a Best-of-N Report section on the parent card and then adopts the
// winner via adoptWinner: hard-reset + push the card branch, replay the
// winner's subtask completions, clean up every candidate's worktree/branch, and
// report per-candidate outcomes to CM.
func runJudge(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg

	// Normal single-solver runs never fan out, so the judge is a no-op. A
	// partially-populated candidate slice (a fan-out that aborted mid-build) is
	// tolerated: nil entries are skipped when filtering survivors.
	if cfg.BestOfN < 2 || len(o.candidates) == 0 {
		return nil
	}

	survivors := survivingCandidates(o.candidates)

	// Announce the phase before the serial verifies + model call below: they are
	// wall-clock unbounded and otherwise emit nothing until the verdict, which
	// reads as a hang on the card activity log.
	if len(survivors) > 0 {
		_ = d.Ops.AddLog(ctx, cfg.CardID, fmt.Sprintf( //nolint:errcheck // advisory record
			"best-of-n: judge phase started — verifying %d candidate(s) before comparison", len(survivors)))
	}

	// Resolve the verify plan once (first caller wins) and run it per candidate on
	// each worktree. The declared/detected/proposed command is repo-level, so the
	// same plan applies to every candidate clone.
	plan, err := o.ensureVerify(ctx)
	if err != nil {
		return err
	}

	// Verify each survivor SERIALLY (the fan-out already peaked memory; parallel
	// verify subprocesses on top would risk the container cap). Capture the diff
	// BEFORE running verify: adoptWinner hard-resets to the winner's COMMIT, so any
	// verify-time working-tree mutation never ships — the diff was the only artifact
	// a mutating verify could pollute, so snapshot it first. A diff over the cap is
	// summarized as a --stat.
	for i, c := range survivors {
		_ = d.Ops.AddLog(ctx, cfg.CardID, fmt.Sprintf( //nolint:errcheck // advisory record
			"best-of-n: verifying candidate %d (%s) — %d of %d", c.idx, c.model, i+1, len(survivors)))

		c.diff, _ = c.git.Diff(ctx, cfg.BaseBranch)
		if len(c.diff) > judgeDiffCap {
			c.diffStat, _ = c.git.DiffStat(ctx, cfg.BaseBranch)
		}

		res, verr := o.runVerifyPlan(ctx, c.dir, plan)
		if verr != nil {
			return verr // parent-context cancel: propagate the abort
		}

		c.verify = res

		_ = d.Ops.AddLog(ctx, cfg.CardID, fmt.Sprintf( //nolint:errcheck // advisory per-candidate result
			"best-of-n: candidate %d (%s) verify %s", c.idx, c.model, verifyStatusWord(res.Status)))
	}

	// Verify failures are eliminated only when at least one candidate passes;
	// when none pass, every UNCAPPED survivor stays in so the judge still picks
	// the least broken one. A capped candidate may enter the pool ONLY through
	// a passing verify — its coder run never confirmed completion, so
	// unverified capped work must never be adoptable, not even as "least
	// broken".
	pool := verifyingCandidates(survivors)
	if len(pool) == 0 {
		pool = uncappedCandidates(survivors)
	}

	// Reachable when every survivor is capped with a failing verify (plus, as
	// refactor insurance, an empty survivor set). The run is parking with
	// first-arrival claims still held: release them, mirroring runFanout's
	// all-failed exit, so the board shows the truth now.
	if len(pool) == 0 {
		o.stopFanoutHeartbeat()

		for _, id := range o.claimedSubIDs() {
			o.releaseSubtask(ctx, id)
		}

		return errors.New("best-of-n: no candidates to judge")
	}

	// One viable candidate: auto-win, no model call.
	if len(pool) == 1 {
		o.winner = pool[0]
		o.judgeModel = ""

		_ = d.Ops.AddLog(ctx, cfg.CardID, fmt.Sprintf( //nolint:errcheck // advisory record
			"best-of-n: auto-win — candidate %d (%s) is the only viable implementation", pool[0].idx, pool[0].model))
		o.logUnverifiedWinner(ctx)

		o.recordJudgeReport(ctx, nil)

		return o.adoptWinner(ctx)
	}

	if err := o.ledger.Check(); err != nil {
		return err
	}

	sections := o.judgeSections(pool)
	prompt := fmt.Sprintf(judgePrompt, len(pool), o.tc.Title, sections)

	model := d.Registry.SelectByComplexity(registry.SelectInput{
		Role:      registry.RoleReviewer,
		Tier:      registry.TierComplex,
		EstTokens: estimateTokens(prompt),
		// The judge reviews the candidates, so exclude every candidate's coder model
		// (a model must not judge its own work) plus the per-card incapable set —
		// exactly the authoritative-review exclusions. Candidates register their
		// models in o.coderModels before judging.
		Exclude: o.reviewExclusions(),
	}).Model

	v, ok, err := o.runJudgeVerdict(ctx, model, prompt, len(pool))
	if err != nil {
		return err
	}

	if !ok {
		// Two unparsable verdicts: fall back to the first verifying candidate
		// rather than fail the whole run. No judge model is recorded — no model
		// actually produced a usable decision.
		o.winner = pool[0]

		_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory record
			"best-of-n: judge verdict unparsable; falling back to first verifying candidate")
		o.logUnverifiedWinner(ctx)

		o.recordJudgeReport(ctx, nil)

		return o.adoptWinner(ctx)
	}

	o.winner = pool[v.Winner-1]
	o.judgeModel = model

	_ = d.Ops.AddLog(ctx, cfg.CardID, fmt.Sprintf( //nolint:errcheck // advisory record
		"best-of-n: judge (%s) selected candidate %d (%s) — %s", model, o.winner.idx, o.winner.model, rationaleHead(v.Rationale)))
	o.logUnverifiedWinner(ctx)

	o.recordJudgeReport(ctx, &v)

	return o.adoptWinner(ctx)
}

// runJudgeVerdict runs the judge model the way review synthesize runs its verdict
// call: up to two attempts (a repair turn on a parse failure), spending on and
// reporting through the shared parent ledger each attempt. It returns
// (verdict, true, nil) on a parsed+validated verdict; (_, false, nil) after two
// parse failures (the caller falls back); and (_, false, err) on a budget park
// or transport error (the caller propagates it).
func (o *run) runJudgeVerdict(ctx context.Context, model, prompt string, poolLen int) (judgeVerdict, bool, error) {
	d := o.d
	cfg := d.Cfg

	var (
		v       judgeVerdict
		lastErr error
	)

	for attempt := range 2 {
		if err := o.ledger.Check(); err != nil {
			return judgeVerdict{}, false, err
		}

		task := prompt
		if attempt > 0 {
			task += repairBlock(lastErr.Error())
		}

		res, err := o.runModel(ctx, d.ReadTools, task, model)

		o.ledger.Spend(res.TotalCostUSD)

		used := res.ModelUsed
		if used == "" {
			used = model
		}

		if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, used,
			res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
			slog.Warn("judge: report usage failed", "card_id", cfg.CardID, "error", reportErr)
		}

		if err != nil {
			return judgeVerdict{}, false, fmt.Errorf("judge run: %w", err)
		}

		v, lastErr = parseJudgeVerdict(res.Output, poolLen)
		if lastErr == nil {
			return v, true, nil
		}

		slog.Warn("judge: verdict parse failed", "card_id", cfg.CardID, "attempt", attempt, "error", lastErr)
	}

	return judgeVerdict{}, false, nil
}

// parseJudgeVerdict extracts the judge verdict JSON (tolerating prose / code
// fences) and validates the winner is a 1-based index into the pool. A missing
// object, malformed JSON, or out-of-range winner is an error so the caller can
// take its repair turn.
func parseJudgeVerdict(s string, poolLen int) (judgeVerdict, error) {
	raw, ok := extractJSON(s)
	if !ok {
		return judgeVerdict{}, fmt.Errorf("no JSON object found in judge output")
	}

	var v judgeVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return judgeVerdict{}, fmt.Errorf("unmarshal judge verdict JSON: %w", err)
	}

	if v.Winner < 1 || v.Winner > poolLen {
		return judgeVerdict{}, fmt.Errorf("judge winner %d out of range [1,%d]", v.Winner, poolLen)
	}

	return v, nil
}

// survivingCandidates returns the candidates that finished without dropping out
// (err == nil), skipping nil entries from a partially-built fan-out slice.
func survivingCandidates(cs []*candidate) []*candidate {
	var out []*candidate

	for _, c := range cs {
		if c == nil || c.err != nil {
			continue
		}

		out = append(out, c)
	}

	return out
}

// verifyingCandidates returns the subset whose verify PASSED, preserving order.
// A verifyPassed status implies a command actually ran — an empty or skipped
// gate never passes — so capped work is admissible only on a genuine pass.
// This is the salvage gate.
func verifyingCandidates(cs []*candidate) []*candidate {
	var out []*candidate

	for _, c := range cs {
		if c.verify.Status == verifyPassed {
			out = append(out, c)
		}
	}

	return out
}

// uncappedCandidates returns the survivors that did NOT hit the turn cap,
// preserving order. It is the "nobody passes verify" fallback pool: capped
// candidates are excluded because their completion was never confirmed by
// their own coder run, so only a passing verify may admit them.
func uncappedCandidates(cs []*candidate) []*candidate {
	var out []*candidate

	for _, c := range cs {
		if !c.capped {
			out = append(out, c)
		}
	}

	return out
}

// judgeSections renders one prompt block per pool candidate, numbered 1-based by
// pool position (matching the verdict's winner index): the coder model, the
// verify result (with the failing output tail), and the diff or its --stat
// summary when the diff was too large.
func (o *run) judgeSections(pool []*candidate) string {
	var b strings.Builder

	for i, c := range pool {
		fmt.Fprintf(&b, "### Candidate %d\n", i+1)
		fmt.Fprintf(&b, "- Coder model: %s\n", c.model)

		if c.capped {
			b.WriteString("- Note: hit the turn cap during the final subtask's wrap-up; the orchestrator committed the work — weigh the verify result and diff.\n")
		}

		switch c.verify.Status {
		case verifyPassed:
			fmt.Fprintf(&b, "- Verify: PASSED (%s)\n", verifyProvenance(o.verify))
		case verifySkipped:
			fmt.Fprintf(&b, "- Verify: SKIPPED — %s\n", c.verify.Note)
		case verifyFailed:
			fmt.Fprintf(&b, "- Verify: FAILED (%s)\n", verifyProvenance(o.verify))

			if tail := lastChars(c.verify.Output, judgeVerifyTail); strings.TrimSpace(tail) != "" {
				b.WriteString("- Verify output (tail):\n\n```\n")
				b.WriteString(tail)
				b.WriteString("\n```\n")
			}
		}

		if c.diffStat != "" {
			b.WriteString("- Diff too large; --stat summary:\n\n```\n")
			b.WriteString(c.diffStat)
			b.WriteString("\n```\n\n")
		} else {
			b.WriteString("- Diff:\n\n```diff\n")
			b.WriteString(c.diff)
			b.WriteString("\n```\n\n")
		}
	}

	return b.String()
}

// recordJudgeReport upserts the "## Best-of-N Report" section onto the parent
// card body: a table row per candidate (index, coder model, verify result, diff
// size, outcome), the judge's rationale when it ran, and the judge model.
// Mirrors recordReview's best-effort card-body recording. v is nil for an
// auto-win or a fallback (no usable verdict).
func (o *run) recordJudgeReport(ctx context.Context, v *judgeVerdict) {
	var b strings.Builder

	b.WriteString("## Best-of-N Report\n\n")
	b.WriteString("| # | Coder model | Verify | Diff | Outcome |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")

	for _, c := range o.candidates {
		if c == nil {
			continue
		}

		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n",
			c.idx, c.model, judgeVerifyCell(c), judgeDiffCell(c), o.judgeOutcome(c))
	}

	if v != nil && strings.TrimSpace(v.Rationale) != "" {
		b.WriteString("\n### Rationale\n\n")
		b.WriteString(strings.TrimSpace(v.Rationale))
		b.WriteString("\n")
	}

	// Per-candidate assessments from the verdict. The judge numbers candidates by
	// pool position (matching the winner index), which is NOT the table's candidate
	// index once the pool is filtered — the clarifier keeps that unambiguous.
	if v != nil {
		if rows := renderJudgeNotes(v.Notes); rows != "" {
			b.WriteString("\n### Assessments\n\n")
			b.WriteString("_(candidate numbers in judge text are pool positions)_\n\n")
			b.WriteString(rows)
		}
	}

	// A winner adopted on a non-passing verify is flagged loudly on the card, so a
	// human reading the Best-of-N report sees the unverified adoption directly.
	if o.winner != nil && o.winner.verify.Status != verifyPassed {
		note := o.winner.verify.Note
		if note == "" {
			note = "verify did not pass"
		}

		fmt.Fprintf(&b, "\n**⚠ Winner adopted without a passing verify — %s**\n", note)
	}

	judge := o.judgeModel
	if judge == "" {
		judge = "— (no model verdict)"
	}

	fmt.Fprintf(&b, "\n_Judge model: %s_\n", judge)

	o.recordSection(ctx, "Best-of-N Report", b.String())
}

// renderJudgeNotes renders the verdict's per-candidate assessments as a bullet
// list ("- Candidate <n>: <assessment>"), skipping blank assessments. It returns
// "" when there is nothing to render, so the caller can omit the whole section.
func renderJudgeNotes(notes []judgeNote) string {
	var b strings.Builder

	for _, note := range notes {
		assessment := strings.TrimSpace(note.Assessment)
		if assessment == "" {
			continue
		}

		fmt.Fprintf(&b, "- Candidate %d: %s\n", note.Candidate, assessment)
	}

	return b.String()
}

// judgeVerifyCell renders a candidate's verify result for the report table. A
// dropped candidate (never verified) shows a dash; a skipped gate is neither a
// tick nor a cross.
func judgeVerifyCell(c *candidate) string {
	if c.err != nil {
		return "—"
	}

	switch c.verify.Status {
	case verifyPassed:
		return "✓"
	case verifyFailed:
		return "✗"
	default:
		return "skip"
	}
}

// judgeDiffCell renders a candidate's diff size for the report table: the char
// count, noting when the diff was --stat summarized. Dropped candidates have no
// diff.
func judgeDiffCell(c *candidate) string {
	if c.err != nil {
		return "—"
	}

	if c.diffStat != "" {
		return fmt.Sprintf("%d chars (stat)", len(c.diff))
	}

	return fmt.Sprintf("%d chars", len(c.diff))
}

// judgeOutcome labels a candidate's fate for the report table. Capped
// candidates carry the marker so the table shows how the work landed; a winner
// adopted on a non-passing verify is flagged unverified.
func (o *run) judgeOutcome(c *candidate) string {
	passed := c.verify.Status == verifyPassed

	switch {
	case c == o.winner:
		switch {
		case c.capped:
			return "win (capped)"
		case !passed:
			return "win (unverified)"
		default:
			return "win"
		}
	case c.err != nil:
		var mte *MaxTurnsError
		if errors.As(c.err, &mte) {
			return "failed (turn cap)"
		}

		return "failed (error)"
	case c.capped && !passed:
		return "failed (turn cap + verify)"
	case passed:
		if c.capped {
			return "loss (capped)"
		}

		return "loss"
	case c.verify.Status == verifySkipped:
		return "loss (unverified)"
	default:
		return "failed (verify)"
	}
}

// logUnverifiedWinner records a loud card note when the adopted winner did not
// pass verify — the all-failed fallback pool can still surface a winner whose
// verify skipped or failed, and a human must see that Best-of-N shipped
// unverified work. A passing winner logs nothing.
func (o *run) logUnverifiedWinner(ctx context.Context) {
	if o.winner == nil || o.winner.verify.Status == verifyPassed {
		return
	}

	note := o.winner.verify.Note
	if note == "" {
		note = "verify did not pass"
	}

	_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, fmt.Sprintf( //nolint:errcheck // advisory honesty record
		"best-of-n: winner (candidate %d) adopted WITHOUT a passing verify — %s", o.winner.idx, note))
}

// lastChars returns the last n characters of s (the tail), or all of s when it
// is shorter.
func lastChars(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[len(s)-n:]
}

// rationaleHead is the one-line head of the judge rationale for the activity log:
// the first line, truncated.
func rationaleHead(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}

	const headMax = 200
	if len(s) > headMax {
		s = s[:headMax] + "…"
	}

	return s
}

// adoptWinner lands the Best-of-N winner on the card branch, replays its
// subtask completions to the board, cleans up every candidate's worktree and
// branch, and reports per-candidate outcomes. It runs at the end of runJudge on
// every path that sets o.winner (auto-win, a parsed verdict, and the
// unparsable-verdict fallback all share this tail).
//
// The adopt-and-push sequence is fatal: the run cannot continue without the
// winner's work actually on the card branch. Everything after it — subtask
// replay, candidate cleanup, and outcome reporting — is best-effort: the code
// is already safely on the branch, so a failure there is logged, not fatal.
func (o *run) adoptWinner(ctx context.Context) error {
	head, err := o.winner.git.Head(ctx)
	if err != nil {
		return fmt.Errorf("best-of-n: winner head: %w", err)
	}

	if err := o.d.Git.HardReset(ctx, head); err != nil {
		return fmt.Errorf("best-of-n: hard reset to winner head %q: %w", head, err)
	}

	// The winner never pushed (candidates stay off-board and never push, per the
	// fan-out contract), so this is the run's first push regardless of how many
	// subtasks the winner completed in its worktree.
	if err := o.pushBranch(ctx); err != nil {
		return fmt.Errorf("best-of-n: push winner: %w", err)
	}

	// Stop heartbeating the first-arrival claims before the replay completes
	// them — heartbeats against just-completed (released) cards are noise.
	// Replay itself takes seconds, nowhere near the stall timeout.
	o.stopFanoutHeartbeat()

	o.replayWinnerSubtasks(ctx)
	o.cleanupCandidates(ctx)
	o.reportCandidateOutcomes(ctx)

	return nil
}

// replayWinnerSubtasks re-applies the winner candidate's subtask completions to
// the board: a candidate runs entirely off-board (Best-of-N disables
// boardOps), so nothing about its subtasks exists in CM until this replay.
// Best-effort per subtask: a claim or complete failure is logged (warn plus a
// card activity entry), not fatal — the winner's code is already on the card
// branch, so board state is cosmetic by comparison.
func (o *run) replayWinnerSubtasks(ctx context.Context) {
	cfg := o.d.Cfg
	summary := "completed by best-of-n winner (" + o.winner.model + ")"

	for _, sub := range o.winner.completed {
		if err := o.d.Ops.ClaimCard(ctx, sub.ID); err != nil {
			slog.Warn("adopt: replay claim failed", "card_id", cfg.CardID, "subtask_id", sub.ID, "error", err)
			_ = o.d.Ops.AddLog(ctx, cfg.CardID, fmt.Sprintf( //nolint:errcheck // advisory record
				"best-of-n: replay claim failed for subtask %s: %v", sub.ID, err))
		}

		if err := o.d.Ops.CompleteTask(ctx, sub.ID, summary); err != nil {
			slog.Warn("adopt: replay complete failed", "card_id", cfg.CardID, "subtask_id", sub.ID, "error", err)
			_ = o.d.Ops.AddLog(ctx, cfg.CardID, fmt.Sprintf( //nolint:errcheck // advisory record
				"best-of-n: replay complete failed for subtask %s: %v", sub.ID, err))
		}
	}
}

// cleanupCandidates removes every candidate's worktree and branch — the winner
// included, since its work now lives on the card branch, not its container-
// local one. This MUST go through the main o.d.Git handle: a candidate's
// GitForDir handle has no cardBranch set (guardPush fails closed on it), so its
// DeleteBranch guard cannot protect the real card branch, and `git worktree
// remove` operates from the superproject regardless of which worktree it
// targets. nil-safe (a partially-populated fan-out slice may hold nil slots for
// a candidate whose worktree was never built) and best-effort per candidate: a
// failure is logged, not fatal, and does not stop the rest of the cleanup.
func (o *run) cleanupCandidates(ctx context.Context) {
	cfg := o.d.Cfg

	for _, c := range o.candidates {
		if c == nil {
			continue
		}

		if err := o.d.Git.RemoveWorktree(ctx, c.dir); err != nil {
			slog.Warn("adopt: remove candidate worktree failed",
				"card_id", cfg.CardID, "candidate", c.idx, "dir", c.dir, "error", err)
		}

		if err := o.d.Git.DeleteBranch(ctx, c.branch); err != nil {
			slog.Warn("adopt: delete candidate branch failed",
				"card_id", cfg.CardID, "candidate", c.idx, "branch", c.branch, "error", err)
		}
	}
}

// reportCandidateOutcomes builds one cmclient.ModelOutcome per non-nil
// candidate — the winner "win"; a candidate that dropped out before judging
// ("err != nil"), or capped work whose verify failed (it never entered the
// judge pool either), reports "failed"; every other survivor "loss" — and
// reports them to CM in a single call. NCandidates counts only non-nil
// entries: a nil slot means no candidate ever started (a fan-out that aborted
// before that worktree was built), so it never raced and must not inflate the
// denominator; a non-nil candidate with err != nil DID race (and lost, or
// crashed), so it counts. Best-effort: a report failure is logged, not fatal.
func (o *run) reportCandidateOutcomes(ctx context.Context) {
	cfg := o.d.Cfg

	n := 0

	for _, c := range o.candidates {
		if c != nil {
			n++
		}
	}

	rows := make([]cmclient.ModelOutcome, 0, n)

	for _, c := range o.candidates {
		if c == nil {
			continue
		}

		result := "loss"

		switch {
		case c == o.winner:
			result = "win"
		case c.err != nil, c.capped && c.verify.Status != verifyPassed:
			// Dropped before judging, or capped work whose verify did not pass — it
			// never entered the pool, so it raced and failed.
			result = "failed"
		}

		rows = append(rows, cmclient.ModelOutcome{
			Model:       c.model,
			Result:      result,
			VerifyPass:  c.verify.Status == verifyPassed,
			CostUSD:     c.ledger.Spent(),
			NCandidates: n,
			JudgeModel:  o.judgeModel,
		})
	}

	if err := o.d.Ops.ReportModelOutcomes(ctx, cfg.CardID, rows); err != nil {
		slog.Warn("adopt: report model outcomes failed", "card_id", cfg.CardID, "error", err)
	}
}
