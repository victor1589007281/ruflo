package cli

import (
	"context"

	"github.com/spf13/cobra"
)

func newHooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Hook invocations and inspection",
	}
	cmd.AddCommand(hooksDispatchCmd(), hooksListCmd())
	return cmd
}

func hooksDispatchCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "dispatch",
		Short: "Record a hook dispatch",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "hooks_pre-task", map[string]any{
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
			raw, err := CallTool(context.Background(), "hooks_metrics", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}
