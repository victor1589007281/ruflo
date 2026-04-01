package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// 本文件实现 status 命令：一键拉取系统总览（版本、子系统健康等），调用 status_overview MCP 工具。

// newStatusCmd 构建「status」单命令，无子命令。
func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "System status overview",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "status_overview", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}
