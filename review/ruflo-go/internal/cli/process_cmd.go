package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newProcessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "process",
		Short: "Background process management (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List managed processes",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "process list: (empty — placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "start",
			Short: "Start a named process",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "process start: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Stop a process",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "process stop: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Process status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "process status: placeholder")
				return err
			},
		},
	)
	return cmd
}
