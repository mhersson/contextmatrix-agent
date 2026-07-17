package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
)

// fallbackDocCommitMessage is used when the document agent wrote docs but its
// finish call carried no usable commit message.
const fallbackDocCommitMessage = "docs: update documentation"

// runDocument is the document phase: one orchestrator-model pass that decides
// whether external documentation is warranted and, if so, writes it, then code
// commits and pushes the result. It is modelled on writePRBody but with a
// best-effort invariant: the ONLY failure that fails the run is a budget park
// (o.ledger.Check at the top). Every other failure - model run, branch diff,
// commit, push - is logged and the run proceeds to review. Most cards correctly
// write nothing: the agent leaves a clean tree, CommitWithMessage reports
// committed == false, and the phase is a no-op.
//
// Placement between execute and review means review's diff includes the doc
// commit, so the specialists verify the docs' accuracy.
func runDocument(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg

	// Budget gate: the only error this phase ever propagates. A breach returns the
	// park error and the model is never called (consistent with every phase).
	if err := o.ledger.Check(); err != nil {
		return err
	}

	model := resolveOrchestratorModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel)

	// The branch diff grounds both the gate decision and doc accuracy. A diff
	// failure is non-fatal (best-effort): log and pass a placeholder so the prompt
	// slot is never blank.
	diff, derr := d.Git.Diff(ctx, cfg.BaseBranch)
	if derr != nil {
		slog.Warn("document: branch diff failed; continuing without diff context",
			"card_id", cfg.CardID, "error", derr)

		diff = "(branch diff unavailable)"
	}

	task := fmt.Sprintf(documentPrompt, o.skillEngage(), o.grounding, cfg.Workspace,
		o.tc.Title, o.tc.Description, o.planOverview(), diff, o.verifyDocContext())

	res, err := o.runModelWrapUp(ctx, d.WriteTools, task, model, documentWrapUpMessage)

	o.spendAndReport(ctx, o.ledger, cfg.CardID, "document: report usage failed", res, model)

	// Best-effort on any model error (transport, context-limit, incapable). A
	// *ContextLimitError is deliberately caught HERE, not propagated - otherwise
	// the execute() FSM loop would park an otherwise-good run on a doc overflow.
	// Budget was gated above and a mid-call overspend is caught by the next phase's
	// ledger.Check, so no model error reaching this arm is ever a budget park.
	if err != nil {
		slog.Warn("document: model run failed; continuing without docs",
			"card_id", cfg.CardID, "error", err)

		_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory; docs are optional
			"document: model run failed, continuing without docs: "+err.Error())

		return nil
	}

	// Commit iff the tree is dirty. After execute the tree is clean, so the only
	// uncommitted changes are doc files the agent just wrote. No docs → clean tree
	// → committed == false → no commit, no push. The finish call's commit_message
	// supplies the message; a missing or empty one falls back to the canonical
	// docs message.
	msg := finishCommitMessage(res.CompletionArgs)
	if msg == "" {
		msg = fallbackDocCommitMessage
	}

	committed, cerr := d.Git.CommitWithMessage(ctx, msg)
	if cerr != nil {
		slog.Warn("document: commit failed; continuing", "card_id", cfg.CardID, "error", cerr)

		_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory; docs are optional
			"document: committing docs failed, continuing: "+cerr.Error())

		return nil
	}

	if !committed {
		_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory outcome note
			"document: no external docs needed")

		return nil
	}

	// Push iff committed. A push failure is non-fatal: the local doc commit still
	// feeds review's Diff(base), and integrate's final lease push re-pushes the
	// whole branch, so the docs reach the remote via that backstop.
	if perr := o.pushBranch(ctx); perr != nil {
		slog.Warn("document: push failed; docs committed locally, integrate will re-push",
			"card_id", cfg.CardID, "error", perr)

		_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory; integrate re-pushes
			"document: pushing docs failed, continuing (integrate will re-push): "+perr.Error())

		return nil
	}

	_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory outcome note
		"document: wrote and pushed documentation ("+msg+")")

	return nil
}
