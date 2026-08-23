package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// reviewPanelSize is the fixed number of review specialists fanned out per
// round: Correctness, Design & Maintainability, Security & Performance.
const reviewPanelSize = 3

// hardReviewIterationCap is a defensive ceiling on the review loop independent
// of the configured attempts cap, so a misbehaving IncrementReviewAttempts (or
// a zero cap) can never loop forever.
const hardReviewIterationCap = 50

// verifyOutputTail caps the verify-command output carried into findings, so a
// noisy failing suite does not swamp the fix prompt. It is a TAIL: a build
// tool's diagnostics are concentrated in its last bytes, and res.Output is
// already bounded at 64 KiB by verifyexec, so nothing upstream is lost.
const verifyOutputTail = 4000

// verifyFailedPrefix marks findings that came from a failed verify gate rather
// than the specialist panel, so runFix can route them to verifyFixPrompt.
const verifyFailedPrefix = "verify command failed: "

// reviewRoundReserve is the least container time a verify run, a specialist
// panel, and a fix round need to complete without being killed mid-work. A var
// so tests can shrink it.
var reviewRoundReserve = 20 * time.Minute

// verdict is the synthesis model's structured decision: approve outright or
// return a concrete fix list for the coder.
type verdict struct {
	Approved bool   `json:"approved"`
	Summary  string `json:"summary"`
	FixTier  string `json:"fix_tier"`
	Fixes    []fix  `json:"fixes"`
}

// fix is one actionable finding the coder must address on the next round.
type fix struct {
	File       string `json:"file"`
	Issue      string `json:"issue"`
	Suggestion string `json:"suggestion"`
}

// ReviewParkedError marks the review cap being exhausted without approval. The
// worker maps it to the park path: exit 0, completed callback, card left in
// review. Parked is not failed - a human picks the card up from review.
type ReviewParkedError struct{}

func (e *ReviewParkedError) Error() string {
	return "review parked: attempts cap exhausted without approval"
}

// reviewTimeLeft parks the review when less than reviewRoundReserve remains on
// the container deadline, before a verify run, panel, or fix round can be
// started that has no chance of finishing. round names the round a re-trigger
// resumes at in the card log.
func (o *run) reviewTimeLeft(ctx context.Context, round int) error {
	d := o.d
	dl := d.Cfg.Deadline

	if dl.IsZero() {
		return nil
	}

	remaining := time.Until(dl)
	if remaining >= reviewRoundReserve {
		return nil
	}

	// A deadline already behind us reads as "0s left", not a negative span.
	remaining = max(remaining, 0)

	d.logCard(ctx, "review parked: %s of container time left, less than the %s a review round needs - a re-trigger resumes at round %d",
		remaining, reviewRoundReserve, round)

	return &ReviewParkedError{}
}

// runReview is the review phase. The parent enters review (idempotent on
// resume), then hands off to reviewLoop (autonomous) or runReviewHITL: cheap
// incremental rounds where a detected verify gate runs first and
// short-circuits to the fix run on failure; otherwise three read-only
// specialists fan out on diverse models and one synthesis call decides
// approve-or-fix.
func runReview(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg

	// Idempotent on resume: only transition into review when not already there.
	if o.tc.State != "review" {
		if err := d.Ops.StartReview(ctx, cfg.CardID); err != nil {
			return fmt.Errorf("start review: %w", err)
		}
	}

	// Resolve the effective attempts cap. When the persisted counter already
	// meets or exceeds the cap AND the card is entering review from a non-review
	// state (e.g., moved back to todo), the card was parked on a prior run with
	// the counter left at the cap. Reset it to zero so round numbering in the
	// fresh run restarts at 1 - a local reset only; the server's review_attempts
	// counter is monotonic and unaffected. Cards still in review (crash-resume
	// from an interrupted authoritative pass) keep their counter so round
	// numbering continues from where it left off.
	attemptsCap := cfg.ReviewAttemptsCap
	if attemptsCap <= 0 {
		attemptsCap = config.DefaultReviewAttemptsCap
	}

	if o.tc.ReviewAttempts >= attemptsCap && o.tc.State != "review" {
		o.tc.ReviewAttempts = 0
	}

	plan, err := o.ensureVerify(ctx)
	if err != nil {
		return err
	}

	if cfg.Interactive {
		return o.runReviewHITL(ctx, plan)
	}

	return o.reviewLoop(ctx, plan, 0)
}

// reviewLoop is the autonomous review loop. Approval exits nil; each
// non-approval increments the card's review attempts and runs a fix. At the
// cliff (the round that would otherwise park) the gated authoritative pass
// takes over instead of parking on a cheap verdict - it is the sole park gate,
// except when the server's own ceiling refuses a cheap round's increment.
// The budget ledger is checked before every model-bearing step.
//
// consumed is the number of review rounds already run in-process this run: 0
// when this loop owns the whole phase, >0 when runReviewHITL hands over after
// a mid-run promotion. o.tc is a run-start snapshot; every in-process round
// that did not approve also incremented the server counter, so
// o.tc.ReviewAttempts + consumed mirrors the persisted count and round
// numbering stays resume-stable.
func (o *run) reviewLoop(ctx context.Context, plan verifyPlan, consumed int) error {
	d := o.d
	cfg := d.Cfg

	// Guard a mis-wired worker: a zero or negative cap would make the cliff trip
	// on the FIRST round and park (via the authoritative pass) every card
	// immediately. Fall back to CM's convention instead.
	attemptsCap := cfg.ReviewAttemptsCap
	if attemptsCap <= 0 {
		attemptsCap = config.DefaultReviewAttemptsCap
	}

	fixRan := false

	for iter := range hardReviewIterationCap {
		// Round number continues across resumes: review_attempts persists the
		// count of prior rounds, so round N is stable for the body record.
		round := o.tc.ReviewAttempts + consumed + iter + 1

		if err := o.reviewTimeLeft(ctx, round); err != nil {
			return err
		}

		// At the cliff (the round that would otherwise park), run the gated
		// authoritative pass instead of another cheap round - never park on a cheap
		// verdict here, except when the server's own ceiling refuses this round's
		// increment. It is terminal: returns nil (finished) or parks.
		if round >= attemptsCap {
			return o.authoritativeReview(ctx, plan, round)
		}

		findings, fixTier, approved, vres, err := o.reviewRound(ctx, plan, round, false)
		if err != nil {
			return err
		}

		// Record this round on the parent card body for the complete review
		// history (CM's review-task workflow skill writes ## Review Findings the same way).
		o.recordReview(ctx, round, findings, approved, vres)

		if approved {
			o.reviewSummary = findings // synthesis verdict summary, for the PR body
			o.fixEscalate = false

			return nil
		}

		// A fix round that committed but left this round's verify red failed as
		// surely as one that landed nothing: the next fix escalates. Round 1's
		// red verify has no fix behind it and escalates nothing.
		if fixRan && vres.Status == verifyFailed {
			o.markFixFailed("left the verify red")
		}

		// Carry this round's findings into the next round so the panel verifies
		// their resolution without importing new scope (cross-round memory).
		o.lastFindings = findings

		if _, err := o.incrementReviewAttempt(ctx, findings); err != nil {
			return err
		}

		committed, err := o.runFix(ctx, findings, round, fixTier, false)
		if err != nil {
			return err
		}

		fixRan = true

		if !committed {
			o.markFixFailed("produced no change")
		}
	}

	return fmt.Errorf("review exceeded the hard iteration cap of %d", hardReviewIterationCap)
}

// runReviewHITL is the HITL review loop: each round produces specialist findings
// (verify gate + 3 specialists + synthesis), records them, and presents them to
// the human, who decides. Approve -> proceed to integrate; adjust -> apply the
// findings plus the human's feedback as a fix, then re-review. The human is the
// decision-maker, so there is no authoritative pass or auto-park; the hard
// iteration cap is only a runaway guard. On a mid-run promotion the loop falls
// back to autonomous decision semantics instead: the promoted round's verdict
// decides, and a rejection lands one fix and hands the remainder to reviewLoop,
// so the attempts cap, authoritative cliff, and park-not-approve semantics all
// apply.
func (o *run) runReviewHITL(ctx context.Context, plan verifyPlan) error {
	d := o.d
	cfg := d.Cfg

	model := resolveDecisionModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel, o.excludedModels())

	for iter := range hardReviewIterationCap {
		round := o.tc.ReviewAttempts + iter + 1

		findings, fixTier, autoApproved, vres, err := o.reviewRound(ctx, plan, round, false)
		if err != nil {
			return err
		}

		o.recordReview(ctx, round, findings, autoApproved, vres)

		outcome, fb, gerr := o.gate(ctx, gateReviewDecision, model, presentFindings(findings, autoApproved))
		if gerr != nil {
			return gerr
		}

		switch outcome {
		case gateApprove:
			o.reviewSummary = findings
			if !autoApproved {
				o.reviewSummary = approvedDespiteFindings(findings)
			}

			return nil

		case gatePromoted:
			// No human decided anything - fall back to the autonomous decision
			// for the verdict already in hand (never re-run the round).
			if autoApproved {
				o.reviewSummary = findings

				return nil
			}

			o.lastFindings = findings

			if _, err := d.Ops.IncrementReviewAttempts(ctx, cfg.CardID); err != nil {
				return fmt.Errorf("increment review attempts: %w", err)
			}

			if _, err := o.runFix(ctx, findings, round, fixTier, false); err != nil {
				return err
			}

			d.logCard(ctx, "review: promoted mid-run with outstanding findings - continuing autonomously from round %d", round+1)

			// The inbox stays closed, so every later gate would return
			// gatePromoted instantly: hand the remainder to the autonomous loop
			// (attempts cap, authoritative cliff, park) and never re-enter this
			// one. iter+1 rounds ran in-process, each counted server-side.
			return o.reviewLoop(ctx, plan, iter+1)
		}

		o.lastFindings = findings

		if _, err := d.Ops.IncrementReviewAttempts(ctx, cfg.CardID); err != nil {
			return fmt.Errorf("increment review attempts: %w", err)
		}

		if _, err := o.runFix(ctx, mergeFeedback(findings, fb), round, fixTier, false); err != nil {
			return err
		}
	}

	return fmt.Errorf("HITL review exceeded the hard iteration cap of %d", hardReviewIterationCap)
}

// presentFindings is the chat message for the review-decision gate: the
// synthesized findings plus the automated recommendation (advisory; the human
// decides).
func presentFindings(findings string, autoApproved bool) string {
	rec := "revise"
	if autoApproved {
		rec = "approve"
	}

	return "Review findings (automated recommendation: " + rec + "):\n\n" + findings +
		"\n\nApprove to integrate, or tell me what you'd like changed."
}

// approvedDespiteFindings frames the review summary for the PR body when the
// human approved integration while the automated verdict still had open
// findings, so the PR model cannot narrate them as fixed. Any future path that
// lets an approval coexist with open findings must reuse this helper.
func approvedDespiteFindings(findings string) string {
	return "The human reviewer approved integration despite these outstanding review findings (they were presented to the reviewer but not fixed):\n\n" + findings
}

// mergeFeedback folds the human's adjust feedback into the synthesized findings
// fed to the fix coder, so the fix run addresses both.
func mergeFeedback(findings, feedback string) string {
	if strings.TrimSpace(feedback) == "" {
		return findings
	}

	return findings + "\n\nADDITIONAL HUMAN FEEDBACK:\n" + feedback
}

// authoritativeReview is the gated strong pass run at the review cliff instead of
// parking on a cheap verdict: a strong, full-scope review; if it approves the
// card finishes; if it confirms real issues, ONE strong full-scope fix and one
// strong re-review; still failing → park with the strong findings. It never loops.
func (o *run) authoritativeReview(ctx context.Context, plan verifyPlan, round int) error {
	d := o.d

	// reviewLoop checked this at the top of the iteration that reached the
	// cliff; checked again here so authoritativeReview keeps its own entry
	// contract for any caller other than that cliff.
	if err := o.reviewTimeLeft(ctx, round); err != nil {
		return err
	}

	findings, fixTier, approved, vres, err := o.reviewRound(ctx, plan, round, true)
	if err != nil {
		return err
	}

	o.recordReview(ctx, round, findings, approved, vres)

	if approved {
		o.reviewSummary = findings

		return nil
	}

	o.lastFindings = findings

	if _, err := o.incrementReviewAttempt(ctx, findings); err != nil {
		return err
	}

	// Gated strong fix - runs only because the authoritative review confirmed
	// real issues.
	if _, err := o.runFix(ctx, findings, round, fixTier, true); err != nil {
		return err
	}

	// One strong re-review of the full change.
	round2 := round + 1

	findings2, _, approved2, vres2, err := o.reviewRound(ctx, plan, round2, true)
	if err != nil {
		return err
	}

	o.recordReview(ctx, round2, findings2, approved2, vres2)

	if approved2 {
		o.reviewSummary = findings2

		return nil
	}

	o.lastFindings = findings2

	n, err := o.incrementReviewAttempt(ctx, findings2)
	if err != nil {
		return err
	}

	// Park with the strong findings. n is the persisted counter after both
	// increments, and is the card's only visible record of how many rounds the
	// configured cap actually bought.
	d.logCard(ctx, "review parked after %d attempts (authoritative pass) - outstanding findings:\n%s", n, findings2)

	return &ReviewParkedError{}
}

// incrementReviewAttempt calls IncrementReviewAttempts and treats
// ContextMatrix's ceiling rejection as an implicit park (ReviewParkedError)
// instead of a hard failure, so a card whose persisted counter already sits at
// the ceiling parks gracefully rather than erroring out mid-review. CM's counter
// is monotonic for the card's lifetime, so every increment site can hit it, not
// just the authoritative pass. Returns the new running total on success.
func (o *run) incrementReviewAttempt(ctx context.Context, findings string) (int, error) {
	n, err := o.d.Ops.IncrementReviewAttempts(ctx, o.d.Cfg.CardID)
	if err == nil {
		return n, nil
	}

	if errors.Is(err, cmclient.ErrReviewAttemptsCapped) {
		o.d.logCard(ctx, "review parked at server cap - outstanding findings:\n%s", findings)

		return 0, &ReviewParkedError{}
	}

	return 0, fmt.Errorf("increment review attempts: %w", err)
}

// reviewRound runs one review pass and returns the outstanding findings text,
// whether the work is approved, the round's verify result, and any fatal error
// (budget park, transport). The tri-state verify gate runs first: FAILED
// short-circuits to the fix loop (redacted output tail as the finding) WITHOUT
// spending reviewer tokens; SKIPPED logs loudly and proceeds to the specialists
// WITHOUT the fix loop (a missing/timed-out gate is not a defect to fix); PASSED
// (or no command) proceeds. On any gate outcome that reaches them, the three
// specialists fan out and the synthesis verdict decides.
func (o *run) reviewRound(ctx context.Context, plan verifyPlan, round int, authoritative bool) (findings string, fixTier string, approved bool, vres verifyResult, err error) {
	// Budget gate before the verify subprocess too - the gate may be cheap, but
	// the fix run it can trigger is not, and we park before doing any work.
	if err := o.ledger.Check(); err != nil {
		return "", "", false, verifyResult{}, err
	}

	if len(plan.Argv) > 0 {
		res, verr := o.runVerifyPlan(ctx, o.d.Cfg.Workspace, plan)
		if verr != nil {
			return "", "", false, verifyResult{}, verr
		}

		vres = res

		o.logVerifyRound(ctx, res, round)

		switch res.Status {
		case verifyFailed:
			// Gate failure goes STRAIGHT to the fix loop without burning reviewer
			// tokens. The redacted output tail is the finding the coder fixes - the
			// "(tail)" label matches judge.go's identical evidence, so the coder
			// knows the block starts mid-output rather than at the command's start.
			// No verdict ran, so the fix run falls back to the card tier (empty fixTier).
			return verifyFailedPrefix + plan.Display + "\n\nVerify output (tail):\n\n" +
				verifyFailureExcerpt(res.Output), "", false, vres, nil
		case verifySkipped:
			// A missing or timed-out gate is inconclusive, not a defect: proceed to
			// the specialists without a fix loop. logVerifyRound already said so.
		case verifyPassed:
			// Proceed to the specialist panel.
		}
	}

	// Gate passed, skipped, or absent - the gate is a cheap pre-filter, not a
	// substitute for review, so specialists always run. With mob session
	// review on, a panel discussion replaces the specialist pass for
	// non-authoritative rounds (the authoritative pass keeps the proven solo
	// machinery); a failed discussion degrades to the fan-out below.
	if o.d.Cfg.Mob.enabled() && o.d.Cfg.Mob.Review && !authoritative {
		if v, ok := o.mobReviewVerdict(ctx); ok {
			if v.Approved {
				return v.Summary, v.FixTier, true, vres, nil
			}

			return formatFixes(v), v.FixTier, false, vres, nil
		}
	}

	specialistFindings, err := o.runSpecialists(ctx, authoritative)
	if err != nil {
		return "", "", false, vres, err
	}

	if err := o.ledger.Check(); err != nil {
		return "", "", false, vres, err
	}

	v, err := o.synthesize(ctx, specialistFindings, authoritative)
	if err != nil {
		return "", "", false, vres, err
	}

	if v.Approved {
		return v.Summary, v.FixTier, true, vres, nil
	}

	return formatFixes(v), v.FixTier, false, vres, nil
}

// runSpecialists fans the three review lenses out as parallel read-only child
// agents over the branch diff and returns their concatenated findings. Each
// child's spend is recorded on the ledger and reported per result.
func (o *run) runSpecialists(ctx context.Context, authoritative bool) (string, error) {
	d := o.d
	cfg := d.Cfg

	// The authoritative pass is FULL scope even when a delta snapshot exists: it
	// re-widens to the base branch so the strong panel reviews the whole change,
	// not just the latest increment.
	base := o.lastReviewBase
	if base == "" || authoritative {
		base = cfg.BaseBranch
	}

	diff, err := d.Git.Diff(ctx, base)
	if err != nil {
		return "", fmt.Errorf("review diff: %w", err)
	}

	panel := o.reviewPanel(ctx, estimateTokens(diff), authoritative)

	d.logCard(ctx, "review panel models: %s, %s, %s", panel[0].Model, panel[1].Model, panel[2].Model)

	lenses := []struct{ role, prompt string }{
		{"correctness", correctnessPrompt},
		{"design", designPrompt},
		{"security", securityPrompt},
	}

	// Prior findings are constant across the three lenses: the same previous-round
	// context goes to every specialist (cross-round memory). The authoritative pass
	// gets the FULL recorded history, not just the last round.
	priorText := o.lastFindings
	if authoritative {
		priorText = reviewFindingsHistory(o.body)
	}

	prior := priorFindingsBlock(priorText)

	specs := make([]harness.SubagentSpec, len(lenses))
	for i, l := range lenses {
		specs[i] = harness.SubagentSpec{
			Role:          l.role,
			Prompt:        fmt.Sprintf(specialistPrompt, o.skillEngage(), o.grounding, l.prompt, o.tc.Title, o.taskDescription, diff, prior),
			Model:         panel[i].Model,
			MaxTurns:      cfg.MaxTurns,
			ContextWindow: panel[i].ContextWindow,
		}
	}

	// Children inherit the parent phase-run routing (harness v0.7.x
	// SubagentOpts.Provider/Reasoning): both fields derive from the same
	// builder every parent model call uses, so parent and children can never
	// drift. Only Provider/Reasoning are read from parentCfg.
	parentCfg := o.harnessConfig(cfg.DefaultModel)

	results, err := harness.SpawnSubagents(ctx, d.Client, cfg.Workspace, d.Emit, specs,
		harness.SubagentOpts{
			DefaultModel:       cfg.DefaultModel,
			ToolOutputMaxBytes: cfg.ToolOutputMax,
			RedactToolOutput:   d.Redact,
			ExtraReadOnlyTools: skillToolSlice(d.SkillTool),
			Provider:           parentCfg.Provider,
			Reasoning:          parentCfg.Reasoning,
		})
	if err != nil {
		return "", fmt.Errorf("spawn review specialists: %w", err)
	}

	var b strings.Builder

	for i, res := range results {
		// The read-only specialist panel runs via harness.SpawnSubagents, which
		// exposes no per-subagent wall time, so duration is 0 (omitted on the
		// wire); phase and step still ride the report.
		o.spendAndReport(ctx, o.ledger, cfg.CardID, "review: report specialist usage failed",
			res.Result, specs[i].Model, "main", 0, "role", res.Role)

		b.WriteString("## ")
		b.WriteString(res.Role)
		b.WriteString(" findings\n")

		if res.Err != nil {
			slog.Warn("review: specialist run failed", "card_id", cfg.CardID, "role", res.Role, "error", res.Err)
			b.WriteString("(specialist run failed: " + res.Err.Error() + ")\n")
		} else {
			b.WriteString(res.Output)
			b.WriteString("\n")
		}
	}

	// Capture the reviewed head as the next round's delta base (mirrors CM's
	// review-task workflow skill, which records review_completed head=<sha>), so
	// rounds 2+ review only the change
	// since this review. Best-effort: the activity-log line is for the audit trail;
	// on crash-resume lastReviewBase starts empty and the next round re-runs full.
	if sha, herr := d.Git.Head(ctx); herr == nil && sha != "" {
		o.lastReviewBase = sha
		d.logCard(ctx, "review snapshot %s", sha)
	}

	return b.String(), nil
}

// reviewPanel returns the three specialist model specs. An explicit,
// catalog-resolvable reviewer pin overrides the entire panel (all three run on
// the pinned model). Otherwise the registry selects a diverse panel for the
// card tier, excluding every model that coded a subtask on this run.
func (o *run) reviewPanel(ctx context.Context, estTokens int, authoritative bool) []registry.ModelSpec {
	if resolvePin(o.d.Registry, o.tc.ModelReviewer) {
		spec := registry.ModelSpec{
			Model:         o.tc.ModelReviewer,
			ContextWindow: o.d.Registry.ContextWindow(o.tc.ModelReviewer),
		}

		panel := make([]registry.ModelSpec, reviewPanelSize)
		for i := range panel {
			panel[i] = spec
		}

		return panel
	}

	if o.tc.ModelReviewer != "" {
		o.warnUnresolvablePin(ctx, "reviewer", o.tc.ModelReviewer)
	}

	// The authoritative pass escalates the panel to the complex tier so the
	// strongest models judge the change before parking.
	tier := tierFromString(o.cardTier)
	if authoritative {
		tier = registry.TierComplex
	}

	return o.d.Registry.SelectReviewPanel(registry.SelectInput{
		Role:      registry.RoleReviewer,
		Tier:      tier,
		EstTokens: estTokens,
		// Exclude both the models that coded this run (a model must not review its
		// own work) and any model proven harness-incapable this run (recoverIncapable
		// records it). Merged so neither set masks the other.
		Exclude: o.reviewExclusions(),
	}, reviewPanelSize)
}

// reviewExclusions is the union of the coder models (a model must not review its
// own code) and the per-card incapable set (models that could not drive the tool
// loop). Both feed the review panel's diversity Exclude so neither is re-picked.
func (o *run) reviewExclusions() map[string]bool {
	excl := make(map[string]bool, len(o.coderModels)+len(o.excluded))
	for m := range o.coderModels {
		excl[m] = true
	}

	for m := range o.excluded {
		excl[m] = true
	}

	return excl
}

// mobReviewVerdict convenes the review discussion and parses its synthesis
// into the existing verdict shape, with ONE moderator repair on a parse
// failure (mirroring synthesize's repair turn). ok=false on any failure -
// the caller falls back to the specialist fan-out. On success it records the
// review snapshot head, exactly like runSpecialists, so later rounds stay
// delta-scoped.
func (o *run) mobReviewVerdict(ctx context.Context) (verdict, bool) {
	briefing, err := o.mobReviewBriefing(ctx)
	if err != nil {
		slog.Warn("mob review: briefing failed; solo fallback",
			"card_id", o.d.Cfg.CardID, "error", err)

		return verdict{}, false
	}

	seats := min(o.d.Cfg.Mob.Participants, len(reviewLenses))

	t := mob.Topic{
		Kind:     "review",
		Briefing: briefing,
		Lenses:   reviewLenses[:seats],
		Rounds:   1,
		Blind:    true,
		SynthesisPrompt: fmt.Sprintf(reviewSynthesisPrompt,
			o.grounding, o.tc.Title, o.taskDescription),
	}

	out, ok := o.mobDiscuss(ctx, t)
	if !ok {
		return verdict{}, false
	}

	v, perr := parseVerdict(out.Synthesis)
	if perr != nil {
		repaired, rerr := o.mobResynthesize(ctx, t, out, perr.Error())
		if rerr != nil {
			slog.Warn("mob review: repair synthesis failed; solo fallback",
				"card_id", o.d.Cfg.CardID, "error", rerr)

			return verdict{}, false
		}

		v, perr = parseVerdict(repaired)
		if perr != nil {
			slog.Warn("mob review: verdict parse failed after repair; solo fallback",
				"card_id", o.d.Cfg.CardID, "error", perr)

			return verdict{}, false
		}
	}

	if sha, herr := o.d.Git.Head(ctx); herr == nil && sha != "" {
		o.lastReviewBase = sha
		o.d.logCard(ctx, "review snapshot %s", sha)
	}

	return v, true
}

// mobReviewBriefing assembles the discussion briefing from the SAME scope
// the specialist fan-out reviews: the branch diff against the delta base
// (lastReviewBase when a prior round snapshotted, else the base branch) plus
// the prior round's findings.
func (o *run) mobReviewBriefing(ctx context.Context) (string, error) {
	base := o.lastReviewBase
	if base == "" {
		base = o.d.Cfg.BaseBranch
	}

	diff, err := o.d.Git.Diff(ctx, base)
	if err != nil {
		return "", fmt.Errorf("review diff: %w", err)
	}

	prior := priorFindingsBlock(o.lastFindings)

	return fmt.Sprintf(reviewBriefing, o.tc.Title, o.taskDescription, fencedDiff(diff), prior), nil
}

// synthesize runs ONE orchestrator-model call that reads the three specialists'
// findings and emits the structured verdict. The verdict JSON is parsed with the
// same extractJSON + one repair turn the planner uses.
func (o *run) synthesize(ctx context.Context, findings string, authoritative bool) (verdict, error) {
	d := o.d
	cfg := d.Cfg

	model := resolveDecisionModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel, o.excludedModels())

	var (
		v       verdict
		lastErr error
	)

	for attempt := range 2 {
		if err := o.ledger.Check(); err != nil {
			return verdict{}, err
		}

		repair := ""
		if attempt > 0 {
			repair = repairBlock(lastErr.Error())
		}

		priorText := o.lastFindings
		if authoritative {
			priorText = reviewFindingsHistory(o.body)
		}

		prior := priorFindingsBlock(priorText)

		task := fmt.Sprintf(synthesisPrompt, o.grounding, o.tc.Title, o.taskDescription, prior, findings, repair)

		res, dur, err := o.runModel(ctx, d.ReadTools, task, model)

		o.spendAndReport(ctx, o.ledger, cfg.CardID, "review: report synthesis usage failed", res, model, "main", dur)

		if err != nil {
			return verdict{}, fmt.Errorf("synthesis run: %w", err)
		}

		v, lastErr = parseVerdict(res.Output)
		if lastErr == nil {
			return v, nil
		}

		slog.Warn("review: verdict parse failed", "card_id", cfg.CardID, "attempt", attempt, "error", lastErr)
	}

	return verdict{}, fmt.Errorf("verdict parse failed after repair: %w", lastErr)
}

// runFixModel runs the fix coder harness with the same in-run incapable recovery
// as the subtask coder: it resolves the fix model (skipping the per-card exclude
// set), logs the pick, runs, and accounts for spend each attempt. An incapable
// model is blacklisted/excluded via recoverIncapable and the next-best fix model
// re-selected for the SAME round; the cap (shared with the coder path via
// o.reselects) parks the run when exhausted. A non-incapable run error returns
// immediately. The successful run's output is consumed inside the harness loop
// (the fixup targets files parsed from the findings, not the model output), so
// it returns only the model that ran and the error.
func (o *run) runFixModel(ctx context.Context, prompt string, round int, fixTier string, authoritative bool) (string, error) {
	d := o.d
	cfg := d.Cfg
	tier := o.fixTierEffective(fixTier, authoritative)

	for attempt := 0; attempt <= reselectCap; attempt++ {
		model := o.resolveFixModel(ctx, fixTier, authoritative)

		if o.fixEscalate {
			d.logCard(ctx, "fix coder %s selected for round %d fixes (tier=%s) - escalated after a fix round that %s",
				model, round, tier, o.fixFailReason)
		} else {
			d.logCard(ctx, "fix coder %s selected for round %d fixes (tier=%s)", model, round, tier)
		}

		res, dur, err := o.runModelCoder(ctx, d.WriteTools, prompt, model, fixWrapUpMessage, tier)

		o.spendAndReport(ctx, o.ledger, cfg.CardID, "review: report fix usage failed", res, model, "main", dur)

		var ie *IncapableError
		if errors.As(err, &ie) {
			if rerr := o.recoverIncapable(ctx, ie); rerr != nil {
				return "", rerr
			}

			continue
		}

		if err != nil {
			return "", fmt.Errorf("review fix run: %w", err)
		}

		return model, nil
	}

	// Unreachable: recoverIncapable errors at the cap before the loop exhausts.
	return "", fmt.Errorf("review fix (card=%s): re-selection loop exhausted", o.d.Cfg.CardID)
}

// runFix runs one coder fix pass against the outstanding findings, lands the
// changes as a fixup onto the commit that last touched the fixed files (HEAD
// fallback), and pushes. Budget is checked before the model call. It reports
// whether the fix landed a commit - a round that landed nothing is a failed
// round to the review loop. After a failed round it parks instead of running
// when no fix model other than the ones that failed is available.
func (o *run) runFix(ctx context.Context, findings string, round int, fixTier string, authoritative bool) (bool, error) {
	d := o.d
	cfg := d.Cfg

	if o.fixEscalate {
		if model := o.resolveFixModel(ctx, fixTier, authoritative); o.fixFailed[model] {
			d.logCard(ctx, "review parked: the previous fix round %s and no other fix model is available (tried: %s) - outstanding findings:\n%s",
				o.fixFailReason, strings.Join(sortedKeys(o.fixFailed), ", "), findings)

			return false, &ReviewParkedError{}
		}
	}

	if err := o.ledger.Check(); err != nil {
		return false, err
	}

	var prompt string
	if strings.HasPrefix(findings, verifyFailedPrefix) {
		// No reviewer ran: the finding is the one failing command, not a critique
		// of the whole card, so the parent card goes in title-only with an
		// explicit scope block instead of the full description.
		prompt = fmt.Sprintf(verifyFixPrompt, o.skillEngage(), o.grounding, cfg.Workspace,
			fixVerifyLine(o.resolvedVerifyPlan()), o.tc.Title, findings)
	} else {
		prompt = fmt.Sprintf(fixPrompt, o.skillEngage(), o.grounding, cfg.Workspace,
			fixVerifyLine(o.resolvedVerifyPlan()), o.tc.Title, o.taskDescription, findings)
	}

	model, err := o.runFixModel(ctx, prompt, round, fixTier, authoritative)
	if err != nil {
		return false, err
	}

	o.lastFixModel = model

	// Target the commit that last touched the fixed files so the fixup autosquashes
	// onto the right change; HEAD is the fallback when the path lookup yields
	// nothing (untracked files, or no path match).
	target, lerr := d.Git.LastCommitTouching(ctx, fixFiles(findings))
	if lerr != nil || target == "" {
		target = "HEAD"
	}

	committed, err := d.Git.CommitFixup(ctx, target)
	if err != nil {
		return false, fmt.Errorf("commit review fixup: %w", err)
	}

	if !committed {
		// The fix produced no commit: HEAD is unchanged, so the reviewed-head
		// snapshot captured this round would make the NEXT round diff HEAD...HEAD
		// (an empty delta). An empty-delta panel sees nothing to critique and can
		// approve, integrating the card with the unresolved finding and bypassing
		// the authoritative pass. Drop the snapshot so the next round re-widens to
		// the full base-branch diff and actually re-examines the outstanding work.
		o.lastReviewBase = ""

		return false, nil
	}

	if err := d.Git.Push(ctx, cfg.Branch); err != nil {
		return false, fmt.Errorf("push review fixup: %w", err)
	}

	return true, nil
}

// markFixFailed records that the most recent fix round failed for the given
// reason: its model is excluded from every later fix pick, and the next pick
// escalates.
func (o *run) markFixFailed(reason string) {
	if o.fixFailed == nil {
		o.fixFailed = map[string]bool{}
	}

	if o.lastFixModel != "" {
		o.fixFailed[o.lastFixModel] = true
	}

	o.fixEscalate = true
	o.fixFailReason = reason
}

// fixExclusions is the fix pick's exclusion set: models proven incapable this
// run plus models whose fix round failed.
func (o *run) fixExclusions() map[string]bool {
	out := make(map[string]bool, len(o.excluded)+len(o.fixFailed))
	maps.Copy(out, o.excluded)
	maps.Copy(out, o.fixFailed)

	return out
}

// fixFailedVendors is the set of vendors behind the failed fix models, for the
// escalated pick's vendor preference.
func (o *run) fixFailedVendors() map[string]bool {
	out := map[string]bool{}

	for model := range o.fixFailed {
		if v := o.d.Registry.Vendor(model); v != "" {
			out[v] = true
		}
	}

	return out
}

// sortedKeys lists a set's members in a stable order for log lines.
func sortedKeys(set map[string]bool) []string {
	keys := slices.Collect(maps.Keys(set))
	slices.Sort(keys)

	return keys
}

// resolveFixModel picks the coder model for the fix run: the card's coder pin
// when catalog-resolvable, else the best-value coder selection for the effective
// fix tier (the synthesizer's fix_tier, falling back to the card tier).
func (o *run) resolveFixModel(ctx context.Context, fixTier string, authoritative bool) string {
	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		// A pinned model is returned even if it is in o.excluded: we never override
		// an explicit operator pin with an auto-selected substitute. A pinned model
		// that is harness-incapable therefore keeps being re-selected, exhausts the
		// re-selection cap, and parks - the blacklist still records it.
		return o.tc.ModelCoder
	}

	if o.tc.ModelCoder != "" {
		o.warnUnresolvablePin(ctx, "coder", o.tc.ModelCoder)
	}

	in := registry.SelectInput{
		Role:    registry.RoleCoder,
		Tier:    o.fixTierEffective(fixTier, authoritative),
		Exclude: o.fixExclusions(),
	}

	if !o.fixEscalate {
		return o.d.Registry.SelectByComplexity(in).Model
	}

	// Escalating after a failed round: prefer a vendor that has not failed this
	// card, and fall back to any vendor when that leaves only a failed model.
	in.ExcludeVendors = o.fixFailedVendors()
	if spec := o.d.Registry.SelectByComplexity(in); !o.fixFailed[spec.Model] {
		return spec.Model
	}

	in.ExcludeVendors = nil

	return o.d.Registry.SelectByComplexity(in).Model
}

// fixTierEffective is the tier the fix coder is sized and selected on: fixTierFor,
// climbed one tier after a failed fix round.
func (o *run) fixTierEffective(fixTier string, authoritative bool) registry.Tier {
	tier := o.fixTierFor(fixTier, authoritative)
	if o.fixEscalate {
		return escalateTier(tier)
	}

	return tier
}

// escalateTier is one tier up; critical stays critical.
func escalateTier(t registry.Tier) registry.Tier {
	switch t {
	case registry.TierSimple:
		return registry.TierModerate
	case registry.TierModerate:
		return registry.TierComplex
	default:
		return registry.TierCritical
	}
}

// effectiveFixTier is the tier the fix run sizes on: the synthesizer's fix_tier
// when present, else the card tier. An empty fix_tier (synthesizer omitted it)
// falls back so the fixer is never under-sized.
func (o *run) effectiveFixTier(fixTier string) string {
	if fixTier == "" {
		return o.cardTier
	}

	return fixTier
}

// fixTierFor is the tier the fix coder is sized on: TierComplex on the
// authoritative pass (escalated), else the synthesizer's fix_tier with the card
// tier as fallback.
func (o *run) fixTierFor(fixTier string, authoritative bool) registry.Tier {
	if authoritative {
		return registry.TierComplex
	}

	return tierFromString(o.effectiveFixTier(fixTier))
}

// reviewFindingsHistory returns every "## Review Findings" section recorded on
// the parent body, concatenated - the full prior-findings context for the
// authoritative pass. Empty when none have been recorded yet.
func reviewFindingsHistory(body string) string {
	return strings.TrimSpace(sectionsWithPrefix(body, "Review Findings"))
}

// parseVerdict extracts the synthesis verdict JSON (tolerating prose / code
// fences) and unmarshals it. A missing object or malformed JSON is an error so
// the synthesis caller can take its single repair turn.
func parseVerdict(s string) (verdict, error) {
	raw, ok := extractJSON(s)
	if !ok {
		return verdict{}, fmt.Errorf("no JSON object found in synthesis output")
	}

	var v verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return verdict{}, fmt.Errorf("unmarshal verdict JSON: %w", err)
	}

	return v, nil
}

// formatFixes renders a verdict's fix list as the findings text carried into the
// fix run and (on cap exhaustion) the activity log. The "- <file>: <issue>" line
// shape is a contract with fixFiles, which parses the paths back out for fixup
// targeting - keep the two in sync.
func formatFixes(v verdict) string {
	var b strings.Builder

	if v.Summary != "" {
		b.WriteString(v.Summary)
		b.WriteString("\n")
	}

	for _, f := range v.Fixes {
		b.WriteString("- ")
		b.WriteString(f.File)
		b.WriteString(": ")
		b.WriteString(f.Issue)

		if f.Suggestion != "" {
			b.WriteString(" - ")
			b.WriteString(f.Suggestion)
		}

		b.WriteString("\n")
	}

	return b.String()
}

// fixFiles extracts the file paths referenced in the findings text so the fixup
// can target the commit that last touched them. It parses the "- <file>: ..."
// line shape formatFixes emits (mirror - keep the two in sync); lines without a
// leading path are ignored.
func fixFiles(findings string) []string {
	var (
		files []string
		seen  = map[string]bool{}
	)

	for line := range strings.SplitSeq(findings, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}

		rest := strings.TrimPrefix(trimmed, "- ")

		path, _, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}

		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}

		seen[path] = true
		files = append(files, path)
	}

	return files
}

// tierFromString maps a planner card-tier string to a registry.Tier. An empty
// or unrecognised value defaults to moderate (conservative: under-selecting a
// reviewer is worse than slightly over-paying).
func tierFromString(tier string) registry.Tier {
	switch tier {
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

// skillToolSlice wraps an optional Skill tool as a SubagentOpts.ExtraReadOnlyTools
// slice. Nil tool → nil slice (the review panel then gets the default read-only set).
func skillToolSlice(t tools.Tool) []tools.Tool {
	if t == nil {
		return nil
	}

	return []tools.Tool{t}
}
