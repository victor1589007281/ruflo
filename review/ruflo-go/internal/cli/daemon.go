package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Background daemon (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "start",
			Short: "Start daemon",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "Daemon started")
				return err
			},
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Stop daemon",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "Daemon stopped")
				return err
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Daemon status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "Daemon status: not running (placeholder)")
				return err
			},
		},
	)
	return cmd
}
