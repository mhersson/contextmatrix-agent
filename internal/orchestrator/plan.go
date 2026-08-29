package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/tools"
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

// maxFollowupCards caps a plan-time deliverable split. A split larger than
// this means the card is mis-scoped for automatic handling: the run parks
// with the proposal instead of mutating the board at scale.
//
//nolint:unused // consumed by the plan step that enforces the followup-split cap
const maxFollowupCards = 4

// planFollowup is one extra deliverable the planner split out of the card:
// created by the orchestrator as a new TOP-LEVEL card (not a subtask), with a
// self-contained description. DependsOn indexes earlier followup entries;
// DependsOnOriginal additionally chains on the card being planned.
type planFollowup struct {
	Title             string `json:"title"`
	Description       string `json:"description"`
	DependsOn         []int  `json:"depends_on"`
	DependsOnOriginal bool   `json:"depends_on_original"`
}

// planUnreachable is one acceptance criterion the planner judged unreachable
// from inside the container: it needs an input that does not exist in the
// repo, or a write target outside it. Recorded on the card for review to
// verify and exclude - never silently dropped.
type planUnreachable struct {
	Criterion string `json:"criterion"`
	Reason    string `json:"reason"`
}

// plan is the planner's structured final output: the overall card tier, the
// ordered subtask list, an optional list of extra deliverables split out as
// followup_cards, and an optional list of acceptance criteria judged
// unreachable_criteria. depends_on indices (subtasks and followup cards each
// index their own array) reference earlier entries only.
type plan struct {
	CardTier      string            `json:"card_tier"`
	Subtasks      []planSubtask     `json:"subtasks"`
	FollowupCards []planFollowup    `json:"followup_cards"`
	Unreachable   []planUnreachable `json:"unreachable_criteria"`
}

// subtaskRef is a created subtask carried on the run struct for the execute
// phase: the real card ID, its title, body, sizing, current state, and the real
// card IDs it depends on. State drives resume skipping in the execute phase
// ("done" subtasks are not re-run); plan-created subtasks start "todo". Body
// carries the planner's description (file lists, acceptance criteria) into the
// coder prompt; resume-loaded refs restore it from the card body.
type subtaskRef struct {
	ID     string
	Title  string
	Body   string
	Sizing sizing
	// PlannerBar is the planner's own word for this subtask, restored from the
	// marker's write-once seed key. Kept separate from Sizing.Bar because an
	// escalation overwrites the bar, and the estimate a later analysis is
	// testing must stay recoverable after the correction that replaced it.
	PlannerBar   string
	State        string
	DependsOnIDs []string
}

// parsePlan extracts a JSON object from s (tolerating prose / code-fence wrap)
// and validates it: 1..maxSubtasks subtasks, valid card and subtask tiers,
// depends_on indices that reference only earlier subtasks (no self/forward
// refs), non-empty followup_cards titles/descriptions with depends_on
// indices referencing only earlier followup entries, and non-empty
// unreachable_criteria criterion text. followup_cards and
// unreachable_criteria are both optional and unvalidated against
// maxFollowupCards - the cap is enforced where the plan is consumed, not here.
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

	for i, fc := range p.FollowupCards {
		if strings.TrimSpace(fc.Title) == "" {
			return plan{}, fmt.Errorf("followup card %d has an empty title", i)
		}

		if strings.TrimSpace(fc.Description) == "" {
			return plan{}, fmt.Errorf("followup card %d has an empty description", i)
		}

		for _, dep := range fc.DependsOn {
			if dep < 0 || dep >= i {
				return plan{}, fmt.Errorf("followup card %d depends_on index %d must reference an earlier followup", i, dep)
			}
		}
	}

	for i, u := range p.Unreachable {
		if strings.TrimSpace(u.Criterion) == "" {
			return plan{}, fmt.Errorf("unreachable criterion %d is empty", i)
		}
	}

	return p, nil
}

// testSplitVerbRe matches the verbs whose object is commonly a whole
// deliverable ("add X", "write X", "pin X") rather than an edit to existing
// code - the shape a test-only subtask title takes. testSplitTestsRe matches
// the tests-token itself. Both are used only in combination by
// titleLooksTestOnly, never alone - a matching verb with no tests token (or
// vice versa) is not a violation.
var (
	testSplitVerbRe  = regexp.MustCompile(`(?i)\b(add|write|extend|create|pin)\b`)
	testSplitTestsRe = regexp.MustCompile(`(?i)\btests?\b`)
)

// filePathSuffixRe matches a path's final segment when it looks like a
// filename with an extension: a word-char run followed by one or more
// dot-separated word-char runs, so multi-dot names (auth.test.ts) match
// alongside plain ones (auth.ts) - the shape that lets filePathTokens tell a
// real path apart from prose that merely contains a slash.
var filePathSuffixRe = regexp.MustCompile(`^[\w-]+(?:\.[\w-]+)+$`)

// testFileSuffixRe matches a Go-style "_test." suffix immediately before the
// extension. testFileInfixRe matches a JS/TS-style ".test." or ".spec."
// infix. Both are used by isTestFilePath alongside a bare "test"/"tests"
// path segment check.
var (
	testFileSuffixRe = regexp.MustCompile(`_test\.\w+$`)
	testFileInfixRe  = regexp.MustCompile(`\.(?:test|spec)\.\w+$`)
)

// titleLooksTestOnly reports whether title has the shape of a subtask whose
// deliverable is testing another subtask's code rather than shipping it: a
// listed verb (add, write, extend, create, pin) followed LATER in the title
// by a tests token. Order matters - a title that mentions tests before the
// verb (e.g. discussing test infrastructure a later clause extends) does not
// match. This is the title-only signal isTestOnlySubtask falls back to when
// the subtask's description carries no file evidence to ground the call.
func titleLooksTestOnly(title string) bool {
	verbLoc := testSplitVerbRe.FindStringIndex(title)
	if verbLoc == nil {
		return false
	}

	testsLoc := testSplitTestsRe.FindStringIndex(title)
	if testsLoc == nil {
		return false
	}

	return testsLoc[0] > verbLoc[0]
}

// filesLabelRe matches a description line that opens the planner-mandated
// file list: an optional list bullet, then a "Files:" label. labelLineRe
// matches a prose "Label:" header (letters, then word chars, spaces, hyphens,
// or parens, then a colon - "Non-goals:", "Acceptance criteria (v2):"; a path
// line never matches, its slashes and dots do not fit), which is where a
// Files: section ends.
var (
	filesLabelRe = regexp.MustCompile(`(?i)^\s*(?:[-*]\s*)?files?\s*:`)
	labelLineRe  = regexp.MustCompile(`^[A-Za-z][\w ()-]*:`)
)

// filesSection extracts the planner-mandated "Files:" portion of a subtask
// description: each Files: label line plus its continuation lines, ending at
// a blank line or the next "Label:" header (e.g. "Acceptance criteria:").
// Empty when the description carries no Files: label, which tells
// isTestOnlySubtask to fall back to scanning the whole description.
func filesSection(description string) string {
	var b strings.Builder

	inSection := false

	for line := range strings.Lines(description) {
		trimmed := strings.TrimSpace(line)

		switch {
		case filesLabelRe.MatchString(line):
			inSection = true
		case !inSection:
			continue
		case trimmed == "" || labelLineRe.MatchString(trimmed):
			inSection = false

			continue
		}

		b.WriteString(line)
	}

	return b.String()
}

// filePathTokens extracts path-like tokens from text: each whitespace-
// separated token, punctuation-trimmed, that contains a '/' and whose final
// segment looks like a filename with an extension.
func filePathTokens(text string) []string {
	var paths []string

	for tok := range strings.FieldsSeq(text) {
		tok = strings.Trim(tok, ",.;:()[]{}\"'`")

		idx := strings.LastIndex(tok, "/")
		if idx < 0 {
			continue
		}

		if filePathSuffixRe.MatchString(tok[idx+1:]) {
			paths = append(paths, tok)
		}
	}

	return paths
}

// isTestFilePath reports whether path matches a common test-file naming
// convention: a "_test." suffix before the extension (Go-style), a
// ".test." or ".spec." infix (JS/TS-style), or a whole "test"/"tests" path
// segment (so "latest/foo.go" does not match on a substring).
func isTestFilePath(path string) bool {
	if testFileSuffixRe.MatchString(path) || testFileInfixRe.MatchString(path) {
		return true
	}

	for seg := range strings.SplitSeq(path, "/") {
		if seg == "test" || seg == "tests" {
			return true
		}
	}

	return false
}

// isTestOnlySubtask reports whether the subtask named title, with description
// desc, is one whose deliverable is testing another subtask's code rather
// than shipping it. File evidence is authoritative when present - the
// planner prompt mandates a "Files:" line, and the evidence is read from that
// section alone when it names any files (prose elsewhere in the description
// routinely NAMES the code under test, e.g. "write tests for plan.go", and
// must not clear the subtask), falling back to the whole description when the
// section yields no paths. ANY extracted path that is not a test file
// clears the subtask (a title matching the verb+tests shape is not itself a
// violation - "Create the login endpoint and write its handler tests" both
// implements and tests in one subtask), while paths found and ALL of them
// test files confirm it, regardless of title. Only when no path evidence
// exists at all does the title heuristic (titleLooksTestOnly) decide alone -
// a residual gap, expected to be rare since the prompt mandates file lists,
// where a title in the verb+tests shape can still false-positive with
// nothing to ground it.
func isTestOnlySubtask(title, desc string) bool {
	// A Files: section that yields no paths (label omitted, or a markdown
	// shape the section walk does not capture, e.g. a blank line between the
	// label and its bullets) falls back to whole-description scanning - the
	// section is only trusted over the description when it actually names
	// files.
	paths := filePathTokens(filesSection(desc))
	if len(paths) == 0 {
		paths = filePathTokens(desc)
	}

	if len(paths) == 0 {
		return titleLooksTestOnly(title)
	}

	for _, p := range paths {
		if !isTestFilePath(p) {
			return false
		}
	}

	return true
}

// testSplitViolation returns the title of the first subtask in p whose
// title/description matches isTestOnlySubtask AND which depends on an
// earlier subtask - the forbidden split the planner prompt already forbids
// ("the subtask that writes the code writes and runs its own tests"): a
// dependent subtask whose own deliverable is testing its dependency's code.
// A matching subtask with no dependency is not this violation - it is
// presumably a legitimate, self-contained subtask that happens to mention
// tests. ok is false when p has no such subtask.
func testSplitViolation(p plan) (title string, ok bool) {
	for _, st := range p.Subtasks {
		if len(st.DependsOn) > 0 && isTestOnlySubtask(st.Title, st.Description) {
			return st.Title, true
		}
	}

	return "", false
}

// extractJSON returns the JSON object the model intended as its answer. A
// whole-output bare object is returned as-is - fence markers inside its string
// values must never trigger fence-stripping, which is not string-aware and
// would mangle the payload. Otherwise it prefers a fenced ```json block
// (models wrap the verdict in one and surround it with prose that contains
// stray braces), and finally returns the LAST balanced top-level object -
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
// A best-effort card-log failure is swallowed - the warning is advisory.
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
			fmt.Sprintf("orchestrator model pin %q not in catalog - using %q", pinned, target))
	}

	if payload != "" {
		return payload
	}

	return fallback
}

// decisionTier is the fixed bar every orchestrator decision phase selects on.
// Decision quality does not scale with task complexity, so even a trivial card
// gets a calibrated judge.
const decisionTier = registry.TierComplex

// resolveDecisionModel resolves the model an orchestrator DECISION phase runs on
// (plan decomposition, review synthesis). These phases are reasoning- and
// calibration-sensitive - a weak model emits malformed plans and mis-calibrated
// verdicts - so, unlike the low-stakes docs phase, they are floored to a capable
// judgment model. A catalog-resolvable ModelOrchestrator pin still wins (operator
// override; an unresolvable pin already warned inside resolveOrchestratorModel).
// Otherwise the floor is the same best-value selection the authoritative review
// panel uses - RoleReviewer @ TierComplex - the measured proxy for orchestrator-
// level judgment (the live catalog measures only coder/reviewer; reviewer is the
// closer fit for both decomposing and judging). Fixed at TierComplex for EVERY
// call: decision quality does not scale with task complexity, so even a trivial
// card gets a calibrated judge. The floor only ever RAISES base, never lowers
// it: a selection that did not clear the complex bar has no claim over an
// operator-configured orchestrator model, so base is kept. Degrades to the base
// resolution when no registry is present.
func resolveDecisionModel(
	ctx context.Context,
	reg *registry.Registry,
	emit *events.Emitter,
	ops Ops,
	cardID, pinned, payload, fallback string,
	exclude map[string]bool,
	phase string,
) string {
	base := resolveOrchestratorModel(ctx, reg, emit, ops, cardID, pinned, payload, fallback)

	// Without a registry there is no ladder and no tier, so there is no
	// selection to record.
	if reg == nil {
		return base
	}

	// A resolvable operator pin is authoritative - never floor over it.
	if resolvePin(reg, pinned) {
		emitModelSelection(emit, phase, "", offLadderPick(reg, base, registry.RoleReviewer, decisionTier, registry.SourcePinned))

		return base
	}

	p := reg.SelectByComplexity(registry.SelectInput{
		Role:    registry.RoleReviewer,
		Tier:    decisionTier,
		Exclude: exclude,
	})

	// base is already an operator-configured orchestrator model for this phase,
	// so the selection layered on top is an upgrade attempt. A pick that did not
	// meet the bar it was asked for would replace a stronger base with a weaker
	// catalogued model: a floor that can land below the thing it is flooring is
	// not a floor.
	//
	// The transcript names base, not p: p was proposed and discarded, and an
	// event naming a model that never ran is worse than none.
	if !p.AtBar() {
		emitModelSelection(emit, phase, "", offLadderPick(reg, base, registry.RoleReviewer, decisionTier, registry.SourceDefault))

		return base
	}

	emitModelSelection(emit, phase, "", p)

	return p.Model
}

// isBudgetError reports whether err is (or wraps) the budget-ceiling sentinel.
func isBudgetError(err error) bool {
	var be *BudgetExceededError

	return errors.As(err, &be)
}

// isParkError reports whether err is one of the sentinels execute stops the run
// on rather than advancing to the next phase (orchestrator.go's park arms).
// A caller that swallows one of these returns nil, so the run walks into the
// next phase carrying whatever half-done state the parked phase left behind -
// and the worker never gets to push the WIP it exists to preserve.
func isParkError(err error) bool {
	var (
		be  *BudgetExceededError
		cle *ContextLimitError
		mte *MaxTurnsError
		tme *ToolchainMissingError
		nme *NoModelError
		vpe *VerifyParkedError
	)

	return errors.As(err, &be) || errors.As(err, &cle) || errors.As(err, &mte) ||
		errors.As(err, &tme) || errors.As(err, &nme) || errors.As(err, &vpe)
}

// runDiagnose runs one read-only investigation pass on the orchestrator model
// for a bug-like card and returns a "## Diagnosis" text blob to ground the
// plan. Budget-checked and usage-reported like every model-bearing step. The
// caller treats a returned error as best-effort: planning proceeds without a
// diagnosis rather than failing. model resolves lazily - the diagnose pass is
// the first certain run of the plan decision model on non-creative bug cards.
func (o *run) runDiagnose(ctx context.Context, model func(context.Context) string) (string, error) {
	d := o.d
	cfg := d.Cfg

	if err := o.ledger.Check(); err != nil {
		return "", err
	}

	task := fmt.Sprintf(diagnosePrompt, o.grounding, cfg.Workspace, readRootsBlock(d.ReadRoots),
		o.tc.Title, o.taskDescription)

	slug := model(ctx)

	res, dur, err := o.runModelDiagnose(ctx, d.ReadTools, task, slug, o.taskImages)

	o.spendAndReport(ctx, o.ledger, cfg.CardID, "plan: report diagnose usage failed", res, slug, "main", dur)

	if err != nil {
		return "", fmt.Errorf("diagnose run: %w", err)
	}

	return strings.TrimSpace(res.Output), nil
}

// draftPlan runs the read-only planner (initial attempt + at most one repair
// turn) and returns the parsed plan, after one test-split validation pass
// (see reviseTestSplit). diagnosis grounds bug-like cards; design carries the
// brainstormed agreed design for creative HITL cards; feedback carries a
// HITL reviewer's requested changes on a re-draft; all collapse to nothing
// when empty. The budget ledger is checked before every model call and every
// call's usage is spent + reported. model resolves lazily: the solo draft is
// the first certain run of the plan decision model when no brainstorm or
// diagnose pass ran ahead of it.
func (o *run) draftPlan(ctx context.Context, model func(context.Context) string, diagnosis, design, feedback string) (plan, error) {
	d := o.d
	cfg := d.Cfg

	var existingTitles []string

	for _, sub := range o.subtasks {
		if sub.State != "not_planned" {
			existingTitles = append(existingTitles, sub.Title)
		}
	}

	resume := resumeBlock(existingTitles)
	diagBlock := diagnosisBlock(diagnosis)
	dsnBlock := designBlock(design)
	fbBlock := feedbackBlock(feedback)
	snapshot := o.repoSnapshotBlock()

	// Resolved once so both the first attempt and its repair share one findings
	// list: the repair must re-emit a plan from analysis it already has.
	reg := d.ReadTools
	if d.PlanTools != nil {
		reg = d.PlanTools()
	}

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

		task := fmt.Sprintf(planPrompt, o.grounding, snapshot, cfg.Workspace, readRootsBlock(o.d.ReadRoots),
			o.tc.Title, o.plannerDescription(), diagBlock, dsnBlock, resume, fbBlock, repair)

		slug := model(ctx)

		res, dur, err := o.runModelPlan(ctx, reg, task, slug, o.taskImages, attempt > 0)

		o.spendAndReport(ctx, o.ledger, cfg.CardID, "plan: report usage failed", res, slug, "main", dur)

		if err != nil {
			return plan{}, fmt.Errorf("planner run: %w", err)
		}

		p, lastErr = parsePlan(res.Output)
		if lastErr == nil {
			return o.reviseTestSplit(ctx, reg, slug, snapshot, diagBlock, dsnBlock, resume, fbBlock, p)
		}

		slog.Warn("plan: parse failed", "card_id", cfg.CardID, "attempt", attempt, "error", lastErr)
	}

	return plan{}, fmt.Errorf("plan parse failed after repair: %w", lastErr)
}

// reviseTestSplit checks a freshly parsed plan p for the forbidden
// tests-in-a-dependent-subtask split (testSplitViolation) and, on a
// violation, re-prompts the planner ONCE with feedback naming the offending
// subtask and asking for the full corrected plan back. This is a code-side
// backstop for a prompt rule real planners have violated in practice - a
// heuristic, so it never fails the run: a budget check that would block the
// revision call, a failed revision call, a failed revision parse, or a
// second violation all warn via the card log and fall back to the ORIGINAL
// plan p, the same as every other failure here. p is already paid for -
// discarding it on the budget check alone would waste it for no reason,
// since execute's own ledger check at its first subtask parks the run anyway
// if the budget is genuinely exhausted.
func (o *run) reviseTestSplit(
	ctx context.Context, reg *tools.Registry, model, snapshot, diagBlock, dsnBlock, resume, fbBlock string, p plan,
) (plan, error) {
	d := o.d
	cfg := d.Cfg

	title, violated := testSplitViolation(p)
	if !violated {
		return p, nil
	}

	d.logCard(ctx, "plan validation: subtask %q splits its tests into a dependent subtask - requesting a revision", title)

	if err := o.ledger.Check(); err != nil {
		slog.Warn("plan: test-split revision budget check failed; proceeding with the original plan",
			"card_id", cfg.CardID, "error", err)
		d.logCard(ctx, "plan validation: revision budget check failed - proceeding with the original plan")

		return p, nil
	}

	// The revision run is stateless - without the previous plan in the prompt,
	// "fold its work into the subtask it depends on" would name structure the
	// model cannot see. MarshalIndent on this plain struct cannot fail; the
	// guard keeps the revision usable (title-only) if that ever changes.
	prev, merr := json.MarshalIndent(p, "", "  ")
	if merr != nil {
		prev = []byte("(previous plan unavailable)")
	}

	task := fmt.Sprintf(planPrompt, o.grounding, snapshot, cfg.Workspace, readRootsBlock(o.d.ReadRoots),
		o.tc.Title, o.plannerDescription(), diagBlock, dsnBlock, resume, fbBlock,
		testSplitRevisionBlock(title, prev))

	res, dur, err := o.runModelPlan(ctx, reg, task, model, o.taskImages, true)

	o.spendAndReport(ctx, o.ledger, cfg.CardID, "plan: report usage failed", res, model, "main", dur)

	if err != nil {
		slog.Warn("plan: test-split revision run failed; proceeding with the original plan", "card_id", cfg.CardID, "error", err)
		d.logCard(ctx, "plan validation: revision request failed - proceeding with the original plan")

		return p, nil
	}

	revised, perr := parsePlan(res.Output)
	if perr != nil {
		slog.Warn("plan: test-split revision parse failed; proceeding with the original plan", "card_id", cfg.CardID, "error", perr)
		d.logCard(ctx, "plan validation: revised plan could not be parsed - proceeding with the original plan")

		return p, nil
	}

	if _, stillViolated := testSplitViolation(revised); stillViolated {
		slog.Warn("plan: test-split violation persists after one revision attempt; proceeding with the original plan",
			"card_id", cfg.CardID, "subtask_title", title)
		d.logCard(ctx, "plan validation: subtask %q still splits its tests after one revision attempt - "+
			"proceeding with the original plan", title)

		return p, nil
	}

	return revised, nil
}

// reviseMobTestSplit is reviseTestSplit's counterpart for mob-drafted plans:
// on a testSplitViolation it re-opens the discussion for ONE feedback round
// (the same non-blind adjust mechanism a HITL reviewer uses, so the panel
// sees its own plan and the finding) and re-checks. Same never-fail
// contract: a failed round or a persisting violation warns and keeps the
// original plan and outcome - both already paid for.
func (o *run) reviseMobTestSplit(ctx context.Context, diagnosis, design string, p plan, out *mob.Outcome) (plan, *mob.Outcome) {
	title, violated := testSplitViolation(p)
	if !violated {
		return p, out
	}

	o.d.logCard(ctx, "plan validation: subtask %q splits its tests into a dependent subtask - requesting a revision", title)

	revised, rout, ok := o.mobDraftPlan(ctx, diagnosis, design, testSplitMobFeedback(title), out)
	if !ok {
		slog.Warn("plan: mob test-split revision round failed; proceeding with the original plan",
			"card_id", o.d.Cfg.CardID)
		o.d.logCard(ctx, "plan validation: revision request failed - proceeding with the original plan")

		return p, out
	}

	if _, still := testSplitViolation(revised); still {
		slog.Warn("plan: test-split violation persists after one revision attempt; proceeding with the original plan",
			"card_id", o.d.Cfg.CardID, "subtask_title", title)
		o.d.logCard(ctx, "plan validation: subtask %q still splits its tests after one revision attempt - "+
			"proceeding with the original plan", title)

		return p, out
	}

	return revised, rout
}

// mobAdjustTailEntries bounds the transcript tail replayed when a HITL
// adjust re-opens a discussion.
const mobAdjustTailEntries = 12

// mobDraftPlan convenes a plan discussion and parses the synthesis into a
// plan, with ONE moderator repair on a parse failure (mirroring draftPlan's
// single repair turn). prior, when non-nil, re-opens the previous discussion
// for one non-blind feedback round (HITL adjust): the briefing is the prior
// transcript tail plus the human's feedback as a human-authored entry.
// ok=false on any failure - the caller falls back to the solo draftPlan path.
func (o *run) mobDraftPlan(ctx context.Context, diagnosis, design, feedback string, prior *mob.Outcome) (plan, *mob.Outcome, bool) {
	seats := min(o.d.Cfg.Mob.Participants, len(planLenses))

	t := mob.Topic{
		Kind:     "plan",
		Lenses:   planLenses[:seats],
		Rounds:   o.d.Cfg.Mob.Rounds,
		Blind:    true,
		Briefing: o.mobPlanBriefing(diagnosis, design),
		SynthesisPrompt: fmt.Sprintf(planSynthesisPrompt,
			o.grounding, o.d.Cfg.Workspace, o.tc.Title, o.plannerDescription()),
	}

	if prior != nil {
		t.Rounds = 1
		t.Blind = false
		t.Briefing = mobAdjustBriefing(*prior, feedback)
	}

	out, ok := o.mobDiscuss(ctx, t)
	if !ok {
		return plan{}, nil, false
	}

	p, perr := parsePlan(out.Synthesis)
	if perr != nil {
		repaired, rerr := o.mobResynthesize(ctx, t, out, perr.Error())
		if rerr != nil {
			slog.Warn("mob plan: repair synthesis failed; solo fallback",
				"card_id", o.d.Cfg.CardID, "error", rerr)

			return plan{}, nil, false
		}

		p, perr = parsePlan(repaired)
		if perr != nil {
			slog.Warn("mob plan: parse failed after repair; solo fallback",
				"card_id", o.d.Cfg.CardID, "error", perr)

			return plan{}, nil, false
		}

		out.Synthesis = repaired
	}

	return p, &out, true
}

// mobPlanBriefing assembles the plan-discussion briefing: grounding, the repo
// snapshot, workspace, the read-only roots, title, description, diagnosis
// (bug-like cards), design (creative HITL cards), and the resume-subtasks
// block. The seats discuss rather than emit, so the briefing carries none of
// the solo planner's decomposition rules or JSON contract - the moderator's
// synthesis prompt owns those.
func (o *run) mobPlanBriefing(diagnosis, design string) string {
	var existingTitles []string

	for _, sub := range o.subtasks {
		if sub.State != "not_planned" {
			existingTitles = append(existingTitles, sub.Title)
		}
	}

	resume := resumeBlock(existingTitles)
	diagBlock := diagnosisBlock(diagnosis)
	dsnBlock := designBlock(design)

	return fmt.Sprintf(planBriefing, o.grounding, o.repoSnapshotBlock(), o.d.Cfg.Workspace,
		readRootsBlock(o.d.ReadRoots), o.tc.Title, o.plannerDescription(),
		diagBlock, dsnBlock, resume)
}

// mobAdjustBriefing re-opens a discussion after a HITL adjust: the tail of
// the prior transcript restores shared context and the human's feedback
// arrives as a human-authored line per the wire conventions.
func mobAdjustBriefing(prior mob.Outcome, feedback string) string {
	entries := prior.Transcript
	if len(entries) > mobAdjustTailEntries {
		entries = entries[len(entries)-mobAdjustTailEntries:]
	}

	return "The group previously discussed this plan. Recent transcript:\n\n" +
		formatDiscussionEntries(entries) +
		"\n\n[round 0] human: " + feedback +
		"\n\nRevise the plan to address the human's feedback."
}

// mobResynthesize runs ONE moderator repair call after a synthesis parse
// failure: the topic's synthesis instruction, the rendered transcript, and
// the repair block naming the parse error. Shared by the plan and review
// mob session paths (the moderator equivalent of draftPlan's repair turn).
func (o *run) mobResynthesize(ctx context.Context, t mob.Topic, out mob.Outcome, parseErr string) (string, error) {
	prompt := t.SynthesisPrompt +
		"\n\nDISCUSSION TRANSCRIPT\n" + formatDiscussionEntries(out.Transcript) +
		"\n" + repairBlock(parseErr)

	moderate := o.mobModeratorRunner(&seatDebugSink{w: o.seatDebug}, mobModeratorStep(t.Kind))

	text, _, _, err := moderate(ctx, prompt)

	return text, err
}

// recordDiscussion upserts the ## Discussion section on the parent card AFTER
// the discussion's output was accepted: seats and models, guests, round
// count, consensus or carried dissent, and cost. Best-effort, like every
// card-body record.
func (o *run) recordDiscussion(ctx context.Context, out *mob.Outcome) {
	var b strings.Builder

	b.WriteString("## Discussion\n\nSeats:\n")

	for _, s := range o.mobSeats {
		fmt.Fprintf(&b, "- %s (%s): %s\n", s.Name, s.Lens, s.Model)
	}

	for _, g := range o.d.Cfg.Mob.Guests {
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
		b.WriteString("Outcome: unresolved dissent - carried into the output as risk notes\n")
	}

	fmt.Fprintf(&b, "Cost: $%.4f", out.CostUSD)

	o.recordSection(ctx, "Discussion", b.String())
	o.d.logCard(ctx, "mob discussion recorded (%d seats, %d rounds, consensus=%t)",
		len(o.mobSeats), rounds, out.Consensus)
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

	// The 'plan decision' model resolves lazily, on the first point the model
	// certainly runs (a brainstorm turn, the diagnose pass, the solo planner,
	// or a plan-gate classification), mirroring runReviewHITL's gate-model
	// resolver: a successful mob discussion drafts the plan without this model,
	// and a card promoted at the first gate skips the classification too, so an
	// entry-time resolution would put a model_selected line on the transcript
	// naming a model that never ran. The slug resolves once and is reused; the
	// model-bearing steps run sequentially inside this phase, so the captured
	// value needs no lock. announce guards the entry-time card log so the
	// diagnose recovery's forced re-resolution below speaks only through its
	// own distinction line.
	resolved := ""

	announced := false

	decisionModel := func(ctx context.Context) string {
		if resolved == "" {
			resolved = resolveDecisionModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
				o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel, o.excludedModels(), "plan decision")

			if !announced {
				announced = true

				d.logCard(ctx, "orchestrator model: %s", resolved)
			}
		}

		return resolved
	}

	// Creative HITL cards get a design dialogue before planning (create-plan
	// Phase 0 Branch C). Skipped in autonomous, for non-creative cards, and when
	// a design already exists on the claim-time body - a prior brainstorm's
	// record or a human-authored one - which is recovered here so it reaches
	// the planner through the same AGREED DESIGN channel a fresh brainstorm
	// uses. Branch C and the bug Branch B are mutually exclusive (isCreative
	// excludes bug-like cards).
	design := recordedDesign(o.tc.Description)

	if cfg.Interactive && isCreative(o.tc) && design == "" {
		d, err := o.runBrainstorm(ctx, decisionModel)
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
		d.logCard(ctx, "running root-cause investigation (bug-like card)")

		diag, derr := o.runDiagnose(ctx, decisionModel)
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
			var ie *IncapableError
			if errors.As(derr, &ie) {
				// The model could not drive the tool loop. Blacklist and exclude
				// it now so the planner, the mob seats and the first coder pick
				// do not land on it again this run; the re-selection cap error
				// is advisory here - planning continues without a diagnosis.
				if rerr := o.recoverIncapable(ctx, ie); rerr != nil {
					slog.Warn("plan: diagnose model incapable and re-selection cap exhausted",
						"card_id", cfg.CardID, "error", rerr)
				}

				// recoverIncapable extended the exclusion set; the resolver's
				// resolve-once cache must be invalidated so the next call
				// re-selects off the excluded slug. The announced flag stays
				// set: this forced re-resolution speaks through one of the two
				// distinction lines below, never the entry-time line again.
				prev := resolved

				resolved = ""

				model := decisionModel(ctx)

				if model != prev {
					d.logCard(ctx, "orchestrator model: %s (re-selected after diagnose)", model)
				} else {
					d.logCard(ctx, "no alternative decision model available; continuing on %s", model)
				}
			}

			slog.Warn("plan: diagnose step failed; planning without a diagnosis",
				"card_id", cfg.CardID, "error", derr)
			d.logCard(ctx, "plan: diagnose step failed; planning without a diagnosis (%s)", derr.Error())
		}
	}

	mobPlan := cfg.Mob.enabled() && cfg.Mob.Plan

	// Autonomous: draft once and create the subtasks. With the mob session on,
	// the draft comes from a panel discussion; any discussion failure degrades
	// to the solo draftPlan path, byte-identical to before.
	if !cfg.Interactive {
		if mobPlan {
			if p, out, ok := o.mobDraftPlan(ctx, diagnosis, design, "", nil); ok {
				p, out = o.reviseMobTestSplit(ctx, diagnosis, design, p, out)

				if err := o.createSubtasks(ctx, p); err != nil {
					return err
				}

				o.recordDiscussion(ctx, out)

				return nil
			}
		}

		p, err := o.draftPlan(ctx, decisionModel, diagnosis, design, "")
		if err != nil {
			return err
		}

		return o.createSubtasks(ctx, p)
	}

	// HITL: draft -> present -> gate; on adjust, re-draft with the feedback.
	// Subtasks are created only after approval, so an adjust never orphans
	// cards. With the mob session on, drafts come from discussions and an
	// adjust re-opens the discussion for one feedback round; once a discussion
	// fails, the rest of the phase stays on the solo path.
	feedback := ""

	var lastOut *mob.Outcome

	mobSolo := false

	for range maxPlanDrafts {
		var p plan

		drafted := false

		if mobPlan && !mobSolo {
			var (
				out *mob.Outcome
				ok  bool
			)

			p, out, ok = o.mobDraftPlan(ctx, diagnosis, design, feedback, lastOut)
			if ok {
				p, out = o.reviseMobTestSplit(ctx, diagnosis, design, p, out)
				drafted = true
				lastOut = out
			} else {
				mobSolo = true
			}
		}

		if !drafted {
			var err error

			p, err = o.draftPlan(ctx, decisionModel, diagnosis, design, feedback)
			if err != nil {
				return err
			}

			lastOut = nil
		}

		o.recordSection(ctx, "Plan", sectionFrom("Plan", formatPlannedPlan(p)))

		outcome, fb, gerr := o.gate(ctx, gatePlanApproval, decisionModel, presentPlan(p))
		if gerr != nil {
			return gerr
		}

		// A promotion passes the plan gate: autonomous runs create subtasks
		// without a sign-off, so passthrough matches autonomous semantics here.
		if outcome == gateApprove || outcome == gatePromoted {
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
// records the resulting refs (plus the card-level sizing) on the run struct.
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

		s := seedSubtaskSizing(st.Tier, st.Description)
		body := writeMeta(st.Description, markerFor(s, st.Tier))

		id, err := d.Ops.CreateCard(ctx, cfg.Project, cfg.CardID, st.Title, body, depIDs)
		if err != nil {
			return fmt.Errorf("create subtask %q: %w", st.Title, err)
		}

		if s.Budget > seedSizing(st.Tier).Budget {
			d.logCard(ctx, "subtask %s lists %d files - turn budget widened to %s",
				id, len(filePathTokens(filesSection(st.Description))), budgetLabel(s.Budget))
		}

		ids[i] = id
		o.subtasks = append(o.subtasks, subtaskRef{
			ID:           id,
			Title:        st.Title,
			Body:         st.Description,
			Sizing:       s,
			PlannerBar:   st.Tier,
			State:        "todo", // freshly created; resume reconciliation refreshes this
			DependsOnIDs: depIDs,
		})
	}

	o.cardSizing = seedSizing(p.CardTier)
	o.cardPlannerBar = p.CardTier

	// Fold the card-level marker into the body BEFORE the "## Plan" record
	// pushes it: the parent card is the only persistence a resumed run can read,
	// and without this every resumed run sizes its review panel and its
	// Best-of-N pool at the moderate default. recordSection preserves whatever
	// is above its heading, so one write carries both.
	o.body = writeMeta(o.body, markerFor(o.cardSizing, p.CardTier))

	// Record the plan on the parent card body so it carries the full history
	// (the subtask cards hold the detail; this is the consolidated view, like
	// CM's create-plan workflow skill writes ## Plan).
	o.recordSection(ctx, "Plan", sectionFrom("Plan", formatPlan(o.subtasks)))

	return nil
}

// recordedDesign returns the content of a "## Design" section already present
// on the claim-time body - a prior brainstorm's record or a human-authored
// design - without the heading line, matching what runBrainstorm returns.
func recordedDesign(description string) string {
	sec := extractSection(description, "Design")
	if sec == "" {
		return ""
	}

	return strings.TrimSpace(strings.TrimPrefix(sec, "## Design"))
}

// plannerDescription is the description slot for the planner prompts: the
// stripped description plus the prior run's recorded "## Plan". The prior plan
// has no other resume channel - resumeBlock carries subtask titles only. Reads
// the immutable claim-time snapshot, not the live body, so a fresh run's
// adjust loop never sees its own in-flight draft.
func (o *run) plannerDescription() string {
	prior := extractSection(o.tc.Description, "Plan")
	if prior == "" {
		return o.taskDescription
	}

	return o.taskDescription + "\n\n" + prior
}
