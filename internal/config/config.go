// Package config loads layered harness configuration: defaults < file < env <
// flags, via koanf. Pointer-optionals distinguish "unset" from a zero value.
package config

import (
	"fmt"
	"io"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

const envPrefix = "CMX_"

// Note: koanf keys are hyphenated so CLI flag names (which posflag uses verbatim
// as keys) match — e.g. --max-turns ⇒ key "max-turns" ⇒ this tag. Env vars map
// via the two-step transform in Load (CMX_MAX_TURNS ⇒ "max-turns").
type Config struct {
	Model            *string           `koanf:"model"`
	Models           []string          `koanf:"models"`
	MaxTurns         *int              `koanf:"max-turns"`
	MaxCostUSD       *float64          `koanf:"max-cost"`
	Roles            map[string]string `koanf:"roles"`
	CapableModel     *string           `koanf:"capable-model"`
	CapabilitiesFile *string           `koanf:"capabilities-file"`
	Provider         *ProviderConfig   `koanf:"provider"`
	Reasoning        *ReasoningConfig  `koanf:"reasoning"`
}

type ProviderConfig struct {
	RequireParameters *bool    `koanf:"require-parameters"`
	Order             []string `koanf:"order"`
	Sort              *string  `koanf:"sort"`
}

type ReasoningConfig struct {
	Effort    *string `koanf:"effort"`
	MaxTokens *int    `koanf:"max-tokens"`
	Exclude   *bool   `koanf:"exclude"`
}

// Defaults is the lowest-precedence layer.
func Defaults() Config {
	mt := 30
	mc := 0.50
	capable := "deepseek/deepseek-v4-flash"

	return Config{MaxTurns: &mt, MaxCostUSD: &mc, CapableModel: &capable}
}

// Validate checks invariants after merging.
func (c *Config) Validate() error {
	if c.MaxTurns != nil && *c.MaxTurns <= 0 {
		return fmt.Errorf("max-turns must be > 0, got %d", *c.MaxTurns)
	}

	if c.MaxCostUSD != nil && *c.MaxCostUSD < 0 {
		return fmt.Errorf("max-cost must be >= 0, got %v", *c.MaxCostUSD)
	}

	return nil
}

// Load merges defaults < file (if non-empty) < env (CMX_*) < flags. flags may be
// nil. Unpassed flags do not clobber lower layers (posflag honors fs.Changed).
func Load(flags *pflag.FlagSet, configFile string) (Config, error) {
	k := koanf.New(".")
	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return Config{}, fmt.Errorf("load defaults: %w", err)
	}

	if configFile != "" {
		if err := k.Load(file.Provider(configFile), yaml.Parser()); err != nil {
			return Config{}, fmt.Errorf("load config file %q: %w", configFile, err)
		}
	}

	envCb := func(s string) string {
		s = strings.ToLower(strings.TrimPrefix(s, envPrefix))
		s = strings.ReplaceAll(s, "__", ".") // nested-key separator: CMX_PROVIDER__SORT -> provider.sort

		return strings.ReplaceAll(s, "_", "-") // word separator: CMX_MAX_TURNS -> max-turns
	}
	if err := k.Load(env.Provider(envPrefix, ".", envCb), nil); err != nil {
		return Config{}, fmt.Errorf("load env: %w", err)
	}

	if flags != nil {
		if err := k.Load(posflag.Provider(flags, ".", k), nil); err != nil {
			return Config{}, fmt.Errorf("load flags: %w", err)
		}
	}

	var c Config
	if err := k.UnmarshalWithConf("", &c, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}

	return c, nil
}

// PrintRedacted writes the effective config with secrets masked. The OpenRouter
// API key is never part of Config (env-only) and is shown here as a reminder.
// fmt.Fprint* return values are intentionally ignored (errcheck would flag them
// as B0 does, so each carries //nolint:errcheck). Labels are hyphenated to match
// the koanf keys users write in YAML.
func PrintRedacted(w io.Writer, c Config) {
	fmt.Fprintln(w, "# effective configuration (secrets redacted)")                      //nolint:errcheck
	fmt.Fprintf(w, "openrouter-api-key: %s\n", "<redacted; set via OPENROUTER_API_KEY>") //nolint:errcheck
	fmt.Fprintf(w, "model: %s\n", strDeref(c.Model))                                     //nolint:errcheck
	fmt.Fprintf(w, "models: %v\n", c.Models)                                             //nolint:errcheck
	fmt.Fprintf(w, "max-turns: %s\n", intDeref(c.MaxTurns))                              //nolint:errcheck
	fmt.Fprintf(w, "max-cost: %s\n", floatDeref(c.MaxCostUSD))                           //nolint:errcheck
	fmt.Fprintf(w, "capable-model: %s\n", strDeref(c.CapableModel))                      //nolint:errcheck
	fmt.Fprintf(w, "capabilities-file: %s\n", strDeref(c.CapabilitiesFile))              //nolint:errcheck
	fmt.Fprintf(w, "roles: %v\n", c.Roles)                                               //nolint:errcheck

	if c.Provider != nil {
		fmt.Fprintf(w, "provider: {require-parameters:%s order:%v sort:%s}\n", //nolint:errcheck
			boolDeref(c.Provider.RequireParameters), c.Provider.Order, strDeref(c.Provider.Sort))
	}

	if c.Reasoning != nil {
		fmt.Fprintf(w, "reasoning: {effort:%s max-tokens:%s exclude:%s}\n", //nolint:errcheck
			strDeref(c.Reasoning.Effort), intDeref(c.Reasoning.MaxTokens), boolDeref(c.Reasoning.Exclude))
	}
}

func strDeref(p *string) string {
	if p == nil {
		return "(unset)"
	}

	return *p
}

func intDeref(p *int) string {
	if p == nil {
		return "(unset)"
	}

	return fmt.Sprintf("%d", *p)
}

func floatDeref(p *float64) string {
	if p == nil {
		return "(unset)"
	}

	return fmt.Sprintf("%g", *p)
}

func boolDeref(p *bool) string {
	if p == nil {
		return "(unset)"
	}

	return fmt.Sprintf("%v", *p)
}
