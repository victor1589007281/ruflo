package cli

import (
	"github.com/spf13/cobra"
)

func newGuidanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guidance",
		Short: "Governance / guidance control plane",
	}
	cmd.AddCommand(
		guidanceCompileCmd(),
		guidanceRetrieveCmd(),
		guidanceEnforceCmd(),
		guidanceDiscoverCmd(),
		guidanceWorkflowCmd(),
		guidanceQuickrefCmd(),
	)
	return cmd
}

func guidanceCompileCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use:   "compile",
		Short: "Compile CLAUDE.md into guidance rules",
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				path = "CLAUDE.md"
			}
			return RunTool(cmd.Context(), "guidance_compile", map[string]any{"path": path})
		},
	}
	c.Flags().StringVar(&path, "path", "", "Path to CLAUDE.md (default: ./CLAUDE.md)")
	return c
}

func guidanceRetrieveCmd() *cobra.Command {
	var intent string
	c := &cobra.Command{
		Use:   "retrieve",
		Short: "Retrieve shards for intent",
		RunE: func(cmd *cobra.Command, args []string) error {
			if intent == "" && len(args) > 0 {
				intent = args[0]
			}
			if intent == "" {
				intent = "general"
			}
			return RunTool(cmd.Context(), "guidance_recommend", map[string]any{"intent": intent})
		},
	}
	c.Flags().StringVar(&intent, "intent", "", "Intent to match (e.g. security, performance)")
	return c
}

func guidanceEnforceCmd() *cobra.Command {
	var command, tool string
	c := &cobra.Command{
		Use:   "enforce",
		Short: "Evaluate command/tool against enforcement gates",
		RunE: func(cmd *cobra.Command, args []string) error {
			if command == "" && len(args) > 0 {
				command = args[0]
			}
			params := map[string]any{}
			if command != "" {
				params["command"] = command
			}
			if tool != "" {
				params["tool"] = tool
			}
			return RunTool(cmd.Context(), "guidance_evaluate", map[string]any(params))
		},
	}
	c.Flags().StringVar(&command, "command", "", "Command to evaluate")
	c.Flags().StringVar(&tool, "tool", "", "Tool name to evaluate")
	return c
}

func guidanceDiscoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover",
		Short: "Guidance capabilities manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "guidance_capabilities", map[string]any{})
		},
	}
}

func guidanceWorkflowCmd() *cobra.Command {
	var phase string
	c := &cobra.Command{
		Use:   "workflow",
		Short: "Guidance workflow hook",
		RunE: func(cmd *cobra.Command, args []string) error {
			if phase == "" {
				phase = "pre-task"
			}
			return RunTool(cmd.Context(), "guidance_run_ledger", map[string]any{"phase": phase})
		},
	}
	c.Flags().StringVar(&phase, "phase", "", "Workflow phase (pre-task, post-task)")
	return c
}

func guidanceQuickrefCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "quickref",
		Short: "Quick reference of active guidance rules",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "guidance_capabilities", map[string]any{})
		},
	}
}
