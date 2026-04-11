package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/hotreload"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/types"
	"github.com/anthropic/claude-go/pkg/vision"
	"github.com/anthropic/claude-go/pkg/wiki"
)

// 飞书消息长度限制 (富文本卡片约 30KB, 普通文本约 4000 字符)
const (
	maxTextMessageLen = 4000
	maxCardContentLen = 28000
)

// dreamAdapter 适配 dreaming.Dreamer 到 agent.DreamRecorder 接口。
type dreamAdapter struct {
	dreamer *dreaming.Dreamer
}

func (da *dreamAdapter) RecordSession(record agent.DreamSessionRecord) {
	if da.dreamer == nil {
		return
	}
	da.dreamer.RecordSession(dreaming.SessionRecord{
		ChatID:  record.ChatID,
		EndTime: record.EndTime,
		Summary: record.Summary,
	})
}

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
	mcpMgr      *dynmcp.Manager             // 动态 MCP 管理器 (进程级别共享)
	skillReg    *skills.Registry            // 技能注册表 (进程级别共享)
	dreamer     *dreaming.Dreamer           // Dreaming 记忆整理引擎
	memStore    *memory.TieredStore         // 多层记忆存储 (进程级别共享)
	teamMgr     *agent.ProductionTeamManager // 生产级 Agent Teams 管理器
	intentRec   *agent.IntentRecognizer     // 自然语言意图识别器
	taskStore   *builtin.TaskStore          // 共享 V2 Task 存储
	evolution   *agent.EvolutionEngine      // 自动进化引擎
	cfgWatcher  *hotreload.Watcher          // 配置热加载监控器
	cronSched   *agent.CronScheduler       // 定时任务调度器
	layout      *basedir.Layout             // 统一目录布局
	wikiEngine  *wiki.Engine               // LLM Wiki 知识库引擎
	visionCli   *vision.Client             // 视觉能力客户端
	skillAuto   *skills.AutoCreator        // 技能自动创建器
	startTime   time.Time                   // 启动时间
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

	// 初始化统一目录布局
	stateRoot := basedir.ResolveDefault(config.StateDir, config.Cwd)
	layout, err := basedir.NewLayout(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("初始化目录布局失败: %w", err)
	}
	if err := layout.EnsureAll(); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	// 初始化统一日志系统
	_ = logging.Init(&logging.LogConfig{
		Dir:     layout.Logs,
		Level:   "info",
		Console: true,
	})

	bot := &Bot{
		config:    config,
		client:    larkClient,
		apiClient: aiClient,
		layout:    layout,
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

	// 6. 创建共享 V2 Task 存储 (Teams + LLM 工具共用同一实例)
	bot.taskStore = builtin.NewTaskStore(layout.TasksFilePath())

	// 7. 创建 Evolution 自动进化引擎 + Role Registry
	bot.evolution = agent.NewEvolutionEngine(layout.Evolution, aiClient)
	roleReg := agent.NewRoleRegistry(config.Cwd)

	// 8. 创建会话管理器 (传入共享组件, 包括 Evolution + Roles)
	bot.sessions = NewSessionManager(config, aiClient, bot.mcpMgr, bot.skillReg, bot.dreamer, bot.memStore, hookConfigs, bot.taskStore, bot.evolution, roleReg)

	// 9. 创建 Agent Pool (动态扩缩, 参考 ruflo v3)
	agentPool := agent.NewAgentPool(bot.sessions.CreateAgentRunner, 8)

	// 10. 初始化 Agent Teams 管理器 (注入全部依赖)
	bot.teamMgr = agent.NewProductionTeamManager(agent.TeamManagerConfig{
		BaseDir:     layout.Teams,
		Factory:     bot.sessions.CreateAgentRunner,
		Notify:      func(chatID, msg string) { bot.sendLongMessage(context.Background(), chatID, msg) },
		TaskTracker: bot.taskStore,
		Pool:        agentPool,
		LLM:         aiClient,
		Evolution:   bot.evolution,
		Dreamer:     &dreamAdapter{dreamer: bot.dreamer},
		Roles:       roleReg,
	})

	// 11. 初始化意图识别器 (中文自然语言 → 自动拆解团队命令)
	bot.intentRec = agent.NewIntentRecognizer(aiClient)

	// 12. 初始化 Cron 定时任务调度器
	bot.cronSched = agent.NewCronScheduler(layout.Cron, &botCronExecutor{bot: bot})
	bot.cronSched.Start()

	// 13. 初始化 Vision 客户端
	bot.visionCli = vision.NewClient(config.APIKey)

	// 14. 初始化 Wiki 引擎 (独立 git 仓库)
	wikiRepoDir := layout.Wiki
	if home, err := os.UserHomeDir(); err == nil {
		wikiRepoDir = home + "/knowledge-wiki"
	}
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	bot.wikiEngine = wiki.NewEngine(wikiRepoDir, config.APIKey, baseURL, config.Model)

	// 15. 初始化技能自动创建器 (Hermes-agent 特性吸收)
	bot.skillAuto = skills.NewAutoCreator(layout.Skills, aiClient, config.Model, bot.skillReg)

	// 16. 启动配置热加载 (如果有配置文件)
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
	if b.layout != nil {
		dreamCfg.MemoryDir = b.layout.Memory
	} else {
		dreamCfg.MemoryDir = config.Cwd + "/.claude/memory"
	}
	b.dreamer = dreaming.NewDreamer(dreamCfg, config.Cwd)

	// 始终注入 LLM API 客户端，dreamer 自动判断: 有 APIClient 则 LLM 整理, 否则本地整理
	b.dreamer.SetAPIClient(b.apiClient)
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
	if b.cronSched != nil {
		ct, ce, _ := b.cronSched.Stats()
		if ct > 0 {
			log.Printf("[飞书Bot] Cron: %d 个定时任务 (%d 启用)", ct, ce)
		}
	}

	return b.wsClient.Start(ctx)
}

// Shutdown 关闭所有资源
func (b *Bot) Shutdown() {
	if b.cronSched != nil {
		b.cronSched.Stop()
	}
	if b.cfgWatcher != nil {
		b.cfgWatcher.Stop()
	}
	b.mcpMgr.Shutdown()
}

// --- Cron 执行器适配器 ---

type botCronExecutor struct {
	bot *Bot
}

func (e *botCronExecutor) RunWorkflow(ctx context.Context, name, workflow, objective, chatID string) error {
	_, err := e.bot.teamMgr.CreateTeam(name, workflow, objective, chatID)
	if err != nil {
		return err
	}
	return e.bot.teamMgr.RunTeam(name, objective)
}

func (e *botCronExecutor) SendQuery(ctx context.Context, chatID, message string) (string, error) {
	return e.bot.sessions.ProcessMessage(ctx, chatID, message)
}

func (e *botCronExecutor) RunCommand(ctx context.Context, chatID, command string) error {
	_, err := e.bot.sessions.ProcessMessage(ctx, chatID, command)
	return err
}

func (e *botCronExecutor) Notify(chatID, message string) {
	e.bot.sendLongMessage(context.Background(), chatID, message)
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

	// 支持文本和图片消息
	if msgType != "text" && msgType != "image" {
		return nil
	}

	// 图片消息: 使用 Vision 能力进行理解
	if msgType == "image" {
		go b.handleImageMessage(chatID, messageID, msg)
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

	// Cron 意图识别: 自然语言定时任务 (优先于团队意图)
	if cronIntent := b.intentRec.RecognizeCron(ctx, userText); cronIntent != nil {
		go b.handleCronIntent(chatID, messageID, cronIntent)
		return nil
	}

	// 意图识别: 中文自然语言 → 自动拆解为团队操作 (MCP 感知, 零侵入, 不匹配则透传)
	hasMCP := len(b.mcpMgr.ListServers()) > 0
	if intent := b.intentRec.RecognizeWithMCPAwareness(ctx, userText, hasMCP); intent != nil && intent.Confidence >= 0.7 {
		go b.handleTeamIntent(chatID, messageID, intent)
		return nil
	}

	// Wiki URL 自动检测: 消息中的 URL 自动 Ingest 到知识库
	if urls := extractURLs(userText); len(urls) > 0 && b.wikiEngine != nil {
		go b.handleWikiIngest(chatID, messageID, urls)
	}

	// 异步处理消息 (避免阻塞飞书回调的 3 秒超时)
	go b.processAndReply(chatID, messageID, userText)

	return nil
}

// handleImageMessage 处理图片消息，使用 Vision 能力进行理解。
func (b *Bot) handleImageMessage(chatID, messageID string, msg *larkim.EventMessage) {
	ctx := context.Background()
	b.sendTextReply(ctx, messageID, "🔍 正在分析图片...")

	// 获取图片内容 (飞书图片需要通过 API 下载)
	imageKey := ""
	content := deref(msg.Content)
	var imgContent struct {
		ImageKey string `json:"image_key"`
	}
	if json.Unmarshal([]byte(content), &imgContent) == nil {
		imageKey = imgContent.ImageKey
	}

	if imageKey == "" || b.visionCli == nil {
		b.sendTextMessage(ctx, chatID, "无法获取图片内容或视觉能力未初始化。")
		return
	}

	response, err := vision.Understand(ctx, b.config.APIKey, "qwen-vl-plus", imageKey,
		"请详细描述这张图片的内容，包括文字、图表、数据等所有关键信息。")
	if err != nil {
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("图片分析失败: %v", err))
		return
	}

	b.sendLongMessage(ctx, chatID, "🖼️ **图片分析结果:**\n\n"+response)
}

// handleWikiIngest 自动将 URL 内容提取到 Wiki 知识库。
func (b *Bot) handleWikiIngest(chatID, messageID string, urls []string) {
	ctx := context.Background()
	for _, u := range urls {
		if err := b.wikiEngine.Ingest(ctx, u); err != nil {
			logging.For("wiki").Warn("URL Ingest 失败", "url", u, "error", err)
			continue
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("📚 已自动提取到知识库: %s", u))
	}
}

// extractURLs 从文本中提取 HTTP(S) URL。
var urlRegexp = regexp.MustCompile(`https?://[^\s<>"{}|\\^` + "`" + `\[\]]+`)

func extractURLs(text string) []string {
	return urlRegexp.FindAllString(text, -1)
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
			"- /dream - 手动触发记忆整理\n\n" +
			"**定时任务 (Cron):**\n" +
			"- /cron list - 列出所有定时任务\n" +
			"- /cron add <表达式> <类型> <内容> - 添加\n" +
			"- /cron remove <ID> - 删除\n" +
			"- /cron pause/resume <ID> - 暂停/恢复\n" +
			"- 直接说「每天9点帮我分析XXX」自动创建\n\n" +
			"**Agent Teams (多Agent协作):**\n" +
			"*自然语言模式 (推荐):*\n" +
			"- 直接说「帮我调研XXX」→ 自动创建 research 团队\n" +
			"- 直接说「帮我开发XXX」→ 自动创建 development 团队\n" +
			"- 直接说「帮我辩论XXX」→ 自动创建 debate 团队\n" +
			"- 说「蜂群模式分析XXX」→ 自动创建 swarm 团队 (Kimi K2.5 蜂群)\n" +
			"- 说「团队进展如何」→ 查看所有团队状态\n" +
			"- 说「停止团队」→ 停止执行中的团队\n\n" +
			"*命令模式:*\n" +
			"- /team create <名称> <工作流> - 创建团队\n" +
			"- /team run <名称> <目标> - 启动执行\n" +
			"- /team status [名称] - 查看状态\n" +
			"- /team stop <名称> - 停止\n" +
			"- /team list - 列出所有\n" +
			"- /team delete <名称> - 删除\n" +
			"- /team workflows - 查看工作流"
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
		evoInfo := "未启用"
		if b.evolution != nil {
			es := b.evolution.Stats()
			evoInfo = fmt.Sprintf("经验 %d 条 (角色:%d, 错误:%d, 通用:%d), 轨迹 %d 条",
				es.TotalExperiences, es.RoleExperiences, es.ErrorPatterns, es.GeneralPrinciples, es.TotalTrajectories)
		}
		cronInfo := "未初始化"
		if b.cronSched != nil {
			ct, ce, cr := b.cronSched.Stats()
			cronInfo = fmt.Sprintf("%d 个任务 (%d 启用), 已执行 %d 次", ct, ce, cr)
		}
		status := fmt.Sprintf("**运行状态**\n"+
			"- 运行时长: %v\n"+
			"- 总会话数: %d\n"+
			"- 活跃处理: %d\n"+
			"- AI 模型: %s\n"+
			"- MCP: %s\n"+
			"- Skills: %s\n"+
			"- Dreaming: %s\n"+
			"- Evolution: %s\n"+
			"- Cron: %s\n"+
			"- 工作目录: %s",
			uptime, total, active, b.config.Model, mcpInfo, skillInfo, dreamInfo, evoInfo, cronInfo, b.config.Cwd)
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

	case strings.HasPrefix(lower, "/team"):
		b.handleTeamCommand(ctx, chatID, messageID, text)
		return true

	case strings.HasPrefix(lower, "/cron"):
		b.handleCronCommand(ctx, chatID, messageID, text)
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
		skillDir := b.config.Cwd + "/.claude/skills"
		if b.layout != nil {
			skillDir = b.layout.Skills
		}
		defaultContent := fmt.Sprintf("---\nname: %s\ndescription: (描述你的技能)\nwhen_to_use: (何时使用)\n---\n# %s\n\n(技能内容)\n", name, name)
		if err := skills.InstallSkill(skillDir, name, defaultContent); err != nil {
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
		uninstallDir := b.config.Cwd + "/.claude/skills"
		if b.layout != nil {
			uninstallDir = b.layout.Skills
		}
		if err := skills.UninstallSkill(uninstallDir, name); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("卸载失败: %v", err))
		} else {
			b.skillReg.Unregister(name)
			b.sendTextReply(ctx, messageID, fmt.Sprintf("已卸载技能: %s", name))
		}

	default:
		b.sendTextReply(ctx, messageID, "未知 /skill 子命令。用法: /skill [list|reload|install|uninstall]")
	}
}

// handleTeamCommand 处理 /team 命令族
// 支持: create, run, status, stop, list, delete, msg, workflows
func (b *Bot) handleTeamCommand(ctx context.Context, chatID, messageID, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.sendTextReply(ctx, messageID, "用法: /team [create|run|status|stop|list|delete|workflows]")
		return
	}

	sub := strings.ToLower(parts[1])
	switch sub {
	case "create":
		if len(parts) < 4 {
			b.sendTextReply(ctx, messageID, "用法: /team create <名称> <工作流>\n工作流: development, research, debate")
			return
		}
		name, workflow := parts[2], parts[3]
		desc := ""
		if len(parts) > 4 {
			desc = strings.Join(parts[4:], " ")
		}
		team, err := b.teamMgr.CreateTeam(name, workflow, desc, chatID)
		if err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("创建失败: %v", err))
			return
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("✅ 团队 **%s** 已创建\n工作流: %s\nAgent: %d\n\n发送 `/team run %s <目标>` 启动执行",
			team.Name, team.Workflow, len(team.Agents), team.Name))

	case "run":
		if len(parts) < 4 {
			b.sendTextReply(ctx, messageID, "用法: /team run <名称> <目标描述>")
			return
		}
		name := parts[2]
		objective := strings.Join(parts[3:], " ")
		if err := b.teamMgr.RunTeam(name, objective); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("启动失败: %v", err))
			return
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("🚀 团队 **%s** 已启动, 后台执行中...\n发送 `/team status %s` 查看进度", name, name))

	case "status":
		if len(parts) >= 3 {
			team := b.teamMgr.GetTeam(parts[2])
			if team == nil {
				b.sendTextReply(ctx, messageID, fmt.Sprintf("团队 %q 不存在", parts[2]))
				return
			}
			b.sendTextReply(ctx, messageID, team.FormatStatus())
		} else {
			teams := b.teamMgr.ListAllTeams()
			if len(teams) == 0 {
				b.sendTextReply(ctx, messageID, "无活跃团队。发送 `/team create <名称> <工作流>` 创建。")
				return
			}
			var sb strings.Builder
			sb.WriteString("**所有团队:**\n")
			for _, t := range teams {
				sb.WriteString(fmt.Sprintf("- **%s** [%s] 工作流=%s", t.Name, t.Status, t.Workflow))
				if t.Objective != "" {
					sb.WriteString(fmt.Sprintf(" 目标=%s", truncateForDream(t.Objective)))
				}
				sb.WriteString("\n")
			}
			b.sendTextReply(ctx, messageID, sb.String())
		}

	case "stop":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /team stop <名称>")
			return
		}
		if err := b.teamMgr.StopTeam(parts[2]); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("停止失败: %v", err))
		} else {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("⏹️ 团队 **%s** 已停止", parts[2]))
		}

	case "delete":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /team delete <名称>")
			return
		}
		if err := b.teamMgr.DeleteTeam(parts[2]); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("删除失败: %v", err))
		} else {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("🗑️ 团队 **%s** 已删除", parts[2]))
		}

	case "list":
		teams := b.teamMgr.ListAllTeams()
		if len(teams) == 0 {
			b.sendTextReply(ctx, messageID, "无团队。发送 `/team create <名称> <工作流>` 创建。")
			return
		}
		var sb strings.Builder
		sb.WriteString("**团队列表:**\n")
		for _, t := range teams {
			sb.WriteString(fmt.Sprintf("- **%s** [%s] %s\n", t.Name, t.Status, t.Workflow))
		}
		b.sendTextReply(ctx, messageID, sb.String())

	case "msg":
		if len(parts) < 5 {
			b.sendTextReply(ctx, messageID, "用法: /team msg <团队> <Agent> <消息>")
			return
		}
		teamName, agentName := parts[2], parts[3]
		content := strings.Join(parts[4:], " ")
		if err := b.teamMgr.SendMailMessage(teamName, "user", agentName, content); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("发送失败: %v", err))
		} else {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("📨 已发送消息给 %s/%s", teamName, agentName))
		}

	case "workflows":
		wfs := agent.ListWorkflows()
		var sb strings.Builder
		sb.WriteString("**可用工作流:**\n\n")
		for _, wf := range wfs {
			sb.WriteString(fmt.Sprintf("**%s** — %s\n", wf.Name, wf.Description))
			sb.WriteString("  阶段: ")
			for i, s := range wf.Stages {
				if i > 0 {
					sb.WriteString(" → ")
				}
				sb.WriteString(s.Role)
			}
			sb.WriteString("\n\n")
		}
		b.sendTextReply(ctx, messageID, sb.String())

	default:
		b.sendTextReply(ctx, messageID, "未知子命令。用法: /team [create|run|status|stop|list|delete|msg|workflows]")
	}
}

// handleTeamIntent 处理意图识别结果 — 中文自然语言自动驱动团队操作。
func (b *Bot) handleTeamIntent(chatID, messageID string, intent *agent.TeamIntent) {
	ctx := context.Background()
	switch intent.Action {
	case "create_and_run":
		team, err := b.teamMgr.CreateTeam(intent.TeamName, intent.Workflow, intent.Objective, chatID)
		if err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("自动创建团队失败: %v", err))
			return
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf(
			"🤖 已识别为多Agent协作任务, 自动创建并启动:\n"+
				"- 团队: **%s**\n"+
				"- 工作流: %s\n"+
				"- 目标: %s\n"+
				"- Agent数: %d\n\n"+
				"后台执行中, 完成后自动通知。发送「团队进展如何」查看进度。",
			team.Name, team.Workflow, truncateForDream(intent.Objective), len(team.Agents)))
		if err := b.teamMgr.RunTeam(intent.TeamName, intent.Objective); err != nil {
			b.sendTextMessage(ctx, chatID, fmt.Sprintf("启动失败: %v", err))
		}

	case "check_status":
		teams := b.teamMgr.ListAllTeams()
		if len(teams) == 0 {
			b.sendTextReply(ctx, messageID, "当前没有活跃的团队。")
			return
		}
		var sb strings.Builder
		for _, t := range teams {
			sb.WriteString(t.FormatStatus())
			sb.WriteString("\n---\n")
		}
		b.sendTextReply(ctx, messageID, sb.String())

	case "stop":
		name, err := b.teamMgr.StopFirstRunning()
		if err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("停止失败: %v", err))
		} else {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("⏹️ 已停止团队 **%s**", name))
		}

	case "list":
		teams := b.teamMgr.ListAllTeams()
		if len(teams) == 0 {
			b.sendTextReply(ctx, messageID, "无团队。")
			return
		}
		var sb strings.Builder
		sb.WriteString("**团队列表:**\n")
		for _, t := range teams {
			sb.WriteString(fmt.Sprintf("- **%s** [%s] %s\n", t.Name, t.Status, t.Workflow))
		}
		b.sendTextReply(ctx, messageID, sb.String())
	}
}

// handleCronCommand 处理 /cron 命令族。
// 支持: list, add, remove, pause, resume, status
func (b *Bot) handleCronCommand(ctx context.Context, chatID, messageID, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.sendTextReply(ctx, messageID, "**定时任务管理:**\n"+
			"- /cron list — 列出所有定时任务\n"+
			"- /cron add <cron表达式> <类型> <内容> — 添加任务\n"+
			"- /cron remove <ID> — 删除任务\n"+
			"- /cron pause <ID> — 暂停任务\n"+
			"- /cron resume <ID> — 恢复任务\n"+
			"- /cron status — 调度器状态\n\n"+
			"**类型:** workflow:<工作流>, query, command\n"+
			"**示例:** `/cron add \"0 9 * * 1-5\" workflow:finance 分析AAPL和TSLA行情`\n\n"+
			"**自然语言:** 直接说「每天早上9点帮我分析AAPL股票」即可自动创建")
		return
	}

	sub := strings.ToLower(parts[1])
	switch sub {
	case "list":
		b.sendTextReply(ctx, messageID, b.cronSched.FormatJobList())

	case "add":
		if len(parts) < 5 {
			b.sendTextReply(ctx, messageID, "用法: /cron add <cron表达式> <类型> <内容>\n"+
				"示例: /cron add \"0 9 * * 1-5\" workflow:finance 分析AAPL\n"+
				"类型: workflow:<name>, query, command")
			return
		}
		schedule := strings.Trim(parts[2], "\"")
		// 如果 cron 表达式被拆分为多个 part (因未加引号), 尝试合并
		typeIdx := 3
		for i := 3; i < len(parts); i++ {
			if strings.Contains(parts[i], ":") || parts[i] == "query" || parts[i] == "command" {
				typeIdx = i
				break
			}
			schedule += " " + parts[i]
		}
		if typeIdx >= len(parts) {
			b.sendTextReply(ctx, messageID, "缺少任务类型。用法: /cron add <表达式> <类型> <内容>")
			return
		}

		jobType := "query"
		workflow := ""
		typePart := parts[typeIdx]
		if strings.HasPrefix(typePart, "workflow:") {
			jobType = "workflow"
			workflow = strings.TrimPrefix(typePart, "workflow:")
		} else if typePart == "command" {
			jobType = "command"
		}

		payload := strings.Join(parts[typeIdx+1:], " ")
		if payload == "" {
			b.sendTextReply(ctx, messageID, "缺少任务内容。")
			return
		}

		job := &agent.CronJob{
			Name:     fmt.Sprintf("cron-%d", time.Now().Unix()%10000),
			Schedule: schedule,
			JobType:  jobType,
			Workflow: workflow,
			Payload:  payload,
			ChatID:   chatID,
		}
		if err := b.cronSched.AddJob(job); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("添加失败: %v", err))
			return
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("✅ 已添加定时任务:\n"+
			"- ID: `%s`\n"+
			"- 调度: `%s`\n"+
			"- 类型: %s\n"+
			"- 内容: %s",
			job.ID, job.Schedule, job.JobType, truncateForDream(job.Payload)))

	case "remove", "delete":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /cron remove <ID>")
			return
		}
		if err := b.cronSched.RemoveJob(parts[2]); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("删除失败: %v", err))
		} else {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("🗑️ 已删除定时任务: %s", parts[2]))
		}

	case "pause":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /cron pause <ID>")
			return
		}
		if err := b.cronSched.PauseJob(parts[2]); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("暂停失败: %v", err))
		} else {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("⏸️ 已暂停: %s", parts[2]))
		}

	case "resume":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /cron resume <ID>")
			return
		}
		if err := b.cronSched.ResumeJob(parts[2]); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("恢复失败: %v", err))
		} else {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("▶️ 已恢复: %s", parts[2]))
		}

	case "status":
		total, enabled, totalRuns := b.cronSched.Stats()
		b.sendTextReply(ctx, messageID, fmt.Sprintf("**Cron 调度器状态:**\n"+
			"- 总任务数: %d\n"+
			"- 启用中: %d\n"+
			"- 总执行次数: %d", total, enabled, totalRuns))

	default:
		b.sendTextReply(ctx, messageID, "未知 /cron 子命令。用法: /cron [list|add|remove|pause|resume|status]")
	}
}

// handleCronIntent 处理自然语言 cron 意图。
func (b *Bot) handleCronIntent(chatID, messageID string, intent *agent.CronIntent) {
	ctx := context.Background()

	job := &agent.CronJob{
		Name:     fmt.Sprintf("auto-%d", time.Now().Unix()%10000),
		Schedule: intent.Schedule,
		JobType:  intent.JobType,
		Workflow: intent.Workflow,
		Payload:  intent.Payload,
		ChatID:   chatID,
	}
	if err := b.cronSched.AddJob(job); err != nil {
		b.sendTextReply(ctx, messageID, fmt.Sprintf("创建定时任务失败: %v", err))
		return
	}
	b.sendTextReply(ctx, messageID, fmt.Sprintf("⏰ 已识别为定时任务, 自动创建:\n"+
		"- ID: `%s`\n"+
		"- 调度: **%s** (`%s`)\n"+
		"- 类型: %s\n"+
		"- 内容: %s\n\n"+
		"发送 `/cron list` 查看所有定时任务, `/cron remove %s` 删除。",
		job.ID, intent.SchedDesc, intent.Schedule, intent.JobType, truncateForDream(intent.Payload), job.ID))
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
//  1. 检测任务复杂度，复杂任务自动注入 Plan 指令
//  2. 发送「正在思考」提示消息
//  3. 调用 SessionManager.ProcessMessage() 执行 AI 推理
//  4. 将结果分段发送回飞书 (考虑消息长度限制)
//  5. 如果出错，发送错误提示
func (b *Bot) processAndReply(chatID, messageID, userText string) {
	ctx := context.Background()

	// Auto Plan: 复杂任务自动注入规划指令
	if detectComplexity(userText) {
		userText = "[Auto Plan] 这是一个复杂任务，请先进入 Plan 模式进行分析和规划，" +
			"调用 EnterPlanMode，分析需求、设计方案，然后调用 ExitPlanMode 附带完整计划，再逐步执行。\n\n" +
			"原始任务:\n" + userText
		b.sendTextReply(ctx, messageID, "🧠 检测到复杂任务，自动启用 Plan 模式进行规划...")
	}

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

// detectComplexity 检测用户消息是否为复杂任务。
// 满足以下任一条件即视为复杂:
//   - 文本长度 > 200 字符
//   - 包含多个子任务连接词 (并且/然后/同时/另外/还需要/以及/第一/第二)
//   - 包含多个技术关键词组合 (架构+设计, 重构+测试, API+数据库 等)
func detectComplexity(text string) bool {
	if len([]rune(text)) > 200 {
		return true
	}
	conjunctions := []string{"并且", "然后", "同时", "另外", "还需要", "以及", "此外", "接着"}
	conjCount := 0
	lower := strings.ToLower(text)
	for _, c := range conjunctions {
		if strings.Contains(lower, c) {
			conjCount++
		}
	}
	if conjCount >= 2 {
		return true
	}
	// 数字编号列表 (1. xxx 2. xxx)
	numberedItems := 0
	for _, prefix := range []string{"1.", "2.", "3.", "4.", "5.", "1、", "2、", "3、", "4、", "一、", "二、", "三、"} {
		if strings.Contains(text, prefix) {
			numberedItems++
		}
	}
	if numberedItems >= 3 {
		return true
	}
	techKW := []string{"架构", "重构", "迁移", "设计", "实现", "开发", "部署", "优化", "分析", "测试"}
	techCount := 0
	for _, kw := range techKW {
		if strings.Contains(text, kw) {
			techCount++
		}
	}
	if techCount >= 3 {
		return true
	}
	return false
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
