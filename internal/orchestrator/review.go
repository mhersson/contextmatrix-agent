package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

// reviewPanelSize is the fixed number of review specialists fanned out per
// round: Correctness, Design & Maintainability, Security & Performance.
const reviewPanelSize = 3

// hardReviewIterationCap is a defensive ceiling on the review loop independent
// of the configured attempts cap, so a misbehaving IncrementReviewAttempts (or
// a zero cap) can never loop forever.
const hardReviewIterationCap = 50

// defaultReviewAttemptsCap is CM's review-attempts convention, used when the
// configured cap is missing or invalid. Three matches the runner's
// MAX_REVISION_PASSES; with the convergence safeguards in place, three rounds
// are enough.
const defaultReviewAttemptsCap = 3

// verifyTimeout bounds the spec/test gate subprocess.
const verifyTimeout = 10 * time.Minute

// verifyOutputTail caps the verify-command output carried into findings, so a
// noisy failing suite does not swamp the fix prompt.
const verifyOutputTail = 4000

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
// review. Parked is not failed — a human picks the card up from review.
type ReviewParkedError struct{ Findings string }

func (e *ReviewParkedError) Error() string {
	return "review parked: attempts cap exhausted without approval"
}

// runReview is the review phase. The parent enters review (idempotent on
// resume), then loops cheap incremental rounds: a detected verify gate runs first
// and short-circuits to the fix run on failure; otherwise three read-only
// specialists fan out on diverse models and one synthesis call decides
// approve-or-fix. Approval exits nil; each non-approval increments the card's
// review attempts and runs a fix. At the cliff (the round that would otherwise
// park) the gated authoritative pass takes over instead of parking on a cheap
// verdict — it is the sole park gate. The budget ledger is checked before every
// model-bearing step.
func runReview(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg

	// Idempotent on resume: only transition into review when not already there.
	if o.tc.State != "review" {
		if err := d.Ops.StartReview(ctx, cfg.CardID); err != nil {
			return fmt.Errorf("start review: %w", err)
		}
	}

	verifyCmd := detectVerifyCommand(cfg.Workspace)

	if cfg.Interactive {
		return o.runReviewHITL(ctx, verifyCmd)
	}

	// ===== autonomous loop (UNCHANGED below this line) =====

	// Guard a mis-wired worker: a zero or negative cap would make the cliff trip
	// on the FIRST round and park (via the authoritative pass) every card
	// immediately. Fall back to CM's convention instead.
	attemptsCap := cfg.ReviewAttemptsCap
	if attemptsCap <= 0 {
		attemptsCap = defaultReviewAttemptsCap
	}

	for iter := 0; iter < hardReviewIterationCap; iter++ {
		// Round number continues across resumes: review_attempts persists the
		// count of prior rounds, so round N is stable for the body record.
		round := o.tc.ReviewAttempts + iter + 1

		// At the cliff (the round that would otherwise park), run the gated
		// authoritative pass instead of another cheap round — never park on a
		// cheap verdict. It is terminal: returns nil (finished) or parks.
		if round >= attemptsCap {
			return o.authoritativeReview(ctx, verifyCmd, round)
		}

		findings, fixTier, approved, err := o.reviewRound(ctx, verifyCmd, false)
		if err != nil {
			return err
		}

		// Record this round on the parent card body for the complete review
		// history (the runner's review-task writes ## Review Findings the same way).
		o.recordReview(ctx, round, findings, approved)

		if approved {
			o.reviewSummary = findings // synthesis verdict summary, for the PR body

			return nil
		}

		// Carry this round's findings into the next round so the panel verifies
		// their resolution without importing new scope (cross-round memory).
		o.lastFindings = findings

		if _, err := d.Ops.IncrementReviewAttempts(ctx, cfg.CardID); err != nil {
			return fmt.Errorf("increment review attempts: %w", err)
		}

		if err := o.runFix(ctx, findings, round, fixTier, false); err != nil {
			return err
		}
	}

	return fmt.Errorf("review exceeded the hard iteration cap of %d", hardReviewIterationCap)
}

// runReviewHITL is the HITL review loop: each round produces specialist findings
// (verify gate + 3 specialists + synthesis), records them, and presents them to
// the human, who decides. Approve -> proceed to integrate; adjust -> apply the
// findings plus the human's feedback as a fix, then re-review. The human is the
// decision-maker, so there is no authoritative pass or auto-park; the hard
// iteration cap is only a runaway guard.
func (o *run) runReviewHITL(ctx context.Context, verifyCmd []string) error {
	d := o.d
	cfg := d.Cfg

	model := resolveDecisionModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel)

	for iter := 0; iter < hardReviewIterationCap; iter++ {
		round := o.tc.ReviewAttempts + iter + 1

		findings, fixTier, autoApproved, err := o.reviewRound(ctx, verifyCmd, false)
		if err != nil {
			return err
		}

		o.recordReview(ctx, round, findings, autoApproved)

		outcome, fb, gerr := o.gate(ctx, gateReviewDecision, model, presentFindings(findings, autoApproved))
		if gerr != nil {
			return gerr
		}

		if outcome == gateApprove {
			o.reviewSummary = findings

			return nil
		}

		o.lastFindings = findings

		if _, err := d.Ops.IncrementReviewAttempts(ctx, cfg.CardID); err != nil {
			return fmt.Errorf("increment review attempts: %w", err)
		}

		if err := o.runFix(ctx, mergeFeedback(findings, fb), round, fixTier, false); err != nil {
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
func (o *run) authoritativeReview(ctx context.Context, verifyCmd []string, round int) error {
	d := o.d
	cfg := d.Cfg

	findings, fixTier, approved, err := o.reviewRound(ctx, verifyCmd, true)
	if err != nil {
		return err
	}

	o.recordReview(ctx, round, findings, approved)

	if approved {
		o.reviewSummary = findings

		return nil
	}

	o.lastFindings = findings

	if _, err := d.Ops.IncrementReviewAttempts(ctx, cfg.CardID); err != nil {
		return fmt.Errorf("increment review attempts: %w", err)
	}

	// Gated strong fix — runs only because the authoritative review confirmed
	// real issues.
	if err := o.runFix(ctx, findings, round, fixTier, true); err != nil {
		return err
	}

	// One strong re-review of the full change.
	round2 := round + 1

	findings2, _, approved2, err := o.reviewRound(ctx, verifyCmd, true)
	if err != nil {
		return err
	}

	o.recordReview(ctx, round2, findings2, approved2)

	if approved2 {
		o.reviewSummary = findings2

		return nil
	}

	o.lastFindings = findings2

	n, err := d.Ops.IncrementReviewAttempts(ctx, cfg.CardID)
	if err != nil {
		return fmt.Errorf("increment review attempts: %w", err)
	}

	_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory; park must surface
		fmt.Sprintf("review parked after %d attempts (authoritative pass) — outstanding findings:\n%s", n, findings2))

	return &ReviewParkedError{Findings: findings2}
}

// reviewRound runs one review pass and returns the outstanding findings text,
// whether the work is approved, and any fatal error (budget park, transport).
// The verify gate runs first: on failure it short-circuits to (gate output,
// not-approved) WITHOUT spending reviewer tokens. On gate pass (or no gate), the
// three specialists fan out and the synthesis verdict decides.
func (o *run) reviewRound(ctx context.Context, verifyCmd []string, authoritative bool) (findings string, fixTier string, approved bool, err error) {
	// Budget gate before the verify subprocess too — the gate may be cheap, but
	// the fix run it can trigger is not, and we park before doing any work.
	if err := o.ledger.Check(); err != nil {
		return "", "", false, err
	}

	if len(verifyCmd) > 0 {
		out, ok := o.runVerify(ctx, verifyCmd)
		if !ok {
			// Gate failure goes STRAIGHT to the fix loop without burning reviewer
			// tokens. The command output (tail) is the finding the coder fixes. No
			// verdict ran, so the fix run falls back to the card tier (empty fixTier).
			return "verify command failed: " + strings.Join(verifyCmd, " ") + "\n" +
				tools.HeadTail(out, verifyOutputTail), "", false, nil
		}
	}

	// Gate passed (or none) — the gate is a cheap pre-filter, not a substitute
	// for review, so specialists always run.
	specialistFindings, err := o.runSpecialists(ctx, authoritative)
	if err != nil {
		return "", "", false, err
	}

	if err := o.ledger.Check(); err != nil {
		return "", "", false, err
	}

	v, err := o.synthesize(ctx, specialistFindings, authoritative)
	if err != nil {
		return "", "", false, err
	}

	if v.Approved {
		return v.Summary, v.FixTier, true, nil
	}

	return formatFixes(v), v.FixTier, false, nil
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

	panel := o.reviewPanel(estimateTokens(diff), authoritative)

	_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory selection record
		fmt.Sprintf("review panel models: %s, %s, %s", panel[0].Model, panel[1].Model, panel[2].Model))

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
			Prompt:        fmt.Sprintf(specialistPrompt, l.prompt, o.tc.Title, o.tc.Description, diff, prior),
			Model:         panel[i].Model,
			MaxTurns:      cfg.MaxTurns,
			ContextWindow: panel[i].ContextWindow,
		}
	}

	results, err := harness.SpawnSubagents(ctx, d.Client, cfg.Workspace, d.Emit, specs,
		harness.SubagentOpts{
			DefaultModel:       cfg.DefaultModel,
			ToolOutputMaxBytes: cfg.ToolOutputMax,
			RedactToolOutput:   d.Redact,
		})
	if err != nil {
		return "", fmt.Errorf("spawn review specialists: %w", err)
	}

	var b strings.Builder

	for i, res := range results {
		// Account for spend even on a child transport error / partial run, then
		// report the model actually used (falling back to the configured slug).
		o.ledger.Spend(res.Result.TotalCostUSD)

		used := res.Result.ModelUsed
		if used == "" {
			used = specs[i].Model
		}

		if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, used,
			res.Result.PromptTokens, res.Result.CompletionTokens, res.Result.TotalCostUSD); reportErr != nil {
			slog.Warn("review: report specialist usage failed", "card_id", cfg.CardID, "role", res.Role, "error", reportErr)
		}

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

	// Capture the reviewed head as the next round's delta base (mirrors the
	// runner's review_completed head=<sha>), so rounds 2+ review only the change
	// since this review. Best-effort: the activity-log line is for the audit trail;
	// on crash-resume lastReviewBase starts empty and the next round re-runs full.
	if sha, herr := d.Git.Head(ctx); herr == nil && sha != "" {
		o.lastReviewBase = sha
		_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory snapshot record
			fmt.Sprintf("review snapshot %s", sha))
	}

	return b.String(), nil
}

// reviewPanel returns the three specialist model specs. An explicit,
// catalog-resolvable reviewer pin overrides the entire panel (all three run on
// the pinned model). Otherwise the registry selects a diverse panel for the
// card tier, excluding every model that coded a subtask on this run.
func (o *run) reviewPanel(estTokens int, authoritative bool) []registry.ModelSpec {
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

// synthesize runs ONE orchestrator-model call that reads the three specialists'
// findings and emits the structured verdict. The verdict JSON is parsed with the
// same extractJSON + one repair turn the planner uses.
func (o *run) synthesize(ctx context.Context, findings string, authoritative bool) (verdict, error) {
	d := o.d
	cfg := d.Cfg

	model := resolveDecisionModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel)

	var (
		v       verdict
		lastErr error
	)

	for attempt := 0; attempt < 2; attempt++ {
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

		task := fmt.Sprintf(synthesisPrompt, o.tc.Title, o.tc.Description, prior, findings, repair)

		res, err := o.runModel(ctx, d.ReadTools, task, model)

		o.ledger.Spend(res.TotalCostUSD)

		if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, res.ModelUsed,
			res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
			slog.Warn("review: report synthesis usage failed", "card_id", cfg.CardID, "error", reportErr)
		}

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
// only the error is returned.
func (o *run) runFixModel(ctx context.Context, prompt string, round int, fixTier string, authoritative bool) error {
	d := o.d
	cfg := d.Cfg

	for attempt := 0; attempt <= reselectCap; attempt++ {
		model := o.resolveFixModel(fixTier, authoritative)

		_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory selection record
			fmt.Sprintf("fix coder %s selected for round %d fixes (tier=%s)", model, round, o.fixTierFor(fixTier, authoritative)))

		res, err := o.runModel(ctx, d.WriteTools, prompt, model)

		o.ledger.Spend(res.TotalCostUSD)

		used := res.ModelUsed
		if used == "" {
			used = model
		}

		if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, used,
			res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
			slog.Warn("review: report fix usage failed", "card_id", cfg.CardID, "error", reportErr)
		}

		var ie *IncapableError
		if errors.As(err, &ie) {
			if rerr := o.recoverIncapable(ctx, ie); rerr != nil {
				return rerr
			}

			continue
		}

		if err != nil {
			return fmt.Errorf("review fix run: %w", err)
		}

		return nil
	}

	// Unreachable: recoverIncapable errors at the cap before the loop exhausts.
	return fmt.Errorf("review fix (card=%s): re-selection loop exhausted", o.d.Cfg.CardID)
}

// runFix runs one coder fix pass against the outstanding findings, lands the
// changes as a fixup onto the commit that last touched the fixed files (HEAD
// fallback), and pushes. Budget is checked before the model call.
func (o *run) runFix(ctx context.Context, findings string, round int, fixTier string, authoritative bool) error {
	d := o.d
	cfg := d.Cfg

	if err := o.ledger.Check(); err != nil {
		return err
	}

	prompt := fmt.Sprintf(fixPrompt, o.tc.Title, o.tc.Description, findings)

	if err := o.runFixModel(ctx, prompt, round, fixTier, authoritative); err != nil {
		return err
	}

	// Target the commit that last touched the fixed files so the fixup autosquashes
	// onto the right change; HEAD is the fallback when the path lookup yields
	// nothing (untracked files, or no path match).
	target, lerr := d.Git.LastCommitTouching(ctx, fixFiles(findings))
	if lerr != nil || target == "" {
		target = "HEAD"
	}

	committed, err := d.Git.CommitFixup(ctx, target)
	if err != nil {
		return fmt.Errorf("commit review fixup: %w", err)
	}

	if committed {
		if err := d.Git.Push(ctx, cfg.Branch); err != nil {
			return fmt.Errorf("push review fixup: %w", err)
		}
	}

	return nil
}

// resolveFixModel picks the coder model for the fix run: the card's coder pin
// when catalog-resolvable, else the best-value coder selection for the effective
// fix tier (the synthesizer's fix_tier, falling back to the card tier).
func (o *run) resolveFixModel(fixTier string, authoritative bool) string {
	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		// A pinned model is returned even if it is in o.excluded: we never override
		// an explicit operator pin with an auto-selected substitute. A pinned model
		// that is harness-incapable therefore keeps being re-selected, exhausts the
		// re-selection cap, and parks — the blacklist still records it.
		return o.tc.ModelCoder
	}

	spec := o.d.Registry.SelectByComplexity(registry.SelectInput{
		Role:    registry.RoleCoder,
		Tier:    o.fixTierFor(fixTier, authoritative),
		Exclude: o.excluded,
	})

	return spec.Model
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
// the parent body, concatenated — the full prior-findings context for the
// authoritative pass. Empty when none have been recorded yet.
func reviewFindingsHistory(body string) string {
	var b strings.Builder

	in := false

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.HasPrefix(line, "## Review Findings")
		}

		if in {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	return strings.TrimSpace(b.String())
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

// detectVerifyCommand best-effort selects the project's test command from the
// workspace markers, in priority order: a Makefile declaring a "test" target ->
// ["make","test"]; else a go.mod -> ["go","test","./..."]; else a package.json
// declaring a scripts.test -> ["npm","test"]; none -> nil (gate skipped).
func detectVerifyCommand(workspace string) []string {
	if makefileHasTestTarget(filepath.Join(workspace, "Makefile")) {
		return []string{"make", "test"}
	}

	if fileExists(filepath.Join(workspace, "go.mod")) {
		return []string{"go", "test", "./..."}
	}

	if packageJSONHasTestScript(filepath.Join(workspace, "package.json")) {
		return []string{"npm", "test"}
	}

	return nil
}

// makefileHasTestTarget reports whether path is a readable Makefile declaring a
// "test:" target. Make targets are declared at column 0, so the match is
// deliberately untrimmed — indented lines (recipes, comments) never match.
func makefileHasTestTarget(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "test:") {
			return true
		}
	}

	return false
}

// packageJSONHasTestScript reports whether path is a readable package.json whose
// scripts object declares a non-empty "test" entry.
func packageJSONHasTestScript(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}

	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}

	return strings.TrimSpace(pkg.Scripts["test"]) != ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}

// execVerify runs argv in workspace with a scrubbed env and a 10-minute timeout,
// returning the combined output and whether it exited cleanly. The command is
// always rooted at the workspace (cmd.Dir) so a verify run cannot escape it.
func execVerify(ctx context.Context, workspace string, argv []string) (string, bool) {
	if len(argv) == 0 {
		return "", true
	}

	cctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...) //nolint:gosec // argv is code-selected, not model input
	cmd.Dir = workspace
	cmd.Env = tools.ScrubbedEnv(nil)

	out, err := cmd.CombinedOutput()

	return string(out), err == nil
}

// formatFixes renders a verdict's fix list as the findings text carried into the
// fix run and (on cap exhaustion) the activity log. The "- <file>: <issue>" line
// shape is a contract with fixFiles, which parses the paths back out for fixup
// targeting — keep the two in sync.
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
			b.WriteString(" — ")
			b.WriteString(f.Suggestion)
		}

		b.WriteString("\n")
	}

	return b.String()
}

// fixFiles extracts the file paths referenced in the findings text so the fixup
// can target the commit that last touched them. It parses the "- <file>: ..."
// line shape formatFixes emits (mirror — keep the two in sync); lines without a
// leading path are ignored.
func fixFiles(findings string) []string {
	var (
		files []string
		seen  = map[string]bool{}
	)

	for _, line := range strings.Split(findings, "\n") {
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
