// Package logbridge converts worker event JSONL into protocol.LogEntry
// frames and fans them out to /logs SSE subscribers.
package logbridge

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-harness/redact"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const subBufSize = 256

// sub is one active subscriber.
type sub struct {
	ch      chan protocol.LogEntry
	project string // empty = all projects
}

// DropObserver is notified once per LogEntry dropped because a subscriber's
// channel was full. The serve layer supplies a Prometheus-backed adapter; the
// interface keeps logbridge free of any metrics dependency.
type DropObserver interface {
	ObserveDrop()
}

// Hub fans out LogEntry frames to registered subscribers.
// mu protects subs and nextID.
type Hub struct {
	mu           sync.Mutex
	subs         map[int]*sub
	nextID       int
	dropObserver DropObserver
}

// NewHub creates a ready Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[int]*sub)}
}

// NewHubWithDropObserver creates a Hub that notifies obs each time a full
// subscriber channel forces a drop. A nil obs behaves like NewHub.
func NewHubWithDropObserver(obs DropObserver) *Hub {
	return &Hub{subs: make(map[int]*sub), dropObserver: obs}
}

// Subscribe registers a subscriber. An empty project string receives all
// entries regardless of project. Returns an opaque id for Unsubscribe.
func (h *Hub) Subscribe(project string) (int, <-chan protocol.LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	id := h.nextID
	ch := make(chan protocol.LogEntry, subBufSize)
	h.subs[id] = &sub{ch: ch, project: project}

	return id, ch
}

// Unsubscribe removes the subscriber and closes its channel.
func (h *Hub) Unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if s, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(s.ch)
	}
}

// Publish delivers e to all matching subscribers. Per-subscriber delivery is
// non-blocking: a full channel is silently dropped.
func (h *Hub) Publish(e protocol.LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, s := range h.subs {
		if s.project != "" && s.project != e.Project {
			continue
		}

		select {
		case s.ch <- e:
		default:
			if h.dropObserver != nil {
				h.dropObserver.ObserveDrop()
			}
		}
	}
}

// PublishUser emits a "user"-type entry directly. It is NOT redacted —
// user content comes from the human and is displayed verbatim.
func (h *Hub) PublishUser(project, cardID, content string) {
	h.Publish(protocol.LogEntry{
		Timestamp: time.Now(),
		Project:   project,
		CardID:    cardID,
		Type:      "user",
		Content:   content,
	})
}

// Bridge maps one worker output line to zero or one published LogEntry.
type Bridge struct {
	hub        *Hub
	redactor   *redact.Redactor
	onAwaiting func(project, cardID string, awaiting bool)
}

// New creates a Bridge. r may be nil (no redaction). onAwaiting may be nil.
func New(hub *Hub, r *redact.Redactor, onAwaiting func(project, cardID string, awaiting bool)) *Bridge {
	return &Bridge{hub: hub, redactor: r, onAwaiting: onAwaiting}
}

// BridgeLine maps one worker output line (stdout JSONL event or raw stderr)
// to zero or one published LogEntry, stamped with project/cardID/time.Now().
func (b *Bridge) BridgeLine(project, cardID string, line []byte, isStderr bool) {
	if isStderr {
		b.publish(protocol.LogEntry{
			Timestamp: time.Now(),
			Project:   project,
			CardID:    cardID,
			Type:      "stderr",
			Content:   b.redactor.Apply(string(line)),
		}, false)

		return
	}

	var ev struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		// Unparsable (e.g. panic stack trace) — surface as stderr.
		b.publish(protocol.LogEntry{
			Timestamp: time.Now(),
			Project:   project,
			CardID:    cardID,
			Type:      "stderr",
			Content:   b.redactor.Apply(string(line)),
		}, false)

		return
	}

	entry, awaiting, skip := b.mapEvent(ev.Kind, ev.Data)
	if skip {
		return
	}

	entry.Timestamp = time.Now()
	entry.Project = project
	entry.CardID = cardID
	entry.Content = b.redactor.Apply(entry.Content)

	b.publish(entry, awaiting)
}

// mapEvent converts a parsed event kind+data into a LogEntry.
// Returns skip=true for kinds that are deliberately not bridged.
// awaiting is only meaningful when skip=false.
func (b *Bridge) mapEvent(kind string, data map[string]any) (entry protocol.LogEntry, awaiting bool, skip bool) {
	switch kind {
	case "model_response":
		content := strField(data, "content")
		if strings.TrimSpace(content) == "" {
			// Pure tool-call turn — no text to show; skip the empty frame.
			return protocol.LogEntry{}, false, true
		}

		return protocol.LogEntry{
			Type:    "text",
			Content: content,
			Model:   strField(data, "model"),
		}, false, false

	case "thinking":
		// Model reasoning, emitted once per turn when the model produced it.
		// Content is redacted centrally by BridgeLine, so don't redact here.
		// It is agent progress, not an awaiting state: awaiting=false, skip=false.
		return protocol.LogEntry{
			Type:    "thinking",
			Content: strField(data, "content"),
		}, false, false

	case "tool_call":
		id := strField(data, "id")
		name := strField(data, "name")
		args := strField(data, "raw_args")

		content := truncate(name+"("+args+")", 200)

		return protocol.LogEntry{
			Type:      "tool_call",
			Content:   content,
			ToolUseID: id,
		}, false, false

	case "usage":
		inputTokens := int64Field(data, "prompt_tokens")
		outputTokens := int64Field(data, "completion_tokens")

		return protocol.LogEntry{
			Type:  "usage",
			Model: strField(data, "model"),
			Usage: &protocol.LogTokenUsage{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}, false, false

	case "state_change":
		if strField(data, "state") == "awaiting_human" {
			return protocol.LogEntry{
				Type:    "system",
				Content: "awaiting human input",
			}, true, false
		}

		return protocol.LogEntry{
			Type:    "system",
			Content: summarizeData(data),
		}, false, false

	case "context_limit":
		return protocol.LogEntry{
			Type:    "system",
			Content: summarizeData(data),
		}, false, false

	case "error":
		return protocol.LogEntry{
			Type:    "stderr",
			Content: strField(data, "error"),
		}, false, false

	case "discussion":
		// Mob session live transcript: briefing, round utterances, moderator
		// notices, synthesis — speaker-labeled via Agent. Seat sub-run
		// events arrive as "seat_debug" and fall through to the default
		// skip, keeping them off the live stream by construction.
		return protocol.LogEntry{
			Type:    "text",
			Content: strField(data, "content"),
			Agent:   strField(data, "agent"),
		}, false, false

	// Transcript-only kinds — not bridged.
	case "model_request", "tool_result", "tool_repair", "user_input", "verification":
		return protocol.LogEntry{}, false, true

	default:
		// Unknown future kinds: skip silently.
		return protocol.LogEntry{}, false, true
	}
}

// publish delivers e to the hub and fires the awaiting hook.
//
// stderr-typed entries (raw stderr, unparsable lines, and the error-kind
// mapping) never touch the awaiting flag: a parked HITL worker still logs to
// stderr (e.g. a heartbeat warning from its slog), and clearing awaiting on
// such a line would let the idle watchdog reap a container that is legitimately
// waiting for a human. Only real agent-progress entries (text, tool_call,
// usage, non-awaiting system) clear it; the awaiting_human system entry sets it.
func (b *Bridge) publish(e protocol.LogEntry, awaiting bool) {
	b.hub.Publish(e)

	if b.onAwaiting != nil && e.Type != "stderr" {
		b.onAwaiting(e.Project, e.CardID, awaiting)
	}
}

// strField extracts a string value from data, returning "" if absent or
// not a string.
func strField(data map[string]any, key string) string {
	if data == nil {
		return ""
	}

	v, ok := data[key]
	if !ok {
		return ""
	}

	s, _ := v.(string)

	return s
}

// int64Field extracts a numeric value (JSON numbers unmarshal as float64)
// from data, returning 0 if absent or not numeric.
func int64Field(data map[string]any, key string) int64 {
	if data == nil {
		return 0
	}

	v, ok := data[key]
	if !ok {
		return 0
	}

	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}

	return 0
}

// summarizeData produces a brief human-readable string from event data for
// system-type frames where no dedicated field carries the message.
func summarizeData(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}

	b, err := json.Marshal(data)
	if err != nil {
		return ""
	}

	return truncate(string(b), 200)
}

// truncate cuts s to at most limit bytes without splitting a multi-byte rune:
// the cut point backs off past any continuation bytes so the result is
// always valid UTF-8 (assuming s is).
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}
