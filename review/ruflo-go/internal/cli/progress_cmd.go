package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newProgressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "progress",
		Short: "Workflow / session progress (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "check",
			Short: "Check progress state",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "progress check: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "sync",
			Short: "Sync progress to remote (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "progress sync: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "summary",
			Short: "Progress summary",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "progress summary: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "watch",
			Short: "Watch progress stream (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "progress watch: placeholder")
				return err
			},
		},
	)
	return cmd
}
