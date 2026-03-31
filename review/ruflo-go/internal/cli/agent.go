package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage orchestration agents",
	}
	cmd.AddCommand(
		agentSpawnCmd(),
		agentListCmd(),
		agentStatusCmd(),
		agentStopCmd(),
		agentHealthCmd(),
	)
	return cmd
}

func agentSpawnCmd() *cobra.Command {
	var typ, name, namespace string
	c := &cobra.Command{
		Use:   "spawn",
		Short: "Spawn/register an agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "agent_spawn", map[string]any{
				"type": typ, "name": name, "namespace": namespace,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&typ, "type", "", "Agent type (required)")
	c.Flags().StringVar(&name, "name", "", "Agent name (required)")
	c.Flags().StringVar(&namespace, "namespace", "", "Namespace")
	_ = c.MarkFlagRequired("type")
	_ = c.MarkFlagRequired("name")
	return c
}

func agentListCmd() *cobra.Command {
	var filter string
	c := &cobra.Command{
		Use:   "list",
		Short: "List agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "agent_list", map[string]any{
				"filter": filter,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&filter, "filter", "", "Filter by state or status")
	return c
}

func agentStatusCmd() *cobra.Command {
	var id, name string
	c := &cobra.Command{
		Use:   "status",
		Short: "Show agent status",
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" && len(args) > 0 {
				id = args[0]
			}
			raw, err := CallTool(context.Background(), "agent_status", map[string]any{
				"id": id, "name": name,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Agent id")
	c.Flags().StringVar(&name, "name", "", "Agent name")
	return c
}

func agentStopCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop an agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" && len(args) > 0 {
				id = args[0]
			}
			if id == "" {
				return fmt.Errorf("agent id required: use --id or pass id as first argument")
			}
			raw, err := CallTool(context.Background(), "agent_terminate", map[string]any{
				"id": id,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Agent id (or pass as first argument)")
	return c
}

func agentHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Agent health summary",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "agent_health", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}
