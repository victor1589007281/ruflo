package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 progress 命令组：工作流/会话进度查询、同步、摘要与监听的占位子命令。

// newProgressCmd 构建「progress」根子命令，内联 check、sync、summary、watch。
func newProgressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "progress",
		Short: "Workflow / session progress (placeholder)",
	}
	cmd.AddCommand(
		// check：占位，检查进度状态。
		&cobra.Command{
			Use:   "check",
			Short: "Check progress state",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "progress check: placeholder")
				return err
			},
		},
		// sync：占位，同步进度到远端。
		&cobra.Command{
			Use:   "sync",
			Short: "Sync progress to remote (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "progress sync: placeholder")
				return err
			},
		},
		// summary：占位，进度摘要。
		&cobra.Command{
			Use:   "summary",
			Short: "Progress summary",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "progress summary: placeholder")
				return err
			},
		},
		// watch：占位，持续观察进度流。
		&cobra.Command{
			Use:   "watch",
			Short: "Watch progress stream (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "progress watch: placeholder")
				return err
			},
		},
	)
	return cmd
}
