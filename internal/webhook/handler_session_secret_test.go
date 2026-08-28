package webhook

import (
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSessionSecretRegistry records every AddSessionKey per session id as a
// slice so clobber is visible. Satisfies SessionSecretRegistry.
type fakeSessionSecretRegistry struct {
	mu       sync.Mutex
	sessions map[string][]string // id → keys in registration order
	removed  []string            // ids passed to RemoveSessionKey, in order
}

func newFakeSessionSecretRegistry() *fakeSessionSecretRegistry {
	return &fakeSessionSecretRegistry{
		sessions: make(map[string][]string),
	}
}

func (f *fakeSessionSecretRegistry) AddSessionKey(id, key string) {
	if key == "" {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.sessions[id] = append(f.sessions[id], key)
}

func (f *fakeSessionSecretRegistry) RemoveSessionKey(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.removed = append(f.removed, id)
	delete(f.sessions, id)
}

func (f *fakeSessionSecretRegistry) keys(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.sessions[id]))
	copy(out, f.sessions[id])

	return out
}

func (f *fakeSessionSecretRegistry) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.removed))
	copy(out, f.removed)

	return out
}

// TestAddSessionSecrets_FullyProvisionedTrigger registers a payload with a git
// token, an LLM endpoint key, a payload MCP key override, and two mob guest
// tokens, and asserts all four classes of secret land under the correct id.
func TestAddSessionSecrets_FullyProvisionedTrigger(t *testing.T) {
	registry := newFakeSessionSecretRegistry()

	s := NewServer(Config{
		APIKey:         "k",
		Executor:       &fakeExecutor{},
		Tracker:        executor.NewTracker(1),
		SessionSecrets: registry,
		LaunchEnv: LaunchEnv{
			BaseImage: "img",
			MCPURL:    "http://mcp",
			MCPAPIKey: "cfg-mcp-key",
		},
		Logger: quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:   "C1",
		Project:  "p",
		GitToken: "cm-git-token",

		LLMEndpoint: &protocol.LLMEndpoint{
			Type:    "openai",
			BaseURL: "https://llm.example/v1",
			APIKey:  "cm-llm-key",
		},
		MCPAPIKey: "payload-mcp-key",
		Mob: &protocol.MobSpec{
			Participants: 3,
			Guests: []protocol.GuestSpec{
				{Name: "laptop", URL: "http://10.0.0.5:8484", Token: "guest-token-1"},
				{Name: "phone", URL: "http://10.0.0.6:8484", Token: "guest-token-2"},
			},
		},
	}

	s.addSessionSecrets("p", "C1", "corr-1", payload)

	got := registry.keys(SessionID("p", "C1", "corr-1"))
	require.Len(t, got, 5, "five entries: git token, llm key, mcp key, and two guest tokens (one entry each)")

	// Check each class is present (order is append-order from addSessionSecrets)
	assert.Contains(t, got, "cm-git-token", "git token must be registered")
	assert.Contains(t, got, "cm-llm-key", "LLM endpoint key must be registered")
	assert.Contains(t, got, "payload-mcp-key", "payload MCP key override must be registered")
	assert.Contains(t, got, "guest-token-1", "first mob guest token must be registered")
	assert.Contains(t, got, "guest-token-2", "second mob guest token must be registered")
}

// TestAddSessionSecrets_UsesConfigMCPKey verifies that when the payload does
// not override mcp_api_key, the config-level MCP key is registered instead.
func TestAddSessionSecrets_UsesConfigMCPKey(t *testing.T) {
	registry := newFakeSessionSecretRegistry()

	s := NewServer(Config{
		APIKey:         "k",
		Executor:       &fakeExecutor{},
		Tracker:        executor.NewTracker(1),
		SessionSecrets: registry,
		LaunchEnv: LaunchEnv{
			BaseImage: "img",
			MCPURL:    "http://mcp",
			MCPAPIKey: "cfg-mcp-key",
		},
		Logger: quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:      "C1",
		Project:     "p",
		GitToken:    "cm-git-token",
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
		// No MCPAPIKey override - should use config default
	}

	s.addSessionSecrets("p", "C1", "corr-1", payload)

	got := registry.keys(SessionID("p", "C1", "corr-1"))
	assert.Contains(t, got, "cfg-mcp-key", "config-level MCP key must be registered when no payload override")
}

// TestAddSessionSecrets_NilEndpointAndMob handles a nil LLM endpoint and a nil
// mob without panicking and without registering bogus keys.
func TestAddSessionSecrets_NilEndpointAndMob(t *testing.T) {
	registry := newFakeSessionSecretRegistry()

	s := NewServer(Config{
		APIKey:         "k",
		Executor:       &fakeExecutor{},
		Tracker:        executor.NewTracker(1),
		SessionSecrets: registry,
		LaunchEnv:      LaunchEnv{BaseImage: "img", MCPURL: "http://mcp", MCPAPIKey: "cfg-mcp-key"},
		Logger:         quietLogger(),
	})

	// No LLMEndpoint, no Mob, still has a git token and config MCP key
	payload := protocol.TriggerPayload{
		CardID:   "C1",
		Project:  "p",
		GitToken: "cm-git-token",
	}

	s.addSessionSecrets("p", "C1", "corr-1", payload)

	got := registry.keys(SessionID("p", "C1", "corr-1"))
	require.Len(t, got, 2, "git token and config MCP key only")
	assert.Contains(t, got, "cm-git-token")
	assert.Contains(t, got, "cfg-mcp-key")
}

// TestAddSessionSecrets_NilRegistryIsNoOp verifies a nil registry never panics
// when calling addSessionSecrets or removeSessionSecrets.
func TestAddSessionSecrets_NilRegistryIsNoOp(t *testing.T) {
	s := NewServer(Config{
		APIKey:    "k",
		Executor:  &fakeExecutor{},
		Tracker:   executor.NewTracker(1),
		LaunchEnv: LaunchEnv{BaseImage: "img", MCPURL: "http://mcp"},
		Logger:    quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:      "C1",
		Project:     "p",
		GitToken:    "cm-git-token",
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
		Mob: &protocol.MobSpec{
			Participants: 2,
			Guests:       []protocol.GuestSpec{{Name: "laptop", URL: "http://10.0.0.5:8484", Token: "guest-token"}},
		},
	}

	// Must not panic
	s.addSessionSecrets("p", "C1", "corr-1", payload)
	s.removeSessionSecrets("p", "C1", "corr-1")
}

// TestRemoveSessionSecrets verifies the container-exit path removes the session
// id from the registry.
func TestRemoveSessionSecrets(t *testing.T) {
	registry := newFakeSessionSecretRegistry()

	s := NewServer(Config{
		APIKey:         "k",
		Executor:       &fakeExecutor{},
		Tracker:        executor.NewTracker(1),
		SessionSecrets: registry,
		LaunchEnv:      LaunchEnv{BaseImage: "img", MCPURL: "http://mcp", MCPAPIKey: "cfg-mcp-key"},
		Logger:         quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:      "C1",
		Project:     "p",
		GitToken:    "cm-git-token",
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
	}

	s.addSessionSecrets("p", "C1", "corr-1", payload)
	require.Len(t, registry.keys(SessionID("p", "C1", "corr-1")), 3, "secrets registered")

	s.removeSessionSecrets("p", "C1", "corr-1")

	assert.Empty(t, registry.keys(SessionID("p", "C1", "corr-1")), "secrets removed after session end")
	assert.Contains(t, registry.removedIDs(), SessionID("p", "C1", "corr-1"), "remove call recorded")
}

// TestRemoveSessionSecrets_DifferentCorrelationIDDoesNotCollide pins the
// keying half of the fix at the handler layer: removeSessionSecrets for a
// stale run's correlation id must not remove a second, still-live run's
// secrets for the SAME project/card registered under a different correlation
// id. Before the fix both runs shared the id "p/C1" and this would have wiped
// run 2's secrets too.
func TestRemoveSessionSecrets_DifferentCorrelationIDDoesNotCollide(t *testing.T) {
	registry := newFakeSessionSecretRegistry()

	s := NewServer(Config{
		APIKey:         "k",
		Executor:       &fakeExecutor{},
		Tracker:        executor.NewTracker(1),
		SessionSecrets: registry,
		LaunchEnv:      LaunchEnv{BaseImage: "img", MCPURL: "http://mcp", MCPAPIKey: "cfg-mcp-key"},
		Logger:         quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:      "C1",
		Project:     "p",
		GitToken:    "cm-git-token",
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
	}

	// Run 1 registers, then is superseded by a re-trigger (run 2) of the same
	// card before its own exit-path removal fires.
	s.addSessionSecrets("p", "C1", "corr-1", payload)
	s.addSessionSecrets("p", "C1", "corr-2", payload)

	// Run 1's stale exit removes only its own bucket.
	s.removeSessionSecrets("p", "C1", "corr-1")

	assert.Empty(t, registry.keys(SessionID("p", "C1", "corr-1")), "run 1's own secrets are removed")
	assert.NotEmpty(t, registry.keys(SessionID("p", "C1", "corr-2")),
		"run 2's secrets must survive run 1's stale removal")
}

// TestMintRunID_ProducesDistinctIDsForTheSameCardID pins the primitive
// handleTrigger's X-Correlation-ID fallback relies on: since CM does not send
// that header today, mintRunID is what actually gives two re-triggers of the
// same card distinct redaction-session ids. A cardID-only fallback (the
// pre-fix behavior) would return the identical string both times.
func TestMintRunID_ProducesDistinctIDsForTheSameCardID(t *testing.T) {
	first := mintRunID("CARD-1")
	second := mintRunID("CARD-1")

	assert.NotEqual(t, first, second, "two mints for the same card must not collide")
	assert.Contains(t, first, "CARD-1", "the minted id stays traceable to its card")
	assert.Contains(t, second, "CARD-1")
}

// TestFailedLaunchRemovesSessionSecrets verifies that when the executor launch
// fails, the session secrets are removed via admitAndLaunch's teardown path.
func TestFailedLaunchRemovesSessionSecrets(t *testing.T) {
	registry := newFakeSessionSecretRegistry()

	s := NewServer(Config{
		APIKey:         "k",
		Executor:       &fakeExecutor{launchErr: assert.AnError},
		Tracker:        executor.NewTracker(1),
		Credentials:    &fakeCredentials{},
		Reporter:       &fakeReporter{},
		SessionSecrets: registry,
		LaunchEnv: LaunchEnv{
			BaseImage: "img",
			MCPURL:    "http://mcp",
			MCPAPIKey: "cfg-mcp-key",
		},
		Logger: quietLogger(),
	})

	payload := protocol.TriggerPayload{
		CardID:      "C1",
		Project:     "p",
		GitToken:    "cm-git-token",
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openai", APIKey: "cm-llm-key"},
	}

	// Use the launch goroutine which goes through admitAndLaunch
	s.launch(s.buildLaunchSpec(payload, "corr", ""), payload)

	assert.Empty(t, registry.keys(SessionID("p", "C1", "corr")), "secrets removed after launch failure")
	assert.Contains(t, registry.removedIDs(), SessionID("p", "C1", "corr"), "remove call recorded on launch failure")
}

// TestRebuildTrap_StaticKeyStillMaskedAfterRunRegisters validates the interface
// contract: after a run is registered AND the static config-level keys (the
// API key, MCP key) were registered independently (simulating the serve.go
// wiring), a line containing the static API key is still masked. This mimics
// what happens when serve.go registers cfg.APIKey and cfg.MCPAPIKey under a
// reserved id, and then per-run secrets are added - the static keys must
// survive the rebuild.
//
// The assertion runs through the real path: a hub subscriber receives the entry
// BridgeLine publishes, so it pins the masking the /logs stream actually gets.
func TestRebuildTrap_StaticKeyStillMaskedAfterRunRegisters(t *testing.T) {
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, nil)
	bridge := logbridge.NewBridge(logbridge.BridgeConfig{
		Hub:                  hub,
		Redactor:             nil, // start with no redactor
		SurfaceAwaitingHuman: false,
	})

	registry := logbridge.NewRedactorRegistry(bridge)

	// Register static config-level keys (simulating the serve.go wiring with a
	// reserved pseudo-session id as described in the parent card).
	registry.AddSessionKey("__static__", "cfg-static-api-key")
	registry.AddSessionKey("__static__", "cfg-mcp-api-key")

	// Now register a per-run secret (simulating what addSessionSecrets does).
	registry.AddSessionKey("p/C1", "run-secret-123")

	// The bridge's redactor must carry all three keys after the per-run add.
	// The Bridge exposes no redactor getter, so assert the effect: publish a
	// line through BridgeLine and read what the hub subscriber receives.
	subID, ch := hub.Subscribe("p")
	defer hub.Unsubscribe(subID)

	bridge.BridgeLine(
		logbridge.Key{Project: "p", CardID: "C1"},
		[]byte("line containing cfg-static-api-key and run-secret-123 and cfg-mcp-api-key"),
		true, // stderr - directly redacted
	)

	select {
	case entry := <-ch:
		assert.NotContains(t, entry.Content, "cfg-static-api-key", "static API key must still be masked after per-run add")
		assert.NotContains(t, entry.Content, "run-secret-123", "per-run secret must be masked")
		assert.NotContains(t, entry.Content, "cfg-mcp-api-key", "static MCP key must still be masked after per-run add")
		assert.Contains(t, entry.Content, "[REDACTED]", "masked values replaced")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bridged entry")
	}
}
