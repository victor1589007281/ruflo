package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// 本文件实现 performance 命令组：基准、采样画像与报告的占位实现，用于 CLI 面对齐与未来接入真实性能子系统。

// newPerformanceCmd 构建「performance」根子命令，内联 benchmark/profile/report 三个占位子命令。
func newPerformanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "performance",
		Short: "Performance benchmark, profile, and report (placeholders)",
	}
	cmd.AddCommand(
		// benchmark：占位，短暂 sleep 后打印耗时。
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
		// profile：占位，提示未采集样本。
		&cobra.Command{
			Use:   "profile",
			Short: "CPU/memory profile (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "profile: no samples collected (placeholder)")
				return err
			},
		},
		// report：占位，提示先运行 benchmark/profile。
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
