package cli

import (
	"context"

	"github.com/spf13/cobra"
)

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

func taskCreateCmd() *cobra.Command {
	var typ, title, description string
	var priority int
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a task (task_create)",
		RunE: func(cmd *cobra.Command, args []string) error {
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
	c.Flags().StringVar(&title, "title", "", "Task title (required)")
	c.Flags().StringVar(&description, "description", "", "Description")
	c.Flags().IntVar(&priority, "priority", 0, "Priority (higher = more urgent)")
	_ = c.MarkFlagRequired("title")
	return c
}

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
