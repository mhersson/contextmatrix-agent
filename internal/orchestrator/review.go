package orchestrator

import (
	"context"
	"encoding/json"
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
// resume), then loops: a cheap detected verify gate runs first and short-circuits
// to the fix run on failure; otherwise three read-only specialists fan out on
// diverse models and one synthesis call decides approve-or-fix. Approval exits
// nil. Each non-approval increments the card's review attempts; reaching the cap
// parks the card. The budget ledger is checked before every model-bearing step.
func runReview(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg

	// Idempotent on resume: only transition into review when not already there.
	if o.tc.State != "review" {
		if err := d.Ops.StartReview(ctx, cfg.CardID); err != nil {
			return fmt.Errorf("start review: %w", err)
		}
	}

	// Guard a mis-wired worker: a zero or negative cap would make n >= cap true
	// on the FIRST non-approval and park every card immediately. Fall back to
	// CM's convention instead.
	attemptsCap := cfg.ReviewAttemptsCap
	if attemptsCap <= 0 {
		attemptsCap = defaultReviewAttemptsCap
	}

	verifyCmd := detectVerifyCommand(cfg.Workspace)

	for iter := 0; iter < hardReviewIterationCap; iter++ {
		// Round number continues across resumes: review_attempts persists the
		// count of prior rounds, so round N is stable for the body record.
		round := o.tc.ReviewAttempts + iter + 1

		findings, approved, err := o.reviewRound(ctx, verifyCmd)
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

		// Not approved: count the attempt. Reaching the cap parks the card for a
		// human rather than burning another fix round.
		n, err := d.Ops.IncrementReviewAttempts(ctx, cfg.CardID)
		if err != nil {
			return fmt.Errorf("increment review attempts: %w", err)
		}

		if n >= attemptsCap {
			// Best-effort log; the parked sentinel is what must surface.
			_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory; park must surface
				fmt.Sprintf("review parked after %d attempts — outstanding findings:\n%s", n, findings))

			return &ReviewParkedError{Findings: findings}
		}

		if err := o.runFix(ctx, findings); err != nil {
			return err
		}
	}

	return fmt.Errorf("review exceeded the hard iteration cap of %d", hardReviewIterationCap)
}

// reviewRound runs one review pass and returns the outstanding findings text,
// whether the work is approved, and any fatal error (budget park, transport).
// The verify gate runs first: on failure it short-circuits to (gate output,
// not-approved) WITHOUT spending reviewer tokens. On gate pass (or no gate), the
// three specialists fan out and the synthesis verdict decides.
func (o *run) reviewRound(ctx context.Context, verifyCmd []string) (string, bool, error) {
	// Budget gate before the verify subprocess too — the gate may be cheap, but
	// the fix run it can trigger is not, and we park before doing any work.
	if err := o.ledger.Check(); err != nil {
		return "", false, err
	}

	if len(verifyCmd) > 0 {
		out, ok := o.runVerify(ctx, verifyCmd)
		if !ok {
			// Gate failure goes STRAIGHT to the fix loop without burning reviewer
			// tokens. The command output (tail) is the finding the coder fixes.
			return "verify command failed: " + strings.Join(verifyCmd, " ") + "\n" +
				tools.HeadTail(out, verifyOutputTail), false, nil
		}
	}

	// Gate passed (or none) — the gate is a cheap pre-filter, not a substitute
	// for review, so specialists always run.
	findings, err := o.runSpecialists(ctx)
	if err != nil {
		return "", false, err
	}

	if err := o.ledger.Check(); err != nil {
		return "", false, err
	}

	v, err := o.synthesize(ctx, findings)
	if err != nil {
		return "", false, err
	}

	if v.Approved {
		return v.Summary, true, nil
	}

	return formatFixes(v), false, nil
}

// runSpecialists fans the three review lenses out as parallel read-only child
// agents over the branch diff and returns their concatenated findings. Each
// child's spend is recorded on the ledger and reported per result.
func (o *run) runSpecialists(ctx context.Context) (string, error) {
	d := o.d
	cfg := d.Cfg

	diff, err := d.Git.Diff(ctx, cfg.BaseBranch)
	if err != nil {
		return "", fmt.Errorf("review diff: %w", err)
	}

	panel := o.reviewPanel(estimateTokens(diff))

	_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory selection record
		fmt.Sprintf("review panel models: %s, %s, %s", panel[0].Model, panel[1].Model, panel[2].Model))

	lenses := []struct{ role, prompt string }{
		{"correctness", correctnessPrompt},
		{"design", designPrompt},
		{"security", securityPrompt},
	}

	specs := make([]harness.SubagentSpec, len(lenses))
	for i, l := range lenses {
		specs[i] = harness.SubagentSpec{
			Role:          l.role,
			Prompt:        fmt.Sprintf(specialistPrompt, l.prompt, o.tc.Title, o.tc.Description, diff),
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

	return b.String(), nil
}

// reviewPanel returns the three specialist model specs. An explicit,
// catalog-resolvable reviewer pin overrides the entire panel (all three run on
// the pinned model). Otherwise the registry selects a diverse panel for the
// card tier, excluding every model that coded a subtask on this run.
func (o *run) reviewPanel(estTokens int) []registry.ModelSpec {
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

	return o.d.Registry.SelectReviewPanel(registry.SelectInput{
		Role:      registry.RoleReviewer,
		Tier:      tierFromString(o.cardTier),
		EstTokens: estTokens,
		Exclude:   o.coderModels,
	}, reviewPanelSize)
}

// synthesize runs ONE orchestrator-model call that reads the three specialists'
// findings and emits the structured verdict. The verdict JSON is parsed with the
// same extractJSON + one repair turn the planner uses.
func (o *run) synthesize(ctx context.Context, findings string) (verdict, error) {
	d := o.d
	cfg := d.Cfg

	model := resolveOrchestratorModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
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

		task := fmt.Sprintf(synthesisPrompt, o.tc.Title, o.tc.Description, findings, repair)

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

// runFix runs one coder fix pass against the outstanding findings, lands the
// changes as a fixup onto the commit that last touched the fixed files (HEAD
// fallback), and pushes. Budget is checked before the model call.
func (o *run) runFix(ctx context.Context, findings string) error {
	d := o.d
	cfg := d.Cfg

	if err := o.ledger.Check(); err != nil {
		return err
	}

	model := o.resolveFixModel()

	prompt := fmt.Sprintf(fixPrompt, o.tc.Title, o.tc.Description, findings)

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

	if err != nil {
		return fmt.Errorf("review fix run: %w", err)
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
// when catalog-resolvable, else the best-value coder selection for the card tier.
func (o *run) resolveFixModel() string {
	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		return o.tc.ModelCoder
	}

	spec := o.d.Registry.SelectByComplexity(registry.SelectInput{
		Role: registry.RoleCoder,
		Tier: tierFromString(o.cardTier),
	})

	return spec.Model
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
	default:
		return registry.TierModerate
	}
}
