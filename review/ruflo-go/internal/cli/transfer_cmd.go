package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

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
