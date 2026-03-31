package cli

import (
	"github.com/spf13/cobra"
)

func newBenchmarkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Performance benchmarks",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "run",
			Short: "Run a named benchmark",
			RunE: func(cmd *cobra.Command, args []string) error {
				name := "default"
				if len(args) > 0 {
					name = args[0]
				}
				return RunTool(cmd.Context(), "performance_benchmark", map[string]any{"name": name})
			},
		},
		&cobra.Command{
			Use:   "report",
			Short: "Write benchmark report",
			RunE: func(cmd *cobra.Command, args []string) error {
				return RunTool(cmd.Context(), "performance_report", map[string]any{})
			},
		},
		&cobra.Command{
			Use:   "compare",
			Short: "Compare benchmark runs (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				return RunTool(cmd.Context(), "performance_metrics", map[string]any{})
			},
		},
	)
	return cmd
}
