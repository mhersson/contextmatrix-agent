package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/filelog"
	"github.com/mhersson/contextmatrix-agent/internal/runinput"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

func checkPayload() protocol.RunKindPayload {
	return protocol.RunKindPayload{
		Kind: protocol.RunKindCheck, CardID: "ABC-12", Project: "proj",
		RepoURL: "https://example.com/r.git", GitToken: "git-tok",
		Check: &protocol.CheckInput{CurrentCommand: "./ci/verify", Text: "run ./ci/verify"},
	}
}

func investigatePayload() protocol.RunKindPayload {
	return protocol.RunKindPayload{
		Kind: protocol.RunKindInvestigate, CardID: "ABC-12", Project: "proj",
		RepoURL: "https://example.com/r.git", GitToken: "git-tok",
		MaxCostUSD: 0.5, Model: "vendor/model-a",
		Selection:   &protocol.SelectionContext{Blacklist: []string{"vendor/bad"}},
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openai", BaseURL: "https://llm.example/v1", APIKey: "cm-llm-key"},
		Investigate: &protocol.InvestigateInput{RunID: "ABC-12-x", Card: "c"},
	}
}

// runKindNow launches p synchronously, as the goroutine handleRunKind starts
// would, and returns the spec it built.
func (h *harness) runKindNow(p protocol.RunKindPayload) executor.LaunchSpec {
	spec := h.server.buildRunKindSpec(p, mintRunID(p.CardID+"-"+p.Kind))
	adm, ctx := h.server.beginAdmission(context.Background(), spec.Key())
	h.server.launchRunKind(ctx, adm, p, spec)

	return spec
}

// postRaw signs and posts body as is to /run-kind.
func (h *harness) postRaw(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/run-kind", bytes.NewReader(body))
	signReq(t, r, testAPIKey, body, nowTS())

	return h.serve(r)
}

func (h *harness) inputDirs(t *testing.T) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(h.inputRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	require.NoError(t, err)

	return entries
}

func TestRunKindInvalidField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *protocol.RunKindPayload)
		base   func() protocol.RunKindPayload
		want   string
	}{
		{"valid check", func(*protocol.RunKindPayload) {}, checkPayload, ""},
		{"valid investigate", func(*protocol.RunKindPayload) {}, investigatePayload, ""},
		{"reserved analyse", func(p *protocol.RunKindPayload) { p.Kind = protocol.RunKindAnalyse }, checkPayload, "kind"},
		{"reserved guidance_pr", func(p *protocol.RunKindPayload) { p.Kind = protocol.RunKindGuidancePR }, checkPayload, "kind"},
		{"unknown kind", func(p *protocol.RunKindPayload) { p.Kind = "bogus" }, checkPayload, "kind"},
		{"card id outside the pattern", func(p *protocol.RunKindPayload) { p.CardID = "../x" }, checkPayload, "card_id"},
		{"no git token", func(p *protocol.RunKindPayload) { p.GitToken = "" }, checkPayload, "git_token"},
		{"check without its input", func(p *protocol.RunKindPayload) { p.Check = nil }, checkPayload, "check"},
		{"check with an investigate input", func(p *protocol.RunKindPayload) { p.Investigate = &protocol.InvestigateInput{} }, checkPayload, "investigate"},
		{"investigate without its input", func(p *protocol.RunKindPayload) { p.Investigate = nil }, investigatePayload, "investigate"},
		{"investigate with a check input", func(p *protocol.RunKindPayload) { p.Check = &protocol.CheckInput{} }, investigatePayload, "check"},
		{"investigate without an endpoint", func(p *protocol.RunKindPayload) { p.LLMEndpoint = nil }, investigatePayload, "llm_endpoint"},
		{"investigate without a budget", func(p *protocol.RunKindPayload) { p.MaxCostUSD = 0 }, investigatePayload, "max_cost_usd"},
		{"check with an endpoint", func(p *protocol.RunKindPayload) { p.LLMEndpoint = &protocol.LLMEndpoint{Type: "openai"} }, checkPayload, "llm_endpoint"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.base()
			tc.mutate(&p)

			assert.Equal(t, tc.want, runKindInvalidField(p))
		})
	}
}

func TestBuildRunKindSpec(t *testing.T) {
	s := NewServer(Config{
		APIKey:      "k",
		Executor:    &fakeExecutor{},
		Tracker:     executor.NewTracker(1),
		Credentials: &fakeCredentials{},
		LaunchEnv: LaunchEnv{
			BaseImage:               "base:image",
			MCPURL:                  "http://cm:8080/mcp",
			MCPAPIKey:               "cfg-mcp-key",
			ContainerTimeoutSeconds: 9000,
			WorkerExtraEnv:          []string{"EXTRA=1"},
		},
		NextAttempt: func(string, string) int { return 3 },
		Logger:      quietLogger(),
	})

	t.Run("check", func(t *testing.T) {
		p := checkPayload()
		p.BaseBranch = "playbook/run-1"

		spec := s.buildRunKindSpec(p, "run-1")

		assert.Equal(t, protocol.RunKindCheck, spec.Kind)
		assert.Equal(t, "base:image", spec.Image)
		assert.Equal(t, "run-1", spec.CorrelationID)
		assert.Equal(t, 1, spec.Attempt)
		assert.Equal(t, "http://cm:8080/mcp", spec.MCPURL, "the CM host stays pinned in /etc/hosts")
		assert.Empty(t, spec.SkillsHostDir)
		assert.Contains(t, spec.Env, "CM_RUN_KIND=check")
		assert.Contains(t, spec.Env, "CM_CARD_ID=ABC-12")
		assert.Contains(t, spec.Env, "CM_PROJECT=proj")
		assert.Contains(t, spec.Env, "CM_REPO_URL=https://example.com/r.git")
		assert.Contains(t, spec.Env, "CM_BASE_BRANCH=playbook/run-1")
		assert.Contains(t, spec.Env, "CMX_CONTAINER_TIMEOUT_SECONDS=9000")
		assert.Contains(t, spec.Env, "EXTRA=1")

		for _, e := range spec.Env {
			for _, never := range []string{"CM_MCP_", "CMX_ATTEMPT=", "CMX_VERIFY=", "CM_MAX_COST_USD=", "CMX_SELECTION="} {
				assert.False(t, strings.HasPrefix(e, never), e)
			}
		}

		card := s.buildLaunchSpec(protocol.TriggerPayload{CardID: "ABC-12", Project: "proj", GitToken: "git-tok"}, "run-0", "")
		assert.NotEqual(t, card.SecretsHostDir, spec.SecretsHostDir)
	})

	t.Run("investigate", func(t *testing.T) {
		p := investigatePayload()
		p.WorkerImage = "project:image"

		spec := s.buildRunKindSpec(p, "run-2")

		selection, err := json.Marshal(p.Selection)
		require.NoError(t, err)

		assert.Equal(t, "project:image", spec.Image)
		assert.Contains(t, spec.Env, "CM_RUN_KIND=investigate")
		assert.Contains(t, spec.Env, "CM_MAX_COST_USD=0.5")
		assert.Contains(t, spec.Env, "CM_MODEL=vendor/model-a")
		assert.Contains(t, spec.Env, "CMX_SELECTION="+string(selection))

		for _, e := range spec.Env {
			assert.False(t, strings.HasPrefix(e, "CM_MCP_"), e)
		}
	})
}

func TestRunKindAdmission(t *testing.T) {
	valid, err := json.Marshal(checkPayload())
	require.NoError(t, err)

	analyse := checkPayload()
	analyse.Kind = protocol.RunKindAnalyse
	reserved, err := json.Marshal(analyse)
	require.NoError(t, err)

	rejections := []struct {
		name     string
		body     []byte
		prepare  func(h *harness)
		wantCode int
		wantErr  string
	}{
		{"invalid json", []byte(`{"kind":`), nil, http.StatusBadRequest, protocol.CodeInvalidJSON},
		{"reserved kind", reserved, nil, http.StatusBadRequest, protocol.CodeInvalidField},
		{"over 1 MiB", bytes.Repeat([]byte("x"), 1<<20+1), nil, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge},
		{"at capacity", valid, func(h *harness) { h.addRun("ABC-99", "proj") }, http.StatusTooManyRequests, protocol.CodeLimitReached},
		{"draining", valid, func(h *harness) { h.draining.Store(true) }, http.StatusServiceUnavailable, protocol.CodeDraining},
	}

	for _, tc := range rejections {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 1)
			if tc.prepare != nil {
				tc.prepare(h)
			}

			w := h.postRaw(t, tc.body)

			require.Equal(t, tc.wantCode, w.Code)
			assert.Equal(t, tc.wantErr, decodeErr(t, w).Code)
			assert.Empty(t, h.exec.launchedSpecs())
			assert.Empty(t, h.inputDirs(t))
		})
	}

	t.Run("a valid check is accepted and launched with its input", func(t *testing.T) {
		h := newHarness(t, 1)

		w := h.postRaw(t, valid)
		require.Equal(t, http.StatusAccepted, w.Code)

		require.Eventually(t, func() bool {
			return len(h.exec.launchedSpecs()) == 1
		}, time.Second, 5*time.Millisecond)

		spec := h.exec.launchedSpecs()[0]
		assert.Equal(t, protocol.RunKindCheck, spec.Kind)
		assert.True(t, strings.HasPrefix(spec.CorrelationID, "ABC-12-check-"), spec.CorrelationID)
		assertInputFile(t, spec.InputHostDir, checkPayload().Check)
	})

	t.Run("a launched run kind sends no status", func(t *testing.T) {
		h := newHarness(t, 1)

		h.runKindNow(checkPayload())

		require.Len(t, h.exec.launchedSpecs(), 1)
		assert.Empty(t, h.reporter.payloads())
	})
}

func TestRunKindAdmissionFailureReportsOnce(t *testing.T) {
	h := newHarness(t, 4)
	h.addRunKind("ABC-12", "proj", protocol.RunKindCheck)

	spec := h.runKindNow(checkPayload())

	got := h.reporter.payloads()
	require.Len(t, got, 1)
	assert.Equal(t, "failed", got[0].WorkerStatus)
	assert.Equal(t, protocol.RunKindCheck, got[0].Kind)
	assert.Equal(t, spec.CorrelationID, got[0].RunID)
	assert.Equal(t, protocol.StopCauseInfrastructure, got[0].StopCause)
	assert.Empty(t, h.exec.launchedSpecs())
	assert.Empty(t, h.inputDirs(t), "a run that did not launch leaves no input dir")
}

// fakeRunInputs is a RunInputStore whose Create always fails. The launch calls
// it on the test goroutine.
type fakeRunInputs struct {
	removed []string
}

func (f *fakeRunInputs) Create(string) (string, error) {
	return "", errors.New("disk full")
}

func (f *fakeRunInputs) Remove(runID string) {
	f.removed = append(f.removed, runID)
}

func (h *harness) openAdmissions() int {
	h.server.admissionsMu.Lock()
	defer h.server.admissionsMu.Unlock()

	return len(h.server.admissions)
}

// TestRunKindStagingFailureReportsOnce: a run whose inputs cannot be staged
// never reaches provisioning or the executor, ends its admission and sends its
// one failed status.
func TestRunKindStagingFailureReportsOnce(t *testing.T) {
	h := newHarness(t, 4)
	inputs := &fakeRunInputs{}
	h.server.runInputs = inputs

	spec := h.runKindNow(checkPayload())

	got := h.reporter.payloads()
	require.Len(t, got, 1)
	assert.Equal(t, "failed", got[0].WorkerStatus)
	assert.Equal(t, protocol.RunKindCheck, got[0].Kind)
	assert.Equal(t, spec.CorrelationID, got[0].RunID)
	assert.Equal(t, protocol.StopCauseInfrastructure, got[0].StopCause)
	assert.Empty(t, h.creds.provisionCalls())
	assert.Empty(t, h.exec.launchedSpecs())
	assert.Equal(t, []string{spec.CorrelationID}, inputs.removed)
	assert.Zero(t, h.openAdmissions(), "the admission is ended")
}

// TestRunKindStopDuringAdmission: ContextMatrix records a run kind as killed
// once its kind-scoped /kill answers, so a stop that lands before the launch
// leaves no status, no run and no input dir, whether staging succeeds or not.
func TestRunKindStopDuringAdmission(t *testing.T) {
	cases := []struct {
		name   string
		inputs *fakeRunInputs // nil keeps the harness's real store
	}{
		{name: "staged"},
		{name: "staging fails", inputs: &fakeRunInputs{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 4)
			if tc.inputs != nil {
				h.server.runInputs = tc.inputs
			}

			p := checkPayload()
			spec := h.server.buildRunKindSpec(p, mintRunID(p.CardID+"-"+p.Kind))
			adm, ctx := h.server.beginAdmission(context.Background(), spec.Key())

			kill := h.request(t, http.MethodPost, "/kill",
				protocol.KillPayload{CardID: p.CardID, Project: p.Project, Kind: p.Kind}, nowTS())
			answered := make(chan *httptest.ResponseRecorder, 1)

			go func() { answered <- h.serve(kill) }()

			require.Eventually(t, func() bool { return ctx.Err() != nil }, time.Second, time.Millisecond)
			h.server.launchRunKind(ctx, adm, p, spec)

			select {
			case w := <-answered:
				require.Equal(t, http.StatusOK, w.Code)
			case <-time.After(5 * time.Second):
				t.Fatal("the stop never answered")
			}

			assert.Empty(t, h.reporter.payloads(), "a stopped run kind reports nothing")
			assert.Empty(t, h.exec.launchedSpecs())
			assert.Empty(t, h.inputDirs(t))
			assert.Zero(t, h.openAdmissions())

			if tc.inputs != nil {
				assert.Equal(t, []string{spec.CorrelationID}, tc.inputs.removed)
			}
		})
	}
}

func TestRunKindLaunchFailureRemovesItsInputDir(t *testing.T) {
	h := newHarness(t, 4)
	h.exec.launchErr = errors.New("boom")

	h.runKindNow(investigatePayload())

	got := h.reporter.payloads()
	require.Len(t, got, 1)
	assert.Equal(t, protocol.RunKindInvestigate, got[0].Kind)
	assert.Equal(t, protocol.StopCauseInfrastructure, got[0].StopCause)
	assert.Empty(t, h.inputDirs(t))
}

// TestRunKindCoexistsWithTheCardRun: run kinds are keyed apart from the card
// run, so a running card does not refuse its check.
func TestRunKindCoexistsWithTheCardRun(t *testing.T) {
	h := newHarness(t, 4)
	h.addRun("ABC-12", "proj")

	h.runKindNow(checkPayload())

	require.Len(t, h.exec.launchedSpecs(), 1)
	assert.Empty(t, h.reporter.payloads())
}

func TestRunKindRegistersItsSecretsOnly(t *testing.T) {
	h := newHarness(t, 4)

	spec := h.runKindNow(investigatePayload())
	runID := spec.CorrelationID

	assert.ElementsMatch(t,
		[]string{"git-tok", "tok-" + runID, "cm-llm-key"},
		h.sessions.keys(SessionID("proj", "ABC-12", runID)),
		"the git token, the proxy token and the endpoint key, and no MCP key")

	registers := h.proxy.registerCalls()
	require.Len(t, registers, 1)
	assert.Equal(t, runID, registers[0].runKey)

	provisions := h.creds.provisionCalls()
	require.Len(t, provisions, 1)
	assert.Equal(t, executor.RunKey{CardID: "ABC-12", Kind: protocol.RunKindInvestigate}.Name(), provisions[0].cardID)
	assert.Equal(t, "git-tok", provisions[0].token)
	assert.Empty(t, provisions[0].expiresAt, "the run clones once; its token is never refreshed")
	assert.Equal(t, "tok-"+runID, provisions[0].endpoint.APIKey)
	assert.Equal(t, testLLMBaseURL, provisions[0].endpoint.BaseURL)

	launched := h.exec.launchedSpecs()
	require.Len(t, launched, 1)
	assertInputFile(t, launched[0].InputHostDir, investigatePayload().Investigate)
}

func TestCheckLaunchSkipsTheLLMProxy(t *testing.T) {
	t.Run("a check stages nothing LLM", func(t *testing.T) {
		h := newHarness(t, 4)

		spec := h.runKindNow(checkPayload())

		assert.Empty(t, h.proxy.registerCalls())

		provisions := h.creds.provisionCalls()
		require.Len(t, provisions, 1)
		assert.Equal(t, secrets.EndpointSecrets{}, provisions[0].endpoint)
		assert.Equal(t, []string{"git-tok"}, h.sessions.keys(SessionID("proj", "ABC-12", spec.CorrelationID)))
	})

	t.Run("a check needs no LLM proxy, even when its launch fails", func(t *testing.T) {
		h := newHarness(t, 4)
		h.server.llmProxy = nil
		h.exec.launchErr = errors.New("boom")

		h.runKindNow(checkPayload())

		got := h.reporter.payloads()
		require.Len(t, got, 1)
		assert.Equal(t, protocol.StopCauseInfrastructure, got[0].StopCause)
	})
}

// assertInputFile checks that dir holds input.json with want's JSON.
func assertInputFile(t *testing.T, dir string, want any) {
	t.Helper()

	require.NotEmpty(t, dir)

	got, err := os.ReadFile(filepath.Join(dir, runinput.InputFile))
	require.NoError(t, err)

	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)

	assert.JSONEq(t, string(wantJSON), string(got))
}

// fakeSections is a SectionSource that writes body, or fails with err.
type fakeSections struct {
	body []byte
	err  error
}

func (f *fakeSections) CopySection(_, _, _ string, w io.Writer) error {
	if f.err != nil {
		return f.err
	}

	_, err := w.Write(f.body)

	return err
}

func TestInvestigateStagesTheWholeSection(t *testing.T) {
	h := newHarness(t, 4)
	h.sections.body = bytes.Repeat([]byte("0123456789abcdef"), 5<<16)

	spec := h.runKindNow(investigatePayload())

	launched := h.exec.launchedSpecs()
	require.Len(t, launched, 1)
	require.NotEmpty(t, launched[0].InputHostDir)
	assert.Equal(t, spec.CorrelationID, launched[0].CorrelationID)

	staged, err := os.ReadFile(filepath.Join(launched[0].InputHostDir, runinput.RunLogFile))
	require.NoError(t, err)
	assert.Equal(t, h.sections.body, staged)
	assert.FileExists(t, filepath.Join(launched[0].InputHostDir, runinput.InputFile))

	info, err := os.Stat(filepath.Join(launched[0].InputHostDir, runinput.RunLogFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestInvestigateStagingFailures(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		cause string
	}{
		{name: "no section", err: filelog.ErrNoSection, cause: protocol.StopCauseError},
		{name: "read fails", err: errors.New("disk error"), cause: protocol.StopCauseInfrastructure},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 4)
			h.sections.err = tc.err

			spec := h.runKindNow(investigatePayload())

			got := h.reporter.payloads()
			require.Len(t, got, 1)
			assert.Equal(t, "failed", got[0].WorkerStatus)
			assert.Equal(t, protocol.RunKindInvestigate, got[0].Kind)
			assert.Equal(t, spec.CorrelationID, got[0].RunID)
			assert.Equal(t, tc.cause, got[0].StopCause)
			assert.Empty(t, h.exec.launchedSpecs())
			assert.Empty(t, h.creds.provisionCalls())
			assert.Empty(t, h.inputDirs(t))
			assert.Zero(t, h.openAdmissions())
		})
	}
}

func TestCheckStagesNoRunLog(t *testing.T) {
	h := newHarness(t, 4)

	h.runKindNow(checkPayload())

	launched := h.exec.launchedSpecs()
	require.Len(t, launched, 1)
	assert.NoFileExists(t, filepath.Join(launched[0].InputHostDir, runinput.RunLogFile))
}
