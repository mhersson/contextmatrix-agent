package cli

import (
	"context"
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
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/spf13/cobra"
)

// The container paths a run kind works in.
const (
	runKindWorkspace = "/home/user/workspace"
	runKindInputsDir = "/home/user/investigation"
)

// runKindMain runs one run kind and writes its outcome as the stream's last
// line. Any cause but done exits non-zero.
func runKindMain(cmd *cobra.Command, kind string) error {
	emit := events.NewEmitter(io.Discard, cmd.OutOrStdout())

	data := startRunKind(cmd.Context(), kind, emit)
	runoutcome.Emit(emit, data)

	if data.Cause != protocol.StopCauseDone {
		return fmt.Errorf("run kind %s ended with cause %s", kind, data.Cause)
	}

	return nil
}

// startRunKind reads the run's spec and files, then runs it. A run that cannot
// start ends with cause error.
func startRunKind(ctx context.Context, kind string, emit *events.Emitter) runoutcome.Data {
	spec, err := runKindSpecFromEnv(kind)
	if err == nil {
		spec, err = readRunKindFiles(spec, filepath.Join(runinput.MountPath, runinput.InputFile))
	}

	if err != nil {
		slog.Error("run kind could not start", "kind", kind, "error", err)

		return runoutcome.Data{Cause: protocol.StopCauseError}
	}

	return runkind.Run(ctx, spec, nil, emit)
}

// runKindSpecFromEnv builds a run kind's spec from the env serve writes. It
// needs no CM_MCP_URL or CM_MCP_API_KEY: a run kind gets neither.
func runKindSpecFromEnv(kind string) (runkind.Spec, error) {
	cardID, err := requireEnv("CM_CARD_ID")
	if err != nil {
		return runkind.Spec{}, err
	}

	if !cardid.Valid(cardID) {
		return runkind.Spec{}, fmt.Errorf("env var CM_CARD_ID: invalid card ID %q (want PREFIX-NNN)", cardID)
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

// readRunKindFiles fills spec's secrets from its secrets file, read as work
// reads them, and its input from inputPath.
func readRunKindFiles(spec runkind.Spec, inputPath string) (runkind.Spec, error) {
	src, err := secrets.Open(spec.SecretsPath)
	if err != nil {
		return spec, fmt.Errorf("read secrets: %w", err)
	}

	spec.GitToken = src.Get("CM_GIT_TOKEN")
	spec.LLMKey = resolveLLMValue("LLM_API_KEY", src)
	spec.LLMBaseURL = resolveLLMValue("LLM_BASE_URL", src)
	spec.LLMType = resolveLLMValue("LLM_TYPE", src)
	spec.LLMOAuthToken = src.Get("LLM_OAUTH_TOKEN")
	spec.LLMEffort = src.Get("LLM_EFFORT")

	input, err := os.ReadFile(inputPath) //nolint:gosec // G304: the fixed input mount path
	if err != nil {
		return spec, fmt.Errorf("read run-kind input: %w", err)
	}

	spec.Input = input

	return spec, nil
}
