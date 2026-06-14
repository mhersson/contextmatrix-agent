package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// validTiers is the closed set of complexity tiers the planner may emit, for
// both the overall card and each subtask. It drives reviewer selection later.
var validTiers = map[string]bool{"simple": true, "moderate": true, "complex": true}

// maxSubtasks caps a single plan; a runaway decomposition is a planning bug,
// not a valid plan.
const maxSubtasks = 20

// planSubtask is one decomposed unit of work in the planner's JSON output.
type planSubtask struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	DependsOn   []int  `json:"depends_on"`
	Tier        string `json:"tier"`
}

// plan is the planner's structured final output: the overall card tier plus the
// ordered subtask list. depends_on indices reference earlier entries only.
type plan struct {
	CardTier string        `json:"card_tier"`
	Subtasks []planSubtask `json:"subtasks"`
}

// subtaskRef is a created subtask carried on the run struct for the execute
// phase: the real card ID, its title, body, tier, current state, and the real
// card IDs it depends on. State drives resume skipping in the execute phase
// ("done" subtasks are not re-run); plan-created subtasks start "todo". Body
// carries the planner's description (file lists, acceptance criteria) into the
// coder prompt; resume-loaded refs lack it (SubtaskStates has no body field).
type subtaskRef struct {
	ID           string
	Title        string
	Body         string
	Tier         string
	State        string
	DependsOnIDs []string
}

// parsePlan extracts a JSON object from s (tolerating prose / code-fence wrap)
// and validates it: 1..maxSubtasks subtasks, valid card and subtask tiers, and
// depends_on indices that reference only earlier subtasks (no self/forward refs).
func parsePlan(s string) (plan, error) {
	raw, ok := extractJSON(s)
	if !ok {
		return plan{}, fmt.Errorf("no JSON object found in planner output")
	}

	var p plan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return plan{}, fmt.Errorf("unmarshal plan JSON: %w", err)
	}

	if !validTiers[p.CardTier] {
		return plan{}, fmt.Errorf("invalid card_tier %q (want simple|moderate|complex)", p.CardTier)
	}

	if len(p.Subtasks) == 0 {
		return plan{}, fmt.Errorf("plan has no subtasks")
	}

	if len(p.Subtasks) > maxSubtasks {
		return plan{}, fmt.Errorf("plan has %d subtasks, max %d", len(p.Subtasks), maxSubtasks)
	}

	for i, st := range p.Subtasks {
		if strings.TrimSpace(st.Title) == "" {
			return plan{}, fmt.Errorf("subtask %d has an empty title", i)
		}

		if !validTiers[st.Tier] {
			return plan{}, fmt.Errorf("subtask %d has invalid tier %q (want simple|moderate|complex)", i, st.Tier)
		}

		for _, dep := range st.DependsOn {
			if dep < 0 || dep >= len(p.Subtasks) {
				return plan{}, fmt.Errorf("subtask %d depends_on index %d out of range [0,%d)", i, dep, len(p.Subtasks))
			}

			if dep >= i {
				return plan{}, fmt.Errorf("subtask %d depends_on index %d must reference an earlier subtask", i, dep)
			}
		}
	}

	return p, nil
}

// extractJSON returns the JSON object the model intended as its answer. It
// prefers a fenced ```json block (models wrap the verdict in one and surround it
// with prose that contains stray braces), and otherwise returns the LAST
// balanced top-level object — robust to prose/code braces appearing before it.
func extractJSON(s string) (string, bool) {
	if fenced, ok := extractFenced(s); ok {
		s = fenced
	}

	depth, start := 0, -1
	lastStart, lastEnd := -1, -1
	inStr, escaped := false, false

	for i := 0; i < len(s); i++ {
		c := s[i]

		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}

			continue
		}

		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}

			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					lastStart, lastEnd = start, i+1
				}
			}
		}
	}

	if lastStart < 0 {
		return "", false
	}

	return s[lastStart:lastEnd], true
}

// extractFenced returns the body of the first ```json (or bare ```) fenced block.
func extractFenced(s string) (string, bool) {
	i := strings.Index(s, "```")
	if i < 0 {
		return "", false
	}

	rest := s[i+3:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 { // drop the optional "json" tag line
		rest = rest[nl+1:]
	}

	end := strings.Index(rest, "```")
	if end < 0 {
		return "", false
	}

	return rest[:end], true
}

// resolvePin reports whether a non-empty card-pinned model slug is honourable:
// the registry exists and the slug is present in the live catalog. Empty pins
// and unknown slugs are not honoured. Both the orchestrator-model resolution and
// the per-subtask coder-model resolution gate on this.
func resolvePin(reg *registry.Registry, pin string) bool {
	return pin != "" && reg != nil && reg.Has(pin)
}

// resolveOrchestratorModel picks the model the orchestrator's own model-bearing
// phases (plan, review synthesis, docs) run on. Precedence:
//  1. the card pin (pinned), if it is catalog-resolvable;
//  2. else warn (slog + card log) and fall to payload;
//  3. payload (CM's default_model from the trigger), if set;
//  4. else the serve-config default.
//
// A best-effort card-log failure is swallowed — the warning is advisory.
func resolveOrchestratorModel(
	ctx context.Context,
	reg *registry.Registry,
	emit *events.Emitter,
	ops Ops,
	cardID, pinned, payload, fallback string,
) string {
	if pinned != "" {
		if resolvePin(reg, pinned) {
			return pinned
		}

		target := payload
		if target == "" {
			target = fallback
		}

		slog.Warn("orchestrator model pin not in catalog, falling back",
			"card_id", cardID, "requested", pinned, "using", target)

		if emit != nil {
			emit.Emit(events.StateChange, map[string]any{
				"warning":   "orchestrator model pin not in catalog, using fallback",
				"requested": pinned,
				"using":     target,
			})
		}

		_ = ops.AddLog(ctx, cardID, //nolint:errcheck // advisory note; failure must not abort planning
			fmt.Sprintf("orchestrator model pin %q not in catalog — using %q", pinned, target))
	}

	if payload != "" {
		return payload
	}

	return fallback
}

// isBudgetError reports whether err is (or wraps) the budget-ceiling sentinel.
func isBudgetError(err error) bool {
	var be *BudgetExceededError

	return errors.As(err, &be)
}

// runDiagnose runs one read-only investigation pass on the orchestrator model
// for a bug-like card and returns a "## Diagnosis" text blob to ground the
// plan. Budget-checked and usage-reported like every model-bearing step. The
// caller treats a returned error as best-effort: planning proceeds without a
// diagnosis rather than failing.
func (o *run) runDiagnose(ctx context.Context, model string) (string, error) {
	d := o.d
	cfg := d.Cfg

	if err := o.ledger.Check(); err != nil {
		return "", err
	}

	task := fmt.Sprintf(diagnosePrompt, o.tc.Title, o.tc.Description)

	res, err := o.runModel(ctx, d.ReadTools, task, model)

	o.ledger.Spend(res.TotalCostUSD)

	if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, res.ModelUsed,
		res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
		slog.Warn("plan: report diagnose usage failed", "card_id", cfg.CardID, "error", reportErr)
	}

	if err != nil {
		return "", fmt.Errorf("diagnose run: %w", err)
	}

	return strings.TrimSpace(res.Output), nil
}

// runPlan is the plan phase: one read-only planner run on the
// orchestrator-resolved model that emits a strict JSON plan, then code creates
// a subtask card per entry with dependency edges mapped to real card IDs.
//
// The model is called at most twice: the initial attempt plus ONE repair turn
// re-prompted with the parse error. The budget ledger is checked before EVERY
// model call and every call's usage is spent + reported.
func runPlan(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg

	model := resolveOrchestratorModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel)

	_ = d.Ops.AddLog(ctx, cfg.CardID, "orchestrator model: "+model) //nolint:errcheck // advisory selection record

	// Bug-like cards get a read-only root-cause investigation before planning
	// (mirrors the runner's create-plan Phase 0 Branch B). The diagnosis grounds
	// the decomposition. Best-effort: a failed diagnose must not block planning.
	diagnosis := ""

	if isBugLike(o.tc) {
		_ = d.Ops.AddLog(ctx, cfg.CardID, "running root-cause investigation (bug-like card)") //nolint:errcheck

		diag, derr := o.runDiagnose(ctx, model)
		switch {
		case derr == nil:
			diagnosis = diag
			if strings.TrimSpace(diag) != "" {
				// Record the root-cause investigation on the parent card body,
				// like the runner's systematic-debugging skill writes ## Diagnosis.
				o.recordSection(ctx, "Diagnosis", sectionFrom("Diagnosis", diag))
			}
		case isBudgetError(derr):
			return derr // park: the FSM's execute() maps this to the budget log
		default:
			slog.Warn("plan: diagnose step failed; planning without a diagnosis",
				"card_id", cfg.CardID, "error", derr)
		}
	}

	// Resume: surface any existing subtasks so the planner reuses their titles.
	// The list is the RECONCILED set (reconcile loaded it from SubtaskStates
	// before the phase loop); runPlan does not re-query the server. On a fresh
	// run o.subtasks is empty, yielding an empty resume block.
	var existingTitles []string
	for _, sub := range o.subtasks {
		existingTitles = append(existingTitles, sub.Title)
	}

	resume := resumeBlock(existingTitles)
	diagBlock := diagnosisBlock(diagnosis)

	var (
		p       plan
		lastErr error
	)

	// Initial attempt + at most one repair turn.
	for attempt := 0; attempt < 2; attempt++ {
		if err := o.ledger.Check(); err != nil {
			return err
		}

		repair := ""
		if attempt > 0 {
			repair = repairBlock(lastErr.Error())
		}

		task := fmt.Sprintf(planPrompt, o.tc.Title, o.tc.Description, diagBlock, resume, repair)

		res, err := o.runModel(ctx, d.ReadTools, task, model)

		// Account for spend even on transport error / partial run.
		o.ledger.Spend(res.TotalCostUSD)

		if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, res.ModelUsed,
			res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
			slog.Warn("plan: report usage failed", "card_id", cfg.CardID, "error", reportErr)
		}

		if err != nil {
			return fmt.Errorf("planner run: %w", err)
		}

		p, lastErr = parsePlan(res.Output)
		if lastErr == nil {
			break
		}

		slog.Warn("plan: parse failed", "card_id", cfg.CardID, "attempt", attempt, "error", lastErr)
	}

	if lastErr != nil {
		return fmt.Errorf("plan parse failed after repair: %w", lastErr)
	}

	return o.createSubtasks(ctx, p)
}

// createSubtasks creates one card per plan subtask in order, mapping each
// depends_on index to the real card ID returned for that earlier subtask, and
// records the resulting refs (plus the overall card tier) on the run struct.
//
// Creation order is deterministic (plan order), and depends_on validation in
// parsePlan guarantees every referenced index is already created when used, so
// the index→ID map is always complete at lookup time. CM's duplicate-subtask
// guard makes re-entry idempotent: an existing card's ID is returned and used
// as the dependency target exactly like a freshly created one.
func (o *run) createSubtasks(ctx context.Context, p plan) error {
	d := o.d
	cfg := d.Cfg

	ids := make([]string, len(p.Subtasks))
	o.subtasks = make([]subtaskRef, 0, len(p.Subtasks))

	for i, st := range p.Subtasks {
		depIDs := make([]string, 0, len(st.DependsOn))
		for _, dep := range st.DependsOn {
			depIDs = append(depIDs, ids[dep])
		}

		id, err := d.Ops.CreateCard(ctx, cfg.Project, cfg.CardID, st.Title, st.Description, depIDs)
		if err != nil {
			return fmt.Errorf("create subtask %q: %w", st.Title, err)
		}

		ids[i] = id
		o.subtasks = append(o.subtasks, subtaskRef{
			ID:           id,
			Title:        st.Title,
			Body:         st.Description,
			Tier:         st.Tier,
			State:        "todo", // freshly created; resume reconciliation refreshes this
			DependsOnIDs: depIDs,
		})
	}

	o.cardTier = p.CardTier

	// Record the plan on the parent card body so it carries the full history
	// (the subtask cards hold the detail; this is the consolidated view, like
	// the runner's create-plan writes ## Plan).
	o.recordSection(ctx, "Plan", sectionFrom("Plan", formatPlan(o.subtasks)))

	return nil
}
