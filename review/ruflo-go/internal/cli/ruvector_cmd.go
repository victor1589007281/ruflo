package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 ruvector 命令组：RuVector 情报子系统的状态、初始化提示与模式覆盖查询，部分委托 neural_* 工具。

// newRuvectorCmd 构建「ruvector」根子命令，挂载 status、init、coverage。
func newRuvectorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ruvector",
		Short: "RuVector intelligence subsystem",
	}
	cmd.AddCommand(
		// status：复用 neural_status 作为 RuVector 神经状态总览。
		&cobra.Command{
			Use:   "status",
			Short: "Neural / RuVector status",
			RunE: func(cmd *cobra.Command, args []string) error {
				return RunTool(cmd.Context(), "neural_status", map[string]any{})
			},
		},
		// init：占位，提示通过 neural_train 与 memory init 完成初始化。
		&cobra.Command{
			Use:   "init",
			Short: "Initialize RuVector state (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "ruvector init: use neural_train / memory init via MCP")
				return err
			},
		},
		// coverage：调用 neural_patterns 作为模式覆盖列表。
		&cobra.Command{
			Use:   "coverage",
			Short: "Pattern coverage report (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				return RunTool(cmd.Context(), "neural_patterns", map[string]any{})
			},
		},
	)
	return cmd
}
