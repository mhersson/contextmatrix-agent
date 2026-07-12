package coop

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// quorumMin is the minimum number of responding seats (any internal/guest
// mix) required in the first live round.
const quorumMin = 2

// closeAllBudget bounds the end-of-discussion close sweep.
const closeAllBudget = 10 * time.Second

// critiqueInstruction closes every critique-round delta.
const critiqueInstruction = "Critique round %d: review the positions above. Critique, defend, revise, or concede — then state your current position. The repository has not changed since the discussion started — do not re-run discovery; use tools only to check claims that are new this round."

// convergencePrompt classifies one round's state on the decision model.
const convergencePrompt = `You are moderating a multi-agent technical discussion. Classify its current state from the transcript below. Respond with EXACTLY one word and nothing else:

consensus - the participants substantially agree on one answer
productive_disagreement - positions differ and another round could reconcile them
stalled - positions are entrenched and repeating; another round will not help

TRANSCRIPT

%s`

// liveSeat pairs a handle with the engine's per-seat transcript cursor: the
// index into the entry slice up to which this seat has already seen content.
type liveSeat struct {
	h        *seatHandle
	lastSeen int
}

// Discuss runs one discussion: open, optional blind round, critique rounds
// with interjections/budget/convergence, synthesis, close. Any returned
// error (including ErrNoQuorum) means the caller falls back to the solo
// path — a discussion never fails the card.
func (e *Engine) Discuss(ctx context.Context, t Topic) (Outcome, error) {
	if t.Rounds < 1 {
		t.Rounds = 1
	}

	seats := e.open(ctx)
	defer e.closeAll(seats)

	e.cfg.Emit("moderator", "", -1, t.Briefing)

	// The briefing is transcript entry 0: replacement-task snapshots
	// (rendered from index 0) and the synthesis transcript carry it, while
	// established tasks — whose deltas start past it — never see it twice.
	entries := []Entry{{Author: "moderator", Round: 0, Content: t.Briefing}}

	var totalCost float64

	// dispatch fans one round out to every non-dead handle in parallel,
	// appends the responses as round entries (in seat order, for determinism),
	// emits them, advances the responders' cursors, and returns the number of
	// responders. bodyFor is called before dispatch, on the pre-round entries.
	dispatch := func(round int, bodyFor func(s *liveSeat) string) int {
		type turnResult struct {
			text string
			err  error
		}

		preLen := len(entries)
		results := make([]turnResult, len(seats))

		var wg sync.WaitGroup

		for i, s := range seats {
			if s.h.dead {
				continue
			}

			body := bodyFor(s)

			wg.Add(1)

			go func(i int, s *liveSeat) {
				defer wg.Done()

				text, err := s.h.sendTurn(ctx, round, body)
				results[i] = turnResult{text: text, err: err}
			}(i, s)
		}

		wg.Wait()

		responded := 0

		for i, s := range seats {
			if s.h.dead {
				continue
			}

			if results[i].err != nil {
				e.cfg.Emit("moderator", "", round,
					fmt.Sprintf("%s absent this round: %v", s.h.name, results[i].err))

				continue
			}

			text := strings.TrimSpace(results[i].text)
			if text == "" {
				// The seat ran out of budget before producing a position.
				// Money was spent and the round content was delivered into
				// its task history, so cost and cursor advance — but a blank
				// utterance is not a response: it must not count toward
				// quorum, enter the transcript, or reach the live stream as
				// an empty bubble.
				totalCost += s.h.lastCost
				s.lastSeen = preLen

				e.cfg.Emit("moderator", "", round,
					fmt.Sprintf("%s produced no position this round (budget exhausted before an answer)", s.h.name))

				continue
			}

			responded++
			totalCost += s.h.lastCost

			en := Entry{Author: s.h.name, Lens: s.h.lens, Round: round, Content: text}
			entries = append(entries, en)
			e.cfg.Emit(en.Author, en.Lens, en.Round, en.Content)

			s.lastSeen = preLen
		}

		return responded
	}

	quorumChecked := false

	if t.Blind {
		responded := dispatch(0, func(*liveSeat) string { return t.Briefing })

		quorumChecked = true

		if responded < quorumMin {
			return Outcome{Transcript: entries, CostUSD: totalCost},
				fmt.Errorf("%w: %d seats responded in round 0", ErrNoQuorum, responded)
		}
	}

	consensus := false

	for r := 1; r <= t.Rounds; r++ {
		roundStart := len(entries) // convergence sees only this round's positions

		if e.cfg.Inbox != nil {
			for _, um := range e.cfg.Inbox.Drain() {
				content := strings.TrimSpace(um.Content)
				if content == "" {
					continue
				}

				en := Entry{Author: "human", Round: r, Content: content}
				entries = append(entries, en)
				e.cfg.Emit(en.Author, "", en.Round, en.Content)
			}
		}

		if e.cfg.BudgetUSD > 0 && totalCost >= e.cfg.BudgetUSD {
			e.cfg.Emit("moderator", "", r, "discussion budget exhausted — synthesizing early")

			break
		}

		instruction := fmt.Sprintf(critiqueInstruction, r)

		responded := dispatch(r, func(s *liveSeat) string {
			from := s.lastSeen
			if s.h.taskID == "" {
				// Fresh or replacement task: full snapshot restores context.
				from = 0
			}

			return renderDelta(entries, from, s.h.name, instruction)
		})

		if !quorumChecked {
			quorumChecked = true

			if responded < quorumMin {
				return Outcome{Transcript: entries, CostUSD: totalCost},
					fmt.Errorf("%w: %d seats responded in round %d", ErrNoQuorum, responded, r)
			}
		}

		verdict, cost, err := e.classify(ctx, entries, roundStart)
		totalCost += cost

		if err != nil {
			// Classification is advisory; a failed classify never ends the
			// discussion — the round cap still bounds it.
			continue
		}

		if verdict == "consensus" {
			consensus = true

			break
		}

		if verdict == "stalled" {
			break
		}
	}

	synth, cost, err := e.cfg.Moderate(ctx,
		t.SynthesisPrompt+"\n\nFULL TRANSCRIPT\n\n"+renderDelta(entries, 0, "", ""))
	totalCost += cost

	if err != nil {
		return Outcome{Transcript: entries, CostUSD: totalCost},
			fmt.Errorf("coop: synthesis: %w", err)
	}

	e.cfg.Emit("moderator", "", -1, synth)

	return Outcome{
		Transcript: entries,
		Synthesis:  synth,
		Consensus:  consensus,
		CostUSD:    totalCost,
	}, nil
}

// open dials every internal seat and guest. A failed dial yields a dead
// placeholder handle (absent forever), surfaced via Emit with round -1 —
// the discussion proceeds on quorum.
func (e *Engine) open(ctx context.Context) []*liveSeat {
	seats := make([]*liveSeat, 0, len(e.cfg.Seats)+len(e.cfg.Guests))

	for _, sc := range e.cfg.Seats {
		h, err := dialSeat(ctx, sc.Name, sc.Lens, e.cfg.SeatEndpoint(sc.Name), e.cfg.Bearer)
		if err != nil {
			e.cfg.Emit("moderator", "", -1, fmt.Sprintf("seat %s failed to open: %v", sc.Name, err))

			h = &seatHandle{name: sc.Name, lens: sc.Lens, dead: true, absent: true}
		}

		h.deadline = e.cfg.InternalDeadline
		seats = append(seats, &liveSeat{h: h})
	}

	for _, g := range e.cfg.Guests {
		// Bound the dial by the guest deadline: a registered endpoint that
		// accepts the connection but never answers must degrade to a dead seat,
		// not hang the whole run. dialGuest's signature stays untouched — the
		// timeout rides its ctx. The client does not retain this ctx past
		// construction, so cancelling it here is safe.
		dialCtx, cancel := context.WithTimeout(ctx, e.cfg.GuestDeadline)
		h, err := dialGuest(dialCtx, g)

		cancel()

		if err != nil {
			// Name only in the transcript — the URL (and any error detail) would
			// otherwise reach the browser stream; keep the full error in the log.
			slog.Warn("coop: guest dial failed", "guest", g.Name, "error", err)
			e.cfg.Emit("moderator", "", -1, fmt.Sprintf("guest %s unreachable", g.Name))

			h = &seatHandle{name: "guest-" + g.Name, guest: true, dead: true, absent: true}
		}

		h.deadline = e.cfg.GuestDeadline
		seats = append(seats, &liveSeat{h: h})
	}

	return seats
}

// classify runs the convergence check and normalizes the verdict to one of
// consensus / stalled / productive_disagreement. from bounds the transcript
// to the current round's positions — the convergence model does not need
// earlier rounds re-sent.
func (e *Engine) classify(ctx context.Context, entries []Entry, from int) (string, float64, error) {
	out, cost, err := e.cfg.Moderate(ctx, fmt.Sprintf(convergencePrompt, renderDelta(entries, from, "", "")))
	if err != nil {
		return "", cost, err
	}

	word := strings.ToLower(strings.Trim(strings.TrimSpace(out), "\"'.`"))
	if i := strings.IndexAny(word, " \t\n"); i > 0 {
		word = word[:i]
	}

	switch word {
	case "consensus", "stalled":
		return word, cost, nil
	default:
		return "productive_disagreement", cost, nil
	}
}

// closeAll closes every live handle on a detached context — the engine's ctx
// may already be canceled, but parked tasks should still be completed.
func (e *Engine) closeAll(seats []*liveSeat) {
	ctx, cancel := context.WithTimeout(context.Background(), closeAllBudget)
	defer cancel()

	for _, s := range seats {
		if s.h.dead {
			continue
		}

		s.h.closeSeat(ctx)
	}
}
