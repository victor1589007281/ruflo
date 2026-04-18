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
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/commands"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/settings"
	swarmintel "github.com/anthropic/claude-go/pkg/swarm_intel"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/session"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
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
	flagAPIKey       string
	flagBaseURL      string
	flagMaxTokens    int
	flagMaxTurns     int
	flagPermission   string
	flagSystemPrompt string
	flagPrint        bool
	flagDebug        bool
	flagMCPConfig    string
	flagResume       string // --resume <sessionID>
	flagContinue     bool   // --continue / -c
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "claude-go",
		Short: "Claude Code (Go) - AI 编程助手",
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
  --permission-mode、--system-prompt、--debug、--mcp-config

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
	rootCmd.PersistentFlags().StringVar(&flagResume, "resume", "", "恢复指定 session ID 的对话")
	rootCmd.PersistentFlags().BoolVarP(&flagContinue, "continue", "c", false, "恢复最近一次对话")

	rootCmd.AddCommand(chatCmd())
	rootCmd.AddCommand(runCmd())
	rootCmd.AddCommand(feishuCmd())
	rootCmd.AddCommand(doctorCmd())
	rootCmd.AddCommand(toolsCmd())
	rootCmd.AddCommand(skillsCmd())
	rootCmd.AddCommand(rolesCmd())
	rootCmd.AddCommand(helpCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
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

			cwd, _ := os.Getwd()
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
				cwd, _ := os.Getwd()
				cmdRegistry := commands.NewRegistry()
				commands.RegisterBuiltins(cmdRegistry)

				siCfg := swarmintel.DefaultConfig()
				siCfg.Notify = func(_, msg string) { fmt.Println(msg) }
				siEngine := swarmintel.NewEngine(eng.APIClient, siCfg)

				teamMgr := agent.NewProductionTeamManager(agent.TeamManagerConfig{
					BaseDir: filepath.Join(cwd, ".claude-go", "teams"),
					Cwd:     cwd,
					Factory: func(ctx context.Context, role, systemPrompt string) (agent.AgentRunner, error) {
						return &cliAgentRunner{eng: eng, role: role, systemPrompt: systemPrompt}, nil
					},
					Notify: func(_, msg string) { fmt.Println(msg) },
					LLM:    eng.APIClient,
					Roles:  agent.NewRoleRegistry(cwd),
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

			ctx := context.Background()
			streamCh := eng.SubmitStream(ctx, userPrompt)
			printStreamEvents(streamCh)
			return nil
		},
	}
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
			if apiKey != "" {
				config.APIKey = apiKey
			}
			if config.APIKey == "" {
				return fmt.Errorf("需要 AI API Key: 设置 ANTHROPIC_API_KEY 环境变量、--api-key 参数或 --config 中的 ai.apiKey")
			}

			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			if config.Cwd == "" {
				config.Cwd = cwd
			}

			// CLI 参数覆盖
			if cmd.Flags().Changed("model") {
				config.Model = flagModel
			}
			if cmd.Flags().Changed("base-url") {
				config.BaseURL = flagBaseURL
			}
			if cmd.Flags().Changed("max-tokens") {
				config.MaxTokens = flagMaxTokens
			}
			if cmd.Flags().Changed("max-turns") {
				config.MaxTurns = flagMaxTurns
			}
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

			bot, err := feishu.NewBot(config)
			if err != nil {
				return fmt.Errorf("创建飞书机器人失败: %w", err)
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

			fmt.Println("========================================")
			fmt.Println("  Claude Code (Go) - 飞书长连接模式")
			fmt.Println("========================================")
			fmt.Printf("  App ID:    %s\n", config.AppID)
			fmt.Printf("  Domain:    %s\n", config.Domain)
			fmt.Printf("  Model:     %s\n", config.Model)
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
			fmt.Printf("Effective skills: %s\n", formatNameList(reg.RoleSkills(args[0])))
			return nil
		},
	}
}

func buildEngine() (*engine.QueryEngine, error) {
	cwd, _ := os.Getwd()

	projectSettings := settings.LoadProjectSettings(cwd)
	projectSettings.ApplyEnv()

	apiKey := getAPIKey()
	if apiKey == "" && projectSettings.AI != nil && projectSettings.AI.APIKey != "" {
		apiKey = projectSettings.AI.APIKey
	}
	if apiKey == "" {
		return nil, fmt.Errorf("需要 API Key: 设置 ANTHROPIC_API_KEY 环境变量、--api-key 参数或 claude-go.json 中的 ai.apiKey")
	}

	effectiveModel := flagModel
	if effectiveModel == "qwen3.5-plus" && projectSettings.Model != "" {
		effectiveModel = projectSettings.Model
	}
	if effectiveModel == "qwen3.5-plus" && projectSettings.AI != nil && projectSettings.AI.Model != "" {
		effectiveModel = projectSettings.AI.Model
	}

	baseURL := flagBaseURL
	if baseURL == "" {
		baseURL = os.Getenv("API_BASE_URL")
	}
	if baseURL == "" && projectSettings.AI != nil && projectSettings.AI.BaseURL != "" {
		baseURL = projectSettings.AI.BaseURL
	}

	var apiClient *api.Client
	if baseURL != "" {
		trimmed := strings.TrimRight(baseURL, "/")
		if strings.HasSuffix(trimmed, "/anthropic") || strings.HasSuffix(trimmed, "/compatible-mode") {
			trimmed += "/v1"
		}
		apiClient = api.NewClient(trimmed, apiKey, effectiveModel)
	} else {
		apiClient = api.NewDashScopeClient(apiKey, effectiveModel)
	}

	mcpConns, err := connectMCP(context.Background(), flagMCPConfig)
	if err != nil {
		return nil, fmt.Errorf("MCP 配置: %w", err)
	}

	permMode := types.PermissionMode(flagPermission)
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

	hookRunner := hooks.NewRunner(nil, "")
	compactor := compact.NewCompactor(apiClient, 200000)
	promptMgr := prompt.NewManager(cwd)
	if flagSystemPrompt != "" {
		promptMgr.CustomPrompt = flagSystemPrompt
	} else if projectSettings.SystemPrompt != "" {
		promptMgr.CustomPrompt = projectSettings.SystemPrompt
	}
	skillReg := skills.NewRegistry()
	skillReg.LoadDefaults(cwd)
	if skillReg.Count() > 0 {
		promptMgr.SkillListing = skillReg.FormatListing()
	}

	effectiveMaxTokens := flagMaxTokens
	if effectiveMaxTokens == 16384 && projectSettings.MaxTokens > 0 {
		effectiveMaxTokens = projectSettings.MaxTokens
	}
	effectiveMaxTurns := flagMaxTurns
	if effectiveMaxTurns == 0 && projectSettings.MaxTurns > 0 {
		effectiveMaxTurns = projectSettings.MaxTurns
	}

	cfg := &engine.Config{
		Model:            effectiveModel,
		MaxTokens:        effectiveMaxTokens,
		MaxTurns:         effectiveMaxTurns,
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

	return engine.NewQueryEngine(cfg, apiClient, reg, hookRunner, permChecker, compactor, promptMgr), nil
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
}

func (r *cliAgentRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	contentJSON, _ := json.Marshal(userPrompt)
	msgs := []types.APIMessage{{Role: "user", Content: contentJSON}}
	sys := []string{r.systemPrompt}
	resp, err := r.eng.APIClient.SendMessage(ctx, msgs, sys, nil, 8192)
	if err != nil {
		return "", err
	}
	for _, c := range resp.Content {
		if c.Type == "text" {
			return c.Text, nil
		}
	}
	return "", fmt.Errorf("no text in response")
}

// printStreamEvents 消费 StreamEvent 通道，实现 token-by-token 实时输出。
func printStreamEvents(ch <-chan types.StreamEvent) {
	inThinking := false
	hasOutput := false
	for ev := range ch {
		switch ev.Kind {
		case types.StreamEventDelta:
			if ev.IsThinking {
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
