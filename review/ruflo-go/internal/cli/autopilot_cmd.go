package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newAutopilotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autopilot",
		Short: "Autopilot mode (placeholder)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "enable",
			Short: "Enable autopilot",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "autopilot enable: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "disable",
			Short: "Disable autopilot",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "autopilot disable: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Autopilot status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "autopilot status: disabled (placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "config",
			Short: "Show or set autopilot config (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "autopilot config: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "reset",
			Short: "Reset autopilot state",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "autopilot reset: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "log",
			Short: "Autopilot decision log (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "autopilot log: (empty — placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "progress",
			Short: "Autopilot progress (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "autopilot progress: placeholder")
				return err
			},
		},
	)
	return cmd
}
