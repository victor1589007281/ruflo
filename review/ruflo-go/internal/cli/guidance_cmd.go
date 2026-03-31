package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newGuidanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guidance",
		Short: "Governance / guidance control plane",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "compile",
			Short: "Compile guidance sources (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "guidance compile: load markdown via EvaluateCommandMCP in runtime (placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "retrieve",
			Short: "Retrieve shards for intent",
			RunE: func(cmd *cobra.Command, args []string) error {
				intent := "general"
				if len(args) > 0 {
					intent = args[0]
				}
				return RunTool(cmd.Context(), "guidance_recommend", map[string]any{"intent": intent})
			},
		},
		&cobra.Command{
			Use:   "enforce",
			Short: "Evaluate command against gates (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "guidance enforce: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "discover",
			Short: "Guidance capabilities manifest",
			RunE: func(cmd *cobra.Command, args []string) error {
				return RunTool(cmd.Context(), "guidance_capabilities", map[string]any{})
			},
		},
		&cobra.Command{
			Use:   "workflow",
			Short: "Guidance workflow hook (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "guidance workflow: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "quickref",
			Short: "Quick reference (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "guidance quickref: see docs (placeholder)")
				return err
			},
		},
	)
	return cmd
}
