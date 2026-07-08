package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
)

// runIntegrate is the integrate phase: pure code with one optional model call
// for the PR body. It re-fetches the base, autosquash-rebases onto it (squashing
// the review fixups), then issues a guarded lease push using the remote tip
// recorded BEFORE the rebase. On a rebase conflict with the base it falls back
// (§5.6): abort (done by the helper), soft-reset to the merge-base, and recommit
// as ONE squashed commit whose content is byte-identical to what review
// approved — never auto-resolving, deferring the conflict to merge/PR time for a
// human. It optionally opens a PR (body written by the orchestrator model),
// reports the push, and transitions the parent to done.
func runIntegrate(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg

	// Re-fetch the base so the rebase targets the live tip; fetch our own branch
	// best-effort (a missing remote branch on first integrate is expected).
	if err := d.Git.Fetch(ctx, cfg.BaseBranch); err != nil {
		return fmt.Errorf("fetch base %q: %w", cfg.BaseBranch, err)
	}

	if err := d.Git.Fetch(ctx, cfg.Branch); err != nil {
		slog.Warn("integrate: fetch branch failed (expected on first push)", "card_id", cfg.CardID, "error", err)
	}

	// Record the remote tip BEFORE the rebase: it is the lease's expected remote
	// state, which the rebase does not change. "" means the branch is not yet on
	// the remote, so a plain guarded push is used instead of a lease push.
	tip, err := d.Git.RemoteTip(ctx, cfg.Branch)
	if err != nil {
		return fmt.Errorf("remote tip %q: %w", cfg.Branch, err)
	}

	onto := "origin/" + cfg.BaseBranch

	if rerr := d.Git.RebaseAutosquash(ctx, onto); errors.Is(rerr, ErrRebaseConflict) {
		// Conflict fallback: the helper already aborted, restoring the pre-rebase
		// HEAD and working tree. Soft-reset to the merge-base and recommit as one
		// squashed commit — content identical to what review approved. We never
		// auto-resolve; the conflict is deferred to merge/PR time for a human.
		mb, mberr := d.Git.MergeBase(ctx, onto, "HEAD")
		if mberr != nil {
			return fmt.Errorf("merge-base for conflict fallback: %w", mberr)
		}

		if err := d.Git.SoftReset(ctx, mb); err != nil {
			return fmt.Errorf("soft reset to merge-base: %w", err)
		}

		if _, err := d.Git.CommitWithMessage(ctx, squashMessage(o.tc.Title)); err != nil {
			return fmt.Errorf("squash commit on conflict fallback: %w", err)
		}

		_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory; the push is what must surface
			"integrate: rebase conflict with base; squashed onto merge-base, conflict deferred to merge")
	} else if rerr != nil {
		return fmt.Errorf("rebase autosquash onto %q: %w", onto, rerr)
	}

	// Guarded push: a known remote tip uses the lease (force-with-lease against the
	// recorded tip); an absent remote branch uses the plain guarded push.
	if tip != "" {
		if err := d.Git.ForcePushWithLease(ctx, cfg.Branch, tip); err != nil {
			return fmt.Errorf("lease push %q: %w", cfg.Branch, err)
		}
	} else {
		if err := d.Git.Push(ctx, cfg.Branch); err != nil {
			return fmt.Errorf("push %q: %w", cfg.Branch, err)
		}
	}

	prURL := ""

	if o.tc.CreatePR {
		url, err := o.openPR(ctx)
		if err != nil {
			// Budget parks must surface so the worker can park the run; any other PR
			// failure is non-fatal — the push already landed, so the work is safe.
			var be *BudgetExceededError
			if errors.As(err, &be) {
				return err
			}

			slog.Warn("integrate: PR creation failed; work is pushed, continuing without a PR",
				"card_id", cfg.CardID, "error", err)

			_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory; the push already landed
				"integrate: pull request creation failed — branch is pushed; open the PR manually: "+err.Error())
		} else {
			prURL = url
		}
	}

	if err := d.Ops.ReportPush(ctx, cfg.CardID, cfg.Branch, prURL); err != nil {
		return fmt.Errorf("report push: %w", err)
	}

	if err := d.Ops.TransitionCard(ctx, cfg.CardID, "done"); err != nil {
		return fmt.Errorf("transition parent to done: %w", err)
	}

	return nil
}

// openPR writes the PR body with one orchestrator-model call (budget-gated and
// spend-reported) and creates the pull request, returning its URL.
func (o *run) openPR(ctx context.Context) (string, error) {
	body, err := o.writePRBody(ctx)
	if err != nil {
		return "", err
	}

	url, err := o.d.PR.Create(ctx, o.tc.Title, body, o.d.Cfg.BaseBranch, o.d.Cfg.Branch)
	if err != nil {
		return "", fmt.Errorf("create pull request: %w", err)
	}

	return url, nil
}

// writePRBody runs ONE orchestrator-model call that drafts the PR description
// from the task, the plan overview, and the review outcome. Budget is checked
// before the call and the spend is reported after.
func (o *run) writePRBody(ctx context.Context) (string, error) {
	d := o.d
	cfg := d.Cfg

	if err := o.ledger.Check(); err != nil {
		return "", err
	}

	model := resolveOrchestratorModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel)

	task := fmt.Sprintf(prBodyPrompt, o.tc.Title, o.tc.Description, o.planOverview(), o.reviewOutcome())

	res, err := o.runModel(ctx, d.ReadTools, task, model)

	o.ledger.Spend(res.TotalCostUSD)

	used := res.ModelUsed
	if used == "" {
		used = model
	}

	if reportErr := d.Ops.ReportUsage(ctx, cfg.CardID, used,
		res.PromptTokens, res.CompletionTokens, res.TotalCostUSD); reportErr != nil {
		slog.Warn("integrate: report PR-body usage failed", "card_id", cfg.CardID, "error", reportErr)
	}

	if err != nil {
		return "", fmt.Errorf("PR-body run: %w", err)
	}

	return strings.TrimSpace(res.Output), nil
}

// planOverview renders the subtask titles as the PR body's plan-overview block.
// Empty (resume entering at integrate with no loaded subtasks) yields a neutral
// placeholder so the prompt slot is never blank.
func (o *run) planOverview() string {
	if len(o.subtasks) == 0 {
		return "(plan overview unavailable)"
	}

	var b strings.Builder

	for _, s := range o.subtasks {
		b.WriteString("- ")
		b.WriteString(s.Title)
		b.WriteString("\n")
	}

	return b.String()
}

// reviewOutcome returns the captured review summary, or a neutral note when the
// review phase was skipped on resume (no summary recorded).
func (o *run) reviewOutcome() string {
	if strings.TrimSpace(o.reviewSummary) == "" {
		return "(review approved; no summary recorded)"
	}

	return o.reviewSummary
}

// squashMessage derives the single squashed commit's conventional message from
// the card title (mirrors sanitizeTitle's "feat: <title>" convention). A blank
// title yields "feat: untitled".
func squashMessage(title string) string { return sanitizeTitle(title) }

// runDone is the done phase: bookkeeping only. It releases the parent's claim
// and logs a completion summary. Both are best-effort on the log; the release is
// also best-effort — the branch is pushed and the card is done, so a transient
// release error (including ErrCardNotClaimed when the claim lapsed) must not
// fail the run and trigger a false FAILED status.
//
// WithoutCancel on both: done runs as the FSM winds up, exactly when an
// end_session/EOF frame may have canceled the run context. The release and the
// completion note must still go out even when the run context is the thing that
// died — mirroring releaseSubtask (execute.go).
func runDone(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg
	ctx = context.WithoutCancel(ctx)

	if err := d.Ops.ReleaseCard(ctx, cfg.CardID); err != nil {
		if !errors.Is(err, cmclient.ErrCardNotClaimed) {
			slog.Warn("release card in done phase failed", "card", cfg.CardID, "error", err)
		}
	}

	_ = d.Ops.AddLog(ctx, cfg.CardID, //nolint:errcheck // advisory completion note
		fmt.Sprintf("run complete: %q integrated and pushed on %s; total spend $%.4f",
			o.tc.Title, cfg.Branch, o.ledger.Spent()))

	return nil
}
