package coop

import (
	"context"
	"fmt"
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
const critiqueInstruction = "Critique round %d: review the positions above. Critique, defend, revise, or concede — then state your current position."

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

			responded++
			totalCost += s.h.lastCost

			en := Entry{Author: s.h.name, Lens: s.h.lens, Round: round, Content: results[i].text}
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

		verdict, cost, err := e.classify(ctx, entries)
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
		h, err := dialGuest(ctx, g)
		if err != nil {
			e.cfg.Emit("moderator", "", -1, fmt.Sprintf("guest %s unreachable: %v", g.Name, err))

			h = &seatHandle{name: "guest-" + g.Name, guest: true, dead: true, absent: true}
		}

		h.deadline = e.cfg.GuestDeadline
		seats = append(seats, &liveSeat{h: h})
	}

	return seats
}

// classify runs the convergence check and normalizes the verdict to one of
// consensus / stalled / productive_disagreement.
func (e *Engine) classify(ctx context.Context, entries []Entry) (string, float64, error) {
	out, cost, err := e.cfg.Moderate(ctx, fmt.Sprintf(convergencePrompt, renderDelta(entries, 0, "", "")))
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
