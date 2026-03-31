package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newPerformanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "performance",
		Short: "Performance benchmark, profile, and report (placeholders)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "benchmark",
			Short: "Run benchmarks",
			RunE: func(cmd *cobra.Command, args []string) error {
				start := time.Now()
				time.Sleep(2 * time.Millisecond)
				_, err := fmt.Fprintf(Stdout(), "benchmark: completed in %s (placeholder)\n", time.Since(start))
				return err
			},
		},
		&cobra.Command{
			Use:   "profile",
			Short: "CPU/memory profile (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "profile: no samples collected (placeholder)")
				return err
			},
		},
		&cobra.Command{
			Use:   "report",
			Short: "Performance report summary",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "performance report: run benchmark and profile subcommands (placeholder)")
				return err
			},
		},
	)
	return cmd
}
