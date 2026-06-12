package cli

import "github.com/spf13/cobra"

// NewRootCmd builds the contextmatrix-agent CLI root.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "contextmatrix-agent",
		Short:         "ContextMatrix agent harness (B0 spike)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newRunCmd())
	root.AddCommand(newSweepCmd())
	root.AddCommand(newFanoutCmd())
	root.AddCommand(newEvalCmd())
	root.AddCommand(newWorkCmd())

	return root
}
