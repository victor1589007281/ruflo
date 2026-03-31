package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newApplianceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "appliance",
		Short: "Appliance image build / run (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "build",
			Short: "Build appliance image",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance build: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "inspect",
			Short: "Inspect appliance manifest",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance inspect: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "verify",
			Short: "Verify appliance signature",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance verify: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "extract",
			Short: "Extract appliance layers",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance extract: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "run",
			Short: "Run appliance container",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance run: placeholder")
				return err
			},
		},
	)
	return cmd
}
