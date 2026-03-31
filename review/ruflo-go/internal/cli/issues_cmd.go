package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newIssuesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "issues",
		Short: "Issue tracker helpers (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "claim",
			Short: "Claim an issue id",
			RunE: func(cmd *cobra.Command, args []string) error {
				if len(args) < 1 {
					return fmt.Errorf("issue id required")
				}
				_, err := fmt.Fprintf(Stdout(), "issues claim %s: placeholder\n", args[0])
				return err
			},
		},
		&cobra.Command{
			Use:   "release",
			Short: "Release issue claim",
			RunE: func(cmd *cobra.Command, args []string) error {
				if len(args) < 1 {
					return fmt.Errorf("issue id required")
				}
				_, err := fmt.Fprintf(Stdout(), "issues release %s: placeholder\n", args[0])
				return err
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List issues (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "issues list: placeholder")
				return err
			},
		},
	)
	return cmd
}
