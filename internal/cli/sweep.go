package cli

import "github.com/spf13/cobra"

func newSweepCmd() *cobra.Command {
	return &cobra.Command{Use: "sweep", Short: "weak vs control sweep (stub)"}
}
