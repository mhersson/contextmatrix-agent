package config

import (
	"fmt"
	"log/slog"
	"os"
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

// CompactionConfig configures the worker harness loop's optional in-window
// context compaction. Enabled=false (the default) preserves the hard
// context_limit stop; Threshold is the fraction of the context window at which
// compaction fires, and KeepRecentTurns is how many recent turns to keep
// verbatim.
type CompactionConfig struct {
	Enabled         bool
	Threshold       float64
	KeepRecentTurns int
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
	ImageListFilters          []string
	MaxConcurrent             int
	ContainerTimeout          time.Duration
	ContainerMemoryBytes      int64
	ContainerPidsLimit        int64
	IdleOutputTimeout         time.Duration
	IdleWatchdogInterval      time.Duration
	SecretsDir                string
	WorkerExtraEnv            map[string]string
	ReplaySkew                time.Duration
	ReplayCacheSize           int
	MessageDedupTTL           time.Duration
	MessageDedupCacheSize     int
	BashTimeoutMaxSeconds     int
	ToolOutputMaxBytes        int
	DefaultModel              string
	ReasoningEffort           string
	LogLevel                  string

	// LogDir is an optional directory on the serve host for per-card raw
	// container-output logs. When set, the agent writes one append file per
	// card, <log_dir>/<project>/<card_id>.log, holding everything the container
	// printed (as `docker logs -f` shows it). Files are 0600 and MAY contain
	// unredacted secrets from model/tool output. Empty (the default) disables
	// the feature. Logs are kept indefinitely - use an external logrotate.
	LogDir string

	// CACertFile is an optional path (on the serve host) to a PEM file of extra
	// CA certificates for corporate TLS interception / a private-CA GitHub
	// Enterprise. When set, it is bind-mounted read-only into each worker
	// container and the worker trusts it for its own outbound TLS (harness LLM
	// client, MCP/cmclient) and its git/gh subprocesses. Empty disables the
	// feature. Container-only: the serve host itself uses the OS trust store.
	CACertFile string

	// MaxCardCost is the cumulative USD ceiling per card. Workers receive it as
	// CMX_MAX_CARD_COST. Zero disables the ceiling; the default (5.0) applies
	// when the key is absent from config and env - koanf cannot distinguish
	// absent-vs-zero, so an explicit 0 in YAML or env also disables the ceiling.
	MaxCardCost float64

	// SelectorPriceHeadroom is the best-value band multiplier used by the model
	// selector. Workers receive it as CMX_SELECTOR_PRICE_HEADROOM. Zero is
	// omitted from the container env (worker uses its own default); the default
	// (1.5) applies when the key is absent from config and env.
	SelectorPriceHeadroom float64

	// AdminPort is the admin listener that serves Prometheus /metrics. Zero
	// disables it (the default). Workers never see it; it is host-side only.
	AdminPort int

	// AdminBindAddr is the address the admin listener binds to. Default
	// 127.0.0.1 (loopback-only); set a LAN address to let an external
	// Prometheus scrape /metrics. Restrict access with a firewall when
	// binding beyond loopback.
	AdminBindAddr string

	// MetricsToken is the static bearer token accepted on GET /metrics as an
	// alternative to the signed-GET HMAC, for scrapers that cannot sign
	// requests. Empty keeps /metrics HMAC-only.
	MetricsToken string

	// Compaction configures optional in-window context compaction in the worker
	// harness loop. Disabled by default (behavior-neutral): the agent keeps its
	// hard context_limit stop. When enabled, the settings reach each worker via
	// CMX_COMPACTION_* and become harness.Config.Compaction.
	Compaction CompactionConfig
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
	AdminPort                 int               `koanf:"admin_port"`
	AdminBindAddr             string            `koanf:"admin_bind_addr"`
	MetricsToken              string            `koanf:"metrics_token"`
	BaseImage                 string            `koanf:"base_image"`
	ImagePullPolicy           string            `koanf:"image_pull_policy"`
	ImageListFilters          []string          `koanf:"image_list_filters"`
	MaxConcurrent             int               `koanf:"max_concurrent"`
	ContainerTimeout          string            `koanf:"container_timeout"`
	ContainerMemoryLimit      int64             `koanf:"container_memory_limit"`
	ContainerPidsLimit        int64             `koanf:"container_pids_limit"`
	IdleOutputTimeout         string            `koanf:"idle_output_timeout"`
	IdleWatchdogInterval      string            `koanf:"idle_watchdog_interval"`
	SecretsDir                string            `koanf:"secrets_dir"`
	CACertFile                string            `koanf:"ca_cert_file"`
	LogDir                    string            `koanf:"log_dir"`
	WorkerExtraEnv            map[string]string `koanf:"worker_extra_env"`
	ReplaySkewSeconds         int               `koanf:"webhook_replay_skew_seconds"`
	ReplayCacheSize           int               `koanf:"webhook_replay_cache_size"`
	MessageDedupTTLSeconds    int               `koanf:"message_dedup_ttl_seconds"`
	MessageDedupCacheSize     int               `koanf:"message_dedup_cache_size"`
	BashTimeoutMaxSeconds     int               `koanf:"bash_timeout_max_seconds"`
	ToolOutputMaxBytes        int               `koanf:"tool_output_max_bytes"`
	DefaultModel              string            `koanf:"default_model"`
	ReasoningEffort           string            `koanf:"reasoning_effort"`
	LogLevel                  string            `koanf:"log_level"`
	MaxCardCost               float64           `koanf:"max_card_cost"`
	SelectorPriceHeadroom     float64           `koanf:"selector_price_headroom"`
	CompactionEnabled         bool              `koanf:"compaction_enabled"`
	CompactionThreshold       float64           `koanf:"compaction_threshold"`
	CompactionKeepRecentTurns int               `koanf:"compaction_keep_recent_turns"`
}

// serviceDefaults is the lowest-precedence layer. Durations are wire-form
// strings/ints here so the structs provider feeds them to koanf identically to
// a file or env value.
func serviceDefaults() serviceRaw {
	return serviceRaw{
		Port:                   9092,
		AdminBindAddr:          "127.0.0.1",
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
		ToolOutputMaxBytes:     131072,
		MaxCardCost:            5.0,
		SelectorPriceHeadroom:  1.5,
		// CompactionEnabled defaults to false (behavior-neutral); threshold and
		// keep-recent carry sane values for when an operator opts in.
		CompactionThreshold:       0.85,
		CompactionKeepRecentTurns: 6,
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

	// CMX_FOO_BAR -> "foo_bar"; nested keys use "__": CMX_WORKER_EXTRA_ENV__FOO
	// -> "worker_extra_env.foo".
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

	imageListFilters := r.ImageListFilters
	if len(imageListFilters) == 0 {
		// Omitted or explicitly empty both fall back to the family default so
		// a misconfiguration can never expose the node's whole image inventory.
		imageListFilters = []string{"contextmatrix-agent"}
	}

	return &ServiceConfig{
		ContextMatrixURL:          r.ContextMatrixURL,
		ContainerContextMatrixURL: r.ContainerContextMatrixURL,
		APIKey:                    r.APIKey,
		MCPAPIKey:                 r.MCPAPIKey,
		Port:                      r.Port,
		AdminPort:                 r.AdminPort,
		AdminBindAddr:             r.AdminBindAddr,
		MetricsToken:              r.MetricsToken,
		BaseImage:                 r.BaseImage,
		ImagePullPolicy:           r.ImagePullPolicy,
		ImageListFilters:          imageListFilters,
		MaxConcurrent:             r.MaxConcurrent,
		ContainerTimeout:          containerTimeout,
		ContainerMemoryBytes:      r.ContainerMemoryLimit,
		ContainerPidsLimit:        r.ContainerPidsLimit,
		IdleOutputTimeout:         idleOutput,
		IdleWatchdogInterval:      idleWatchdog,
		SecretsDir:                r.SecretsDir,
		CACertFile:                r.CACertFile,
		LogDir:                    r.LogDir,
		WorkerExtraEnv:            r.WorkerExtraEnv,
		ReplaySkew:                time.Duration(r.ReplaySkewSeconds) * time.Second,
		ReplayCacheSize:           r.ReplayCacheSize,
		MessageDedupTTL:           time.Duration(r.MessageDedupTTLSeconds) * time.Second,
		MessageDedupCacheSize:     r.MessageDedupCacheSize,
		BashTimeoutMaxSeconds:     r.BashTimeoutMaxSeconds,
		ToolOutputMaxBytes:        r.ToolOutputMaxBytes,
		DefaultModel:              r.DefaultModel,
		ReasoningEffort:           r.ReasoningEffort,
		LogLevel:                  r.LogLevel,
		MaxCardCost:               r.MaxCardCost,
		SelectorPriceHeadroom:     r.SelectorPriceHeadroom,
		Compaction: CompactionConfig{
			Enabled:         r.CompactionEnabled,
			Threshold:       r.CompactionThreshold,
			KeepRecentTurns: r.CompactionKeepRecentTurns,
		},
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
// BaseImage and a non-canonical ReasoningEffort tier are both permitted but
// warn via slog so operators notice tag drift or provider-specific values.
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

	for _, f := range c.ImageListFilters {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("image_list_filters entries must be non-empty")
		}
	}

	switch c.ReasoningEffort {
	case "", "low", "medium", "high":
	default:
		slog.Warn("reasoning_effort is not a canonical tier (low|medium|high); "+
			"forwarding to the provider as-is",
			"reasoning_effort", c.ReasoningEffort)
	}

	if c.MaxConcurrent < 1 {
		return fmt.Errorf(
			"max_concurrent must be >= 1, got %d: 0 disables the webhook capacity pre-check "+
				"while the tracker refuses every launch - triggers would be accepted then all fail",
			c.MaxConcurrent)
	}

	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be in 1..65535, got %d", c.Port)
	}

	if c.AdminPort != 0 && (c.AdminPort < 1 || c.AdminPort > 65535) {
		return fmt.Errorf("admin_port must be 0 (disabled) or in 1..65535, got %d", c.AdminPort)
	}

	if c.AdminPort != 0 && c.AdminPort == c.Port {
		return fmt.Errorf("admin_port must differ from port (both set to %d)", c.Port)
	}

	if c.AdminBindAddr == "" {
		c.AdminBindAddr = "127.0.0.1"
	}

	if c.ContainerTimeout > reconcileCap {
		return fmt.Errorf(
			"container_timeout %s exceeds the 150m reconcile cap: ContextMatrix force-kills "+
				"containers older than 150m externally, so the agent's own watchdog must fire first",
			c.ContainerTimeout)
	}

	if c.MaxCardCost < 0 {
		return fmt.Errorf("max_card_cost must be >= 0 (0 disables the ceiling), got %g", c.MaxCardCost)
	}

	if c.SelectorPriceHeadroom < 0 || (c.SelectorPriceHeadroom > 0 && c.SelectorPriceHeadroom < 1) {
		return fmt.Errorf(
			"selector_price_headroom must be 0 (use worker default) or >= 1 (band multiplier), got %g",
			c.SelectorPriceHeadroom)
	}

	if c.CACertFile != "" {
		if _, err := os.Stat(c.CACertFile); err != nil {
			return fmt.Errorf("ca_cert_file %q: %w", c.CACertFile, err)
		}
	}

	if c.LogDir != "" {
		if err := os.MkdirAll(c.LogDir, 0o700); err != nil {
			return fmt.Errorf("log_dir %q: %w", c.LogDir, err)
		}

		f, err := os.CreateTemp(c.LogDir, ".cm-agent-write-*")
		if err != nil {
			return fmt.Errorf("log_dir %q not writable: %w", c.LogDir, err)
		}

		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
	}

	if c.Compaction.Enabled {
		if c.Compaction.Threshold <= 0 || c.Compaction.Threshold > 1 {
			return fmt.Errorf(
				"compaction_threshold must be in (0,1] when compaction is enabled, got %g", c.Compaction.Threshold)
		}

		if c.Compaction.KeepRecentTurns < 1 {
			return fmt.Errorf(
				"compaction_keep_recent_turns must be >= 1 when compaction is enabled, got %d", c.Compaction.KeepRecentTurns)
		}
	}

	return nil
}
