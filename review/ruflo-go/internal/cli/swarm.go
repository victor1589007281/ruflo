package cli

import (
	"context"

	"github.com/spf13/cobra"
)

func newSwarmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "swarm",
		Short: "Swarm coordination",
	}
	cmd.AddCommand(
		swarmInitCmd(),
		swarmStatusCmd(),
		swarmStopCmd(),
		swarmHealthCmd(),
	)
	return cmd
}

func swarmInitCmd() *cobra.Command {
	var topology, strategy string
	var maxAgents int
	var v3 bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Initialize a swarm",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "swarm_init", map[string]any{
				"topology":   topology,
				"max_agents": maxAgents,
				"strategy":   strategy,
				"v3_mode":    v3,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&topology, "topology", "hierarchical", "Swarm topology")
	c.Flags().IntVar(&maxAgents, "max-agents", 8, "Maximum agents")
	c.Flags().StringVar(&strategy, "strategy", "specialized", "Coordination strategy")
	c.Flags().BoolVar(&v3, "v3-mode", false, "Enable V3 full coordination preset")
	return c
}

func swarmStatusCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "status",
		Short: "Swarm status",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "swarm_status", map[string]any{
				"id": id,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Swarm id (optional)")
	return c
}

func swarmStopCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop swarm(s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "swarm_shutdown", map[string]any{
				"id": id,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Swarm id (empty stops all)")
	return c
}

func swarmHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Swarm health",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "swarm_health", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}
