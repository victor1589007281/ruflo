package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 process 命令组：托管后台进程的列表、启动、停止与状态占位子命令。

// newProcessCmd 构建「process」根子命令，内联 list、start、stop、status。
func newProcessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "process",
		Short: "Background process management (placeholder)",
	}
	cmd.AddCommand(
		// list：占位，托管进程列表为空。
		&cobra.Command{
			Use:   "list",
			Short: "List managed processes",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "process list: (empty — placeholder)")
				return err
			},
		},
		// start：占位，启动命名进程。
		&cobra.Command{
			Use:   "start",
			Short: "Start a named process",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "process start: placeholder")
				return err
			},
		},
		// stop：占位，停止进程。
		&cobra.Command{
			Use:   "stop",
			Short: "Stop a process",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "process stop: placeholder")
				return err
			},
		},
		// status：占位，进程状态。
		&cobra.Command{
			Use:   "status",
			Short: "Process status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "process status: placeholder")
				return err
			},
		},
	)
	return cmd
}
