package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-agent/internal/worker"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/spf13/cobra"
)

// cmEnvFile is the bind-mounted env file path the service injects secrets into.
const cmEnvFile = "/run/cm-secrets/env"

// cardIDPattern matches ContextMatrix card IDs (PREFIX-NNN, accepting upper-
// and lower-case letters): a letter-led prefix of letters, digits, and dashes
// (CM only requires the project prefix to be non-empty, so MY-PROJ-001 is a
// legitimate ID), ending in a dash and a numeric suffix — exactly what CM's
// server-side ID generator produces. The card ID becomes the cm/<id> work branch name, so this
// conservative shape keeps crafted refs (colons, slashes, dots, spaces,
// leading dashes) out of the push path entirely instead of relying on git's
// refspec parser to reject them.
var cardIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*-[0-9]+$`)

// mcpCloseTimeout bounds the MCP client teardown: a slow or dead tunnel
// must not keep a finished worker alive past the backend's kill grace —
// the process exit code is the run's success signal.
const mcpCloseTimeout = 2 * time.Second

// closeBounded closes c but gives up after d, logging instead of hanging.
func closeBounded(c io.Closer, d time.Duration) {
	done := make(chan struct{})

	go func() {
		_ = c.Close() //nolint:errcheck // best-effort teardown

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(d):
		slog.Warn("mcp client close timed out; exiting anyway", "timeout", d)
	}
}

func newWorkCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "work",
		Short:  "Container entrypoint: execute one card under ContextMatrix control",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := specFromEnv()
			if err != nil {
				return err
			}

			src, err := secrets.Open(cmEnvFile)
			if err != nil {
				return fmt.Errorf("read secrets: %w", err)
			}

			// LLM values resolve env-first-then-file: the llm-only-payload delivery
			// sets LLM_* as container env, which wins (set-but-empty counts as set),
			// falling back to the mounted file otherwise. The git token is NOT
			// resolved from env — the credential helper re-reads CM_GIT_TOKEN from the
			// file per git op so host-side rotation reaches a long-running worker.
			spec.LLMKey = resolveLLMValue("LLM_API_KEY", src)
			spec.LLMBaseURL = resolveLLMValue("LLM_BASE_URL", src)
			spec.LLMType = resolveLLMValue("LLM_TYPE", src)
			spec.GitToken = src.Get("CM_GIT_TOKEN")

			// Guest specs carry bearer tokens, so they ride the secrets file —
			// same delivery as the git token, never plain container env.
			if spec.Mob != nil {
				spec.Mob.Guests = mobGuestsFromSecrets(src)
			}

			// human off (io.Discard), JSONL → stdout for the service log bridge.
			emit := events.NewEmitter(io.Discard, cmd.OutOrStdout())

			// When an extra CA is mounted, the worker's own outbound TLS (LLM
			// client + MCP connection) must trust it. Build the injections once
			// and share them across both clients.
			caLLMOpts, caMCPOpts, err := caInjections(spec.CACertFile)
			if err != nil {
				return err
			}

			clientOpts := []llm.Option{llm.WithRetry(llm.DefaultRetryPolicy()), llm.WithDialect(dialectFromType(spec.LLMType))}
			clientOpts = append(clientOpts, caLLMOpts...)

			if spec.LLMBaseURL != "" {
				clientOpts = append(clientOpts, llm.WithBaseURL(spec.LLMBaseURL))
			}

			client := llm.NewClient(spec.LLMKey, clientOpts...)

			ops, err := cmclient.New(cmd.Context(), spec.MCPURL, spec.MCPAPIKey, "cmx-agent-"+strings.ToLower(spec.CardID), caMCPOpts...)
			if err != nil {
				return fmt.Errorf("connect mcp: %w", err)
			}
			defer closeBounded(ops, mcpCloseTimeout)

			res, err := worker.Run(cmd.Context(), spec, ops, client, emit, cmd.InOrStdin())
			if err != nil {
				return err
			}

			slog.Info("run finished", "reason", res.Reason)

			return nil
		},
	}
}

// specFromEnv builds a RunSpec from the CM_*/CMX_* environment contract.
// Required vars are CM_CARD_ID, CM_PROJECT, CM_REPO_URL, CM_MCP_URL,
// CM_MCP_API_KEY. Missing required vars return an error naming the var.
func specFromEnv() (worker.RunSpec, error) {
	cardID, err := requireEnv("CM_CARD_ID")
	if err != nil {
		return worker.RunSpec{}, err
	}

	if !cardIDPattern.MatchString(cardID) {
		return worker.RunSpec{}, fmt.Errorf("env var CM_CARD_ID: invalid card ID %q (want PREFIX-NNN)", cardID)
	}

	project, err := requireEnv("CM_PROJECT")
	if err != nil {
		return worker.RunSpec{}, err
	}

	repoURL, err := requireEnv("CM_REPO_URL")
	if err != nil {
		return worker.RunSpec{}, err
	}

	mcpURL, err := requireEnv("CM_MCP_URL")
	if err != nil {
		return worker.RunSpec{}, err
	}

	mcpAPIKey, err := requireEnv("CM_MCP_API_KEY")
	if err != nil {
		return worker.RunSpec{}, err
	}

	bashTimeoutMax, err := envInt("CMX_BASH_TIMEOUT_MAX_SECONDS", 600)
	if err != nil {
		return worker.RunSpec{}, err
	}

	toolOutputMax, err := envInt("CMX_TOOL_OUTPUT_MAX_BYTES", 131072)
	if err != nil {
		return worker.RunSpec{}, err
	}

	defaults := config.Defaults()

	maxTurns, err := envInt("CMX_MAX_TURNS", derefInt(defaults.MaxTurns))
	if err != nil {
		return worker.RunSpec{}, err
	}

	maxCardCost, err := envFloat("CMX_MAX_CARD_COST", 0)
	if err != nil {
		return worker.RunSpec{}, err
	}

	selectorPriceHeadroom, err := envFloat("CMX_SELECTOR_PRICE_HEADROOM", 0)
	if err != nil {
		return worker.RunSpec{}, err
	}

	compactionThreshold, err := envFloat("CMX_COMPACTION_THRESHOLD", 0.85)
	if err != nil {
		return worker.RunSpec{}, err
	}

	compactionKeepRecentTurns, err := envInt("CMX_COMPACTION_KEEP_RECENT_TURNS", 6)
	if err != nil {
		return worker.RunSpec{}, err
	}

	bestOfN, err := envInt("CM_BEST_OF_N", 0)
	if err != nil {
		return worker.RunSpec{}, err
	}

	mobParticipants, err := envInt("CM_MOB_PARTICIPANTS", 0)
	if err != nil {
		return worker.RunSpec{}, err
	}

	mobRounds, err := envInt("CM_MOB_ROUNDS", 0)
	if err != nil {
		return worker.RunSpec{}, err
	}

	mobBudgetFactor, err := envFloat("CM_MOB_BUDGET_FACTOR", 0)
	if err != nil {
		return worker.RunSpec{}, err
	}

	// Guests are NOT read here: they carry bearer tokens and ride the mounted
	// secrets file, resolved in RunE next to the LLM key.
	var mobSpec *protocol.MobSpec

	if mobParticipants >= 2 {
		mobSpec = &protocol.MobSpec{
			Participants: mobParticipants,
			Rounds:       mobRounds,
			BudgetFactor: mobBudgetFactor,
		}

		if v := os.Getenv("CM_MOB_PHASES"); v != "" {
			mobSpec.Phases = strings.Split(v, ",")
		}
	}

	compactionEnabled := os.Getenv("CMX_COMPACTION_ENABLED") == "true"

	defaultModel := os.Getenv("CMX_DEFAULT_MODEL")
	if defaultModel == "" {
		defaultModel = derefStr(defaults.CapableModel)
	}

	workspace := os.Getenv("CMX_WORKSPACE")
	if workspace == "" {
		workspace = "/home/user/workspace"
	}

	var selection *protocol.SelectionContext

	if raw := os.Getenv("CMX_SELECTION"); raw != "" {
		var sc protocol.SelectionContext
		if err := json.Unmarshal([]byte(raw), &sc); err != nil {
			slog.Warn("CMX_SELECTION parse failed; will use default model",
				"card_id", cardID, "project", project, "error", err)
		} else {
			selection = &sc
		}
	}

	var verify *protocol.VerifyConfig

	if raw := os.Getenv("CMX_VERIFY"); raw != "" {
		var vc protocol.VerifyConfig
		if err := json.Unmarshal([]byte(raw), &vc); err != nil {
			slog.Warn("CMX_VERIFY parse failed; will detect the verify command",
				"card_id", cardID, "project", project, "error", err)
		} else {
			verify = &vc
		}
	}

	taskSkillsDir := os.Getenv("CMX_TASK_SKILLS_DIR")

	var (
		taskSkills    []string
		taskSkillsSet bool
	)

	if _, ok := os.LookupEnv("CM_TASK_SKILLS_SET"); ok {
		taskSkillsSet = true

		if v := os.Getenv("CM_TASK_SKILLS"); v != "" {
			taskSkills = strings.Split(v, ",")
		}
	}

	spec := worker.RunSpec{
		CardID:                    cardID,
		Project:                   project,
		RepoURL:                   repoURL,
		MCPURL:                    mcpURL,
		MCPAPIKey:                 mcpAPIKey,
		SecretsEnvPath:            cmEnvFile,
		BaseBranch:                os.Getenv("CM_BASE_BRANCH"),
		Model:                     os.Getenv("CM_MODEL"),
		Interactive:               os.Getenv("CM_INTERACTIVE") == "true",
		BestOfN:                   bestOfN,
		Mob:                       mobSpec,
		BashTimeoutMax:            bashTimeoutMax,
		ToolOutputMax:             toolOutputMax,
		MaxTurns:                  maxTurns,
		MaxCardCost:               maxCardCost,
		SelectorPriceHeadroom:     selectorPriceHeadroom,
		CompactionEnabled:         compactionEnabled,
		CompactionThreshold:       compactionThreshold,
		CompactionKeepRecentTurns: compactionKeepRecentTurns,
		DefaultModel:              defaultModel,
		ReasoningEffort:           os.Getenv("CMX_REASONING_EFFORT"),
		Workspace:                 workspace,
		CACertFile:                os.Getenv("CMX_CA_CERT_FILE"),
		Selection:                 selection,
		Verify:                    verify,
		TaskSkillsDir:             taskSkillsDir,
		TaskSkills:                taskSkills,
		TaskSkillsSet:             taskSkillsSet,
	}

	return spec, nil
}

// resolveLLMValue resolves an LLM endpoint value env-first-then-file: a
// container env var set by the llm-only-payload delivery wins (os.LookupEnv, so
// set-but-empty counts as set — an empty LLM_BASE_URL means "the type's
// canonical default"), falling back to the mounted secrets file when the var is
// unset. Used only for the LLM_* values; the git token stays file-only so the
// credential helper picks up host-side rotation.
func resolveLLMValue(name string, src *secrets.Source) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}

	return src.Get(name)
}

// requireEnv returns the value of the named env var or an error naming it.
func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("required env var %s is not set", name)
	}

	return v, nil
}

// envInt parses an optional integer env var, returning defaultVal when the var
// is absent. A non-empty value that fails to parse returns an error.
func envInt(name string, defaultVal int) (int, error) {
	s := os.Getenv(name)
	if s == "" {
		return defaultVal, nil
	}

	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("env var %s: invalid integer %q", name, s)
	}

	return v, nil
}

// envFloat parses an optional float64 env var, returning defaultVal when the
// var is absent. A non-empty value that fails to parse returns an error.
func envFloat(name string, defaultVal float64) (float64, error) {
	s := os.Getenv(name)
	if s == "" {
		return defaultVal, nil
	}

	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("env var %s: invalid float %q", name, s)
	}

	return v, nil
}

// caInjections builds the extra-CA injections for the worker's in-container
// clients from certPath (the in-container CA PEM path): an llm option so the
// harness LLM client trusts the CA, and a cmclient option so the MCP connection
// shares that trust. An empty certPath yields no options — the clients keep
// their defaults. The git/gh subprocesses get the CA separately, via RunSpec
// threaded to NewGit / NewPRCreator (their env is scrubbed by the harness).
func caInjections(certPath string) ([]llm.Option, []cmclient.Option, error) {
	if certPath == "" {
		return nil, nil, nil
	}

	httpClient, err := config.HTTPClientWithCA(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("build ca http client: %w", err)
	}

	transport, err := config.CATransport(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("build ca transport: %w", err)
	}

	return []llm.Option{llm.WithHTTPClient(httpClient)}, []cmclient.Option{cmclient.WithBaseTransport(transport)}, nil
}

// dialectFromType maps the LLM_TYPE string to the harness dialect. Defaults to
// OpenRouter for empty or unrecognised values so existing deployments with no
// LLM_TYPE set keep working unchanged.
func dialectFromType(s string) llm.Dialect {
	if s == "openai" {
		return llm.DialectOpenAI
	}

	return llm.DialectOpenRouter
}

// mobGuestsFromSecrets parses the CM_MOB_GUESTS JSON ([]protocol.GuestSpec)
// from the mounted secrets file. Guests carry bearer tokens, so they ride the
// secrets mount, never plain container env. A parse failure degrades to no
// guests — a discussion must never fail the run.
func mobGuestsFromSecrets(src *secrets.Source) []protocol.GuestSpec {
	raw := src.Get("CM_MOB_GUESTS")
	if raw == "" {
		return nil
	}

	var guests []protocol.GuestSpec
	if err := json.Unmarshal([]byte(raw), &guests); err != nil {
		slog.Warn("CM_MOB_GUESTS parse failed; discussion runs without guests", "error", err)

		return nil
	}

	return guests
}
