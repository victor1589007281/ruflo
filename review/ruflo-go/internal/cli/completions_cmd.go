package cli

import (
	"os"

	"github.com/spf13/cobra"
)

func newCompletionsCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completions",
		Short: "Generate shell completion scripts",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "bash",
			Short: "Bash completions",
			RunE: func(cmd *cobra.Command, args []string) error {
				return root.GenBashCompletion(os.Stdout)
			},
		},
		&cobra.Command{
			Use:   "zsh",
			Short: "Zsh completions",
			RunE: func(cmd *cobra.Command, args []string) error {
				return root.GenZshCompletion(os.Stdout)
			},
		},
		&cobra.Command{
			Use:   "fish",
			Short: "Fish completions",
			RunE: func(cmd *cobra.Command, args []string) error {
				return root.GenFishCompletion(os.Stdout, true)
			},
		},
		&cobra.Command{
			Use:   "powershell",
			Short: "PowerShell completions",
			RunE: func(cmd *cobra.Command, args []string) error {
				return root.GenPowerShellCompletion(os.Stdout)
			},
		},
	)
	return cmd
}
