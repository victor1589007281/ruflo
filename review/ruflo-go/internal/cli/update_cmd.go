package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 update 命令组：CLI 自更新检查与应用（Go 构建占位，打印当前 root 包内 version 常量）。

// newUpdateCmd 构建「update」根子命令，内联 check（显示 version）与 apply（未实现）。
func newUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "CLI self-update (placeholder)",
	}
	cmd.AddCommand(
		// check：占位，打印内置版本号。
		&cobra.Command{
			Use:   "check",
			Short: "Check for updates",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "update check: current", version, "(placeholder)")
				return err
			},
		},
		// apply：占位，Go 发行版未实现热更新。
		&cobra.Command{
			Use:   "apply",
			Short: "Apply update",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "update apply: not implemented in Go build")
				return err
			},
		},
	)
	return cmd
}
