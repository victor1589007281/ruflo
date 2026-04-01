package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// 本文件实现 memory 命令组：代理记忆的存取、检索、删除、列举、统计与初始化，对应 memory_* MCP 工具。

// newMemoryCmd 构建「memory」根子命令，挂载 store、retrieve、search、delete、list、stats、init。
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

// memoryStoreCmd 写入键值，可选 namespace，调用 memory_store。
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

// memoryRetrieveCmd 按键精确读取，调用 memory_retrieve。
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

// memorySearchCmd 语义/向量检索，支持 limit 与相似度 threshold，调用 memory_search。
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

// memoryDeleteCmd 删除指定 key，调用 memory_delete。
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

// memoryListCmd 列举命名空间下键名，受 limit 约束，调用 memory_list。
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

// memoryStatsCmd 输出记忆存储统计信息，调用 memory_stats。
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

// memoryInitCmd 初始化或重置记忆后端，--force 时强制重建，调用 memory_init。
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
