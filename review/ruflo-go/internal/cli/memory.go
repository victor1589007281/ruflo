package cli

import (
	"context"

	"github.com/spf13/cobra"
)

func newMemoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Agent memory operations",
	}
	cmd.AddCommand(
		memoryStoreCmd(),
		memoryRetrieveCmd(),
		memorySearchCmd(),
		memoryDeleteCmd(),
		memoryListCmd(),
		memoryStatsCmd(),
		memoryInitCmd(),
	)
	return cmd
}

func memoryStoreCmd() *cobra.Command {
	var key, value, namespace string
	c := &cobra.Command{
		Use:   "store",
		Short: "Store a memory entry",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "memory_store", map[string]any{
				"key": key, "value": value, "namespace": namespace,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&key, "key", "", "Key (required)")
	c.Flags().StringVar(&value, "value", "", "Value (required)")
	c.Flags().StringVar(&namespace, "namespace", "", "Namespace")
	_ = c.MarkFlagRequired("key")
	_ = c.MarkFlagRequired("value")
	return c
}

func memoryRetrieveCmd() *cobra.Command {
	var key, namespace string
	c := &cobra.Command{
		Use:   "retrieve",
		Short: "Retrieve by key",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "memory_retrieve", map[string]any{
				"key": key, "namespace": namespace,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&key, "key", "", "Key (required)")
	c.Flags().StringVar(&namespace, "namespace", "", "Namespace")
	_ = c.MarkFlagRequired("key")
	return c
}

func memorySearchCmd() *cobra.Command {
	var query, namespace string
	var limit int
	var threshold float64
	c := &cobra.Command{
		Use:   "search",
		Short: "Search memory",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "memory_search", map[string]any{
				"query": query, "namespace": namespace, "limit": limit, "threshold": threshold,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&query, "query", "", "Search query")
	c.Flags().StringVar(&namespace, "namespace", "", "Namespace")
	c.Flags().IntVar(&limit, "limit", 20, "Max results")
	c.Flags().Float64Var(&threshold, "threshold", 0.01, "Minimum score")
	return c
}

func memoryDeleteCmd() *cobra.Command {
	var key, namespace string
	c := &cobra.Command{
		Use:   "delete",
		Short: "Delete a key",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "memory_delete", map[string]any{
				"key": key, "namespace": namespace,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&key, "key", "", "Key (required)")
	c.Flags().StringVar(&namespace, "namespace", "", "Namespace")
	_ = c.MarkFlagRequired("key")
	return c
}

func memoryListCmd() *cobra.Command {
	var namespace string
	var limit int
	c := &cobra.Command{
		Use:   "list",
		Short: "List keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "memory_list", map[string]any{
				"namespace": namespace, "limit": limit,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&namespace, "namespace", "", "Namespace")
	c.Flags().IntVar(&limit, "limit", 100, "Max keys")
	return c
}

func memoryStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Memory statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "memory_stats", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}

func memoryInitCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Initialize memory store",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "memory_init", map[string]any{
				"force": force,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().BoolVar(&force, "force", false, "Force re-init")
	return c
}
