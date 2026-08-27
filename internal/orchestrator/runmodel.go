package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// ContextLimitError marks a phase stopping because the model neared its context
// window. The worker maps it like the budget park: push WIP, release, fail - so
// in-flight work survives and a human can split the subtask or pin a larger-window model.
type ContextLimitError struct {
	Model         string
	ContextWindow int
}

func (e *ContextLimitError) Error() string {
	return fmt.Sprintf("context limit reached for model %q (window %d tokens)", e.Model, e.ContextWindow)
}

// MaxTurnsError marks a phase stopping because the harness exhausted its turn
// cap (Reason "max_turns", Completed=false, err==nil). It is normalized to a
// typed error at the runModelCfg choke point so NO phase treats truncated work
// as success. Park-on-cap is no longer unconditional: two salvage paths rescue a
// capped subtask whose committed work passes an authoritative verify - a
// Best-of-N candidate capped on its FINAL subtask, deferred to the judge's
// verify gate (see salvageCapped), and a single-solver (parent / mob session)
// subtask, which has no judge and so runs the verify inline before completing (see
// salvageSoloCapped). Every other cap parks: the worker maps it like the
// context-limit park (push WIP, release, fail) so the partial work survives for
// resume.
type MaxTurnsError struct {
	Model string
	Turns int
}

func (e *MaxTurnsError) Error() string {
	return fmt.Sprintf("turn cap reached on model %q after %d turns", e.Model, e.Turns)
}

// IncapableError marks a phase stopping because the model cannot drive the tool
// loop - it emitted tool calls every turn but none parsed valid arguments.
// Reason is the harness's own IncapableDetail sentence when the failure pattern
// was classified (e.g. a suspected upstream gateway defect), or the generic
// fallback otherwise. The recovery path (recoverIncapable) catches this to
// blacklist the model and re-select.
type IncapableError struct {
	Model  string
	Reason string
}

func (e *IncapableError) Error() string {
	return fmt.Sprintf("model %q is harness-incapable: %s", e.Model, e.Reason)
}

// harnessConfig builds the per-phase harness.Config with the run-wide safety
// fields (size cap, secret redaction) plus the model's own context window.
// Centralizing this is the guard against a new phase forgetting the hardening.
//
// A ContextWindow of 0 (model absent from the catalog) is intentional and safe:
// the harness guards the limit check with `if cfg.ContextWindow > 0`, so an
// unknown/uncatalogued model simply opts out of context-limit detection rather
// than tripping it spuriously.
func (o *run) harnessConfig(model string) harness.Config {
	cfg := harness.Config{
		Model:              model,
		MaxTurns:           o.d.Cfg.MaxTurns,
		ToolOutputMaxBytes: o.d.Cfg.ToolOutputMax,
		RedactToolOutput:   o.d.Redact,
		ContextWindow:      o.d.Registry.ContextWindow(model),
		Provider:           o.d.Cfg.Provider,
	}

	// Opt into in-window compaction only when enabled; otherwise leave
	// Compaction nil so the harness keeps its hard context_limit stop.
	if o.d.Cfg.Compaction.Enabled {
		cfg.Compaction = &harness.Compaction{
			Threshold:       o.d.Cfg.Compaction.Threshold,
			KeepRecentTurns: o.d.Cfg.Compaction.KeepRecentTurns,
		}
	}

	// Reasoning is nil when effort is empty (off) so the field is omitted and
	// models that don't support it are unaffected.
	cfg.Reasoning = reasoningRaw(o.d.Cfg.ReasoningEffort)

	return cfg
}

// reasoningRaw renders an effort string to the OpenRouter reasoning object the
// harness carries. Returns nil for "" so the field is omitted. The L1 dialect
// translates this to reasoning_effort for the openai endpoint.
func reasoningRaw(effort string) json.RawMessage {
	if effort == "" {
		return nil
	}

	raw, _ := (llm.Reasoning{Effort: &effort}).Raw() //nolint:errcheck

	return raw
}

// runModel routes a phase model-call through the centralized config and
// normalizes a context_limit/incapable result into a typed error. The returned
// duration is the wall time of the harness step, threaded to spendAndReport.
func (o *run) runModel(ctx context.Context, reg *tools.Registry, prompt, model string) (harness.Result, time.Duration, error) {
	return o.runModelCfg(ctx, reg, prompt, model, o.harnessConfig(model))
}

// runModelCfg is the single choke point wrapping harness.Run. It times the
// call and returns the wall-clock duration alongside the result so every caller
// can report it to CM by return value - never via a shared field on run, which
// concurrent Best-of-N candidates and mob seats would race on.
func (o *run) runModelCfg(ctx context.Context, reg *tools.Registry, prompt, model string, cfg harness.Config) (harness.Result, time.Duration, error) {
	start := time.Now()
	res, err := harness.Run(ctx, o.d.Client, reg, o.d.Emit, prompt, cfg)
	dur := time.Since(start)

	if err == nil && res.Reason == "context_limit" {
		return res, dur, &ContextLimitError{Model: model, ContextWindow: o.d.Registry.ContextWindow(model)}
	}

	if err == nil && res.Reason == harness.ReasonIncapable {
		reason := res.IncapableDetail
		if reason == "" {
			reason = "cannot drive the tool loop"
		}

		return res, dur, &IncapableError{Model: model, Reason: reason}
	}

	if err == nil && res.Reason == "max_turns" {
		return res, dur, &MaxTurnsError{Model: model, Turns: res.Turns}
	}

	return res, dur, err
}

// spendAndReport is the shared accounting tail of every model call: it charges
// the result against ledger EVEN when the call errored (a failed or partial run
// burned tokens too), reports the usage to CM on targetCardID, and returns the
// model actually used - res.ModelUsed when the provider echoed one, else
// configuredModel. step tags the call kind (main, gate, checkpoint, ...) and
// dur is the harness step's wall time; both ride the usage report as
// observability telemetry (o.curPhase supplies the phase). A report failure is
// advisory: warned as warnMsg with card_id, any extraAttrs, and the error -
// never propagated. On success, the response's server-priced card total is
// synced into the RUN ledger (o.ledger, deliberately not the ledger parameter:
// Best-of-N candidates charge their own sub-ledgers locally but all report
// against the shared parent card, and syncing that total into one candidate
// would bill it for its siblings). This is what keeps the ceiling enforcing
// when a gateway reports no per-call cost.
func (o *run) spendAndReport(ctx context.Context, ledger *Ledger, targetCardID, warnMsg string,
	res harness.Result, configuredModel, step string, dur time.Duration, extraAttrs ...any,
) string {
	ledger.Spend(res.TotalCostUSD)

	used := res.ModelUsed
	if used == "" {
		used = configuredModel
	}

	total, reportErr := o.d.Ops.ReportUsage(ctx, targetCardID, cmclient.UsageReport{
		Model:               used,
		PromptTokens:        res.PromptTokens,
		CompletionTokens:    res.CompletionTokens,
		CacheReadTokens:     res.CacheReadTokens,
		CacheCreationTokens: res.CacheCreationTokens,
		ActualCostUSD:       res.TotalCostUSD,
		Phase:               o.curPhase,
		Step:                step,
		DurationMS:          dur.Milliseconds(),
		Source:              "collector",
	})
	if reportErr != nil {
		attrs := make([]any, 0, len(extraAttrs)+4)
		attrs = append(attrs, "card_id", targetCardID)
		attrs = append(attrs, extraAttrs...)
		attrs = append(attrs, "error", reportErr)

		slog.Warn(warnMsg, attrs...)
	} else {
		o.ledger.SyncServerTotal(targetCardID, total)
	}

	return used
}

// wrapUpTurns is the remaining-turn threshold at which coder-family runs get
// the harness wrap-up nudge: late enough to matter, early enough that a model
// can ignore it once, run one final check, and still land its closing message.
// An orchestrator constant on purpose - not an operator knob.
const wrapUpTurns = 5

// planRepairMaxTurns caps the planner's single repair turn. The first attempt
// already ran a full exploration pass; the repair must re-emit a valid plan,
// not restart the investigation - so it gets a tight budget (min'd with the
// configured cap so a smaller MaxTurns is never raised). Kept comfortably above
// wrapUpTurns so the wrap-up nudge still lands.
const planRepairMaxTurns = 12

// planMaxTurns caps the planner's first attempt. The plan phase is the one
// model run with no terminal tool and no tier scaling, so without a cap of its
// own it inherits the flat configured budget and the wrap-up nudge can sit
// beyond any turn the planner actually reaches - leaving the phase unguarded.
// Chosen to sit well above where a planner has converged in practice, so the
// cap is a backstop rather than the primary control.
const planMaxTurns = 25

// planTurnCap is the plan run's turn budget. Both cases are min'd with base so
// a smaller configured cap is never raised.
func planTurnCap(base int, repair bool) int {
	if repair {
		return min(base, planRepairMaxTurns)
	}

	return min(base, planMaxTurns)
}

// diagnoseMaxTurns caps the diagnosis phase. Like the planner it is a
// read-only investigation with no terminal tool and no tier scaling, so
// without a cap of its own it inherits the flat configured budget and the
// wrap-up nudge can sit beyond any turn it actually reaches. Set to the
// planner's value: the two phases explore comparable breadth, and the
// guarded planner never reached 25 across a recorded batch of 16 runs while
// an unguarded diagnosis run reached 28.
const diagnoseMaxTurns = 25

// diagnoseTurnCap is the diagnosis run's turn budget, min'd with base so a
// smaller configured cap is never raised - mirrors planTurnCap.
func diagnoseTurnCap(base int) int {
	return min(base, diagnoseMaxTurns)
}

// coderBatchNudgeTurns arms the harness batching nudge for the coder family:
// after this many consecutive turns that each spend a whole model call on one
// read-only lookup, the harness injects a single message suggesting the model
// group independent lookups. It suggests only - it never groups anything itself,
// so a model whose next lookup depends on this one can ignore it.
//
// Three is deliberate. It is the length at which the measured waste starts (37
// stretches of three or more consecutive single read-only turns across the
// recorded runs, one reaching six), and it is short enough that lookups remain
// to group when it lands. Two would fire on the ordinary dependent pair: grep,
// then read what it found.
//
// Scoped to the coder because that is the measured offender - 1.32 calls per
// turn, 79% of its turns making exactly one, despite being the only phase whose
// prompt already asks it to batch. Planning and diagnosis already batch at 2.39
// and 2.10 calls per turn; nudging them would spend the one-shot injection on a
// phase doing it right.
//
// The message is left empty on purpose: the harness default names the count,
// and no phase-specific wording improves on that.
const coderBatchNudgeTurns = 3

// runModelWrapUp is runModel with the wrap-up nudge configured: when
// wrapUpTurns turns remain before the cap, the harness injects msg once as a
// fresh user message. Used by the document run; the coder and fix runs use
// runModelCoder, which layers a laddered turn budget on the same nudge.
func (o *run) runModelWrapUp(ctx context.Context, reg *tools.Registry, prompt, model, msg string) (harness.Result, time.Duration, error) {
	cfg := o.harnessConfig(model)
	cfg.WrapUpTurns = wrapUpTurns
	cfg.WrapUpMessage = msg

	return o.runModelCfg(ctx, reg, prompt, model, cfg)
}

// runModelCoder is runModelWrapUp with a laddered turn budget and the batching
// nudge armed. budget is a step into turnBudgetLadder, not a turn count: the
// operator's base cap is the authority and the step scales it, so lifting the
// base lifts every rung.
//
// The cap and its wrap-up reserve come back from coderTurnCfg together, and the
// message is built from the reserve that call returned. The harness injects the
// nudge on exact equality with the REMAINING turns, so a widened cap under a
// constant reserve would buy unsupervised turns and then announce a deadline
// far too late - and a message quoting the constant would misstate the room the
// model actually has. Neither can drift now: one call returns the pair, and the
// message is a function of it.
//
// Used by the execute-phase coder and the review-phase fix run - both laddered
// coder work. document.go keeps runModelWrapUp: its work carries no sizing and
// is not read-heavy.
func (o *run) runModelCoder(ctx context.Context, reg *tools.Registry, prompt, model string,
	msg func(int) string, budget int,
) (harness.Result, time.Duration, error) {
	cfg := o.harnessConfig(model)
	cfg.MaxTurns, cfg.WrapUpTurns = coderTurnCfg(cfg.MaxTurns, budget)
	cfg.WrapUpMessage = msg(cfg.WrapUpTurns)
	// Message left empty: the harness default names the count of single-lookup
	// turns that triggered it, which is the useful part. Never both nudges: the
	// wrap-up above suppresses this one for the rest of the run, because telling
	// a model to batch its lookups while telling it to finish is contradictory.
	cfg.BatchNudgeTurns = coderBatchNudgeTurns
	// One terminal-only grace call at the cap: a coder whose work is done but
	// that dithered past the wrap-up nudge can still land finish (a run-1 failure
	// mode). This is the first net; the verify-gated salvage remains the second.
	cfg.GraceTurn = true

	return o.runModelCfg(ctx, reg, prompt, model, cfg)
}

// runModelPlan is the planner's model call. Unlike the coder phases, the planner
// finishes by emitting a JSON plan as its final message (there is no finish
// tool), so it gets a plan-specific wrap-up nudge that forces the emit before
// the turn cap instead of letting the model explore straight into it. Both the
// first attempt and the repair turn get a bounded budget (planTurnCap): the
// repair is tighter still, because the model already had a full exploration
// pass and must re-emit a valid plan, not start over.
func (o *run) runModelPlan(ctx context.Context, reg *tools.Registry, prompt, model string, images []llm.ImageURL, repair bool) (harness.Result, time.Duration, error) {
	cfg := o.harnessConfig(model)
	cfg.TaskImages = images
	cfg.WrapUpTurns = wrapUpTurns
	cfg.WrapUpMessage = planWrapUpMessage
	cfg.MaxTurns = planTurnCap(cfg.MaxTurns, repair)

	return o.runModelCfg(ctx, reg, prompt, model, cfg)
}

// runModelDiagnose is the diagnosis phase's model call. Like the planner it
// gets the wrap-up nudge and a phase cap of its own instead of the flat
// configured budget. GraceTurn is deliberately NOT set: the harness only
// grants the grace call when the registry carries a Terminal tool (see the
// harness's graceFinish), and the diagnosis investigator runs on d.ReadTools,
// which is read-only and registers none - exactly like the planner's
// registry, which omits the field for the same reason. Setting it here would
// be inert configuration, not an extra guard.
func (o *run) runModelDiagnose(ctx context.Context, reg *tools.Registry, prompt, model string, images []llm.ImageURL) (harness.Result, time.Duration, error) {
	cfg := o.harnessConfig(model)
	cfg.TaskImages = images
	cfg.WrapUpTurns = wrapUpTurns
	cfg.WrapUpMessage = diagnoseWrapUpMessage
	cfg.MaxTurns = diagnoseTurnCap(cfg.MaxTurns)

	return o.runModelCfg(ctx, reg, prompt, model, cfg)
}
