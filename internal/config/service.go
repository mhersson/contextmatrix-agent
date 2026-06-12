package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// reconcileCap is the ceiling on ContainerTimeout. ContextMatrix's reconcile
// sweep force-kills agent containers older than 150 minutes from the outside;
// the agent's own watchdog must fire first so cleanup and the failed status
// callback happen properly. A timeout above this races the external kill and
// loses.
const reconcileCap = 150 * time.Minute

// defaultSecretsDir is a filesystem PATH, not a credential. Naming it via a
// const avoids the gosec G101 false-positive that fires on the path literal
// inside the defaults struct.
const defaultSecretsDir = "/var/run/cm-agent/secrets" //nolint:gosec // path, not a credential

// GitHubAppConfig holds GitHub App credentials for minting installation tokens.
// Mirrors the runner's field shape so operators carry one mental model.
type GitHubAppConfig struct {
	AppID          int64  `koanf:"app_id"`
	InstallationID int64  `koanf:"installation_id"`
	PrivateKeyPath string `koanf:"private_key_path"`
}

// GitHubPATConfig holds a fine-grained personal access token used instead of a
// GitHub App where App creation is restricted.
type GitHubPATConfig struct {
	Token string `koanf:"token"`
}

// GitHubConfig is the unified GitHub auth block. Set AuthMode to "app" or "pat".
type GitHubConfig struct {
	AuthMode   string          `koanf:"auth_mode"`
	APIBaseURL string          `koanf:"api_base_url"`
	App        GitHubAppConfig `koanf:"app"`
	PAT        GitHubPATConfig `koanf:"pat"`
}

// ServiceConfig is the host-side agent service configuration. ContextMatrix
// POSTs lifecycle webhooks at the service; it launches one worker container per
// card. Durations carried as "<n>_seconds" YAML keys are plain ints in the wire
// form and converted on load; the rest are Go duration strings (e.g. "2h30m").
type ServiceConfig struct {
	ContextMatrixURL          string
	ContainerContextMatrixURL string
	APIKey                    string
	MCPAPIKey                 string
	Port                      int
	BaseImage                 string
	ImagePullPolicy           string
	MaxConcurrent             int
	ContainerTimeout          time.Duration
	ContainerMemoryBytes      int64
	ContainerPidsLimit        int64
	IdleOutputTimeout         time.Duration
	IdleWatchdogInterval      time.Duration
	SecretsDir                string
	OpenRouterAPIKey          string
	GitHub                    GitHubConfig
	WorkerExtraEnv            map[string]string
	ReplaySkew                time.Duration
	ReplayCacheSize           int
	MessageDedupTTL           time.Duration
	MessageDedupCacheSize     int
	BashTimeoutMaxSeconds     int
	ToolOutputMaxBytes        int
	DefaultModel              string
	LogLevel                  string
}

// serviceRaw is the koanf-unmarshalled wire shape. Duration fields are split:
// "<n>_seconds" keys are ints, free-form durations are strings parsed by
// time.ParseDuration. Keeping the wire shape separate from ServiceConfig means
// the typed public struct never carries half-parsed values.
type serviceRaw struct {
	ContextMatrixURL          string            `koanf:"contextmatrix_url"`
	ContainerContextMatrixURL string            `koanf:"container_contextmatrix_url"`
	APIKey                    string            `koanf:"api_key"`
	MCPAPIKey                 string            `koanf:"mcp_api_key"`
	Port                      int               `koanf:"port"`
	BaseImage                 string            `koanf:"base_image"`
	ImagePullPolicy           string            `koanf:"image_pull_policy"`
	MaxConcurrent             int               `koanf:"max_concurrent"`
	ContainerTimeout          string            `koanf:"container_timeout"`
	ContainerMemoryLimit      int64             `koanf:"container_memory_limit"`
	ContainerPidsLimit        int64             `koanf:"container_pids_limit"`
	IdleOutputTimeout         string            `koanf:"idle_output_timeout"`
	IdleWatchdogInterval      string            `koanf:"idle_watchdog_interval"`
	SecretsDir                string            `koanf:"secrets_dir"`
	OpenRouterAPIKey          string            `koanf:"openrouter_api_key"`
	GitHub                    GitHubConfig      `koanf:"github"`
	WorkerExtraEnv            map[string]string `koanf:"worker_extra_env"`
	ReplaySkewSeconds         int               `koanf:"webhook_replay_skew_seconds"`
	ReplayCacheSize           int               `koanf:"webhook_replay_cache_size"`
	MessageDedupTTLSeconds    int               `koanf:"message_dedup_ttl_seconds"`
	MessageDedupCacheSize     int               `koanf:"message_dedup_cache_size"`
	BashTimeoutMaxSeconds     int               `koanf:"bash_timeout_max_seconds"`
	ToolOutputMaxBytes        int               `koanf:"tool_output_max_bytes"`
	DefaultModel              string            `koanf:"default_model"`
	LogLevel                  string            `koanf:"log_level"`
}

// serviceDefaults is the lowest-precedence layer. Durations are wire-form
// strings/ints here so the structs provider feeds them to koanf identically to
// a file or env value.
func serviceDefaults() serviceRaw {
	return serviceRaw{
		Port:                   9092,
		ImagePullPolicy:        "if-not-present",
		MaxConcurrent:          5,
		ContainerTimeout:       "2h30m",
		ContainerMemoryLimit:   8 * 1024 * 1024 * 1024, // 8 GiB
		ContainerPidsLimit:     512,
		IdleOutputTimeout:      "30m",
		IdleWatchdogInterval:   "30s",
		SecretsDir:             defaultSecretsDir,
		ReplaySkewSeconds:      330,
		ReplayCacheSize:        10000,
		MessageDedupTTLSeconds: 600,
		MessageDedupCacheSize:  1000,
		BashTimeoutMaxSeconds:  600,
		ToolOutputMaxBytes:     30000,
	}
}

// LoadService merges defaults < file (if it loads) < env (CMX_*). A nonexistent
// path is not an error: the file layer is skipped and the result is
// defaults+env, matching config.go's Load behavior.
func LoadService(path string) (*ServiceConfig, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(serviceDefaults(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load service defaults: %w", err)
	}

	if path != "" {
		// A missing file is allowed (defaults+env only); other read/parse errors
		// are not.
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil && !isNotExist(err) {
			return nil, fmt.Errorf("load service config file %q: %w", path, err)
		}
	}

	// CMX_FOO_BAR -> "foo_bar"; nested keys use "__": CMX_GITHUB__AUTH_MODE ->
	// "github.auth_mode", CMX_GITHUB__PAT__TOKEN -> "github.pat.token".
	envCb := func(s string) string {
		s = strings.ToLower(strings.TrimPrefix(s, envPrefix))

		return strings.ReplaceAll(s, "__", ".")
	}
	if err := k.Load(env.Provider(envPrefix, ".", envCb), nil); err != nil {
		return nil, fmt.Errorf("load service env: %w", err)
	}

	var raw serviceRaw
	if err := k.UnmarshalWithConf("", &raw, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("unmarshal service config: %w", err)
	}

	return raw.toConfig()
}

// toConfig parses the wire-form durations and assembles the typed config.
func (r serviceRaw) toConfig() (*ServiceConfig, error) {
	containerTimeout, err := parseDurationField("container_timeout", r.ContainerTimeout)
	if err != nil {
		return nil, err
	}

	idleOutput, err := parseDurationField("idle_output_timeout", r.IdleOutputTimeout)
	if err != nil {
		return nil, err
	}

	idleWatchdog, err := parseDurationField("idle_watchdog_interval", r.IdleWatchdogInterval)
	if err != nil {
		return nil, err
	}

	return &ServiceConfig{
		ContextMatrixURL:          r.ContextMatrixURL,
		ContainerContextMatrixURL: r.ContainerContextMatrixURL,
		APIKey:                    r.APIKey,
		MCPAPIKey:                 r.MCPAPIKey,
		Port:                      r.Port,
		BaseImage:                 r.BaseImage,
		ImagePullPolicy:           r.ImagePullPolicy,
		MaxConcurrent:             r.MaxConcurrent,
		ContainerTimeout:          containerTimeout,
		ContainerMemoryBytes:      r.ContainerMemoryLimit,
		ContainerPidsLimit:        r.ContainerPidsLimit,
		IdleOutputTimeout:         idleOutput,
		IdleWatchdogInterval:      idleWatchdog,
		SecretsDir:                r.SecretsDir,
		OpenRouterAPIKey:          r.OpenRouterAPIKey,
		GitHub:                    r.GitHub,
		WorkerExtraEnv:            r.WorkerExtraEnv,
		ReplaySkew:                time.Duration(r.ReplaySkewSeconds) * time.Second,
		ReplayCacheSize:           r.ReplayCacheSize,
		MessageDedupTTL:           time.Duration(r.MessageDedupTTLSeconds) * time.Second,
		MessageDedupCacheSize:     r.MessageDedupCacheSize,
		BashTimeoutMaxSeconds:     r.BashTimeoutMaxSeconds,
		ToolOutputMaxBytes:        r.ToolOutputMaxBytes,
		DefaultModel:              r.DefaultModel,
		LogLevel:                  r.LogLevel,
	}, nil
}

// parseDurationField parses a Go duration string, returning a field-named error
// on failure. An empty string yields a zero duration (the validated invariants
// catch any that must be non-zero).
func parseDurationField(name, val string) (time.Duration, error) {
	if val == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", name, val, err)
	}

	return d, nil
}

// isNotExist reports whether err is a missing-file error from the file provider.
// The provider wraps os errors, so match on the message tail.
func isNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such file or directory")
}

// Validate checks the service config invariants after merging. A non-digest
// BaseImage is permitted but warns via slog so operators notice tag drift.
func (c *ServiceConfig) Validate() error {
	if c.ContextMatrixURL == "" {
		return fmt.Errorf("contextmatrix_url is required")
	}

	if c.APIKey == "" {
		return fmt.Errorf("api_key is required")
	}

	if len(c.APIKey) < 32 {
		return fmt.Errorf("api_key must be at least 32 characters, got %d", len(c.APIKey))
	}

	if c.OpenRouterAPIKey == "" {
		return fmt.Errorf("openrouter_api_key is required")
	}

	if c.BaseImage == "" {
		return fmt.Errorf("base_image is required")
	}

	if !strings.Contains(c.BaseImage, "@sha256:") {
		slog.Warn("base_image is not pinned to a digest; tag drift is possible",
			"base_image", c.BaseImage)
	}

	switch c.ImagePullPolicy {
	case "never", "if-not-present", "always":
	default:
		return fmt.Errorf("image_pull_policy must be never|if-not-present|always, got %q", c.ImagePullPolicy)
	}

	if c.MaxConcurrent < 1 {
		return fmt.Errorf(
			"max_concurrent must be >= 1, got %d: 0 disables the webhook capacity pre-check "+
				"while the tracker refuses every launch — triggers would be accepted then all fail",
			c.MaxConcurrent)
	}

	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be in 1..65535, got %d", c.Port)
	}

	if c.ContainerTimeout > reconcileCap {
		return fmt.Errorf(
			"container_timeout %s exceeds the 150m reconcile cap: ContextMatrix force-kills "+
				"containers older than 150m externally, so the agent's own watchdog must fire first",
			c.ContainerTimeout)
	}

	return c.GitHub.validate()
}

// validate checks the GitHub auth block, mirroring the runner's contract:
// exactly one auth path is populated per auth_mode.
func (g *GitHubConfig) validate() error {
	switch g.AuthMode {
	case "app":
		if g.App.AppID == 0 {
			return fmt.Errorf("github.app.app_id is required when github.auth_mode is \"app\"")
		}

		if g.App.InstallationID == 0 {
			return fmt.Errorf("github.app.installation_id is required when github.auth_mode is \"app\"")
		}

		if g.App.PrivateKeyPath == "" {
			return fmt.Errorf("github.app.private_key_path is required when github.auth_mode is \"app\"")
		}

		if g.PAT.Token != "" {
			return fmt.Errorf("github.pat.token must be empty when github.auth_mode is \"app\"")
		}
	case "pat":
		if g.PAT.Token == "" {
			return fmt.Errorf("github.pat.token is required when github.auth_mode is \"pat\"")
		}

		if g.App.AppID != 0 || g.App.InstallationID != 0 || g.App.PrivateKeyPath != "" {
			return fmt.Errorf("github.app.* must be empty when github.auth_mode is \"pat\"")
		}
	default:
		return fmt.Errorf("github.auth_mode is required: must be \"app\" or \"pat\" (got %q)", g.AuthMode)
	}

	return nil
}
