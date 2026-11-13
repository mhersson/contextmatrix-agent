package webhook

import (
	"github.com/mhersson/contextmatrix-agent/internal/executor"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// buildRunKindSpec folds a run-kind payload together with the static LaunchEnv
// into the run's launch spec. A run kind gets no MCP URL or key in its env and
// no skills; MCPURL only pins the ContextMatrix host in its /etc/hosts. Its
// git token and LLM values ride the per-run secrets file, as for a card run;
// its input dir is set by the launch once it exists.
func (s *Server) buildRunKindSpec(p protocol.RunKindPayload, correlationID string) executor.LaunchSpec {
	k := executor.RunKey{Project: p.Project, CardID: p.CardID, Kind: p.Kind}

	image := s.launchEnv.BaseImage
	if p.WorkerImage != "" {
		image = p.WorkerImage
	}

	env := []string{
		"CM_RUN_KIND=" + p.Kind,
		"CM_CARD_ID=" + p.CardID,
		"CM_PROJECT=" + p.Project,
		"CM_REPO_URL=" + p.RepoURL,
		"CM_BASE_BRANCH=" + p.BaseBranch,
	}

	if usesModel(p.Kind) {
		env = append(env, "CM_MODEL="+p.Model, "CM_MAX_COST_USD="+formatFloat(p.MaxCostUSD))
		env = append(env, s.selectionEnv(k, p.Selection)...)
	}

	env = append(env, s.workerKnobEnv()...)
	env = append(env, s.launchEnv.WorkerExtraEnv...)

	secretsHostDir := ""
	if s.credentials != nil {
		secretsHostDir = s.credentials.HostDir(p.Project, k.Name())
	}

	return executor.LaunchSpec{
		CardID:         p.CardID,
		Project:        p.Project,
		Kind:           p.Kind,
		Image:          image,
		Env:            env,
		SecretsHostDir: secretsHostDir,
		CACertHostFile: s.launchEnv.CACertFile,
		MemoryBytes:    s.launchEnv.MemoryBytes,
		PidsLimit:      s.launchEnv.PidsLimit,
		CorrelationID:  correlationID,
		MCPURL:         s.launchEnv.MCPURL,
		Attempt:        1,
	}
}
