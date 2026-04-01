package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 transfer 命令组：Transfer 商店与插件目录的搜索、详情与下载计数等，映射 transfer_* MCP 工具（含连字符工具名）。

// newTransferCmd 构建「transfer」根子命令，挂载 store-search、store-info、store-download、plugin-search、plugin-info。
func newTransferCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "transfer",
		Short: "Transfer store and plugin catalog",
	}
	cmd.AddCommand(
		transferStoreSearchCmd(),
		transferStoreInfoCmd(),
		transferStoreDownloadCmd(),
		transferPluginSearchCmd(),
		transferPluginInfoCmd(),
	)
	return cmd
}

// transferStoreSearchCmd 按 query 搜索商店目录，调用 transfer_store-search。
func transferStoreSearchCmd() *cobra.Command {
	var query string
	c := &cobra.Command{
		Use:   "store-search",
		Short: "Search transfer store catalog",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "transfer_store-search", map[string]any{"query": query})
		},
	}
	c.Flags().StringVar(&query, "query", "", "Search query")
	return c
}

// transferStoreInfoCmd 按 id（--id 或首参）获取商店条目详情，调用 transfer_store-info。
func transferStoreInfoCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "store-info",
		Short: "Get store item by id",
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" && len(args) > 0 {
				id = args[0]
			}
			if id == "" {
				return fmt.Errorf("--id or positional id required")
			}
			return RunTool(cmd.Context(), "transfer_store-info", map[string]any{"id": id})
		},
	}
	c.Flags().StringVar(&id, "id", "", "Item id")
	return c
}

// transferStoreDownloadCmd 记录某商店条目的下载行为，调用 transfer_store-download。
func transferStoreDownloadCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "store-download",
		Short: "Record download for store item",
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" && len(args) > 0 {
				id = args[0]
			}
			if id == "" {
				return fmt.Errorf("--id or positional id required")
			}
			return RunTool(cmd.Context(), "transfer_store-download", map[string]any{"id": id})
		},
	}
	c.Flags().StringVar(&id, "id", "", "Item id")
	return c
}

// transferPluginSearchCmd 搜索插件目录，调用 transfer_plugin-search。
func transferPluginSearchCmd() *cobra.Command {
	var query string
	c := &cobra.Command{
		Use:   "plugin-search",
		Short: "Search plugin catalog",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "transfer_plugin-search", map[string]any{"query": query})
		},
	}
	c.Flags().StringVar(&query, "query", "", "Search query")
	return c
}

// transferPluginInfoCmd 按 id 获取插件元数据，调用 transfer_plugin-info。
func transferPluginInfoCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "plugin-info",
		Short: "Get plugin by id",
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" && len(args) > 0 {
				id = args[0]
			}
			if id == "" {
				return fmt.Errorf("--id or positional id required")
			}
			return RunTool(cmd.Context(), "transfer_plugin-info", map[string]any{"id": id})
		},
	}
	c.Flags().StringVar(&id, "id", "", "Plugin id")
	return c
}
