package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file drives the REAL autonomous orchestrator FSM end-to-end in-process:
// plan -> execute -> document -> review -> integrate -> done. The only stubbed boundaries
// are OpenRouter (a content-aware httptest SSE server) and the CM card-ops
// surface (an in-process recorder satisfying both worker.CardOps and the wider
// orchestrator.Ops). Git is REAL - a temp clone against a local bare origin -
// and so is the SSE wire client, the harness loop, the registry/selector, and
// the verify-command subprocess. runOrchestrator is NOT swapped here: the true
// orchestrator.Run executes, so this is the real-stack "autonomous completes a
// card" coverage that the HITL conversion (worker_test.go / e2e_test.go) set
// aside.
//
// The whole suite is guarded behind testing.Short() so `go test -short` skips
// it; `make test` (plain `go test ./...`, no -short) runs it.

// --- content-aware OpenRouter SSE stub -------------------------------------

// scriptedBackend serves /chat/completions by inspecting each request's prompt
// and conversation state, returning the SSE body the relevant phase expects.
// Matching on CONTENT (not call order) is required because the three review
// specialists run as PARALLEL subagents - an order-keyed stub would race, and a
// specialist could steal the synthesis body. Every served reply carries a
// scripted usage cost so the budget ledger and report_usage assertions have
// real numbers to sum.
//
// approveImmediately toggles the synthesis verdict: true approves on the first
// round (happy path); false rejects once with a fix that cites fixFile, then
// approves on the second synthesis call (the review fix-loop variant).
type scriptedBackend struct {
	mu sync.Mutex

	approveImmediately bool
	fixFile            string // file the reject verdict cites (drives the fixup target)

	planCost       float64
	coderCost      float64
	documentCost   float64
	specialistCost float64
	synthesisCost  float64
	fixCost        float64

	// recorded telemetry for assertions.
	synthesisCalls int     // number of synthesis (verdict) turns served
	totalCost      float64 // sum of every usage cost this backend emitted
	requests       int     // total /chat/completions calls served
}

// chatRequest is the subset of the OpenRouter request body the stub inspects.
type chatRequest struct {
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`
}

func (b *scriptedBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			// No /models endpoint: FetchCatalog 404s, the registry degrades to
			// priors/capabilities, and model resolution falls back to the default.
			http.NotFound(w, r)

			return
		}

		body, _ := io.ReadAll(r.Body)

		var req chatRequest

		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, b.reply(req))
	}
}

// reply selects the SSE body for one request from the prompt and conversation
// state, recording the emitted cost.
func (b *scriptedBackend) reply(req chatRequest) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.requests++

	firstUser := ""
	hasToolResult := false

	for _, m := range req.Messages {
		if m.Role == "user" && firstUser == "" {
			firstUser = m.Content
		}

		if m.Role == "tool" || m.ToolCallID != "" {
			hasToolResult = true
		}
	}

	switch {
	case strings.Contains(firstUser, "You are the planning agent"):
		b.totalCost += b.planCost

		return ssePlan(b.planCost)

	case strings.Contains(firstUser, "You are the coding agent for one subtask"):
		// Turn 1 writes the file; turn 2 (the tool result is now in the
		// conversation) calls finish with the commit message and ends the run.
		if hasToolResult {
			b.totalCost += b.coderCost

			return sseCoderCommit(coderCommitFor(firstUser), b.coderCost)
		}

		return sseWriteTool("call_code", writeArgsFor(firstUser), 0)

	case strings.Contains(firstUser, "You are a code-review specialist"):
		b.totalCost += b.specialistCost

		return sseStop(specialistFindings, b.specialistCost)

	case strings.Contains(firstUser, "You are the review synthesizer"):
		b.synthesisCalls++
		b.totalCost += b.synthesisCost

		if b.approveImmediately || b.synthesisCalls >= 2 {
			return sseStop(verdictApproved, b.synthesisCost)
		}

		return sseStop(verdictReject(b.fixFile), b.synthesisCost)

	case strings.Contains(firstUser, "You are the coding agent addressing review feedback"):
		// The fix coder edits the cited file then stops; no finish call is required
		// here - the orchestrator lands it as a fixup regardless of what the model
		// outputs.
		if hasToolResult {
			b.totalCost += b.fixCost

			return sseStop("Applied the fix.", b.fixCost)
		}

		return sseWriteTool("call_fix", writeArg(b.fixFile, "package main\n\n// fixed per review\n"), 0)

	case strings.Contains(firstUser, "You are the documentation agent"):
		// Conservative no-op: the agent writes nothing (no tool call), so the tree
		// stays clean and the phase commits/pushes nothing - the common case for a
		// change that needs no external docs. It still reports a costed usage so the
		// ledger/usage assertions account for the document model call.
		b.totalCost += b.documentCost

		return sseStop("No external documentation is needed for this change.", b.documentCost)

	case strings.Contains(firstUser, "You are writing the pull request description"):
		// Only reached when CreatePR is set; the tests keep it off, so this is a
		// defensive fallback rather than a live path.
		return sseStop("## What\nWork.\n", 0)

	default:
		// An unrecognised prompt must fail loudly, not hang on the natural-stop
		// fallback - a silent extra turn would mask a real wiring break.
		return sseStop("UNEXPECTED PROMPT", 0)
	}
}

// snapshot returns a stable copy of the recorded counters under the lock.
func (b *scriptedBackend) snapshot() (synthesisCalls, requests int, totalCost float64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.synthesisCalls, b.requests, b.totalCost
}

// --- SSE wire builders (the exact format llm/sse.go parses) ----------------

// usageWithCost is a usage chunk carrying the authoritative provider cost the
// ledger sums (the stopBody helper in e2e_test.go omits cost on purpose).
func usageWithCost(cost float64) string {
	return `"usage":{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,"cost":` +
		jsonNumber(cost) + `}`
}

// sseStop is a single content turn with a stop finish and a costed usage chunk.
func sseStop(content string, cost float64) string {
	return `data: {"model":"default/model","choices":[{"delta":{"content":` + jsonString(content) +
		`},"finish_reason":"stop"}],` + usageWithCost(cost) + "}\n" +
		"data: [DONE]\n"
}

// sseWriteTool is a turn emitting one streamed `write` tool call (id+name, then
// the JSON arguments) plus a tool_calls finish.
func sseWriteTool(callID, args string, cost float64) string {
	var sb strings.Builder

	sb.WriteString(`data: {"model":"default/model","choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"id":"` + callID + `","type":"function","function":{"name":"write","arguments":""}}]}}]}` + "\n")
	sb.WriteString(`data: {"choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"function":{"arguments":` + jsonString(args) + `}}]}}]}` + "\n")
	sb.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],` + usageWithCost(cost) + "}\n")
	sb.WriteString("data: [DONE]\n")

	return sb.String()
}

// sseFinish is a turn emitting one streamed `finish` tool call (id+name, then
// the JSON commit_message argument) plus a tool_calls finish - mirrors
// sseWriteTool's exact SSE delta framing with the tool name and args swapped.
func sseFinish(commitMsg string, cost float64) string {
	args, _ := json.Marshal(map[string]string{"commit_message": commitMsg})

	var sb strings.Builder

	sb.WriteString(`data: {"model":"default/model","choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"id":"call_finish","type":"function","function":{"name":"finish","arguments":""}}]}}]}` + "\n")
	sb.WriteString(`data: {"choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"function":{"arguments":` + jsonString(string(args)) + `}}]}}]}` + "\n")
	sb.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],` + usageWithCost(cost) + "}\n")
	sb.WriteString("data: [DONE]\n")

	return sb.String()
}

// ssePlan is the planner's stop turn: a two-subtask plan with one dependency
// edge (subtask 1 depends on subtask 0).
func ssePlan(cost float64) string {
	plan := `{"card_tier":"moderate","subtasks":[` +
		`{"title":"Add feature A","description":"Files: feature_a.txt. Create feature_a.txt.","depends_on":[],"tier":"simple"},` +
		`{"title":"Add feature B","description":"Files: feature_b.txt. Create feature_b.txt after A.","depends_on":[0],"tier":"moderate"}` +
		`]}`

	return sseStop(plan, cost)
}

// sseCoderCommit is a coder's final turn: it ends the run by calling finish.
func sseCoderCommit(commitMsg string, cost float64) string {
	return sseFinish(commitMsg, cost)
}

const specialistFindings = "Strengths: clear change.\nNo concerns.\nVerdict: looks good."

const verdictApproved = `{"approved":true,"summary":"All three lenses clean.","fixes":[]}`

// verdictReject is the one-time not-approved verdict in the fix-loop variant. It
// cites file so fixFiles extracts the path and the fixup targets the commit that
// last touched it (collapsing under autosquash at integrate).
func verdictReject(file string) string {
	return `{"approved":false,"summary":"One fix required.","fixes":[` +
		`{"file":"` + file + `","issue":"needs a tweak","suggestion":"adjust it"}]}`
}

// writeArgsFor builds the write-tool arguments for a coder turn, choosing the
// file from the subtask the prompt is about (the planner put the filename in
// each subtask description, and the prompt embeds the subtask title).
func writeArgsFor(prompt string) string {
	if strings.Contains(prompt, "Add feature B") {
		return writeArg("feature_b.txt", "feature B\n")
	}

	return writeArg("feature_a.txt", "feature A\n")
}

// coderCommitFor returns the conventional commit message for the subtask the
// prompt is about. The messages carry a scope ("app") so they diverge from
// sanitizeTitle's scopeless "feat: <lowercased title>" fallback - if the
// streamed finish commit_message argument were ever dropped, resolution would
// fall through to that fallback and the git-log assertions below would fail
// instead of passing vacuously.
func coderCommitFor(prompt string) string {
	if strings.Contains(prompt, "Add feature B") {
		return "feat(app): add feature b"
	}

	return "feat(app): add feature a"
}

func writeArg(path, content string) string {
	b, _ := json.Marshal(map[string]string{"path": path, "content": content})

	return string(b)
}

// jsonNumber formats a float as a compact JSON number (avoids strconv import
// churn and keeps the wire bytes deterministic).
func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)

	return string(b)
}

// --- in-process CM ops recorder (worker.CardOps + orchestrator.Ops) --------

// stubOps records every card-ops call in order and serves the scripted task
// context and subtask IDs the orchestrator needs. It satisfies the wide
// orchestrator.Ops surface (so ops2orchestrator hands the real FSM a live Ops)
// AND the narrow worker.CardOps the worker's Run consumes. All state is mutex
// guarded: the heartbeat goroutine and parallel review subagents call
// concurrently.
type stubOps struct {
	mu  sync.Mutex
	log []opRecord

	tcx cmclient.TaskContext

	nextID         int
	reviewAttempts int

	createCalls []createRecord
	usageCalls  []usageRecord
	pushCalls   []pushRecord
	phases      []string
	bodyUpdates []string
}

type opRecord struct {
	op   string
	args []any
}

type createRecord struct {
	project    string
	parent     string
	title      string
	dependsOn  []string
	returnedID string
}

type usageRecord struct {
	cardID        string
	model         string
	actualCostUSD float64
}

type pushRecord struct {
	cardID string
	branch string
	prURL  string
}

func newStubOps() *stubOps {
	return &stubOps{
		tcx: cmclient.TaskContext{
			Title:       "Build the widget",
			Description: "Implement the widget across two files.",
			State:       "in_progress",
			Phase:       "", // fresh autonomous run -> the FSM starts at plan
			Autonomous:  true,
			CreatePR:    false,
		},
	}
}

func (s *stubOps) record(op string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.log = append(s.log, opRecord{op: op, args: args})
}

func (s *stubOps) count(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0

	for _, c := range s.log {
		if c.op == op {
			n++
		}
	}

	return n
}

func (s *stubOps) ClaimCard(_ context.Context, cardID string) error {
	s.record("ClaimCard", cardID)

	return nil
}

func (s *stubOps) GetTaskContext(_ context.Context, cardID string, _ bool) (cmclient.TaskContext, error) {
	s.record("GetTaskContext", cardID)

	return s.tcx, nil
}

func (s *stubOps) Heartbeat(_ context.Context, cardID string) error {
	s.record("Heartbeat", cardID)

	return nil
}

func (s *stubOps) CreateCard(_ context.Context, project, parent, title, _ string, dependsOn []string) (string, error) {
	s.mu.Lock()
	s.nextID++
	id := "CMX-SUB-" + itoa(s.nextID)
	s.createCalls = append(s.createCalls, createRecord{
		project:    project,
		parent:     parent,
		title:      title,
		dependsOn:  append([]string(nil), dependsOn...),
		returnedID: id,
	})
	s.mu.Unlock()

	s.record("CreateCard", project, parent, title)

	return id, nil
}

func (s *stubOps) SetPhase(_ context.Context, _, phase string) error {
	s.mu.Lock()
	s.phases = append(s.phases, phase)
	s.mu.Unlock()

	s.record("SetPhase", phase)

	return nil
}

func (s *stubOps) UpdateCardBody(_ context.Context, _, body string) error {
	s.mu.Lock()
	s.bodyUpdates = append(s.bodyUpdates, body)
	s.mu.Unlock()

	s.record("UpdateCardBody", body)

	return nil
}

func (s *stubOps) TransitionCard(_ context.Context, _, state string) error {
	s.record("TransitionCard", state)

	return nil
}

func (s *stubOps) StartReview(_ context.Context, cardID string) error {
	s.record("StartReview", cardID)

	return nil
}

func (s *stubOps) IncrementReviewAttempts(_ context.Context, cardID string) (int, error) {
	s.record("IncrementReviewAttempts", cardID)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.reviewAttempts++

	return s.reviewAttempts, nil
}

func (s *stubOps) SubtaskStates(_ context.Context, project, parentID string) ([]cmclient.SubtaskState, error) {
	s.record("SubtaskStates", project, parentID)

	// Fresh run never reconciles via this path; return nothing.
	return nil, nil
}

func (s *stubOps) AddLog(_ context.Context, cardID, message string) error {
	s.record("AddLog", cardID, message)

	return nil
}

func (s *stubOps) ReportUsage(_ context.Context, cardID, model string, _, _ int64, actualCostUSD float64) error {
	s.mu.Lock()
	s.usageCalls = append(s.usageCalls, usageRecord{cardID: cardID, model: model, actualCostUSD: actualCostUSD})
	s.mu.Unlock()

	s.record("ReportUsage", cardID, model, actualCostUSD)

	return nil
}

func (s *stubOps) ReportPush(_ context.Context, cardID, branch, prURL string) error {
	s.mu.Lock()
	s.pushCalls = append(s.pushCalls, pushRecord{cardID: cardID, branch: branch, prURL: prURL})
	s.mu.Unlock()

	s.record("ReportPush", cardID, branch, prURL)

	return nil
}

func (s *stubOps) ReportModelOutcomes(_ context.Context, cardID string, outcomes []cmclient.ModelOutcome) error {
	s.record("ReportModelOutcomes", cardID, len(outcomes))

	return nil
}

func (s *stubOps) BlacklistModel(_ context.Context, cardID, model, reason string) error {
	s.record("BlacklistModel", cardID, model, reason)

	return nil
}

func (s *stubOps) CompleteTask(_ context.Context, cardID, _ string) error {
	s.record("CompleteTask", cardID)

	return nil
}

func (s *stubOps) ReleaseCard(_ context.Context, cardID string) error {
	s.record("ReleaseCard", cardID)

	return nil
}

func (s *stubOps) RecordSkillEngaged(_ context.Context, _, _ string) error {
	return nil
}

// --- bare-origin seeding helper --------------------------------------------

// setupBareRemoteWithFiles is setupBareRemote plus extra seed files committed to
// main, so the gate-positive variant's clone carries a go.mod and a passing
// test at the workspace root. files maps repo-relative paths to contents.
func setupBareRemoteWithFiles(t *testing.T, files map[string]string) string {
	t.Helper()

	bare := t.TempDir()
	scratch := t.TempDir()

	runGit(t, bare, "init", "--bare", "-b", "main", bare)

	runGit(t, scratch, "init", "-b", "main", scratch)
	runGit(t, scratch, "config", "user.email", "test@example.com")
	runGit(t, scratch, "config", "user.name", "test")

	require.NoError(t, os.WriteFile(filepath.Join(scratch, "README.md"), []byte("seed\n"), 0o644))

	for rel, content := range files {
		abs := filepath.Join(scratch, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	}

	runGit(t, scratch, "add", "-A")
	runGit(t, scratch, "commit", "-m", "init")
	runGit(t, scratch, "remote", "add", "origin", bare)
	runGit(t, scratch, "push", "-u", "origin", "main")

	return bare
}

// gitLog returns the subject lines of every commit reachable from branch on the
// bare remote, newest first.
func gitLog(t *testing.T, remote, branch string) []string {
	t.Helper()

	cmd := exec.Command("git", "log", "--format=%s", branch)
	cmd.Dir = remote
	cmd.Env = gitEnv()

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git log %s: %s", branch, out)

	var subjects []string

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			subjects = append(subjects, line)
		}
	}

	return subjects
}

// runOrchestratorE2E wires the REAL worker.Run for an autonomous card against a
// real clone of remote, a content-aware SSE backend, and the in-process ops
// recorder. It returns the result and the recorder for assertions.
func runOrchestratorE2E(t *testing.T, remote string, backend *scriptedBackend, ops *stubOps) (Result, error) {
	t.Helper()

	stubURL := startSSEBackend(t, backend)

	client := llm.NewClient("test-key", llm.WithBaseURL(stubURL))

	spec := baseSpec(t, remote, t.TempDir())
	spec.Interactive = false // autonomous -> the FSM owns the run
	spec.MaxCardCost = 0     // no budget ceiling: this run must complete, not park
	spec.MaxTurns = 6

	emit := events.NewEmitter(io.Discard, io.Discard)

	return Run(context.Background(), spec, ops, client, emit, openStdin(t))
}

// startSSEBackend starts the httptest server for the scripted backend and
// returns its base URL, closing it in cleanup.
func startSSEBackend(t *testing.T, backend *scriptedBackend) string {
	t.Helper()

	srv := httptest.NewServer(backend.handler())
	t.Cleanup(srv.Close)

	return srv.URL
}

// --- Test: happy path (gate absent, approve first round) -------------------

// TestOrchestratorEndToEndHappyPath drives a fresh autonomous card all the way
// through the real FSM with no review fix loop. The workspace has no go.mod /
// Makefile / package.json, so the verify gate is absent; the three specialists
// run and synthesis approves on the first round. Git is real against a bare
// origin: the run cuts cm/cmx-001, commits two subtasks, reviews, then
// autosquash-integrates and lease-pushes the final history.
func TestOrchestratorEndToEndHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in-process orchestrator e2e in -short mode")
	}

	t.Parallel()

	// baseSpec declares a trivial always-pass verify command, so the gate is a
	// deterministic pass and no model proposal fires during resolution.
	remote := setupBareRemote(t)

	backend := &scriptedBackend{
		approveImmediately: true,
		planCost:           0.0100,
		coderCost:          0.0200,
		documentCost:       0.0030,
		specialistCost:     0.0050,
		synthesisCost:      0.0100,
	}

	ops := newStubOps()

	res, err := runOrchestratorE2E(t, remote, backend, ops)
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	// --- phase order: plan, execute, judge, document, review, integrate, done, in order -----
	// judge is persisted like every phase even on a normal run, where its body is a no-op.
	assert.Equal(t, []string{"plan", "execute", "judge", "document", "review", "integrate", "done"}, ops.phases,
		"phases must be persisted in forward order exactly once each")

	// --- subtask wiring: two create_card calls with parent + the dep edge ---
	require.Len(t, ops.createCalls, 2, "the plan has two subtasks")

	a, b := ops.createCalls[0], ops.createCalls[1]
	assert.Equal(t, "CMX-001", a.parent, "subtask A is parented to the card")
	assert.Equal(t, "demo", a.project)
	assert.Empty(t, a.dependsOn, "subtask A has no dependencies")
	assert.Equal(t, "CMX-001", b.parent, "subtask B is parented to the card")
	assert.Equal(t, []string{a.returnedID}, b.dependsOn, "subtask B depends on the real card ID of A")

	// --- origin has cm/cmx-001 with the integrated (squashed) history -------
	branch := "cm/cmx-001"
	require.True(t, remoteHasBranch(t, remote, branch), "the card branch must exist on origin")

	subjects := gitLog(t, remote, branch)
	assert.Contains(t, subjects, "feat(app): add feature a")
	assert.Contains(t, subjects, "feat(app): add feature b")

	for _, s := range subjects {
		assert.NotContains(t, s, "fixup!", "the happy path produces no fixup commits")
	}

	// Both subtask files landed on the integrated branch.
	assert.Equal(t, "feature A\n", branchFile(t, remote, branch, "feature_a.txt"))
	assert.Equal(t, "feature B\n", branchFile(t, remote, branch, "feature_b.txt"))

	// --- review approved on the first round: no extra attempts -------------
	assert.Equal(t, 0, ops.count("IncrementReviewAttempts"),
		"first-round approval increments no review attempts")
	assert.Equal(t, 1, ops.count("StartReview"))

	// --- the card was driven to done and released, never CompleteTask'd ----
	// (the parent transitions to done in integrate; the worker does not call
	// CompleteTask on the FSM happy path - done releases the claim).
	assert.Equal(t, 1, ops.count("TransitionCard"), "parent transitions to done once")
	assert.GreaterOrEqual(t, ops.count("ReleaseCard"), 1, "done releases the parent claim")

	// --- report_push carried the card branch -------------------------------
	require.NotEmpty(t, ops.pushCalls, "integrate reports the push")

	last := ops.pushCalls[len(ops.pushCalls)-1]
	assert.Equal(t, branch, last.branch, "the reported push branch is the card branch")
	assert.Empty(t, last.prURL, "no PR was created (CreatePR is off)")

	// --- report_usage carried actual_cost_usd; the ledger sums to the total -
	require.NotEmpty(t, ops.usageCalls, "every model-bearing step reports usage")

	var reported float64

	for _, u := range ops.usageCalls {
		assert.Positive(t, u.actualCostUSD, "each usage report carries a non-zero actual cost")

		reported += u.actualCostUSD
	}

	_, _, emitted := backend.snapshot()
	assert.InDelta(t, emitted, reported, 1e-9,
		"the scripted usage costs must sum to the total reported through report_usage")

	// Expected spend: plan + 2 coders + 1 document + 3 specialists + 1 synthesis.
	expected := backend.planCost + 2*backend.coderCost + backend.documentCost +
		3*backend.specialistCost + backend.synthesisCost
	assert.InDelta(t, expected, reported, 1e-9, "the total reported cost matches the scripted script")
}

// --- Test: gate-positive + one review fix loop -----------------------------

// TestOrchestratorEndToEndFixLoop drives the FSM with a real verify gate (a
// go.mod + a passing test seeded at the workspace root, so `go test ./...`
// succeeds) and a single review fix loop: synthesis rejects once with a fix
// citing feature_b.txt, the coder lands a fixup, the gate passes again, and the
// second synthesis approves. After integrate, origin's history must show NO
// "fixup!" subjects - autosquash collapsed the fixup into its target.
func TestOrchestratorEndToEndFixLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in-process orchestrator e2e in -short mode")
	}

	t.Parallel()

	// A self-contained, dependency-free module so the gate's `go test ./...`
	// runs fully offline. The passing test means the gate never short-circuits
	// to a fix on its own - only the scripted synthesis verdict does.
	remote := setupBareRemoteWithFiles(t, map[string]string{
		"go.mod":       "module e2eworkspace\n\ngo 1.26\n",
		"keep_test.go": "package e2eworkspace\n\nimport \"testing\"\n\nfunc TestKeep(t *testing.T) {}\n",
	})

	backend := &scriptedBackend{
		approveImmediately: false, // reject once, then approve
		fixFile:            "feature_b.txt",
		planCost:           0.0100,
		coderCost:          0.0200,
		documentCost:       0.0030,
		specialistCost:     0.0050,
		synthesisCost:      0.0100,
		fixCost:            0.0150,
	}

	ops := newStubOps()

	res, err := runOrchestratorE2E(t, remote, backend, ops)
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	// Two synthesis turns: the reject and the approve.
	synthCalls, _, emitted := backend.snapshot()
	assert.Equal(t, 2, synthCalls, "synthesis runs twice: reject then approve")

	// One not-approved round => exactly one review-attempts increment, well
	// under the cap so the run never parks.
	assert.Equal(t, 1, ops.count("IncrementReviewAttempts"),
		"the single rejection increments review attempts once")

	// The run still reached integrate and done (judge is a persisted no-op on a normal run).
	assert.Equal(t, []string{"plan", "execute", "judge", "document", "review", "integrate", "done"}, ops.phases)
	assert.Equal(t, 1, ops.count("TransitionCard"), "parent transitions to done")

	// --- origin's integrated history is autosquashed: NO fixup! subjects ----
	branch := "cm/cmx-001"
	require.True(t, remoteHasBranch(t, remote, branch))

	subjects := gitLog(t, remote, branch)
	for _, s := range subjects {
		assert.NotContains(t, s, "fixup!",
			"autosquash must collapse the review fixup; no fixup! subject survives on origin")
	}

	assert.Contains(t, subjects, "feat(app): add feature a")
	assert.Contains(t, subjects, "feat(app): add feature b")

	// Non-vacuous autosquash proof. The fix coder OVERWROTE feature_b.txt (the
	// cited file), the orchestrator committed that as a fixup targeting B's
	// commit and pushed it, and integrate's --autosquash folded it back in. So:
	//   1. the integrated tree carries the FIXED content (the fix really ran),
	//   2. yet there is exactly ONE "feat(app): add feature b" subject and no
	//      fixup! (the fixup collapsed rather than surviving as its own commit).
	// A surviving fixup would show seed + three functional commits (== 4) and a
	// fixup! subject; this pins the collapse to the fix, not just subject text.
	assert.Equal(t, "package main\n\n// fixed per review\n",
		branchFile(t, remote, branch, "feature_b.txt"),
		"the review fix landed in the integrated tree")
	assert.Len(t, subjects, 3, "seed + two squashed subtask commits; the fixup collapsed: %v", subjects)

	featBCommits := 0

	for _, s := range subjects {
		if s == "feat(app): add feature b" {
			featBCommits++
		}
	}

	assert.Equal(t, 1, featBCommits, "the fixup folded into B's single commit, not a second one")

	// --- report_usage still sums (now including the fix run) ----------------
	var reported float64

	for _, u := range ops.usageCalls {
		reported += u.actualCostUSD
	}

	assert.InDelta(t, emitted, reported, 1e-9, "ledger: scripted costs sum to the reported total")

	// plan + 2 coders + 1 document + (specialists+synthesis) x2 rounds + 1 fix run.
	expected := backend.planCost + 2*backend.coderCost + backend.documentCost +
		2*(3*backend.specialistCost+backend.synthesisCost) + backend.fixCost
	assert.InDelta(t, expected, reported, 1e-9, "the fix-loop total matches the scripted script")

	// --- the reported push branch is still the card branch -----------------
	require.NotEmpty(t, ops.pushCalls)

	last := ops.pushCalls[len(ops.pushCalls)-1]
	assert.Equal(t, branch, last.branch)
}

// compile-time proof the recorder satisfies BOTH surfaces: the worker's narrow
// CardOps (what Run consumes) and the wider orchestrator.Ops the FSM drives via
// ops2orchestrator. The wide fit is load-bearing - a nil Ops would panic the
// real FSM this test exercises.
var (
	_ CardOps          = (*stubOps)(nil)
	_ orchestrator.Ops = (*stubOps)(nil)
)
