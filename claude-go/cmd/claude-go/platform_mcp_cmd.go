package main

// platform_mcp_cmd.go — design/02 §3.5 L5「platform-mcp-server」的 stdio 入口。
//
// 与 codeintel-mcp-server 的分工: 那个暴露代码图谱, 这个暴露平台运行态
// (teams / workflows / skills / tools / TaskService)。协议在 pkg/platformmcp,
// Backend 在 pkg/dashboard —— 与 :18080 上 /api/mcp/rpc 用的是同一份 Backend,
// 两个传输看到的能力完全一致 (不会出现"HTTP 有这个工具、stdio 没有")。
//
// 单独放一个文件而不是塞进 main.go: main.go 是多方同时改的争用文件。

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/anthropic/claude-go/pkg/dashboard"
	"github.com/anthropic/claude-go/pkg/platformmcp"
)

func platformMCPServerCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "platform-mcp-server",
		Short: "Platform MCP Server (teams/workflows/skills/tools/tasks 以 MCP 工具对外)",
		Long: `启动平台 MCP Server (stdio), 把 claude-go 自身的运行态以 MCP 工具对外,
作为远程 agent 管理平台的标准入口 (design/02 §3.5 L5)。

工具:
  platform_teams_list / platform_team_get        团队清单与详情
  platform_workflows_list                       可用编排工作流
  platform_skills_list / platform_tools_list     技能与内置工具
  platform_tasks_list / platform_task_status     任务档案
  platform_task_submit                          提交团队运行任务

注意: 本进程是独立只读进程, 没有 TaskService, 因此 platform_task_submit 会如实
报错而不是静默排队。要真正提交任务, 用 :18080 上的 HTTP 形态
(POST /api/mcp/rpc, 与本命令同一套工具), 那里由飞书主进程注入了 TaskService。

Claude Desktop / Cursor 配置:
  {"mcpServers":{"claude-go-platform":{"command":"claude-go","args":["platform-mcp-server"]}}}`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info, err := resolveDashStateDir(stateDir, flagConfig)
			if err != nil {
				return err
			}
			// stdout 只许有 JSON-RPC; 提示信息一律走 stderr。
			fmt.Fprintf(os.Stderr, "[platform-mcp] stdio 就绪 stateDir=%s\n", info.Resolved)
			srv := dashboard.NewServer(dashboard.Config{StateDir: info.Resolved})
			return platformmcp.New(srv.PlatformMCPBackend()).ServeStdio(os.Stdin, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "数据目录 (默认与 dashboard/bot 一致)")
	return cmd
}
