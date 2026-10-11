// main.go CLI 入口点。
// 对应 TS 源码: review/claude/src/cli/print.ts + review/claude/src/entrypoints/
//
// Claude Code 有多种运行模式:
//   - 交互式 REPL: 默认模式, 用户输入 → 模型回复 → 工具执行 → 循环
//   - Print/Pipe: claude -p "prompt" 一次性执行
//   - SDK: 程序化调用 (QueryEngine.ask)
//   - MCP Server: 作为 MCP 服务器为其他客户端提供工具
//
// Go 版本支持:
//   - chat: 交互式 REPL
//   - run/print: 一次性执行
//   - feishu: 飞书长连接后台守护 (WebSocket)
//   - mcp: 列出/管理 MCP 服务器
//   - doctor: 系统诊断
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/agentbind"
	"github.com/anthropic/claude-go/pkg/agentdbsemantic"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/assembly"
	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/backup"
	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/cluster"
	"github.com/anthropic/claude-go/pkg/codeintel"
	"github.com/anthropic/claude-go/pkg/commands"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/complexity"
	"github.com/anthropic/claude-go/pkg/dashboard"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/console"
	"github.com/anthropic/claude-go/pkg/evolution/learners"
	"github.com/anthropic/claude-go/pkg/evolution/skillaudit"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/hotreload"
	"github.com/anthropic/claude-go/pkg/llmgw"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/media"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/sandbox"
	"github.com/anthropic/claude-go/pkg/session"
	"github.com/anthropic/claude-go/pkg/sessionapi"
	"github.com/anthropic/claude-go/pkg/settings"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/statestore"
	storedual "github.com/anthropic/claude-go/pkg/statestore/dual"
	swarmintel "github.com/anthropic/claude-go/pkg/swarm_intel"
	"github.com/anthropic/claude-go/pkg/synctask"
	"github.com/anthropic/claude-go/pkg/textutil"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
	"github.com/anthropic/claude-go/pkg/weakmodel"
	"github.com/anthropic/claude-go/pkg/worker"
	"github.com/spf13/cobra"
)

// fullHelpGuide 由 help 子命令打印，与 README/文档中的完整用法保持一致。
const fullHelpGuide = `Claude Code (Go) - AI 编程助手

基础用法:
  claude-go chat                    交互式对话模式
  claude-go run "你的问题"          一次性执行模式
  claude-go feishu --config=xxx     飞书长连接模式
  claude-go doctor                  系统诊断
  claude-go tools                   列出可用工具
  claude-go skills                  查看技能列表/详情
  claude-go roles coder             查看角色最终技能绑定
  claude-go help                    查看完整帮助

飞书模式详细配置:
  # 使用配置文件 (推荐)
  claude-go feishu --config=claude-go.json

  # 使用命令行参数
  claude-go feishu --app-id=cli_xxx --app-secret=xxx --api-key=sk-xxx

  # 环境变量
  FEISHU_APP_ID=cli_xxx FEISHU_APP_SECRET=xxx ANTHROPIC_API_KEY=sk-xxx claude-go feishu

配置文件示例 (claude-go.json):
  {
    "feishu": { "appId": "cli_xxx", "appSecret": "xxx" },
    "ai": { "model": "qwen3.5-plus", "apiKey": "sk-xxx" },
    "mcpServers": { "ruflo": { "command": "npx", "args": ["ruflo@latest", "mcp", "start"] } },
    "dreaming": { "enabled": true, "minHours": 12 }
  }

通用参数:
  --model           AI 模型名 (默认 qwen3.5-plus)
  --api-key         API Key (或 ANTHROPIC_API_KEY 环境变量)
  --base-url        API 地址 (默认 DashScope)
  --max-tokens      最大输出 token (默认 16384)
  --max-turns       最大循环次数 (0=无限)
  --permission-mode 权限模式 bypass/default/plan/auto
  --system-prompt   自定义系统提示词
  --debug           调试模式
  --config          JSON 配置文件路径
  --mcp-config      MCP 配置 JSON 路径

交互式命令:
  /help             查看帮助
  /status           运行状态
  /clear            清除对话
  /model            查看/切换模型
  /cost             查看 token 用量

  /team create <名称> <工作流>   创建多 Agent 团队
  /team run <名称> <目标>        启动团队执行
  /team status [名称]            查看团队状态
  /team stop/delete <名称>       停止/删除团队
  /team workflows                查看可用工作流

  /go <工作流> <目标>            一键创建并启动团队
    例: /go research 调研 k8s 最佳实践
    例: /go development 开发用户登录模块

  /wiki status                   查看知识库状态
  /wiki query <问题>             查询知识库
  /wiki organize [inc]           全量/增量整理
  /wiki lint                     检查链接健康
  /wiki health                   LLM 健康检查

群体智能预测 (Swarm Intelligence):
  /predict <问题>                群体智能预测 (多Agent辩论+贝叶斯融合)
    例: /predict AI Agent 2027年市场规模?
  /simulate <场景> [--mode X]    场景模拟 (social/game/montecarlo)
    例: /simulate 如果量子计算突破会怎样?
`

var (
	flagModel          string
	flagExecutionModel string
	flagPlanModel      string // --plan-model <provider:model> 规划期独立模型 (类似 advisor 独立客户端)
	flagAttach         []string
	flagCwd            string
	flagFinalOnly      bool
	flagEmitSession    bool
	flagRunLabel       string   // --run-label <名字> 本次 run 在轨迹页上的显示名 (空=回落成 session id)
	flagAgent          string   // --agent <名字> 以单个 agent 的身份运行本次任务 (角色名或 agent 资产名)
	flagPlugins        []string // --plugin <名字> 启用插件包 (可重复); 展开其 agents/skills/hooks 并装进本次运行
	flagAPIKey         string
	flagBaseURL        string
	flagMaxTokens      int
	flagMaxTurns       int
	flagPermission     string
	flagSystemPrompt   string
	flagPrint          bool
	flagVerify         string
	flagBestOf         int
	flagDebug          bool
	flagMCPConfig      string
	flagConfig         string
	flagResume         string // --resume <sessionID>
	flagContinue       bool   // --continue / -c
	flagAdvisor        string // --advisor <provider:model|off>
	flagAutoPlan       string // --auto-plan on|off (空=读配置, 默认开)
	flagComplexityMode string // --complexity-mode heuristic|llm|hybrid
	flagAllowedTools   string // --allowed-tools t1,t2 (whitelist; non-empty = only these)
	flagOutputFormat   string // --output-format text|json
	// agentDB 语义增强面 (规划 14.1.4.3): --agentdb-url <serve 地址> 装配
	// AgentDBSemanticInjectHook, 使真实引擎经 serve /v1/memory|retrieve|files
	// 跨源检索并注入 system prompt。nil = 不装配 (默认)。
	flagAgentDBURL     string // --agentdb-url http://127.0.0.1:PORT
	flagAgentDBTopK    int    // --agentdb-topk 每源检索条数 (默认 5)
	flagAgentDBBudget  int    // --agentdb-budget 注入上下文 token 预算 (默认 600)
	flagAgentDBReserve int    // --agentdb-reserve 为任务正文预留 token (默认 200)
)

func main() {
	// 会话分片 A/B (见 pkg/api/sessionshard.go): 决定出站 X-Claude-Code-Session-Id
	// 是否把 NodeID 也算进会话键。默认 0 = 关闭, 出站头与改造前逐字节一致。
	// 放在 cobra 之前: 它必须在第一个出站请求之前生效。
	if pct := api.InitSessionShardFromEnv(); pct > 0 {
		log.Printf("[claude-go] 会话分片实验: %d%% 的 run 走 run+node 会话键 (CLAUDE_GO_SESSION_SHARD)", pct)
	}

	rootCmd := &cobra.Command{
		Use:   "claude-go",
		Short: "Claude Code (Go) - AI 编程助手",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// 在任何子命令执行前, 幂等初始化全局 LLM 指标采集钩子。
			// 这样 chat/run/feishu/team/dashboard 等任意路径发起的 LLM 调用,
			// 都会写入 <stateDir>/metrics/llm.jsonl, 供 dashboard 统一展示。
			//
			// 例外: dashboard / feishu 子命令会自己根据 --config (config.cwd/stateDir)
			// 解析出精确路径再调用 InitGlobalLLMCollector (支持重定位)。在这里用 os.Getwd()
			// 做兜底会提前创建错位的 .claude-go 目录, 所以直接跳过, 交给子命令自己初始化。
			path := cmd.CommandPath()
			if strings.Contains(path, " dashboard") || strings.Contains(path, " feishu") {
				return
			}
			cwd, _ := os.Getwd()
			stateDirInput := ""
			if jsonCfg, _, err := loadRuntimeJSONConfig(); err == nil && jsonCfg != nil {
				if jsonCfg.Cwd != "" {
					cwd = jsonCfg.Cwd
				}
				stateDirInput = jsonCfg.StateDir
			}
			stateDir := basedir.ResolveDefault(stateDirInput, cwd)
			_ = os.MkdirAll(filepath.Join(stateDir, "metrics"), 0o755)
			metrics.InitGlobalLLMCollector(stateDir)
		},
		Long: `Claude Code (Go) 是 Claude Code 客户端的 Go 实现，对标 review/claude/ (Tengu)，
提供交互对话、一次性执行、飞书长连接、系统诊断与工具列表等能力。

核心能力包括 QueryEngine、Tool 系统、MCP 客户端、Agent Teams、记忆与 Hook、
权限与 Compact 等。子命令：

  chat    交互式 REPL，适合本地反复调试
  run     单次执行（类似 claude -p），适合脚本与自动化
  feishu  WebSocket 长连接，企业飞书/Lark 机器人后台
  doctor  检查 API Key、rg/git/shell、CLAUDE.md 等环境
  tools   列出当前注册的内置与 MCP 工具
  skills  查看已加载技能或某个技能详情
  roles   查看角色以及某个角色最终绑定的技能

全局标志（所有子命令可用，部分会被 feishu 的配置文件合并/覆盖）：
  --model、--api-key、--base-url、--max-tokens、--max-turns、
  --permission-mode、--system-prompt、--debug、--mcp-config、--config

更完整的用法、飞书配置示例与机器人斜杠命令说明请执行：

  claude-go help`,
		Example: `  # 交互式对话
  claude-go chat

  # 一次性提问（可叠加全局参数）
  claude-go run "解释这段 Go 代码" --model qwen3.5-plus

  # 飞书机器人（推荐配置文件）
  claude-go feishu --config=claude-go.json

  # 环境与工具
  claude-go doctor
  claude-go tools
  claude-go skills
  claude-go roles coder

  # 完整使用指南（含飞书、配置 JSON、斜杠命令）
  claude-go help`,
	}

	rootCmd.PersistentFlags().StringVar(&flagModel, "model", "qwen3.5-plus", "模型名称")
	rootCmd.PersistentFlags().StringVar(&flagExecutionModel, "execution-model", "", "工具消费回合自动路由的快速模型 (留空=关闭; 例: lfm2.5:2.6b-q4_k_m)")
	rootCmd.PersistentFlags().StringVar(&flagPlanModel, "plan-model", "", "规划期独立模型 (plan mode 专用, provider:model; 留空=规划期沿用主模型; 例: kimi:kimi-k2)")
	rootCmd.PersistentFlags().StringVar(&flagAPIKey, "api-key", "", "API Key (或设置 ANTHROPIC_API_KEY 环境变量)")
	rootCmd.PersistentFlags().StringVar(&flagBaseURL, "base-url", "", "API base URL")
	rootCmd.PersistentFlags().IntVar(&flagMaxTokens, "max-tokens", 16384, "最大输出 token 数")
	rootCmd.PersistentFlags().IntVar(&flagMaxTurns, "max-turns", 0, "最大循环次数 (0=无限)")
	rootCmd.PersistentFlags().StringVar(&flagPermission, "permission-mode", "bypass", "权限模式 (default/auto/plan/bypass)")
	rootCmd.PersistentFlags().StringVar(&flagSystemPrompt, "system-prompt", "", "自定义系统提示词")
	rootCmd.PersistentFlags().BoolVarP(&flagPrint, "print", "p", false, "Print 模式 (非交互式)")
	rootCmd.PersistentFlags().BoolVar(&flagDebug, "debug", false, "调试模式")
	rootCmd.PersistentFlags().StringVar(&flagMCPConfig, "mcp-config", "", "MCP 配置文件路径 (JSON，顶层 mcpServers)")
	rootCmd.PersistentFlags().StringVar(&flagConfig, "config", "", "JSON 配置文件路径 (默认自动发现 CLAUDE_GO_CONFIG、./claude-go.json、~/.claude-go/config/config.json)")
	rootCmd.PersistentFlags().StringVar(&flagResume, "resume", "", "恢复指定 session ID 的对话")
	rootCmd.PersistentFlags().StringArrayVar(&flagAttach, "attach", nil, "附加媒体文件 (图片/视频/音频, 可重复); 图片直接进视觉模型, 视频抽帧, 音频 ASR 转写")
	rootCmd.PersistentFlags().StringVar(&flagCwd, "cwd", "", "工作目录 (优先级最高, 覆盖配置文件 cwd; 工具相对路径与会话存储均锚定于此)")
	rootCmd.PersistentFlags().BoolVar(&flagFinalOnly, "final-only", false, "仅输出最终回答文本 (不输出 thinking 过程, 供程序化调用解析)")
	rootCmd.PersistentFlags().BoolVar(&flagEmitSession, "emit-session-id", false, "结束时输出 __CLAUDE_GO_SESSION__=<id> 行, 供调用方记录以便 --resume 续聊")
	rootCmd.PersistentFlags().StringVar(&flagRunLabel, "run-label", "", "本次运行在轨迹页上的显示名 (空=用 session id)。供 claude-go coder 这类调用方按任务名标注, 使运行可按名检索")
	rootCmd.PersistentFlags().StringVar(&flagAgent, "agent", "", "以单个 agent 的身份运行本次任务 (角色名或 agent 资产名); 命中后该 agent 的 systemPrompt/skills/工具面/模型/回合上限生效。名字两层都查不到时报错退出, 不回落成普通 run")
	rootCmd.PersistentFlags().StringArrayVar(&flagPlugins, "plugin", nil, "启用插件包 (可重复, 每个旗标一个名字): 展开 <root>/plugins/<name>/ 的 agents/skills/hooks 并装进本次运行, 先于 --agent 解析 (插件可提供本次要跑的 agent)。找不到该插件时报错退出, 不静默跳过")
	rootCmd.PersistentFlags().BoolVarP(&flagContinue, "continue", "c", false, "恢复最近一次对话")
	rootCmd.PersistentFlags().StringVar(&flagVerify, "verify", "", "任务验收命令 (方案三 L6a): run 结束后在 cwd 执行, 非零即未过; 配 --best-of 启用择优")
	rootCmd.PersistentFlags().IntVar(&flagBestOf, "best-of", 1, "任务级 best-of-N 尝试次数 (1=关; 2-4, 需配 --verify; 每次全新会话)")
	rootCmd.PersistentFlags().StringVar(&flagAdvisor, "advisor", "", "启用 advisor 顾问工具并指定模型别名 (provider:model); \"off\" 强制关闭 (覆盖配置文件)")
	rootCmd.PersistentFlags().StringVar(&flagAutoPlan, "auto-plan", "", "引擎级复杂任务自判断 → 自动 Plan Mode (on|off); 空=读配置, 默认开")
	rootCmd.PersistentFlags().StringVar(&flagComplexityMode, "complexity-mode", "", "复杂度判定模式: heuristic|llm|hybrid (空=读配置, 默认 heuristic)")
	rootCmd.PersistentFlags().StringVar(&flagAllowedTools, "allowed-tools", "", "工具白名单 (逗号分隔); 非空时仅这些工具可见且可执行, 其余硬拒 (用于受限托管 agent)")
	rootCmd.PersistentFlags().StringVar(&flagOutputFormat, "output-format", "text", "输出格式: text (默认) | json (final/is_error/error_kind/session_id 顶层 envelope, 供程序化调用; token 用量见 metrics JSONL)")
	rootCmd.PersistentFlags().StringVar(&flagAgentDBURL, "agentdb-url", "", "agentDB serve 地址 (如 http://127.0.0.1:8787); 设置后装配层2语义增强注入 (AgentDBSemanticInjectHook)")
	rootCmd.PersistentFlags().IntVar(&flagAgentDBTopK, "agentdb-topk", 5, "agentDB 每源检索条数")
	rootCmd.PersistentFlags().IntVar(&flagAgentDBBudget, "agentdb-budget", 600, "agentDB 注入上下文 token 预算")
	rootCmd.PersistentFlags().IntVar(&flagAgentDBReserve, "agentdb-reserve", 200, "agentDB 注入为任务正文预留 token")

	rootCmd.AddCommand(chatCmd())
	rootCmd.AddCommand(runCmd())
	rootCmd.AddCommand(coderCmd())
	rootCmd.AddCommand(feishuCmd())
	rootCmd.AddCommand(doctorCmd())
	rootCmd.AddCommand(toolsCmd())
	rootCmd.AddCommand(skillsCmd())
	rootCmd.AddCommand(rolesCmd())
	rootCmd.AddCommand(dashboardCmd())
	rootCmd.AddCommand(llmGatewayCmd())
	rootCmd.AddCommand(workerCmd())
	rootCmd.AddCommand(evoCmd())
	rootCmd.AddCommand(backupCmd())
	rootCmd.AddCommand(sandboxCmd())
	rootCmd.AddCommand(teamCmd())
	rootCmd.AddCommand(helpCmd())
	rootCmd.AddCommand(codeintelMCPServerCmd())
	rootCmd.AddCommand(platformMCPServerCmd()) // L5 platform-mcp-server (design/02 §3.5)
	rootCmd.AddCommand(wechatCmd())
	rootCmd.AddCommand(swarmCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// teamCmd 团队管理 CLI (list/status/resume/stop/delete)。
// resume/stop/delete 通过 dashboard API 转发到运行中的主进程执行。
func teamCmd() *cobra.Command {
	var dashboardURL string
	cmd := &cobra.Command{
		Use:   "team",
		Short: "团队管理: 列出、查看状态、恢复、停止、删除",
		Long:  `通过 CLI 管理 claude-go 团队, 无需打开 dashboard。`,
	}
	cmd.PersistentFlags().StringVar(&dashboardURL, "dashboard", "http://localhost:7777", "Dashboard API 地址")

	// list
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "列出所有团队",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonCfg, _ := feishu.LoadJSONConfig(flagConfig)
			stateDirInput := ""
			if jsonCfg != nil {
				stateDirInput = jsonCfg.StateDir
			}
			cwd, _ := os.Getwd()
			stateDir := basedir.ResolveDefault(stateDirInput, cwd)
			teamsDir := filepath.Join(stateDir, "teams")
			entries, err := os.ReadDir(teamsDir)
			if err != nil {
				return fmt.Errorf("读取团队目录失败: %w", err)
			}
			if len(entries) == 0 {
				fmt.Println("暂无团队")
				return nil
			}
			fmt.Printf("%-24s %-12s %-16s %s\n", "名称", "状态", "工作流", "目标")
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				path := filepath.Join(teamsDir, e.Name(), "team.json")
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				var t struct {
					Name      string `json:"name"`
					Status    string `json:"status"`
					Workflow  string `json:"workflow"`
					Objective string `json:"objective"`
				}
				if json.Unmarshal(data, &t) != nil {
					continue
				}
				obj := t.Objective
				if len(obj) > 30 {
					obj = obj[:27] + "..."
				}
				fmt.Printf("%-24s %-12s %-16s %s\n", t.Name, t.Status, t.Workflow, obj)
			}
			return nil
		},
	})

	// status
	cmd.AddCommand(&cobra.Command{
		Use:   "status <名称>",
		Short: "查看团队详细状态",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonCfg, _ := feishu.LoadJSONConfig(flagConfig)
			stateDirInput := ""
			if jsonCfg != nil {
				stateDirInput = jsonCfg.StateDir
			}
			cwd, _ := os.Getwd()
			stateDir := basedir.ResolveDefault(stateDirInput, cwd)
			path := filepath.Join(stateDir, "teams", args[0], "team.json")
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("团队 %q 不存在", args[0])
			}
			var t struct {
				Name       string    `json:"name"`
				Status     string    `json:"status"`
				Workflow   string    `json:"workflow"`
				Objective  string    `json:"objective"`
				CreatedAt  time.Time `json:"createdAt"`
				StartedAt  time.Time `json:"startedAt"`
				FinishedAt time.Time `json:"finishedAt"`
				Error      string    `json:"error"`
				Progress   *struct {
					Phase        string    `json:"phase"`
					Iteration    int       `json:"iteration"`
					BytesWritten int64     `json:"bytesWritten"`
					UpdatedAt    time.Time `json:"updatedAt"`
					TaskID       string    `json:"taskId"`
				} `json:"progress"`
				Agents map[string]struct {
					Name     string    `json:"name"`
					Role     string    `json:"role"`
					Status   string    `json:"status"`
					Phase    string    `json:"phase"`
					LastBeat time.Time `json:"lastBeat"`
				} `json:"agents"`
			}
			if err := json.Unmarshal(data, &t); err != nil {
				return fmt.Errorf("解析团队状态失败: %w", err)
			}
			fmt.Printf("团队: %s [%s]\n", t.Name, t.Status)
			fmt.Printf("工作流: %s\n", t.Workflow)
			fmt.Printf("目标: %s\n", t.Objective)
			fmt.Printf("创建时间: %s\n", t.CreatedAt.Format("2006-01-02 15:04:05"))
			if !t.StartedAt.IsZero() {
				elapsed := time.Since(t.StartedAt)
				if !t.FinishedAt.IsZero() {
					elapsed = t.FinishedAt.Sub(t.StartedAt)
				}
				fmt.Printf("耗时: %v\n", elapsed.Round(time.Second))
			}
			if t.Error != "" {
				fmt.Printf("错误: %s\n", t.Error)
			}
			if t.Progress != nil && (t.Progress.Phase != "" || t.Progress.Iteration > 0 || !t.Progress.UpdatedAt.IsZero()) {
				line := fmt.Sprintf("实时进展: 阶段=%s 轮次=%d 产出=%dB", t.Progress.Phase, t.Progress.Iteration, t.Progress.BytesWritten)
				if !t.Progress.UpdatedAt.IsZero() {
					line += fmt.Sprintf(" (%s前)", time.Since(t.Progress.UpdatedAt).Round(time.Second))
				}
				fmt.Println(line)
			}
			if len(t.Agents) > 0 {
				fmt.Println("Agent 状态:")
				for _, a := range t.Agents {
					line := fmt.Sprintf("  - %s [%s]", a.Name, a.Status)
					if a.Role != "" {
						line += fmt.Sprintf(" (%s)", a.Role)
					}
					if a.Phase != "" {
						line += fmt.Sprintf(" 阶段=%s", a.Phase)
					}
					if !a.LastBeat.IsZero() {
						line += fmt.Sprintf(" 心跳=%s前", time.Since(a.LastBeat).Round(time.Second))
					}
					fmt.Println(line)
				}
			}
			return nil
		},
	})

	// resume / stop / delete 共用同一个 HTTP 调用逻辑
	for _, action := range []string{"resume", "stop", "delete"} {
		a := action
		cmd.AddCommand(&cobra.Command{
			Use:   fmt.Sprintf("%s <名称>", a),
			Short: fmt.Sprintf("%s 团队", map[string]string{"resume": "恢复", "stop": "停止", "delete": "删除"}[a]),
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				url := fmt.Sprintf("%s/api/actions/team/%s/%s", strings.TrimRight(dashboardURL, "/"), a, args[0])
				resp, err := http.Post(url, "application/json", nil)
				if err != nil {
					return fmt.Errorf("请求 dashboard 失败: %w", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("dashboard 返回 %s", resp.Status)
				}
				fmt.Printf("✅ 团队 %s 已执行 %s\n", args[0], a)
				return nil
			},
		})
	}

	return cmd
}

// sandboxCmd 管理 claude-go 本地沙盒执行层。
func sandboxCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "沙盒 runtime 诊断、状态和清理",
		Long:  "诊断 Native cgroup v2、Docker、process guard 的可用性，并管理 claude-go 创建的沙盒残留资源。",
	}
	cmd.PersistentFlags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认 <cwd>/.claude-go)")

	cmd.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "检查 runtime 自动适配结果",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 8*time.Second)
			defer cancel()
			mgr := sandbox.DefaultManager()
			fmt.Println("Sandbox runtime probes:")
			for _, p := range mgr.ProbeAll(ctx) {
				status := "unavailable"
				if p.OK {
					status = "available"
				}
				fmt.Printf("- %-16s %-11s memory=%t pids=%t cpu=%t detail=%s\n",
					p.Runtime, status, p.CanMemory, p.CanPids, p.CanCPU, p.Detail)
			}
			selected := mgr.SelectRuntime(ctx, true)
			if selected.OK {
				fmt.Printf("Selected for team verification: %s\n", selected.Runtime)
			} else {
				fmt.Printf("Selected for team verification: none (%s)\n", selected.Detail)
				fmt.Println("Tip: on macOS start Docker/Colima; on Linux run sandbox setup-cgroupv2 or delegate a writable cgroup v2 subtree.")
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "列出沙盒容器和日志目录",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "label=claude-go.sandbox=true", "--format", "table {{.Names}}\t{{.Status}}\t{{.Image}}").CombinedOutput(); err == nil && strings.TrimSpace(string(out)) != "" {
				fmt.Println(string(out))
			} else {
				fmt.Println("Docker sandboxes: unavailable or none")
			}
			cwd, _ := os.Getwd()
			root := basedir.ResolveDefault(stateDir, cwd)
			logDir := filepath.Join(root, "sandboxes")
			entries, err := os.ReadDir(logDir)
			if err != nil {
				fmt.Printf("Log sandboxes: none (%s)\n", logDir)
				return nil
			}
			fmt.Printf("Log sandboxes in %s: %d\n", logDir, len(entries))
			for i, e := range entries {
				if i >= 20 {
					fmt.Printf("... %d more\n", len(entries)-i)
					break
				}
				fmt.Println("-", e.Name())
			}
			return nil
		},
	})

	var cleanupLogs bool
	cleanupCmd := &cobra.Command{
		Use:   "cleanup",
		Short: "清理 claude-go 标记的沙盒残留",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=claude-go.sandbox=true").Output()
			if err == nil {
				ids := strings.Fields(string(out))
				if len(ids) > 0 {
					rmArgs := append([]string{"rm", "-f"}, ids...)
					if rmOut, rmErr := exec.CommandContext(ctx, "docker", rmArgs...).CombinedOutput(); rmErr != nil {
						return fmt.Errorf("docker cleanup failed: %w\n%s", rmErr, string(rmOut))
					}
					fmt.Printf("Removed docker sandboxes: %d\n", len(ids))
				} else {
					fmt.Println("Removed docker sandboxes: 0")
				}
			}
			if cleanupLogs {
				cwd, _ := os.Getwd()
				root := basedir.ResolveDefault(stateDir, cwd)
				logDir := filepath.Join(root, "sandboxes")
				if err := os.RemoveAll(logDir); err != nil {
					return fmt.Errorf("remove sandbox logs: %w", err)
				}
				fmt.Printf("Removed sandbox logs: %s\n", logDir)
			}
			return nil
		},
	}
	cleanupCmd.Flags().BoolVar(&cleanupLogs, "logs", false, "同时删除 .claude-go/sandboxes 日志目录")
	cmd.AddCommand(cleanupCmd)

	return cmd
}

func helpCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "help",
		Short:   "查看完整使用指南",
		Long:    "打印包含基础用法、飞书配置、通用参数、机器人命令与多 Agent 协作说明的完整指南。",
		Example: `  claude-go help`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Print(fullHelpGuide)
		},
	}
}

// chatCmd 交互式 REPL 模式。
// 对应 TS: 默认的交互式入口
func chatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "chat",
		Short: "交互式对话 (REPL 模式)",
		Example: `  claude-go chat

  # 指定模型与调试输出
  claude-go chat --model qwen3.5-plus --debug`,
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			eng, err := buildEngine()
			if err != nil {
				return err
			}

			cwd := eng.Config.Cwd
			store, storeErr := session.NewSessionStore(cwd)
			if storeErr == nil {
				eng.SessionStore = store
				defer store.Close()

				if flagResume != "" {
					msgs, err := store.ResumeSession(flagResume)
					if err != nil {
						return fmt.Errorf("resume session %s: %w", flagResume, err)
					}
					eng.Messages = msgs
					fmt.Printf("[已恢复会话 %s, %d 条消息]\n", flagResume, len(msgs))
				} else if flagContinue {
					sid, err := store.MostRecentSessionID()
					if err == nil {
						msgs, err := store.ResumeSession(sid)
						if err == nil {
							eng.Messages = msgs
							fmt.Printf("[已恢复最近会话 %s, %d 条消息]\n", sid, len(msgs))
						}
					}
				}
			}

			// 交互式会话的 trace ctx 与 run span 挂点。
			//
			// 缺 RunID 时, 这一轮里的每一次 llm_call / turn 都会经
			// tracestore.bucketName("") 落进 **trace-orphan** 桶 —— 而 webapp 的轨迹页
			// 只列 trace-run-*(见 pkg/feishu/session_trace.go 的包注释, 那儿有完整的
			// 因果链), 于是整段交互式会话在页面上**不存在**。实测: 一次 chat 往
			// orphan 写 2 条 (llm_call + turn), 而这个桶自 8 月起累积到 1.8 万条 /
			// 10 MB 且一直在涨 —— 它就是这个泄漏的唯一剩余产生方。
			//
			// RunID 在**进程开始时发一次**, 不是每轮一次: REPL 的一次进程就是一个会话
			// (与 pkg/feishu 的 session 语义对齐)。每轮换一个 RunID 会把一次对话撕成
			// N 个桶, "这次会话里到底发生了什么"就再也拼不回来 —— 而这正是轨迹页要
			// 回答的那个问题。
			//
			// defer 的位置与 run 路径同理: 它是唯一"无论从哪条出口退出都会走一遍"的
			// 地方 (/exit、EOF、EOF 后的正常 return 三条)。它按引用读 runID, 所以下面
			// 那句赋值晚于这里也没关系; runID 仍为空时 recordRunSpan 自己的守卫会跳过
			// —— 拿不到 session id 时硬编一个会造出"看起来属于某个会话"的假归属。
			sessionStart := time.Now()
			ctx, runID := beginCLISessionTrace(store)
			defer func() {
				if eng == nil {
					return
				}
				// 状态取自**本函数的返回值**而不是"跑完了就当成功": 成功路径也必须
				// 报数, 但报什么数要由驱动侧决定 (与 run 路径读 errKind 同一纪律)。
				status, errText := tracestore.RunStatusOK, ""
				if retErr != nil {
					status, errText = tracestore.RunStatusFailed, retErr.Error()
				}
				// objective 传空: 一次 REPL 会话没有单一任务目标, 拿第一轮输入冒充
				// 会让"这条 run 干了什么"看起来比实际确定。
				recordCLISessionRunSpan(eng.TraceStore, runID, flagRunLabel, store,
					eng.Config.Model, status, errText, sessionStart)
			}()

			history, _ := session.NewPromptHistory()

			cmdRegistry := commands.NewRegistry()
			commands.RegisterBuiltins(cmdRegistry)

			siCfg := swarmintel.DefaultConfig()
			siCfg.Notify = func(_, msg string) {
				fmt.Println(msg)
			}
			siEngine := swarmintel.NewEngine(eng.APIClient, siCfg)

			shouldExit := false
			cmdCtx := &commands.CommandContext{
				Engine:       eng,
				SessionStore: store,
				History:      history,
				SwarmIntel:   siEngine,
				Cwd:          cwd,
				OnClear: func() {
					// 清空后可选: 创建新 session
				},
				OnExit: func() {
					shouldExit = true
				},
			}

			fmt.Println("Claude Code (Go) - 交互式模式")
			fmt.Println("输入消息开始对话, /help 查看所有命令")
			if store != nil {
				fmt.Printf("Session: %s\n", store.SessionID())
			}
			fmt.Println("---")

			scanner := bufio.NewScanner(os.Stdin)
			scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

			for {
				fmt.Print("\n> ")
				if !scanner.Scan() {
					break
				}
				input := strings.TrimSpace(scanner.Text())
				if input == "" {
					continue
				}

				if strings.HasPrefix(input, "/") {
					cmdName, cmdArgs := commands.ParseSlashCommand(input)
					if cmd := cmdRegistry.Find(cmdName); cmd != nil {
						_ = cmd.Execute(cmdArgs, cmdCtx)
						if shouldExit {
							break
						}
						continue
					}
					fmt.Printf("未知命令: /%s, 输入 /help 查看可用命令\n", cmdName)
					continue
				}

				if history != nil {
					sid := ""
					if store != nil {
						sid = store.SessionID()
					}
					history.Append(input, sid)
				}

				// 用上面那个带 RunID 的 ctx —— 每轮重新 context.Background()
				// 会把 RunID 丢掉, 于是本轮的 Span 又回到 orphan 桶 (这正是改造前
				// 的形态: 有 session 存储、却没有会话级的轨迹归属)。
				streamCh := eng.SubmitStream(ctx, input)

				printStreamEvents(streamCh)

				usage := eng.GetContextUsage()
				if usage.Percentage > 80 {
					fmt.Printf("\033[33m[上下文 %.0f%% — 建议 /compact]\033[0m\n", usage.Percentage)
				}
			}
			return nil
		},
	}
}

// runCmd 一次性执行模式 (对应 claude -p)
func runCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run [prompt]",
		Short: "一次性执行 (Print 模式)",
		Args:  cobra.MinimumNArgs(1),
		Example: `  claude-go run "你的问题"
  claude-go run "/go trading-v2 分析英伟达"
  claude-go run "/team create trading-v2 分析英伟达" --permission-mode bypass`,
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := buildEngine()
			if err != nil {
				return err
			}

			userPrompt := strings.Join(args, " ")

			// H4-3 长程档上限: 在这里 (命令作用域) 读一次 —— agentFactory 闭包与
			// 它同在 runCmd 里, Execute 时的 ctx 拿不到配置。读不到 (无配置文件 /
			// ai 段缺失) 时留 0, cliAgentRunner.Execute 回落到编译期兜底值。
			//
			// 独立读一次而不复用下面那个 jsonCfg: 那一份在 `/` 前缀分支内部, 而
			// 这个值要覆盖的是整条 run 路径。多读一次磁盘的代价是几毫秒, 换来的是
			// "配置读取点"与"使用点"在同一个作用域里 —— 复用会让以后的读者以为
			// 只有 slash 命令才认这个配置。
			cliLongHorizonTurns := 0
			if cfgLong, _, cfgErr := loadRuntimeJSONConfig(); cfgErr == nil && cfgLong != nil && cfgLong.AI != nil {
				cliLongHorizonTurns = cfgLong.AI.LongHorizonMaxTurns
			}

			// --agent: 以单个 agent 的身份跑这一条 prompt。
			//
			// ★ 位置在 slash 分发**之前**, 有两个都不能省的理由:
			//
			//  1. 语义上 `--agent` 是"不要编排, 只要这一个 agent"。让它先进斜杠分支就等于
			//     "某个 agent 的 prompt 以 / 开头时, 它会变成一次团队编排" —— 而那个分支
			//     里 agentFactory 装出来的是**角色**, 与 --agent 指定的那个不是同一个东西。
			//  2. 安全上 --agent 会写 eng.Config 的工具面 (applyAgentToolFace), 而斜杠分支
			//     拥有全量能力。先进去再出来, 白名单可能已经被那条路径的能力面绕过。
			//
			// 这里**不看** eng.Config.AllowedTools: 上面那条闸门管的是"不可信输入里的 /",
			// 而 --agent 是调用方显式给的旗标 (不是 prompt 内容), 且它自己的工具面已经
			// 在 applyAgentToolFace 里与既有白名单求过交 —— 是收窄方向, 不需要再挡。
			if strings.TrimSpace(flagAgent) != "" {
				return runAsAgent(cmd, eng, userPrompt, cliLongHorizonTurns)
			}

			// 受限会话 (--allowed-tools 已设置) 绝不进入 slash-command 分发:
			// /team 等编排路径拥有全量能力, 会绕过白名单 — 不可信输入以 "/"
			// 开头时按普通 prompt 处理 (安全前提: 白名单只对引擎路径强制)。
			if strings.HasPrefix(userPrompt, "/") && eng.Config.AllowedTools == nil {
				cwd := eng.Config.Cwd
				jsonCfg, _, cfgErr := loadRuntimeJSONConfig()
				if cfgErr != nil {
					return fmt.Errorf("加载配置文件失败: %w", cfgErr)
				}
				cmdRegistry := commands.NewRegistry()
				commands.RegisterBuiltins(cmdRegistry)

				siCfg := swarmintel.DefaultConfig()
				siCfg.Notify = func(_, msg string) { fmt.Println(msg) }
				siEngine := swarmintel.NewEngine(eng.APIClient, siCfg)

				agentFactory := func(ctx context.Context, role, systemPrompt string) (agent.AgentRunner, error) {
					mcfg, _ := ctx.Value(agent.ModelConfigKey{}).(modelconfig.ResolvedConfig)
					// stage 从**创建时的** ctx 读: RunMetadata 由编排层在装配这个
					// runner 之前写进 ctx (agent.runAgent), Execute 时的 ctx 已被
					// 换过, 拿不到。与 feishu 的 sessionAgentRunner 同一理由。
					meta := agent.RunMetadataFromContext(ctx)
					// 阶段声明的 Hooks/Skills (21.8.12 P4/X1) 同样在**创建时**捕获 ——
					// 与上面两句同一条理由: 声明由 executeStage 写进创建 ctx, Execute
					// 时 ctx 已被换过 (agent.runAgent 会重建), 那时候读回来是空的。
					// 读回是空值不报错, 只是静默不生效 —— 所以必须在这里接入。
					//
					// Model 不在这里捕获: 它的消费点在 runAgent 内部 (那个函数**覆写**
					// ModelConfigKey, 提前塞进 ctx 的值会被整体冲掉, 见其注释), 所以
					// mcfg 读到的已经是"阶段声明解析之后"的最终值。
					decl := agent.StageDeclFromContext(ctx)
					// H4-3 长程档上限 (装配期捕获, Execute 时手边没有 jsonCfg)。
					return &cliAgentRunner{
						eng: eng, role: role, stage: meta.Stage,
						systemPrompt: systemPrompt, modelCfg: mcfg, stageDecl: decl,
						longHorizonMaxTurns: cliLongHorizonTurns,
					}, nil
				}
				agentPool := agent.NewAgentPool(agentFactory, 8)

				// 任务持久化路径与 feishu bot / dashboard 对齐: <state>/tasks/tasks.json
				// (通过 basedir.Layout 标准化, 避免多处写入多个不同位置)
				stateDirInput := ""
				if jsonCfg != nil {
					stateDirInput = jsonCfg.StateDir
				}
				tasksStateDir := basedir.ResolveDefault(stateDirInput, cwd)
				tasksLayout, _ := basedir.NewLayout(tasksStateDir)
				if tasksLayout != nil {
					_ = os.MkdirAll(tasksLayout.Tasks, 0o755)
				}
				tasksFilePath := filepath.Join(tasksStateDir, "tasks", "tasks.json")
				if tasksLayout != nil {
					tasksFilePath = tasksLayout.TasksFilePath()
				}
				taskStore := builtin.NewTaskStore(tasksFilePath)

				// 信箱后端 (21.8.9-D6): 落在 <state>/statestore 下, 与团队数据同根。
				// 传 nil 会回落到**进程内** MemStore —— CLI 一次 run 结束进程就退, 那条
				// 路径上等于所有消息当场蒸发, 而且不会有任何报错。所以必须显式给。
				teamStateStore := statestore.NewFileStore(filepath.Join(tasksStateDir, "statestore"))
				dagAdapter := agent.NewTaskStoreDAGAdapter(taskStore, func() []agent.DAGTaskSummary {
					raw := taskStore.ReadyTasks()
					out := make([]agent.DAGTaskSummary, len(raw))
					for i, t := range raw {
						out[i] = agent.DAGTaskSummary{
							ID: t.ID, Subject: t.Subject, Description: t.Description,
							Status: t.Status, Owner: t.Owner, DependsOn: t.DependsOn, Priority: t.Priority,
							WriteScopes: t.WriteScopes,
						}
					}
					return out
				}, func() []agent.DAGTaskSummary {
					raw := taskStore.GetAllTasks()
					out := make([]agent.DAGTaskSummary, len(raw))
					for i, t := range raw {
						out[i] = agent.DAGTaskSummary{
							ID: t.ID, Subject: t.Subject, Description: t.Description,
							Status: t.Status, Owner: t.Owner, DependsOn: t.DependsOn, Priority: t.Priority,
							WriteScopes: t.WriteScopes,
						}
					}
					return out
				})

				// 加载模型配置并创建解析器 (层级: role > plan > 全局默认)
				var modelResolver *agent.PlanConfigResolver
				if jsonCfg != nil {
					if registry, resolver, err := modelconfig.LoadFromConfig(jsonCfg.ToModelConfigJSON()); err == nil && resolver != nil {
						modelResolver = agent.NewPlanConfigResolver(resolver)
						_ = registry
					}
				}

				// 加载用户自定义动态工作流 (<state>/workflows/*.json) —— 与飞书 bot 路径对齐
				// (design/02 §六: 工作流应跨入口一致)。此前仅飞书 bot 加载, CLI headless run
				// 无法跑自定义/图模式工作流。
				// 角色表与 dashboard 的校验点用**同一份来源** (21.8.12 W1):
				// 只认 cwd 的话, dashboard 用 <stateDir> 校验通过的工作流, 在这里
				// 会查不到它的角色并静默退化成内联 prompt —— 一个通过了校验的
				// 工作流, 运行时静默地不按它校验时的样子跑。
				// 失败明细与降级明细都要打出来。此前这里把 details 丢成 `_`,
				// 于是"40 个文件里 3 个装载失败"在这条路径上只表现为一个数字,
				// 而那 3 个是谁、为什么, 只有翻日志才知道 (而且日志里也没有)。
				//
				// 装载在这里做一次并把 roles 存下来给下面 teamMgr 用: 装载用的
				// 角色表与执行时用的**必须同一份实例**, 否则"装载时解析得出的结论"
				// 与"执行时解析得出的结论"可以在同一个进程里不同 —— 这是 21.8.12
				// W1 那条"校验通过、运行时静默降级"在进程内部的微型版本。
				runRoles := agent.NewRoleRegistryFor(cwd, tasksStateDir)
				if wfDir := filepath.Join(tasksStateDir, "workflows"); wfDir != "" {
					rep := agent.LoadWorkflowsFromDirReport(wfDir, runRoles)
					if rep.Loaded > 0 || rep.Failed > 0 || len(rep.Deferred) > 0 {
						fmt.Printf("[run] 自定义工作流加载: %s\n", rep.Summary(wfDir))
						for _, d := range rep.Details {
							fmt.Printf("[run][workflow][fail] %s\n", d)
						}
						for _, d := range rep.Deferred {
							fmt.Printf("[run][workflow][degraded] %s\n", d)
						}
					}
				}
				// 内置工作流自检 (21.8.12 W4): 与 bot 路径同一份实现、同一口径。
				// CLI 的单跑是最容易撞上"某个内置 stage 的 agent 名字写错了"的场景
				// —— 因为它不经过任何 dashboard 校验, 直接跑。
				//
				// 这里只打印**有问题**的那几条 (不像 bot 那样无条件打一行汇总):
				// bot 一次启动只走一次, 而 run 是频繁路径 —— 每次 run 都刷一行
				// "全绿"只会把这一整段变成噪声, 而它真正要传递的信息是"有坏的吗"。
				// 需要完整读数时走 `claude-go doctor`。
				for _, is := range agent.AuditBuiltinWorkflows(runRoles) {
					if is.Hard != "" {
						fmt.Printf("[run][workflow][builtin][fail] %s: %s\n", is.Name, is.Hard)
						continue
					}
					for _, w := range is.Warnings {
						fmt.Printf("[run][workflow][builtin][degraded] %s: %s\n", is.Name, w)
					}
				}
				// 进化引擎: headless run 路径与飞书路径同构装配 (design/03 §1.2 开环1 修复)。
				// 此前 CLI 团队路径不实例化 Evolution → workflow.go 全部学习分支被
				// `we.evolution != nil` 守卫跳过, 下游平台流量零学习。
				evoDir := filepath.Join(tasksStateDir, "evolution")
				if tasksLayout != nil {
					evoDir = tasksLayout.Evolution
				}
				evoEngine := agent.NewEvolutionEngine(evoDir, eng.APIClient)
				// 第七章缺口修复: Evolution→Memory 交叉学习接线——MemoryIngestFn
				// 此前全仓零赋值点 (声明 evolution.go:143, 消费 :568), 高质量团队经验
				// (质量≥0.6 且 5 分钟内新提炼) 注入 L1 情景记忆供后续会话检索。
				if eng.MemoryStore != nil {
					ms := eng.MemoryStore
					evoEngine.MemoryIngestFn = func(content, source string, topics []string) {
						ms.Add(&memory.MemoryEntry{Content: content, Source: source, Topics: topics, Importance: 0.7})
					}
				}
				// 统一学习循环 (design/03 §4.3): 此前 NewEvolutionLoop 全仓零生产调用方,
				// submitLearn 永远走回落直调 —— 循环写完了但没通电。挂上它才有去重/预算闸/
				// 空闲期深度整理, 也才有学习器 d/e 的运行相位。
				// 13.8.2: 把 LLM 计量器接进循环, learn_llm_tokens (source=evolution) 才有
				// 记账落点。typed-nil: GlobalLLMCollector 可能返回 nil *Collector, 直接
				// 传接口会把 nil 包成非 nil 接口, l.metrics != nil 误判为真。
				var evoSink agent.MetricsSink
				if c := metrics.GlobalLLMCollector(); c != nil {
					evoSink = c
				}
				evoLoop := agent.NewEvolutionLoop(evoEngine, evoSink, agent.EvolutionLoopConfig{})
				evoLoop.EnableStructureLearning(agent.StructureConfig{
					StateDir: filepath.Dir(evoDir),
					// Reflector 用 fallback 档位模型 —— §4.2 H3 明令禁止拿被评估的主模型
					// 自评。没配 fallback 时为 nil, prompt 进化会跳过并记日志, 不静默降级。
					Reflector: cliEvoReflector(eng.APIClient),
				})
				evoLoop.Start(context.Background())
				teamMgr := agent.NewProductionTeamManager(agent.TeamManagerConfig{
					BaseDir: filepath.Join(tasksStateDir, "teams"),
					Cwd:     cwd,
					Factory: agentFactory,
					Pool:    agentPool,
					Notify:  func(_, msg string) { fmt.Println(msg) },
					LLM:     eng.APIClient,
					// 与上面那次装载**同一个实例** (21.8.12 W1): 装载工作流用的是哪张
					// 角色表, 执行时就该是哪张。两处各建一份 (哪怕参数相同) 也会在
					// 装载与执行之间留下一个可观测的窗口 —— 期间自定义角色文件若变了,
					// 两张表就不同, 而差异只在"某个 stage 退化成内联 prompt"上现形。
					Roles:              runRoles,
					TaskTracker:        dagAdapter,
					StateStore:         teamStateStore,
					Concurrency:        eng.APIClient.Guard,
					PlanConfigResolver: modelResolver,
					Evolution:          evoEngine,
					EvolutionLoop:      evoLoop,
					TraceStore:         eng.TraceStore, // gate Span (design/03 §4.1 第 5 种 Kind)
					Dreamer:            &cliDreamAdapter{dreamer: cliDreamer},
					SkillCreator:       cliSkillCreator(tasksLayout, tasksStateDir, eng),
				})

				// 资产索引 (21.8.12 P0/P1): 六种资产按标准布局装载一次, 装进进程级索引。
				// 这是 Agent 工具的 subagent_type 能被查表 (P5) 的前提 —— 没有索引,
				// 清单为空、查表恒 miss, 模型填什么名字都只会走角色名子串猜测。
				fmt.Printf("[run] 资产装载: %s\n", loadAssetCatalog(cwd, tasksStateDir))
				// 与 runAsAgent 同理: 上一行重建了进程级目录, --plugin 合并进去的
				// 插件 agents 要重新合并进这份新目录的副本, 否则团队路径查不到它们。
				if cliPlugins != nil {
					mergePluginAgents(agent.AgentCatalog(), cliPlugins.bundles)
				}

				cmdCtx := &commands.CommandContext{
					Engine:     eng,
					TeamMgr:    teamMgr,
					SwarmIntel: siEngine,
					Cwd:        cwd,
					WaitSync:   true,
					OnClear:    func() {},
					OnExit:     func() {},
				}

				cmdName, cmdArgs := commands.ParseSlashCommand(userPrompt)
				if c := cmdRegistry.Find(cmdName); c != nil {
					return c.Execute(cmdArgs, cmdCtx)
				}
				fmt.Printf("未知命令: %s\n", cmdName)
				return nil
			}

			// 会话持久化: 支持 --resume 续聊与 --emit-session-id (供 webapp 等程序化调用)
			store, storeErr := session.NewSessionStore(eng.Config.Cwd)
			if storeErr == nil {
				eng.SessionStore = store
				defer store.Close()
				if flagResume != "" {
					msgs, err := store.ResumeSession(flagResume)
					if err != nil {
						return fmt.Errorf("resume session %s: %w", flagResume, err)
					}
					eng.Messages = msgs
				}
			}

			// trace 上下文: 装配点 (buildEngine) 已把配置声明的工单/实例索引写进
			// runTraceIDs, 这里把它挂到本次运行的 ctx 上 —— 之后引擎内每一层
			// (TraceCapture / api.setTraceHeaders / 落盘 record) 都从 ctx 读, 不需要
			// 逐层传参。空 IDs 时 trace.With 是恒等操作 (逐字段判空合并), 行为与改造前
			// 逐字节一致。
			ctx := trace.With(context.Background(), runTraceIDs)

			// 会话级轨迹: 把 RunID 补上, 否则本次运行的 Span 会落进 trace-orphan 桶
			// (tracestore.bucketName("") -> trace-orphan), 而 webapp 的轨迹页只列
			// trace-run-*(见 pkg/feishu/session_trace.go 的包注释, 那儿有完整的因果链)。
			//
			// 只补 RunID/SessionID 两个键, **不覆盖** runTraceIDs 里已有的工单/实例索引
			// —— trace.With 逐字段判空合并, 上面那行已经把它们挂上去了, 这里再 With
			// 一次是叠加而不是替换。
			//
			// store 为 nil (没有 home 目录等) 时跳过: 没有 session id 就没有可辨的桶名,
			// 硬编一个会造出"看起来属于某个会话"的假归属。
			runID := ""
			if store != nil {
				sid := store.SessionID()
				runID = fmt.Sprintf("run-%s-%d", sid, runTraceSeq.Add(1))
				ctx = trace.With(ctx, trace.IDs{RunID: runID, SessionID: sid})
			}

			// run span 的挂点。**位置就是全部要害**: defer 是唯一"无论从哪条 return
			// 退出都会走一遍"的地方, 而本 RunE 里有多条提前 return —— 那些恰恰是最
			// 该被看见的失败运行 (与 pkg/agent/run_span.go 文件头第 22-31 行同源论证)。
			//
			// 此前 run span 只有两个产生方: 团队路径 (pkg/agent/run_span.go, 挂在
			// beginRun 的收尾闭包) 与飞书会话路径 (pkg/feishu/session_trace.go)。CLI 的
			// run 路径**一个都没有** —— 而 webapp 的轨迹页只从第一条 kind:"run" 的 span
			// 派生 name/workflow/status/duration (traces.ts:526 summarizeTrace)。缺了它,
			// 这些运行在页面上就是一行裸哈希 + 一个灰点, 按任何关键字都搜不到。
			// 实测: 234 个 trace-run-* 里 24 个如此, mtime 恰好落在 coder 长跑那一晚。
			//
			// 注意这与"桶名对不上"是**两个**问题: 上面那段已经补了 RunID, 所以文件
			// 正确落在 trace-run-* 桶里、被 glob 收得到 —— 缺的是文件里的内容。
			//
			// errKind 提前到这里声明 (下面原本是 `var final, errKind string`), 是为了让
			// 这个 defer 能排在它前面: 延迟闭包按引用捕获, 但被捕获的变量**声明必须在
			// defer 语句之前**才是合法的。
			start := time.Now()
			var errKind string
			defer func() {
				if eng == nil {
					return
				}
				// 显示名在收尾时现算 —— 回落顺序与 pkg/feishu 的 Session.displayName
				// 一致: 显式标签优先, 没有就用 session id。session id 是**有意义**的
				// 身份而不是占位符, 所以不印 "(未命名)" 之类会掩盖真实取值的东西。
				name := strings.TrimSpace(flagRunLabel)
				if name == "" && store != nil {
					name = store.SessionID()
				}
				status := tracestore.RunStatusOK
				if errKind != "" {
					status = tracestore.RunStatusFailed
				}
				recordRunSpan(eng.TraceStore, runID, name, eng.Config.Model, status, errKind, userPrompt, start)
			}()

			attachBlocks, err := ingestAttachments(flagAttach)
			if err != nil {
				return err
			}
			jsonMode := flagOutputFormat == "json"
			// 方案三 L6a: 任务级 best-of-N——配 --verify 时每次尝试跑全新会话,
			// 首个过验收命令的尝试胜出 (Large Language Monkeys 的 pass@k 幂律,
			// 只作用于"最终可验证产物", 步级不做——13.3.3 L6 纪律)。
			attempts := 1
			if flagVerify != "" && flagBestOf > 1 {
				attempts = flagBestOf
				if attempts > 4 {
					attempts = 4
				}
			}
			// errKind 已提前到上面的 run span 挂点处声明 (deferred 闭包要捕获它)。
			var final string
			verifyPassed := false
			for attempt := 1; ; attempt++ {
				if attempt > 1 {
					eng, err = buildEngine() // 全新会话, 避免上次轨迹污染本次采样
					if err != nil {
						return err
					}
				}
				wmlInjectExperience(eng, userPrompt) // L7 注入半环 (每次尝试一致)
				streamCh := eng.SubmitStreamBlocks(ctx, userPrompt, attachBlocks)
				var ek string
				final, ek = consumeStreamEvents(streamCh, jsonMode)
				errKind = ek
				if flagVerify == "" || errKind != "" {
					break
				}
				if verr := wmlVerifyRun(eng.Config.Cwd, flagVerify); verr == nil {
					verifyPassed = true
					break
				} else {
					log.Printf("[best-of] 第 %d/%d 次尝试未过验收: %v", attempt, attempts, verr)
				}
				if attempt >= attempts {
					break
				}
			}
			if flagVerify != "" {
				log.Printf("[best-of] 验收结论: passed=%v (attempts=%d)", verifyPassed, attempts)
			}
			sessionID := ""
			if store != nil {
				sessionID = store.SessionID()
			}
			// 第七章缺口修复: CLI 单查询 Dreaming 触发 (此前仅团队收尾/飞书会话触发,
			// 单查询路径零触发)。AfterQuery 自带全部门控 (enable/时间/会话数), 幂等安全。
			if cliDreamer != nil {
				cliDreamer.AfterQuery(ctx)
			}
			// 方案三 L7 蒸馏半环: 会话末把轨迹交独立档位反思器蒸馏经验卡。
			// 默认关 (env CLAUDE_GO_WML_EXPERIENCE=1 开): 蒸馏是一次额外的强模型调用,
			// 不能悄悄加在每个 run 末尾。H3: 反思器别名经 CLAUDE_GO_WML_REFLECTOR 指定,
			// 与主模型同名即拒 (不同源纪律)。
			if eng.Config.WeakModel.Enabled && errKind == "" && wmlExperienceEnabled() {
				if reflAlias := strings.TrimSpace(os.Getenv("CLAUDE_GO_WML_REFLECTOR")); reflAlias != "" {
					if reflClient, err := buildClientFromAlias(reflAlias); err == nil && reflClient.Model != eng.Config.Model {
						transcript := summarizeMessagesForDistill(eng.Messages)
						if cards, err := learners.DistillExperience(ctx, reflClient, transcript, "cli-run:"+sessionID); err == nil {
							jsonCfgL, _, _ := loadRuntimeJSONConfig()
							cwdL, _ := os.Getwd()
							sdInput := ""
							if jsonCfgL != nil {
								sdInput = jsonCfgL.StateDir
							}
							if err := learners.AppendExperienceCards(basedir.ResolveDefault(sdInput, cwdL), cards); err == nil {
								log.Printf("[weakmodel] 经验卡蒸馏入库 %d 条", len(cards))
							}
						} else {
							log.Printf("[weakmodel] 经验蒸馏未入库: %v", err)
						}
					}
				}
			}
			// 装配自报的索引字段在此补全: buildEngine 采集时还不知道会话 ID
			// (SessionStore 是它返回之后才建的), 而报告要能被"按会话反查"。
			// 只补索引, 不动任何装配实质 —— 故 fingerprint 不受影响。
			if cliAssembly != nil {
				cliAssembly.SessionID = sessionID
				cliAssembly.TicketID = runTraceIDs.TicketID
				cliAssembly.InstanceID = runTraceIDs.InstanceID
			}
			if jsonMode {
				return emitRunEnvelope(final, sessionID, errKind, cliAssembly, cliAssemblyReconcile)
			}
			if flagEmitSession && store != nil {
				fmt.Printf("\n__CLAUDE_GO_SESSION__=%s\n", sessionID)
			}
			return nil
		},
	}
}

// beginCLISessionTrace 给出**交互式会话**这一轮的 trace 上下文: ctx + 本次运行的
// RunID (拿不到会话 id 时为空)。
//
// 抽成函数而不是留在 chat 的 RunE 里, 是为了让它有一条**能测**的判据 —— 这个修复
// 的失效形态恰恰是无声的: 少一句 trace.With, 整段交互式会话就落回 orphan 桶,
// 没有任何一层会报错, 界面也不报错 (只是那一屏什么都没有)。
//
// RunID 在进程开始时发一次, 不是每轮一次: REPL 的一次进程就是一个会话 (与
// pkg/feishu 的 session 语义对齐)。每轮换一个 RunID 会把一次对话撕成 N 个桶,
// "这次会话里到底发生了什么"就再也拼不回来。
//
// store 为 nil 时**刻意不发号**: 没有会话 id 就没有可辨的桶名, 硬编一个会造出
// "看起来属于某个会话"的假归属 (与 run 路径同一条守卫)。
func beginCLISessionTrace(store *session.SessionStore) (context.Context, string) {
	ctx := trace.With(context.Background(), runTraceIDs)
	if store == nil {
		return ctx, ""
	}
	sid := store.SessionID()
	runID := fmt.Sprintf("run-%s-%d", sid, runTraceSeq.Add(1))
	return trace.With(ctx, trace.IDs{RunID: runID, SessionID: sid}), runID
}

// recordCLISessionRunSpan 写交互式会话的自述 run span。
//
// 与 recordRunSpan 同底座同 attrs 键, 差别只有取名回落顺序的输入 (这里直接收
// store 与原始 label, 因为会话 id 只有 store 知道)。抽出来同样是为了可测。
func recordCLISessionRunSpan(ts *tracestore.Store, runID, label string, store *session.SessionStore,
	model, status, errText string, start time.Time) {
	// 回落顺序与 pkg/feishu 的 Session.displayName 一致: 显式标签优先, 没有就用
	// session id。session id 是**有意义**的身份而不是占位符, 所以不印 "(未命名)"。
	name := strings.TrimSpace(label)
	if name == "" && store != nil {
		name = store.SessionID()
	}
	// objective 传空: 一次 REPL 会话没有单一任务目标, 拿第一轮输入冒充会让
	// "这条 run 干了什么"看起来比实际确定。
	recordRunSpan(ts, runID, name, model, status, errText, "", start)
}

// recordRunSpan 写本次 CLI run 的自述 span (KindRun)。
//
// 与 pkg/feishu/session_trace.go 的收尾闭包**同形、同 attrs 键** —— 两条路径的运行
// 在轨迹页上应当长得一样, 不能一条有名字一条只有哈希。差异只有一处且是刻意的:
// 那边 InputRef 传空串 (会话路径的 objective 在别处), 这里传真正的 userPrompt。
//
// fail-open 且零分配退出: store 未装配 (裸调/测试/没有 home 目录) 时直接返回, 与
// pkg/agent/run_span.go:50「无 trace 的执行不产 run span, 免得写出挂不上任何桶的
// 孤儿」同一守卫。runID 为空同理 —— 空 TraceID 会被 tracestore 归进 trace-orphan。
func recordRunSpan(ts *tracestore.Store, runID, name, model, status, errText, objective string, start time.Time) {
	if ts == nil || runID == "" {
		return
	}
	attrs := map[string]any{
		// workflow 取 session: 如实描述"这条运行走的是 CLI 会话路径, 不属于任何团队
		// 工作流"。留空会让它落进页面的「(未知工作流)」桶, 与"真的没归因"混在一起。
		"workflow": tracestore.SessionWorkflowLabel,
		"status":   status, // "completed" | "failed" —— 页面的 StatusDot 按这两个字面量判色
		"team":     name,   // 页面按 team 分组, 与 workflow 是两个维度
	}
	// model 为空时不写这个键而不是写空串: 空串与"模型就是空"在读数上不可分
	// (同 session_trace.go:114)。
	if model != "" {
		attrs["model"] = model
	}
	if errText != "" {
		// 与 pkg/feishu/session_trace.go 的 truncateResult(errText, 500) 输出逐字节相同
		// (那边就是 textutil.Truncate 的本地包装), 只是这里直接用底座的那个,
		// 免得 cmd 包再抄一份同义函数。
		attrs["error"] = textutil.Truncate(errText, 500, "...")
	}
	ts.Write(tracestore.Span{
		TraceID: runID,
		SpanID:  internal_hook.GenerateUUID(),
		Kind:    tracestore.KindRun,
		Name:    name,
		// 刻意不设 ParentID: run 是这棵树的根 (与 run_span.go:88 同一处理)。
		InputRef:  ts.MakeRef(objective),
		OutputRef: ts.MakeRef(errText),
		Attrs:     attrs,
		TS:        start.UnixMilli(),
		DurMS:     time.Since(start).Milliseconds(),
	})
}

// wmlInjectExperience L7 注入半环: 弱模型模式下按任务文本检索经验卡注入 system prompt。
// 检索是确定性关键词重合 (learners.RetrieveExperience), 不多花一次模型调用。
func wmlInjectExperience(eng *engine.QueryEngine, userPrompt string) {
	if eng == nil || !eng.Config.WeakModel.Enabled {
		return
	}
	jsonCfgL, _, _ := loadRuntimeJSONConfig()
	cwdL, _ := os.Getwd()
	sdInput := ""
	if jsonCfgL != nil {
		sdInput = jsonCfgL.StateDir
	}
	expStateDir := basedir.ResolveDefault(sdInput, cwdL)
	cards, _ := learners.LoadExperienceCards(expStateDir)
	if len(cards) == 0 {
		return
	}
	top := learners.RetrieveExperience(cards, userPrompt, 3)
	if hints := learners.FormatExperienceHints(top); hints != "" {
		eng.PromptMgr.ExperienceHints = hints
		log.Printf("[weakmodel] 注入经验卡 %d 条 (库共 %d 条)", len(top), len(cards))
	}
}

// wmlVerifyRun L6a 验收命令执行器: cwd 下 sh -c, 3 分钟超时, 零退出码即过。
func wmlVerifyRun(cwd, cmd string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = cwd
	out, err := c.CombinedOutput()
	if err != nil {
		tail := strings.TrimSpace(string(out))
		if len(tail) > 300 {
			tail = tail[:300] + "…"
		}
		return fmt.Errorf("%v: %s", err, tail)
	}
	return nil
}

// wmlExperienceEnabled L7 蒸馏开关 (默认关: 会话末多一次强模型调用必须是显式选择)。
func wmlExperienceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_GO_WML_EXPERIENCE"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// summarizeMessagesForDistill 把会话消息渲染成蒸馏用摘要 (尾部 20 条, 各截 300 字符)。
func summarizeMessagesForDistill(msgs []types.Message) string {
	var b strings.Builder
	start := 0
	if len(msgs) > 20 {
		start = len(msgs) - 20
	}
	for _, m := range msgs[start:] {
		for _, blk := range m.Content {
			var text string
			switch blk.Type {
			case types.ContentBlockText:
				text = blk.Text
			case types.ContentBlockToolUse:
				text = "调用工具 " + blk.Name + " " + string(blk.Input)
			case types.ContentBlockToolResult:
				text = "工具结果: " + blk.Content
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			if len(text) > 300 {
				text = text[:300] + "…"
			}
			b.WriteString(string(m.Type) + ": " + text + "\n")
		}
	}
	return b.String()
}

// runEnvelope is the --output-format json result: a single parseable object
// carrying the final answer, session id, and error classification, so callers
// no longer scrape free-form stdout (design/09 §8 structured IO contract).
type runEnvelope struct {
	Final     string `json:"final"`
	SessionID string `json:"session_id,omitempty"`
	IsError   bool   `json:"is_error"`
	ErrorKind string `json:"error_kind,omitempty"`
	// Assembly 是本次运行的装配自报 (方案 20.6.4 D0.7)。
	//
	// 随 envelope 一起出而不是单独落一个文件: 平台已经在按 envelope 判定结果, 多一个
	// 文件就多一次"忘了取/取错版本"的机会; 而自我报告的**及时性**正是它的全部价值 ——
	// 它在这一份响应里, 就绝不会与这次运行错位。
	//
	// 指针 + omitempty: 采集失败 (引擎未构造等) 时整段缺席, 而不是给一个空对象假装
	// "装配为空" —— 后者在对账侧会读成"声明了但一项没装上", 把采集失败误报成装配失败。
	Assembly *assembly.Report `json:"assembly,omitempty"`

	// AssemblyReconcile 是**声明**与上面那份自报的对账差异 (方案 20.6.4 D0.7)。
	//
	// 与 Assembly 并排出、且用独立字段而不是塞进 Report 里: 自报是"实际装成了什么"
	// (可观测事实), 对账差异是"两者之差" (结论)。混成一个对象, 消费方就没法只信
	// 事实而不接受结论 —— 而事实与结论的可信度并不相同。差异为空数组与整段缺席也
	// 是两回事: 前者是"对过账, 一致", 后者是"没对账", 面板上必须分得开。
	//
	// 注意本字段是**本进程自比**: 声明来自本进程读到的配置, 实际来自本进程的引擎。
	// 平台手里那份岗位模板是另一份声明, 它要在平台侧拿同一份报告再对一次 ——
	// 两处都报, 才能分出"下发丢了"与"下发到了但没装配上"。
	AssemblyReconcile *assembly.ReconcileResult `json:"assembly_reconcile,omitempty"`
}

func emitRunEnvelope(final, sessionID, errKind string, report *assembly.Report, reconcile *assembly.ReconcileResult) error {
	env := runEnvelope{
		Final:             final,
		SessionID:         sessionID,
		IsError:           errKind != "",
		ErrorKind:         errKind,
		Assembly:          report,
		AssemblyReconcile: reconcile,
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// ingestAttachments 把 --attach 指定的媒体文件转换为内容块 (多模态输入)。
func ingestAttachments(paths []string) ([]types.ContentBlock, error) {
	var blocks []types.ContentBlock
	for _, p := range paths {
		bs, err := media.IngestFile(p)
		if err != nil {
			return nil, fmt.Errorf("处理附件 %s 失败: %w", p, err)
		}
		blocks = append(blocks, bs...)
	}
	return blocks, nil
}

// feishuCmd 飞书长连接后台守护模式。
// 通过 WebSocket 与飞书服务器建立持久连接，
// 接收消息事件，桥接到 QueryEngine 处理，回复结果。
//
// 支持两种配置方式:
//  1. CLI 参数: claude-go feishu --app-id=xxx --app-secret=xxx
//  2. JSON 配置: claude-go feishu --config=claude-go.json
//
// JSON 配置文件会先加载，CLI 参数会覆盖 JSON 中的值。
// 环境变量优先级最低: FEISHU_APP_ID, FEISHU_APP_SECRET, FEISHU_DOMAIN
func feishuCmd() *cobra.Command {
	var (
		appID          string
		appSecret      string
		domain         string
		sessionTimeout int
		maxSessions    int
		mentionOnly    bool
		cwd            string
		configPath     string
		httpPort       int // serve 模式的 HTTP 端口 (覆盖 wiki.apiPort); 支持 --addr host:port 形式
		addr           string
		dispatchMode   string // "" | "queue" (分布式控制面: 挂 cluster 任务队列端点)
		// placementPrefer 阶段执行的放置偏好 (design/01 §4.9 Placement.Prefer):
		// "local"(默认, 行为不变) | "any" | "remote:<worker 名>"。
		placementPrefer string
		// cwd 三档位 (design/02 §3.3): 全部默认关 —— 不声明档位时行为与改造前一字不变。
		workspaceMode   string
		workspaceVolume string
		gitRemote       string
		gitBranch       string
		gitBaseRef      string
		gitMaxFileMB    int
	)

	cmd := &cobra.Command{
		Use:     "feishu",
		Aliases: []string{"serve"},
		Short:   "飞书长连接后台守护模式 (WebSocket); 以 serve 调用时为 headless 全栈模式(不连飞书)",
		Long: `启动飞书机器人守护进程，通过 WebSocket 长连接接收飞书消息。
每个对话(chat_id)维护独立的 AI 会话，支持多轮对话、工具执行、MCP 调用等完整能力。

需要在飞书开发者后台创建企业自建应用，获取 App ID 和 App Secret。
应用需要订阅 im.message.receive_v1 事件，并开启长连接模式。

配置优先级（后者覆盖前者）: 环境变量 < JSON 配置文件 < 本命令标志与全局标志。
AI Key 可来自 --api-key、ANTHROPIC_API_KEY / DASHSCOPE_API_KEY，或 JSON 中 ai.apiKey。

飞书专用参数:
  --config          JSON 配置文件路径，可包含 feishu、ai、mcpServers、hooks、permissionMode 等
  --app-id          飞书应用 App ID；未填时使用 FEISHU_APP_ID
  --app-secret      飞书应用 App Secret；未填时使用 FEISHU_APP_SECRET
  --domain          飞书域名: feishu (国内，默认) 或 lark (国际)；可用 FEISHU_DOMAIN
  --session-timeout 单会话空闲超时，单位分钟 (默认 30)
  --max-sessions    最大并发会话数 (默认 100)
  --mention-only    群聊是否仅响应 @机器人 (默认 true)
  --cwd             工作目录，影响工具与 CLAUDE.md 加载 (默认当前目录)

与根命令相同的全局参数在显式传入时会覆盖 JSON 中的 ai.* 等对应项，例如:
  --model、--api-key、--base-url、--max-tokens、--max-turns、
  --permission-mode、--system-prompt、--debug、--mcp-config

推荐用法示例:
  claude-go feishu --config=claude-go.json
  claude-go feishu --app-id=cli_xxx --app-secret=xxx --api-key=sk-xxx
  FEISHU_APP_ID=cli_xxx FEISHU_APP_SECRET=xxx ANTHROPIC_API_KEY=sk-xxx claude-go feishu

JSON 配置文件示例:
  {
    "feishu": { "appId": "cli_xxx", "appSecret": "xxx" },
    "ai": { "model": "qwen3.5-plus", "apiKey": "sk-xxx", "baseUrl": "https://..." },
    "mcpServers": {
      "my-server": { "command": "npx", "args": ["-y", "some-mcp-server"] }
    },
    "hooks": [{ "event": "PreToolUse", "command": "echo pre" }],
    "permissionMode": "bypass"
  }`,
		Example: `  # 使用配置文件 (推荐)
  claude-go feishu --config=claude-go.json

  # 使用命令行参数（需同时提供 AI Key）
  claude-go feishu --app-id=cli_xxx --app-secret=xxx --api-key=sk-xxx

  # 环境变量
  FEISHU_APP_ID=cli_xxx FEISHU_APP_SECRET=xxx ANTHROPIC_API_KEY=sk-xxx claude-go feishu

  # 国际版 Lark + 自定义工作目录
  claude-go feishu --config=claude-go.json --domain=lark --cwd=/path/to/project`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 1. 构建默认配置
			config := feishu.DefaultBotConfig()
			if configPath == "" {
				configPath = flagConfig
			}

			// 2. 加载 JSON 配置 (优先级低于 CLI 参数)
			jsonCfg, err := feishu.LoadJSONConfig(configPath)
			if err != nil {
				return fmt.Errorf("加载配置文件失败: %w", err)
			}
			if jsonCfg != nil {
				jsonCfg.ApplyToBot(config)
				// JSON config 中的 mcpServers 和 hooks
				if len(jsonCfg.MCPServers) > 0 {
					config.MCPServers = jsonCfg.MCPServers
				}
				if len(jsonCfg.Hooks) > 0 {
					config.Hooks = jsonCfg.Hooks
				}
				if configPath != "" {
					fmt.Printf("[飞书Bot] 已加载配置文件: %s\n", configPath)
				}
			}

			// 3. CLI 参数覆盖 JSON 配置
			if appID == "" {
				appID = os.Getenv("FEISHU_APP_ID")
			}
			if appSecret == "" {
				appSecret = os.Getenv("FEISHU_APP_SECRET")
			}
			if domain == "" {
				if d := os.Getenv("FEISHU_DOMAIN"); d != "" {
					domain = d
				}
			}
			if appID != "" {
				config.AppID = appID
			}
			if appSecret != "" {
				config.AppSecret = appSecret
			}
			if domain != "" && domain != "feishu" {
				config.Domain = domain
			}

			// serve 别名 = headless 全栈模式 (design/02 §四 单体部署形态):
			// :18080 wiki+dashboard+teams+cron+sync 全量, 不连飞书 WS, 无需飞书凭证。
			if cmd.CalledAs() == "serve" {
				config.Headless = true
			}

			// --addr host:port / --http-port: 覆盖 wiki.apiPort (serve 模式 HTTP 端口)。
			// 默认 18080 (K8s 单体/分布式部署经此暴露服务)。
			if addr != "" {
				if _, portStr, err := net.SplitHostPort(addr); err == nil {
					if p, perr := strconv.Atoi(portStr); perr == nil {
						httpPort = p
					}
				}
			}
			if config.Headless {
				if httpPort == 0 && config.Wiki.APIPort == 0 {
					httpPort = 18080 // headless 默认端口
				}
				if httpPort > 0 {
					config.Wiki.APIPort = httpPort
					config.Wiki.Enabled = true
				}
			} else if httpPort > 0 {
				config.Wiki.APIPort = httpPort
			}
			if !config.Headless && (config.AppID == "" || config.AppSecret == "") {
				return fmt.Errorf("需要飞书应用凭证: 使用 --app-id/--app-secret 或 --config 或设置 FEISHU_APP_ID/FEISHU_APP_SECRET (headless 部署请用 serve 子命令)")
			}

			apiKey := getAPIKey()
			baseURL := flagBaseURL
			modelAlias := flagModel

			// 如果 JSON 配置中已有 providers，优先使用
			// 否则从 CLI 参数/环境变量构建合成 provider
			if len(config.Providers) == 0 {
				if apiKey == "" && config.Headless {
					// headless HTTP-only 部署 (design/02 §四): 无 LLM 凭证时合成占位 provider,
					// 让 :18080 wiki/dashboard/teams 全栈仍可启动 (LLM 调用会失败但 HTTP 端点可用)。
					// 真实 LLM E2E 经 --config 或环境变量提供真凭证。
					config.Providers = map[string]feishu.ProviderConfig{
						"placeholder": {
							Name: "placeholder", BaseURL: "http://127.0.0.1:1", APIKey: "placeholder",
							Models: map[string]feishu.ProviderModelConfig{"placeholder:noop": {}},
						},
					}
					config.ModelAlias = "placeholder:noop"
					log.Printf("[serve] 未配置 LLM 凭证, 使用占位 provider (HTTP 栈可用, LLM 调用不可用)")
				} else if apiKey == "" {
					return fmt.Errorf("需要 AI API Key: 设置 ANTHROPIC_API_KEY 环境变量、--api-key 参数，或在配置文件中设置 providers")
				} else {
					if baseURL == "" {
						baseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic/v1"
					}
					// 推断 provider 名称
					providerName := "compatible"
					lb := strings.ToLower(baseURL)
					switch {
					case strings.Contains(lb, "dashscope") || strings.Contains(lb, "aliyuncs.com"):
						providerName = "dashscope"
					case strings.Contains(lb, "anthropic.com"):
						providerName = "anthropic"
					case strings.Contains(lb, "openai.com"):
						providerName = "openai"
					}
					// 构建合成 provider
					config.Providers = map[string]feishu.ProviderConfig{
						providerName: {
							Name:    providerName,
							BaseURL: baseURL,
							APIKey:  apiKey,
							Models: map[string]feishu.ProviderModelConfig{
								providerName + ":" + modelAlias: {},
							},
						},
					}
					config.ModelAlias = providerName + ":" + modelAlias
				}
			} else {
				// 有 providers 配置时，若 CLI 指定了 model，覆盖 ModelAlias
				if cmd.Flags().Changed("model") {
					config.ModelAlias = flagModel
				}
			}

			// --cwd 显式给了就**覆盖**配置文件, 与该 flag 的帮助文本("优先级最高,
			// 覆盖配置文件 cwd")一致 —— 改造前是 `if config.Cwd == ""` 才用它, 即配置
			// 文件反过来盖住了命令行。
			//
			// 这不是洁癖: 分布式部署里 distributed.yaml 给控制面传 `--cwd /workspace`
			// (共享 PVC 挂载点), 而 ConfigMap 里通常也写了 cwd。旧优先级下那个 flag 是
			// **空操作**, 团队 cwd 落回配置里的值 ⇒ 任务声明 /workspace 而 worker 声明
			// 配置里的路径, pvc 档的 checkWorkspace 必拒 —— **pvc 档在这类部署下全线
			// 不通**, 且报错说的是"工作区不一致"而不是"你的 --cwd 没生效"。
			// worker 二进制里本来就是 flag 覆盖 config, 只有控制面反着。
			if cwd != "" {
				config.Cwd = cwd
			}
			if config.Cwd == "" {
				config.Cwd, _ = os.Getwd()
			}

			// CLI 参数覆盖
			if cmd.Flags().Changed("system-prompt") {
				config.SystemPrompt = flagSystemPrompt
			}
			if cmd.Flags().Changed("debug") {
				config.Debug = flagDebug
			}
			if cmd.Flags().Changed("permission-mode") {
				config.PermissionMode = flagPermission
			}
			if cmd.Flags().Changed("mention-only") {
				config.MentionOnly = mentionOnly
			}
			if cmd.Flags().Changed("mcp-config") {
				config.MCPConfigPath = flagMCPConfig
			}
			if sessionTimeout > 0 && cmd.Flags().Changed("session-timeout") {
				config.SessionTimeout = time.Duration(sessionTimeout) * time.Minute
			}
			if maxSessions > 0 && cmd.Flags().Changed("max-sessions") {
				config.MaxSessions = maxSessions
			}

			// 统一 HTTP 服务: 把 dashboard 挂到 wiki API 同一端口,
			// 避免飞书 bot 运行期间还要单独开 dashboard 进程。
			// 前提: 配置了 Wiki.APIPort。
			// dashTeamAction 延迟绑定: 闭包捕获 botRef，在 HTTP 启动时 bot 已初始化。
			var botRef *feishu.Bot
			var dashCfgRef *dashboard.Config
			var taskSvc *agent.FileQueueTaskService
			// syncBridge 非 nil = sync 的执行体已交 TaskService (design/02 §3.5)。
			var syncBridge *synctask.Bridge
			// 远程 Agent 运行时 (design/02 §3.3): 仅 --dispatch-mode queue 下装配。
			// 见下方 dispatchMode 分支 (挂端点) 与 NewBot 之后的接线 (换执行工厂)。
			var workerBroker *worker.Broker
			var runtimeReg agent.RuntimeRegistry
			// clusterRegRef 分布式控制面 worker 注册表 (13.7.9 池观测注入用):
			// 仅 --dispatch-mode queue 下赋值; dashboard 池聚合经它只读 Alive()。
			var clusterRegRef *cluster.Registry
			// cwd 档位策略 (design/02 §3.3): Broker 与执行工厂必须用**同一份**,
			// 否则会出现"工厂声明了工作区但 Broker 不下发档位"这类半通电状态。
			var wsPolicy *worker.WorkspacePolicy
			if m, mErr := worker.ParseWorkspaceMode(workspaceMode); mErr != nil {
				return mErr
			} else if m != worker.WorkspaceModeUnset {
				wsPolicy = &worker.WorkspacePolicy{
					Mode: m, Volume: workspaceVolume,
					GitRemote: gitRemote, GitBranch: gitBranch, GitBaseRef: gitBaseRef,
					GitMaxFileBytes: int64(gitMaxFileMB) << 20,
				}
				// 策略不自洽在启动时就报错 (缺卷名/缺 remote 会让所有任务永远 pending)。
				if vErr := wsPolicy.Validate(); vErr != nil {
					return vErr
				}
				if dispatchMode != "queue" {
					return fmt.Errorf("--workspace-mode 需要 --dispatch-mode queue (档位只影响远程执行)")
				}
			}

			if config.Wiki.APIPort > 0 {
				// 与 bot 使用相同的 stateDir 解析逻辑，确保 dashboard 读写 metrics 路径一致。
				stateDir := basedir.ResolveDefault(config.StateDir, config.Cwd)
				dashCfg := dashboard.Config{
					StateDir: stateDir,
					TeamAction: func(action, teamName string, payload map[string]interface{}) error {
						if botRef == nil {
							return fmt.Errorf("bot 尚未初始化")
						}
						return botRef.DashboardTeamAction(action, teamName, payload)
					},
					LLMComplete: func(ctx context.Context, sys, user string) (string, error) {
						if botRef == nil {
							return "", fmt.Errorf("bot 尚未初始化")
						}
						return botRef.DashboardLLMComplete(ctx, sys, user)
					},
					MCPServers: func() interface{} {
						if botRef == nil {
							return []interface{}{}
						}
						return botRef.DashboardMCPServers()
					},
					ReloadSkills: func() {
						if botRef != nil {
							botRef.DashboardReloadSkills()
						}
					},
					// 与 ReloadSkills 同形, 但刷的是**另一份**索引 —— 见
					// Bot.DashboardReloadAssets 的注释: 资产索引有两处持有者,
					// 漏一处会让"新建的 agent"只在一个入口上可见。
					ReloadAgents: func() {
						if botRef != nil {
							botRef.DashboardReloadAssets()
						}
					},
				}
				dashCfgRef = &dashCfg

				// 分布式控制面 (design/02 §3.2): --dispatch-mode queue 时挂 cluster 端点,
				// 暴露任务队列 + worker 注册表, 供 worker 拉取执行。
				if dispatchMode == "queue" {
					stateDir := basedir.ResolveDefault(config.StateDir, config.Cwd)
					ss := storedual.OpenForStateDir(filepath.Join(stateDir, "statestore"), os.Getenv) // 双写: 文件+agentDB, 读源 CLAUDE_GO_STATESTORE_READ
					clusterQueue := cluster.NewQueue(ss, 5*time.Minute)
					clusterReg := cluster.NewRegistry(ss, 90*time.Second)
					// Broker 把队列包成 AgentRuntime 的远程实现 (design/02 §3.3):
					// 没有它, 队列只是"能入队但没人派活、执行体是桩"的空管道。
					brk, brkErr := worker.NewBroker(worker.BrokerOptions{
						Queue: clusterQueue, Registry: clusterReg,
						Workspace: wsPolicy,
						Logf:      func(f string, a ...any) { log.Printf(f, a...) },
					})
					if brkErr != nil {
						return fmt.Errorf("装配远程 runtime 失败: %w", brkErr)
					}
					workerBroker = brk
					runtimeReg = agent.NewRuntimeRegistry()
					clusterRegRef = clusterReg
					config.Wiki.APIExtensions = append(config.Wiki.APIExtensions,
						func(mux *http.ServeMux) {
							cluster.Mount(mux, clusterQueue, clusterReg)
							brk.Mount(mux) // /cluster/node-events: worker 的事件回传
							fmt.Printf("[Cluster] 分布式控制面已挂载 (/cluster/*), 任务队列就绪\n")
						})
				}

				// 动作队列消费方 (design/01 §4.12)。改造前 :7777 的动作队列有两处写入
				// (dashboard 的 extra_handlers / v13_handlers) 而**全仓没有消费方** ——
				// "等待 claude-go 主进程消费"那句提示是假承诺, 写进去的动作烂在盘上。
				// 这里就是那个缺失的主进程消费方: 没有它, ConsumeActions 只是建成未通电。
				//
				// Runner 与 ActionExecutor 都走惰性取 botRef, 与 TeamAction/CronController
				// 同一模式 —— 本段在 NewBot 内经 APIExtensions 执行, 早于 `botRef = bot`。
				// Runner 此刻只能留空 (bot 尚未构造), NewBot 返回后经 SetRunner 注入;
				// 在此之前若已有动作被消费, 任务会诚实地停在 pending 而不假装 running。
				taskSvc = agent.NewFileQueueTaskService(agent.TaskServiceOptions{
					Store:      storedual.OpenForStateDir(filepath.Join(stateDir, "statestore"), os.Getenv), // 双写: 文件+agentDB, 读源 CLAUDE_GO_STATESTORE_READ
					ActionsDir: filepath.Join(stateDir, ".dashboard", "actions"),
					ActionExecutor: agent.TeamActionExecutor(func(action, target string, payload map[string]any) error {
						if botRef == nil {
							return fmt.Errorf("bot 尚未初始化")
						}
						return botRef.DashboardTeamAction(action, target, payload)
					}),
				})
				dashboard.SetActionSink(taskSvc)

				// sync (IMA/WeRead) 的执行体交 TaskService (design/02 §3.5 通道表)。
				//
				// 只在真配了知识库时接线: KnowledgeRepo 为空时 registerSyncJobs 本就
				// 直接 return, 端点也没有可写的目标 —— 接了只会让 /sync/* 从 503
				// 变成"能提交但注定失败", 那是把没配置伪装成配置错误。
				//
				// 必须在 NewBot **之前**塞进 config: wikiAPI 在 NewBot 内构造并立即
				// Start, 之后再注入会留一个"已在听但执行体还是老路"的静默窗口。
				if config.Sync.KnowledgeRepo != "" {
					syncBridge = synctask.New(synctask.Options{Tasks: taskSvc, Config: config.Sync})
					config.Wiki.SyncTaskSubmitter = syncBridge
				}

				config.Wiki.APIExtensions = append(config.Wiki.APIExtensions,
					// 会话 HTTP 面 (/api/sessions/*): 建会话 / 发消息 / 轮询回复。
					//
					// 挂在这**同一个 mux** 上而不是自开端口: 鉴权是 httpauth.Middleware
					// 在 wiki API 的 Start 里对整条 mux 统一施加的 (pkg/wiki/api.go:126),
					// 自开 listener 会绕开它 —— dashboard 与 cluster 两组路由历史上就是这么
					// 漏掉的 (api.go 的 Start 注释里记着这件事)。
					//
					// botRef 惰性取: 本闭包在 NewBot 内经 APIExtensions 执行, 早于
					// `botRef = bot`。此刻取一个已构造好的值会拿到 nil 并让装配崩掉。
					func(mux *http.ServeMux) {
						sessionapi.Mount(mux, func() sessionapi.Host {
							if botRef == nil {
								return nil
							}
							sm := botRef.Sessions()
							if sm == nil {
								return nil
							}
							return sm.SessionHost()
						})
						fmt.Printf("[Sessions] 已挂载 /api/sessions 到 wiki API 端口 %d\n",
							config.Wiki.APIPort)
					},
					func(mux *http.ServeMux) {
						dsrv := dashboard.MountOn(*dashCfgRef, mux)
						// 注入活动定时任务调度器的【解析器】, 启用 /api/cron 写接口(创建/更新/启停/删除)。
						// 关键: 本闭包在 NewBot 内经 APIExtensions 执行, 早于 `botRef = bot`(NewBot 返回后)
						// → 此刻 botRef 尚为 nil。故传解析器请求期再取, 与 TeamAction 等同进程回调同一惰性模式。
						dsrv.SetCronController(func() dashboard.CronController {
							if botRef == nil {
								return nil
							}
							if cs := botRef.CronScheduler(); cs != nil {
								return cs
							}
							return nil
						})
						// 可用模型别名清单 (providers 注册的 alias), 供 /api/models →
						// webapp cron 面板的任务模型下拉。配置进程内本就持有, 只做只读枚举。
						dsrv.SetModelLister(func() []string {
							aliases := make([]string, 0, 16)
							for _, p := range config.Providers {
								for alias := range p.Models {
									aliases = append(aliases, alias)
								}
							}
							sort.Strings(aliases)
							return aliases
						})
						// 13.7.9 池观测: control 装配时注入 worker 注册表【解析器】, 启用
						// /api/pool 的 workers 段 (clusterRegRef 仅 queue 模式赋值)。
						dsrv.SetPoolLister(func() ([]cluster.WorkerInfo, error) {
							if clusterRegRef == nil {
								return nil, nil
							}
							return clusterRegRef.Alive()
						})
						fmt.Printf("[Dashboard] 已挂载到 wiki API 端口 %d (stateDir=%s)\n",
							config.Wiki.APIPort, stateDir)
					})
			}
			bot, err := feishu.NewBot(config)
			if err != nil {
				return fmt.Errorf("创建飞书机器人失败: %w", err)
			}
			botRef = bot

			// 配置热加载: providers/models/ai 改了立即生效, 不必重启控制面。
			// 控制面是 k8s 里长跑的 serve 进程, 重启会打断队列里在跑的任务。
			//
			// 两个 apply 分开调、各自返回错误: 模型段解析失败不该连带吞掉引擎开关
			// 的更新, 反之亦然。WatchConfig 在两个都失败时才报"重载失败", 任一成功
			// 都算这次配置变更已被吸收。
			{
				cfgPath, _ := feishu.ResolveJSONConfigPath(configPath)
				hotreload.WatchConfig(cfgPath, 5*time.Second, "claude-go-serve",
					func(p string) error {
						errModel := bot.ReloadModelConfig(p)
						errSw := bot.ApplyEngineSwitches(p)
						if errModel != nil && errSw != nil {
							return fmt.Errorf("模型配置: %v; 引擎开关: %v", errModel, errSw)
						}
						if errModel != nil {
							log.Printf("[HotReload] 模型配置重载失败 (引擎开关已应用, 继续用旧模型配置): %v", errModel)
						}
						return nil
					})
			}
			_ = dashCfgRef // suppress unused warning when Wiki.APIPort == 0

			// 团队运行器就位后再注入 (bot 构造完才有 TeamManager)。
			//
			// 有 syncBridge 时套一层 KindRunner 按 TaskSpec.Kind 分派: 不套的话那个
			// 唯一的 TeamRunner 会拿到 Kind="sync" 的档案并**按团队跑它** (Spec.Team
			// 是空的), 既不报错也不同步 —— 正是本仓反复吃过的静默错行为。
			// KindRunner 转发 TaskInterrupter, 否则 team 的 Stop/Pause 会静默失效
			// (interrupt 走的是 currentRunner().(TaskInterrupter) 类型断言)。
			if taskSvc != nil {
				var teamRunner agent.TaskRunner
				if bot.TeamManager() != nil {
					teamRunner = agent.NewTeamRunner(bot.TeamManager())
				}
				switch {
				case syncBridge != nil:
					kr := agent.NewKindRunner(teamRunner)
					kr.Register(synctask.Kind, syncBridge)
					taskSvc.SetRunner(kr)
					fmt.Printf("[TaskService] 执行器已接线 (team + kind=%v)\n", kr.Kinds())
				case teamRunner != nil:
					// 无 sync 接线时**不**套 KindRunner: 少一层包装 = 少一处
					// "包装漏转发某个可选接口" 的面。
					taskSvc.SetRunner(teamRunner)
				}
			}

			// 优雅退出: 捕获 SIGINT/SIGTERM
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				sig := <-sigCh
				fmt.Printf("\n[飞书Bot] 收到信号 %v，正在关闭...\n", sig)
				bot.Shutdown()
				cancel()
			}()

			// 周期消费动作队列。只靠 HTTP 请求内联 drain 不够: 队列里可能有
			// 上一次进程退出时残留的 pending 动作, 没人再发请求就永远不被消费。
			if taskSvc != nil {
				taskSvc.StartActionConsumer(ctx, 2*time.Second)
				fmt.Printf("[TaskService] 动作队列消费方已启动 (间隔 2s)\n")
			}

			// 远程 Agent 运行时接线 (design/02 §3.3 / design/01 §4.9)。
			//
			//   ① 本地执行体收编为 local-session runtime (lease=0 永不过期);
			//   ② 阶段执行工厂换成放置感知的工厂: Pick(Placement) → 本地或远程 worker;
			//   ③ 周期同步 worker 注册表 → RuntimeRegistry (心跳续租, 掉线由租约剔除)。
			//
			// 默认 --placement-prefer=local ⇒ 行为与改造前一致 (仍走本机执行);
			// 设成 remote:<worker 名> 才把阶段派到那个 worker。
			if workerBroker != nil && runtimeReg != nil && bot.TeamManager() != nil {
				localCaps := agent.RuntimeCaps{Bash: true, MaxParallel: 8}
				bot.TeamManager().WrapAgentFactory(func(local agent.CreateAgentFunc) agent.CreateAgentFunc {
					if rt := agent.NewLocalRuntime("local-session", localCaps, local); rt != nil {
						runtimeReg.Register(rt, 0)
					}
					return worker.RuntimeFactory(runtimeReg, &agent.Placement{
						Prefer: placementPrefer, Affinity: "team",
					}, wsPolicy)
				})
				go workerBroker.SyncLoop(ctx, 10*time.Second, runtimeReg, 90*time.Second)
				fmt.Printf("[Cluster] 远程 runtime 已接线 (placement prefer=%s, 每 10s 同步 worker)\n", placementPrefer)
				if wsPolicy != nil {
					fmt.Printf("[Cluster] cwd 档位=%s (要求 worker 具备 %v); 团队 cwd=%s\n",
						wsPolicy.Mode, wsPolicy.RequireCaps(), config.Cwd)
					if wsPolicy.Mode == worker.WorkspaceModeGit {
						fmt.Printf("[Cluster] git 档: remote=%s 分支模板=%s (阶段结束后控制面 ff-only 同步进团队 cwd, 门禁才看得见)\n",
							wsPolicy.GitRemote, wsPolicy.GitBranch)
					}
				}
				// k8s-job runtime (design/01 §4.9 三个内置 runtime 的最后一个):
				// 一个图节点一个一次性 K8s Job, 见 pkg/worker/k8sjob.go。
				// **默认关**: 只有 CLAUDE_GO_K8SJOB=1 才装配, 未启用时一切照旧。
				if worker.K8SJobEnabled() {
					jobRT, jErr := workerBroker.K8SJobRuntime(worker.K8SJobOptionsFromEnv())
					if jErr != nil {
						// 探活不通就**拒绝启动**而不是跳过: 悄悄跳过会让声明了
						// remote:k8s-job 的部署以为自己在用它、实际全落回本地 ——
						// 那是"配置写了但没生效"这类最难发现的问题。
						return fmt.Errorf("装配 k8s-job runtime 失败: %w", jErr)
					}
					runtimeReg.Register(jobRT, 0) // 它不靠心跳存活, 租约=永不过期
					fmt.Printf("[Cluster] k8s-job runtime 已注册 (caps=%+v); "+
						"用 --placement-prefer remote:%s 把阶段派给它\n",
						jobRT.Capabilities(), worker.K8SJobRuntimeName)
				}
			}

			fmt.Println("========================================")
			fmt.Println("  Claude Code (Go) - 飞书长连接模式")
			fmt.Println("========================================")
			fmt.Printf("  App ID:    %s\n", config.AppID)
			fmt.Printf("  Domain:    %s\n", config.Domain)
			fmt.Printf("  Model:     %s\n", config.ModelAlias)
			fmt.Printf("  Cwd:       %s\n", config.Cwd)
			fmt.Printf("  Sessions:  max=%d, timeout=%dm\n", config.MaxSessions, int(config.SessionTimeout.Minutes()))
			fmt.Printf("  @Only:     %v\n", config.MentionOnly)
			if len(config.MCPServers) > 0 || config.MCPConfigPath != "" {
				fmt.Printf("  MCP:       配置文件=%s, 内联=%d个\n", config.MCPConfigPath, len(config.MCPServers))
			}
			if len(config.Hooks) > 0 {
				fmt.Printf("  Hooks:     %d 条规则\n", len(config.Hooks))
			}
			fmt.Println("========================================")
			fmt.Println("正在连接飞书服务器...")

			return bot.Start(ctx)
		},
	}

	cmd.Flags().StringVar(&appID, "app-id", "", "飞书应用 App ID (或设置 FEISHU_APP_ID)")
	cmd.Flags().StringVar(&appSecret, "app-secret", "", "飞书应用 App Secret (或设置 FEISHU_APP_SECRET)")
	cmd.Flags().StringVar(&domain, "domain", "feishu", "飞书域名: feishu (国内) 或 lark (国际)")
	cmd.Flags().IntVar(&sessionTimeout, "session-timeout", 30, "会话超时时间 (分钟)")
	cmd.Flags().IntVar(&maxSessions, "max-sessions", 100, "最大并发会话数")
	cmd.Flags().BoolVar(&mentionOnly, "mention-only", true, "群聊中仅响应 @机器人 的消息")
	cmd.Flags().StringVar(&cwd, "cwd", "", "工作目录 (默认当前目录)")
	cmd.Flags().StringVar(&configPath, "config", "", "JSON 配置文件路径 (包含 feishu/ai/mcpServers/hooks 等)")
	cmd.Flags().IntVar(&httpPort, "http-port", 0, "serve 模式 HTTP 端口 (覆盖 wiki.apiPort; 默认 18080)")
	cmd.Flags().StringVar(&dispatchMode, "dispatch-mode", "", "分布式控制面模式: queue (挂 /cluster/* 任务队列端点)")
	cmd.Flags().StringVar(&placementPrefer, "placement-prefer", "local", "阶段执行放置偏好: local | any | remote:<worker 名> (需 --dispatch-mode queue)")
	cmd.Flags().StringVar(&workspaceMode, "workspace-mode", "", "远程执行的 cwd 档位: local | pvc | git (空=不声明, 行为与单机一致)")
	cmd.Flags().StringVar(&workspaceVolume, "workspace-volume", "", "pvc 档: 共享 RWX 卷名 (worker 必须挂同名卷)")
	cmd.Flags().StringVar(&gitRemote, "workspace-git-remote", "", "git 档: 约定 git 位置 (bare 仓 URL/路径)")
	cmd.Flags().StringVar(&gitBranch, "workspace-git-branch", worker.DefaultGitBranchTemplate, "git 档: 工作区分支模板 (支持 {team}/{run})")
	cmd.Flags().StringVar(&gitBaseRef, "workspace-git-base", "", "git 档: 分支首次创建时的起点 (空=origin/HEAD)")
	cmd.Flags().IntVar(&gitMaxFileMB, "workspace-git-max-file-mb", 32, "git 档: 单文件上限 (MiB); 超限的阶段直接失败而不是悄悄跳过")
	cmd.Flags().StringVar(&addr, "addr", "", "serve 模式监听地址 host:port (等价 --http-port, 便于 K8s 声明)")

	return cmd
}

// doctorCmd 系统诊断
func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "doctor",
		Short:   "系统诊断",
		Example: `  claude-go doctor`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("Claude Code (Go) - System Diagnostics")
			fmt.Println("=====================================")

			// 检查 API key
			apiKey := getAPIKey()
			if apiKey != "" {
				fmt.Println("✓ API Key: 已配置")
			} else {
				fmt.Println("✗ API Key: 未配置 (设置 ANTHROPIC_API_KEY 或 --api-key)")
			}

			// 检查工具依赖
			checkCommand("rg", "ripgrep (Grep 工具)")
			checkCommand("git", "Git")
			checkCommand("sh", "Shell")

			// 检查记忆文件
			cwd, _ := os.Getwd()
			loader := prompt.NewManager(cwd)
			files := loader.MemoryLoader.LoadAll()
			fmt.Printf("✓ CLAUDE.md 文件: 发现 %d 个\n", len(files))

			// 角色表与工作流自检 (21.8.12 W1/W3/W4)。
			//
			// doctor 是"这台机器上的东西对不对"的入口, 而这三项恰好都是**静默**
			// 失效的: 角色来源少扫一档 (只看 cwd 不看 stateDir)、落盘工作流的 agent
			// 名不在表里 (退化成内联 prompt)、内置工作流的名字写错。三者都不会报错,
			// 只会在某次运行里给出一个不属于你的结果。
			stateDir := ""
			if jsonCfg, _, err := loadRuntimeJSONConfig(); err == nil && jsonCfg != nil {
				stateDir = basedir.ResolveDefault(jsonCfg.StateDir, jsonCfg.Cwd)
			}
			if stateDir == "" {
				stateDir = resolveStateDir("")
			}
			roles := agent.NewRoleRegistryFor(cwd, stateDir)
			fmt.Printf("✓ 角色表: %s\n", roles.DescribeSources())
			for _, issue := range roles.LoadIssues() {
				fmt.Printf("  ✗ %s\n", issue)
			}

			// doctor **不注册**工作流 —— 装载会写进程内的注册表, 而 doctor 的诊断
			// 动作不该有副作用 (那正是"验收脚本不许改被测对象")。所以这里走的是
			// **只读**自检 inspectWorkflowDir, 而不是会注册的 LoadWorkflowsFromDirReport。
			wfDir := ""
			if stateDir != "" {
				wfDir = filepath.Join(stateDir, "workflows")
			}
			if inspected, hard, degraded := inspectWorkflowDir(wfDir, roles); inspected > 0 {
				fmt.Printf("✓ 落盘工作流自检: 共 %d 条, 硬错 %d, 静默降级 %d\n", inspected, hard, degraded)
			}
			if issues := agent.AuditBuiltinWorkflows(roles); len(issues) > 0 {
				for _, is := range issues {
					if is.Hard != "" {
						fmt.Printf("  ✗ 内置 %s: %s\n", is.Name, is.Hard)
						continue
					}
					for _, w := range is.Warnings {
						fmt.Printf("  ! 内置 %s: %s\n", is.Name, w)
					}
				}
				// unchecked 必须报: 一部分内置的 stage 在图模板里, WorkflowDef.Stages
				// 为空 —— 它们一个 stage 都查不到。不报这个数的话, "全部查过且全绿"与
				// "一大半根本没查"在 doctor 的输出上完全一样, 而 doctor 的全部价值
				// 就是让人相信"内置与角色表没脱节"。
				total, hard, degraded, unchecked := agent.BuiltinAuditSummary(issues)
				fmt.Printf("✓ 内置工作流自检: 共 %d 条, 硬错 %d, 静默降级 %d, stage 不在定义中(无从检查) %d\n",
					total, hard, degraded, unchecked)
			}
		},
	}
}

// inspectWorkflowDir 只读地自检一个工作流目录 (21.8.12 W4 的落盘半边)。
//
// **不注册、不落盘、不改任何全局状态** —— doctor 是诊断入口, 而诊断动作若带副作用,
// 它报出来的结论就不再是"你现在的状态", 而是"我检查过之后的状态"。这条纪律在
// 21.8.12 的验收方法论里写作"验收脚本不许改被测对象"。
//
// 与 LoadWorkflowsFromDirReport 的关系: 那个会注册, 是本函数的写版本。两份都保留
// 是因为它们的**时机**不同 —— 启动装载必须注册, doctor 必须不注册; 而校验收敛在
// ValidateAll 一处, 所以两份给出的判定必然一致 (不会出现"doctor 说好、启动说坏")。
func inspectWorkflowDir(dir string, roles *agent.RoleRegistry) (inspected, hard, degraded int) {
	if strings.TrimSpace(dir) == "" {
		return 0, 0, 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, 0
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var def agent.WorkflowDef
		if err := json.Unmarshal(data, &def); err != nil {
			continue
		}
		inspected++
		warns, verr := def.ValidateAll(roles)
		if verr != nil {
			hard++
			fmt.Printf("  ✗ 工作流 %s: %s\n", e.Name(), verr)
			continue
		}
		for _, w := range warns {
			degraded++
			fmt.Printf("  ! 工作流 %s: %s\n", e.Name(), w)
		}
	}
	return inspected, hard, degraded
}

// toolsCmd 列出可用工具
func toolsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "tools",
		Short:   "列出所有可用工具",
		Example: `  claude-go tools`,
		Run: func(cmd *cobra.Command, args []string) {
			reg := tool.NewRegistry()
			builtin.RegisterBaseTools(reg, nil)

			fmt.Println("Available Tools:")
			fmt.Println("================")
			for _, t := range reg.All() {
				readOnly := ""
				if t.IsReadOnly(nil) {
					readOnly = " [readonly]"
				}
				fmt.Printf("  %-15s %s%s\n", t.Name(), firstLine(t.Description()), readOnly)
			}
		},
	}
}

func skillsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "skills [name]",
		Short: "查看已加载技能或某个技能详情",
		Example: `  claude-go skills
  claude-go skills golang-patterns`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			reg := skills.NewRegistry()
			reg.LoadDefaults(cwd)

			if len(args) == 0 {
				allSkills := reg.All()
				if len(allSkills) == 0 {
					fmt.Println("No skills loaded.")
					return nil
				}
				fmt.Println("Loaded Skills:")
				fmt.Println("==============")
				for _, s := range allSkills {
					fmt.Printf("- %-24s %s [%s]\n", s.Name, firstLine(s.Description), s.LoadedFrom)
				}
				return nil
			}

			skill, ok := reg.Get(args[0])
			if !ok {
				return fmt.Errorf("技能 %q 未找到", args[0])
			}
			fmt.Printf("Name: %s\n", skill.Name)
			fmt.Printf("Description: %s\n", skill.Description)
			fmt.Printf("When to use: %s\n", skill.WhenToUse)
			fmt.Printf("Loaded from: %s\n", skill.LoadedFrom)
			if skill.SourcePath != "" {
				fmt.Printf("Source path: %s\n", skill.SourcePath)
			}
			fmt.Println()
			fmt.Println("---")
			fmt.Println()
			fmt.Println(skill.Body)
			return nil
		},
	}
}

func rolesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "roles [name]",
		Short: "查看角色以及某个角色最终绑定的技能",
		Example: `  claude-go roles
  claude-go roles coder
  claude-go roles go-reviewer`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			// 来源与 run/飞书/dashboard 一致 (21.8.12 W1): 这个子命令的用途就是
			// "查得到底装了哪些角色", 它列出来的表若与服务时用的那张不同, 它给出的
			// 就是一份会误导排查的答案 —— 而它恰恰是排查时第一个被跑的命令。
			//
			// stateDir 从**运行时配置**里取, 而不是直接 resolveStateDir(""):
			// 后者只看 CLI 旗标与 cwd 回落, 会漏掉配置文件里写的 stateDir ——
			// 而那恰是生产上这份部署真正的状态目录 (见 loadRuntimeJSONConfig)。
			stateDir := ""
			if jsonCfg, _, err := loadRuntimeJSONConfig(); err == nil && jsonCfg != nil {
				stateDir = basedir.ResolveDefault(jsonCfg.StateDir, jsonCfg.Cwd)
			}
			if stateDir == "" {
				stateDir = resolveStateDir("")
			}
			reg := agent.NewRoleRegistryFor(cwd, stateDir)
			fmt.Println(reg.DescribeSources())

			if len(args) == 0 {
				fmt.Println("Available Roles:")
				fmt.Println("================")
				for _, role := range reg.ListByCategory("") {
					resolved := reg.ResolveRoleName(role.Name)
					suffix := ""
					if resolved != role.Name {
						suffix = fmt.Sprintf(" -> %s", resolved)
					}
					fmt.Printf("- %-24s %s%s\n", role.Name, firstLine(role.Description), suffix)
				}
				return nil
			}

			info := reg.DescribeRole(args[0])
			if info == nil {
				return fmt.Errorf("角色 %q 未找到", args[0])
			}
			fmt.Printf("Requested role: %s\n", info.Requested)
			fmt.Printf("Resolved role: %s\n", info.Resolved)
			fmt.Printf("Description: %s\n", info.Description)
			if len(info.Tags) > 0 {
				fmt.Printf("Tags: %s\n", strings.Join(info.Tags, ", "))
			}
			fmt.Printf("File skills: %s\n", formatNameList(info.FileSkills))
			fmt.Printf("Builtin skills: %s\n", formatNameList(info.BuiltinSkills))
			fmt.Printf("Recommended skills: %s\n", formatNameList(info.RecommendedSkills))
			fmt.Printf("Injected skills: %s\n", formatNameList(info.InjectedSkills))
			fmt.Printf("Estimated injected skill chars: %d\n", info.InjectedSkillChars)
			return nil
		},
	}
}

// dashboardCmd 拉起本地只读可视化 dashboard (支持 run/start/stop/status/open)。
// evoCmd 进化操作台 (design/03 §4.7): 检视学习闭环健康度 + shadow 技能审计晋升。
// 子命令: status(默认健康度检视) / audit(技能进化门禁裁决) / promote|rollback(手动改状态)。
// 治理护栏: 审计判据/阈值锁定为常量 (design/03 §4.6 四律, 不可经 CLI 改)。
func evoCmd() *cobra.Command {
	var (
		stateDirFlag string
		jsonOut      bool
	)
	resolveStateDir := func() string {
		if stateDirFlag != "" {
			return stateDirFlag
		}
		jsonCfg, _ := feishu.LoadJSONConfig(flagConfig)
		cwd, _ := os.Getwd()
		sdInput := ""
		if jsonCfg != nil {
			sdInput = jsonCfg.StateDir
		}
		return basedir.ResolveDefault(sdInput, cwd)
	}

	cmd := &cobra.Command{
		Use:   "evo",
		Short: "进化操作台: 学习闭环健康度 + 技能进化门禁 (design/03 §4.7)",
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := console.Inspect(resolveStateDir())
			if err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Println(string(b))
			} else {
				fmt.Print(rep.Format())
			}
			return nil
		},
	}
	cmd.PersistentFlags().StringVar(&stateDirFlag, "state-dir", "", "状态目录 (默认从 config/cwd 解析)")
	cmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "JSON 输出")

	// evo audit: 依据 rewards.jsonl 裁决 shadow 技能晋升/退役 (design/03 §4.3c)
	var apply bool
	var improveModel string
	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "技能进化门禁: 依据奖励证据裁决 shadow 技能晋升/退役 (--apply 生效, 默认 dry-run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			sd := resolveStateDir()
			// 13.8.6 P1: 传 trace log 目录启用配对归因 (policy_decision 留痕);
			// 目录不存在时 readPolicySkills 返回空映射 → 自动回退时序判据。
			res, err := skillaudit.AuditWithTraces(
				filepath.Join(sd, "skills"), filepath.Join(sd, "evolution", "rewards.jsonl"),
				filepath.Join(sd, "statestore", "log"), apply)
			if err != nil {
				return err
			}
			mode := "dry-run (加 --apply 生效)"
			if apply {
				mode = "已应用"
			}
			fmt.Printf("技能进化门禁 [%s]\n  评估 shadow 技能: %d\n  晋升: %v\n  退役: %v\n  保持: %v\n",
				mode, res.Evaluated, res.Promoted, res.Retired, res.Held)
			// 不越权闸拦下的必须单独显示 (design/03 §4.6): "分数够了但权限面越界"与
			// "分数不够先等等"要人做的事完全不同, 混进 Held 会让越权被当成还没攒够样本。
			if len(res.Rejected) > 0 {
				fmt.Printf("  不越权闸拒绝: %v\n", res.Rejected)
				for name, reasons := range res.RejectReasons {
					fmt.Printf("    · %s: %s\n", name, strings.Join(reasons, "; "))
				}
			}
			// 第七章缺口修复: ImproveSkill 接线——审计判低 (退役/拒绝) 的技能
			// 交 LLM 反思改写, 改进产物仍留 shadow 态 (preserveStatus 不越门禁)。
			// 需 --apply 且 --improve-model 显式给出模型别名 (改进是 LLM 调用,
			// 不能在 dry-run 或未知模型下静默发生)。
			if apply && improveModel != "" {
				targets := append(append([]string{}, res.Retired...), res.Rejected...)
				if len(targets) > 0 {
					if ac := buildImproveCreator(sd, improveModel); ac != nil {
						for _, name := range targets {
							reason := "审计裁决: " + name
							if rs, ok := res.RejectReasons[name]; ok {
								reason = strings.Join(rs, "; ")
							}
							if err := ac.ImproveSkill(cmd.Context(), name, reason, false); err != nil {
								fmt.Printf("    改进失败 %s: %v\n", name, err)
							} else {
								fmt.Printf("    已触发改进 %s (仍留 shadow, 待复审)\n", name)
							}
						}
					} else {
						fmt.Println("  ⚠ 改进器构造失败 (模型别名/registry), 跳过 ImproveSkill")
					}
				}
			}
			return nil
		},
	}
	auditCmd.Flags().BoolVar(&apply, "apply", false, "真正改写 SKILL.md 状态 (默认 dry-run)")
	auditCmd.Flags().StringVar(&improveModel, "improve-model", "", "审计判低后触发 ImproveSkill 反思改写的模型别名 (如 ollama:gemma4:26b-a4b-it-qat)")

	// evo promote/rollback: 手动改技能状态 (留痕)
	promoteCmd := &cobra.Command{
		Use:   "promote <skill-name>",
		Short: "手动晋升技能到 active (design/03 §4.6 可回滚)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := filepath.Join(resolveStateDir(), "skills", args[0], "SKILL.md")
			if err := skillaudit.SetStatus(p, "active"); err != nil {
				return err
			}
			fmt.Printf("✅ 技能 %s 已晋升 active\n", args[0])
			return nil
		},
	}
	rollbackCmd := &cobra.Command{
		Use:   "rollback <skill-name>",
		Short: "回退技能到 shadow (design/03 §4.6 一键回滚)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := filepath.Join(resolveStateDir(), "skills", args[0], "SKILL.md")
			if err := skillaudit.SetStatus(p, "shadow"); err != nil {
				return err
			}
			fmt.Printf("↩️ 技能 %s 已回退 shadow\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(auditCmd, promoteCmd, rollbackCmd,
		evoReplayCmd(resolveStateDir), evoGepaStepCmd(resolveStateDir),
		evoGepaLoopCmd(resolveStateDir), // 方案三 L5: 回放打分 + GEPA 完整版单步/多轮
		evoDistillCmd())                 // 方案三数据通道: T1 示例/T5 分布/T6 修复模式挖掘
	return cmd
}

// workerCmd 分布式 worker (design/02 §3.3 L3): 连接控制面, 心跳注册, 拉取任务执行回报。
// v1 执行 "stage" 类任务: payload 为图节点描述, 经本地 agent 运行时执行 (无 LLM 凭证时
// 回报占位结果, 证明任务分发链路)。真实分布式执行需 worker 侧完整引擎装配 (R3 后续)。
func workerCmd() *cobra.Command {
	var (
		control    string
		workerName string
		caps       []string
		pollMs     int
	)
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "分布式 worker: 连接控制面拉取任务执行 (design/02 §3.3)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if control == "" {
				control = os.Getenv("CLAUDE_GO_CONTROL")
			}
			if control == "" {
				return fmt.Errorf("需要控制面地址: --control 或 CLAUDE_GO_CONTROL")
			}
			if workerName == "" {
				workerName = os.Getenv("POD_NAME")
			}
			if workerName == "" {
				host, _ := os.Hostname()
				workerName = "worker-" + host
			}
			if pollMs <= 0 {
				pollMs = 1000
			}
			client := cluster.NewClient(control, workerName)
			kinds := []string{"stage"}
			log.Printf("[worker] %s 连接控制面 %s, caps=%v", workerName, control, caps)

			ctx := cmd.Context()
			// 心跳 goroutine (每 30s)
			go func() {
				t := time.NewTicker(30 * time.Second)
				defer t.Stop()
				_ = client.Heartbeat(caps, kinds, nil) // 立即注册一次
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						if err := client.Heartbeat(caps, kinds, nil); err != nil {
							log.Printf("[worker] 心跳失败: %v", err)
						}
					}
				}
			}()

			// 拉取-执行循环
			poll := time.Duration(pollMs) * time.Millisecond
			var done int
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				task, err := client.PullWithCaps(kinds, caps)
				if err != nil {
					log.Printf("[worker] 拉取失败: %v", err)
					time.Sleep(poll)
					continue
				}
				if task == nil {
					time.Sleep(poll)
					continue
				}
				log.Printf("[worker] 执行任务 %s (kind=%s run=%s node=%s)", task.ID, task.Kind, task.RunID, task.NodeID)
				result, execErr := executeWorkerTask(task, workerName)
				if execErr != nil {
					_ = client.Fail(task.ID, execErr.Error())
					log.Printf("[worker] 任务 %s 失败: %v", task.ID, execErr)
				} else {
					_ = client.Complete(task.ID, result)
					done++
					log.Printf("[worker] 任务 %s 完成 (累计 %d)", task.ID, done)
				}
			}
		},
	}
	cmd.Flags().StringVar(&control, "control", "", "控制面基址 (如 http://claude-go-control:18080)")
	cmd.Flags().StringVar(&workerName, "name", "", "worker 名 (默认 POD_NAME 或主机名)")
	cmd.Flags().StringSliceVar(&caps, "caps", nil, "能力标签 (bash/browser/k8s-sandbox)")
	cmd.Flags().IntVar(&pollMs, "poll-ms", 1000, "无任务时轮询间隔 (毫秒)")
	return cmd
}

// executeWorkerTask 执行一个 stage 任务。v1 简化实现: 回显 payload + worker 标识,
// 证明控制面→worker→控制面的任务分发链路。R3 后续接完整引擎做真实 agent 执行。
func executeWorkerTask(task *cluster.Task, worker string) (json.RawMessage, error) {
	out := map[string]any{
		"worker":  worker,
		"task_id": task.ID,
		"run_id":  task.RunID,
		"node_id": task.NodeID,
		"echo":    json.RawMessage(task.Payload),
		"status":  "completed",
	}
	return json.Marshal(out)
}

// llmGatewayCmd LLM 网关独立进程 (design/02 §3.1 R1, L1 层)。
// 反向代理式: 按 model 路由 provider、注入鉴权、token 双边记账、access.jsonl。
// 多副本/多平台共享同一网关 = 共享配额观测。
func llmGatewayCmd() *cobra.Command {
	var (
		gwAddr       string
		gwConfigPath string
	)
	cmd := &cobra.Command{
		Use:   "llm-gateway",
		Short: "LLM 网关独立进程 (provider 路由/鉴权注入/token 双边记账)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if gwConfigPath == "" {
				gwConfigPath = flagConfig
			}
			botCfg := feishu.DefaultBotConfig()
			jsonCfg, err := feishu.LoadJSONConfig(gwConfigPath)
			if err != nil {
				return fmt.Errorf("加载配置失败: %w", err)
			}
			if jsonCfg != nil {
				jsonCfg.ApplyToBot(botCfg)
			}
			if len(botCfg.Providers) == 0 {
				return fmt.Errorf("配置中无 providers, 网关无路由可用 (--config 指定含 providers 的配置)")
			}
			// 抽成闭包: 启动与热加载共用同一段构建逻辑, 免得两处漂移
			buildRoutes := func(cfg *feishu.BotConfig) ([]llmgw.Route, string) {
				var routes []llmgw.Route
				for name, p := range cfg.Providers {
					// 同一 provider 下按 proxy 分组: 配置了 proxy 的模型 (如 muse-spark)
					// 拆成独立 route, 其余保持直连组 —— 保证"没配置代理的模型绝不走代理"。
					type grp struct {
						rt  *llmgw.Route
						key string
					}
					var groups []grp
					for alias, mc := range p.Models {
						var found *llmgw.Route
						for i := range groups {
							if groups[i].key == mc.Proxy {
								found = groups[i].rt
								break
							}
						}
						if found == nil {
							rt := llmgw.Route{Provider: name, BaseURL: p.BaseURL, APIKey: p.APIKey, Proxy: mc.Proxy}
							// 环境变量覆盖 key (K8s Secret 注入形态): <PROVIDER>_API_KEY
							if env := os.Getenv(strings.ToUpper(name) + "_API_KEY"); env != "" {
								rt.APIKey = env
							}
							groups = append(groups, grp{rt: &rt, key: mc.Proxy})
							found = &rt
						}
						// 别名 "provider:model" → 裸模型名入路由表
						if i := strings.IndexByte(alias, ':'); i >= 0 {
							found.Models = append(found.Models, alias[i+1:])
						} else {
							found.Models = append(found.Models, alias)
						}
					}
					for _, g := range groups {
						routes = append(routes, *g.rt)
					}
				}
				dp := ""
				if i := strings.IndexByte(cfg.ModelAlias, ':'); i > 0 {
					dp = cfg.ModelAlias[:i]
				}
				return routes, dp
			}
			routes, defaultProvider := buildRoutes(botCfg)
			gwStateDir := basedir.ResolveDefault("", ".")
			if jsonCfg != nil && jsonCfg.StateDir != "" {
				gwStateDir = jsonCfg.StateDir
			}
			srv, err := llmgw.NewServer(routes, defaultProvider, filepath.Join(gwStateDir, "llm-gateway"))
			if err != nil {
				return err
			}
			// 配置热加载: providers / models / proxy / apiKey 改了原地换路由表。
			// 用 SetRoutes 而不是重建 Server —— 重建会丢掉已建的直连与代理连接池。
			{
				cfgPath, _ := feishu.ResolveJSONConfigPath(gwConfigPath)
				hotreload.WatchConfig(cfgPath, 5*time.Second, "llm-gateway", func(path string) error {
					c, e := feishu.LoadJSONConfig(path)
					if e != nil || c == nil {
						return fmt.Errorf("重新加载失败: %v", e)
					}
					nc := feishu.DefaultBotConfig()
					c.ApplyToBot(nc)
					nr, ndp := buildRoutes(nc)
					return srv.SetRoutes(nr, ndp)
				})
			}
			fmt.Printf("[llm-gateway] 监听 %s | providers=%d 默认=%s\n", gwAddr, len(routes), defaultProvider)
			return http.ListenAndServe(gwAddr, srv.Handler())
		},
	}
	cmd.Flags().StringVar(&gwAddr, "addr", "127.0.0.1:18081", "监听地址")
	cmd.Flags().StringVar(&gwConfigPath, "config", "", "JSON 配置文件 (含 providers)")
	return cmd
}

func dashboardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "本地 Web Dashboard (只读可视化)",
		Long: `本地 Dashboard 聚合展示 claude-go 的运行状态与质量数据:

  • 团队运行 (workflow / stages / 对抗循环 Eval / 时间线 / 产出)
  • 持续观测指标 (team / evolution / dreaming / memory / task, 含趋势与 CSV 导出)
  • Cron 定时任务 (最近执行、成功率)
  • Dreaming 记忆整理 (压缩率、dream 日志、topic treemap)
  • Evolution 进化机制 (经验库、轨迹、质量分布、热力图)
  • 智能诊断 (本地规则, 不经 LLM)
  • 多项目切换 (扫描父级或自定义 root)

默认仅监听 127.0.0.1, 不暴露到外网。所有数据均来自本地 .claude-go/ 目录,
与 feishu bot 共用同一份数据 (通过 basedir.ResolveDefault(stateDir, cwd) 解析)。

Dashboard 写入范围有限且只在自己子目录:
  • .claude-go/backups/            (用户触发的备份归档)
  • .claude-go/.dashboard/actions  (UI 动作队列, 由 bot 消费)
  • .claude-go/.dashboard/triggers (LLM 异步诊断任务)

子命令:
  run       前台运行 (默认, 按 Ctrl+C 退出)
  start     后台启动 (daemon), pid 记录在 .claude-go/.dashboard/dashboard.pid
  stop      停止后台进程
  status    查看后台进程状态
  open      打开浏览器访问当前运行的 Dashboard`,
		Example: `  claude-go dashboard             # 前台运行 (默认)
  claude-go dashboard start       # 后台启动
  claude-go dashboard status      # 查看状态
  claude-go dashboard stop        # 停止
  claude-go dashboard open        # 浏览器打开`,
	}
	cmd.AddCommand(dashboardRunCmd())
	cmd.AddCommand(dashboardStartCmd())
	cmd.AddCommand(dashboardStopCmd())
	cmd.AddCommand(dashboardStatusCmd())
	cmd.AddCommand(dashboardOpenCmd())

	// 兼容旧行为: 无子命令时等价于 run (保留 flags 传参)
	var (
		addr       string
		port       int
		stateDir   string
		noOpen     bool
		configPath string
	)
	cmd.Flags().StringVar(&addr, "addr", "", "监听地址 (如 127.0.0.1:7777), 与 --port 二选一")
	cmd.Flags().IntVar(&port, "port", 7777, "监听端口 (默认绑定 127.0.0.1)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认从 --config 的 stateDir/cwd 推导, 兜底 <CWD>/.claude-go)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "不自动打开浏览器")
	cmd.Flags().StringVar(&configPath, "config", "", "JSON 配置文件路径 (与 feishu bot 共用同一 stateDir/cwd 解析规则)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runDashboardForeground(addr, port, stateDir, noOpen, configPath)
	}
	return cmd
}

func dashboardRunCmd() *cobra.Command {
	var (
		addr       string
		port       int
		stateDir   string
		noOpen     bool
		configPath string
	)
	c := &cobra.Command{
		Use:   "run",
		Short: "前台运行 Dashboard (Ctrl+C 退出)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDashboardForeground(addr, port, stateDir, noOpen, configPath)
		},
	}
	c.Flags().StringVar(&addr, "addr", "", "监听地址 (如 127.0.0.1:7777)")
	c.Flags().IntVar(&port, "port", 7777, "监听端口 (自动顺延至可用)")
	c.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认从 --config 的 stateDir/cwd 推导, 兜底 <CWD>/.claude-go)")
	c.Flags().BoolVar(&noOpen, "no-open", false, "不自动打开浏览器")
	c.Flags().StringVar(&configPath, "config", "", "JSON 配置文件路径 (与 feishu bot 共用同一 stateDir/cwd 解析规则)")
	return c
}

func dashboardStartCmd() *cobra.Command {
	var (
		addr       string
		port       int
		stateDir   string
		noOpen     bool
		configPath string
	)
	c := &cobra.Command{
		Use:   "start",
		Short: "后台启动 Dashboard (daemon)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 与 feishu bot 完全一致的 stateDir 解析, 保证 daemon 和前台 run 用同一份数据目录。
			info, err := resolveDashStateDir(stateDir, configPath)
			if err != nil {
				return err
			}
			printDashStateDirInfo(info, "Dashboard/start")
			resolved := info.Resolved
			// 已在跑则拒绝
			if st, _ := dashboard.QueryStatus(resolved); st.Running {
				fmt.Printf("已在运行: pid=%d %s\n", st.PID, st.URL)
				return nil
			}
			freePort := dashboard.FindFreePort(port)
			bindAddr := addr
			if bindAddr == "" {
				bindAddr = fmt.Sprintf("127.0.0.1:%d", freePort)
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			logPath := dashboard.LogFilePath(resolved)
			logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return fmt.Errorf("打开日志: %w", err)
			}
			defer logFile.Close()

			// 启动子进程: claude-go dashboard run --addr ... --state-dir ... --no-open
			args2 := []string{"dashboard", "run",
				"--addr", bindAddr,
				"--state-dir", resolved,
				"--no-open",
			}
			if configPath != "" {
				absConfig, _ := filepath.Abs(configPath)
				args2 = append(args2, "--config", absConfig)
			}
			subCmd := exec.Command(exe, args2...)
			subCmd.Stdout = logFile
			subCmd.Stderr = logFile
			subCmd.Stdin = nil
			// detach: 新 session, 让父退出后子进程继续运行
			subCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			subCmd.Env = append(os.Environ(), "CLAUDE_GO_DASHBOARD_DAEMON=1")
			if err := subCmd.Start(); err != nil {
				return fmt.Errorf("启动失败: %w", err)
			}
			// 写 pid 文件
			_ = dashboard.WritePIDFile(resolved, dashboard.PIDFile{
				PID:       subCmd.Process.Pid,
				Port:      freePort,
				Addr:      bindAddr,
				StartedAt: time.Now(),
				StateDir:  resolved,
				LogFile:   logPath,
			})
			// 等待子进程就绪 (轮询 /api/health, 最长 5s)
			healthURL := fmt.Sprintf("http://%s/api/health", bindAddr)
			ready := false
			for i := 0; i < 50; i++ {
				time.Sleep(100 * time.Millisecond)
				resp, err := http.Get(healthURL)
				if err == nil && resp.StatusCode == http.StatusOK {
					resp.Body.Close()
					ready = true
					break
				}
				if err == nil {
					resp.Body.Close()
				}
			}
			if !ready {
				// 子进程可能崩溃, 清理 pid 文件
				dashboard.RemovePIDFile(resolved)
				return fmt.Errorf("Dashboard 启动超时, 子进程未就绪 (PID=%d), 请查看日志: %s", subCmd.Process.Pid, logPath)
			}
			fmt.Printf("\n🚀 Dashboard 已后台启动\n")
			fmt.Printf("   URL      : http://%s\n", bindAddr)
			fmt.Printf("   PID      : %d\n", subCmd.Process.Pid)
			fmt.Printf("   StateDir : %s\n", resolved)
			fmt.Printf("   Log      : %s\n", logPath)
			fmt.Printf("   停止     : claude-go dashboard stop\n\n")
			if !noOpen {
				_ = openBrowser("http://" + bindAddr)
			}
			return nil
		},
	}
	c.Flags().StringVar(&addr, "addr", "", "监听地址 (如 0.0.0.0:7777), 与 --port 二选一")
	c.Flags().IntVar(&port, "port", 7777, "期望端口 (占用时自动顺延)")
	c.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认从 --config 的 stateDir/cwd 推导, 兜底 <CWD>/.claude-go)")
	c.Flags().BoolVar(&noOpen, "no-open", false, "不自动打开浏览器")
	c.Flags().StringVar(&configPath, "config", "", "JSON 配置文件路径 (与 bot/run 共用同一 stateDir 解析规则)")
	return c
}

func dashboardStopCmd() *cobra.Command {
	var (
		stateDir   string
		configPath string
	)
	c := &cobra.Command{
		Use:   "stop",
		Short: "停止后台 Dashboard 进程",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 必须与 start 用同一份解析逻辑, 否则 pid 文件找不到会导致 "残留" 进程无法清理。
			info, err := resolveDashStateDir(stateDir, configPath)
			if err != nil {
				return err
			}
			resolved := info.Resolved
			st, err := dashboard.StopDaemon(resolved)
			if err != nil {
				return err
			}
			if st.PID == 0 {
				fmt.Println("没有运行中的 Dashboard 后台进程")
				return nil
			}
			fmt.Printf("已停止 Dashboard (pid=%d)\n", st.PID)
			return nil
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认从 --config 的 stateDir/cwd 推导, 兜底 <CWD>/.claude-go)")
	c.Flags().StringVar(&configPath, "config", "", "JSON 配置文件路径 (与 bot/run/start 共用同一 stateDir 解析规则)")
	return c
}

func dashboardStatusCmd() *cobra.Command {
	var (
		stateDir   string
		configPath string
		asJSON     bool
	)
	c := &cobra.Command{
		Use:   "status",
		Short: "查看后台 Dashboard 状态",
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := resolveDashStateDir(stateDir, configPath)
			if err != nil {
				return err
			}
			resolved := info.Resolved
			st, _ := dashboard.QueryStatus(resolved)
			if asJSON {
				b, _ := json.MarshalIndent(st, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if !st.Running {
				fmt.Println("状态      : 未运行")
				if st.PID > 0 {
					fmt.Printf("残留 PID  : %d (可能已崩溃, 使用 stop 清理)\n", st.PID)
				}
				fmt.Printf("StateDir  : %s\n", resolved)
				return nil
			}
			fmt.Println("状态      : 运行中")
			fmt.Printf("PID       : %d\n", st.PID)
			fmt.Printf("URL       : %s\n", st.URL)
			fmt.Printf("Uptime    : %s\n", st.Uptime)
			fmt.Printf("StateDir  : %s\n", st.StateDir)
			if st.LogFile != "" {
				fmt.Printf("Log       : %s\n", st.LogFile)
			}
			return nil
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认从 --config 的 stateDir/cwd 推导, 兜底 <CWD>/.claude-go)")
	c.Flags().StringVar(&configPath, "config", "", "JSON 配置文件路径 (与 bot/run/start 共用同一 stateDir 解析规则)")
	c.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出")
	return c
}

func dashboardOpenCmd() *cobra.Command {
	var (
		stateDir   string
		configPath string
	)
	c := &cobra.Command{
		Use:   "open",
		Short: "在浏览器打开当前运行的 Dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := resolveDashStateDir(stateDir, configPath)
			if err != nil {
				return err
			}
			resolved := info.Resolved
			st, _ := dashboard.QueryStatus(resolved)
			if !st.Running {
				return fmt.Errorf("没有运行中的 Dashboard, 请先执行: claude-go dashboard start")
			}
			return openBrowser(st.URL)
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认从 --config 的 stateDir/cwd 推导, 兜底 <CWD>/.claude-go)")
	c.Flags().StringVar(&configPath, "config", "", "JSON 配置文件路径 (与 bot/run/start 共用同一 stateDir 解析规则)")
	return c
}

// backupCmd 备份 / 恢复 .claude-go 运行态关键数据。
//
// 覆盖: teams/, memory/, metrics/, evolution/, swarm_intel/,
//
//	tasks.json, cron_jobs.json, blackboard.json, config.json 等
//
// 归档路径: <stateDir>/backups/<timestamp>-<label>.tar.gz
func backupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "备份与恢复 .claude-go 运行态 (团队/记忆/指标/进化/群体智能)",
		Long: `claude-go backup 是灾难恢复 & 跨机迁移通道:

  create   把关键数据 tar.gz 归档到 .claude-go/backups/
  list     列出已有归档 (时间/大小/label)
  restore  从归档恢复 (默认先产生一个 safety 快照, 再覆盖)
  delete   删除指定归档

设计原则:
  • 纯本地, 无网络依赖
  • 归档可直接拷贝到另一台机器再 restore
  • restore 前默认会自动 safety-before-restore 备份一份当前现场`,
		Example: `  # 创建一次带标签的快照
  claude-go backup create --label weekly-snapshot

  # 只列出
  claude-go backup list

  # 从指定归档恢复 (路径必须在 .claude-go/backups/ 下)
  claude-go backup restore --path .claude-go/backups/20260418-120000-weekly-snapshot.tar.gz

  # 删除归档
  claude-go backup delete --path .claude-go/backups/xxx.tar.gz`,
	}
	cmd.AddCommand(backupCreateCmd())
	cmd.AddCommand(backupListCmd())
	cmd.AddCommand(backupRestoreCmd())
	cmd.AddCommand(backupDeleteCmd())
	return cmd
}

func resolveStateDir(stateDir string) string {
	cwd, _ := os.Getwd()
	return basedir.ResolveDefault(stateDir, cwd)
}

func backupCreateCmd() *cobra.Command {
	var (
		stateDir       string
		label          string
		outPath        string
		includeReports bool
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "创建一次备份归档",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := runBackupCreate(resolveStateDir(stateDir), label, outPath, includeReports)
			if err != nil {
				return err
			}
			fmt.Printf("\n✅ 已创建备份\n")
			fmt.Printf("   归档     : %s\n", res.Path)
			fmt.Printf("   文件数   : %d\n", res.FileCount)
			fmt.Printf("   大小     : %.2f MB\n", float64(res.Size)/1024.0/1024.0)
			fmt.Printf("   标签     : %s\n", res.Label)
			fmt.Printf("   子系统   : %s\n", strings.Join(res.Targets, ", "))
			return nil
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认 .claude-go)")
	c.Flags().StringVar(&label, "label", "manual", "标签 (只保留 [-_a-zA-Z0-9])")
	c.Flags().StringVar(&outPath, "out", "", "自定义输出路径 (默认 stateDir/backups/<ts>-<label>.tar.gz)")
	c.Flags().BoolVar(&includeReports, "include-reports", true, "是否包含 team/*/REPORT.md")
	return c
}

func backupListCmd() *cobra.Command {
	var stateDir string
	var asJSON bool
	c := &cobra.Command{
		Use:   "list",
		Short: "列出所有备份归档",
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := runBackupList(resolveStateDir(stateDir))
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(list, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(list) == 0 {
				fmt.Println("(暂无备份)")
				return nil
			}
			fmt.Printf("%-30s %-14s %-10s %s\n", "TIME", "SIZE(MB)", "LABEL", "PATH")
			for _, e := range list {
				fmt.Printf("%-30s %-14.2f %-10s %s\n",
					e.CreatedAt.Format(time.RFC3339),
					float64(e.Size)/1024.0/1024.0,
					e.Label,
					e.Path)
			}
			return nil
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认 .claude-go)")
	c.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出")
	return c
}

func backupRestoreCmd() *cobra.Command {
	var (
		stateDir    string
		path        string
		skipSafety  bool
		noOverwrite bool
	)
	c := &cobra.Command{
		Use:   "restore",
		Short: "从归档恢复 (默认先 safety 快照再覆盖)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				return fmt.Errorf("--path 必填 (一般位于 .claude-go/backups/)")
			}
			res, err := runBackupRestore(resolveStateDir(stateDir), path, skipSafety, !noOverwrite)
			if err != nil {
				return err
			}
			fmt.Printf("\n✅ 已恢复\n")
			fmt.Printf("   文件数   : %d\n", res.RestoredFiles)
			if res.SafetyBackup != "" {
				fmt.Printf("   安全快照 : %s\n", res.SafetyBackup)
			}
			if res.Manifest != nil {
				fmt.Printf("   源快照   : %s (label=%s)\n",
					res.Manifest.CreatedAt.Format(time.RFC3339),
					res.Manifest.Label)
			}
			return nil
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认 .claude-go)")
	c.Flags().StringVar(&path, "path", "", "归档路径 (.tar.gz)")
	c.Flags().BoolVar(&skipSafety, "skip-safety-backup", false, "恢复前跳过 safety 快照 (不推荐)")
	c.Flags().BoolVar(&noOverwrite, "no-overwrite", false, "冲突时保留现有文件 (默认覆盖)")
	return c
}

func backupDeleteCmd() *cobra.Command {
	var stateDir, path string
	c := &cobra.Command{
		Use:   "delete",
		Short: "删除指定备份归档",
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				return fmt.Errorf("--path 必填")
			}
			return runBackupDelete(resolveStateDir(stateDir), path)
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "数据根目录 (默认 .claude-go)")
	c.Flags().StringVar(&path, "path", "", "归档路径")
	return c
}

// ---- backup adapters (使用 pkg/backup) ----

type backupCreateSummary struct {
	Path      string
	Size      int64
	FileCount int
	Label     string
	Targets   []string
}

func runBackupCreate(stateDir, label, out string, includeReports bool) (*backupCreateSummary, error) {
	res, err := backup.Create(backup.CreateOptions{
		StateDir:       stateDir,
		OutPath:        out,
		Label:          label,
		IncludeReports: includeReports,
	})
	if err != nil {
		return nil, err
	}
	return &backupCreateSummary{
		Path:      res.Path,
		Size:      res.Size,
		FileCount: res.Manifest.FileCount,
		Label:     res.Manifest.Label,
		Targets:   res.Manifest.Targets,
	}, nil
}

func runBackupList(stateDir string) ([]backup.ListEntry, error) {
	return backup.List(stateDir)
}

func runBackupRestore(stateDir, path string, skipSafety, overwrite bool) (*backup.RestoreResult, error) {
	return backup.Restore(backup.RestoreOptions{
		StateDir:         stateDir,
		ArchivePath:      path,
		SkipSafetyBackup: skipSafety,
		Overwrite:        overwrite,
	})
}

func runBackupDelete(stateDir, path string) error {
	if err := backup.Delete(stateDir, path); err != nil {
		return err
	}
	fmt.Printf("已删除: %s\n", path)
	return nil
}

// dashStateDirInfo 记录 Dashboard StateDir 的解析结果及数据来源,
// 便于启动日志透明展示 "为什么 stateDir 最终是这个值"。
type dashStateDirInfo struct {
	Resolved      string             // 最终路径 (basedir.ResolveDefault 的输出)
	StateDirInput string             // 参与解析的 stateDir (CLI 或 JSON.stateDir)
	StateDirFrom  string             // "cli" / "json.stateDir" / "json.dashboard.stateDir(deprecated)" / ""
	CwdInput      string             // 参与解析的 cwd
	CwdFrom       string             // "json.cwd" / "os.getwd"
	JSONCfg       *feishu.JSONConfig // 预加载的 JSON 配置 (供调用方复用)
	ConfigPath    string             // 实际生效的配置文件路径
	Warnings      []string           // 例如 dashboard.stateDir 废弃提示
}

// botAPIURL 从配置中解析 feishu bot 的 wiki API URL，供 standalone dashboard 转发 team 操作。
// 返回空字符串表示未配置 wiki API 端口。
func botAPIURL(cfg *feishu.JSONConfig) string {
	if cfg == nil || cfg.Wiki == nil || cfg.Wiki.APIPort == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", cfg.Wiki.APIPort)
}

// resolveDashStateDir 统一 Dashboard 所有子命令 (run/start/stop/status/open) 的
// StateDir 解析规则, 与飞书 bot 保持完全一致 (basedir.ResolveDefault(StateDir, Cwd)):
//
//	优先级:
//	  stateDir:  CLI --state-dir
//	             > JSON.stateDir (顶层, 与 bot 共用)
//	             > JSON.dashboard.stateDir (已废弃, 打警告)
//	  cwd:       JSON.cwd (与 bot 共用)
//	             > os.Getwd() (进程当前目录)
//	  resolved:  basedir.ResolveDefault(stateDir, cwd)
//	             → stateDir 非空则直接用; 否则 cwd/.claude-go
//
// 这样可以保证:
//  1. 只设置 config.cwd 时, Dashboard 的 StateDir 与 bot 完全对齐 (cwd/.claude-go)
//  2. 独立运行 `claude-go dashboard run -c xxx.json` 也能正确解析到 bot 的数据目录
//  3. backups/.dashboard/actions/.dashboard/triggers 都落在与 bot 一致的位置
//
// configPath 为空时会沿用 feishu.LoadJSONConfig 的自动发现规则
// (./claude-go.json / ./config/claude-go.json / ~/.claude-go/config/config.json / ~/.claude-go/config.json)。
func resolveDashStateDir(cliStateDir, configPath string) (*dashStateDirInfo, error) {
	if configPath == "" {
		configPath = flagConfig
	}
	jsonCfg, err := feishu.LoadJSONConfig(configPath)
	if err != nil && configPath != "" {
		return nil, fmt.Errorf("加载配置文件失败: %w", err)
	}

	info := &dashStateDirInfo{
		JSONCfg:    jsonCfg,
		ConfigPath: configPath,
	}

	switch {
	case cliStateDir != "":
		info.StateDirInput = cliStateDir
		info.StateDirFrom = "cli"
	case jsonCfg != nil && jsonCfg.StateDir != "":
		info.StateDirInput = jsonCfg.StateDir
		info.StateDirFrom = "json.stateDir"
	case jsonCfg != nil && jsonCfg.Dashboard != nil && jsonCfg.Dashboard.StateDir != "":
		info.StateDirInput = jsonCfg.Dashboard.StateDir
		info.StateDirFrom = "json.dashboard.stateDir(deprecated)"
		info.Warnings = append(info.Warnings,
			"dashboard.stateDir 已废弃, 请改用顶层 stateDir (或留空自动取 cwd/.claude-go), 以便与 feishu bot 共用数据目录")
	}

	// 即使 CLI / 顶层 stateDir 已指定, 如果 dashboard.stateDir 同时存在也要提醒,
	// 避免用户误以为它仍然生效。
	if info.StateDirFrom != "json.dashboard.stateDir(deprecated)" &&
		jsonCfg != nil && jsonCfg.Dashboard != nil && jsonCfg.Dashboard.StateDir != "" {
		info.Warnings = append(info.Warnings,
			fmt.Sprintf("已忽略 dashboard.stateDir=%q (已废弃), 实际使用 %s", jsonCfg.Dashboard.StateDir, info.StateDirFrom))
	}

	if jsonCfg != nil && jsonCfg.Cwd != "" {
		info.CwdInput = jsonCfg.Cwd
		info.CwdFrom = "json.cwd"
	} else {
		info.CwdInput, _ = os.Getwd()
		info.CwdFrom = "os.getwd"
	}

	info.Resolved = basedir.ResolveDefault(info.StateDirInput, info.CwdInput)
	return info, nil
}

// printDashStateDirInfo 把 stateDir 解析过程透明输出到 stderr,
// 便于排查 "bot 和 dashboard 数据目录对不上" 类问题。
// label 用于区分调用者 (dashboard run / dashboard start / ...)。
func printDashStateDirInfo(info *dashStateDirInfo, label string) {
	if info == nil {
		return
	}
	for _, w := range info.Warnings {
		fmt.Fprintf(os.Stderr, "[%s] ⚠️  %s\n", label, w)
	}
	if info.ConfigPath != "" {
		fmt.Fprintf(os.Stderr, "[%s] 配置文件   : %s\n", label, info.ConfigPath)
	}
	if info.StateDirFrom != "" {
		fmt.Fprintf(os.Stderr, "[%s] stateDir   : %s  (来源: %s)\n", label, info.Resolved, info.StateDirFrom)
	} else {
		fmt.Fprintf(os.Stderr, "[%s] stateDir   : %s  (默认: %s/.claude-go, 来源=%s)\n",
			label, info.Resolved, info.CwdInput, info.CwdFrom)
	}
}

// runDashboardForeground 前台阻塞运行 dashboard。
func runDashboardForeground(addr string, port int, stateDir string, noOpen bool, configPath string) error {
	// StateDir / cwd 与飞书 bot 完全一致的解析规则, 避免数据目录错位。
	info, err := resolveDashStateDir(stateDir, configPath)
	if err != nil {
		return err
	}
	jsonCfg := info.JSONCfg

	// 让 dashboard 内部 LLM 解析器能找到配置文件
	if configPath != "" {
		_ = os.Setenv("CLAUDE_GO_CONFIG", configPath)
	}

	// addr/port/noOpen: CLI > JSON.dashboard
	if jsonCfg != nil && jsonCfg.Dashboard != nil {
		if addr == "" && jsonCfg.Dashboard.Addr != "" {
			addr = jsonCfg.Dashboard.Addr
		}
		if port == 7777 && jsonCfg.Dashboard.Port > 0 {
			port = jsonCfg.Dashboard.Port
		}
		if !noOpen && jsonCfg.Dashboard.NoOpen {
			noOpen = true
		}
	}

	resolved := info.Resolved
	printDashStateDirInfo(info, "Dashboard")

	bindAddr := addr
	if bindAddr == "" {
		if port == 0 {
			port = 7777
		}
		free := dashboard.FindFreePort(port)
		bindAddr = fmt.Sprintf("127.0.0.1:%d", free)
	}

	// 启动 LLM 调用指标采集 (全局钩子): 所有 api.Client 的调用都会落盘到
	// {stateDir}/metrics/llm.jsonl, dashboard 展示统一的 LLM token/质量视图。
	metrics.InitGlobalLLMCollector(resolved)

	srv := dashboard.NewServer(dashboard.Config{
		StateDir:  resolved,
		Addr:      bindAddr,
		CacheTTL:  2 * time.Second,
		BotAPIURL: botAPIURL(jsonCfg),
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	url := fmt.Sprintf("http://%s", bindAddr)
	if os.Getenv("CLAUDE_GO_DASHBOARD_DAEMON") == "" {
		fmt.Printf("\n🚀 Claude-Go Dashboard 已启动\n")
		fmt.Printf("   URL       : %s\n", url)
		fmt.Printf("   StateDir  : %s\n", resolved)
		fmt.Printf("   Mode      : 只读\n")
		fmt.Printf("   按 Ctrl+C 退出\n\n")
		if !noOpen {
			_ = openBrowser(url)
		}
	} else {
		fmt.Printf("[dashboard daemon] url=%s stateDir=%s\n", url, resolved)
	}

	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		if err := srv.Stop(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch {
	case fileExists("/usr/bin/open"):
		cmd = exec.Command("open", url)
	case fileExists("/usr/bin/xdg-open"), fileExists("/usr/local/bin/xdg-open"):
		cmd = exec.Command("xdg-open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func normalizeProviderModelAlias(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	parts := strings.SplitN(model, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return model
	}
	return parts[1]
}

func loadRuntimeJSONConfig() (*feishu.JSONConfig, string, error) {
	cfg, path, _, err := loadRuntimeJSONConfigWithOrigin()
	return cfg, path, err
}

// loadRuntimeJSONConfigWithOrigin 同 loadRuntimeJSONConfig, 额外返回命中的环节名。
//
// 环节名只给装配自报用 (pkg/assembly.ConfigSource.Origin): 查找链有六环, 出问题时
// "你改的不是它读的那一份"是最常见的浪费, 而两条路径都是合法存在的文件, 只给路径
// 看不出谁赢了。选哪一份的逻辑只有 ResolveJSONConfigPathWithOrigin 一处, 两条入口
// 不可能在这里分叉。
func loadRuntimeJSONConfigWithOrigin() (*feishu.JSONConfig, string, string, error) {
	path, origin, err := feishu.ResolveJSONConfigPathWithOrigin(flagConfig)
	if err != nil {
		return nil, "", origin, err
	}
	if path == "" {
		return nil, "", origin, nil
	}
	cfg, err := feishu.LoadJSONConfig(path)
	if err != nil {
		return nil, path, origin, err
	}
	return cfg, path, origin, nil
}

func resolveRuntimeModelConfig(jsonCfg *feishu.JSONConfig, preferredModel string) (modelconfig.ResolvedConfig, bool, error) {
	if jsonCfg == nil || len(jsonCfg.Providers) == 0 {
		return modelconfig.ResolvedConfig{}, false, nil
	}

	cfgJSON := jsonCfg.ToModelConfigJSON()
	_, resolver, err := modelconfig.LoadFromConfig(cfgJSON)
	if err != nil {
		return modelconfig.ResolvedConfig{}, false, err
	}

	preferredModel = strings.TrimSpace(preferredModel)
	defaultResolved := resolver.Resolve("", "")
	if preferredModel == "" || preferredModel == "qwen3.5-plus" {
		return defaultResolved, defaultResolved.ProviderName != "", nil
	}

	if !strings.Contains(preferredModel, ":") {
		if defaultResolved.ProviderName != "" {
			defaultResolved.Alias = preferredModel
			defaultResolved.ProviderName = preferredModel
			defaultResolved.FallbackModels = nil
			defaultResolved.FallbackBaseURL = ""
			defaultResolved.FallbackAPIKey = ""
			return defaultResolved, true, nil
		}
		return modelconfig.ResolvedConfig{Alias: preferredModel, ProviderName: preferredModel}, true, nil
	}

	cfgJSON.AI.GlobalConfig.DefaultModelAlias = preferredModel
	cfgJSON.AI.GlobalConfig.DefaultFallbackAliases = nil
	cfgJSON.AI.Plans = nil
	_, resolver, err = modelconfig.LoadFromConfig(cfgJSON)
	if err != nil {
		return modelconfig.ResolvedConfig{}, false, err
	}
	resolved := resolver.Resolve("", "")
	return resolved, resolved.ProviderName != "", nil
}

// advisorSectionFromConfig 返回配置中的 advisor 段, 优先 config.json, 回退项目级 settings.json。
func advisorSectionFromConfig(jsonCfg *feishu.JSONConfig, projectSettings *settings.Settings) *feishu.AdvisorSection {
	if jsonCfg != nil && jsonCfg.Advisor != nil {
		return jsonCfg.Advisor
	}
	if projectSettings != nil && projectSettings.Advisor != nil {
		return projectSettings.Advisor
	}
	return nil
}

// buildAdvisorClient 按 "--advisor flag > config advisor 段" 的优先级构造 advisor 模型客户端。
// 返回 ok=false 表示 advisor 未启用或配置无效 (无效时打印警告, 不阻断启动)。
func buildAdvisorClient(jsonCfg *feishu.JSONConfig, projectSettings *settings.Settings) (*api.Client, builtin.AdvisorOptions, bool) {
	opts := builtin.AdvisorOptions{}
	alias := ""
	if adv := advisorSectionFromConfig(jsonCfg, projectSettings); adv != nil {
		if adv.Enabled {
			alias = adv.ModelAlias
		}
		opts.MaxCallsPerSession = adv.MaxCallsPerSession
		opts.CooldownTurns = adv.CooldownTurns
		opts.MaxTranscriptTokens = adv.MaxTranscriptTokens
		opts.MaxOutputTokens = adv.MaxOutputTokens
	}
	if flagAdvisor != "" {
		if strings.EqualFold(flagAdvisor, "off") {
			return nil, opts, false
		}
		alias = flagAdvisor
	}
	if alias == "" {
		return nil, opts, false
	}

	advResolved, ok, err := resolveRuntimeModelConfig(jsonCfg, alias)
	if err != nil || !ok || advResolved.BaseURL == "" || advResolved.APIKey == "" || advResolved.ProviderName == "" {
		fmt.Fprintf(os.Stderr, "警告: advisor 别名 %q 解析失败 (请检查 providers 配置), advisor 已禁用\n", alias)
		return nil, opts, false
	}
	var client *api.Client
	if api.IsLocalEndpoint(advResolved.BaseURL) {
		client = api.NewOllamaClient(advResolved.BaseURL, advResolved.ProviderName)
		client.APIKey = advResolved.APIKey
	} else {
		client = api.NewClient(advResolved.BaseURL, advResolved.APIKey, advResolved.OutboundModelName())
	}
	client.Protocol = advResolved.Protocol
	client.ReasoningPlayback = advResolved.ReasoningPlayback
	client.SetProxy(advResolved.Proxy)
	client.Tag = "advisor"
	if advResolved.CallTimeoutSec > 0 {
		client.CallTimeout = time.Duration(advResolved.CallTimeoutSec) * time.Second
	}
	if advResolved.FirstTokenTimeoutSec > 0 {
		client.FirstTokenTimeout = time.Duration(advResolved.FirstTokenTimeoutSec) * time.Second
	}
	if flagDebug {
		fmt.Fprintf(os.Stderr, "[AdvisorTool] 已启用, advisor 模型: %s\n", alias)
	}
	return client, opts, true
}

// buildPlanClient 构造规划期独立模型客户端 (Tag=plan), 类似 advisor 独立客户端:
// 独立 provider/模型, 会话 plan flag ON (规划期) 时 queryLoop 全程路由到此客户端。
// alias 格式 "provider:model" 或短名; 解析失败时打印警告并返回 nil (不阻断启动)。
func buildPlanClient(jsonCfg *feishu.JSONConfig, alias string) (*api.Client, bool) {
	planResolved, ok, err := resolveRuntimeModelConfig(jsonCfg, alias)
	if err != nil || !ok || planResolved.BaseURL == "" || planResolved.APIKey == "" || planResolved.ProviderName == "" {
		fmt.Fprintf(os.Stderr, "警告: plan 模型别名 %q 解析失败 (请检查 providers 配置), 规划期将沿用主模型\n", alias)
		return nil, false
	}
	var client *api.Client
	if api.IsLocalEndpoint(planResolved.BaseURL) {
		client = api.NewOllamaClient(planResolved.BaseURL, planResolved.ProviderName)
		client.APIKey = planResolved.APIKey
	} else {
		client = api.NewClient(planResolved.BaseURL, planResolved.APIKey, planResolved.OutboundModelName())
	}
	client.Protocol = planResolved.Protocol
	client.ReasoningPlayback = planResolved.ReasoningPlayback
	client.SetProxy(planResolved.Proxy)
	client.Tag = "plan"
	if planResolved.CallTimeoutSec > 0 {
		client.CallTimeout = time.Duration(planResolved.CallTimeoutSec) * time.Second
	}
	if planResolved.FirstTokenTimeoutSec > 0 {
		client.FirstTokenTimeout = time.Duration(planResolved.FirstTokenTimeoutSec) * time.Second
	}
	if flagDebug {
		fmt.Fprintf(os.Stderr, "[PlanModel] 已启用, 规划期模型: %s\n", alias)
	}
	return client, true
}

func applyRuntimePromptDebug(apiClient *api.Client, jsonCfg *feishu.JSONConfig) {
	if apiClient == nil {
		return
	}
	enabled := false
	dir := ""
	maxFiles := 500
	maxBytes := int64(200 * 1024 * 1024)
	sampleRate := 1.0
	redact := true
	stateDir := ""
	if jsonCfg != nil {
		stateDir = strings.TrimSpace(jsonCfg.StateDir)
		if jsonCfg.Engine != nil {
			if jsonCfg.Engine.PromptDebug != nil {
				enabled = *jsonCfg.Engine.PromptDebug
			}
			dir = strings.TrimSpace(jsonCfg.Engine.PromptDebugDir)
			if jsonCfg.Engine.PromptDebugMaxFiles > 0 {
				maxFiles = jsonCfg.Engine.PromptDebugMaxFiles
			}
			if jsonCfg.Engine.PromptDebugMaxBytes > 0 {
				maxBytes = jsonCfg.Engine.PromptDebugMaxBytes
			}
			if jsonCfg.Engine.PromptDebugSampleRate > 0 {
				sampleRate = jsonCfg.Engine.PromptDebugSampleRate
			}
			if jsonCfg.Engine.PromptDebugRedact != nil {
				redact = *jsonCfg.Engine.PromptDebugRedact
			}
		}
	}
	if env := strings.TrimSpace(os.Getenv("CLAUDE_GO_PROMPT_DEBUG")); env != "" {
		enabled = env == "1" || strings.EqualFold(env, "true") || strings.EqualFold(env, "yes") || strings.EqualFold(env, "on")
	}
	if envDir := strings.TrimSpace(os.Getenv("CLAUDE_GO_PROMPT_DEBUG_DIR")); envDir != "" {
		dir = envDir
	}
	if !enabled {
		return
	}
	if dir == "" {
		if stateDir == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				stateDir = filepath.Join(home, ".claude-go")
			}
		}
		if stateDir == "" {
			stateDir = ".claude-go"
		}
		dir = filepath.Join(stateDir, "prompt-debug")
	}
	apiClient.PromptDebugEnabled = true
	apiClient.PromptDebugDir = dir
	apiClient.PromptDebugMaxFiles = maxFiles
	apiClient.PromptDebugMaxBytes = maxBytes
	apiClient.PromptDebugSampleRate = sampleRate
	apiClient.PromptDebugRedact = redact
}

func applyRuntimeSandboxConfig(jsonCfg *feishu.JSONConfig, cwd string) {
	stateDirInput := ""
	var cfg sandbox.Config
	if jsonCfg != nil {
		stateDirInput = jsonCfg.StateDir
		if jsonCfg.Sandbox != nil {
			cfg = *jsonCfg.Sandbox
		}
	}
	cfg.StateDir = basedir.ResolveDefault(stateDirInput, cwd)
	sandbox.Configure(cfg)
}

// cliSkillCreator 为 headless run 团队路径构造技能自创建器 (design/03 §1.2 开环2)。
// Registry 传 nil: CLI 是一次性进程, 无需热重载; 新技能落 <state>/skills/ 目录,
// 下一次进程 LoadDefaults 时自然收录。
func cliSkillCreator(layout *basedir.Layout, stateDir string, eng *engine.QueryEngine) agent.SkillAutoCreator {
	if eng == nil || eng.APIClient == nil {
		return nil
	}
	skillDir := filepath.Join(stateDir, "skills")
	if layout != nil {
		skillDir = layout.Skills
	}
	return skills.NewAutoCreator(skillDir, eng.APIClient, eng.APIClient.Model, nil)
}

// cliDreamer 暴露 buildEngine 装配的 Dreamer 给 run 团队路径复用, 使 headless
// 团队完成后也触发 Dreaming 整理 (design/03 §1.2 开环3 修复)。
// P0 过渡方案: P2 分层接口 (design/02 R0) 落地后由显式依赖注入取代。
var cliDreamer *dreaming.Dreamer

// cliPoolPolicy 池个性化 policy 的两段式注入载体 (13.7-P1): buildEngine 装配点
// 先建空 policy 传给包装三件, TraceStore 创建后补 SetStateDir/SetTraceStore。
// 仅在统一池开启时非 nil。
var cliPoolPolicy *builtin.PoolPolicy

// cliAssembly 保存最近一次 buildEngine 的装配自报 (方案 20.6.4 D0.7)。
//
// 与 cliDreamer/cliPoolPolicy 同一套路 (buildEngine 无返回值通道可带, 又要在 callRunE
// 里取用): 装完就存在这里, envelope 与日志再从这里取。
//
// 为什么不在 callRunE 里现采: 采集要读引擎的活对象, 而 best-of-N 会**重建**引擎
// (attempt>1 时 buildEngine 再来一次), 那时早先采集的那份就代表不了最后一次尝试。
// 由 buildEngine 在装配收尾点自己采, 每次构造对应一份, 语义上不会错位。
//
// 每次 buildEngine 覆盖写: 单进程同一时刻只服务一个 run (runCmd 是前台命令),
// 不构成共享状态。团队路径不经过这里, 故为 nil 时一切照旧。
var cliAssembly *assembly.Report

// cliAssemblyReconcile 是本进程自比的对账结论 (方案 20.6.4 D0.7):
// 声明侧取自配置与 Enable* 开关, 实际侧取自 cliAssembly。与 cliAssembly 同生共死,
// 都在 buildEngine 的装配收尾点产出。
var cliAssemblyReconcile *assembly.ReconcileResult

// runTraceIDs 配置声明的运维索引维度 (工单 / 实例), 由 buildEngine 装配点写入。
//
// 与 SessionID 一样是**跨 run 稳定**的业务键, 但不参与会话分片 (那是 SessionID 的
// 职责): 它只回答"这条记录属于哪个工单 / 哪个实例"。两者刻意不合并 —— 一张工单会
// 跨多次运行, 拿它当缓存分片键会让两次运行互相顶掉缓存, 见 pkg/trace.IDs 的说明。
var runTraceIDs trace.IDs

// runTraceSeq 是 `claude-go run` 的 RunID 发号器 (session_trace.go 同款理由)。
//
// 进程内全局递增: 一次 run 进程通常只跑一次, 序号主要为 best-of-N 那几次尝试
// 与将来的常驻调用方留出唯一性 —— 否则同一 session id 的两次尝试会写进同一个
// 桶文件, 而它们本该是两次独立运行 (奖励归因要求 RunID 每次唯一)。
var runTraceSeq atomic.Uint64

// cliDreamAdapter 适配 dreaming.Dreamer → agent.DreamRecorder (与 feishu.dreamAdapter 同构)。
type cliDreamAdapter struct{ dreamer *dreaming.Dreamer }

func (da *cliDreamAdapter) RecordSession(record agent.DreamSessionRecord) {
	if da.dreamer == nil {
		return
	}
	da.dreamer.RecordSession(dreaming.SessionRecord{
		ChatID:  record.ChatID,
		EndTime: record.EndTime,
		Summary: record.Summary,
	})
}

func (da *cliDreamAdapter) AfterQuery(ctx context.Context) {
	if da.dreamer == nil {
		return
	}
	da.dreamer.AfterQuery(ctx)
}

func buildEngine() (*engine.QueryEngine, error) {
	cwd, _ := os.Getwd()

	jsonCfg, jsonConfigPath, configOrigin, err := loadRuntimeJSONConfigWithOrigin()
	if err != nil {
		return nil, fmt.Errorf("加载配置文件失败: %w", err)
	}
	// 运维索引维度 (方案 20.6.4): 工单与实例是本次运行的业务归属, 配置里声明一次,
	// 由 buildEngine 在装配点写进 trace.IDs, 之后所有出站请求头 / Span / 落盘记录
	// 都带着它 —— 这是"给一个 instance_id, 30 秒内拉出全部决策链"的地基 (D0.5)。
	// 空 = 交互式使用, 属正常状态而非缺失, 故只在非空时注入。
	if jsonCfg != nil && (jsonCfg.TicketID != "" || jsonCfg.InstanceID != "") {
		runTraceIDs = trace.IDs{TicketID: jsonCfg.TicketID, InstanceID: jsonCfg.InstanceID}
	}
	if jsonCfg != nil && jsonCfg.Cwd != "" {
		cwd = jsonCfg.Cwd
	}
	if flagCwd != "" {
		if abs, err := filepath.Abs(flagCwd); err == nil {
			cwd = abs
		} else {
			cwd = flagCwd
		}
	}
	applyRuntimeSandboxConfig(jsonCfg, cwd)

	projectSettings := settings.LoadProjectSettings(cwd)
	projectSettings.ApplyEnv()

	resolvedModel, hasResolvedModel, err := resolveRuntimeModelConfig(jsonCfg, flagModel)
	if err != nil {
		return nil, fmt.Errorf("解析 providers 配置失败: %w", err)
	}

	apiKey := getAPIKey()
	if apiKey == "" && hasResolvedModel && resolvedModel.APIKey != "" {
		apiKey = resolvedModel.APIKey
	}
	if apiKey == "" && projectSettings.AI != nil && projectSettings.AI.APIKey != "" {
		apiKey = projectSettings.AI.APIKey
	}

	effectiveModel := flagModel
	if hasResolvedModel {
		// 出站模型名取完整别名 (带 provider 前缀)。注意与下面 normalizeProviderModelAlias
		// 的分工: 那条分支跑在 !hasResolvedModel 时, 处理的是"裸 provider:model 字面量";
		// 这里是已解析配置, 别名本身就是权威值, 不能再剥前缀。
		effectiveModel = resolvedModel.PreferredModelName()
	}
	if effectiveModel == "qwen3.5-plus" && projectSettings.Model != "" {
		effectiveModel = projectSettings.Model
	}
	if effectiveModel == "qwen3.5-plus" && projectSettings.AI != nil && projectSettings.AI.Model != "" {
		effectiveModel = projectSettings.AI.Model
	}
	if !hasResolvedModel {
		// 仅对未经 providers 解析的 "provider:model" 形式剥离前缀;
		// 已解析的 ProviderName 是厂商真实模型名, 可能本身含冒号
		// (如 Ollama 的 "gemma4:26b-a4b-it-qat"), 不能二次截断。
		effectiveModel = normalizeProviderModelAlias(effectiveModel)
	}

	baseURL := flagBaseURL
	if baseURL == "" {
		baseURL = os.Getenv("API_BASE_URL")
	}
	if baseURL == "" && hasResolvedModel && resolvedModel.BaseURL != "" {
		baseURL = resolvedModel.BaseURL
	}
	if baseURL == "" && projectSettings.AI != nil && projectSettings.AI.BaseURL != "" {
		baseURL = projectSettings.AI.BaseURL
	}

	// 本机端点 (如 Ollama localhost:11434) 无需 API Key，自动使用占位 key
	if apiKey == "" && api.IsLocalEndpoint(baseURL) {
		apiKey = "ollama"
	}
	if apiKey == "" {
		return nil, fmt.Errorf("需要 API Key: 设置 ANTHROPIC_API_KEY 环境变量、--api-key 参数，或在配置文件 providers 中设置 apiKey (本机 Ollama 端点可免 key)")
	}

	var apiClient *api.Client
	if baseURL != "" {
		trimmed := strings.TrimRight(baseURL, "/")
		if strings.HasSuffix(trimmed, "/anthropic") || strings.HasSuffix(trimmed, "/compatible-mode") {
			trimmed += "/v1"
		}
		if api.IsLocalEndpoint(trimmed) {
			// 本地模型解码受内存带宽限制，复用 Ollama 客户端的宽松超时
			apiClient = api.NewOllamaClient(trimmed, effectiveModel)
			apiClient.APIKey = apiKey
		} else {
			apiClient = api.NewClient(trimmed, apiKey, effectiveModel)
		}
	} else {
		apiClient = api.NewDashScopeClient(apiKey, effectiveModel)
	}
	if hasResolvedModel {
		// 出站 model 值与上面 effectiveModel 的分工: effectiveModel 是**身份**
		// (进 transcript / metrics, 别名带 provider 更利于归因); 这里改的是
		// **发到线上的字节**, 取决于链路形态 —— 经网关发别名 (网关的查找键),
		// 直连 provider 发裸名 (厂商端点不认前缀)。见 OutboundModelName。
		apiClient.Model = resolvedModel.OutboundModelName()
		apiClient.FallbackModels = resolvedModel.FallbackModels
		apiClient.FallbackBaseURL = resolvedModel.FallbackBaseURL
		apiClient.FallbackAPIKey = resolvedModel.FallbackAPIKey
		apiClient.PromptCacheMode = resolvedModel.PromptCacheMode
		// 出站协议路由: anthropic(/messages) | openai(/chat/completions) | openai-responses(/responses)。
		// 漏设会导致 chat/responses 模型 (glm/mimo/muse-spark) 全部被路由到 /messages 而失败。
		apiClient.Protocol = resolvedModel.Protocol
		apiClient.ReasoningPlayback = resolvedModel.ReasoningPlayback
		// 模型级出站代理: 只有配置了 proxy 的模型 (如 muse-spark 走东京 tinyproxy) 走代理。
		apiClient.SetProxy(resolvedModel.Proxy)
		// 模型级超时覆盖 (本地 Ollama 解码慢, 配置中可调大)
		if resolvedModel.FirstTokenTimeoutSec > 0 {
			apiClient.FirstTokenTimeout = time.Duration(resolvedModel.FirstTokenTimeoutSec) * time.Second
		}
		if resolvedModel.CallTimeoutSec > 0 {
			apiClient.CallTimeout = time.Duration(resolvedModel.CallTimeoutSec) * time.Second
		}
		if resolvedModel.DeadlineRetryBaseSec > 0 {
			apiClient.DeadlineRetryBase = time.Duration(resolvedModel.DeadlineRetryBaseSec) * time.Second
		}
	}
	applyRuntimePromptDebug(apiClient, jsonCfg)

	// 全局 LLM 准入控制器: RPM 令牌桶 + 并发信号量 + AIMD
	// 使用模型配置中的限流参数 (替代硬编码默认值)
	guardCfg := api.DefaultGuardConfig()
	if hasResolvedModel {
		if resolvedModel.RPM > 0 {
			guardCfg.RPM = resolvedModel.RPM
		}
		if resolvedModel.MaxParallel > 0 {
			guardCfg.MaxParallel = resolvedModel.MaxParallel
		}
		if resolvedModel.MinParallel > 0 {
			guardCfg.MinParallel = resolvedModel.MinParallel
		}
	}
	apiClient.Guard = api.NewRateLimitGuard(guardCfg)

	// 为本客户端打上业务标签, 便于 dashboard 按 source 维度聚合指标。
	// 交互/单次执行 → "cli", feishu/team/dashboard 会覆盖此值。
	apiClient.Tag = "cli"

	mcpConfigPath := flagMCPConfig
	if mcpConfigPath == "" && jsonCfg != nil && len(jsonCfg.MCPServers) > 0 {
		mcpConfigPath = jsonConfigPath
	}
	mcpConns, err := connectMCP(context.Background(), mcpConfigPath)
	if err != nil {
		return nil, fmt.Errorf("MCP 配置: %w", err)
	}

	permMode := types.PermissionMode(flagPermission)
	if flagPermission == "bypass" && jsonCfg != nil && jsonCfg.PermissionMode != "" {
		permMode = types.PermissionMode(jsonCfg.PermissionMode)
	}
	if permMode == "bypass" && projectSettings.Permissions.DefaultMode != "" {
		permMode = types.PermissionMode(projectSettings.Permissions.DefaultMode)
	}
	permChecker := permissions.NewChecker(permMode)

	for _, rule := range projectSettings.Permissions.Allow {
		permChecker.AllowRules = append(permChecker.AllowRules, types.PermissionRule{
			ToolName: rule.Tool,
			Pattern:  rule.Pattern,
			Source:   "settings.json",
		})
	}
	for _, rule := range projectSettings.Permissions.Deny {
		permChecker.DenyRules = append(permChecker.DenyRules, types.PermissionRule{
			ToolName: rule.Tool,
			Pattern:  rule.Pattern,
			Source:   "settings.json",
		})
	}

	contextWindow := 200000
	if hasResolvedModel && resolvedModel.ContextWindow > 0 {
		contextWindow = resolvedModel.ContextWindow
	}

	var hookConfigs []types.HookConfig
	if jsonCfg != nil {
		for _, h := range jsonCfg.Hooks {
			hookConfigs = append(hookConfigs, types.HookConfig{
				Event:    types.HookEvent(h.Event),
				HookType: types.HookType(h.HookType),
				Command:  h.Command,
				Timeout:  h.Timeout,
				If:       h.If,
				URL:      h.URL,

				MCPServer:    h.MCPServer,
				MCPTool:      h.MCPTool,
				PluginPath:   h.PluginPath,
				PluginSymbol: h.PluginSymbol,
				OPAPolicy:    h.OPAPolicy,
				OPAQuery:     h.OPAQuery,
				FunctionName: h.FunctionName,
				GRPCService:  h.GRPCService,
				GRPCMethod:   h.GRPCMethod,
			})
		}
	}
	// 外部 hook 的装载点。config 段的 hooks 与飞书路径读的是同一个 JSONConfig
	// 字段, 所以两条入口在这里**逐字节同构** —— 这是修掉装配不对称的一半 (另一半
	// 是下面的 EnableFrontierOptimizations)。
	hookRunner := hooks.NewRunner(hookConfigs, "")
	if n := hookRunner.Count(); n > 0 {
		log.Printf("[assembly] 已装载外部 hook %d 条 (来自 %s)", n, jsonConfigPath)
	}
	// 插件展开 (run --plugin, 实现见 plugin_flags.go)。位置是全部要害: 必须在技能
	// 注册表装载 (下方 skillReg)、池装配 (RegisterPoolTools) 与 runAsAgent 的 agent
	// 解析之前 —— `--plugin foo --agent <来自 foo 的 agent>` 依赖这个顺序。
	// hook 走 Runner 的可装卸组 (装配期装上 = 引擎开跑前生效); 同时并入 hookConfigs
	// 变量 —— deps.hookConfigs 会把它们带给子代理的 nested runner (runNestedAgent
	// 的合并读的是这个切片, 只装组的话插件 hook 在"子代理自己有资产 hook"的那条
	// 分支上会静默消失)。
	if wiring, err := wireFlagPlugins(cwd, jsonCfg, hookRunner); err != nil {
		return nil, err
	} else if wiring != nil {
		cliPlugins = wiring
		hookConfigs = append(hookConfigs, wiring.hookCfgs...)
	}
	compactor := compact.NewCompactor(apiClient, contextWindow)
	promptMgr := prompt.NewManager(cwd)
	// 日期默认不写进系统提示 (见 prompt.Manager.ShowEnvironmentDate):
	// 它每天变一次, 而它之后还有工具说明/记忆/角色指令, 等于让整段前缀天天作废。
	if jsonCfg != nil && jsonCfg.Engine != nil && jsonCfg.Engine.EnvironmentDate != nil {
		promptMgr.ShowEnvironmentDate = *jsonCfg.Engine.EnvironmentDate
	}
	if flagSystemPrompt != "" {
		promptMgr.CustomPrompt = flagSystemPrompt
	} else if jsonCfg != nil && jsonCfg.SystemPrompt != "" {
		promptMgr.CustomPrompt = jsonCfg.SystemPrompt
	} else if projectSettings.SystemPrompt != "" {
		promptMgr.CustomPrompt = projectSettings.SystemPrompt
	}
	skillReg := skills.NewRegistry()
	skillReg.LoadDefaults(cwd)
	// 插件技能 (run --plugin): 在 Skill 工具注册判定与清单渲染 (下方两处) 之前进
	// 注册表, source="plugin" (rank 150 —— 见 skills.SourceRank: 输给项目, 赢过用户级)。
	if cliPlugins != nil {
		loadPluginSkillDirs(skillReg, cliPlugins.bundles)
	}
	if skillReg.Count() > 0 {
		// 0 = 不设软上限, 由描述字符预算约束; 所有技能名称始终可见, 保证 Skill 工具可被正确调用。
		promptMgr.SkillListing = skillReg.FormatShortListingForDir(0, cwd) // paths 命中的技能才进清单 (第七章缺口修复)
	}

	effectiveMaxTokens := flagMaxTokens
	if effectiveMaxTokens == 16384 && hasResolvedModel && resolvedModel.MaxTokens > 0 {
		effectiveMaxTokens = resolvedModel.MaxTokens
	}
	if effectiveMaxTokens == 16384 && projectSettings.MaxTokens > 0 {
		effectiveMaxTokens = projectSettings.MaxTokens
	}
	effectiveMaxTurns := flagMaxTurns
	if effectiveMaxTurns == 0 && hasResolvedModel && resolvedModel.MaxTurns > 0 {
		effectiveMaxTurns = resolvedModel.MaxTurns
	}
	if effectiveMaxTurns == 0 && projectSettings.MaxTurns > 0 {
		effectiveMaxTurns = projectSettings.MaxTurns
	}
	// Path B 执行模型 (工具消费回合): 配置优先, flag 覆盖。
	// serve/run 两条路径共用 buildEngine, 此前只认 --execution-model flag,
	// 配置文件里的 ai.executionModel 在此路径被静默丢弃 —— 部署控制面用
	// config.json 而非 flag 注入时, 双模型路由完全不生效。
	effectiveExecutionModel := flagExecutionModel
	if effectiveExecutionModel == "" && jsonCfg != nil && jsonCfg.AI != nil && jsonCfg.AI.ExecutionModel != "" {
		effectiveExecutionModel = jsonCfg.AI.ExecutionModel
	}
	// AutoMode 有效配置: flag > jsonCfg.ai.autoPlan > 默认 (开 + heuristic)。
	// 仅当已处于显式 plan 权限时禁用 (自动规划本身即会强制只读, 尊重已规划意图);
	// bypass 权限下保持开启 —— 规划阶段的只读强制由 DynamicPlanCheck 独立生效,
	// 与 CLI/飞书入口无关, 这是"引擎自带能力"的默认形态。
	autoPlanMode := true
	complexityMode := "heuristic"
	if jsonCfg != nil && jsonCfg.AI != nil && jsonCfg.AI.AutoPlan != nil {
		if jsonCfg.AI.AutoPlan.Enabled != nil {
			autoPlanMode = *jsonCfg.AI.AutoPlan.Enabled
		}
		if jsonCfg.AI.AutoPlan.Mode != "" {
			complexityMode = jsonCfg.AI.AutoPlan.Mode
		}
	}
	switch flagAutoPlan {
	case "off":
		autoPlanMode = false
	case "on":
		autoPlanMode = true
	}
	if flagComplexityMode != "" {
		complexityMode = flagComplexityMode
	}
	if permMode == types.PermissionModePlan {
		autoPlanMode = false
	}

	// AutoAdvisor: 复杂任务自动咨询 advisor 并注入建议。默认开 (有 advisor 时生效),
	// 可经 config ai.autoPlan.autoAdvisor:false 关闭。真正生效还需下方 advisor 已装配
	// (buildAdvisorClient ok 时 cfg.AutoAdvisor 才置位)。
	autoAdvisor := true
	if jsonCfg != nil && jsonCfg.AI != nil && jsonCfg.AI.AutoPlan != nil && jsonCfg.AI.AutoPlan.AutoAdvisor != nil {
		autoAdvisor = *jsonCfg.AI.AutoPlan.AutoAdvisor
	}

	// 计划文件目录: ExitPlanMode 将计划落盘, 实施阶段模型读取。默认 <stateDir>/plans。
	planDir := ""
	if jsonCfg != nil && strings.TrimSpace(jsonCfg.StateDir) != "" {
		planDir = filepath.Join(jsonCfg.StateDir, "plans")
	} else {
		planDir = filepath.Join(cwd, ".claude-go", "plans")
	}

	// 文件级检查点: Write/StrReplace 每次改动前留一份修订版本, 供回退。
	// stateDir 显式取自配置 (与 planDir 同一口径), 不再让工具侧二次推断 ——
	// 推断规则一旦和这里分叉, 快照会落到另一个目录, 回退时找不到。
	fileHistoryStateDir := ""
	if jsonCfg != nil {
		fileHistoryStateDir = jsonCfg.StateDir
	}
	builtin.SetFileHistory(builtin.NewFileHistory(fileHistoryStateDir, cwd))

	// H3-2 隔离运行的中间态 (RunIsolated 的落盘与断点恢复)。与上面 file-history
	// 同根: 两者都是"这个工作区上发生过什么"的旁路账本, 分到两个根目录只会让清理
	// 与备份漏掉一个。空 StateDir 时按同一套 ResolveDefault 解析 ——
	// NewFileHistory 内部也是这么退化的, 这里显式算一次是为了让两个目录真的落在
	// 同一处, 而不是各自推各自的。
	//
	// 真正的装配在引擎构造之后 (SetRunStateDir 是 *QueryEngine 的方法)。取值先算
	// 在这里, 是因为 fileHistoryStateDir 只在这个作用域里有。
	runStateDir := basedir.ResolveDefault(fileHistoryStateDir, cwd)

	// Plan 模型客户端: 规划期独立模型 (类似 advisor 独立客户端, Tag=plan)。
	// 别名优先级: --plan-model flag > config ai.planModel; 空 = 关闭 (规划期沿用主模型)。
	var planClient *api.Client
	effectivePlanModel := flagPlanModel
	if effectivePlanModel == "" && jsonCfg != nil && jsonCfg.AI != nil && jsonCfg.AI.PlanModel != "" {
		effectivePlanModel = jsonCfg.AI.PlanModel
	}
	if effectivePlanModel != "" {
		planClient, _ = buildPlanClient(jsonCfg, effectivePlanModel)
	}

	cfg := &engine.Config{
		Model:            effectiveModel,
		OutboundModel:    apiClient.Model, // 身份(Model)与出站名分离, 见 engine.Config.OutboundModel
		ExecutionModel:   effectiveExecutionModel,
		MaxTokens:        effectiveMaxTokens,
		MaxTurns:         effectiveMaxTurns,
		ContextWindow:    contextWindow,
		Cwd:              cwd,
		PermissionMode:   permMode,
		IsNonInteractive: flagPrint,
		Debug:            flagDebug,
		AllowedTools:     effectiveAllowedTools(flagAllowedTools, jsonCfg),
		SessionID:        "_global", // EnterPlanMode/ExitPlanMode 写全局会话 plan flag
		// 单一 plan 状态源: AutoModeHook 自动开启 / 模型工具共用同一 flag。
		DynamicPlanCheck: builtin.NewPlanModeChecker("_global"),
		PlanModeSetter:   func(on bool) { builtin.SetPlanModeForSession("_global", on) },
		AutoPlanMode:     autoPlanMode,
		ComplexityMode:   complexityMode,
		// AutoAdvisor 在 advisor 装配成功后才置位 (见下方 buildAdvisorClient 分支)。
		PlanFileDir: planDir,
		// PlanClient 规划期独立模型客户端 (Tag=plan); nil = 规划期沿用主模型。
		PlanClient: planClient,
	}
	// 记忆注入形态 (配置项 engine.memoryIndexMode)。nil = 未配置, 由
	// QueryEngine.resolvedMemoryIndexMode 回落到 CLAUDE_GO_MEMORY_INDEX_MODE。
	// headless 进程是一次性的: 改配置要下次启动才生效 —— 这正是配置项存在的
	// 意义 (env 要改容器清单并 rollout, 配置改完下次拉起即带上)。
	if jsonCfg != nil && jsonCfg.Engine != nil && jsonCfg.Engine.MemoryIndexMode != nil {
		cfg.MemoryIndexMode = jsonCfg.Engine.MemoryIndexMode
	}
	// 方案三 (手册 13.3): 弱模型 harness 增强——本地小模型 (Ollama 接入) 的
	// 低级失败 (schema 违背/选错工具/上下文腐烂/不规划) 用冻结权重的确定性机制消除。
	// 开关解析: CLAUDE_GO_WEAK_MODEL=1|0 显式控制 > 自动 (provider=="ollama" 时开)。
	{
		wmlProvider := ""
		if hasResolvedModel {
			wmlProvider = resolvedModel.Provider
		} else if i := strings.Index(effectiveModel, ":"); i > 0 {
			wmlProvider = effectiveModel[:i]
		}
		cfg.WeakModel = weakmodel.Resolve(wmlProvider, os.Getenv)
		if cfg.WeakModel.Enabled {
			promptMgr.WeakModelHints = cfg.WeakModel.PlanningHints
			log.Printf("[weakmodel] 弱模型增强已启用 (provider=%s): schemaRetry=%v resultPostprocess=%v toolMasking=%v planningHints=%v toolChoiceAny=%v",
				wmlProvider, cfg.WeakModel.SchemaRetry, cfg.WeakModel.ResultPostprocess,
				cfg.WeakModel.ToolMasking, cfg.WeakModel.PlanningHints, cfg.WeakModel.ToolChoiceAny)
		}
	}
	// 方案三 L8: 引擎级 FallbackModel (确定性失败信号升级的目标档位)。
	// 与客户端 FallbackModels 同源; 此前引擎字段从未被装配 (L8 是第一个消费方)。
	if cfg.FallbackModel == "" && hasResolvedModel && len(resolvedModel.FallbackModels) > 0 {
		cfg.FallbackModel = resolvedModel.FallbackModels[0]
	}
	// llm/hybrid 复杂度判定: 用主 apiClient 的 SimpleComplete + 分类器 system prompt。
	if autoPlanMode && (complexityMode == "llm" || complexityMode == "hybrid") {
		cfg.LLMComplexityFn = cliComplexityClassifier(apiClient)
	}

	// agentDB 语义增强面 (规划 14.1.4.3): --agentdb-url 装配 AgentDBSemanticInjectHook,
	// 使真实引擎经 serve /v1/memory|retrieve|files 跨源检索并注入 system prompt。
	if url := strings.TrimSpace(flagAgentDBURL); url != "" {
		cfg.AgentDBSemantic = agentdbsemantic.New(strings.TrimRight(url, "/"))
		cfg.SemanticTopK = flagAgentDBTopK
		cfg.SemanticTokenBudget = flagAgentDBBudget
		cfg.SemanticReserve = flagAgentDBReserve
		log.Printf("[agentdb] 已装配层2语义增强注入: url=%s topK=%d budget=%d reserve=%d",
			url, flagAgentDBTopK, flagAgentDBBudget, flagAgentDBReserve)
	}

	deps := &engineDeps{
		cfg:         cfg,
		apiClient:   apiClient,
		hookRunner:  hookRunner,
		permChecker: permChecker,
		compactor:   compactor,
		promptMgr:   promptMgr,
		mcpConns:    mcpConns,
		skillReg:    skillReg,
		hookConfigs: hookConfigs,
	}

	reg := tool.NewRegistry()
	todoTool := builtin.RegisterBaseTools(reg, nil)
	// H1-3: 把待办清单接到压缩器上。压缩会把摘要之前的历史整个折掉 —— 包括模型
	// 写过的 TodoWrite 调用 —— 于是"还剩什么没做完"在压缩后对模型不可见。
	// 传的是 Snapshot (有序) 而不是 GetTodos (map 顺序随机): 这份清单每轮重发,
	// 顺序一抖前缀缓存就从插入点起全废。
	if todoTool != nil {
		cfg.TaskRoster = func() []internal_hook.TaskRosterItem {
			snaps := todoTool.Snapshot()
			out := make([]internal_hook.TaskRosterItem, 0, len(snaps))
			for _, s := range snaps {
				out = append(out, internal_hook.TaskRosterItem{ID: s.ID, Content: s.Content, Status: s.Status})
			}
			return out
		}
	}
	// H6 多档冒烟的真实档位构造器 (design/03 §4.5)。RegisterBaseTools 上一行注册了
	// evo_* 八件套, 但 SetEvoTierFactory 此前**只在飞书 bot 装配** —— CLI 形态下
	// evo_smoke 只有一个确定性"装配档", 而装配档不是模型档位, MinTiers>=2 必然拒绝:
	// 同一个工具在两条入口下能力不同, 那是"CLI 侧建成未通电"。档位取法与
	// FallbackReflector 完全一致 (primary + 配置声明的 fallback), 没配 fallback 时
	// 只给 primary 一档、冒烟照旧拒绝 —— 那是正确行为。
	if f := agent.EvoTierFactory(apiClient); f != nil {
		builtin.SetEvoTierFactory(f)
	}
	mcp.RegisterMCPTools(reg, mcpConns)
	if skillReg.Count() > 0 {
		reg.Register(skills.NewSkillTool(skillReg))
	}

	var runAgent agent.RunAgentFunc
	runAgent = func(ctx context.Context, prompt string, opts agent.RunOptions) (string, error) {
		return runNestedAgent(ctx, deps, runAgent, prompt, opts)
	}
	// 图外派生可见性 (design/01 §4.8): TraceStore 在下面才构造得出来 (要先算 stateDir),
	// 故留引用, 见 `agentTool.SetTraceStore(ts)`。
	agentTool := agent.NewAgentTool(runAgent)
	reg.Register(agentTool)
	// delegate_task 子代理分解工具 (Path B): 规划方把工具密集/可并行的子任务拆给
	// 在快速执行模型上运行的独立子代理, 报告回主模型综合。模型固定取
	// tctx.ExecutionModel, 见 pkg/agent/delegate_tool.go。cap=80 为子代理回合数兜底。
	delegateTool := agent.NewDelegateTool(runAgent, 80)
	reg.Register(delegateTool)

	// Advisor 顾问工具 (设计文档 docs/advisor-tool-design.md)
	// 别名优先级: --advisor flag > config advisor 段; "--advisor off" 强制关闭。
	if advisorClient, advisorOpts, ok := buildAdvisorClient(jsonCfg, projectSettings); ok {
		advTool := builtin.NewAdvisorTool(advisorClient, advisorOpts)
		reg.Register(advTool)
		// AdvisorConsultFn 供 AutoModeHook 自动咨询 + AdvisorCheckpointHook 周期咨询共用。
		// 只要 advisor 已装配就接上; 周期触发 (checkpoint) 与 AutoAdvisor 各按自己的开关生效。
		cfg.AdvisorConsultFn = advTool.Consult
		cfg.AutoAdvisor = autoAdvisor // 复杂任务自动咨询 advisor 并注入建议
		if adv := advisorSectionFromConfig(jsonCfg, projectSettings); adv != nil && (adv.CheckpointEveryTurns > 0 || adv.CheckpointOnLoop) {
			cfg.AdvisorCheckpointEveryTurns = adv.CheckpointEveryTurns
			cfg.AdvisorCheckpointOnLoop = adv.CheckpointOnLoop
		}
	}

	// 13.7-P0 统一池 (§13.7.2「注册不懒、广告懒」): env 开关默认关, 开启后
	// 自训练/动态资产 (evo_*/Media/CodeIntel) 下沉池内、广告面只发包装三件+已加载面。
	// 开关解析与 WeakModel 同模式 (显式 > 默认关); 治理集与 toolExposed 同源拷贝。
	if tool.PoolEnabled(os.Getenv) {
		// 13.7-P1 两段式注入: 装配点早于 stateDir/ts 创建 (下方 :3569/:3621), 先建
		// 空 policy 挂上包装三件, ts 就绪后再 SetStateDir/SetTraceStore 补齐
		// (仿 agentTool.SetTraceStore 先例)。CLI 路径无团队语义 (Team/Role 空),
		// 个性化不表达, 但留痕 Span 与 α>0 手动实验 (CLAUDE_GO_POOL_ALPHA) 均可用。
		pp := builtin.NewPoolPolicy("", nil)
		// 资产索引用**进程级**的那一份 (loadAssetCatalog 装入) —— 与 BindAgentAsset
		// 的解析路径同源。已装则 hook 资产一并进池 (21.8.12 P3); 未装 (nil) 时
		// 池里不收 hook, 与改造前逐字节一致。
		pool := builtin.RegisterPoolTools(reg, skillReg, agent.AgentCatalog(), cfg.DisabledTools, cfg.AllowedTools, pp)
		// 13.7-P2 L1 配给: 描述位按 bandit 后验配给 (CLI 无团队语义, 空键后验)。
		// SkillListing 是启动快照, 冷启动后验要等下方 SetStateDir 回灌完才可见 ——
		// 那里再重渲染一次 (系统提示词逐轮重建, 读的是同一字符串)。
		skillReg.SetRanker(pp.RankScore)
		log.Printf("[toolpool] 统一池已启用: 池内 %d 项下沉 (evo_*/Media/CodeIntel/hook), 广告面=基础+包装三件+已加载",
			len(pool.MemberNames()))
		cliPoolPolicy = pp // 存活到 TraceStore 创建后补注入
	}

	eng := engine.NewQueryEngine(cfg, apiClient, reg, hookRunner, permChecker, compactor, promptMgr)

	// H3-2 中间态通电。团队 stage 的引擎在 cliAgentRunner.Execute 里**复用这一个**
	// eng (见 agentFactory), 所以这里装配一次就覆盖了整条 CLI 团队路径。
	eng.SetRunStateDir(runStateDir)

	// Path B 子代理分解引导: 配置了执行模型时, 提示规划方把工具密集/可并行子任务
	// 拆给 delegate_task 在快速执行模型上跑, 报告由主模型综合。TaskInstruction 由
	// engine 每回合追加到 system prompt 尾部 (engine.go Phase 2); 子代理新建引擎
	// 无此引导 (它们是被委派的执行者)。
	if cfg.ExecutionModel != "" {
		eng.TaskInstruction = fmt.Sprintf(`
## 子代理分解
你运行在主模型 %s, 本机配置了快速执行模型 %s。
遇到工具密集/重复性/可并行的子任务时, 用 %s 工具拆给在 %s 上运行的独立子代理执行:
- 子代理有全新上下文、自带工具循环, 但看不到本对话历史 → 子任务描述必须自包含 (目标+约束+验收标准)。
- 相互独立的子任务放进 tasks 数组一次并行委派。
- 委派只做执行, 综合判断与最终回答由你完成。简单任务不要委派。`,
			cfg.Model, cfg.ExecutionModel, tool.DelegateToolName, cfg.ExecutionModel)
	}

	// V3 Anti-Amnesia: CLI 模式统一接入记忆系统
	stateDirInput := ""
	if jsonCfg != nil {
		stateDirInput = jsonCfg.StateDir
	}
	stateDir := basedir.ResolveDefault(stateDirInput, cwd)
	memDir := filepath.Join(stateDir, "memory")
	_ = os.MkdirAll(memDir, 0755)

	// L1: TieredStore (情景记忆)
	tieredStore := memory.NewTieredStoreWithPersist(memDir)
	// 第七章缺口修复: 记忆检索向量化可选后端 (默认关=纯 BM25 现状;
	// CLAUDE_GO_MEMORY_EMBEDDER=ollama:bge-m3 开启 BM25+余弦混合检索)。
	if emb := memory.EmbedderFromEnv(os.Getenv); emb != nil {
		tieredStore.SetEmbedder(emb)
		log.Printf("[memory] 向量化混合检索已启用 (embedder=%s)", os.Getenv("CLAUDE_GO_MEMORY_EMBEDDER"))
	}
	eng.MemoryStore = tieredStore

	// 按需取回两件套 (memory_search / memory_read)。注册在这里而不是 RegisterBaseTools
	// 那一段: 那时 tieredStore 还没建出来 (要先解析 stateDir 才知道记忆目录), 拿不到
	// 句柄就只能注册两个永远报"不可用"的空工具。工具必须指向**活的** store ——
	// 记忆在运行期一直在增长, 构造时快照一份绕不开这个顺序问题。
	builtin.RegisterMemoryTools(reg, tieredStore)

	// L2: FactStore (结构化记忆)
	factStore := memory.NewFactStore(memDir)
	eng.FactStore = factStore

	// Ingestor (自动摄入)
	ingestor := memory.NewIngestor(factStore, tieredStore)
	eng.Ingestor = ingestor

	// Dreaming (记忆整理)
	dreamCfg := dreaming.DefaultDreamConfig()
	dreamCfg.MemoryDir = filepath.Join(cwd, ".claude", "memory")
	dreamer := dreaming.NewDreamer(dreamCfg, cwd)
	dreamer.SetAPIClient(apiClient)
	cliDreamer = dreamer // 供 run 团队路径接入 TeamManagerConfig.Dreamer (headless dreaming 触发)

	// V3: 增强整合器
	consolidator := dreaming.NewConsolidator(factStore, apiClient, nil)
	dreamer.SetConsolidator(consolidator)

	// Dreaming 记忆目录注入 PromptMgr
	promptMgr.DreamMemoryDir = dreamer.Stats().MemoryDir

	// Decay Manager: 启动时执行一次衰减周期
	decayMgr := memory.NewDecayManager(factStore)
	go decayMgr.RunCycle()

	// PreCompact 蒸馏接入
	compactor.SetPreCompactFn(func(facts []string, source string) {
		ingestor.IngestFacts(facts, source)
	})

	// TraceStore 轨迹底座 (design/03 §4.1 E1): 落 <state>/statestore/, 采集 turn/tool_call Span。
	// 采样率由 CLAUDE_GO_TRACE_SAMPLE 控制 (默认全采); TTL 由 CLAUDE_GO_TRACE_TTL
	// 控制 (默认不清理, 保持既有语义)。此前两者都是"有能力无调用方"。
	ss := storedual.OpenForStateDir(filepath.Join(stateDir, "statestore"), os.Getenv) // 双写: 文件+agentDB, 读源 CLAUDE_GO_STATESTORE_READ
	ts := tracestore.New(ss)
	eng.TraceStore = ts
	// 图外派生 (Agent 工具 → runNestedAgent 的裸 QueryEngine) 的可见性两件套
	// (design/01 §4.8): ① 派生本身写 subagent Span; ② 子代理引擎自己的 llm_call
	// Span —— 改造前 nested 引擎的 TraceStore 恒 nil, 子代理烧的 token 在轨迹上无痕。
	agentTool.SetTraceStore(ts)
	delegateTool.SetTraceStore(ts)
	deps.traceStore = ts
	// 13.7-P1 池 policy 后置注入第二段: stateDir/ts 此刻已就绪, 补齐 bandit 持久化
	// (<state>/evolution/poolbandit.json + trace 回灌) 与选择留痕 Span (两段式, 见
	// 上方池装配点)。
	if cliPoolPolicy != nil {
		cliPoolPolicy.SetStateDir(stateDir)
		cliPoolPolicy.SetTraceStore(ts)
		// 13.7-P2 L1 配给第二段: coldStart 回灌后后验可见, SkillListing 重渲染
		// (启动时快照 + 逐轮重建读同一字段, 引擎下轮即用新序)。
		promptMgr.SkillListing = skillReg.FormatShortListingForDir(0, cwd)
	}
	if ttl := tracestore.TTLFromEnv(); ttl > 0 {
		// 每小时扫一次足够: trace 文件按 run 落, 清理粒度是"整个文件过期"。
		tracestore.StartJanitor(context.Background(), filepath.Join(stateDir, "statestore", "log"), ttl, time.Hour)
	}

	// 13.6-P1 F4: 技能目录热刷新 (长驻 REPL 期间增删改 SKILL.md 即时生效)。
	// 清单/快照型消费方 (promptMgr.SkillListing) 不在此刷新 —— 下轮 runIsolated
	// 的引擎各自现读注册表; 进程退出随进程结束, 无需显式 Stop。失败降级不阻塞。
	skillWatcher := skills.NewDirWatcher(skillReg)
	if err := skillWatcher.Start(); err != nil {
		log.Printf("[Skills] 热刷新未启用 (%v), 手动 /skill reload 仍可用", err)
	}

	// 组件是在 NewQueryEngine **之后**赋的, 必须重建一次 HookChain。
	// 带 nil 守卫的 hook (MemoryInject 要 MemoryStore/FactStore、TraceCapture 要
	// TraceStore) 在构造时这些字段还是 nil, 不补这一次就静默缺席——而下游平台
	// 的全部流量走的正是本路径 (CLI headless)。见 design/03 §4.1/§4.4 标注与
	// pkg/engine/hook_registration_order_test.go。
	eng.RefreshHooks()

	// 前沿优化组件的装配 —— **必须在 RefreshHooks 之后**。
	//
	// 这是修掉 "CLI 路径六个组件为 nil" 的那一半。改造前 EnableFrontierOptimizations
	// 只在飞书路径 (pkg/feishu/session.go) 被调用, 于是下游平台走的 CLI headless 下:
	// error_classifier / json_repair / loop_detector / prompt_cache / budget_degrade
	// 的组件字段全是 nil, 对应的内置 hook **从未注册**, 而 `--output-format json` 的
	// 响应与退出码一切正常 —— 看板上"配置已下发", 实际装配里那一半是空的。
	//
	// 范围刻意与飞书**主会话**路径 (session.go:923) 逐字对齐: 只调
	// EnableFrontierOptimizations, 不额外开 EnableStopSignal —— 后者是实验性的
	// CaRT 停止信号, 连飞书侧也只有团队 agent 路径显式打开 (session.go:1550)。
	// 借这次修复顺手把它也开了, 是搭便车的行为变更, 不是接线修复。
	//
	// 顺序要紧: applyFeatureFlags 会懒初始化组件, 而注册这些组件的 hook 认的是**构造
	// 时**的字段状态。先 RefreshHooks 再开开关的话, 那批 hook 也已经错过了这一趟 ——
	// 所以下面还要再 RefreshHooks 一次 (幂等, 重建 HookChain)。
	//
	// 开关读 config 的同名字段 (EngineSection.EnableFrontierOptimizations), 缺省 true
	// —— 与飞书路径的默认值一致。默认开是因为这几个组件都是**护栏性质**的: 错误分类、
	// JSON 修复、循环检测、前缀缓存、预算降级。关掉它们不会让 Agent 更聪明, 只会让它
	// 更不受约束。要省成本应当调预算, 不该拆护栏。
	if jsonCfg == nil || jsonCfg.Engine == nil || jsonCfg.Engine.EnableFrontierOptimizations == nil ||
		*jsonCfg.Engine.EnableFrontierOptimizations {
		// CaRT 停止信号不在一键包里 (实验性), 此前只有飞书团队路径开、CLI 永不开。
		// 现在由配置显式声明: 平台想让岗位带上它就置 true, 缺省仍按 CLI 原行为 (不开)
		// —— 加这个开关不改变任何现网默认值, 只是把"哪条路径开"从代码里的分支
		// 变成配置里的一句声明 (方案 20.6.5 项 2: 修掉两条路径的装配不对称)。
		if jsonCfg != nil && jsonCfg.Engine != nil && jsonCfg.Engine.EnableStopSignal != nil {
			eng.Config.EnableStopSignal = *jsonCfg.Engine.EnableStopSignal
		}
		// 必须在 EnableFrontierOptimizations 之前设好: 该函数内部的 applyFeatureFlags
		// 按构造时的字段状态懒初始化组件, 事后再改开关组件不会被补建。
		eng.EnableFrontierOptimizations()
		eng.RefreshHooks()
		log.Printf("[assembly] 前沿优化组件已装配 (CLI 路径, stop_signal=%v); 实际内置 hook: %d 个相位有注册",
			eng.Config.EnableStopSignal, len(eng.InternalHookPhases()))
	} else {
		log.Printf("[assembly] 前沿优化组件被配置显式关闭 (enableFrontierOptimizations=false): " +
			"错误分类 / JSON 修复 / 循环检测 / 前缀缓存 / 预算降级 均不启用")
	}

	// 装配自报 (方案 20.6.4 D0.7): 在**装配收尾点**采集, 且必须在上面那次
	// EnableFrontierOptimizations + RefreshHooks **之后** —— 报告的职责是照出
	// "实际装了什么", 提前采会把刚补上的那六个组件报成缺席, 那是自报造假。
	//
	// 采集只读引擎字段, 不改任何开关 (见 assembly_report.go 的包注释)。
	cliAssembly = engine.CollectAssemblyReport(eng, engine.AssemblyReportInput{
		DeclarationVersion: declarationVersionOf(jsonCfg),
		Config: &assembly.ConfigSource{
			Path:   jsonConfigPath,
			Origin: configOrigin,
			Found:  jsonConfigPath != "",
		},
		StateDir: stateDir,
	})
	if cliAssembly != nil {
		visible, registered := 0, 0
		if cliAssembly.Tools != nil {
			visible, registered = len(cliAssembly.Tools.Visible), cliAssembly.Tools.Registered
		}
		log.Printf("[assembly] 装配自报已采集: fingerprint=%s config=%s 工具可见 %d/%d 内置 hook %d 相位 外部 hook %d 条",
			cliAssembly.Fingerprint(), configOrigin,
			visible, registered,
			len(cliAssembly.InternalHooks), len(cliAssembly.ExternalHooks))
		for _, w := range cliAssembly.Warnings {
			log.Printf("[assembly][warn] %s", w)
		}

		// 自报之后立刻对账 (方案 20.6.4 的顺序: 先自报, 再对账, 最后才是面板)。
		// 对账只比"声明 vs 实际", 不产生新的观测 —— 所以它必须紧跟采集, 且拿的是
		// 同一份刚采下来的报告, 而不是回头再采一次 (再采一次就可能与面板看到的不是
		// 同一份, 那正是 20.6 要消灭的错位)。
		//
		// 声明侧只认平台的装配描述符 (20.6.3)。没有描述符就如实报"未对账" ——
		// 而不是退回"用配置推断一份声明": 那样得到的是一份**恒等**的对账 (配置与
		// 它自己比), 看起来全绿, 而真正要照的那条缝恰好被盖住了。
		if decl := declarationFromProfile(jsonCfg); decl != nil {
			cliAssemblyReconcile = assembly.NewReconcileResult(decl, cliAssembly)
			if len(cliAssemblyReconcile.Diffs) > 0 {
				log.Printf("[assembly] 对账 (描述符版本 %s): %s",
					jsonCfg.AssemblyProfile.Version, cliAssemblyReconcile.Summary)
				for _, diff := range cliAssemblyReconcile.Diffs {
					log.Printf("[assembly][diff] %s %s/%s 声明=%q 实际=%q",
						diff.Kind, diff.Facet, diff.Item, diff.Declared, diff.Actual)
				}
			} else {
				log.Printf("[assembly] 对账 (描述符版本 %s): %s",
					jsonCfg.AssemblyProfile.Version, cliAssemblyReconcile.Summary)
			}
		} else {
			log.Printf("[assembly] 未对账: 本次配置没有 assemblyProfile (装配描述符); " +
				"声明侧只能由平台签发, harness 不自行推断")
		}
	}

	return eng, nil
}

func getAPIKey() string {
	if flagAPIKey != "" {
		return flagAPIKey
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key
	}
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return key
	}
	return ""
}

func checkCommand(name, desc string) {
	_, err := exec.LookPath(name)
	if err == nil {
		fmt.Printf("✓ %s: 已安装\n", desc)
	} else {
		fmt.Printf("✗ %s: 未安装\n", desc)
	}
}

func printMessage(msg types.Message) {
	switch msg.Type {
	case types.MessageTypeAssistant:
		for _, block := range msg.Content {
			switch block.Type {
			case types.ContentBlockText:
				fmt.Print(block.Text)
			case types.ContentBlockToolUse:
				if flagDebug {
					fmt.Printf("\n[调用工具: %s]\n", block.Name)
					if len(block.Input) > 0 {
						var pretty json.RawMessage
						if json.Unmarshal(block.Input, &pretty) == nil {
							out, _ := json.MarshalIndent(pretty, "  ", "  ")
							fmt.Printf("  输入: %s\n", string(out))
						}
					}
				}
			}
		}
		fmt.Println()
	case types.MessageTypeUser:
		for _, block := range msg.Content {
			if block.Type == types.ContentBlockToolResult {
				if flagDebug {
					status := "✓"
					if block.IsError {
						status = "✗"
					}
					content := block.Content
					if len(content) > 200 {
						content = content[:200] + "..."
					}
					fmt.Printf("[%s 工具结果] %s\n", status, content)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// --agent: 让"一个 agent"成为可运行的实体 (能力中心调试台的后端)
// ---------------------------------------------------------------------------

// agentRunSpec 是"以某个 agent 身份跑一次"所需的全部参数, 已从资产层/角色层解析完毕。
//
// 它是**只读读数** (解析结果), 不含任何运行时对象 —— 同一份 spec 可以被打印、
// 落库、喂给 runner, 三条路看到的是同一个东西。把 eng/client 之类塞进来会让
// "这台机器上会怎么跑"与"声明说要怎么跑"再次分成两个真相源。
type agentRunSpec struct {
	// Name 请求的名字 (原样保留, 即使只命中了一层)。
	Name string
	// Source 命中哪一层: "asset" | "role"。UI 要如实显示 —— 这两层的优先级、
	// 可覆盖字段、失效方式都不同, 合并成一个"已命中"会让用户以为自己在改资产。
	Source string
	// SystemPrompt 该 agent 的人格正文 (含其专属 skills 内容)。
	SystemPrompt string
	// Model 声明的模型 (空 = 跟随外层/全局默认)。
	Model string
	// MaxTurns 声明的回合上限 (0 = 跟随外层长程档)。
	MaxTurns int
	// PermissionMode 声明的权限档 (空 = 不覆盖)。
	PermissionMode string
	// ToolProfile / HasToolProfile 声明的工具档位。
	//
	// 与 AssetBinding 一样必须**配对读**: 空档位与"没声明档位"是两回事。
	ToolProfile    builtin.ToolProfile
	HasToolProfile bool
	// Tools / HasTools 显式白名单。
	//
	// ★ HasTools 这一位不能省。`tools: []` 是**显式空白名单** (fail-closed, 拒绝
	// 一切工具), 它解析出来的切片与"根本没写 tools:"一样长都是 0。少了存在性这一位,
	// 一份 fail-closed 声明会在下游静默退化成完全放行 —— 见
	// AssetBinding.AssemblyDeclaration 里同一段论证。
	Tools    []string
	HasTools bool
	// DisallowedTools / HasDisallowedTools 显式黑名单。
	DisallowedTools    []string
	HasDisallowedTools bool
	// Skills 声明解析出的技能名 (资产层已与技能池对过账)。
	Skills []string
	// Hooks 声明解析出的外部 hook。
	Hooks []types.HookConfig
	// Plugins 声明依赖的插件包名 (frontmatter `plugins:`), 由 runAsAgent 在解析后
	// 展开并与 --plugin 旗标去重合并 (pkg/pluginrun)。
	Plugins []string
}

// resolveAgentRunSpec 按名字解析出运行规格。
//
// 查表顺序 (高 → 低):
//
//  1. agent 资产 (asset.Meta, 经 agentbind.BindAgentAsset) —— 它是"这个 agent"最强的
//     身份声明: 有正文、有档位、有权限档, 且可被下游按目录覆盖。
//  2. 角色注册表 (pkg/agent.RoleRegistry) —— 93 个内置人格, 只有正文与技能。
//
// 两层都没命中就**报错**。绝不静默回落成"普通 run":
// 回落会让一个拼错的 agent 名与一个正确的 agent 名从外面**完全不可区分** ——
// 两种情况都是"跑起来了, 输出了一段文本", 而唯一能区分它们的证据被那条回落吃掉了。
// 与 21.8.12 里"档位写错静默掰成 chat"是同一类缺陷, 不在这里重犯。
func resolveAgentRunSpec(name string, roles *agent.RoleRegistry, skillReg *skills.Registry) (*agentRunSpec, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("--agent 的名字为空")
	}

	spec := &agentRunSpec{Name: name}

	// 1) 资产层。
	if b := cliBindAgentAsset(name, skillReg); b != nil && b.Asset != nil {
		if !b.Active {
			// 影子/归档资产一律不装配 —— 与 P5 的查表口径一致 (清单里没有它,
			// 运行时也不该能被激活; 两句话必须是同一句)。
			return nil, fmt.Errorf("agent 资产 %q 不是 active 状态 (shadow/archived), 不能作为运行身份", name)
		}
		spec.Source = "asset"
		spec.SystemPrompt = b.Prompt
		spec.Model = b.Model
		spec.MaxTurns = b.MaxTurns
		if b.HasPermissionMode {
			spec.PermissionMode = string(b.PermissionMode)
		}
		spec.ToolProfile = b.ToolProfile
		spec.HasToolProfile = b.HasToolProfile
		spec.Tools = append([]string(nil), b.Tools...)
		spec.HasTools = b.HasTools
		spec.DisallowedTools = append([]string(nil), b.DisallowedTools...)
		spec.HasDisallowedTools = b.HasDisallowedTools
		spec.Skills = append([]string(nil), b.Skills...)
		spec.Hooks = b.Hooks
		// 插件依赖原样带上 —— 展开发生在 runAsAgent (那时引擎与目录都已就绪),
		// 这里只负责"声明了哪些名字"。
		spec.Plugins = append([]string(nil), b.Asset.MetaList("plugins")...)
		return spec, nil
	}

	// 2) 角色注册表。
	if roles != nil {
		if rd := roles.Get(name); rd != nil {
			spec.Source = "role"
			// MergedPrompt 的 objective/prevResult 传空: 这里的 prompt 是**人格底座**,
			// 任务正文由调用方作为 user 消息传 (与 executeStage 把 objective 作为
			// 第三参传进来同构)。在解析层就把任务拼进系统提示词, 会让同一份 spec
			// 与某一次具体的运行绑死, 而这正是"运行时配置"要解耦的东西。
			spec.SystemPrompt = roles.MergedPrompt(rd.Name, "", "")
			spec.Skills = append([]string(nil), rd.BuiltinSkills...)
			return spec, nil
		}
	}

	// 两层都空: 把"查过哪两层"写进错误里 —— 只报"没找到"会让人以为只有一层可查。
	hint := ""
	if roles != nil {
		if names := roles.Names(); len(names) > 0 {
			hint = fmt.Sprintf(" (角色注册表里有 %d 个角色可用, 例: %s)", len(names), strings.Join(firstN(names, 3), ", "))
		}
	}
	return nil, fmt.Errorf("找不到 agent %q: 既不是 agent 资产, 也不在角色注册表里%s", name, hint)
}

// firstN 返回切片前 n 项 (不足则全给)。仅用于把候选写进错误信息。
func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// agentTurnCap 摘出"这次跑的回合上限声明"。
//
// ★ 显式旗标 --max-turns 优先于 agent 自身的声明。这条优先级是必须的, 不是口味:
//
//	实测在 --agent 路径上, --max-turns 2 / 5 / 不传 三种跑法发出的请求数**完全相同**
//	(都是 151 = 150 轮兜底 + 1 次收尾), 也就是说缺了这一步时, 旗标在这条路径上
//	是个只显示不生效的字段 —— 而调试台上它恰恰是用户最主要的调参手段。
//	与 run 路径的 effectiveMaxTurns 同一条"flag 覆盖配置"纪律。
//
//	思考型角色的 1 轮短路仍在最前面 (由 cliStageMaxTurns 保证): 那条短路同时关掉
//	工具循环, 绕开它会让 architect 这类角色拿到一个开着工具的多轮循环。
func agentTurnCap(spec *agentRunSpec, longHorizonTurns int) int {
	if flagMaxTurns > 0 {
		return flagMaxTurns
	}
	return spec.MaxTurns
}

// buildAgentRunner 把一份解析好的 agent 规格装成一个可执行的 runner。
//
// 直接构造包级的 cliAgentRunner, **不绕 agentFactory 闭包** —— 那个闭包被限在
// runCmd 的 `/` 前缀分支内部 (它依赖 commands.Registry / swarmintel / 持久化任务库
// 那一整套斜杠编排设施), 而 `--agent` 恰恰是"不要编排、只要一个 agent"。把
// agentFactory 提到分支外面会让一次普通 run 也去构造整套编排设施。
//
// 复用 cliAgentRunner 而不是新写一个 runner, 是为了让两条路径共用同一段执行语义:
// 回合上限的判定链 (cliStageMaxTurns)、思考型角色的关工具规则、阶段技能的并入方式、
// 中途注入队列 —— 各写一份的结果是"同一个 agent 在两条路径上跑出不同轮数/不同工具面",
// 而那种差异从任何单侧看都是不可见的。
func buildAgentRunner(eng *engine.QueryEngine, spec *agentRunSpec, mcfg modelconfig.ResolvedConfig, longHorizonTurns int) *cliAgentRunner {
	r := &cliAgentRunner{
		eng:          eng,
		role:         spec.Name,
		systemPrompt: spec.SystemPrompt,
		modelCfg:     mcfg,
		// stage 用 agent 名: 它进 trace span 的 Stage 字段与日志前缀, 是"这次跑的是谁"
		// 在产物上唯一的落点。留空会让中间态退化成按 Role+Cwd 分账 (见 withStage)。
		stage:               spec.Name,
		longHorizonMaxTurns: longHorizonTurns,
	}
	if cap := agentTurnCap(spec, longHorizonTurns); cap > 0 {
		// 回合上限接进**既有**判定链: cliStageMaxTurns(role, modelTurns, longTurns)
		// 里 modelTurns 的语义就是"这次跑的回合上限", 且它是最优先的一档
		// (思考型角色的 1 轮短路仍在最前面)。不在这里另加一条分支 —— 另加就会与它
		// 分叉成"同一个 agent 在两条路径上跑不同轮数"。
		//
		// 取值走 agentTurnCap (旗标 > agent 声明), 与 runAsAgent 印日志时用的是
		// 同一个函数: 两处各写一遍的话, 日志会印一个数、实际跑另一个数。
		r.modelCfg.MaxTurns = cap
	}
	// 资产声明的技能交给既有渲染路径 (stageDeclSkillListing → FormatShortListingForNames)。
	// 只对资产层设: 角色层的 MergedPrompt **已经把** Skills/BuiltinSkills 的正文拼进
	// 人格里了 (见 RoleRegistry.MergedPrompt), 再设一遍等于同一份技能在提示词里出现两次。
	if spec.Source == "asset" {
		r.stageDecl.Skills = spec.Skills
	}
	// 资产 hook 已是解析结果 (types.HookConfig), 不能塞进 stageDecl.Hooks —— 那个字段
	// 的契约是**声明原文** (<event>:<type>:<target>[:<if>]), 交给 hooksFromDecl 再解析
	// 一次会把一个合法的 HookConfig 当成拼错的声明丢掉。故单开一个字段原样带上。
	r.declHooks = spec.Hooks
	return r
}

// engineSkillRegistry 从引擎身上取当前真正在服务这次调用的技能注册表。
//
// 路径与 cliAgentRunner.stageDeclSkillListing **完全一致** (从 Skill 工具自己身上拿,
// 而不是包级变量): 引擎里那个 SkillTool 是装配时按本次运行的工具面注册的, 只有它
// 知道"哪一份技能库在服务这次调用"。用另一份去解析资产声明的技能名, 会让
// "资产里 skills: 写了但没生效"与"写了、生效了"从外面分不出来。
//
// 取不到时返回 nil —— 调制解调器会把它当成"这条路径上没有技能池可查"并如实留痕
// (AssetBinding.bindSkills 的 SkillPool==nil 分支), 不是静默当作"没有技能"。
func engineSkillRegistry(eng *engine.QueryEngine) *skills.Registry {
	if eng == nil || eng.Tools == nil {
		return nil
	}
	raw, ok := eng.Tools.Get("Skill")
	if !ok {
		return nil
	}
	st, ok := raw.(*skills.SkillTool)
	if !ok || st.Registry() == nil {
		return nil
	}
	return st.Registry()
}

// modelCfgForAgentRun 解析本次 agent 运行要用的模型配置。
//
// 返回零值表示"跟随引擎默认客户端" (cliAgentRunner.Execute: modelCfg.ProviderName
// 为空时不克隆, 直接用 eng.APIClient) —— agent 没声明模型时的正确行为是跟随外层,
// 而不是在这里替它挑一个。
func modelCfgForAgentRun(spec *agentRunSpec) (modelconfig.ResolvedConfig, error) {
	name := strings.TrimSpace(spec.Model)
	if name == "" {
		return modelconfig.ResolvedConfig{}, nil
	}
	jsonCfg, _, err := loadRuntimeJSONConfig()
	if err != nil {
		return modelconfig.ResolvedConfig{}, fmt.Errorf("agent %q 声明了模型 %q, 但读配置失败: %w", spec.Name, name, err)
	}
	mcfg, ok, err := resolveRuntimeModelConfig(jsonCfg, name)
	if err != nil {
		return modelconfig.ResolvedConfig{}, fmt.Errorf("agent %q 声明的模型 %q 解析失败: %w", spec.Name, name, err)
	}
	if !ok {
		// 解析不出来时**报错**而不是回落: 一个写错的模型名与一个正确的模型名,
		// 在"跑起来了, 输出了一段文本"这个读数上完全同形 —— 回落会把唯一的证据吃掉。
		return modelconfig.ResolvedConfig{}, fmt.Errorf("agent %q 声明的模型 %q 在 providers 配置里解析不出结果", spec.Name, name)
	}
	return mcfg, nil
}

// runAsAgent 是 `--agent` 的整条执行路径: 解析 → 应用工具面 → 跑一次 → 出结果。
//
// 独立成函数而不是内联在 runCmd 的 RunE 里, 是因为它要**自己**建会话存储与 trace
// 上下文: runCmd 里那一段 (session.NewSessionStore + trace.With(RunID)) 排在斜杠
// 分支**之后**, 而 --agent 在它之前就 return 了。不自己建的话, 本次运行的 span
// 会因为没有 RunID 落进 trace-orphan 桶 (tracestore.bucketName("")), 而轨迹页只列
// trace-run-* —— 那正是这个调试台首先要解决的"看不见"。
func runAsAgent(cmd *cobra.Command, eng *engine.QueryEngine, userPrompt string, longHorizonTurns int) error {
	cwd := eng.Config.Cwd
	// stateDir 的算法与斜杠分支**逐字一致** (jsonCfg.StateDir 优先, 否则按 cwd 推)。
	//
	// 传空串会少掉一个资产根 (state) —— 而部署里 agent 资产正是放在 stateDir 下的
	// (/mnt/data/claude-go/agents)。少这一个根的表现是"资产明明在磁盘上, 却查不到",
	// 于是所有 --agent 都会静默退到角色层, 从外面完全看不出资产层从来没被查过。
	stateDirInput := ""
	if jsonCfg, _, cfgErr := loadRuntimeJSONConfig(); cfgErr == nil && jsonCfg != nil {
		stateDirInput = jsonCfg.StateDir
	}
	stateDir := basedir.ResolveDefault(stateDirInput, cwd)

	// 资产目录: 必须在 resolveAgentRunSpec **之前**装载 —— 它读的是包里那份全局索引
	// (agent.LookupAgentAsset), 没装载过时资产层恒为空, 于是所有资产名都会"查不到"
	// 而静默退到角色层, 表现是"资产总是没命中"。runCmd 的另一处 (斜杠分支) 也调它。
	log.Printf("[agent] 资产装载: %s", loadAssetCatalog(cwd, stateDir))
	// 上面那行**重建**了进程级目录: buildEngine 展开 --plugin 时合并进去的插件
	// agents 随旧目录一起被换掉, 这里把同一批 agents 重新合并进新目录的副本
	// (仍是复制合并, 共享目录不被改污)。技能与 hook 已装在同一个引擎上, 不重复装。
	if cliPlugins != nil {
		mergePluginAgents(agent.AgentCatalog(), cliPlugins.bundles)
	}

	rr := agent.NewRoleRegistryFor(cwd, stateDir)
	spec, err := resolveAgentRunSpec(flagAgent, rr, engineSkillRegistry(eng))
	if err != nil {
		return err
	}
	// 显式 --system-prompt 覆盖 agent 自身的人格 —— 与 run 路径同一条纪律
	// (那里是 flagSystemPrompt → promptMgr.CustomPrompt, 也是整段替换)。
	//
	// ★ 实测: 不加这一步时, `--agent researcher --system-prompt <标记>` 发出的请求里
	//   system 字段**只有 researcher 的人格**, 标记一个字都没有 —— 而同一个旗标不带
	//   --agent 时是生效的 (对照实测 system 长度 19 = 只有标记)。也就是说这是个
	//   只显示不生效的字段。调试台上"覆盖 system prompt"是最主要的试验手段之一:
	//   用户改一版提示词、跑一次、看产出变没变 —— 而这条路径会让他每次都看到**同一个**
	//   结果, 从而得出"提示词对行为没影响"这个完全错误的结论。
	if strings.TrimSpace(flagSystemPrompt) != "" {
		spec.SystemPrompt = flagSystemPrompt
	}
	// agent 资产 frontmatter `plugins:` 声明的插件: 与 --plugin 旗标去重后展开。
	// 引擎已在 buildEngine 建好, 三条成员各走自己的接缝: agents 仍合并进目录副本,
	// skills 走引擎里真正服役的那份注册表 (engineSkillRegistry —— 包级变量可能是
	// 另一份), hooks 走 Runner 的可装卸组。找不到的插件名直接报错退出, 不静默跳过。
	if extra := pluginNamesDifference(spec.Plugins, flagPlugins); len(extra) > 0 {
		bundles, err := expandRunPlugins(agent.AgentCatalog(), extra)
		if err != nil {
			return err
		}
		mergePluginAgents(agent.AgentCatalog(), bundles)
		loadPluginSkillDirs(engineSkillRegistry(eng), bundles)
		installPluginHookGroups(eng.HookRunner, bundles)
	}
	if err := applyAgentToolFace(eng, spec); err != nil {
		return err
	}
	mcfg, err := modelCfgForAgentRun(spec)
	if err != nil {
		return err
	}

	// 会话存储 + trace 上下文 (与 runCmd 的默认路径同一套语义, 见那里的长注释)。
	ctx := trace.With(context.Background(), runTraceIDs)
	sessionID := ""
	runID := ""
	if store, storeErr := session.NewSessionStore(cwd); storeErr == nil {
		eng.SessionStore = store
		defer store.Close()
		sessionID = store.SessionID()
		runID = fmt.Sprintf("run-%s-%d", sessionID, runTraceSeq.Add(1))
		ctx = trace.With(ctx, trace.IDs{RunID: runID, SessionID: sessionID})
	}
	// store 拿不到时不发号 (与默认路径同一条守卫): 硬编一个 RunID 会造出"看着有归属、
	// 其实归属错了"的假记录。

	// run span 的挂点。与默认路径同为 defer —— 位置就是全部要害: 无论从哪条 return
	// 退出都会走一遍, 而那些提前 return 恰恰是最该被看见的失败运行。
	start := time.Now()
	var errKind string
	defer func() {
		name := strings.TrimSpace(flagRunLabel)
		if name == "" {
			name = spec.Name
		}
		status := tracestore.RunStatusOK
		if errKind != "" {
			status = tracestore.RunStatusFailed
		}
		recordRunSpan(eng.TraceStore, runID, name, eng.Config.Model, status, errKind, userPrompt, start)
	}()

	// 轨迹页要按 agent 名检索: 调用方没给 --run-label 时自动填, 与 coder 的
	// "coder:<任务名>" 同一套机制。显式给了就不覆盖 —— 调用方的标注更具体。
	if strings.TrimSpace(flagRunLabel) == "" {
		flagRunLabel = "agent:" + spec.Name
	}

	// 印的是**实际生效**的回合上限, 不是 agent 的声明值。
	//
	// ★ 这里曾经印 spec.MaxTurns, 那是个会说谎的读数: 一个 agent 没声明 maxTurns
	//   时它印 0, 而实际跑的是 150 轮 (longHorizonMaxTurnsOr 的兜底)。声明了 maxTurns
	//   而角色恰好是思考型时, 它印声明的值, 而实际是 1。调试台上"实际生效值"面板
	//   直接吃这一行, 印错了就是让用户拿着一个从没生效过的数字去调优。
	//   取值走与 Execute 同一个 cliStageMaxTurns —— 两处各算一遍迟早分叉。
	effectiveTurns := cliStageMaxTurns(spec.Name, agentTurnCap(spec, longHorizonTurns), longHorizonMaxTurnsOr(longHorizonTurns))

	log.Printf("[agent] 以 %q 运行 (来源: %s, 模型: %q, 回合上限: %d, 工具档位: %s)",
		spec.Name, spec.Source, spec.Model, effectiveTurns, spec.ToolProfile)
	// 印的是**实际发给模型的**工具数, 不是注册表里的工具总数。
	//
	// ★ 这一行与上面那行是同一类错误的两半, 一起修的: 原先无条件印
	//   len(eng.VisibleTools()) —— 而 Execute 里 disableTools(= 思考型角色) 会把
	//   apiTools 直接置空 (pkg/engine/runner.go:241), **一个工具描述都不发给模型**。
	//   实测: researcher 发出的请求体里 tools 字段**整个不存在**, 而这一行印 433。
	//   调试台的"实际生效值"面板直接吃这个数, 于是对一个根本没拿到工具的 agent
	//   显示"可见工具 433 个" —— 用户会据此以为工具是通的, 转而去查别的方向。
	//
	//   判据必须是驱动实际行为的那个谓词 IsThinkingRole, 与 Execute 里
	//   disableTools 用的是同一个函数 —— 这里再列一遍"哪些算思考型"迟早分叉。
	//   工具面的**档位**(spec.ToolProfile / --allowed-tools 白名单)是另一层,
	//   它改的是 VisibleTools() 的内容, 与这里的置空是叠加关系。
	toolsDisabled := modelconfig.IsThinkingRole(spec.Name)
	if toolsDisabled {
		log.Printf("[agent] 本次可见工具 0 个 (思考型角色 %q: 该角色关闭工具循环, 不向模型暴露任何工具)", spec.Name)
	} else {
		vis := eng.VisibleTools()
		log.Printf("[agent] 本次可见工具 %d 个: %s", len(vis), strings.Join(firstN(vis, 20), ", "))
	}

	runner := buildAgentRunner(eng, spec, mcfg, longHorizonTurns)
	final, runErr := runner.Execute(ctx, userPrompt)
	if runErr != nil {
		errKind = "agent_run_error"
		// 产物一并带回: RunIsolated 在出错时已经把收集到的文本给了, 丢掉它等于让
		// 调试台只能看到"失败了"而看不到失败前跑到了哪一步。
		if final != "" {
			log.Printf("[agent] 失败前的部分产出 (%d 字节): 见 --output-format json 的 final 字段", len(final))
		}
	}

	if flagOutputFormat == "json" {
		// 用默认路径同一个信封 —— 调试台按 final/is_error/error_kind/session_id
		// 四个字段解析, 两条路径的信封形状必须一致, 否则前端要写两套解析。
		if eerr := emitRunEnvelope(final, sessionID, errKind, nil, nil); eerr != nil {
			return eerr
		}
	} else if !flagFinalOnly {
		fmt.Println(final)
	}
	if flagEmitSession && sessionID != "" {
		fmt.Printf("\n__CLAUDE_GO_SESSION__=%s\n", sessionID)
	}
	// 出错时把错误原样返回: 退出码要非零。**但产物已经打出去了** —— 调试台需要
	// 两样都拿到 (is_error 为真 + final 有内容), 只给其一都不足以定位。
	if runErr != nil {
		return runErr
	}
	return nil
}

// applyAgentToolFace 把 agent 声明的工具面/权限档写进引擎, 并重建 hook 链。
//
// ★ 前提: 只在"构造引擎 → 跑一次 → 进程退出"的用法下安全。它改的是 eng.Config
// (进程级状态), 同一进程内第二次 run 会带着上一次的残留。v1 的 `--agent` 是单次
// 运行即退出, 所以成立; **长驻进程 (serve / dashboard / feishu) 里不要复用这个函数** ——
// 那样一个 agent 的工具面会漏到下一个请求上, 而漏的方向可能是放宽。
//
// RefreshHooks 是赋值语义 (registerInternalHooks 里 `e.HookChain = NewHookChain()`,
// 不是追加), 所以重复调用安全。不改工具面时**也必须调它**: ToolGateHook 是在那里面
// 按当前的 AllowedTools/DisabledTools 决定要不要注册的, 顺序反了就是"白名单写进去了
// 但那条闸门没通电", 表现是 agent 的工具面看起来生效了、实际全放行。
func applyAgentToolFace(eng *engine.QueryEngine, spec *agentRunSpec) error {
	if eng == nil || eng.Config == nil {
		return fmt.Errorf("引擎未就绪, 无法应用 agent %q 的工具面", spec.Name)
	}

	// 1) 档位 → 黑名单 (只削**内置**工具)。
	//
	// 用 DisabledTools 而不是 AllowedTools, 理由是这个档位的语义: profile 决定的是
	// "注册哪些内置工具", 而引擎的注册表里除内置工具外还有 Skill / 池工具 / 记忆工具 /
	// MCP —— 飞书那条路径也是先按档位注册、**再把这几类添上去**的。若改用白名单
	// (AllowedTools = 该档注册的内置工具名), 一个 `tool-profile: coding` 的 agent
	// 会连 Skill 工具一起丢掉, 与飞书路径同名的 agent 行为不一致。
	//
	// 求差而不是替换 DisabledTools: 配置里那份可能已经含弱模型掩码 (L4) 等既有禁用,
	// 覆盖会把它们悄悄放开。
	if spec.HasToolProfile {
		allowedByProfile := map[string]bool{}
		for _, n := range builtin.ToolNameCatalog(spec.ToolProfile) {
			allowedByProfile[n] = true
		}
		// 问 admin 档要"全部内置工具"当分母 —— 它等于 RegisterBaseTools 注册的集合,
		// 也就是本引擎写入 reg 的那一批 (见 buildEngine)。ToolNameCatalog 的注释
		// 明确要求判"是不是真工具"问 admin 档。
		for _, n := range builtin.ToolNameCatalog(builtin.ToolProfileAdmin) {
			if allowedByProfile[n] {
				continue
			}
			if eng.Config.DisabledTools == nil {
				eng.Config.DisabledTools = map[string]bool{}
			}
			eng.Config.DisabledTools[n] = true
		}
	}

	// 2) 显式白名单。
	//
	// ★ 与既有白名单**求交**, 不是替换。`--allowed-tools` 已经把引擎限死时, 替换会让
	// 一次受限会话被一个 `--agent` 悄悄放宽 —— 而这是调试入口, 恰恰最可能被喂来路
	// 不明的名字。求交是唯一fail-closed 的方向。
	if spec.HasTools {
		declared := make(map[string]bool, len(spec.Tools))
		for _, t := range spec.Tools {
			if t = strings.TrimSpace(t); t != "" {
				declared[t] = true
			}
		}
		// 注意 HasTools 为真但列表为空 = **显式空白名单** (拒绝一切)。上面那个循环
		// 留下的空 map 正是这个语义, 不要因为它"看起来是空的"就跳过分派。
		if eng.Config.AllowedTools != nil {
			for name := range eng.Config.AllowedTools {
				if !declared[name] {
					delete(eng.Config.AllowedTools, name)
				}
			}
		} else {
			eng.Config.AllowedTools = declared
		}
	}

	// 3) 显式黑名单。
	if spec.HasDisallowedTools {
		for _, t := range spec.DisallowedTools {
			if t = strings.TrimSpace(t); t != "" {
				if eng.Config.DisabledTools == nil {
					eng.Config.DisabledTools = map[string]bool{}
				}
				eng.Config.DisabledTools[t] = true
			}
		}
	}

	// 4) 权限档。两处都要改: Config.PermissionMode 是引擎自己读的 (Plan Mode 判定
	// 等), PermChecker.Mode 是工具执行路径读的。只改一处会让"引擎以为自己是什么档"
	// 与"闸门按什么档放行"分叉, 而那正是权限类缺陷最典型的形态。
	if spec.PermissionMode != "" {
		mode := types.PermissionMode(spec.PermissionMode)
		eng.Config.PermissionMode = mode
		if eng.PermChecker != nil {
			eng.PermChecker.Mode = mode
		}
	}

	eng.RefreshHooks()
	return nil
}

// cliAgentRunner 为 CLI run 模式提供的简单 AgentRunner。
type cliAgentRunner struct {
	eng          *engine.QueryEngine
	role         string
	systemPrompt string
	// stage 稳定阶段标识 (agent.RunMetadata.Stage), 供 H3-2 中间态按阶段分账。
	stage    string
	modelCfg modelconfig.ResolvedConfig // 为空则使用 eng.APIClient 默认
	// longHorizonMaxTurns 长程档的回合上限 (H4-3), 来自 ai.longHorizonMaxTurns。
	// 0 = 未配置, Execute 用编译期兜底 (与改造前硬编码的 150 同值)。
	longHorizonMaxTurns int
	// stageDecl 本阶段声明的 Hooks / Skills (21.8.12 P4/X1)。
	//
	// 装配期从创建 ctx 捕获 (见 agentFactory), 因为 Execute 的 ctx 是另一份。
	// Model 不在这里 —— 它在 agent.runAgent 内部就被消费掉了 (那条路径覆写
	// ModelConfigKey), 到这里时已经体现在 modelCfg 里。
	stageDecl agent.StageDecl

	// declHooks 装配期就已解析好的 hook 配置 (types.HookConfig), 来自 --agent 的资产声明。
	//
	// 单开一个字段而不是塞进 stageDecl.Hooks: 后者的契约是**声明原文**
	// (<event>:<type>:<target>[:<if>]), 由 hooksFromDecl 在 Execute 里解析。把一份
	// 已解析的 HookConfig 放进去, 会被当成一条拼错的声明整条丢掉 —— 而丢的表现是
	// "资产声明的 hook 没跑", 不报任何错。
	declHooks []types.HookConfig

	// mu 保护 inject。中继 (agent 包的 startMidRunMailRelay) 在另一个 goroutine 上
	// 读它, 而 Execute 在起/收时写它 —— 不锁就是数据竞态。
	mu     sync.Mutex
	inject *engine.InjectQueue // 仅在 Execute 进行中非 nil (21.8.12 P5)
}

// CanInjectMidRun / InjectMidRun 实现 agent.MidRunMailInjector (21.8.12 P5)。
//
// ★ 队列在这里**每次 Execute 新建一个**, 而不是复用 r.eng 上那个 (main.go 里
// cliAgentRunner 持有的 eng 是全进程共享的一个):
//
//	agentPool 8 并发共用同一个 eng (见 agentFactory 的注释), 引擎级队列下
//	"A 阶段的更正"会被"B 阶段"收走 —— 而两边都看不出异常: 收到的那个当真去做,
//	没收到的那个以为没人给它发信。队列的身份必须是**这次运行**。
func (r *cliAgentRunner) CanInjectMidRun() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inject != nil && r.inject.Active()
}

func (r *cliAgentRunner) InjectMidRun(text string) bool {
	r.mu.Lock()
	q := r.inject
	r.mu.Unlock()
	if q == nil {
		return false
	}
	return q.Inject(midRunMessage(text))
}

// midRunMessage 把一段注入正文包成 user 消息 (与 pkg/feishu 的同名助手逐字同形)。
//
// 两处各写一份的代价落在 role 上: InjectQueue 按 role 拒, 而拒的表现只是"没送出去" ——
// 一个 role 写错的副本会静默地永不投递, 且没有任何报错指向它。
func midRunMessage(text string) types.Message {
	return types.Message{
		Type:      types.MessageTypeUser,
		UUID:      internal_hook.GenerateUUID(),
		CreatedAt: time.Now(),
		Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: text}},
	}
}

// cliStageMaxTurns 决定 CLI 路径一个 stage 的回合上限 (H4-3)。
//
// 抽成纯函数是为了能被直接断言: 这段逻辑此前是三个互相覆盖的赋值
// (先 150 → 再被模型级覆盖 → 再被思考型覆盖成 1), 顺序错一次就是一处静默偏差,
// 而它的外部表现只有"某类 stage 多跑或少跑了几十轮"。
//
// 判据与 pkg/feishu 的 stageMaxTurns **同序**:
//  1. 思考型 ⇒ 1 (它与"关工具"是同一个决定, 关工具之后多一轮都没有意义)
//  2. 模型级 maxTurns ⇒ 原样 (kimi 系配的 40 是按那个模型的回合开销定的)
//  3. 长程档
//
// longTurns 由调用方兜到 150 之后再传进来 (见 longHorizonMaxTurnsOr) ——
// 本函数不关心"没配置时用什么", 只关心优先级。
func cliStageMaxTurns(role string, modelTurns, longTurns int) int {
	if modelconfig.IsThinkingRole(role) {
		return 1
	}
	if modelTurns > 0 {
		return modelTurns
	}
	return longTurns
}

// longHorizonMaxTurnsOr 长程档上限, 未配置 (=0) 时取编译期兜底。
//
// 兜底值是 150 —— 与 pkg/feishu 的 stageMaxTurns **不同**: 那里未配置时回落到
// 基线 (不动), 这里回落到 150 (等于改造前 CLI 路径的硬编码值)。两处刻意不一致,
// 因为两条路径的**改造前行为**本来就不一致: 团队侧是 40, CLI 侧是 150。
// "未配置 = 改造前" 是这一整项的唯一纪律, 各自对各自的改造前负责。
func longHorizonMaxTurnsOr(n int) int {
	if n > 0 {
		return n
	}
	return modelconfig.DefaultLongHorizonMaxTurns
}

// maxTokensOrDefault 取一个 >0 的 maxTokens, 否则回落默认。
func maxTokensOrDefault(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

// hooksFromDecl 把阶段声明的 hook 原文解析成引擎可用的配置 (21.8.12 P4/X1)。
//
// 解析走 hooks.ParseHookDecls —— 与资产 hooks: 段**同一份实现**。在这里再写一遍
// `<event>:<type>:<target>[:<if>]` 的切分, 就是同一套取值域有两个真相源, 而漂移的
// 表现是"同一种写法在资产里读得出、在 stage 里读不出", 不报错。
//
// 解析问题逐条点名、其余照常生效: 一份声明里三条 hook 两条合法一条拼错时, 合法的
// 两条该跑, 拼错的那条该被看见 —— 整份作废会让作者以为"格式整个没写对"。
// effectiveHooks 汇总本次执行要并入引擎 Runner 的外部 hook。
//
// 两条来源**相加**而不是二选一: 资产声明的 hook (declHooks, 已解析) 与阶段声明的
// hook (stageDecl.Hooks, 待解析)。只取一条的后果是"某一种写法在资产里读得出、
// 在 stage 里读不出", 且不报错。
func (r *cliAgentRunner) effectiveHooks() []types.HookConfig {
	out := hooksFromDecl(r.stageDecl, r.stage)
	if len(r.declHooks) == 0 {
		return out
	}
	// append 到新切片: out 可能直接持有 hooksFromDecl 的返回值, 原地追加会让
	// 两次 Execute 共用同一段底层数组 (本 runner 是可复用的)。
	merged := make([]types.HookConfig, 0, len(out)+len(r.declHooks))
	merged = append(merged, out...)
	merged = append(merged, r.declHooks...)
	return merged
}

func hooksFromDecl(decl agent.StageDecl, stage string) []types.HookConfig {
	if len(decl.Hooks) == 0 {
		return nil
	}
	cfgs, probs := hooks.ParseHookDecls(decl.Hooks)
	for _, p := range probs {
		log.Printf("[Hooks] 阶段 %q 声明的 hook 问题: %s", stage, p)
	}
	return cfgs
}

// stageDeclSkillListing 把阶段声明的技能名渲染成短清单 (21.8.12 P4/X1)。
//
// 取注册表的路径是**这一处最讲究的地方**: 从 Skill 工具自己身上拿
// (skills.SkillTool.Registry), 而不是用包级的 deps.skillReg。
//
// 理由是"哪一份技能库真正在服务这次调用"只有工具知道: 嵌套子代理路径
// (runNestedAgent) 会另造一个 nestedReg 并注册一个新 SkillTool, 那时包级变量指向
// 的仍是**父**的那一份 —— 用包级变量渲染出来的清单与模型按名字 load 得出来的结果
// 不是同一份注册表, 而两者的失配不报错, 只表现为"清单里有、load 说没有"。
// 让工具自答则必然一致。
//
// 渲染不出清单的三条路各自点名: 没有 Skill 工具 (被白名单挡了 / 思考型关工具)、
// 注册表为空、名字没注册。三者都表现为"声明了却什么都没发生", 只有日志能分开它们 ——
// 与 pkg/feishu 的 stageSkillListing 同一口径。
func (r *cliAgentRunner) stageDeclSkillListing() string {
	if len(r.stageDecl.Skills) == 0 {
		return ""
	}
	if r.eng == nil || r.eng.Tools == nil {
		log.Printf("[Skills] 阶段 %q 声明了技能 %v, 但引擎没有工具注册表, 这部分声明不生效",
			r.stage, r.stageDecl.Skills)
		return ""
	}
	raw, ok := r.eng.Tools.Get("Skill")
	if !ok {
		// 思考型角色 (DisableTools) 与 --allowed-tools 白名单都会走到这里, 而两者的
		// **都是**"这条路径上技能面整个不存在" —— 声明无效是必然的, 值得点名的是
		// 作者在关工具的 stage 上声明了技能这个组合本身。
		log.Printf("[Skills] 阶段 %q 声明了技能 %v, 但本次执行没有 Skill 工具 (思考型关工具 / 白名单挡下), 这部分声明不生效",
			r.stage, r.stageDecl.Skills)
		return ""
	}
	st, ok := raw.(*skills.SkillTool)
	if !ok || st.Registry() == nil || st.Registry().Count() == 0 {
		log.Printf("[Skills] 阶段 %q 声明了技能 %v, 但 Skill 工具背后的注册表为空, 这部分声明不生效",
			r.stage, r.stageDecl.Skills)
		return ""
	}
	reg := st.Registry()
	var unknown []string
	for _, n := range r.stageDecl.Skills {
		if _, known := reg.GetAny(n); !known {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		// "没注册" 与 "注册了但不可注入 (shadow/archived)" 在清单上同形, 所以这里
		// 只报前者 —— 后者是运维已知的状态, 前者一定是打错了字 (与 pkg/feishu 同口径)。
		log.Printf("[Skills] 阶段 %q 声明的技能未在注册表里, 这部分声明不生效: %s",
			r.stage, strings.Join(unknown, ", "))
	}
	return reg.FormatShortListingForNames(r.stageDecl.Skills)
}

func (r *cliAgentRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	// 走完整的工具循环 (engine.RunIsolated): 这里既支持 Anthropic 原生 tool_use,
	// 也支持 XML / [TOOL_CALL] bracket 风格的工具调用回退.
	// 之前的实现直接走 client.SendMessage 没有工具循环, 导致 MiniMax 等模型在文本里塞
	// 的 <minimax:tool_call>... 永远不会被执行, 整个 team stage 形同空转.
	//
	// 角色策略: 思考型 (architect/researcher/planner) 关闭工具, 强制单轮产出文档.
	// 操作型 (coder/tester/reviewer) 开放工具循环, 让它真的去读/写/编译代码.
	client := r.eng.APIClient
	if r.modelCfg.ProviderName != "" {
		// 出站模型名走 OutboundModelName —— 与 feishu 侧一致,
		// 见 modelconfig.ResolvedConfig.OutboundModelName。
		client = client.ConfiguredCloneFull(
			r.modelCfg.BaseURL, r.modelCfg.APIKey, r.modelCfg.OutboundModelName(),
			r.modelCfg.FallbackModels, r.modelCfg.FallbackBaseURL, r.modelCfg.FallbackAPIKey,
		)
	}
	role := strings.ToLower(strings.TrimSpace(r.role))

	// 回合上限 (H4-3): 判定顺序与取值收在 cliStageMaxTurns 里, 见那里的注释。
	//
	// 改动前后各有一条要交代:
	//   - **改前**: 这里无条件给 150, 之后再也没有读过 r.modelCfg.MaxTurns ——
	//     模型配置里写的 maxTurns 在这条路径上**从来不起作用** (kimi 系配的 40
	//     也被一起丢掉)。现在它原样优先。
	//   - **改后**: 未配置 ai.longHorizonMaxTurns 时仍是 150
	//     (longHorizonMaxTurnsOr 的兜底), 与改造前逐字相同。
	maxTokens := maxTokensOrDefault(r.modelCfg.MaxTokens, 16384)
	maxTurns := cliStageMaxTurns(role, r.modelCfg.MaxTurns, longHorizonMaxTurnsOr(r.longHorizonMaxTurns))
	// 思考型角色 = 关工具 = 单轮。判据与名单走 modelconfig 的同一份定义 ——
	// "思考型" 是一个集合成员判断, 而这个集合在团队路径上也要用 (见 pkg/feishu
	// 的档位分类)。两处各列一遍名单迟早分叉成"同一个角色在两条路径上跑不同的轮数"。
	disableTools := modelconfig.IsThinkingRole(role)
	if disableTools {
		// 思考型一次性吐完整设计稿; 优先使用模型配置里的 maxTokens
		maxTokens = maxTokensOrDefault(r.modelCfg.MaxTokens, 65536)
	}
	// 阶段专属技能并入 system prompt (21.8.12 P4/X1)。**追加**在装配期那一段之后,
	// 不替换 —— 见 stageDeclSkillListing 的论证。
	stageSystem := r.stageDeclSkillListing()
	systemPrompt := r.systemPrompt
	if stageSystem != "" {
		if strings.TrimSpace(systemPrompt) == "" {
			systemPrompt = stageSystem
		} else {
			systemPrompt = systemPrompt + "\n" + stageSystem
		}
	}

	// 运行中入站队列 (21.8.12 P5 / D6-3 缺口 2 级 2)。每次 Execute 一个 —— 见
	// cliAgentRunner 上那两个方法关于"不能用引擎级队列"的论证 (8 并发共用一个 eng)。
	inject := engine.NewInjectQueue()
	r.mu.Lock()
	r.inject = inject
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inject = nil
		r.mu.Unlock()
	}()

	out, err := r.eng.RunIsolated(ctx, userPrompt, engine.IsolatedRunOptions{
		SystemPrompt: systemPrompt,
		MaxTurns:     maxTurns,
		MaxTokens:    maxTokens,
		Client:       client,
		DisableTools: disableTools,
		Stage:        r.stage, // H3-2 中间态身份; 空 = 身份退化为 Role+Cwd
		Inject:       inject,
		// 阶段声明的 hook 交给引擎并入它既有的 Runner (见 IsolatedRunOptions.Hooks):
		// 这里手上没有那份基线配置 (Runner 构造完就把入参丢了), 而且**替换**会让
		// 平台侧的审计/门禁 hook 在这条执行路径上静默消失。解析与资产 hooks: 段
		// 走同一份实现 —— 两处各写一份的话, `<event>:<type>:<target>[:<if>]` 的
		// 切分规则会漂移成"同一种写法在资产里读得出、在 stage 里读不出"。
		Hooks: r.effectiveHooks(),
	})
	if err != nil {
		// 把目前为止收集到的文本一并带回, 方便上游写 checkpoint / debug.
		if out != "" {
			return out, err
		}
		return "", err
	}
	return out, nil
}

// printStreamEvents 消费 StreamEvent 通道，实现 token-by-token 实时输出。
func printStreamEvents(ch <-chan types.StreamEvent) {
	consumeStreamEvents(ch, false)
}

// consumeStreamEvents 消费 StreamEvent 通道。quiet=false 时 token-by-token 实时
// 输出 (原 printStreamEvents 行为); quiet=true 时不打印, 仅累积最终 (非 thinking)
// 文本与错误分类, 供 --output-format json 的 envelope 使用。
func consumeStreamEvents(ch <-chan types.StreamEvent, quiet bool) (finalText, errKind string) {
	inThinking := false
	hasOutput := false
	var final strings.Builder
	for ev := range ch {
		switch ev.Kind {
		case types.StreamEventDelta:
			if ev.IsThinking {
				if flagFinalOnly || quiet {
					continue // --final-only / json: thinking 过程不输出
				}
				if !inThinking {
					fmt.Print("\033[2m") // dim
					inThinking = true
				}
				fmt.Print(ev.DeltaText)
			} else {
				final.WriteString(ev.DeltaText)
				if quiet {
					hasOutput = true
					continue
				}
				if inThinking {
					fmt.Print("\033[0m") // reset
					inThinking = false
				}
				fmt.Print(ev.DeltaText)
			}
			hasOutput = true
		case types.StreamEventBlockDone:
			if inThinking && !quiet {
				fmt.Print("\033[0m")
				inThinking = false
			}
		case types.StreamEventToolStart:
			if flagDebug && !quiet {
				fmt.Printf("\n[调用工具: %s]\n", ev.ToolName)
			}
		case types.StreamEventToolDone:
			if flagDebug && !quiet {
				result := ev.ToolResult
				if len(result) > 200 {
					result = result[:200] + "..."
				}
				fmt.Printf("[工具完成] %s\n", result)
			}
		case types.StreamEventMessageDone:
			// MessageDone 标志一轮 assistant 完成
		case types.StreamEventError:
			if ev.Error != nil {
				errKind = classifyRunError(ev.Error)
				if !quiet {
					fmt.Fprintf(os.Stderr, "\n[Error] %v\n", ev.Error)
				}
			}
		}
	}
	if hasOutput && !quiet {
		fmt.Println()
	}
	return strings.TrimSpace(final.String()), errKind
}

// classifyRunError maps a stream error to a coarse kind for the JSON envelope
// so callers can pick a retry strategy without parsing free-form messages.
func classifyRunError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "context deadline") || strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "429"):
		return "rate_limit"
	case strings.Contains(msg, "overloaded") || strings.Contains(msg, "529"):
		return "overloaded"
	case strings.Contains(msg, "too long") || strings.Contains(msg, "context length") || strings.Contains(msg, "max tokens"):
		return "prompt_too_long"
	default:
		return "error"
	}
}

// parseAllowedTools splits a comma-separated whitelist into a set. Only a
// fully-empty flag ("") yields nil (no whitelist). A non-empty flag that parses
// to zero valid entries (e.g. " , ") returns a non-nil EMPTY set, which the
// engine treats as deny-all — fail-closed, so a caller template that renders
// blank never silently exposes every tool.
func parseAllowedTools(csv string) map[string]bool {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	set := make(map[string]bool)
	for _, t := range strings.Split(csv, ",") {
		if t = strings.TrimSpace(t); t != "" {
			set[t] = true
		}
	}
	return set
}

// effectiveAllowedTools 合并工具白名单的两个来源, 优先级 flag > config。
//
// 为什么需要 config 这一侧: 托管 Agent 的部署态由平台下发岗位的工具面, 而此前
// 白名单只能拼命令行 —— 命令行参数**不可回读** (进程起来后没人知道当时传了什么)、
// **不可版本化** (改一次没法 diff / 回滚)、**不可对账** (只有下发没有生效的回程)。
// 平台 launcher 传的正是空 --allowed-tools 加一份 config, 所以这一侧不通就等于
// 岗位模板下发不了工具面。
//
// 三态语义 (与 engine.Config.AllowedTools 逐字一致, 不做任何"友好化"):
//
//	flag 非空          → 用 flag 解析结果 (非 nil, 空串" , "会解析成空集 = 拒绝一切)
//	flag 空, config 有 → 用 config 那份 (非 nil; 空数组同样是拒绝一切)
//	两边都空           → nil = 不设白名单 (全部工具可见)
//
// 最后一行的区分是**实质**的: 前两种是"有人声明了白名单" (哪怕声明成空), 第三种是
// "没人声明"。把第三种也当成拒绝一切, 会让每一次没配白名单的交互式运行一个工具都
// 调不动 —— fail-closed 要防的是"岗位模板渲染成空却当成全放行", 不是"本来就没设限"。
func effectiveAllowedTools(flagCSV string, jsonCfg *feishu.JSONConfig) map[string]bool {
	if strings.TrimSpace(flagCSV) != "" {
		return parseAllowedTools(flagCSV)
	}
	if jsonCfg == nil || jsonCfg.AllowedTools == nil {
		return nil
	}
	set := make(map[string]bool, len(jsonCfg.AllowedTools))
	for _, t := range jsonCfg.AllowedTools {
		set[t] = true
	}
	return set
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

func formatNameList(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

// engineDeps shared dependencies for the root engine and nested Agent runs.
type engineDeps struct {
	cfg         *engine.Config
	apiClient   *api.Client
	hookRunner  *hooks.Runner
	permChecker *permissions.Checker
	compactor   *compact.Compactor
	promptMgr   *prompt.Manager
	mcpConns    []*mcp.Connection
	skillReg    *skills.Registry
	// hookConfigs 是构造 hookRunner 用的那一份配置, 与 hookRunner 并排留着是因为
	// 子代理要给自己的 Runner **追加**资产声明的 hook (21.8.12 P2): 只留 Runner 的话
	// 那份配置在构造完之后就取不回来了, 于是子代理只能要么复用父 Runner (资产 hook
	// 装到父会话身上, 而父引擎同时在跑自己的轮次) 要么丢掉继承来的 hook。两者都错。
	hookConfigs []types.HookConfig
	// traceStore 供嵌套子代理引擎共用同一份轨迹底座 (design/01 §4.8)。
	// 在 deps 构造之后才被赋值 (TraceStore 要先算出 stateDir), 故可为 nil。
	traceStore *tracestore.Store
}

func connectMCP(ctx context.Context, path string) ([]*mcp.Connection, error) {
	if path == "" {
		return nil, nil
	}
	cfgs, err := mcp.LoadServerConfigsFromFile(path)
	if err != nil {
		return nil, err
	}
	mcpClient := mcp.NewClient()
	var out []*mcp.Connection
	for _, cfg := range cfgs {
		if cfg.Command == "" {
			fmt.Fprintf(os.Stderr, "skip MCP server %q: empty command\n", cfg.Name)
			continue
		}
		conn, err := mcpClient.Connect(ctx, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "MCP %q: %v\n", cfg.Name, err)
			continue
		}
		out = append(out, conn)
	}
	return out, nil
}

// ==================== P5: subagent_type 的资产查表 (21.8.12) ====================
//
// 与 pkg/feishu/session.go 同名的两个函数是**故意的重复**, 不是抄漏——但它们的重复
// 方式值得写清楚: 飞书那侧返回的档位经 ConstraintSet (ProfileSourceExplicit, 有来源
// 可审计), 这侧的 CLI 没有约束体系, 直接决定注册哪个档位。两处都只做同一件事:
// 查表 + 校验声明取值是否合法, 非法就留痕并当作"没声明"。
//
// 判据是 cmd/claude-go/p5_asset_wiring_test.go: 它直接调 runNestedAgent, 从
// 出站请求里数工具集。

// cliBindAgentAsset 按 subagent_type 装配出这份 agent 的全部声明 (21.8.12 P2)。
//
// 与飞书那条路径**共用同一个组装器** (pkg/agentbind): 档位合法性、技能对池、
// hook 解析、权限档宽严判定这几条规则只有一处定义。分成两份会让"同一样的 .md 在
// 两条路径上装出不同结果", 而那种差异从任何单侧看都是不可见的。
//
// 返回 nil 表示"没查到 / 非 active", 调用方照旧走各自的基线。
func cliBindAgentAsset(subagentType string, skillReg *skills.Registry) *agentbind.AssetBinding {
	if strings.TrimSpace(subagentType) == "" {
		return nil
	}
	b := agentbind.BindAgentAsset(subagentType, skillReg)
	for _, w := range b.Warnings() {
		log.Printf("[P2][warn] %s", w)
	}
	return b
}

// loadAssetCatalog 装载资产索引 (21.8.12 P0/P1), 返回一行摘要供调用方打日志。
//
// CLI 与飞书两条启动路径都要调它: 资产目录的读取方此前**一处都没有**, 只在其中
// 一条路径上补, 会让"CLI 里能用的 agent 定义, 飞书里查不到"成为一条没人知道的规则。
func loadAssetCatalog(cwd, stateDir string) string {
	roots := asset.DefaultRoots(cwd, stateDir)
	set := asset.LoadStandard(roots)
	agent.SetAgentCatalog(set)
	for _, p := range set.DedupReport() {
		log.Printf("[asset][warn] %s", p)
	}
	return set.Describe(roots)
}

func runNestedAgent(ctx context.Context, deps *engineDeps, runAgent agent.RunAgentFunc, agentPrompt string, opts agent.RunOptions) (string, error) {
	nestedReg := tool.NewRegistry()
	// P5 查表 (21.8.12): subagent_type 先按 agent 资产查表, 查到就用资产声明的档位。
	//
	// 这条路径改造前比飞书那条更彻底: 它**根本不按档位装配**, 一律 RegisterBaseTools
	// (全量内置工具)。所以资产声明的 tool-profile 在这里原本连一个可能的落点都没有。
	//
	// 这里**不做 ProfileNarrows 收窄判定**, 理由不是"不想做"而是"在这条路径上它恒
	// 成立因此没有信息": 基线是全量内置工具集, 六个档位注册的每一件都在它的子集里,
	// 没有任何一份声明能放宽。反过来说, 加一道恒真的闸只会制造一种"看起来有防护"的
	// 假象 —— 而它还会真的挡掉合法声明: 我第一版就是拿 coding 当基线去判, 结果
	// `analysis` 与 `coding` 互不包含 (analysis 有 WebSearch、coding 有 Bash/Write),
	// ProfileNarrows 两个方向都返回 false, 于是最该生效的那份声明被静默拒掉了。
	//
	// 真正该挡的是**取值不认识**: 那由 cliAgentAssetProfile 负责 (它把非法取值当作
	// "没声明"并记日志) —— 不挡的话 NormalizeToolProfile 会把 `codign` 静默掰成 chat,
	// 子代理会突然什么都没有, 而日志上一个字都没有。
	//
	// 注意下面几个注册 (Agent/delegate_task/MCP/Skill) 在**两种分支之外**, 无论走哪个
	// 档位都注册 —— 档位收窄不该切断递归派生能力 (那是"削错了东西")。
	assetBind := cliBindAgentAsset(opts.SubagentType, deps.skillReg)
	if assetBind == nil {
		assetBind = &agentbind.AssetBinding{} // 空读数: 下面的声明一律空, 走基线
	}
	if assetBind.HasToolProfile {
		builtin.RegisterProfileToolsWithStore(nestedReg, nil, nil, assetBind.ToolProfile)
	} else {
		builtin.RegisterBaseTools(nestedReg, nil)
	}
	mcp.RegisterMCPTools(nestedReg, deps.mcpConns)
	if deps.skillReg != nil && deps.skillReg.Count() > 0 {
		nestedReg.Register(skills.NewSkillTool(deps.skillReg))
	}
	// 子代理自己也带轨迹底座: 本条路径**给子代理又注册了一次 Agent 工具**(飞书那条
	// 不注册), 递归可以很深。这里若用裸 NewAgentTool, 第 2 层及以下的派生就再次不可见,
	// 而恰恰是深层递归最需要被看见。delegate_task 同样注册, 子代理也能继续分解。
	nestedReg.Register(agent.NewAgentToolWithTrace(runAgent, deps.traceStore))
	nestedReg.Register(agent.NewDelegateToolWithTrace(runAgent, deps.traceStore, 80))

	nestedCfg := *deps.cfg
	client := deps.apiClient
	// P2 声明: 资产声明的模型 / 回合上限 —— 只在真的声明了时才生效, 且**弱于**
	// 调用方显式传的 opts (显式参数 > 资产默认: 一个派生点说"这个子任务用快模型"
	// 比资产里那句常驻默认更具体)。
	if assetBind.Model != "" && opts.Model == "" {
		opts.Model = assetBind.Model
	}
	if assetBind.MaxTurns > 0 && opts.MaxTurns <= 0 {
		opts.MaxTurns = assetBind.MaxTurns
	}
	if opts.Model != "" {
		// 子代理模型确定化烘焙: 既改 Config.Model 标签, 又把模型写进 client 副本,
		// 并清空 ExecutionModel/PlanClient —— 子代理全程单模型, 不套用主会话的
		// 相位路由 (含规划期 plan 模型路由, 否则 delegate_task 在规划期会被 plan
		// 客户端劫持, 违背"子代理固定取指定模型"契约)。
		// (原实现只改 nestedCfg.Model 标签, 实际 client 仍是规划方模型, 靠首回合
		// WithModel 恰好救回; 这里把"恰好"变成"确定"。)
		nestedCfg.Model = opts.Model
		nestedCfg.ExecutionModel = ""
		nestedCfg.PlanClient = nil
		if opts.Model != client.Model {
			client = client.WithModel(opts.Model)
		}
	}
	if opts.MaxTurns > 0 {
		nestedCfg.MaxTurns = opts.MaxTurns
	}
	perm := deps.permChecker
	if opts.ReadOnly {
		nestedCfg.PermissionMode = types.PermissionModePlan
		perm = permissions.NewChecker(types.PermissionModePlan)
	}
	// P2 权限档声明: **只许收窄**。资产目录是 state/project/user 三层都写得进去的
	// 地方, 一份 .md 里写 `permission-mode: bypass` 若无条件生效, 就是一条从文件到
	// 权限的提升路径 —— 而且完全静默。口径与飞书那条路径、与 ApplyConstraints 的
	// "约束按构造只会收窄"一致。
	if assetBind.HasPermissionMode {
		if permissions.Widens(nestedCfg.PermissionMode, assetBind.PermissionMode) {
			log.Printf("[P2][warn] agent 资产 %q 声明的 permission-mode=%q 比外层 (%q) 更宽, 已拒绝"+
				" —— 声明只能收窄权限面", assetBind.Name, assetBind.PermissionMode, nestedCfg.PermissionMode)
		} else {
			nestedCfg.PermissionMode = assetBind.PermissionMode
			perm = permissions.NewChecker(assetBind.PermissionMode)
		}
	}

	// 工具名单 (P2′): 资产声明的 `tools:` 白名单与 `disallowed-tools:` 黑名单。
	//
	// ★ 这里补的是一个**已经存在的洞**, 不是新功能。改造前 assetBind 已经把这两份名单
	// 解析出来、也写进了装配读数 (Binding.Declared), 但**没有任何一行把它们送进运行时**
	// —— 于是 `disallowed-tools: [Bash]` 的效果是"报告里写着挡了, 实际一个没挡"。
	//
	// 本条路径不用 ConstraintSet (那是飞书路径的机制), 所以直接写 cfg 的两个字段 ——
	// 但仍然照 ApplyConstraints 的**同两条规则**: 黑名单取并集, 白名单取交集。
	// 只按自己的来源"放行"是权限提升: nestedCfg 是从父引擎拷来的, 它的 AllowedTools
	// 是父会话的有效约束, 拿资产那份整个盖上去等于把父会话的白名单**放宽**成资产的。
	//
	// ★ 门槛是 HasTools (键写没写), **不是** `Tools != nil` 也不是 `len(Tools) > 0`。
	//
	// 这两个更"自然"的写法都会把 `tools: []` 吃掉: 依赖 nil-ness 就等于依赖
	// bindTools 记得把空名单补成非 nil 空切片 (一条别处一改就悄悄失效的隐式约定),
	// 用长度当门槛则连"空名单"这个分支都进不来。而 `tools: []` 的语义正是
	// fail-closed 拒绝一切 —— 它被吃掉之后, 这份声明在运行时的效果与"没写 tools:"
	// 逐字相同: 子代理拿到父会话的全部工具。一份刻意收紧的声明静默变成完全放行,
	// 是本条分支最不能出的错。
	if assetBind.HasTools {
		if nestedCfg.AllowedTools == nil {
			next := make(map[string]bool, len(assetBind.Tools))
			for _, n := range assetBind.Tools {
				next[n] = true
			}
			nestedCfg.AllowedTools = next
		} else {
			next := make(map[string]bool, len(assetBind.Tools))
			for _, n := range assetBind.Tools {
				if nestedCfg.AllowedTools[n] {
					next[n] = true
				}
			}
			// 交集之后的空 map 是**合法**的 (父会话白名单与资产白名单不相交 ⇒
			// 这个子代理什么工具都不给)。这不是误配, 但它是"什么都不给", 必须
			// 让人看得见 —— 否则下一个现象是"子代理一个工具都没调, 也没有报错"。
			//
			// 两条成因分开关照: 显式空白名单是**作者要求**的拒绝一切 (设计如此),
			// 交集为空才是"两份名单各写各的、恰好不相交"(通常是要修的配置)。
			if len(next) == 0 {
				if len(assetBind.Tools) == 0 {
					log.Printf("[P2] agent 资产 %q 声明了显式空白名单 (tools: []), 子代理 %q 将按声明拒绝一切工具",
						assetBind.Name, opts.SubagentType)
				} else {
					log.Printf("[P2][warn] agent 资产 %q 的 tools 白名单与父会话白名单不相交, "+
						"子代理 %q 将**没有任何工具可用** (资产=%v)", assetBind.Name, opts.SubagentType, assetBind.Tools)
				}
			}
			nestedCfg.AllowedTools = next
		}
	}
	// 黑名单这一维用 HasDisallowedTools 而不是 len()>0: `disallowed-tools: []` 是
	// "声明了一份空黑名单" (一处都不挡), 与"没写这一行"在本条路径上**行为**确实一样
	// (空集合的并集是空), 但存在那一侧不同 —— 对账要拿它去回答"这份声明到底管了没有"。
	// 用长度当门槛会让这份声明在对账里彻底隐形。行为相同、读数不同, 正是这类键的共性。
	if assetBind.HasDisallowedTools {
		if nestedCfg.DisabledTools == nil {
			nestedCfg.DisabledTools = make(map[string]bool, len(assetBind.DisallowedTools))
		}
		for _, n := range assetBind.DisallowedTools {
			nestedCfg.DisabledTools[n] = true
		}
	}

	nestedPromptMgr := prompt.NewManager(nestedCfg.Cwd)
	nestedPromptMgr.CustomPrompt = deps.promptMgr.CustomPrompt
	nestedPromptMgr.AppendPrompt = deps.promptMgr.AppendPrompt
	// P2: 资产正文追加在**继承来的** AppendPrompt 之后, 而不是替换它 —— 父会话经
	// --append-system-prompt 传下来的指令与这份 agent 自己的角色指令是两回事, 一份
	// 都不该吃掉另一份。
	if p := assetBind.Prompt; p != "" {
		if nestedPromptMgr.AppendPrompt != "" {
			nestedPromptMgr.AppendPrompt = nestedPromptMgr.AppendPrompt + "\n\n" + p
		} else {
			nestedPromptMgr.AppendPrompt = p
		}
	}
	nestedPromptMgr.OverridePrompt = deps.promptMgr.OverridePrompt
	nestedPromptMgr.CoordinatorPrompt = deps.promptMgr.CoordinatorPrompt
	nestedPromptMgr.AgentPrompt = deps.promptMgr.AgentPrompt
	nestedPromptMgr.Model = nestedCfg.Model
	nestedPromptMgr.SkillListing = deps.promptMgr.SkillListing
	// P2 skill 绑定: 资产声明的技能并进这份子代理的技能清单。并进而非替换 ——
	// 继承来的那份是父会话的池视图, 整个换掉等于资产声明顺带撤销了父会话的技能面。
	// 没有技能池 (deps.skillReg 为空) 时组装器已经报过 unknown_name, 这里不重复留痕。
	if len(assetBind.Skills) > 0 && deps.skillReg != nil {
		if extra := deps.skillReg.FormatShortListingForNames(assetBind.Skills); strings.TrimSpace(extra) != "" {
			if nestedPromptMgr.SkillListing != "" {
				nestedPromptMgr.SkillListing = nestedPromptMgr.SkillListing + "\n" + extra
			} else {
				nestedPromptMgr.SkillListing = extra
			}
		}
	}
	nestedPromptMgr.ShowEnvironmentDate = deps.promptMgr.ShowEnvironmentDate

	// P2 hook 绑定: 资产声明的外部 hook **追加**在继承来的 Runner 的配置之后。
	//
	// Runner 是 per-调用 构造的 (deps.hookRunner 属于父引擎), 所以这里要给子代理
	// 单独建一个 —— 复用父的那个会让资产 hook 装到父会话身上 (而那个引擎同时还在
	// 跑父会话自己的轮次)。没有声明时**逐字节复用** deps.hookRunner: 这是常态路径,
	// 不该因为多了一次无意义的构造而改变任何行为。
	nestedHookRunner := deps.hookRunner
	if len(assetBind.Hooks) > 0 {
		merged := make([]types.HookConfig, 0, len(deps.hookConfigs)+len(assetBind.Hooks))
		merged = append(merged, deps.hookConfigs...)
		merged = append(merged, assetBind.Hooks...)
		nestedHookRunner = hooks.NewRunnerWithContext(ctx, merged, "")
	}

	// 生命周期 hook 的**开始**两拍 (与 pkg/feishu/session.go 的 runNestedAgent 逐条对齐)。
	//
	// 只把资产声明的 hook 装进 Runner 而从不触发, 它就是一个装配读数上的 wired、
	// 对账上的干净、运行上永远不跑的脚本 —— 声明了但没人读。
	//
	// 为什么放在这里而不是更早: SessionStart/SubagentStart 必须发生在**本轮引擎开始
	// 跑之前**、且只发生一次。放在 SubmitMessage 之前是这个函数里唯一同时满足这两条
	// 的位置。
	//
	// 只对**新构造的** Runner 触发 (deps.hookRunner 由父引擎创建并会在父会话的各轮里
	// 重复使用, 在这里再触发一次会让父会话的 SessionStart 变成每个子代理一次)。
	// 没有资产声明时逐字节维持改造前的行为 —— 存量部署 (config.json 无 hooks 键) 零变化。
	if nestedHookRunner != deps.hookRunner && nestedHookRunner != nil {
		if hookOut := nestedHookRunner.ExecuteSessionHooks(types.HookEventSessionStart); hookOut != nil &&
			(hookOut.Decision == "block" || hookOut.Decision == "deny") {
			return "", fmt.Errorf("SessionStart blocked by hook: %s", hookOut.Reason)
		}
		// 角色名取 opts.SubagentType —— 与上面查资产用的键**同一个来源**。这里不能
		// 回落到 nestedCfg.Role: 那条路径上它是从父引擎继承的 (本函数一处都不改它),
		// 于是 SubagentStart 会报出父会话的角色, 而 hook 里写的 If 判据正是拿这个名字
		// 匹配的 —— 一个报错名字的 hook 与不触发在运维上分不出来。
		if hookOut := nestedHookRunner.ExecuteSubagentStartHooks(opts.SubagentType, agentPrompt); hookOut != nil &&
			(hookOut.Decision == "block" || hookOut.Decision == "deny") {
			return "", fmt.Errorf("SubagentStart blocked by hook: %s", hookOut.Reason)
		}
	}

	nested := engine.NewQueryEngine(&nestedCfg, client, nestedReg, nestedHookRunner, perm, deps.compactor, nestedPromptMgr)
	nested.TraceStore = deps.traceStore // design/01 §4.8: 子代理的 llm_call 轨迹, 改造前恒缺席

	// P2 对账: 装完之后立刻把"这份资产声明要什么"与"这台引擎实际装成了什么"比一次
	// (先自报, 再对账, 最后才轮到面板)。自报取自**活引擎** —— 读配置得到的报告会
	// 一路全绿, 而那正是本仓修过的那个缺陷 (配置里都有, 引擎里那一半是空的)。
	//
	// Config 留 nil: 这是一个程序化装配出来的子引擎, 不是从配置查找链读出来的。
	if decl := assetBind.AssemblyDeclaration(); decl != nil {
		_, rec := engine.ReconcileAssembly(nested, engine.AssemblyReportInput{}, decl)
		if rec != nil {
			log.Printf("[P2] agent 资产 %q 的装配对账: %s", assetBind.Name, rec.Summary)
			for _, d := range rec.Diffs {
				log.Printf("[P2][diff] %s %s/%s 声明=%q 实际=%q", d.Kind, d.Facet, d.Item, d.Declared, d.Actual)
			}
		}
	}

	var sb strings.Builder
	for msg := range nested.SubmitMessage(ctx, agentPrompt) {
		if msg.Type != types.MessageTypeAssistant {
			continue
		}
		for _, b := range msg.Content {
			if b.Type == types.ContentBlockText {
				sb.WriteString(b.Text)
			}
		}
	}
	return sb.String(), nil
}

// ============================================================================
// Code Intelligence MCP Server
// ============================================================================

func codeintelMCPServerCmd() *cobra.Command {
	var (
		repoPath     string
		transport    string
		addr         string
		configPath   string
		indexBaseDir string
		gnCfg        *codeintel.GitNexusConfig
	)
	cmd := &cobra.Command{
		Use:   "codeintel-mcp-server",
		Short: "Code Intelligence MCP Server (GitNexus + Graphify)",
		Long: `启动 Code Intelligence MCP Server，对外暴露代码图谱查询工具。

支持三种传输方式:
  stdio  — JSON-RPC 2.0 over stdin/stdout（Claude Desktop / Cursor 默认）
  http   — RESTful JSON-RPC 端点，适合 service 守护
  sse    — Server-Sent Events 流式推送

配置优先级: CLI 参数 > --config JSON 中的 codeIntel > 默认值

示例:
  claude-go codeintel-mcp-server --repo /path/to/repo
  claude-go codeintel-mcp-server --config /path/to/config.json
  claude-go codeintel-mcp-server --repo /path/to/repo --transport http --addr 127.0.0.1:9234`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 若指定了 --config，从 JSON 读取 codeIntel 配置
			if configPath != "" {
				jsonCfg, err := feishu.LoadJSONConfig(configPath)
				if err == nil && jsonCfg != nil && jsonCfg.CodeIntel != nil {
					if !cmd.Flags().Changed("repo") {
						repoPath = jsonCfg.CodeIntel.RepoPath
					}
					if !cmd.Flags().Changed("transport") {
						transport = jsonCfg.CodeIntel.Transport
					}
					if !cmd.Flags().Changed("addr") {
						addr = jsonCfg.CodeIntel.Addr
					}
					if !cmd.Flags().Changed("index-base-dir") {
						indexBaseDir = jsonCfg.CodeIntel.IndexBaseDir
					}
					// gitnexus 调优段此前从未接线, calcHeapMB 一直用内置默认值
					if g := jsonCfg.CodeIntel.GitNexus; g != nil {
						gnCfg = &codeintel.GitNexusConfig{
							DefaultHeapMB:        g.DefaultHeapMB,
							MaxHeapMB:            g.MaxHeapMB,
							PerThousandFilesMB:   g.PerThousandFilesMB,
							PerHundredMBSourceMB: g.PerHundredMBSourceMB,
							IndexTimeoutMin:      g.IndexTimeoutMin,
						}
					}
				}
			}
			if transport == "" {
				transport = "stdio"
			}
			if addr == "" {
				addr = "127.0.0.1:9234"
			}
			srv := codeintel.NewMCPServerV2(repoPath)
			srv.IndexBaseDir = indexBaseDir
			srv.GitNexusCfg = gnCfg

			// 配置热加载: 这个服务的可热更新面很窄 —— repo / transport / addr /
			// indexBaseDir 都是启动期绑定的 (监听地址、索引目录), 改它们只能重启;
			// 真正能在运行中换的是 codeIntel.gitnexus 那段调优 (堆上限、超时、并行度),
			// 它们在每次索引请求时现读 (pkg/codeintel/mcp_transport.go:689/720/828)。
			// 所以这里只热更新这一段, 并在日志里说明其余项要重启 —— 不做"看起来
			// 热加载了其实没生效"的假象。
			if configPath != "" {
				if cfgPath, e := feishu.ResolveJSONConfigPath(configPath); e == nil {
					hotreload.WatchConfig(cfgPath, 5*time.Second, "codeintel-mcp", func(path string) error {
						c, e := feishu.LoadJSONConfig(path)
						if e != nil || c == nil || c.CodeIntel == nil {
							return fmt.Errorf("重新加载失败: %v", e)
						}
						g := c.CodeIntel.GitNexus
						if g == nil {
							return nil
						}
						srv.GitNexusCfg = &codeintel.GitNexusConfig{
							DefaultHeapMB:        g.DefaultHeapMB,
							MaxHeapMB:            g.MaxHeapMB,
							PerThousandFilesMB:   g.PerThousandFilesMB,
							PerHundredMBSourceMB: g.PerHundredMBSourceMB,
							IndexTimeoutMin:      g.IndexTimeoutMin,
						}
						return nil
					})
				}
			}
			var t codeintel.MCPTransport
			switch transport {
			case "http":
				t = codeintel.NewHTTPTransport(addr)
			case "sse":
				t = codeintel.NewSSETransport(addr)
			default:
				t = codeintel.NewStdioTransport()
			}
			return t.Run(srv)
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "默认仓库路径 (可选; 工具通过参数传入 repo_path)")
	cmd.Flags().StringVar(&transport, "transport", "stdio", "传输方式: stdio | http | sse")
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:9234", "HTTP/SSE 监听地址 (仅 transport=http/sse 时生效)")
	cmd.Flags().StringVar(&configPath, "config", "", "JSON 配置文件路径 (读取其中的 codeIntel 段)")
	cmd.Flags().StringVar(&indexBaseDir, "index-base-dir", "", "集中索引根目录 (如 /mnt/data/codeintel)")
	return cmd
}

// cliComplexityClassifier 构造 CLI 的 LLM 复杂度分类器 (llm/hybrid 模式用)。
// 用主 apiClient.SimpleComplete + pkg/complexity 的分类器 system prompt,
// 失败时回落启发式判定 (由 complexity.Judge 内部处理)。
func cliComplexityClassifier(client *api.Client) func(ctx context.Context, text string) (bool, string, error) {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, text string) (bool, string, error) {
		out, err := client.SimpleComplete(ctx, complexity.ClassifierSystemPrompt, text)
		if err != nil {
			return false, "", err
		}
		return strings.Contains(out, "COMPLEX"), strings.TrimSpace(out), nil
	}
}

// noopSwarmLLM 离线占位 LLM, 仅供 swarm history 等只读子命令构造引擎用 (不会真正发起调用)。
type noopSwarmLLM struct{}

func (noopSwarmLLM) SimpleComplete(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("offline: 只读模式不可调用 LLM")
}

// swarmCmd 群体智能 CLI: 直接驱动 swarm_intel 引擎执行预测/模拟, 供脚本与 Web 后端程序化调用。
// 与 chat/run 内的 /predict /simulate 同源, 但支持 --json: 结构化结果写 stdout, 过程通知写 stderr。
// 结果持久化在 ~/.claude-go/swarm_intel/, dashboard GET /api/hivemind 可统一查看。
func swarmCmd() *cobra.Command {
	var jsonOut bool
	var timeoutMin int

	cmd := &cobra.Command{
		Use:   "swarm",
		Short: "群体智能引擎 (预测/模拟/历史)",
		Long: `群体智能 (SwarmIntel) 引擎 CLI。

5+2 阶段流水线: 分解 → 侦察 → 预测 → 辩论 → 融合 → 校准 → 学习
多角色分析师独立预测后经多轮辩论, Boids 协调 + 贝叶斯/拜占庭融合 + Conformal 校准,
信素记忆跨会话强化。结果持久化在 <stateDir>/swarm_intel/, dashboard /api/hivemind 可读。`,
		Example: `  claude-go swarm predict "2027年AI Agent市场规模?"
  claude-go swarm simulate "如果量子计算突破会怎样?" --mode social
  claude-go swarm predict "BTC半年内趋势" --json
  claude-go swarm history --n 10 --json`,
	}
	cmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "结构化 JSON 输出 (stdout 仅 JSON, 过程通知走 stderr)")
	cmd.PersistentFlags().IntVar(&timeoutMin, "timeout", 15, "执行超时 (分钟)")

	newSwarmEngine := func() (*swarmintel.Engine, error) {
		eng, err := buildEngine()
		if err != nil {
			return nil, err
		}
		eng.APIClient.Tag = "swarm"
		siCfg := swarmintel.DefaultConfig()
		siCfg.Notify = func(_, msg string) {
			if jsonOut {
				fmt.Fprintln(os.Stderr, msg)
			} else {
				fmt.Println(msg)
			}
		}
		return swarmintel.NewEngine(eng.APIClient, siCfg), nil
	}

	emitJSON := func(v any) error {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}

	predictCmd := &cobra.Command{
		Use:   "predict <目标问题>",
		Short: "群体智能预测 (Boids协调+多轮辩论+贝叶斯融合)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			si, err := newSwarmEngine()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMin)*time.Minute)
			defer cancel()
			result, err := si.Predict(ctx, "cli", strings.Join(args, " "))
			if err != nil {
				return fmt.Errorf("预测失败: %w", err)
			}
			if jsonOut {
				return emitJSON(result)
			}
			fmt.Println()
			for _, o := range result.Outcomes {
				barLen := int(o.Probability * 30)
				if barLen > 30 {
					barLen = 30
				}
				bar := strings.Repeat("█", barLen) + strings.Repeat("░", 30-barLen)
				fmt.Printf("  %-16s %s %.1f%%  95%%CI [%.1f%%, %.1f%%]\n",
					o.Outcome, bar, o.Probability*100, o.Lower95*100, o.Upper95*100)
			}
			fmt.Printf("\n  共识度: %.0f%% | 辩论: %d轮 | 分析师: %d | 融合: %s\n",
				result.Consensus*100, result.Rounds, len(result.Agents), result.Method)
			if result.Summary != "" {
				fmt.Printf("\n%s\n", result.Summary)
			}
			return nil
		},
	}

	var simMode string
	var simAgents, simRounds int
	simulateCmd := &cobra.Command{
		Use:   "simulate <场景目标>",
		Short: "多Agent场景模拟与涌现行为检测",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			si, err := newSwarmEngine()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMin)*time.Minute)
			defer cancel()
			cfg := swarmintel.SimulationConfig{Mode: simMode, Agents: simAgents, Rounds: simRounds}
			result, err := si.Simulate(ctx, "cli", strings.Join(args, " "), cfg)
			if err != nil {
				return fmt.Errorf("模拟失败: %w", err)
			}
			if jsonOut {
				return emitJSON(result)
			}
			fmt.Println()
			for _, s := range result.Scenarios {
				fmt.Printf("  📌 %s (概率 %.0f%%)\n     %s\n", s.Name, s.Probability*100, s.Description)
			}
			if len(result.Emergent) > 0 {
				fmt.Println("\n涌现行为:")
				for _, e := range result.Emergent {
					fmt.Printf("  🌊 %s\n", e)
				}
			}
			if result.Summary != "" {
				fmt.Printf("\n%s\n", result.Summary)
			}
			return nil
		},
	}
	simulateCmd.Flags().StringVar(&simMode, "mode", "social", "模拟模式: social|game|montecarlo|crisis|org|creative|market|policy|tech")
	simulateCmd.Flags().IntVar(&simAgents, "agents", 5, "模拟 Agent 数量")
	simulateCmd.Flags().IntVar(&simRounds, "rounds", 3, "模拟轮数")

	var histN int
	historyCmd := &cobra.Command{
		Use:   "history",
		Short: "查看最近的预测历史 (离线, 不调用 LLM)",
		RunE: func(cmd *cobra.Command, args []string) error {
			siCfg := swarmintel.DefaultConfig()
			si := swarmintel.NewEngine(noopSwarmLLM{}, siCfg)
			records, err := si.GetHistory(histN)
			if err != nil {
				return fmt.Errorf("读取历史失败: %w", err)
			}
			if jsonOut {
				return emitJSON(records)
			}
			if len(records) == 0 {
				fmt.Println("暂无预测历史")
				return nil
			}
			for _, r := range records {
				fmt.Printf("[%s] %s (共识度 %.0f%%)\n", r.CreatedAt.Format("2006-01-02 15:04"), r.Question, r.Consensus*100)
			}
			return nil
		},
	}
	historyCmd.Flags().IntVar(&histN, "n", 20, "返回条数")

	cmd.AddCommand(predictCmd)
	cmd.AddCommand(simulateCmd)
	cmd.AddCommand(historyCmd)
	return cmd
}
