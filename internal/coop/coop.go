// Package coop implements the A2A co-op discussion engine: N internal seats
// (one AgentExecutor each behind a loopback JSON-RPC server) plus optional
// operator-registered guests discuss a topic under a code-driven moderator.
// The engine is phase-agnostic: callers set the briefing, lenses, rounds, and
// synthesis instruction; the engine owns rounds, quorum, convergence, budget,
// and transcript wiring. Wire and behavior spec:
// docs/superpowers/specs/2026-07-10-a2a-coop-design.md (contextmatrix repo).
package coop

import (
	"context"
	"errors"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"
)

const (
	// utteranceCap bounds one utterance in bytes on receipt (spec-fixed).
	utteranceCap = 16 * 1024

	// truncationMarker is appended when an utterance is cut at utteranceCap.
	truncationMarker = "\n[truncated by moderator]"

	// maxSeatTurns is the spec-fixed harness turn cap for one internal seat
	// turn. The orchestrator glue mirrors this value when it builds the seat
	// runner's harness config.
	maxSeatTurns = 8

	// internalTurnDeadline and guestTurnDeadline are the spec-fixed per-turn
	// deadlines, measured from SendMessage to utterance.
	internalTurnDeadline = 240 * time.Second
	guestTurnDeadline    = 300 * time.Second
)

// ErrNoQuorum marks a quorum failure — fewer than two seats responded in the
// first live round. Callers treat it (like any Discuss error) as a signal to
// fall back to the existing solo path; a discussion never fails the card.
var ErrNoQuorum = errors.New("coop: no quorum")

// Topic is one discussion request. The engine never branches on Kind — it is
// labeling/telemetry only; Rounds/Blind/Lenses are the control knobs.
type Topic struct {
	Kind     string // "plan" | "review" | "checkpoint"
	Briefing string
	Lenses   []string // one per internal seat, priority-ordered
	Rounds   int      // critique rounds >= 1 (round 0 is controlled by Blind)
	Blind    bool     // run an independent proposal round first

	// SynthesisPrompt is the caller-supplied synthesis instruction; the
	// engine appends the full rendered transcript to it and calls Moderate
	// once. Dissent guidance lives here, not in the engine.
	SynthesisPrompt string
}

// Entry is one transcript line. Author is "seat-1".."seat-N",
// "guest-<name>", "human", or "moderator". Lens is "" for human/moderator.
type Entry struct {
	Author  string
	Lens    string
	Round   int // 0 = blind proposals; critique rounds start at 1
	Content string
}

type Outcome struct {
	Transcript []Entry
	Synthesis  string
	Consensus  bool
	CostUSD    float64
}

// Turn is a role-tagged prior exchange rebuilt from A2A task history.
// Role is "user" (moderator->seat) or "assistant" (seat->moderator).
type Turn struct {
	Role    string
	Content string
}

// SeatConfig describes one internal seat.
type SeatConfig struct {
	Name  string // "seat-1"..
	Lens  string
	Model string // registry-selected model id (informational; runner resolves)
}

// GuestSeat is a resolved guest endpoint.
type GuestSeat struct {
	Name  string // registry name; transcript author is "guest-"+Name
	URL   string
	Token string
}

// SeatRunner executes one internal seat turn (a fresh harness run in the
// orchestrator glue). history is the seat's prior conversation; prompt is the
// incoming round message. Returns the utterance text and its USD cost.
type SeatRunner func(ctx context.Context, seat SeatConfig, history []Turn, prompt string) (string, float64, error)

// ModeratorRunner executes one moderator model call (convergence classify,
// synthesis) on the decision model. Returns output text and USD cost.
type ModeratorRunner func(ctx context.Context, prompt string) (string, float64, error)

// EmitFn publishes one live transcript event (kind "discussion") — wired to
// the orchestrator's emitter. round may be -1 for non-round notices.
type EmitFn func(author, lens string, round int, content string)

type EngineConfig struct {
	Seats     []SeatConfig
	Guests    []GuestSeat
	Runner    SeatRunner
	Moderate  ModeratorRunner
	Emit      EmitFn        // never nil
	Inbox     harness.Inbox // human interjections; nil = none
	BudgetUSD float64       // co-op budget term; 0 = unlimited

	// SeatEndpoint maps an internal seat name to its loopback A2A endpoint —
	// wired to (*Server).SeatEndpoint by the caller, which owns the server
	// lifecycle (StartServer/Close). Bearer authenticates every internal-seat
	// call. Runner above is not invoked by the engine directly: the caller
	// passes the same value to StartServer, whose executors call it; it rides
	// this config so the glue configures one object.
	SeatEndpoint func(name string) string
	Bearer       string

	// test seams (defaulted by NewEngine):
	InternalDeadline time.Duration // 240 * time.Second
	GuestDeadline    time.Duration // 300 * time.Second
}

// Engine runs co-op discussions. Construct with NewEngine; the Discuss
// implementation lives in engine.go.
type Engine struct {
	cfg EngineConfig
}

// NewEngine returns an Engine with the spec deadlines defaulted and a no-op
// emitter substituted for a nil one (the contract says Emit is never nil; the
// default keeps a mis-wired caller from panicking instead of discussing).
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.InternalDeadline <= 0 {
		cfg.InternalDeadline = internalTurnDeadline
	}

	if cfg.GuestDeadline <= 0 {
		cfg.GuestDeadline = guestTurnDeadline
	}

	if cfg.Emit == nil {
		cfg.Emit = func(string, string, int, string) {}
	}

	return &Engine{cfg: cfg}
}
