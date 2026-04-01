package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 workflow 命令组：工作流执行与状态查询的占位，便于与 TS CLI 子命令名对齐。

// newWorkflowCmd 构建「workflow」根子命令，内联 run/list/status 占位输出。
func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Workflow execution (placeholder)",
	}
	cmd.AddCommand(
		// run：占位，按名称运行工作流。
		&cobra.Command{
			Use:   "run",
			Short: "Run a workflow by name",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "workflow run: not implemented (placeholder)")
				return err
			},
		},
		// list：占位，列出已注册工作流。
		&cobra.Command{
			Use:   "list",
			Short: "List workflows",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "(no workflows registered — placeholder)")
				return err
			},
		},
		// status：占位，工作流空闲状态。
		&cobra.Command{
			Use:   "status",
			Short: "Workflow status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "workflow status: idle (placeholder)")
				return err
			},
		},
	)
	return cmd
}
