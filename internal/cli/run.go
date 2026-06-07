package cli

import "github.com/spf13/cobra"

func newRunCmd() *cobra.Command {
	return &cobra.Command{Use: "run", Short: "run the harness on one model (stub)"}
}
