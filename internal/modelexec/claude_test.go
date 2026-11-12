package modelexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// stubTool is a registry entry that only needs a name.
type stubTool struct{ name string }

func (s stubTool) Name() string     { return s.name }
func (s stubTool) Schema() llm.Tool { return llm.Tool{} }
func (stubTool) Execute(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

func stubRegistry(names ...string) *tools.Registry {
	ts := make([]tools.Tool, 0, len(names))
	for _, n := range names {
		ts = append(ts, stubTool{n})
	}

	return tools.NewRegistry(ts...)
}

// fakeRunner records every Spec and answers through script.
type fakeRunner struct {
	mu     sync.Mutex
	specs  []claudecode.Spec
	script func(claudecode.Spec) (harness.Result, error)
}

func (f *fakeRunner) run(_ context.Context, spec claudecode.Spec) (harness.Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()

	if f.script != nil {
		return f.script(spec)
	}

	return harness.Result{Completed: true, Reason: "done", Output: "ok"}, nil
}

func (f *fakeRunner) recorded() []claudecode.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]claudecode.Spec(nil), f.specs...)
}

func newTestClaude(t *testing.T, cfg ClaudeConfig, f *fakeRunner) *Claude {
	t.Helper()

	if cfg.OAuthToken == "" {
		cfg.OAuthToken = "tok"
	}

	c, err := newClaude(cfg, f.run)
	require.NoError(t, err)

	return c
}

func baseReq() orchestrator.RunRequest {
	return orchestrator.RunRequest{
		Registry: stubRegistry("read"),
		Dir:      "/ws",
		Task:     "do it",
		Emit:     events.NewEmitter(nil, nil),
		Config:   harness.Config{Model: "anthropic/claude-sonnet-5.5"},
	}
}

func TestClaudeRunMapsTheRequest(t *testing.T) {
	f := &fakeRunner{}
	cfg := ClaudeConfig{OAuthToken: "tok-1", Env: []string{"PATH=/usr/bin"}, Effort: "high", Home: "/home/w"}
	c := newTestClaude(t, cfg, f)

	emit := events.NewEmitter(nil, nil)
	imgs := []llm.ImageURL{{URL: "data:image/png;base64,AA"}}
	deltaCalled := false
	redact := func(s string) string { return s }

	req := orchestrator.RunRequest{
		Registry:  stubRegistry("read", "write", "edit", "bash", "grep", "glob", "git"),
		Dir:       "/ws",
		Task:      "do it",
		Emit:      emit,
		SessionID: "s1",
		Config: harness.Config{
			Model: "anthropic/claude-sonnet-5.5", SystemPrompt: "seat persona", MaxTurns: 12, MaxCostUSD: 1.5,
			TaskImages: imgs, OnDelta: func(llm.Delta) { deltaCalled = true }, RedactToolOutput: redact,
		},
	}
	reg := tools.NewRegistry(append(req.Registry.All(), orchestrator.NewFinishTool())...)
	req.Registry = reg

	_, err := c.Run(t.Context(), req)
	require.NoError(t, err)

	specs := f.recorded()
	require.Len(t, specs, 1)
	s := specs[0]

	assert.Equal(t, "/ws", s.Dir)
	assert.Equal(t, "/home/w", s.Home)
	assert.Equal(t, "tok-1", s.OAuthToken)
	assert.Equal(t, cfg.Env, s.Env)
	assert.Equal(t, "claude-sonnet-5-5", s.Model)
	assert.Equal(t, "do it", s.Task)
	assert.Equal(t, imgs, s.TaskImages)
	assert.Equal(t, "seat persona", s.SystemPromptAppend)
	assert.Empty(t, s.SystemPrompt)
	assert.ElementsMatch(t, []string{"Read", "Write", "Edit", "Bash"}, s.BuiltinTools)

	require.NotNil(t, s.Tools)

	for _, n := range []string{"grep", "glob", "git", "finish"} {
		_, ok := s.Tools.Get(n)
		assert.True(t, ok, n)
	}

	_, ok := s.Tools.Get("read")
	assert.False(t, ok)

	assert.Equal(t, 12, s.MaxTurns)
	assert.InDelta(t, 1.5, s.MaxBudgetUSD, 1e-9)
	assert.Equal(t, claudecode.NormalizeEffort("high"), s.Effort)
	assert.Equal(t, "s1", s.SessionID)
	assert.Empty(t, s.Resume)
	assert.Same(t, emit, s.Emitter)
	assert.NotNil(t, s.Redact)
	require.NotNil(t, s.OnDelta)
	assert.NotNil(t, s.OnRateLimit)

	s.OnDelta(llm.Delta{})
	assert.True(t, deltaCalled)
}

func TestClaudeRunPrimesSeededHistory(t *testing.T) {
	h := []llm.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}

	f := &fakeRunner{}
	c := newTestClaude(t, ClaudeConfig{}, f)

	req := baseReq()
	req.Config.History = h
	_, err := c.Run(t.Context(), req)
	require.NoError(t, err)

	task := f.recorded()[0].Task
	assert.True(t, strings.HasPrefix(task, claudecode.RenderTranscript(h)))
	assert.True(t, strings.HasSuffix(task, "do it"))

	f2 := &fakeRunner{}
	c2 := newTestClaude(t, ClaudeConfig{}, f2)
	req.Resume = "r1"
	_, err = c2.Run(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, "do it", f2.recorded()[0].Task)
}

func TestClaudeRunKeepsGrounding(t *testing.T) {
	task := "## Repository instructions (AGENTS.md)\n\nGROUNDING-MARKER-7f3\n\n## Task\n\ndo it"
	h := []llm.Message{{Role: "user", Content: "a"}}

	f := &fakeRunner{}
	c := newTestClaude(t, ClaudeConfig{}, f)

	req := baseReq()
	req.Task = task
	_, err := c.Run(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, task, f.recorded()[0].Task)

	req.Config.History = h
	_, err = c.Run(t.Context(), req)
	require.NoError(t, err)

	primed := f.recorded()[1].Task
	assert.True(t, strings.HasPrefix(primed, claudecode.RenderTranscript(h)))
	assert.True(t, strings.HasSuffix(primed, task))
}

func TestClaudeRunServesSkillTool(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "demo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo", "SKILL.md"),
		[]byte("---\nname: demo\ndescription: a demo skill\n---\nbody\n"), 0o644))

	skill, ok := tools.NewSkillTool(dir, nil, false, nil)
	require.True(t, ok)

	f := &fakeRunner{}
	c := newTestClaude(t, ClaudeConfig{}, f)

	req := baseReq()
	req.Registry = tools.NewRegistry(stubTool{"read"}, skill)
	_, err := c.Run(t.Context(), req)
	require.NoError(t, err)

	s := f.recorded()[0]
	require.NotNil(t, s.Tools)

	_, ok = s.Tools.Get("skill")
	assert.True(t, ok)
	assert.NotContains(t, s.BuiltinTools, "skill")

	req.Registry = stubRegistry("read", "grep")
	_, err = c.Run(t.Context(), req)
	require.NoError(t, err)

	s = f.recorded()[1]
	require.NotNil(t, s.Tools)

	_, ok = s.Tools.Get("skill")
	assert.False(t, ok)
}

func TestClaudeRunRejectsAModelWithoutAClaudeCodeID(t *testing.T) {
	f := &fakeRunner{}
	c := newTestClaude(t, ClaudeConfig{}, f)

	req := baseReq()
	req.Config.Model = "openai/gpt-6"
	_, err := c.Run(t.Context(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "openai/gpt-6")
	assert.Empty(t, f.recorded())
}

func TestClaudeRunEmptyCompletionIsAnEmptyResponse(t *testing.T) {
	tests := []struct {
		name    string
		res     harness.Result
		wantErr bool
	}{
		{"blank output", harness.Result{Completed: true, Reason: "done", Output: "  "}, true},
		{"completion args", harness.Result{Completed: true, Reason: "done", CompletionArgs: json.RawMessage(`{}`)}, false},
		{"output", harness.Result{Completed: true, Reason: "done", Output: "x"}, false},
		{"not completed", harness.Result{Reason: "max_turns"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{script: func(claudecode.Spec) (harness.Result, error) { return tt.res, nil }}
			c := newTestClaude(t, ClaudeConfig{}, f)

			_, err := c.Run(t.Context(), baseReq())
			if tt.wantErr {
				require.ErrorIs(t, err, harness.ErrEmptyResponse)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestClaudeRunConvertsTheUsageLimit(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	f := &fakeRunner{script: func(claudecode.Spec) (harness.Result, error) {
		return harness.Result{Reason: "error", TotalCostUSD: 0.2},
			fmt.Errorf("w: %w", &claudecode.UsageLimitError{Window: "seven_day", ResetsAt: t0})
	}}
	c := newTestClaude(t, ClaudeConfig{}, f)

	res, err := c.Run(t.Context(), baseReq())

	var ule *orchestrator.UsageLimitError
	require.ErrorAs(t, err, &ule)
	assert.Equal(t, "seven_day", ule.Window)
	assert.True(t, ule.ResetsAt.Equal(t0))
	assert.InDelta(t, 0.2, res.TotalCostUSD, 1e-9)
}

func TestClaudeRunPassesNotLoggedInThrough(t *testing.T) {
	f := &fakeRunner{script: func(claudecode.Spec) (harness.Result, error) {
		return harness.Result{}, fmt.Errorf("x: %w", claudecode.ErrNotLoggedIn)
	}}
	c := newTestClaude(t, ClaudeConfig{}, f)

	_, err := c.Run(t.Context(), baseReq())
	require.ErrorIs(t, err, claudecode.ErrNotLoggedIn)
}

func TestClaudeKeepsTheLastRateLimitWindows(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	a := []protocol.SubscriptionUsage{{Window: "five_hour", Utilization: 0.1, ResetsAt: t0}}
	b := []protocol.SubscriptionUsage{{Window: "seven_day", Utilization: 0.3, ResetsAt: t0}}

	f := &fakeRunner{}
	c := newTestClaude(t, ClaudeConfig{}, f)
	assert.Nil(t, c.SubscriptionUsage())

	f.script = func(s claudecode.Spec) (harness.Result, error) {
		s.OnRateLimit(claudecode.RateLimit{Windows: a})
		s.OnRateLimit(claudecode.RateLimit{Windows: b})

		return harness.Result{Completed: true, Output: "x"}, nil
	}

	_, err := c.Run(t.Context(), baseReq())
	require.NoError(t, err)
	assert.Equal(t, b, c.SubscriptionUsage())

	f.script = func(s claudecode.Spec) (harness.Result, error) {
		s.OnRateLimit(claudecode.RateLimit{Windows: []protocol.SubscriptionUsage{
			{Window: "seven_day", Utilization: 0.1, ResetsAt: t0},
			{Window: "five_hour", Utilization: 0.2},
		}})

		return harness.Result{Completed: true, Output: "x"}, nil
	}

	_, err = c.Run(t.Context(), baseReq())
	require.NoError(t, err)

	got := c.SubscriptionUsage()
	require.Len(t, got, 1)
	assert.Equal(t, "seven_day", got[0].Window)
}

func TestClaudeSpawnSubagentsRunsOneRunPerSpec(t *testing.T) {
	f := &fakeRunner{script: func(s claudecode.Spec) (harness.Result, error) {
		if strings.Contains(s.Task, "prompt-b") {
			return harness.Result{}, errors.New("boom")
		}

		return harness.Result{Completed: true, Reason: "done", Output: s.Task}, nil
	}}
	c := newTestClaude(t, ClaudeConfig{}, f)

	root := t.TempDir()
	extra := stubTool{"extra"}
	req := orchestrator.FanoutRequest{
		Root: root,
		Emit: events.NewEmitter(nil, nil),
		Specs: []harness.SubagentSpec{
			{Role: "a", Prompt: "prompt-a", MaxTurns: 3},
			{Role: "b", Prompt: "prompt-b", Model: "anthropic/claude-opus-5.5"},
			{Role: "c", Prompt: "prompt-c"},
		},
		Opts:      harness.SubagentOpts{DefaultModel: "anthropic/claude-sonnet-5.5", ExtraReadOnlyTools: []tools.Tool{extra}},
		OnDelta:   func(llm.Delta) {},
		BudgetUSD: 0.7,
	}

	results, err := c.SpawnSubagents(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, results, 3)

	assert.Equal(t, []string{"a", "b", "c"}, []string{results[0].Role, results[1].Role, results[2].Role})
	require.Error(t, results[1].Err)
	assert.NoError(t, results[0].Err)
	assert.NoError(t, results[2].Err)
	assert.Equal(t, "prompt-a", results[0].Output)

	specs := f.recorded()
	require.Len(t, specs, 3)

	for _, s := range specs {
		assert.Equal(t, root, s.Dir)
		assert.ElementsMatch(t, []string{"Read"}, s.BuiltinTools)
		require.NotNil(t, s.Tools)

		for _, n := range []string{"grep", "glob", "git", "extra"} {
			_, ok := s.Tools.Get(n)
			assert.True(t, ok, n)
		}

		assert.InDelta(t, 0.7, s.MaxBudgetUSD, 1e-9)
		assert.NotNil(t, s.OnDelta)
	}
}

func TestNewClaudeDefaultsHomeToTheUsersHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	f := &fakeRunner{}
	c := newTestClaude(t, ClaudeConfig{}, f)
	_, err := c.Run(t.Context(), baseReq())
	require.NoError(t, err)
	assert.Equal(t, dir, f.recorded()[0].Home)
	assert.DirExists(t, dir)

	f2 := &fakeRunner{}
	c2 := newTestClaude(t, ClaudeConfig{Home: "/explicit"}, f2)
	_, err = c2.Run(t.Context(), baseReq())
	require.NoError(t, err)
	assert.Equal(t, "/explicit", f2.recorded()[0].Home)
}

func TestNewClaudeRequiresAToken(t *testing.T) {
	_, err := NewClaude(ClaudeConfig{})
	require.Error(t, err)
}

func TestClaudeEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://p:3128")
	t.Setenv("LLM_OAUTH_TOKEN", "secret-000000")

	env := ClaudeEnv([]string{"PATH=/usr/bin"}, "/run/cm-ca/ca.crt", 0)
	assert.Contains(t, env, "PATH=/usr/bin")
	assert.Contains(t, env, "HTTPS_PROXY=http://p:3128")
	assert.Contains(t, env, "NODE_EXTRA_CA_CERTS=/run/cm-ca/ca.crt")

	seen := map[string]bool{}

	for _, e := range env {
		assert.NotContains(t, e, "secret-000000")

		k, _, _ := strings.Cut(e, "=")
		assert.False(t, seen[k], k)
		seen[k] = true
	}

	for _, e := range ClaudeEnv([]string{"PATH=/usr/bin"}, "", 0) {
		assert.False(t, strings.HasPrefix(e, "NODE_EXTRA_CA_CERTS="))
	}

	dup := ClaudeEnv([]string{"PATH=/usr/bin", "HTTPS_PROXY=http://old:1"}, "", 0)
	assert.Contains(t, dup, "HTTPS_PROXY=http://p:3128")
	assert.NotContains(t, dup, "HTTPS_PROXY=http://old:1")
}

func TestClaudeEnvBashTimeouts(t *testing.T) {
	tests := []struct {
		name        string
		seconds     int
		wantMax     string
		wantDefault string
	}{
		{"above the default", 600, "BASH_MAX_TIMEOUT_MS=600000", ""},
		{"below the default", 60, "BASH_MAX_TIMEOUT_MS=60000", "BASH_DEFAULT_TIMEOUT_MS=60000"},
		{"at the default", 120, "BASH_MAX_TIMEOUT_MS=120000", ""},
		{"unset", 0, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := ClaudeEnv([]string{"PATH=/usr/bin"}, "", tt.seconds)

			var gotMax, gotDefault string

			for _, e := range env {
				switch {
				case strings.HasPrefix(e, "BASH_MAX_TIMEOUT_MS="):
					gotMax = e
				case strings.HasPrefix(e, "BASH_DEFAULT_TIMEOUT_MS="):
					gotDefault = e
				}
			}

			assert.Equal(t, tt.wantMax, gotMax)
			assert.Equal(t, tt.wantDefault, gotDefault)
		})
	}
}

func TestClaudeRunNormalizesEffort(t *testing.T) {
	f := &fakeRunner{}
	c := newTestClaude(t, ClaudeConfig{Effort: "high"}, f)
	_, err := c.Run(t.Context(), baseReq())
	require.NoError(t, err)
	assert.Equal(t, claudecode.NormalizeEffort("high"), f.recorded()[0].Effort)

	f2 := &fakeRunner{}
	c2 := newTestClaude(t, ClaudeConfig{Effort: "minimal"}, f2)
	_, err = c2.Run(t.Context(), baseReq())
	require.NoError(t, err)
	assert.Empty(t, f2.recorded()[0].Effort)
}

func TestCheckClaudeCode(t *testing.T) {
	tests := []struct {
		name     string
		found    string
		tested   bool
		checkErr error
		wantErr  bool
		wantWarn bool
	}{
		{"tested", claudecode.Version, true, nil, false, false},
		{"different version", "2.0.1", false, nil, false, true},
		{"missing", "", false, exec.ErrNotFound, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			logger := slog.New(slog.NewTextHandler(&buf, nil))
			err := checkClaudeCode(t.Context(), logger, func(context.Context, string) (string, bool, error) {
				return tt.found, tt.tested, tt.checkErr
			})

			if tt.wantErr {
				require.ErrorIs(t, err, exec.ErrNotFound)
				assert.Contains(t, err.Error(), claudecode.Version)

				return
			}

			require.NoError(t, err)

			if tt.wantWarn {
				assert.Contains(t, buf.String(), "level=WARN")
				assert.Contains(t, buf.String(), tt.found)
				assert.Contains(t, buf.String(), claudecode.Version)
			} else {
				assert.NotContains(t, buf.String(), "WARN")
				assert.Contains(t, buf.String(), "level=INFO")
				assert.Contains(t, buf.String(), tt.found)
			}
		})
	}
}

// scriptedTool returns a fixed result and optionally ends the run.
type scriptedTool struct {
	name     string
	res      tools.Result
	err      error
	terminal bool
}

func (s scriptedTool) Name() string     { return s.name }
func (s scriptedTool) Schema() llm.Tool { return llm.Tool{} }
func (s scriptedTool) Execute(context.Context, map[string]any) (tools.Result, error) {
	return s.res, s.err
}
func (s scriptedTool) Terminal() bool { return s.terminal }

const bridgeSecret = "SYNTHETIC-SECRET-MARKER"

func redactSecret(s string) string { return strings.ReplaceAll(s, bridgeSecret, "[REDACTED]") }

func bridgedSpec(t *testing.T, cfg harness.Config, reg *tools.Registry) claudecode.Spec {
	t.Helper()

	f := &fakeRunner{}
	c := newTestClaude(t, ClaudeConfig{}, f)

	req := baseReq()
	req.Registry = reg
	cfg.Model = req.Config.Model
	req.Config = cfg

	_, err := c.Run(t.Context(), req)
	require.NoError(t, err)

	specs := f.recorded()
	require.Len(t, specs, 1)
	require.NotNil(t, specs[0].Tools)

	return specs[0]
}

func TestClaudeBridgedToolOutputIsRedactedAndCapped(t *testing.T) {
	const limit = 200

	body := bridgeSecret + strings.Repeat("x", 5000) + "TAIL-END"
	reg := tools.NewRegistry(scriptedTool{name: "grep", res: tools.Result{Text: body}})
	spec := bridgedSpec(t, harness.Config{RedactToolOutput: redactSecret, ToolOutputMaxBytes: limit}, reg)

	tool, ok := spec.Tools.Get("grep")
	require.True(t, ok)

	res, err := tool.Execute(t.Context(), nil)
	require.NoError(t, err)

	assert.NotContains(t, res.Text, bridgeSecret)
	assert.Equal(t, tools.HeadTail(redactSecret(body), limit), res.Text)
	assert.Less(t, len(res.Text), len(body))
	assert.Contains(t, res.Text, "[REDACTED]")
	assert.True(t, strings.HasSuffix(res.Text, "TAIL-END"))
}

func TestClaudeBridgedToolErrorIsRedacted(t *testing.T) {
	inner := errors.New("failed with " + bridgeSecret)
	reg := tools.NewRegistry(scriptedTool{name: "grep", err: inner})
	spec := bridgedSpec(t, harness.Config{RedactToolOutput: redactSecret}, reg)

	tool, _ := spec.Tools.Get("grep")
	_, err := tool.Execute(t.Context(), nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), bridgeSecret)
	require.NotErrorIs(t, err, inner)

	clean := fmt.Errorf("wrapped: %w", os.ErrNotExist)
	reg = tools.NewRegistry(scriptedTool{name: "grep", err: clean})
	spec = bridgedSpec(t, harness.Config{RedactToolOutput: redactSecret}, reg)

	tool, _ = spec.Tools.Get("grep")
	_, err = tool.Execute(t.Context(), nil)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestClaudeBridgedToolKeepsTerminal(t *testing.T) {
	reg := tools.NewRegistry(
		scriptedTool{name: "finish", terminal: true},
		scriptedTool{name: "grep"},
		stubTool{"plain"},
	)
	spec := bridgedSpec(t, harness.Config{}, reg)

	for name, want := range map[string]bool{"finish": true, "grep": false, "plain": false} {
		tool, ok := spec.Tools.Get(name)
		require.True(t, ok, name)

		term, ok := tool.(tools.Terminal)
		require.True(t, ok, name)
		assert.Equal(t, want, term.Terminal(), name)
	}
}

func TestClaudeBridgedToolWithoutRedactorOrCapIsUnchanged(t *testing.T) {
	body := strings.Repeat("y", 1000)
	reg := tools.NewRegistry(scriptedTool{name: "grep", res: tools.Result{Text: body}})
	spec := bridgedSpec(t, harness.Config{}, reg)

	tool, _ := spec.Tools.Get("grep")
	res, err := tool.Execute(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, body, res.Text)
}

func TestClaudeSpawnSubagentsCapsBridgedOutput(t *testing.T) {
	const limit = 100

	f := &fakeRunner{}
	c := newTestClaude(t, ClaudeConfig{}, f)

	body := bridgeSecret + strings.Repeat("z", 3000)
	_, err := c.SpawnSubagents(t.Context(), orchestrator.FanoutRequest{
		Root:  t.TempDir(),
		Emit:  events.NewEmitter(nil, nil),
		Specs: []harness.SubagentSpec{{Role: "a", Prompt: "p"}},
		Opts: harness.SubagentOpts{
			DefaultModel:       "anthropic/claude-sonnet-5.5",
			ExtraReadOnlyTools: []tools.Tool{scriptedTool{name: "extra", res: tools.Result{Text: body}}},
			ToolOutputMaxBytes: limit,
			RedactToolOutput:   redactSecret,
		},
	})
	require.NoError(t, err)

	specs := f.recorded()
	require.Len(t, specs, 1)

	tool, ok := specs[0].Tools.Get("extra")
	require.True(t, ok)

	res, err := tool.Execute(t.Context(), nil)
	require.NoError(t, err)
	assert.NotContains(t, res.Text, bridgeSecret)
	assert.Equal(t, tools.HeadTail(redactSecret(body), limit), res.Text)
}
