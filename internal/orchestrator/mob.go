package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// MobGuest is one operator-registered external A2A participant, delivered
// via the trigger payload (orchestrator-local so this package never imports
// protocol).
type MobGuest struct{ Name, URL, Token string }

// MobConfig is the orchestrator's view of mob session mode for one run. The
// worker maps the payload spec onto it; the zero value is off.
type MobConfig struct {
	Participants int  // 0 = off; >= 2 = on
	Plan         bool // phases contain "plan"
	Review       bool // phases contain "review"
	// Execute enables post-subtask checkpoint discussions: phases contain
	// "execute" AND the payload's execute_checkpoints server flag rode along.
	Execute bool
	// CheckpointMinTier is the minimum subtask tier that convenes a
	// checkpoint: simple|moderate|complex|critical. Worker-defaulted to
	// "simple" when Execute is on.
	CheckpointMinTier string
	// CheckpointRounds is the critique-round count for checkpoint
	// discussions (plan/review keep Rounds). Worker-defaulted to 3.
	CheckpointRounds int
	Rounds           int // critique rounds (CM-clamped; default 2)
	BudgetFactor     float64
	Guests           []MobGuest
}

func (c MobConfig) enabled() bool { return c.Participants >= 2 }

// Lens priority tables (spec § Seats and selection): callers slice [:seats]
// so any seat count 2..5 is well-defined.
var planLenses = []string{"feasibility/simplicity", "architecture/extensibility", "risk/testing", "performance", "developer-experience"}

var reviewLenses = []string{"correctness", "security", "design", "performance", "developer-experience"}

// mobSeatMaxTurns caps one seat turn's harness run (spec constant - fixed by
// the design, not config).
const mobSeatMaxTurns = 8

// mobModeratorMaxTurns caps a moderator call. Moderator calls run
// TOOLLESS: convergence classification and synthesis transform the
// transcript they are handed. Run 2 showed a tooled moderator burning its
// whole budget exploring the repo instead of synthesizing (empty output →
// solo fallback). With no tools a call normally completes in one turn; the
// cap is a backstop.
const mobModeratorMaxTurns = 4

// mobSeatToolOutputMaxBytes caps one tool result in a seat's context. Seats
// argue positions from read-only lookups; the coder-sized cap (128 KB) let a
// single broad grep inflate a seat prompt ~17x in the first live run.
const mobSeatToolOutputMaxBytes = 16 * 1024

// mobSeatWrapUpTurns is the remaining-turn threshold at which a seat run
// gets the harness wrap-up nudge. Run 2 showed seats burning all 8 turns on
// exploration and returning empty utterances; the nudge forces a position
// while turns remain.
const mobSeatWrapUpTurns = 2

// mobDiscuss convenes one discussion: it mints the per-discussion bearer,
// starts the loopback seat server, builds the engine config, runs Discuss,
// and closes the server. It NEVER fails the card: any error - bearer, server
// start, quorum, engine - logs, leaves an advisory card-log entry, and
// returns ok=false so the caller falls back to its solo path.
func (o *run) mobDiscuss(ctx context.Context, t mob.Topic) (mob.Outcome, bool) {
	bearer, err := mobBearer()
	if err != nil {
		slog.Warn("mob: bearer generation failed", "card_id", o.d.Cfg.CardID, "error", err)

		return mob.Outcome{}, false
	}

	// Bound this discussion by what remains of the run's single mob session
	// budget term. effectiveCeiling adds exactly one BudgetFactor x MaxCardCost
	// term on top of the card ceiling, so every discussion (plan draft, each
	// review round, each HITL re-open) draws from that shared headroom rather
	// than a fresh term each time. An unlimited ceiling (MaxCardCost <= 0)
	// keeps the unbounded mob session sizing and never exhausts.
	bounded := o.d.Cfg.MaxCardCost > 0

	headroom := 0.0
	if bounded {
		headroom = effectiveCeiling(o.d.Cfg) - o.ledger.Spent()
		if headroom <= 0 {
			slog.Warn("mob: budget exhausted; continuing solo", "card_id", o.d.Cfg.CardID, "kind", t.Kind)
			_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory degrade record
				fmt.Sprintf("mob budget exhausted (%s) - continuing solo", t.Kind))

			return mob.Outcome{}, false
		}
	}

	cfg := buildEngineConfig(o, t, bearer)
	o.mobSeats = cfg.Seats

	// Clamp the mob session term to the remaining headroom (min keeps whichever binds).
	if bounded {
		cfg.BudgetUSD = min(cfg.BudgetUSD, headroom)
	}

	server, err := mob.StartServer(cfg.Seats, cfg.Runner, bearer)
	if err != nil {
		slog.Warn("mob: loopback server failed to start", "card_id", o.d.Cfg.CardID, "error", err)
		_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory degrade record
			fmt.Sprintf("mob discussion unavailable (%s): %v - continuing solo", t.Kind, err))

		return mob.Outcome{}, false
	}
	defer server.Close() //nolint:errcheck

	cfg.SeatEndpoint = server.SeatEndpoint

	engine := o.mobEngine
	if engine == nil {
		engine = func(ctx context.Context, cfg mob.EngineConfig, t mob.Topic) (mob.Outcome, error) {
			return mob.NewEngine(cfg).Discuss(ctx, t)
		}
	}

	out, err := engine(ctx, cfg, t)
	if err != nil {
		slog.Warn("mob: discussion failed", "card_id", o.d.Cfg.CardID, "kind", t.Kind, "error", err)
		_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, //nolint:errcheck // advisory degrade record
			fmt.Sprintf("mob discussion failed (%s): %v - continuing solo", t.Kind, err))

		return out, false
	}

	return out, true
}

// buildEngineConfig assembles the discussion engine's configuration for one
// topic: registry-selected seat models (review topics exclude the models that
// coded the card), operator guests, the harness-backed seat runner, the
// decision-model moderator, live "discussion" events, the human inbox, and
// the mob session budget term. SeatEndpoint is NOT set here - the caller
// wires it once the loopback server has started (the server is built from
// this config's Seats/Runner, so it cannot exist before this call returns).
func buildEngineConfig(o *run, t mob.Topic, bearer string) mob.EngineConfig {
	var exclude map[string]bool
	// Review and checkpoint topics judge code this run wrote: exclude the
	// models that coded it (and the per-card incapable set).
	if t.Kind == "review" || t.Kind == "checkpoint" {
		exclude = o.reviewExclusions()
	}

	panel := o.d.Registry.SelectDiscussionPanel(registry.SelectInput{
		Role:    registry.RoleReviewer,
		Tier:    registry.TierComplex,
		Exclude: exclude,
	}, len(t.Lenses))

	seats := make([]mob.SeatConfig, len(t.Lenses))
	for i, lens := range t.Lenses {
		seats[i] = mob.SeatConfig{
			Name:  fmt.Sprintf("seat-%d", i+1),
			Lens:  lens,
			Model: panel[i].Model,
		}
	}

	guests := make([]mob.GuestSeat, 0, len(o.d.Cfg.Mob.Guests))
	for _, g := range o.d.Cfg.Mob.Guests {
		guests = append(guests, mob.GuestSeat{Name: g.Name, URL: g.URL, Token: g.Token})
	}

	// mobBudget = BudgetFactor x MaxCardCost; a disabled card budget disables
	// the mob session term too. Per-turn seat cap: mobBudget / (seats x
	// (Rounds+2)) - Rounds critique rounds plus the blind round plus synthesis
	// headroom.
	budget := 0.0
	if o.d.Cfg.MaxCardCost > 0 {
		budget = o.d.Cfg.Mob.BudgetFactor * o.d.Cfg.MaxCardCost
	}

	perTurn := 0.0
	if budget > 0 && len(seats) > 0 {
		perTurn = budget / (float64(len(seats)) * float64(t.Rounds+2))
	}

	sink := &seatDebugSink{w: o.seatDebug}

	return mob.EngineConfig{
		Seats:    seats,
		Guests:   guests,
		Runner:   o.mobSeatRunner(sink, perTurn),
		Moderate: o.mobModeratorRunner(sink),
		Emit: func(author, lens, model string, round int, content string) {
			o.d.Emit.Emit(events.Kind("discussion"), map[string]any{
				"agent":   author,
				"lens":    lens,
				"model":   model,
				"round":   round,
				"content": content,
			})
		},
		Inbox:     o.d.Human,
		BudgetUSD: budget,
		Bearer:    bearer,
	}
}

// seatContextMaxBytes bounds the accumulated seat conversation carried
// between rounds. Positions and round prompts are never dropped; oldest
// tool outputs are blanked first. At the spec round counts (blind + <=2
// critique rounds x 8 turns x 16 KB tool cap) the bound is rarely hit - it
// is insurance against pathological accumulation, not compaction.
const seatContextMaxBytes = 384 * 1024

// trimmedToolMarker replaces a tool output dropped by trimSeatContext.
const trimmedToolMarker = "[tool output dropped to bound seat context - re-read the file if needed]"

// trimSeatContext prepares a finished seat run's messages for reuse as the
// next round's history: it drops the leading system message (seatConfig
// re-adds it every run) and, while the total size exceeds
// seatContextMaxBytes, blanks tool outputs oldest-first.
func trimSeatContext(msgs []llm.Message) []llm.Message {
	if len(msgs) > 0 && msgs[0].Role == "system" {
		msgs = msgs[1:]
	}

	total := 0
	for i := range msgs {
		total += len(msgs[i].Content)
	}

	for i := 0; total > seatContextMaxBytes && i < len(msgs); i++ {
		if msgs[i].Role != "tool" || len(msgs[i].Content) <= len(trimmedToolMarker) {
			continue
		}

		total -= len(msgs[i].Content) - len(trimmedToolMarker)
		msgs[i].Content = trimmedToolMarker
	}

	return msgs
}

// mobSeatRunner returns the SeatRunner: each turn is a fresh harness run on
// the seat's model with read-only tools, the seat persona system prompt, and
// the per-turn cost cap. sessions carries each seat's full message
// transcript (tool calls and results included) across rounds of one
// discussion, so a seat argues round N from what it read in rounds 0..N-1
// instead of re-exploring. The text-only A2A history remains the fallback
// for the first round and for replacement tasks. Events go to the
// seat-debug emitter so seat tool chatter stays off the live stream. Usage
// is spent against the run ledger and reported to CM per turn.
func (o *run) mobSeatRunner(sink *seatDebugSink, perTurnCap float64) mob.SeatRunner {
	var (
		mu       sync.Mutex
		sessions = map[string][]llm.Message{}
	)

	return func(ctx context.Context, seat mob.SeatConfig, history []mob.Turn, prompt string) (string, float64, error) {
		mu.Lock()
		seeded := sessions[seat.Name]
		mu.Unlock()

		if seeded == nil {
			seeded = mobHistory(history)
		}

		cfg := seatConfig(o.harnessConfig(seat.Model), seat, perTurnCap, seeded)

		emit := events.NewEmitter(io.Discard, sink.named(seat.Name))

		res, err := harness.Run(ctx, o.d.Client, o.d.ReadTools, emit, prompt, cfg)

		used := o.spendAndReport(ctx, o.ledger, o.d.Cfg.CardID, "mob: report seat usage failed",
			res, seat.Model, "seat", seat.Name)

		if err != nil {
			return "", res.TotalCostUSD, fmt.Errorf("seat %s run: %w", seat.Name, err)
		}

		out := res.Output
		msgs := res.Messages

		// Budget stops mid-exploration leave res.Output empty with a nil
		// error; posting "" gave run 2 its empty bubbles and starved the
		// moderator. One toolless single-turn call, seeded with the run's
		// own transcript, converts the exploration already paid for into a
		// position. Covers max_cost stops, which the wrap-up nudge cannot.
		if strings.TrimSpace(out) == "" {
			finalCfg := cfg
			finalCfg.History = trimSeatContext(msgs)
			finalCfg.MaxTurns = 1
			finalCfg.MaxCostUSD = 0 // one bounded call; the turn cap is the guard
			finalCfg.WrapUpTurns = 0
			finalCfg.WrapUpMessage = ""

			fres, ferr := harness.Run(ctx, o.d.Client, tools.NewRegistry(), emit, seatForcedFinalPrompt, finalCfg)

			// Report on the first call's resolved model: clearing ModelUsed keeps
			// the backstop from re-falling-back to a different slug.
			fres.ModelUsed = ""
			o.spendAndReport(ctx, o.ledger, o.d.Cfg.CardID, "mob: report seat usage failed",
				fres, used, "seat", seat.Name, "call", "backstop")

			if ferr == nil {
				out = fres.Output
				msgs = fres.Messages
			}

			// Billed either way; keep the engine's discussion budget coherent
			// with the ledger even when the backstop call errors.
			res.TotalCostUSD += fres.TotalCostUSD
		}

		mu.Lock()
		sessions[seat.Name] = trimSeatContext(msgs)
		mu.Unlock()

		return out, res.TotalCostUSD, nil
	}
}

// seatConfig derives one seat turn's harness config from the run-wide base.
func seatConfig(base harness.Config, seat mob.SeatConfig, perTurnCap float64, history []llm.Message) harness.Config {
	base.SystemPrompt = fmt.Sprintf(seatSystemPrompt, seat.Name, seat.Lens)
	base.MaxTurns = mobSeatMaxTurns
	base.MaxCostUSD = perTurnCap
	base.ToolOutputMaxBytes = mobSeatToolOutputMaxBytes
	base.History = history
	base.WrapUpTurns = mobSeatWrapUpTurns
	base.WrapUpMessage = seatWrapUpMessage

	return base
}

// mobModeratorRunner returns the ModeratorRunner: one-shot decision-model
// calls (convergence classification, synthesis, repair re-synthesis). The
// model resolves lazily on first use - resolveDecisionModel does advisory
// card-log I/O and needs a ctx - and the engine calls Moderate sequentially,
// so the lazy init is race-free.
func (o *run) mobModeratorRunner(sink *seatDebugSink) mob.ModeratorRunner {
	model := ""
	emit := events.NewEmitter(io.Discard, sink.named("moderator"))

	return func(ctx context.Context, prompt string) (string, string, float64, error) {
		if model == "" {
			model = resolveDecisionModel(ctx, o.d.Registry, o.d.Emit, o.d.Ops, o.d.Cfg.CardID,
				o.tc.ModelOrchestrator, o.d.Cfg.PayloadModel, o.d.Cfg.DefaultModel)
		}

		cfg := o.harnessConfig(model)
		cfg.MaxTurns = mobModeratorMaxTurns

		res, err := harness.Run(ctx, o.d.Client, tools.NewRegistry(), emit, prompt, cfg)

		used := o.spendAndReport(ctx, o.ledger, o.d.Cfg.CardID, "mob: report moderator usage failed", res, model)

		if err != nil {
			return "", used, res.TotalCostUSD, fmt.Errorf("moderator run: %w", err)
		}

		return res.Output, used, res.TotalCostUSD, nil
	}
}

// mobHistory maps role-tagged discussion turns to seeded harness history.
func mobHistory(turns []mob.Turn) []llm.Message {
	if len(turns) == 0 {
		return nil
	}

	msgs := make([]llm.Message, len(turns))
	for i, t := range turns {
		msgs[i] = llm.Message{Role: t.Role, Content: t.Content}
	}

	return msgs
}

// mobBearer mints the per-discussion loopback bearer: 32 hex chars from
// crypto/rand (cheap, uniform with guest bearer auth).
func mobBearer() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mob bearer: %w", err)
	}

	return hex.EncodeToString(b[:]), nil
}

// formatDiscussionEntries renders transcript entries in the wire convention
// ("[round N] author (lens): text"), one blank line between entries. Used
// for repair re-synthesis and HITL re-open briefings.
func formatDiscussionEntries(entries []mob.Entry) string {
	lines := make([]string, 0, len(entries))

	for _, e := range entries {
		if e.Lens != "" {
			lines = append(lines, fmt.Sprintf("[round %d] %s (%s): %s", e.Round, e.Author, e.Lens, e.Content))
		} else {
			lines = append(lines, fmt.Sprintf("[round %d] %s: %s", e.Round, e.Author, e.Content))
		}
	}

	return strings.Join(lines, "\n\n")
}

// seatDebugSink serializes seat_debug writes from parallel seat runs onto
// the run's shared debug writer. named hands each seat (and the moderator)
// its own stamping writer; run 2's log could not attribute interleaved
// events because all seats shared one anonymous stream.
type seatDebugSink struct {
	mu sync.Mutex
	w  io.Writer
}

// named returns an io.Writer that rewrites worker JSONL event lines to
// kind "seat_debug", stamps the author under "seat", and funnels them
// through the shared sink.
func (s *seatDebugSink) named(seat string) io.Writer {
	return &seatDebugWriter{sink: s, seat: seat}
}

type seatDebugWriter struct {
	sink *seatDebugSink
	seat string
}

func (w *seatDebugWriter) Write(p []byte) (int, error) {
	w.sink.mu.Lock()
	defer w.sink.mu.Unlock()

	if _, err := w.sink.w.Write(rewriteSeatDebugLine(p, w.seat)); err != nil {
		return 0, err
	}

	return len(p), nil
}

// rewriteSeatDebugLine rewrites one JSONL event line's kind to
// "seat_debug", preserving the original under "seat_kind" and stamping the
// authoring seat under "seat". Unparsable input is returned unchanged.
func rewriteSeatDebugLine(p []byte, seat string) []byte {
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		return p
	}

	if kind, ok := m["kind"]; ok {
		m["seat_kind"] = kind
	}

	m["kind"] = "seat_debug"
	m["seat"] = seat

	out, err := json.Marshal(m)
	if err != nil {
		return p
	}

	return append(out, '\n')
}
