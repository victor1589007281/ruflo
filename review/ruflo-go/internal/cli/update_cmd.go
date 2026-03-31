package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "CLI self-update (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "check",
			Short: "Check for updates",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "update check: current", version, "(placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "apply",
			Short: "Apply update",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "update apply: not implemented in Go build")
				return err
			},
		},
	)
	return cmd
}
