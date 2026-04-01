package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/mcp/transport"
	"github.com/spf13/cobra"
)

// 本文件实现 MCP 相关入口：main 在无子命令管道模式下直接调用 RunMCPServer；CLI 提供 mcp start 显式启动 stdio 服务。
// 使用全局 ToolRegistry 构造 MCPServer，经可注入的 MCPServeFunc（默认 ServeStdioOS）对外提供 JSON-RPC。

// MCPServeFunc 为 stdio 传输入口，测试时可替换为 mock。
var MCPServeFunc = transport.ServeStdioOS

// RunMCPServer 基于全局 ToolRegistry 创建 MCP 服务实例，在 Verbose 时向 stderr 打日志，并阻塞服务直至 ctx 取消或传输错误。
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

// newMCPCmd 构建「mcp」根子命令，当前仅含 start。
func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol server",
	}
	cmd.AddCommand(mcpStartCmd())
	return cmd
}

// mcpStartCmd 显式启动 MCP stdio 服务器（与 main 自动模式行为一致），RunE 委托 RunMCPServer。
func mcpStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start MCP JSON-RPC over stdio",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunMCPServer(cmd.Context())
		},
	}
}
