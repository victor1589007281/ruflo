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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/backup"
	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/codeintel"
	"github.com/anthropic/claude-go/pkg/commands"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/dashboard"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/sandbox"
	"github.com/anthropic/claude-go/pkg/session"
	"github.com/anthropic/claude-go/pkg/settings"
	"github.com/anthropic/claude-go/pkg/skills"
	swarmintel "github.com/anthropic/claude-go/pkg/swarm_intel"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/media"
	"github.com/anthropic/claude-go/pkg/types"
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
	flagModel        string
	flagAttach       []string
	flagCwd          string
	flagFinalOnly    bool
	flagEmitSession  bool
	flagAPIKey       string
	flagBaseURL      string
	flagMaxTokens    int
	flagMaxTurns     int
	flagPermission   string
	flagSystemPrompt string
	flagPrint        bool
	flagDebug        bool
	flagMCPConfig    string
	flagConfig       string
	flagResume       string // --resume <sessionID>
	flagContinue     bool   // --continue / -c
)

func main() {
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
	rootCmd.PersistentFlags().BoolVarP(&flagContinue, "continue", "c", false, "恢复最近一次对话")

	rootCmd.AddCommand(chatCmd())
	rootCmd.AddCommand(runCmd())
	rootCmd.AddCommand(feishuCmd())
	rootCmd.AddCommand(doctorCmd())
	rootCmd.AddCommand(toolsCmd())
	rootCmd.AddCommand(skillsCmd())
	rootCmd.AddCommand(rolesCmd())
	rootCmd.AddCommand(dashboardCmd())
	rootCmd.AddCommand(backupCmd())
	rootCmd.AddCommand(sandboxCmd())
	rootCmd.AddCommand(teamCmd())
	rootCmd.AddCommand(helpCmd())
	rootCmd.AddCommand(codeintelMCPServerCmd())
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
				Agents     []struct {
					Name   string `json:"name"`
					Status string `json:"status"`
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
			if len(t.Agents) > 0 {
				fmt.Println("Agent 状态:")
				for _, a := range t.Agents {
					fmt.Printf("  - %s [%s]\n", a.Name, a.Status)
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
		RunE: func(cmd *cobra.Command, args []string) error {
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

				ctx := context.Background()
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

			if strings.HasPrefix(userPrompt, "/") {
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
					return &cliAgentRunner{eng: eng, role: role, systemPrompt: systemPrompt, modelCfg: mcfg}, nil
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
				dagAdapter := agent.NewTaskStoreDAGAdapter(taskStore, func() []agent.DAGTaskSummary {
					raw := taskStore.ReadyTasks()
					out := make([]agent.DAGTaskSummary, len(raw))
					for i, t := range raw {
						out[i] = agent.DAGTaskSummary{
							ID: t.ID, Subject: t.Subject, Description: t.Description,
							Status: t.Status, Owner: t.Owner, DependsOn: t.DependsOn, Priority: t.Priority,
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
				teamMgr := agent.NewProductionTeamManager(agent.TeamManagerConfig{
					BaseDir:            filepath.Join(tasksStateDir, "teams"),
					Cwd:                cwd,
					Factory:            agentFactory,
					Pool:               agentPool,
					Notify:             func(_, msg string) { fmt.Println(msg) },
					LLM:                eng.APIClient,
					Roles:              agent.NewRoleRegistry(cwd),
					TaskTracker:        dagAdapter,
					Concurrency:        eng.APIClient.Guard,
					PlanConfigResolver: modelResolver,
				})

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

			ctx := context.Background()
			attachBlocks, err := ingestAttachments(flagAttach)
			if err != nil {
				return err
			}
			streamCh := eng.SubmitStreamBlocks(ctx, userPrompt, attachBlocks)
			printStreamEvents(streamCh)
			if flagEmitSession && store != nil {
				fmt.Printf("\n__CLAUDE_GO_SESSION__=%s\n", store.SessionID())
			}
			return nil
		},
	}
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
	)

	cmd := &cobra.Command{
		Use:   "feishu",
		Short: "飞书长连接后台守护模式 (WebSocket)",
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

			if config.AppID == "" || config.AppSecret == "" {
				return fmt.Errorf("需要飞书应用凭证: 使用 --app-id/--app-secret 或 --config 或设置 FEISHU_APP_ID/FEISHU_APP_SECRET")
			}

			apiKey := getAPIKey()
			baseURL := flagBaseURL
			modelAlias := flagModel

			// 如果 JSON 配置中已有 providers，优先使用
			// 否则从 CLI 参数/环境变量构建合成 provider
			if len(config.Providers) == 0 {
				if apiKey == "" {
					return fmt.Errorf("需要 AI API Key: 设置 ANTHROPIC_API_KEY 环境变量、--api-key 参数，或在配置文件中设置 providers")
				}
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
			} else {
				// 有 providers 配置时，若 CLI 指定了 model，覆盖 ModelAlias
				if cmd.Flags().Changed("model") {
					config.ModelAlias = flagModel
				}
			}

			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			if config.Cwd == "" {
				config.Cwd = cwd
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
				}
				dashCfgRef = &dashCfg

				config.Wiki.APIExtensions = append(config.Wiki.APIExtensions,
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
						fmt.Printf("[Dashboard] 已挂载到 wiki API 端口 %d (stateDir=%s)\n",
							config.Wiki.APIPort, stateDir)
					})
			}
			bot, err := feishu.NewBot(config)
			if err != nil {
				return fmt.Errorf("创建飞书机器人失败: %w", err)
			}
			botRef = bot
			_ = dashCfgRef // suppress unused warning when Wiki.APIPort == 0

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
		},
	}
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
			reg := agent.NewRoleRegistry(cwd)

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
	path, err := feishu.ResolveJSONConfigPath(flagConfig)
	if err != nil {
		return nil, "", err
	}
	if path == "" {
		return nil, "", nil
	}
	cfg, err := feishu.LoadJSONConfig(path)
	if err != nil {
		return nil, path, err
	}
	return cfg, path, nil
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

func buildEngine() (*engine.QueryEngine, error) {
	cwd, _ := os.Getwd()

	jsonCfg, jsonConfigPath, err := loadRuntimeJSONConfig()
	if err != nil {
		return nil, fmt.Errorf("加载配置文件失败: %w", err)
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
		effectiveModel = resolvedModel.ProviderName
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
		apiClient.FallbackModels = resolvedModel.FallbackModels
		apiClient.FallbackBaseURL = resolvedModel.FallbackBaseURL
		apiClient.FallbackAPIKey = resolvedModel.FallbackAPIKey
		apiClient.PromptCacheMode = resolvedModel.PromptCacheMode
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
	hookRunner := hooks.NewRunner(hookConfigs, "")
	compactor := compact.NewCompactor(apiClient, contextWindow)
	promptMgr := prompt.NewManager(cwd)
	if flagSystemPrompt != "" {
		promptMgr.CustomPrompt = flagSystemPrompt
	} else if jsonCfg != nil && jsonCfg.SystemPrompt != "" {
		promptMgr.CustomPrompt = jsonCfg.SystemPrompt
	} else if projectSettings.SystemPrompt != "" {
		promptMgr.CustomPrompt = projectSettings.SystemPrompt
	}
	skillReg := skills.NewRegistry()
	skillReg.LoadDefaults(cwd)
	if skillReg.Count() > 0 {
		// 0 = 不设软上限, 由描述字符预算约束; 所有技能名称始终可见, 保证 Skill 工具可被正确调用。
		promptMgr.SkillListing = skillReg.FormatShortListing(0)
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
	cfg := &engine.Config{
		Model:            effectiveModel,
		MaxTokens:        effectiveMaxTokens,
		MaxTurns:         effectiveMaxTurns,
		ContextWindow:    contextWindow,
		Cwd:              cwd,
		PermissionMode:   permMode,
		IsNonInteractive: flagPrint,
		Debug:            flagDebug,
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
	}

	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg, nil)
	mcp.RegisterMCPTools(reg, mcpConns)
	if skillReg.Count() > 0 {
		reg.Register(skills.NewSkillTool(skillReg))
	}

	var runAgent agent.RunAgentFunc
	runAgent = func(ctx context.Context, prompt string, opts agent.RunOptions) (string, error) {
		return runNestedAgent(ctx, deps, runAgent, prompt, opts)
	}
	reg.Register(agent.NewAgentTool(runAgent))

	eng := engine.NewQueryEngine(cfg, apiClient, reg, hookRunner, permChecker, compactor, promptMgr)

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
	eng.MemoryStore = tieredStore

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

// cliAgentRunner 为 CLI run 模式提供的简单 AgentRunner。
type cliAgentRunner struct {
	eng          *engine.QueryEngine
	role         string
	systemPrompt string
	modelCfg     modelconfig.ResolvedConfig // 为空则使用 eng.APIClient 默认
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
		client = client.ConfiguredCloneFull(
			r.modelCfg.BaseURL, r.modelCfg.APIKey, r.modelCfg.ProviderName,
			r.modelCfg.FallbackModels, r.modelCfg.FallbackBaseURL, r.modelCfg.FallbackAPIKey,
		)
	}
	role := strings.ToLower(strings.TrimSpace(r.role))
	disableTools := false
	// 做事型角色 (coder/tester/reviewer): 写 20+ 文件, 边读边改, 默认 30 turns 远远不够.
	// 实测 Phase1+Phase2 全量实现 6 分多钟才跑了 30 turns, 还没接近完成. 提到 150 留足空间.
	maxTurns := 150
	maxTokens := 16384
	if r.modelCfg.MaxTokens > 0 {
		maxTokens = r.modelCfg.MaxTokens
	}
	switch role {
	case "architect", "researcher", "planner",
		"outline-architect", "evo-architect", "game-architect":
		disableTools = true
		maxTurns = 1
		// 思考型一次性吐完整设计稿; 优先使用模型配置里的 maxTokens
		if r.modelCfg.MaxTokens > 0 {
			maxTokens = r.modelCfg.MaxTokens
		} else {
			maxTokens = 65536
		}
	}
	out, err := r.eng.RunIsolated(ctx, userPrompt, engine.IsolatedRunOptions{
		SystemPrompt: r.systemPrompt,
		MaxTurns:     maxTurns,
		MaxTokens:    maxTokens,
		Client:       client,
		DisableTools: disableTools,
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
	inThinking := false
	hasOutput := false
	for ev := range ch {
		switch ev.Kind {
		case types.StreamEventDelta:
			if ev.IsThinking {
				if flagFinalOnly {
					continue // --final-only: thinking 过程不输出
				}
				if !inThinking {
					fmt.Print("\033[2m") // dim
					inThinking = true
				}
				fmt.Print(ev.DeltaText)
			} else {
				if inThinking {
					fmt.Print("\033[0m") // reset
					inThinking = false
				}
				fmt.Print(ev.DeltaText)
			}
			hasOutput = true
		case types.StreamEventBlockDone:
			if inThinking {
				fmt.Print("\033[0m")
				inThinking = false
			}
		case types.StreamEventToolStart:
			if flagDebug {
				fmt.Printf("\n[调用工具: %s]\n", ev.ToolName)
			}
		case types.StreamEventToolDone:
			if flagDebug {
				result := ev.ToolResult
				if len(result) > 200 {
					result = result[:200] + "..."
				}
				fmt.Printf("[工具完成] %s\n", result)
			}
		case types.StreamEventMessageDone:
			// MessageDone 标志一轮 assistant 完成
		case types.StreamEventError:
			fmt.Fprintf(os.Stderr, "\n[Error] %v\n", ev.Error)
		}
	}
	if hasOutput {
		fmt.Println()
	}
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

func runNestedAgent(ctx context.Context, deps *engineDeps, runAgent agent.RunAgentFunc, agentPrompt string, opts agent.RunOptions) (string, error) {
	nestedReg := tool.NewRegistry()
	builtin.RegisterBaseTools(nestedReg, nil)
	mcp.RegisterMCPTools(nestedReg, deps.mcpConns)
	if deps.skillReg != nil && deps.skillReg.Count() > 0 {
		nestedReg.Register(skills.NewSkillTool(deps.skillReg))
	}
	nestedReg.Register(agent.NewAgentTool(runAgent))

	nestedCfg := *deps.cfg
	if opts.Model != "" {
		nestedCfg.Model = opts.Model
	}
	perm := deps.permChecker
	if opts.ReadOnly {
		nestedCfg.PermissionMode = types.PermissionModePlan
		perm = permissions.NewChecker(types.PermissionModePlan)
	}

	nestedPromptMgr := prompt.NewManager(nestedCfg.Cwd)
	nestedPromptMgr.CustomPrompt = deps.promptMgr.CustomPrompt
	nestedPromptMgr.AppendPrompt = deps.promptMgr.AppendPrompt
	nestedPromptMgr.OverridePrompt = deps.promptMgr.OverridePrompt
	nestedPromptMgr.CoordinatorPrompt = deps.promptMgr.CoordinatorPrompt
	nestedPromptMgr.AgentPrompt = deps.promptMgr.AgentPrompt
	nestedPromptMgr.Model = nestedCfg.Model
	nestedPromptMgr.SkillListing = deps.promptMgr.SkillListing
	nested := engine.NewQueryEngine(&nestedCfg, deps.apiClient, nestedReg, deps.hookRunner, perm, deps.compactor, nestedPromptMgr)

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
