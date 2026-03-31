package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Workflow execution (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "run",
			Short: "Run a workflow by name",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "workflow run: not implemented (placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List workflows",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "(no workflows registered — placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Workflow status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "workflow status: idle (placeholder)")
				return err
			},
		},
	)
	return cmd
}
