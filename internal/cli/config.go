package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-agent/internal/config"
)

// newConfigCmd groups the config inspection commands contextmatrix-setup
// drives: defaults (schema) and validate (would serve start with this file).
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print default or validate a service configuration",
	}
	cmd.AddCommand(newConfigDefaultsCmd())
	cmd.AddCommand(newConfigValidateCmd())

	return cmd
}

func newConfigDefaultsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "defaults",
		Short: "Print the complete default service configuration as YAML",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := config.DefaultsYAML()
			if err != nil {
				return err
			}

			_, err = cmd.OutOrStdout().Write(out)

			return err
		},
	}
}

func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file>",
		Short: "Load <file> exactly as serve would; exit 1 on the first error",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]

			if _, err := os.Stat(path); err != nil {
				return err
			}

			cfg, err := config.LoadService(path)
			if err != nil {
				return err
			}

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("invalid service config: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s: ok\n", path)

			return nil
		},
	}
}
