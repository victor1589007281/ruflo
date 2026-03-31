package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newAnalyzeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Code / diff analysis (stubs)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "diff",
			Short: "Analyze git diff (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "analyze diff: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "diff-risk",
			Short: "Risk score for diff (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "analyze diff-risk: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "diff-classify",
			Short: "Classify diff (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "analyze diff-classify: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "file-risk",
			Short: "Per-file risk (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "analyze file-risk: placeholder")
				return err
			},
		},
		&cobra.Command{
			Use:   "diff-stats",
			Short: "Diff statistics (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "analyze diff-stats: placeholder")
				return err
			},
		},
	)
	return cmd
}
