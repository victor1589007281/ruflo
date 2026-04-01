package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// 本文件实现 cleanup 命令：删除当前项目下 .claude-flow 目录（可选 dry-run 仅打印将删除路径）。

// newCleanupCmd 构建「cleanup」命令，支持 --dry-run。
func newCleanupCmd() *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove .claude-flow artifacts in the current project",
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			target := filepath.Join(wd, ".claude-flow")
			if dryRun {
				_, err := fmt.Fprintf(Stdout(), "dry-run: would remove %s\n", target)
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			_, err = fmt.Fprintf(Stdout(), "removed %s\n", target)
			return err
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "Print path only, do not delete")
	return c
}
