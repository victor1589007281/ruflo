package cli

import (
	"github.com/spf13/cobra"
)

func newRouteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Task routing (ReasoningBank)",
	}
	cmd.AddCommand(routeTaskCmd())
	return cmd
}

// routeTaskCmd 提交任务标签、描述与复杂度 hint，调用 hooks_route 完成路由决策输出。
func routeTaskCmd() *cobra.Command {
	var task, description string
	var complexity float64
	c := &cobra.Command{
		Use:   "task",
		Short: "Route a task description via ReasoningBank",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "hooks_route", map[string]any{
				"task":        task,
				"description": description,
				"complexity":  complexity,
			})
		},
	}
	c.Flags().StringVar(&task, "task", "", "Task label")
	c.Flags().StringVar(&description, "description", "", "Task description")
	c.Flags().Float64Var(&complexity, "complexity", 0, "Complexity hint 0-1")
	return c
}
