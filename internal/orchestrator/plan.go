package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
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
// phase: the real card ID, its title, tier, and the real card IDs it depends on.
type subtaskRef struct {
	ID           string
	Title        string
	Tier         string
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

// extractJSON returns the first balanced top-level JSON object in s, tolerating
// prose and code fences around it. It scans from the first '{' and tracks brace
// depth while skipping over string literals (so braces inside strings don't
// throw off the balance). Returns false when no complete object is present.
func extractJSON(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}

	depth := 0
	inStr := false
	escaped := false

	for i := start; i < len(s); i++ {
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
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}

	return "", false
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
		if reg != nil && reg.Has(pinned) {
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

	// Resume: surface any existing subtasks so the planner reuses their titles.
	var existingTitles []string

	if states, err := d.Ops.SubtaskStates(ctx, cfg.Project, cfg.CardID); err == nil {
		for _, st := range states {
			existingTitles = append(existingTitles, st.Title)
		}
	} else {
		slog.Warn("plan: subtask states unavailable, planning without resume block",
			"card_id", cfg.CardID, "error", err)
	}

	resume := resumeBlock(existingTitles)

	hcfg := harness.Config{
		Model:    model,
		MaxTurns: cfg.MaxTurns,
	}

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

		task := fmt.Sprintf(planPrompt, o.tc.Title, o.tc.Description, resume, repair)

		res, err := harness.Run(ctx, d.Client, d.ReadTools, d.Emit, task, hcfg)

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
			Tier:         st.Tier,
			DependsOnIDs: depIDs,
		})
	}

	o.cardTier = p.CardTier

	return nil
}
