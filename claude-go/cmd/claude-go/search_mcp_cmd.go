package main

// search_mcp_cmd.go — search-mcp-server 子命令。
//
// 把 zero-cost 检索与结构化行情以 MCP 工具对外, 供 Claude Desktop / 其它 agent
// 通过 stdio 或 HTTP 挂载。协议与工具实现分别在 pkg/searchmcp 与 pkg/tool/builtin,
// 与 agent 内部使用的是同一份工具定义。

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/anthropic/claude-go/pkg/searchmcp"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// searchMCPServerCmd 构造 search-mcp-server 子命令。
//
// stdout 只留给 JSON-RPC; 提示一律走 stderr, 否则标准输出多一行日志就会
// 破坏 stdio 客户端的协议解析。
func searchMCPServerCmd() *cobra.Command {
	var transport, addr string
	cmd := &cobra.Command{
		Use:   "search-mcp-server",
		Short: "Search & market data MCP server (zero-cost web search, fetch, quotes)",
		Long: `以 MCP 工具对外暴露联网检索与结构化行情:
  WebSearch / WebFetch        多源检索与正文抓取 (免 key)
  market_snapshot             时点快照 (报价/K线/财务/公告/新闻)
  market_quote / market_history / market_fundamentals
  market_filings / market_news / market_macro

配置 (推荐):
  --config <path>     config.json; 从 search.githubToken 读取 GitHub PAT
                      (优先级高于 GITHUB_TOKEN 环境变量, 后者仅为兼容旧部署保留)

环境变量 (全部可选):
  SEARCH_PROXY_URL    出口代理, 例如 http://host:8888
  SEARCH_NO_PROXY     逗号分隔的直连名单
  SEARCH_MAX_TIER     L0..L3, 默认 L2 (L3 付费默认不可达)
  CLAUDE_GO_DATA_DISABLED=1  关闭数据面 (本命令将报错退出)

Claude Desktop / Cursor 配置:
  {"mcpServers":{"claude-go-search":{"command":"claude-go","args":["search-mcp-server"]}}}`,
		RunE: func(_ *cobra.Command, _ []string) error {
			data := builtin.DefaultDataStack()
			if data == nil {
				return fmt.Errorf("搜索数据面未装配: 检查 SEARCH_PROXY_URL 是否合法, 或 CLAUDE_GO_DATA_DISABLED 是否被设为 1")
			}
			srv := searchmcp.New(data)
			if srv == nil {
				return fmt.Errorf("搜索数据面不完整 (缺少 search 或 market 注册表)")
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			switch transport {
			case "stdio", "":
				fmt.Fprintf(os.Stderr, "[search-mcp] stdio 就绪 (proxy=%v)\n", data.EgressHealthy())
				return srv.ServeStdio(ctx, os.Stdin, os.Stdout)
			case "http":
				fmt.Fprintf(os.Stderr, "[search-mcp] HTTP 监听 %s (proxy=%v)\n", addr, data.EgressHealthy())
				return srv.ServeHTTP(ctx, addr)
			default:
				return fmt.Errorf("未知 transport %q: 用 stdio 或 http", transport)
			}
		},
	}
	cmd.Flags().StringVar(&transport, "transport", "stdio", "传输方式: stdio | http")
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:9235", "HTTP 监听地址 (仅 --transport http)")
	return cmd
}
