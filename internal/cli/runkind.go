package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mhersson/contextmatrix-agent/internal/cardid"
	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/runinput"
	"github.com/mhersson/contextmatrix-agent/internal/runkind"
	"github.com/mhersson/contextmatrix-agent/internal/runoutcome"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/spf13/cobra"
)

// The container paths a run kind works in.
const (
	runKindWorkspace = "/home/user/workspace"
	runKindInputsDir = "/home/user/investigation"
)

// runKindMain runs one run kind and writes its outcome as the stream's last
// line. A run that cannot start ends with cause error; any cause but done exits
// non-zero.
func runKindMain(cmd *cobra.Command, kind string) error {
	emit := events.NewEmitter(io.Discard, cmd.OutOrStdout())
	data := runoutcome.Data{Cause: protocol.StopCauseError}

	spec, client, err := prepareRunKind(kind, filepath.Join(runinput.MountPath, runinput.InputFile))
	if err != nil {
		slog.Error("run kind could not start", "kind", kind, "error", err)
	} else {
		data = runkind.Run(cmd.Context(), spec, client, emit)
	}

	runoutcome.Emit(emit, data)

	if data.Cause != protocol.StopCauseDone {
		return fmt.Errorf("run kind %s ended with cause %s", kind, data.Cause)
	}

	return nil
}

// prepareRunKind reads the run's spec, secrets and input, and builds its LLM
// client when it was given LLM access.
func prepareRunKind(kind, inputPath string) (runkind.Spec, llm.LLM, error) {
	spec, err := runKindSpecFromEnv(kind)
	if err != nil {
		return runkind.Spec{}, nil, err
	}

	src, err := secrets.Open(spec.SecretsPath)
	if err != nil {
		return runkind.Spec{}, nil, fmt.Errorf("read secrets: %w", err)
	}

	spec, err = readRunKindFiles(spec, src, inputPath)
	if err != nil {
		return runkind.Spec{}, nil, err
	}

	client, err := runKindClient(spec, src)
	if err != nil {
		return runkind.Spec{}, nil, err
	}

	return spec, client, nil
}

// runKindSpecFromEnv builds a run kind's spec from the env serve writes. It
// needs no CM_MCP_URL or CM_MCP_API_KEY: a run kind gets neither.
func runKindSpecFromEnv(kind string) (runkind.Spec, error) {
	var cardID string

	if kind == protocol.RunKindGuidancePR {
		if os.Getenv("CM_CARD_ID") != "" {
			return runkind.Spec{}, fmt.Errorf("env var CM_CARD_ID: must be empty for %s", kind)
		}
	} else {
		var err error

		cardID, err = requireEnv("CM_CARD_ID")
		if err != nil {
			return runkind.Spec{}, err
		}

		if !cardid.Valid(cardID) {
			return runkind.Spec{}, fmt.Errorf("env var CM_CARD_ID: invalid card ID %q (want PREFIX-NNN)", cardID)
		}
	}

	project, err := requireEnv("CM_PROJECT")
	if err != nil {
		return runkind.Spec{}, err
	}

	repoURL, err := requireEnv("CM_REPO_URL")
	if err != nil {
		return runkind.Spec{}, err
	}

	maxCost, err := envFloat("CM_MAX_COST_USD", 0)
	if err != nil {
		return runkind.Spec{}, err
	}

	bashTimeoutMax, err := envInt("CMX_BASH_TIMEOUT_MAX_SECONDS", defaultBashTimeoutMaxSeconds)
	if err != nil {
		return runkind.Spec{}, err
	}

	toolOutputMax, err := envInt("CMX_TOOL_OUTPUT_MAX_BYTES", defaultToolOutputMaxBytes)
	if err != nil {
		return runkind.Spec{}, err
	}

	defaultModel := os.Getenv("CMX_DEFAULT_MODEL")
	if defaultModel == "" {
		defaultModel = config.DefaultCapableModel
	}

	return runkind.Spec{
		Kind:            kind,
		CardID:          cardID,
		Project:         project,
		RepoURL:         repoURL,
		BaseBranch:      os.Getenv("CM_BASE_BRANCH"),
		SecretsPath:     cmEnvFile,
		CACertFile:      os.Getenv("CMX_CA_CERT_FILE"),
		Workspace:       runKindWorkspace,
		InputsDir:       runKindInputsDir,
		RunLogPath:      filepath.Join(runinput.MountPath, runinput.RunLogFile),
		Model:           os.Getenv("CM_MODEL"),
		DefaultModel:    defaultModel,
		ReasoningEffort: os.Getenv("CMX_REASONING_EFFORT"),
		Selection:       selectionFromEnv(cardID, project),
		MaxCostUSD:      maxCost,
		BashTimeoutMax:  bashTimeoutMax,
		ToolOutputMax:   toolOutputMax,
	}, nil
}

// readRunKindFiles fills spec's secrets from src, read as work reads them, and
// its input from inputPath.
func readRunKindFiles(spec runkind.Spec, src *secrets.Source, inputPath string) (runkind.Spec, error) {
	spec.GitToken = src.Get("CM_GIT_TOKEN")
	spec.LLMKey, spec.LLMBaseURL, spec.LLMType, spec.LLMOAuthToken, spec.LLMEffort = llmValues(src)

	input, err := os.ReadFile(inputPath) //nolint:gosec // G304: the fixed input mount path
	if err != nil {
		return spec, fmt.Errorf("read run-kind input: %w", err)
	}

	spec.Input = input

	return spec, nil
}

// runKindClient builds the run's LLM client as work does when serve staged LLM
// access, which it does exactly for the kinds that need a model; otherwise nil.
func runKindClient(spec runkind.Spec, src *secrets.Source) (llm.LLM, error) {
	if !llmStaged(src) {
		return nil, nil
	}

	caLLMOpts, _, err := caInjections(spec.CACertFile)
	if err != nil {
		return nil, err
	}

	return newLLMClient(spec.LLMKey, spec.LLMBaseURL, spec.LLMType, caLLMOpts), nil
}

// llmStaged reports whether the secrets file carries LLM access. Only the
// file counts: an env value such as a worker_extra_env entry is not staged.
func llmStaged(src *secrets.Source) bool {
	for _, k := range []string{"LLM_TYPE", "LLM_BASE_URL", "LLM_API_KEY", "LLM_OAUTH_TOKEN"} {
		if src.Get(k) != "" {
			return true
		}
	}

	return false
}
