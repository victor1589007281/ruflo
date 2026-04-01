package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 migrate 命令组：V2→V3 数据/配置迁移的占位子命令（run/status/rollback）。

// newMigrateCmd 构建「migrate」根子命令，内联 run、status、rollback。
func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "V2 to V3 migration (placeholder)",
	}
	cmd.AddCommand(
		// run：占位，无待执行迁移。
		&cobra.Command{
			Use:   "run",
			Short: "Run pending migrations",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "migrate run: no pending migrations (placeholder)")
				return err
			},
		},
		// status：占位，已是最新。
		&cobra.Command{
			Use:   "status",
			Short: "Migration status",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "migrate status: up to date (placeholder)")
				return err
			},
		},
		// rollback：占位，Go 桩不支持回滚。
		&cobra.Command{
			Use:   "rollback",
			Short: "Rollback last migration batch",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "migrate rollback: not supported in Go stub")
				return err
			},
		},
	)
	return cmd
}
