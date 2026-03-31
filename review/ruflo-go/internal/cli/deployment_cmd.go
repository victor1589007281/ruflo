package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newDeploymentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deployment",
		Short: "Deployment management (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "deploy",
			Short: "Deploy to environment",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "deployment deploy: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "rollback",
			Short: "Rollback deployment",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "deployment rollback: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Deployment status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "deployment status: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "environments",
			Short: "List environments",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "environments: dev, staging, prod (placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "release",
			Short: "Create or list release",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "deployment release: placeholder")
				return err
			},
		},
	)
	return cmd
}
