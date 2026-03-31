package cli

import (
	"github.com/spf13/cobra"
)

func newHooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Hook invocations and inspection",
	}
	cmd.AddCommand(
		hooksPreTaskCmd(),
		hooksPostTaskCmd(),
		hooksRouteHooksCmd(),
		hooksWorkerParentCmd(),
		hooksSessionStartHooksCmd(),
		hooksSessionEndHooksCmd(),
		hooksDispatchCmd(),
		hooksListCmd(),
	)
	return cmd
}

func hooksPreTaskCmd() *cobra.Command {
	var description string
	c := &cobra.Command{
		Use:   "pre-task",
		Short: "Invoke pre-task hook (hooks_pre-task)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "hooks_pre-task", map[string]any{
				"description": description,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&description, "description", "", "Task description")
	_ = c.MarkFlagRequired("description")
	return c
}

func hooksPostTaskCmd() *cobra.Command {
	var taskID string
	var success bool
	c := &cobra.Command{
		Use:   "post-task",
		Short: "Invoke post-task hook (hooks_post-task)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "hooks_post-task", map[string]any{
				"task_id": taskID,
				"success": success,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&taskID, "task-id", "", "Task id")
	c.Flags().BoolVar(&success, "success", false, "Whether the task succeeded")
	_ = c.MarkFlagRequired("task-id")
	return c
}

func hooksRouteHooksCmd() *cobra.Command {
	var task string
	c := &cobra.Command{
		Use:   "route",
		Short: "Route task via hooks (hooks_route)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "hooks_route", map[string]any{
				"task": task,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&task, "task", "", "Task text to route")
	_ = c.MarkFlagRequired("task")
	return c
}

func hooksWorkerParentCmd() *cobra.Command {
	w := &cobra.Command{
		Use:   "worker",
		Short: "Background worker hooks",
	}
	w.AddCommand(hooksWorkerListCmd(), hooksWorkerDispatchCmd())
	return w
}

func hooksWorkerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List workers (hooks_worker-list)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "hooks_worker-list", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}

func hooksWorkerDispatchCmd() *cobra.Command {
	var trigger string
	c := &cobra.Command{
		Use:   "dispatch",
		Short: "Dispatch workers by trigger (hooks_worker-dispatch)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "hooks_worker-dispatch", map[string]any{
				"trigger": trigger,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&trigger, "trigger", "", "Worker trigger name")
	_ = c.MarkFlagRequired("trigger")
	return c
}

func hooksSessionStartHooksCmd() *cobra.Command {
	var sessionID string
	c := &cobra.Command{
		Use:   "session-start",
		Short: "Session start hook (hooks_session-start)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "hooks_session-start", map[string]any{
				"session_id": sessionID,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&sessionID, "session-id", "", "Session id")
	_ = c.MarkFlagRequired("session-id")
	return c
}

func hooksSessionEndHooksCmd() *cobra.Command {
	var sessionID string
	var exportMetrics bool
	c := &cobra.Command{
		Use:   "session-end",
		Short: "Session end hook (hooks_session-end)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "hooks_session-end", map[string]any{
				"session_id":     sessionID,
				"export_metrics": exportMetrics,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&sessionID, "session-id", "", "Session id")
	c.Flags().BoolVar(&exportMetrics, "export-metrics", false, "Export metrics on session end")
	_ = c.MarkFlagRequired("session-id")
	return c
}

func hooksDispatchCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "dispatch",
		Short: "Record a hook dispatch",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "hooks_pre-task", map[string]any{
				"description": name,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Hook name (required)")
	_ = c.MarkFlagRequired("name")
	return c
}

func hooksListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List recent hook invocations",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "hooks_metrics", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}
