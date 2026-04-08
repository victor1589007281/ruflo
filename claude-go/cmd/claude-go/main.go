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
	"strings"
	"syscall"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
	"github.com/spf13/cobra"
)

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
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "claude-go",
		Short: "Claude Code client - Go implementation",
		Long: `Claude Code 客户端的 Go 实现。
对标 review/claude/ 源码 (Tengu)，
复刻了 QueryEngine、Tool 系统、MCP 客户端、Agent Teams、
记忆系统、Hook 系统、权限系统、Compact 系统等核心能力。`,
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

	rootCmd.AddCommand(chatCmd())
	rootCmd.AddCommand(runCmd())
	rootCmd.AddCommand(feishuCmd())
	rootCmd.AddCommand(doctorCmd())
	rootCmd.AddCommand(toolsCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// chatCmd 交互式 REPL 模式。
// 对应 TS: 默认的交互式入口
func chatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "chat",
		Short: "交互式对话 (REPL 模式)",
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := buildEngine()
			if err != nil {
				return err
			}

			fmt.Println("Claude Code (Go) - 交互式模式")
			fmt.Println("输入消息开始对话, /quit 退出, /clear 清空历史")
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
				if input == "/quit" || input == "/exit" {
					break
				}
				if input == "/clear" {
					eng.ClearMessages()
					fmt.Println("[对话已清空]")
					continue
				}

				ctx := context.Background()
				ch := eng.SubmitMessage(ctx, input)

				for msg := range ch {
					printMessage(msg)
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
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := buildEngine()
			if err != nil {
				return err
			}

			userPrompt := strings.Join(args, " ")
			ctx := context.Background()
			ch := eng.SubmitMessage(ctx, userPrompt)

			for msg := range ch {
				printMessage(msg)
			}
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

支持两种配置方式:
  1. CLI 参数:    claude-go feishu --app-id=cli_xxx --app-secret=xxx
  2. JSON 配置:   claude-go feishu --config=claude-go.json
  3. 环境变量:    FEISHU_APP_ID=cli_xxx FEISHU_APP_SECRET=xxx claude-go feishu

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
		Use:   "doctor",
		Short: "系统诊断",
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
		Use:   "tools",
		Short: "列出所有可用工具",
		Run: func(cmd *cobra.Command, args []string) {
			reg := tool.NewRegistry()
			builtin.RegisterBaseTools(reg)

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

func buildEngine() (*engine.QueryEngine, error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return nil, fmt.Errorf("需要 API Key: 设置 ANTHROPIC_API_KEY 环境变量或使用 --api-key 参数")
	}

	baseURL := flagBaseURL
	if baseURL == "" {
		baseURL = os.Getenv("API_BASE_URL")
	}

	var apiClient *api.Client
	if baseURL != "" {
		apiClient = api.NewClient(baseURL, apiKey, flagModel)
	} else {
		apiClient = api.NewDashScopeClient(apiKey, flagModel)
	}

	cwd, _ := os.Getwd()

	mcpConns, err := connectMCP(context.Background(), flagMCPConfig)
	if err != nil {
		return nil, fmt.Errorf("MCP 配置: %w", err)
	}

	permMode := types.PermissionMode(flagPermission)
	permChecker := permissions.NewChecker(permMode)

	hookRunner := hooks.NewRunner(nil, "")
	compactor := compact.NewCompactor(apiClient, 200000)
	promptMgr := prompt.NewManager(cwd)
	if flagSystemPrompt != "" {
		promptMgr.CustomPrompt = flagSystemPrompt
	}

	cfg := &engine.Config{
		Model:            flagModel,
		MaxTokens:        flagMaxTokens,
		MaxTurns:         flagMaxTurns,
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
	}

	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg)
	mcp.RegisterMCPTools(reg, mcpConns)

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

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
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

func runNestedAgent(ctx context.Context, deps *engineDeps, runAgent agent.RunAgentFunc, prompt string, opts agent.RunOptions) (string, error) {
	nestedReg := tool.NewRegistry()
	builtin.RegisterBaseTools(nestedReg)
	mcp.RegisterMCPTools(nestedReg, deps.mcpConns)
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

	nested := engine.NewQueryEngine(&nestedCfg, deps.apiClient, nestedReg, deps.hookRunner, perm, deps.compactor, deps.promptMgr)

	var sb strings.Builder
	for msg := range nested.SubmitMessage(ctx, prompt) {
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
