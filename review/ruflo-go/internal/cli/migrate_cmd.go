package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "V2 to V3 migration (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "run",
			Short: "Run pending migrations",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "migrate run: no pending migrations (placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Migration status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "migrate status: up to date (placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "rollback",
			Short: "Rollback last migration batch",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "migrate rollback: not supported in Go stub")
				return err
			},
		},
	)
	return cmd
}
