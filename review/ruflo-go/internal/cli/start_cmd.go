package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

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
