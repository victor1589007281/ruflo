package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 daemon 命令组：后台守护进程的启动、停止与状态。当前为占位实现，仅向 stdout 打印提示，后续可对接真实进程管理。

// newDaemonCmd 构建「daemon」根子命令，内联定义 start/stop/status 三个占位子命令。
func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Background daemon (placeholder)",
	}
	cmd.AddCommand(
		// start：占位，打印「已启动」提示。
		&cobra.Command{
			Use:   "start",
			Short: "Start daemon",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "Daemon started")
				return err
			},
		},
		// stop：占位，打印「已停止」提示。
		&cobra.Command{
			Use:   "stop",
			Short: "Stop daemon",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "Daemon stopped")
				return err
			},
		},
		// status：占位，报告未运行状态。
		&cobra.Command{
			Use:   "status",
			Short: "Daemon status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "Daemon status: not running (placeholder)")
				return err
			},
		},
	)
	return cmd
}
