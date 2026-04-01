package cli

import (
	"github.com/spf13/cobra"
)

// 本文件实现 benchmark 命令组：对接 performance_* MCP 工具的命名基准运行、报告与对比（对比当前转发 metrics）。

// newBenchmarkCmd 构建「benchmark」根子命令，挂载 run、report、compare。
func newBenchmarkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Performance benchmarks",
	}
	cmd.AddCommand(
		// run：调用 performance_benchmark，默认可选名 default 或首参为套件名。
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
		// report：调用 performance_report 写出报告结构。
		&cobra.Command{
			Use:   "report",
			Short: "Write benchmark report",
			RunE: func(cmd *cobra.Command, args []string) error {
				return RunTool(cmd.Context(), "performance_report", map[string]any{})
			},
		},
		// compare：占位语义，当前调用 performance_metrics 提供可比数据。
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
