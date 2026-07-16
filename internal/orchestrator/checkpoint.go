package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// checkpointLenses is the execute-checkpoint lens priority table; callers
// slice [:seats] like planLenses/reviewLenses.
var checkpointLenses = []string{
	"correctness", "diff-hygiene/simplicity", "risk/regressions",
	"architecture-fit", "performance",
}

// tierRanks orders registry tiers for the checkpoint_min_tier floor.
var tierRanks = map[registry.Tier]int{
	registry.TierSimple:   0,
	registry.TierModerate: 1,
	registry.TierComplex:  2,
	registry.TierCritical: 3,
}

// checkpointMaxFixes bounds a revise verdict's fix list (spec: at most 3
// concrete fixes per checkpoint).
const checkpointMaxFixes = 3

// checkpointEligible reports whether sub gets an execute checkpoint on this
// run: mob on, execute phase live, and the subtask's tier at or above the
// configured floor.
func (o *run) checkpointEligible(sub subtaskRef) bool {
	cfg := o.d.Cfg.Mob
	if !cfg.enabled() || !cfg.Execute {
		return false
	}

	return tierRanks[tierOf(sub)] >= tierRanks[tierFromString(cfg.CheckpointMinTier)]
}

// checkpointVerdict is the moderator's synthesis decision for one execute
// checkpoint: proceed, or revise with concrete fixes, plus a short prose
// summary for the card record.
type checkpointVerdict struct {
	Verdict string `json:"verdict"` // "proceed" | "revise"
	Fixes   []fix  `json:"fixes"`
	Summary string `json:"summary"` // 4-5 line narrative; best-effort, never gates the verdict
}

// parseCheckpointVerdict extracts and validates the checkpoint synthesis
// JSON (tolerating prose / code fences, like parseVerdict). Any verdict
// other than proceed/revise is a parse error so the caller can take its
// single repair turn.
func parseCheckpointVerdict(s string) (checkpointVerdict, error) {
	raw, ok := extractJSON(s)
	if !ok {
		return checkpointVerdict{}, fmt.Errorf("no JSON object found in synthesis output")
	}

	var v checkpointVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return checkpointVerdict{}, fmt.Errorf("unmarshal checkpoint verdict JSON: %w", err)
	}

	if v.Verdict != "proceed" && v.Verdict != "revise" {
		return checkpointVerdict{}, fmt.Errorf("verdict must be %q or %q (got %q)", "proceed", "revise", v.Verdict)
	}

	return v, nil
}

// mobCheckpoint convenes the post-subtask checkpoint discussion on the diff
// committed since startHead and, on a revise verdict, runs ONE fix pass on
// the same solver. Best-effort throughout (the mob contract): any failure —
// diff, quorum, engine, parse, fix run — logs and returns so the run
// continues on the committed diff. The revised diff is never re-checkpointed.
func (o *run) mobCheckpoint(ctx context.Context, sc *solverCtx, sub subtaskRef, startHead string) {
	diff, err := sc.git.Diff(ctx, startHead)
	if err != nil || strings.TrimSpace(diff) == "" {
		slog.Warn("mob checkpoint: no diff to discuss; skipping",
			"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", err)

		return
	}

	// Byte-cap the briefing diff (judge-input precedent): past the cap the
	// seats argue from the diffstat instead.
	if len(diff) > judgeDiffCap {
		if stat, serr := sc.git.DiffStat(ctx, startHead); serr == nil && stat != "" {
			diff = stat
		} else {
			diff = diff[:judgeDiffCap]
		}
	}

	if o.envFacts == "" {
		o.envFacts = environmentFacts(o.d.Cfg.Workspace)
	}

	seats := min(o.d.Cfg.Mob.Participants, len(checkpointLenses))

	t := mob.Topic{
		Kind: "checkpoint",
		Briefing: fmt.Sprintf(checkpointBriefing, sub.Title, subtaskBody(sub), o.tc.Title,
			o.envFacts, fencedDiff(diff)),
		Lenses:          checkpointLenses[:seats],
		Rounds:          o.d.Cfg.Mob.CheckpointRounds,
		Blind:           false,
		SynthesisPrompt: fmt.Sprintf(checkpointSynthesisPrompt, o.grounding, sub.Title),
	}

	out, ok := o.mobDiscuss(ctx, t)
	if !ok {
		return
	}

	v, perr := parseCheckpointVerdict(out.Synthesis)
	if perr != nil {
		repaired, rerr := o.mobResynthesize(ctx, t, out, perr.Error())
		if rerr == nil {
			v, perr = parseCheckpointVerdict(repaired)
		}

		if perr != nil {
			_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory record
				fmt.Sprintf("mob checkpoint (%s): verdict unparsable — proceeding", sub.ID))

			return
		}
	}

	if v.Verdict != "revise" || len(v.Fixes) == 0 {
		_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory record
			fmt.Sprintf("mob checkpoint (%s): proceed", sub.ID))

		return
	}

	if len(v.Fixes) > checkpointMaxFixes {
		v.Fixes = v.Fixes[:checkpointMaxFixes]
	}

	_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory record
		fmt.Sprintf("mob checkpoint (%s): revise — %d fixes", sub.ID, len(v.Fixes)))

	// One fix pass, budget permitting. A ledger already at the card ceiling
	// declines the revise here; the next subtask's budget gate owns parking.
	if lerr := sc.ledger.Check(); lerr != nil {
		_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory record
			fmt.Sprintf("mob checkpoint (%s): revise skipped — budget exhausted", sub.ID))

		return
	}

	findings := formatFixes(verdict{Fixes: v.Fixes})
	prompt := fmt.Sprintf(checkpointRevisePrompt, o.skillEngage(), o.grounding, sc.workspace,
		verifyCommandBlock(o.resolvedVerifyPlan()), sub.Title, findings)

	res, rerr := o.runCoderWith(ctx, sc, sub, prompt)
	if rerr != nil {
		slog.Warn("mob checkpoint: revise run failed; proceeding on the committed diff",
			"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", rerr)

		// Discard the failed pass's partial edits so the next subtask's
		// commit cannot sweep them in. Best-effort: untracked files survive
		// a hard reset, and a reset failure only dirties attribution.
		if herr := sc.git.HardReset(ctx, "HEAD"); herr != nil {
			slog.Warn("mob checkpoint: failed to discard partial revise edits",
				"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", herr)
		}

		return
	}

	msg := finishCommitMessage(res.CompletionArgs)
	if msg == "" {
		msg = "fix: address checkpoint findings"
	}

	o.commitRevise(ctx, sc, sub, msg)
}

// commitRevise commits the revise pass's changes and surfaces a full
// decline (clean tree — the fixer applied nothing) on the card's activity
// log, so a "declined:" finish message is visible instead of vanishing.
// Best-effort like the rest of the checkpoint path.
func (o *run) commitRevise(ctx context.Context, sc *solverCtx, sub subtaskRef, msg string) {
	committed, cerr := sc.git.CommitWithMessage(ctx, msg)
	if cerr != nil {
		slog.Warn("mob checkpoint: revise commit failed",
			"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", cerr)

		return
	}

	if !committed {
		first, _, _ := strings.Cut(msg, "\n")
		_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory record
			fmt.Sprintf("mob checkpoint (%s): revise made no changes — %s", sub.ID, first))
	}
}
