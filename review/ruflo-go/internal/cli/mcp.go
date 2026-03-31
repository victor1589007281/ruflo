package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/mcp/transport"
	"github.com/spf13/cobra"
)

// MCPServeFunc is the stdio transport entry (injectable for tests).
var MCPServeFunc = transport.ServeStdioOS

// RunMCPServer serves MCP over stdio using ToolRegistry with optional verbose logging.
func RunMCPServer(ctx context.Context) error {
	if ToolRegistry == nil {
		return fmt.Errorf("cli: ToolRegistry not initialized")
	}
	srv := mcp.NewMCPServer(ToolRegistry)
	srv.Version = "3.5.0"
	if Verbose {
		_, _ = fmt.Fprintln(os.Stderr, "ruflo: MCP stdio server (protocol tools/list, tools/call)")
	}
	if err := MCPServeFunc(ctx, srv); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol server",
	}
	cmd.AddCommand(mcpStartCmd())
	return cmd
}

func mcpStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start MCP JSON-RPC over stdio",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunMCPServer(cmd.Context())
		},
	}
}
