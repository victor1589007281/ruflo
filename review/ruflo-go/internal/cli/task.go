package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// 本文件实现 task 命令组：编排任务的创建、列表、状态、指派与取消，对应 MCP 工具 task_*。

// newTaskCmd 构建「task」根子命令，挂载 create、list、status、assign、cancel。
func newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Create and manage orchestration tasks",
	}
	cmd.AddCommand(
		taskCreateCmd(),
		taskListCmd(),
		taskStatusCmd(),
		taskAssignCmd(),
		taskCancelCmd(),
	)
	return cmd
}

// taskCreateCmd 创建任务：必填 description；若未给 title 则从 description 截断生成；调用 task_create。
func taskCreateCmd() *cobra.Command {
	var typ, title, description string
	var priority int
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a task (task_create)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if title == "" && description != "" {
				title = description
				if len(title) > 60 {
					title = title[:60] + "..."
				}
			}
			raw, err := CallTool(cmd.Context(), "task_create", map[string]any{
				"type": typ, "title": title, "description": description, "priority": priority,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&typ, "type", "", "Task type (e.g. implementation)")
	c.Flags().StringVar(&title, "title", "", "Task title (optional; defaults from description)")
	c.Flags().StringVar(&description, "description", "", "Description (required)")
	c.Flags().IntVar(&priority, "priority", 0, "Priority (higher = more urgent)")
	_ = c.MarkFlagRequired("description")
	return c
}

// taskListCmd 列出当前任务集合，调用 task_list。
func taskListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tasks (task_list)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "task_list", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}

// taskStatusCmd 按 --id 查询任务状态，调用 task_status。
func taskStatusCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "status",
		Short: "Show task status (task_status)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "task_status", map[string]any{"id": id})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Task id (required)")
	_ = c.MarkFlagRequired("id")
	return c
}

// taskAssignCmd 将任务指派给代理，需 --id 与 --agent-id，调用 task_assign。
func taskAssignCmd() *cobra.Command {
	var id, agentID string
	c := &cobra.Command{
		Use:   "assign",
		Short: "Assign task to an agent (task_assign)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "task_assign", map[string]any{
				"id": id, "agent_id": agentID,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Task id (required)")
	c.Flags().StringVar(&agentID, "agent-id", "", "Agent id (required)")
	_ = c.MarkFlagRequired("id")
	_ = c.MarkFlagRequired("agent-id")
	return c
}

// taskCancelCmd 取消指定 id 的任务，调用 task_cancel。
func taskCancelCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "cancel",
		Short: "Cancel a task (task_cancel)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "task_cancel", map[string]any{"id": id})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Task id (required)")
	_ = c.MarkFlagRequired("id")
	return c
}
