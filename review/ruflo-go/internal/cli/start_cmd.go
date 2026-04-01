package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 start 命令：一键触发蜂群默认初始化（hierarchical/8/specialized）并提示守护进程需单独 daemon start（当前守护为桩）。

// newStartCmd 构建「start」单命令，先 RunTool swarm_init 再打印 daemon 桩提示。
func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start Ruflo orchestration (swarm init + daemon)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := RunTool(ctx, "swarm_init", map[string]any{
				"topology":   "hierarchical",
				"max_agents": 8,
				"strategy":   "specialized",
			}); err != nil {
				return err
			}
			_, err := fmt.Fprintln(Stdout(), "daemon: start requested (stub — use `ruflo daemon start`)")
			return err
		},
	}
}
