package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/hotreload"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/types"
)

// 飞书消息长度限制 (富文本卡片约 30KB, 普通文本约 4000 字符)
const (
	maxTextMessageLen = 4000
	maxCardContentLen = 28000
)

// Bot 飞书机器人。
// 通过 WebSocket 长连接接收飞书消息事件，
// 将用户消息桥接到 claude-go 的 QueryEngine，
// 然后将 AI 回复通过飞书 IM API 发送给用户。
//
// 核心流程:
//  1. Start() 建立 WebSocket 长连接
//  2. 飞书推送 im.message.receive_v1 事件
//  3. handleMessage() 提取消息文本
//  4. SessionManager 获取/创建会话
//  5. QueryEngine 处理消息 (异步 goroutine)
//  6. sendReply() 将结果回复到飞书
//
// 关键约束:
//   - 事件回调必须 3 秒内返回 → 异步处理
//   - 群聊中默认仅响应 @机器人 的消息
//   - 支持 /clear 和 /help 等斜杠命令
type Bot struct {
	config     *BotConfig
	client     *lark.Client       // 飞书 API 客户端 (用于发送消息)
	wsClient   *larkws.Client     // WebSocket 长连接客户端
	sessions   *SessionManager    // 会话管理器
	apiClient  *api.Client        // AI API 客户端
	mcpMgr      *dynmcp.Manager     // 动态 MCP 管理器 (进程级别共享)
	skillReg    *skills.Registry    // 技能注册表 (进程级别共享)
	dreamer     *dreaming.Dreamer   // Dreaming 记忆整理引擎
	memStore    *memory.TieredStore // 多层记忆存储 (进程级别共享)
	cfgWatcher  *hotreload.Watcher  // 配置热加载监控器
	startTime   time.Time           // 启动时间
}

// NewBot 创建飞书机器人。
// 参数:
//   - config: 机器人配置 (AppID, AppSecret, AI API 配置等)
//
// 初始化流程:
//  1. 创建飞书 API 客户端 (用于主动发消息)
//  2. 创建 AI API 客户端
//  3. 创建 SessionManager
//  4. 注册消息事件处理器
//  5. 创建 WebSocket 长连接客户端
func NewBot(config *BotConfig) (*Bot, error) {
	if config.AppID == "" || config.AppSecret == "" {
		return nil, fmt.Errorf("飞书 AppID 和 AppSecret 不能为空")
	}
	if config.APIKey == "" {
		return nil, fmt.Errorf("AI API Key 不能为空")
	}

	// 确定飞书 API 域名
	var domain string
	switch config.Domain {
	case "lark":
		domain = "https://open.larksuite.com"
	default:
		domain = "https://open.feishu.cn"
	}

	// 创建飞书 API 客户端 (用于发送消息)
	larkClient := lark.NewClient(config.AppID, config.AppSecret,
		lark.WithOpenBaseUrl(domain),
	)

	// 创建 AI API 客户端
	var aiClient *api.Client
	if config.BaseURL != "" {
		aiClient = api.NewClient(config.BaseURL, config.APIKey, config.Model)
	} else {
		aiClient = api.NewDashScopeClient(config.APIKey, config.Model)
	}

	bot := &Bot{
		config:    config,
		client:    larkClient,
		apiClient: aiClient,
		startTime: time.Now(),
	}

	// 1. 初始化动态 MCP 管理器 (进程级别共享)
	bot.mcpMgr = dynmcp.NewManager()
	bot.initMCPServers(config)

	// 2. 初始化 Skills 注册表 (进程级别共享)
	bot.skillReg = skills.NewRegistry()
	bot.initSkills(config)

	// 3. 初始化 Dreaming 引擎
	bot.initDreaming(config)

	// 4. 初始化多层记忆存储
	bot.memStore = memory.NewTieredStore()

	// 5. 解析 Hook 配置
	hookConfigs := bot.parseHookConfigs(config)

	// 6. 创建会话管理器 (传入共享组件)
	bot.sessions = NewSessionManager(config, aiClient, bot.mcpMgr, bot.skillReg, bot.dreamer, bot.memStore, hookConfigs)

	// 6. 启动配置热加载 (如果有配置文件)
	if config.MCPConfigPath != "" {
		bot.startConfigWatcher(config.MCPConfigPath)
	}

	// 注册飞书事件处理器
	// dispatcher.NewEventDispatcher 的两个参数(verificationToken, encryptKey)
	// 在长连接模式下必须填空字符串 (SDK 内部处理加密鉴权)
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(bot.onMessageReceive)

	// 创建 WebSocket 长连接客户端
	logLevel := larkcore.LogLevelInfo
	if config.Debug {
		logLevel = larkcore.LogLevelDebug
	}

	var wsOpts []larkws.ClientOption
	wsOpts = append(wsOpts, larkws.WithEventHandler(eventHandler))
	wsOpts = append(wsOpts, larkws.WithLogLevel(logLevel))

	// 飞书国内使用 FeishuBaseUrl, 国际版使用 LarkBaseUrl
	if config.Domain == "lark" {
		wsOpts = append(wsOpts, larkws.WithDomain(lark.LarkBaseUrl))
	} else {
		wsOpts = append(wsOpts, larkws.WithDomain(lark.FeishuBaseUrl))
	}

	bot.wsClient = larkws.NewClient(config.AppID, config.AppSecret, wsOpts...)

	return bot, nil
}

// initMCPServers 通过动态 MCP 管理器初始化所有 MCP 连接。
func (b *Bot) initMCPServers(config *BotConfig) {
	ctx := context.Background()

	// 从 MCPConfigPath 加载
	if config.MCPConfigPath != "" {
		configs, err := mcp.LoadServerConfigsFromFile(config.MCPConfigPath)
		if err != nil {
			log.Printf("[飞书Bot] 加载 MCP 配置失败 (%s): %v", config.MCPConfigPath, err)
		} else {
			b.mcpMgr.InitFromConfigs(ctx, configs)
		}
	}

	// 从内联 MCPServers 加载
	for name, entry := range config.MCPServers {
		sc := mcp.ServerConfig{
			Name: name, Transport: entry.Transport,
			Command: entry.Command, Args: entry.Args,
			URL: entry.URL, Env: entry.Env,
		}
		if sc.Transport == "" {
			sc.Transport = "stdio"
		}
		if err := b.mcpMgr.AddServer(ctx, sc); err != nil {
			log.Printf("[飞书Bot] MCP 连接失败 (%s): %v", name, err)
		}
	}
}

// initSkills 加载技能
func (b *Bot) initSkills(config *BotConfig) {
	loaded := b.skillReg.LoadDefaults(config.Cwd)
	if len(config.SkillDirs) > 0 {
		loaded += b.skillReg.LoadFromDirs(config.SkillDirs, "config")
	}
	if loaded > 0 {
		log.Printf("[飞书Bot] Skills: 已加载 %d 个技能", loaded)
	}
}

// initDreaming 初始化 Dreaming 引擎
func (b *Bot) initDreaming(config *BotConfig) {
	dreamCfg := dreaming.DefaultDreamConfig()
	dreamCfg.Enabled = config.DreamEnabled
	if config.DreamMinHours > 0 {
		dreamCfg.MinHours = config.DreamMinHours
	}
	if config.DreamMinSessions > 0 {
		dreamCfg.MinSessions = config.DreamMinSessions
	}
	if config.DreamConsolidateMode != "" {
		dreamCfg.ConsolidateMode = config.DreamConsolidateMode
	}
	dreamCfg.MemoryDir = config.Cwd + "/.claude/memory"
	b.dreamer = dreaming.NewDreamer(dreamCfg, config.Cwd)

	// 注入 LLM API 客户端 (用于 LLM 模式整理)
	if dreamCfg.ConsolidateMode == "llm" {
		b.dreamer.SetAPIClient(b.apiClient)
	}
}

// parseHookConfigs 解析 Hook 配置
func (b *Bot) parseHookConfigs(config *BotConfig) []types.HookConfig {
	var hookConfigs []types.HookConfig
	for _, h := range config.Hooks {
		hookConfigs = append(hookConfigs, types.HookConfig{
			Event: types.HookEvent(h.Event), Command: h.Command,
			Timeout: h.Timeout, If: h.If,
		})
	}
	return hookConfigs
}

// startConfigWatcher 启动配置文件热加载监控
func (b *Bot) startConfigWatcher(configPath string) {
	b.cfgWatcher = hotreload.NewWatcher(configPath, 5*time.Second)
	b.cfgWatcher.OnChange(func(path string) {
		log.Printf("[HotReload] 配置文件变更: %s", path)
		b.reloadConfig(path)
	})
	b.cfgWatcher.Start()
}

// reloadConfig 热加载配置文件。
// 重新加载 MCP 连接和技能，但不重启飞书 WebSocket。
func (b *Bot) reloadConfig(path string) {
	configs, err := mcp.LoadServerConfigsFromFile(path)
	if err != nil {
		log.Printf("[HotReload] MCP 配置解析失败: %v", err)
		return
	}

	ctx := context.Background()

	// 比较当前连接和新配置，增删差异
	currentServers := make(map[string]bool)
	for _, info := range b.mcpMgr.ListServers() {
		currentServers[info.Name] = true
	}

	newServers := make(map[string]bool)
	for _, cfg := range configs {
		newServers[cfg.Name] = true
		if !currentServers[cfg.Name] {
			if err := b.mcpMgr.AddServer(ctx, cfg); err != nil {
				log.Printf("[HotReload] 添加 MCP %s 失败: %v", cfg.Name, err)
			}
		}
	}

	for name := range currentServers {
		if !newServers[name] {
			b.mcpMgr.RemoveServer(name)
		}
	}

	// 重新加载技能
	reloaded := b.skillReg.Reload()
	if reloaded > 0 {
		log.Printf("[HotReload] 重新加载 %d 个技能", reloaded)
	}
}

// Start 启动飞书机器人 (阻塞)。
// 建立 WebSocket 长连接，开始接收事件。
// 连接成功后会打印 "connected to wss://..."
// 主线程阻塞直到 context 取消或连接断开。
func (b *Bot) Start(ctx context.Context) error {
	log.Printf("[飞书Bot] 启动中... AppID=%s, Domain=%s, Model=%s",
		b.config.AppID, b.config.Domain, b.config.Model)
	log.Printf("[飞书Bot] 工作目录: %s", b.config.Cwd)
	log.Printf("[飞书Bot] 会话超时: %v, 最大会话数: %d",
		b.config.SessionTimeout, b.config.MaxSessions)
	servers := b.mcpMgr.ListServers()
	if len(servers) > 0 {
		log.Printf("[飞书Bot] MCP 服务器: %d 个连接", len(servers))
		for _, s := range servers {
			log.Printf("[飞书Bot]   - %s: %d 个工具, 状态=%s", s.Name, s.ToolCount, s.Status)
		}
	}
	if b.skillReg.Count() > 0 {
		log.Printf("[飞书Bot] Skills: %d 个已加载", b.skillReg.Count())
	}
	if b.dreamer != nil && b.config.DreamEnabled {
		log.Printf("[飞书Bot] Dreaming: 已启用")
	}

	return b.wsClient.Start(ctx)
}

// Shutdown 关闭所有资源
func (b *Bot) Shutdown() {
	if b.cfgWatcher != nil {
		b.cfgWatcher.Stop()
	}
	b.mcpMgr.Shutdown()
}

// onMessageReceive 处理飞书消息接收事件。
// 对应事件: im.message.receive_v1
//
// 处理流程:
//  1. 提取消息元数据 (chat_id, message_id, sender_id)
//  2. 判断消息类型 (仅处理文本消息)
//  3. 群聊中检查是否 @机器人
//  4. 解析斜杠命令 (/clear, /help, /status)
//  5. 异步处理: goroutine 中调用 SessionManager.ProcessMessage()
//  6. 发送处理中提示 → 等待结果 → 发送回复
//
// 注意: 此回调必须在 3 秒内返回, 所以 AI 处理放在 goroutine 中
func (b *Bot) onMessageReceive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}

	msg := event.Event.Message
	sender := event.Event.Sender

	chatID := deref(msg.ChatId)
	messageID := deref(msg.MessageId)
	msgType := deref(msg.MessageType)
	chatType := deref(msg.ChatType)
	senderID := ""
	if sender != nil && sender.SenderId != nil {
		senderID = deref(sender.SenderId.OpenId)
	}

	if b.config.Debug {
		log.Printf("[飞书Bot] 收到消息: chat=%s, sender=%s, type=%s, chatType=%s",
			chatID, senderID, msgType, chatType)
	}

	// 仅处理文本消息
	if msgType != "text" {
		return nil
	}

	// 提取文本内容
	content := deref(msg.Content)
	var textContent FeishuTextContent
	if err := json.Unmarshal([]byte(content), &textContent); err != nil {
		log.Printf("[飞书Bot] 解析消息内容失败: %v", err)
		return nil
	}

	userText := strings.TrimSpace(textContent.Text)

	// 群聊中检查 @机器人
	// 飞书 @mention 格式: "@_user_xxx " 前缀
	if chatType == "group" && b.config.MentionOnly {
		mentions := msg.Mentions
		if len(mentions) == 0 {
			return nil // 群聊中未 @ 机器人，忽略
		}
		// 移除 @mention 文本 (格式: @_user_N )
		for _, mention := range mentions {
			if mention.Key != nil {
				userText = strings.ReplaceAll(userText, deref(mention.Key), "")
			}
		}
		userText = strings.TrimSpace(userText)
	}

	if userText == "" {
		return nil
	}

	// 处理斜杠命令
	if handled := b.handleSlashCommand(ctx, chatID, messageID, userText); handled {
		return nil
	}

	// 异步处理消息 (避免阻塞飞书回调的 3 秒超时)
	go b.processAndReply(chatID, messageID, userText)

	return nil
}

// handleSlashCommand 处理斜杠命令
// 支持的命令:
//   - /clear: 清除当前会话历史
//   - /help:  显示帮助信息
//   - /status: 显示机器人状态
func (b *Bot) handleSlashCommand(ctx context.Context, chatID, messageID, text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))

	switch {
	case lower == "/clear" || lower == "/reset":
		b.sessions.ClearSession(chatID)
		b.sendTextReply(ctx, messageID, "对话已清除，开始新的会话。")
		return true

	case lower == "/help":
		help := "**Claude Code (Go) 飞书机器人**\n\n" +
			"直接发送消息与我对话，我可以:\n" +
			"- 编写和分析代码\n" +
			"- 执行 Shell 命令\n" +
			"- 读写和编辑文件\n" +
			"- 搜索代码库\n" +
			"- 调用 MCP 工具\n\n" +
			"**基础命令:**\n" +
			"- /clear - 清除对话历史\n" +
			"- /help - 显示此帮助\n" +
			"- /status - 查看运行状态\n" +
			"- /reload - 热加载配置\n\n" +
			"**MCP 管理:**\n" +
			"- /mcp list - 列出 MCP 服务器\n" +
			"- /mcp add <名称> <命令> [参数] - 动态添加\n" +
			"- /mcp remove <名称> - 动态移除\n\n" +
			"**技能管理:**\n" +
			"- /skill list - 列出已加载技能\n" +
			"- /skill install <名称> - 创建技能模板\n" +
			"- /skill uninstall <名称> - 卸载技能\n" +
			"- /skill reload - 重新加载技能\n\n" +
			"**Dreaming:**\n" +
			"- /dream - 手动触发记忆整理"
		b.sendTextReply(ctx, messageID, help)
		return true

	case lower == "/status":
		total, active := b.sessions.Stats()
		uptime := time.Since(b.startTime).Round(time.Second)
		mcpInfo := "无"
		servers := b.mcpMgr.ListServers()
		if len(servers) > 0 {
			var mcpNames []string
			totalTools := 0
			for _, s := range servers {
				mcpNames = append(mcpNames, fmt.Sprintf("%s(%d)", s.Name, s.ToolCount))
				totalTools += s.ToolCount
			}
			mcpInfo = fmt.Sprintf("%d 个服务器, %d 个工具\n  %s", len(servers), totalTools, strings.Join(mcpNames, ", "))
		}
		skillInfo := fmt.Sprintf("%d 个已加载", b.skillReg.Count())
		dreamInfo := "未启用"
		if b.dreamer != nil {
			ds := b.dreamer.Stats()
			if ds.Enabled {
				dreamInfo = fmt.Sprintf("已启用 (上次: %s, 待整理会话: %d, 正在整理: %v)",
					formatTimeSince(ds.LastDreamTime), ds.SessionsSinceDream, ds.IsDreaming)
			}
		}
		status := fmt.Sprintf("**运行状态**\n"+
			"- 运行时长: %v\n"+
			"- 总会话数: %d\n"+
			"- 活跃处理: %d\n"+
			"- AI 模型: %s\n"+
			"- MCP: %s\n"+
			"- Skills: %s\n"+
			"- Dreaming: %s\n"+
			"- 工作目录: %s",
			uptime, total, active, b.config.Model, mcpInfo, skillInfo, dreamInfo, b.config.Cwd)
		b.sendTextReply(ctx, messageID, status)
		return true

	case strings.HasPrefix(lower, "/mcp"):
		b.handleMCPCommand(ctx, chatID, messageID, text)
		return true

	case strings.HasPrefix(lower, "/skill"):
		b.handleSkillCommand(ctx, chatID, messageID, text)
		return true

	case lower == "/dream":
		b.handleDreamCommand(ctx, messageID)
		return true

	case lower == "/reload":
		if b.cfgWatcher != nil {
			b.cfgWatcher.ForceReload()
			b.sendTextReply(ctx, messageID, "配置已重新加载。")
		} else {
			b.sendTextReply(ctx, messageID, "未配置热加载 (启动时无 --config 参数)。")
		}
		return true
	}

	return false
}

// handleMCPCommand 处理 /mcp 命令
// 支持: /mcp list, /mcp add <name> <command> [args...], /mcp remove <name>
func (b *Bot) handleMCPCommand(ctx context.Context, chatID, messageID, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.sendTextReply(ctx, messageID, "/mcp list - 列出 MCP 服务器\n/mcp add <name> <command> [args] - 添加\n/mcp remove <name> - 移除")
		return
	}

	sub := strings.ToLower(parts[1])
	switch sub {
	case "list":
		servers := b.mcpMgr.ListServers()
		if len(servers) == 0 {
			b.sendTextReply(ctx, messageID, "无活跃 MCP 服务器。")
			return
		}
		var sb strings.Builder
		sb.WriteString("**MCP 服务器列表:**\n")
		for _, s := range servers {
			sb.WriteString(fmt.Sprintf("- **%s** [%s] %d 个工具: %s\n", s.Name, s.Status, s.ToolCount, strings.Join(s.Tools, ", ")))
		}
		b.sendTextReply(ctx, messageID, sb.String())

	case "add":
		if len(parts) < 4 {
			b.sendTextReply(ctx, messageID, "用法: /mcp add <name> <command> [args...]")
			return
		}
		name := parts[2]
		cmd := parts[3]
		args := parts[4:]
		cfg := mcp.ServerConfig{Name: name, Transport: "stdio", Command: cmd, Args: args}
		if err := b.mcpMgr.AddServer(context.Background(), cfg); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("添加失败: %v", err))
		} else {
			s := b.mcpMgr.ListServers()
			for _, si := range s {
				if si.Name == name {
					b.sendTextReply(ctx, messageID, fmt.Sprintf("已添加 MCP: %s (%d 个工具)", name, si.ToolCount))
					return
				}
			}
			b.sendTextReply(ctx, messageID, fmt.Sprintf("已添加 MCP: %s", name))
		}

	case "remove":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /mcp remove <name>")
			return
		}
		name := parts[2]
		if err := b.mcpMgr.RemoveServer(name); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("移除失败: %v", err))
		} else {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("已移除 MCP: %s", name))
		}

	default:
		b.sendTextReply(ctx, messageID, "未知 /mcp 子命令。用法: /mcp [list|add|remove]")
	}
}

// handleSkillCommand 处理 /skill 命令
// 支持: /skill list, /skill install <name> <content>, /skill uninstall <name>, /skill reload
func (b *Bot) handleSkillCommand(ctx context.Context, chatID, messageID, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.sendTextReply(ctx, messageID, "/skill list - 列出技能\n/skill reload - 重载技能\n/skill install <name> - 安装\n/skill uninstall <name> - 卸载")
		return
	}

	sub := strings.ToLower(parts[1])
	switch sub {
	case "list":
		allSkills := b.skillReg.All()
		if len(allSkills) == 0 {
			b.sendTextReply(ctx, messageID, "无已加载技能。在 .claude/skills/<name>/SKILL.md 中添加。")
			return
		}
		var sb strings.Builder
		sb.WriteString("**已加载技能:**\n")
		for _, s := range allSkills {
			sb.WriteString(fmt.Sprintf("- **%s**: %s (来源: %s)\n", s.Name, s.Description, s.LoadedFrom))
		}
		b.sendTextReply(ctx, messageID, sb.String())

	case "reload":
		count := b.skillReg.Reload()
		b.sendTextReply(ctx, messageID, fmt.Sprintf("已重载 %d 个技能。", count))

	case "install":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /skill install <name>\n(在 .claude/skills/<name>/SKILL.md 中创建文件)")
			return
		}
		name := parts[2]
		baseDir := b.config.Cwd + "/.claude/skills"
		defaultContent := fmt.Sprintf("---\nname: %s\ndescription: (描述你的技能)\nwhen_to_use: (何时使用)\n---\n# %s\n\n(技能内容)\n", name, name)
		if err := skills.InstallSkill(baseDir, name, defaultContent); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("安装失败: %v", err))
		} else {
			b.skillReg.Reload()
			b.sendTextReply(ctx, messageID, fmt.Sprintf("已创建技能模板: .claude/skills/%s/SKILL.md\n请编辑内容后发送 /skill reload。", name))
		}

	case "uninstall":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /skill uninstall <name>")
			return
		}
		name := parts[2]
		baseDir := b.config.Cwd + "/.claude/skills"
		if err := skills.UninstallSkill(baseDir, name); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("卸载失败: %v", err))
		} else {
			b.skillReg.Unregister(name)
			b.sendTextReply(ctx, messageID, fmt.Sprintf("已卸载技能: %s", name))
		}

	default:
		b.sendTextReply(ctx, messageID, "未知 /skill 子命令。用法: /skill [list|reload|install|uninstall]")
	}
}

// handleDreamCommand 处理 /dream 命令
func (b *Bot) handleDreamCommand(ctx context.Context, messageID string) {
	if b.dreamer == nil {
		b.sendTextReply(ctx, messageID, "Dreaming 未启用。在配置中设置 dreaming.enabled=true。")
		return
	}
	stats := b.dreamer.Stats()
	if stats.IsDreaming {
		b.sendTextReply(ctx, messageID, "正在进行记忆整理中...")
		return
	}
	if err := b.dreamer.ForceDream(context.Background()); err != nil {
		b.sendTextReply(ctx, messageID, fmt.Sprintf("触发 Dreaming 失败: %v", err))
	} else {
		b.sendTextReply(ctx, messageID, "已触发记忆整理 (后台执行)。")
	}
}

// formatTimeSince 格式化距今时间
func formatTimeSince(t time.Time) string {
	if t.IsZero() {
		return "从未"
	}
	d := time.Since(t)
	if d < time.Minute {
		return "刚才"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%.1f 小时前", d.Hours())
	}
	return fmt.Sprintf("%.1f 天前", d.Hours()/24)
}

// processAndReply 异步处理消息并发送回复。
// 此方法运行在独立的 goroutine 中:
//  1. 发送「正在思考」提示消息
//  2. 调用 SessionManager.ProcessMessage() 执行 AI 推理
//  3. 将结果分段发送回飞书 (考虑消息长度限制)
//  4. 如果出错，发送错误提示
func (b *Bot) processAndReply(chatID, messageID, userText string) {
	ctx := context.Background()

	// 发送「正在思考」提示
	if b.config.ThinkingMessage != "" {
		b.sendTextReply(ctx, messageID, b.config.ThinkingMessage)
	}

	// 处理消息
	response, err := b.sessions.ProcessMessage(ctx, chatID, userText)
	if err != nil {
		log.Printf("[飞书Bot] 处理消息失败: chat=%s, err=%v", chatID, err)
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("处理失败: %v", err))
		return
	}

	// 分段发送 (飞书文本消息有长度限制)
	b.sendLongMessage(ctx, chatID, response)
}

// sendTextReply 回复指定消息 (引用回复)
func (b *Bot) sendTextReply(ctx context.Context, messageID, text string) {
	content, _ := json.Marshal(FeishuTextContent{Text: text})

	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()

	resp, err := b.client.Im.Message.Reply(ctx, req)
	if err != nil {
		log.Printf("[飞书Bot] 回复消息失败: %v", err)
		return
	}
	if !resp.Success() {
		log.Printf("[飞书Bot] 回复消息失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
}

// sendTextMessage 发送文本消息到指定 chat
func (b *Bot) sendTextMessage(ctx context.Context, chatID, text string) {
	content, _ := json.Marshal(FeishuTextContent{Text: text})

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType("text").
			ReceiveId(chatID).
			Content(string(content)).
			Build()).
		Build()

	resp, err := b.client.Im.Message.Create(ctx, req)
	if err != nil {
		log.Printf("[飞书Bot] 发送消息失败: %v", err)
		return
	}
	if !resp.Success() {
		log.Printf("[飞书Bot] 发送消息失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
}

// sendCardMessage 发送卡片消息 (用于富文本/代码块)
func (b *Bot) sendCardMessage(ctx context.Context, chatID, title, markdownContent string) {
	card := FeishuCardContent{
		Config: &CardConfig{WideScreenMode: true},
		Header: &CardHeader{
			Title:    &CardText{Content: title, Tag: "plain_text"},
			Template: "blue",
		},
		Elements: []CardElement{
			{
				Tag: "markdown",
				Content: markdownContent,
			},
		},
	}

	content, _ := json.Marshal(card)

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType("interactive").
			ReceiveId(chatID).
			Content(string(content)).
			Build()).
		Build()

	resp, err := b.client.Im.Message.Create(ctx, req)
	if err != nil {
		log.Printf("[飞书Bot] 发送卡片消息失败: %v", err)
		return
	}
	if !resp.Success() {
		log.Printf("[飞书Bot] 发送卡片消息失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
}

// sendLongMessage 发送长消息 (自动分段)。
// 飞书文本消息有 ~4000 字符限制。
// 对于超长内容，先尝试卡片消息 (限制更高)，
// 如仍超限则按段落分割发送多条消息。
func (b *Bot) sendLongMessage(ctx context.Context, chatID, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}

	// 短消息直接发送文本
	if len(content) <= maxTextMessageLen {
		b.sendTextMessage(ctx, chatID, content)
		return
	}

	// 中等长度用卡片消息
	if len(content) <= maxCardContentLen {
		b.sendCardMessage(ctx, chatID, "Claude Code", content)
		return
	}

	// 超长消息: 分段发送
	chunks := splitMessage(content, maxCardContentLen)
	for i, chunk := range chunks {
		title := fmt.Sprintf("Claude Code (%d/%d)", i+1, len(chunks))
		b.sendCardMessage(ctx, chatID, title, chunk)
		// 避免发送过快触发限流
		if i < len(chunks)-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// splitMessage 按段落边界分割长文本
func splitMessage(content string, maxLen int) []string {
	if len(content) <= maxLen {
		return []string{content}
	}

	var chunks []string
	remaining := content

	for len(remaining) > 0 {
		if len(remaining) <= maxLen {
			chunks = append(chunks, remaining)
			break
		}

		// 在 maxLen 范围内找最近的段落边界
		cutPoint := maxLen
		if idx := strings.LastIndex(remaining[:maxLen], "\n\n"); idx > maxLen/2 {
			cutPoint = idx + 2
		} else if idx := strings.LastIndex(remaining[:maxLen], "\n"); idx > maxLen/2 {
			cutPoint = idx + 1
		}

		chunks = append(chunks, remaining[:cutPoint])
		remaining = remaining[cutPoint:]
	}

	return chunks
}

// deref 安全解引用字符串指针
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
