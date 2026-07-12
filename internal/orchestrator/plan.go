package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/coop"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
)

// validTiers is the closed set of complexity tiers the planner may emit, for
// both the overall card and each subtask. It drives reviewer selection later.
var validTiers = map[string]bool{"simple": true, "moderate": true, "complex": true, "critical": true}

// maxSubtasks caps a single plan; a runaway decomposition is a planning bug,
// not a valid plan.
const maxSubtasks = 20

// maxPlanDrafts bounds the HITL plan-approval re-draft loop: a human who keeps
// adjusting can iterate, but a runaway never spins forever (they end a run via
// end_session). The cap is generous; reaching it is an error.
const maxPlanDrafts = 10

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
		return plan{}, fmt.Errorf("invalid card_tier %q (want simple|moderate|complex|critical)", p.CardTier)
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
			return plan{}, fmt.Errorf("subtask %d has invalid tier %q (want simple|moderate|complex|critical)", i, st.Tier)
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

// extractJSON returns the JSON object the model intended as its answer. A
// whole-output bare object is returned as-is — fence markers inside its string
// values must never trigger fence-stripping, which is not string-aware and
// would mangle the payload. Otherwise it prefers a fenced ```json block
// (models wrap the verdict in one and surround it with prose that contains
// stray braces), and finally returns the LAST balanced top-level object —
// robust to prose/code braces appearing before it.
func extractJSON(s string) (string, bool) {
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
		return t, true
	}

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
	_, after, ok := strings.Cut(s, "```")
	if !ok {
		return "", false
	}

	rest := after
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 { // drop the optional "json" tag line
		rest = rest[nl+1:]
	}

	before, _, ok := strings.Cut(rest, "```")
	if !ok {
		return "", false
	}

	return before, true
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

// resolveDecisionModel resolves the model an orchestrator DECISION phase runs on
// (plan decomposition, review synthesis). These phases are reasoning- and
// calibration-sensitive — a weak model emits malformed plans and mis-calibrated
// verdicts — so, unlike the low-stakes docs phase, they are floored to a capable
// judgment model. A catalog-resolvable ModelOrchestrator pin still wins (operator
// override; an unresolvable pin already warned inside resolveOrchestratorModel).
// Otherwise the floor is the same best-value selection the authoritative review
// panel uses — RoleReviewer @ TierComplex — the measured proxy for orchestrator-
// level judgment (the live catalog measures only coder/reviewer; reviewer is the
// closer fit for both decomposing and judging). Fixed at TierComplex for EVERY
// call: decision quality does not scale with task complexity, so even a trivial
// card gets a calibrated judge. Degrades to the base resolution when no registry
// is present.
func resolveDecisionModel(
	ctx context.Context,
	reg *registry.Registry,
	emit *events.Emitter,
	ops Ops,
	cardID, pinned, payload, fallback string,
) string {
	base := resolveOrchestratorModel(ctx, reg, emit, ops, cardID, pinned, payload, fallback)

	// A resolvable operator pin is authoritative — never floor over it.
	if resolvePin(reg, pinned) || reg == nil {
		return base
	}

	floor := reg.SelectByComplexity(registry.SelectInput{
		Role: registry.RoleReviewer,
		Tier: registry.TierComplex,
	}).Model
	if floor == "" {
		return base // defensive; SelectByComplexity does not return empty today
	}

	return floor
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

	task := fmt.Sprintf(diagnosePrompt, o.grounding, cfg.Workspace, o.tc.Title, o.tc.Description)

	res, err := o.runModelImages(ctx, d.ReadTools, task, model, o.taskImages)

	o.ledger.Spend(res.TotalCostUSD)

	used := res.ModelUsed
	if used == "" {
		used = model
	}

	if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, used,
		res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
		slog.Warn("plan: report diagnose usage failed", "card_id", cfg.CardID, "error", reportErr)
	}

	if err != nil {
		return "", fmt.Errorf("diagnose run: %w", err)
	}

	return strings.TrimSpace(res.Output), nil
}

// draftPlan runs the read-only planner (initial attempt + at most one repair
// turn) and returns the parsed plan. diagnosis grounds bug-like cards; design
// carries the brainstormed agreed design for creative HITL cards; feedback
// carries a HITL reviewer's requested changes on a re-draft; all collapse to
// nothing when empty. The budget ledger is checked before every model call and
// every call's usage is spent + reported.
func (o *run) draftPlan(ctx context.Context, model, diagnosis, design, feedback string) (plan, error) {
	d := o.d
	cfg := d.Cfg

	var existingTitles []string
	for _, sub := range o.subtasks {
		existingTitles = append(existingTitles, sub.Title)
	}

	resume := resumeBlock(existingTitles)
	diagBlock := diagnosisBlock(diagnosis)
	dsnBlock := designBlock(design)
	fbBlock := feedbackBlock(feedback)
	snapshot := o.repoSnapshotBlock()

	var (
		p       plan
		lastErr error
	)

	for attempt := range 2 {
		if err := o.ledger.Check(); err != nil {
			return plan{}, err
		}

		repair := ""
		if attempt > 0 {
			repair = repairBlock(lastErr.Error())
		}

		task := fmt.Sprintf(planPrompt, o.grounding, snapshot, cfg.Workspace, o.tc.Title, o.tc.Description,
			diagBlock, dsnBlock, resume, fbBlock, repair)

		res, err := o.runModelPlan(ctx, d.ReadTools, task, model, o.taskImages, attempt > 0)

		o.ledger.Spend(res.TotalCostUSD)

		used := res.ModelUsed
		if used == "" {
			used = model
		}

		if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, used,
			res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
			slog.Warn("plan: report usage failed", "card_id", cfg.CardID, "error", reportErr)
		}

		if err != nil {
			return plan{}, fmt.Errorf("planner run: %w", err)
		}

		p, lastErr = parsePlan(res.Output)
		if lastErr == nil {
			return p, nil
		}

		slog.Warn("plan: parse failed", "card_id", cfg.CardID, "attempt", attempt, "error", lastErr)
	}

	return plan{}, fmt.Errorf("plan parse failed after repair: %w", lastErr)
}

// coopAdjustTailEntries bounds the transcript tail replayed when a HITL
// adjust re-opens a discussion.
const coopAdjustTailEntries = 12

// coopDraftPlan convenes a plan discussion and parses the synthesis into a
// plan, with ONE moderator repair on a parse failure (mirroring draftPlan's
// single repair turn). prior, when non-nil, re-opens the previous discussion
// for one non-blind feedback round (HITL adjust): the briefing is the prior
// transcript tail plus the human's feedback as a human-authored entry.
// ok=false on any failure — the caller falls back to the solo draftPlan path.
func (o *run) coopDraftPlan(ctx context.Context, diagnosis, design, feedback string, prior *coop.Outcome) (plan, *coop.Outcome, bool) {
	seats := min(o.d.Cfg.Coop.Participants, len(planLenses))

	t := coop.Topic{
		Kind:     "plan",
		Lenses:   planLenses[:seats],
		Rounds:   o.d.Cfg.Coop.Rounds,
		Blind:    true,
		Briefing: o.coopPlanBriefing(diagnosis, design),
		SynthesisPrompt: fmt.Sprintf(planSynthesisPrompt,
			o.grounding, o.d.Cfg.Workspace, o.tc.Title, o.tc.Description),
	}

	if prior != nil {
		t.Rounds = 1
		t.Blind = false
		t.Briefing = coopAdjustBriefing(*prior, feedback)
	}

	out, ok := o.coopDiscuss(ctx, t)
	if !ok {
		return plan{}, nil, false
	}

	p, perr := parsePlan(out.Synthesis)
	if perr != nil {
		repaired, rerr := o.coopResynthesize(ctx, t, out, perr.Error())
		if rerr != nil {
			slog.Warn("coop plan: repair synthesis failed; solo fallback",
				"card_id", o.d.Cfg.CardID, "error", rerr)

			return plan{}, nil, false
		}

		p, perr = parsePlan(repaired)
		if perr != nil {
			slog.Warn("coop plan: parse failed after repair; solo fallback",
				"card_id", o.d.Cfg.CardID, "error", perr)

			return plan{}, nil, false
		}

		out.Synthesis = repaired
	}

	return p, &out, true
}

// coopPlanBriefing assembles the plan-discussion briefing from the SAME
// content blocks draftPlan feeds the solo planner: grounding, workspace,
// title, description, diagnosis (bug-like cards), design (creative HITL
// cards), and the resume-subtasks block.
func (o *run) coopPlanBriefing(diagnosis, design string) string {
	var existingTitles []string
	for _, sub := range o.subtasks {
		existingTitles = append(existingTitles, sub.Title)
	}

	resume := resumeBlock(existingTitles)
	diagBlock := diagnosisBlock(diagnosis)
	dsnBlock := designBlock(design)

	return fmt.Sprintf(planBriefing, o.grounding, o.repoSnapshotBlock(), o.d.Cfg.Workspace, o.tc.Title, o.tc.Description,
		diagBlock, dsnBlock, resume)
}

// coopAdjustBriefing re-opens a discussion after a HITL adjust: the tail of
// the prior transcript restores shared context and the human's feedback
// arrives as a human-authored line per the wire conventions.
func coopAdjustBriefing(prior coop.Outcome, feedback string) string {
	entries := prior.Transcript
	if len(entries) > coopAdjustTailEntries {
		entries = entries[len(entries)-coopAdjustTailEntries:]
	}

	return "The group previously discussed this plan. Recent transcript:\n\n" +
		formatDiscussionEntries(entries) +
		"\n\n[round 0] human: " + feedback +
		"\n\nRevise the plan to address the human's feedback."
}

// coopResynthesize runs ONE moderator repair call after a synthesis parse
// failure: the topic's synthesis instruction, the rendered transcript, and
// the repair block naming the parse error. Shared by the plan and review
// co-op paths (the moderator equivalent of draftPlan's repair turn).
func (o *run) coopResynthesize(ctx context.Context, t coop.Topic, out coop.Outcome, parseErr string) (string, error) {
	prompt := t.SynthesisPrompt +
		"\n\nDISCUSSION TRANSCRIPT\n" + formatDiscussionEntries(out.Transcript) +
		"\n" + repairBlock(parseErr)

	moderate := o.coopModeratorRunner(&seatDebugSink{w: o.seatDebug})

	text, _, err := moderate(ctx, prompt)

	return text, err
}

// recordDiscussion upserts the ## Discussion section on the parent card AFTER
// the discussion's output was accepted: seats and models, guests, round
// count, consensus or carried dissent, and cost. Best-effort, like every
// card-body record.
func (o *run) recordDiscussion(ctx context.Context, out *coop.Outcome) {
	var b strings.Builder

	b.WriteString("## Discussion\n\nSeats:\n")

	for _, s := range o.coopSeats {
		fmt.Fprintf(&b, "- %s (%s): %s\n", s.Name, s.Lens, s.Model)
	}

	for _, g := range o.d.Cfg.Coop.Guests {
		fmt.Fprintf(&b, "- guest-%s\n", g.Name)
	}

	rounds := 0

	for _, e := range out.Transcript {
		if e.Round > rounds {
			rounds = e.Round
		}
	}

	fmt.Fprintf(&b, "\nCritique rounds: %d\n", rounds)

	if out.Consensus {
		b.WriteString("Outcome: consensus\n")
	} else {
		b.WriteString("Outcome: unresolved dissent — carried into the output as risk notes\n")
	}

	fmt.Fprintf(&b, "Cost: $%.4f", out.CostUSD)

	o.recordSection(ctx, "Discussion", b.String())
	_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory record
		fmt.Sprintf("co-op discussion recorded (%d seats, %d rounds, consensus=%t)",
			len(o.coopSeats), rounds, out.Consensus))
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

	model := resolveDecisionModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel)

	_ = d.Ops.AddLog(ctx, cfg.CardID, "orchestrator model: "+model) //nolint:errcheck // advisory selection record

	// Creative HITL cards get a design dialogue before planning (create-plan
	// Phase 0 Branch C). Skipped in autonomous, for non-creative cards, and when
	// a design already exists. Branch C and the bug Branch B are mutually
	// exclusive (isCreative excludes bug-like cards).
	design := ""

	if cfg.Interactive && isCreative(o.tc) && !hasDesignSection(o.body) {
		d, err := o.runBrainstorm(ctx, model)
		if err != nil {
			return err
		}

		design = d
	}

	// Bug-like cards get a read-only root-cause investigation before planning
	// (mirrors CM's create-plan workflow skill, Phase 0 Branch B). The diagnosis
	// grounds the decomposition. Best-effort: a failed diagnose must not block planning.
	diagnosis := ""

	if isBugLike(o.tc) {
		_ = d.Ops.AddLog(ctx, cfg.CardID, "running root-cause investigation (bug-like card)") //nolint:errcheck

		diag, derr := o.runDiagnose(ctx, model)
		switch {
		case derr == nil:
			diagnosis = diag
			if strings.TrimSpace(diag) != "" {
				// Record the root-cause investigation on the parent card body,
				// like CM's systematic-debugging workflow skill writes ## Diagnosis.
				o.recordSection(ctx, "Diagnosis", sectionFrom("Diagnosis", diag))
			}
		case isBudgetError(derr):
			return derr // park: the FSM's execute() maps this to the budget log
		default:
			slog.Warn("plan: diagnose step failed; planning without a diagnosis",
				"card_id", cfg.CardID, "error", derr)
		}
	}

	coopPlan := cfg.Coop.enabled() && cfg.Coop.Plan

	// Autonomous: draft once and create the subtasks. With co-op on, the
	// draft comes from a panel discussion; any discussion failure degrades to
	// the solo draftPlan path, byte-identical to before.
	if !cfg.Interactive {
		if coopPlan {
			if p, out, ok := o.coopDraftPlan(ctx, diagnosis, "", "", nil); ok {
				if err := o.createSubtasks(ctx, p); err != nil {
					return err
				}

				o.recordDiscussion(ctx, out)

				return nil
			}
		}

		p, err := o.draftPlan(ctx, model, diagnosis, "", "")
		if err != nil {
			return err
		}

		return o.createSubtasks(ctx, p)
	}

	// HITL: draft -> present -> gate; on adjust, re-draft with the feedback.
	// Subtasks are created only after approval, so an adjust never orphans
	// cards. With co-op on, drafts come from discussions and an adjust
	// re-opens the discussion for one feedback round; once a discussion
	// fails, the rest of the phase stays on the solo path.
	feedback := ""

	var lastOut *coop.Outcome

	coopSolo := false

	for range maxPlanDrafts {
		var p plan

		drafted := false

		if coopPlan && !coopSolo {
			var (
				out *coop.Outcome
				ok  bool
			)

			p, out, ok = o.coopDraftPlan(ctx, diagnosis, design, feedback, lastOut)
			if ok {
				drafted = true
				lastOut = out
			} else {
				coopSolo = true
			}
		}

		if !drafted {
			var err error

			p, err = o.draftPlan(ctx, model, diagnosis, design, feedback)
			if err != nil {
				return err
			}

			lastOut = nil
		}

		o.recordSection(ctx, "Plan", sectionFrom("Plan", formatPlannedPlan(p)))

		outcome, fb, gerr := o.gate(ctx, gatePlanApproval, model, presentPlan(p))
		if gerr != nil {
			return gerr
		}

		if outcome == gateApprove {
			if err := o.createSubtasks(ctx, p); err != nil {
				return err
			}

			if lastOut != nil {
				o.recordDiscussion(ctx, lastOut)
			}

			return nil
		}

		feedback = fb
	}

	return fmt.Errorf("plan approval did not converge after %d drafts", maxPlanDrafts)
}

// presentPlan is the chat message for the plan-approval gate: the planned
// decomposition plus the ask. The full plan is also on the card body.
func presentPlan(p plan) string {
	return "I've drafted the following plan:\n\n" + formatPlannedPlan(p) +
		"\n\nApprove to start execution, or tell me what you'd like to adjust."
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

		id, err := d.Ops.CreateCard(ctx, cfg.Project, cfg.CardID, st.Title, withTierMarker(st.Description, st.Tier), depIDs)
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
	// CM's create-plan workflow skill writes ## Plan).
	o.recordSection(ctx, "Plan", sectionFrom("Plan", formatPlan(o.subtasks)))

	return nil
}
