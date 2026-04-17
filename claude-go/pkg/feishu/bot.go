package feishu

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/browser"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/hotreload"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
	"github.com/anthropic/claude-go/pkg/vision"
	"github.com/anthropic/claude-go/pkg/wiki"
	swarm_intel "github.com/anthropic/claude-go/pkg/swarm_intel"
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

// dagTaskAdapter 适配 builtin.TaskStore 到 agent.DAGTaskTracker 接口。
// 桥接 builtin.TaskSummary → agent.DAGTaskSummary 类型差异。
type dagTaskAdapter struct {
	store *builtin.TaskStore
}

func (d *dagTaskAdapter) AddTask(subject, description, owner string) (string, error) {
	return d.store.AddTask(subject, description, owner)
}
func (d *dagTaskAdapter) SetTaskStatus(id, status string) error {
	return d.store.SetTaskStatus(id, status)
}
func (d *dagTaskAdapter) AddTaskWithDeps(subject, description, owner string, dependsOn []string, priority int) (string, error) {
	return d.store.AddTaskWithDeps(subject, description, owner, dependsOn, priority)
}
func (d *dagTaskAdapter) ReadyTasks() []agent.DAGTaskSummary {
	v2Tasks := d.store.ReadyTasks()
	result := make([]agent.DAGTaskSummary, len(v2Tasks))
	for i, t := range v2Tasks {
		result[i] = agent.DAGTaskSummary{
			ID: t.ID, Subject: t.Subject, Description: t.Description,
			Status: t.Status, Owner: t.Owner, DependsOn: t.DependsOn,
			Priority: t.Priority,
		}
	}
	return result
}
func (d *dagTaskAdapter) SetTaskStatusAndUnblock(id, status string) (int, error) {
	return d.store.SetTaskStatusAndUnblock(id, status)
}

// memoryAdapter 适配 memory.TieredStore 到 agent.MemoryWriter 接口。
// 团队完成后写入高权重记忆，确保团队名+目标可被 BM25 检索到。
type memoryAdapter struct {
	store *memory.TieredStore
}

func (ma *memoryAdapter) AddTeamMemory(teamName, workflow, objective, summary string) {
	if ma.store == nil {
		return
	}
	content := fmt.Sprintf("团队 %s (工作流: %s) 执行完成。\n目标: %s\n\n结果摘要:\n%s",
		teamName, workflow, objective, summary)
	if len(content) > 3000 {
		content = content[:3000] + "...(截断)"
	}
	ma.store.Add(&memory.MemoryEntry{
		Content:    content,
		Topics:     []string{teamName, workflow, "team_result"},
		Source:     "team_result",
		Importance: 0.9, // 高权重: 团队产出是重要的长期记忆
	})
}

// wikiBrowserAdapter 适配 browser.Client 到 wiki.BrowserFetcher 接口。
type wikiBrowserAdapter struct {
	client *browser.Client
}

func (w *wikiBrowserAdapter) Available() bool {
	return w.client.Available()
}

func (w *wikiBrowserAdapter) Fetch(ctx context.Context, url string) (title, text, html string, err error) {
	result, err := w.client.Fetch(ctx, url)
	if err != nil {
		return "", "", "", err
	}
	return result.Title, result.Text, result.HTML, nil
}

// browserSearchAdapter 适配 browser.Client 到 builtin.WebSearcher 接口。
type browserSearchAdapter struct {
	client *browser.Client
}

func (a *browserSearchAdapter) Search(ctx context.Context, query string) (title, text, html string, err error) {
	result, err := a.client.Search(ctx, query)
	if err != nil {
		return "", "", "", err
	}
	return result.Title, result.Text, result.HTML, nil
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
	client     *lark.Client                 // 飞书 API 客户端 (用于发送消息)
	wsClient   *larkws.Client               // WebSocket 长连接客户端
	sessions   *SessionManager              // 会话管理器
	apiClient  *api.Client                  // AI API 客户端
	mcpMgr     *dynmcp.Manager              // 动态 MCP 管理器 (进程级别共享)
	skillReg   *skills.Registry             // 技能注册表 (进程级别共享)
	dreamer    *dreaming.Dreamer            // Dreaming 记忆整理引擎
	memStore   *memory.TieredStore          // 多层记忆存储 (进程级别共享)
	teamMgr    *agent.ProductionTeamManager // 生产级 Agent Teams 管理器
	intentRec  *agent.IntentRecognizer      // 自然语言意图识别器
	taskStore  *builtin.TaskStore           // 共享 V2 Task 存储
	evolution  *agent.EvolutionEngine       // 自动进化引擎
	cfgWatcher *hotreload.Watcher           // 配置热加载监控器
	cronSched  *agent.CronScheduler         // 定时任务调度器
	layout     *basedir.Layout              // 统一目录布局
	wikiEngine  *wiki.Engine                 // LLM Wiki 知识库引擎
	swarmEngine *swarm_intel.Engine           // 群体智能预测引擎
	visionCli   *vision.Client               // 视觉能力客户端
	skillAuto  *skills.AutoCreator          // 技能自动创建器
	startTime  time.Time                    // 启动时间

	// 消息去重: 防止同一条消息触发多个团队
	processedMsgs sync.Map // messageID → timestamp
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

	// 4. 初始化多层记忆存储 (v2: 磁盘持久化, 解决重启后失忆)
	bot.memStore = memory.NewTieredStoreWithPersist(bot.layout.Memory)

	// 5. 解析 Hook 配置
	hookConfigs := bot.parseHookConfigs(config)

	// 6. 创建共享 V2 Task 存储 (Teams + LLM 工具共用同一实例)
	bot.taskStore = builtin.NewTaskStore(layout.TasksFilePath())

	// 7. 创建 Evolution 自动进化引擎 + Role Registry
	bot.evolution = agent.NewEvolutionEngine(layout.Evolution, aiClient)
	roleReg := agent.NewRoleRegistry(config.Cwd)

	// 8. 创建会话管理器 (传入共享组件, 包括 Evolution + Roles)
	bot.sessions = NewSessionManager(config, aiClient, bot.mcpMgr, bot.skillReg, bot.dreamer, bot.memStore, hookConfigs, bot.taskStore, bot.evolution, roleReg)
	bot.sessions.SetMediaSendFn(bot.SendMediaToChat)

	// 9. 创建 Agent Pool (动态扩缩, 参考 ruflo v3)
	agentPool := agent.NewAgentPool(bot.sessions.CreateAgentRunner, 8)

	// 10. 初始化 Agent Teams 管理器 (注入全部依赖)
	bot.teamMgr = agent.NewProductionTeamManager(agent.TeamManagerConfig{
		BaseDir: layout.Teams,
		Cwd:     config.Cwd,
		Factory: bot.sessions.CreateAgentRunner,
		Notify:  func(chatID, msg string) { bot.sendLongMessage(context.Background(), chatID, msg) },
		MediaNotify: func(chatID string, data []byte, filename, mediaType string) error {
			ctx := context.Background()
			switch mediaType {
			case "image":
				return bot.sendImageMessage(ctx, chatID, data, filename)
			default:
				return bot.sendFileMessage(ctx, chatID, data, filename, "stream")
			}
		},
		TaskTracker: &dagTaskAdapter{store: bot.taskStore},
		Pool:        agentPool,
		LLM:         aiClient,
		Evolution:   bot.evolution,
		Dreamer:     &dreamAdapter{dreamer: bot.dreamer},
		Roles:       roleReg,
	})

	// 10a. 修复: SetTeamManager 必须在 teamMgr 创建后调用 (之前因时序 bug 注入了 nil)
	bot.sessions.SetTeamManager(bot.teamMgr)

	// 10b. 注入记忆写入 (团队完成后高权重记忆可被检索)
	bot.teamMgr.SetMemoryWriter(&memoryAdapter{store: bot.memStore})

	// 10c-extra. 注入 LLM 事件回调 → 飞书通知 (限流/熔断/致命错误时主动推送)
	aiClient.OnLLMEvent = func(eventType, detail string) {
		icon := "ℹ️"
		switch eventType {
		case "retry":
			icon = "🔄"
		case "circuit_open":
			icon = "🔴"
		case "circuit_close":
			icon = "🟢"
		case "fatal":
			icon = "🚨"
		}
		msg := fmt.Sprintf("%s **LLM 事件 [%s]**\n%s", icon, eventType, detail)
		// 广播到所有活跃团队的 chatID
		if bot.teamMgr != nil {
			for _, t := range bot.teamMgr.ListAllTeams() {
				if t.Status == agent.TeamStatusRunning && t.ChatID != "" {
					bot.sendLongMessage(context.Background(), t.ChatID, msg)
				}
			}
		}
		log.Printf("[LLM事件] %s: %s", eventType, detail)
	}

	// 10c. 注入持续观测指标到 Dreamer (复用 teamMgr 的 Collector)
	if bot.dreamer != nil && bot.teamMgr.Metrics() != nil {
		bot.dreamer.MetricsRecorder = bot.teamMgr.Metrics()
	}

	// 11. 初始化意图识别器 (中文自然语言 → 自动拆解团队命令)
	bot.intentRec = agent.NewIntentRecognizer(aiClient)

	// 12. 初始化 Cron 定时任务调度器
	bot.cronSched = agent.NewCronScheduler(layout.Cron, &botCronExecutor{bot: bot})
	bot.cronSched.Start()

	// 13. 初始化 Vision 客户端 (复用 api.Client)
	bot.visionCli = vision.NewClient(aiClient)

	// 14. 初始化 Wiki 引擎 (独立 git 仓库, 从配置加载)
	if config.Wiki.Enabled {
		wikiRepoDir := ""
		if len(config.Wiki.Repos) > 0 {
			wikiRepoDir = config.Wiki.Repos[0]
		}
		if wikiRepoDir == "" {
			if home, err := os.UserHomeDir(); err == nil {
				wikiRepoDir = home + "/knowledge-wiki"
			} else {
				wikiRepoDir = layout.Wiki
			}
		}
		bot.wikiEngine = wiki.NewEngineWithLLM(wikiRepoDir, aiClient)
		// 设置浏览器抓取器（绕过防爬虫）
		browserCfg := browser.DefaultConfig()
		if config.Browser.ChromePath != "" {
			browserCfg.ChromePath = config.Browser.ChromePath
		}
		if config.Browser.ProxyURL != "" {
			browserCfg.ProxyURL = config.Browser.ProxyURL
		}
		browserClient := browser.NewClient(browserCfg)
		bot.wikiEngine.SetBrowser(&wikiBrowserAdapter{client: browserClient})
		// 注入浏览器搜索适配器，让 WebSearchTool 能发起真实搜索
		if browserClient.Available() {
			bot.sessions.SetSearcher(&browserSearchAdapter{client: browserClient})
		}

		// 对所有注册的 Wiki 仓库内置 Schema（页面模板、分类、摄取工作流、Lint、整理任务）
		allRepos := config.Wiki.Repos
		if len(allRepos) == 0 {
			allRepos = []string{wikiRepoDir}
		} else {
			found := false
			for _, r := range allRepos {
				if r == wikiRepoDir {
					found = true
					break
				}
			}
			if !found {
				allRepos = append(allRepos, wikiRepoDir)
			}
		}
		for _, repoPath := range allRepos {
			if err := wiki.EnsureRepo(repoPath); err != nil {
				log.Printf("[Wiki] 初始化仓库失败 (%s): %v", repoPath, err)
			} else {
				log.Printf("[Wiki] 仓库已初始化 (含内置Schema): %s", repoPath)
			}
		}
		// 启动 raw 目录变化监控 (自动触发增量整理)
		bot.wikiEngine.WatchRaw(context.Background(), func(newFiles []string) {
			logging.For("wiki").Info("raw 目录新增文件", "count", len(newFiles))
			if config.Wiki.AutoOrganize {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				if result, err := bot.wikiEngine.IncrementalOrganize(ctx); err == nil {
					logging.For("wiki").Info("自动增量整理完成", "updated", result.UpdatedPages)
				}
			}
		})
		// 启动 Wiki HTTP API (供 Obsidian 插件调用)
		if config.Wiki.APIPort > 0 {
			wikiAPI := wiki.NewAPIServer(bot.wikiEngine, config.Wiki.APISecret)
			if err := wikiAPI.Start(config.Wiki.APIPort); err != nil {
				log.Printf("[Wiki API] 启动失败: %v", err)
			}
		}
	}

	// 14b. 初始化群体智能预测引擎
	{
		siCfg := swarm_intel.DefaultConfig()
		siCfg.Notify = func(chatID, msg string) {
			bot.sendLongMessage(context.Background(), chatID, msg)
		}
		bot.swarmEngine = swarm_intel.NewEngine(aiClient, siCfg)
		log.Printf("[SwarmIntel] 群体智能引擎已初始化")
	}

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

func (e *botCronExecutor) WikiOrganize(ctx context.Context, mode string) (string, error) {
	if e.bot.wikiEngine == nil {
		return "", fmt.Errorf("wiki 引擎未初始化")
	}
	var result *wiki.OrganizeResult
	var err error
	if mode == "incremental" {
		result, err = e.bot.wikiEngine.IncrementalOrganize(ctx)
	} else {
		result, err = e.bot.wikiEngine.Organize(ctx)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("更新 %d 个页面\n%s", result.UpdatedPages, result.Log), nil
}

func (e *botCronExecutor) WikiHealthCheck(ctx context.Context) (string, error) {
	if e.bot.wikiEngine == nil {
		return "", fmt.Errorf("wiki 引擎未初始化")
	}
	report, err := e.bot.wikiEngine.HealthCheck(ctx)
	if err != nil {
		return "", err
	}
	return report.Summary, nil
}

func (e *botCronExecutor) WikiLint(ctx context.Context) (string, error) {
	if e.bot.wikiEngine == nil {
		return "", fmt.Errorf("wiki 引擎未初始化")
	}
	report, err := e.bot.wikiEngine.Lint(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Wiki 页面: %d, Raw 文件: %d, 坏链: %d, 孤立页: %d",
		report.TotalPages, report.TotalRaw, len(report.BrokenLinks), len(report.OrphanedPages)), nil
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

	// 多模态消息路由
	switch msgType {
	case "image":
		go b.handleImageMessage(chatID, messageID, msg)
		return nil
	case "file":
		go b.handleFileMessage(chatID, messageID, msg)
		return nil
	case "video", "media":
		go b.handleVideoMessage(chatID, messageID, msg)
		return nil
	case "audio":
		go b.handleAudioMessage(chatID, messageID)
		return nil
	case "interactive":
		go b.handleInteractiveMessage(chatID, messageID, msg)
		return nil
	case "share_chat", "share_user":
		go b.handleShareMessage(chatID, messageID, msg, msgType)
		return nil
	case "post": // 富文本消息: 提取纯文本后走正常流程
		userText := ExtractPostText(deref(msg.Content))
		if userText == "" {
			return nil
		}
		go b.processAndReply(chatID, messageID, userText)
		return nil
	case "text":
		// 继续下面的文本处理逻辑
	default:
		b.sendTextReply(ctx, messageID, fmt.Sprintf("暂不支持 %s 类型消息，支持: 文本/图片/文件/视频/富文本/卡片/分享。", msgType))
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

	// === 三层消息去重，防止幽灵团队创建 ===

	// 防护1: messageID 精确去重 (防止飞书 webhook 重试)
	if _, loaded := b.processedMsgs.LoadOrStore(messageID, time.Now().Unix()); loaded {
		if b.config.Debug {
			log.Printf("[飞书Bot] 消息去重(messageID): %s", messageID)
		}
		return nil
	}

	// 防护2: 消息时间戳检查 — 忽略超过10分钟的旧消息（防止飞书延迟投递）
	if createTimeStr := deref(msg.CreateTime); createTimeStr != "" {
		if createTimeMS, err := strconv.ParseInt(createTimeStr, 10, 64); err == nil {
			msgAge := time.Since(time.UnixMilli(createTimeMS))
			if msgAge > 10*time.Minute {
				if b.config.Debug {
					log.Printf("[飞书Bot] 丢弃过期消息(%.0f分钟前): %s", msgAge.Minutes(), messageID)
				}
				return nil
			}
		}
	}

	// 防护3: 内容指纹去重 — 同一chatID+相同内容 5 秒内不重复处理
	// 注意: 仅防止飞书 webhook 秒级重复投递, 不阻止用户主动重发相同消息
	contentFingerprint := chatID + ":" + userText
	fpKey := "fp:" + contentFingerprint
	if _, loaded := b.processedMsgs.LoadOrStore(fpKey, time.Now().Unix()); loaded {
		fpTS, _ := b.processedMsgs.Load(fpKey)
		if ts, ok := fpTS.(int64); ok && time.Now().Unix()-ts < 5 {
			if b.config.Debug {
				log.Printf("[飞书Bot] 消息去重(内容指纹,5s内): %s", messageID)
			}
			return nil
		}
		// 超过 5 秒的相同内容视为用户主动重发, 允许处理
		b.processedMsgs.Store(fpKey, time.Now().Unix())
	}

	// 定期清理 (messageID 保留 30 分钟覆盖飞书重试窗口, 指纹仅保留 60 秒)
	go func() {
		now := time.Now().Unix()
		b.processedMsgs.Range(func(k, v interface{}) bool {
			key, _ := k.(string)
			ts, _ := v.(int64)
			if strings.HasPrefix(key, "fp:") {
				if now-ts > 60 {
					b.processedMsgs.Delete(k)
				}
			} else if now-ts > 1800 {
				b.processedMsgs.Delete(k)
			}
			return true
		})
	}()

	// 处理斜杠命令 (包括 /team 精确命令)
	if handled := b.handleSlashCommand(ctx, chatID, messageID, userText); handled {
		return nil
	}

	// Cron 意图识别: 自然语言定时任务
	if cronIntent := b.intentRec.RecognizeCron(ctx, userText); cronIntent != nil {
		go b.handleCronIntent(chatID, messageID, cronIntent)
		return nil
	}

	// 团队状态/停止: 仅保留低风险的 NL 检测（查询和停止操作不会创建资源）
	// 团队创建一律走 /team 或 /go 命令，避免 NL 误触发
	if intent := b.intentRec.RecognizeSafeOnly(ctx, userText); intent != nil {
		go b.handleTeamIntent(chatID, messageID, intent)
		return nil
	}

	// Wiki 自然语言命令检测 + URL 自动检测
	if b.wikiEngine != nil && b.config.Wiki.Enabled {
		wikiAction := DetectWikiIntent(userText)
		urls := extractURLs(userText)

		switch wikiAction {
		case "ingest":
			if len(urls) > 0 {
				go b.handleWikiIngest(chatID, messageID, urls)
				return nil
			}
			textToIngest := removeWikiKeywords(userText)
			if textToIngest != "" {
				go func() {
					if err := b.wikiEngine.IngestText(context.Background(), "飞书笔记", textToIngest, "feishu-manual"); err != nil {
						b.sendTextMessage(context.Background(), chatID, fmt.Sprintf("Wiki 收藏失败: %v", err))
						return
					}
					b.sendTextReply(context.Background(), messageID, "📚 已收藏到知识库 wiki")
				}()
				return nil
			}
		case "organize":
			go func() {
				b.sendTextReply(context.Background(), messageID, "📝 开始整理知识库...")
				oCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				if result, err := b.wikiEngine.Organize(oCtx); err == nil {
					b.sendLongMessage(context.Background(), chatID,
						fmt.Sprintf("✅ Wiki 整理完成: 更新 %d 个页面\n%s", result.UpdatedPages, result.Log))
				}
			}()
			return nil
		case "query":
			question := removeWikiQueryKeywords(userText)
			if question != "" {
				go func() {
					qCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					answer, _, _ := b.wikiEngine.QueryAndArchive(qCtx, question)
					if answer != "" {
						b.sendLongMessage(context.Background(), chatID, "📖 **知识库:**\n\n"+answer)
					}
				}()
				return nil
			}
		}

		if b.config.Wiki.AutoIngestURL && len(urls) > 0 {
			go b.handleWikiIngest(chatID, messageID, urls)
		}
	}

	// 异步处理消息 (避免阻塞飞书回调的 3 秒超时)
	go b.processAndReply(chatID, messageID, userText)

	return nil
}

// handleImageMessage 处理图片消息：下载为 base64 → Vision 理解。
func (b *Bot) handleImageMessage(chatID, messageID string, msg *larkim.EventMessage) {
	ctx := context.Background()
	b.sendTextReply(ctx, messageID, "🔍 正在分析图片...")

	content := deref(msg.Content)
	var imgContent struct {
		ImageKey string `json:"image_key"`
	}
	if json.Unmarshal([]byte(content), &imgContent) != nil || imgContent.ImageKey == "" {
		b.sendTextMessage(ctx, chatID, "无法获取图片 key。")
		return
	}
	if b.visionCli == nil {
		b.sendTextMessage(ctx, chatID, "视觉能力未初始化。")
		return
	}

	b64, err := b.downloadImageAsBase64(ctx, messageID, imgContent.ImageKey)
	if err != nil {
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("下载图片失败: %v", err))
		return
	}

	response, err := b.visionCli.Understand(ctx, b64,
		"请详细描述这张图片的内容，包括文字、图表、数据等所有关键信息。如果有代码、公式、表格等结构化内容，请按原始格式呈现。")
	if err != nil {
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("图片分析失败: %v", err))
		return
	}

	b.sendLongMessage(ctx, chatID, "🖼️ **图片分析结果:**\n\n"+response)
}

// handleFileMessage 处理文件消息：提取文件信息并提供说明。
func (b *Bot) handleFileMessage(chatID, messageID string, msg *larkim.EventMessage) {
	ctx := context.Background()
	content := deref(msg.Content)
	var fileContent struct {
		FileKey  string `json:"file_key"`
		FileName string `json:"file_name"`
	}
	if json.Unmarshal([]byte(content), &fileContent) != nil || fileContent.FileKey == "" {
		b.sendTextReply(ctx, messageID, "无法获取文件信息。")
		return
	}
	b.sendTextReply(ctx, messageID, fmt.Sprintf("📄 收到文件: **%s**\n\n正在处理...", fileContent.FileName))

	ext := strings.ToLower(fileContent.FileName)
	switch {
	case strings.HasSuffix(ext, ".png") || strings.HasSuffix(ext, ".jpg") ||
		strings.HasSuffix(ext, ".jpeg") || strings.HasSuffix(ext, ".gif") ||
		strings.HasSuffix(ext, ".webp"):
		if b.visionCli != nil {
			b64, err := b.downloadFileAsBase64(ctx, messageID, fileContent.FileKey)
			if err == nil {
				resp, err := b.visionCli.Understand(ctx, b64, "请详细分析这张图片的内容。")
				if err == nil {
					b.sendLongMessage(ctx, chatID, "🖼️ **图片文件分析:**\n\n"+resp)
					return
				}
			}
		}
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("已收到图片文件 %s，但视觉分析暂不可用。", fileContent.FileName))
	default:
		b.sendTextMessage(ctx, chatID,
			fmt.Sprintf("📄 已收到文件: **%s** (file_key: %s)\n\n"+
				"目前支持图片文件的自动分析。其他类型文件请描述需求，我会尽力协助。",
				fileContent.FileName, fileContent.FileKey))
	}
}

// handleVideoMessage 处理视频消息：提取关键帧或描述。
func (b *Bot) handleVideoMessage(chatID, messageID string, msg *larkim.EventMessage) {
	ctx := context.Background()
	content := deref(msg.Content)
	var vidContent struct {
		FileKey  string `json:"file_key"`
		ImageKey string `json:"image_key"` // 视频封面图
	}
	if json.Unmarshal([]byte(content), &vidContent) != nil {
		b.sendTextReply(ctx, messageID, "无法解析视频消息。")
		return
	}

	// 如果有封面图，用 Vision 分析
	if vidContent.ImageKey != "" && b.visionCli != nil {
		b.sendTextReply(ctx, messageID, "🎬 收到视频，正在分析封面图...")
		b64, err := b.downloadImageAsBase64(ctx, messageID, vidContent.ImageKey)
		if err == nil {
			resp, err := b.visionCli.Understand(ctx, b64, "这是一个视频的封面图/缩略图，请描述画面内容，推测视频的可能主题。")
			if err == nil {
				b.sendLongMessage(ctx, chatID, "🎬 **视频封面分析:**\n\n"+resp)
				return
			}
		}
	}

	b.sendTextMessage(ctx, chatID, "🎬 已收到视频消息。目前支持通过封面图分析视频内容。请描述你对该视频的具体需求。")
}

// handleAudioMessage 处理音频消息。
func (b *Bot) handleAudioMessage(chatID, messageID string) {
	ctx := context.Background()
	b.sendTextReply(ctx, messageID, "🎵 已收到音频消息。目前暂不支持音频转写，请将内容转为文字发送。")
}

// downloadImageAsBase64 通过飞书 API 下载消息中的图片并转为 base64。
func (b *Bot) downloadImageAsBase64(ctx context.Context, messageID, imageKey string) (string, error) {
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(imageKey).
		Type("image").
		Build()

	resp, err := b.client.Im.MessageResource.Get(ctx, req)
	if err != nil {
		return "", fmt.Errorf("飞书图片下载 API 失败: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("飞书图片下载失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}

	if resp.File == nil {
		return "", fmt.Errorf("图片数据为空")
	}
	data, err := io.ReadAll(resp.File)
	if err != nil {
		return "", fmt.Errorf("读取图片数据失败: %w", err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("图片数据为空")
	}

	mediaType := http.DetectContentType(data)
	b64 := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
	return b64, nil
}

// downloadFileAsBase64 通过飞书 API 下载文件并转为 base64。
func (b *Bot) downloadFileAsBase64(ctx context.Context, messageID, fileKey string) (string, error) {
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(fileKey).
		Type("file").
		Build()

	resp, err := b.client.Im.MessageResource.Get(ctx, req)
	if err != nil {
		return "", fmt.Errorf("飞书文件下载 API 失败: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("飞书文件下载失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}

	if resp.File == nil {
		return "", fmt.Errorf("文件数据为空")
	}
	data, err := io.ReadAll(resp.File)
	if err != nil {
		return "", fmt.Errorf("读取文件数据失败: %w", err)
	}

	mediaType := http.DetectContentType(data)
	b64 := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
	return b64, nil
}

// sendImageMessage 发送图片消息到飞书（需先上传图片获取 image_key）。
func (b *Bot) sendImageMessage(ctx context.Context, chatID string, imageData []byte, filename string) error {
	imgReq := larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().
			ImageType("message").
			Image(bytes.NewReader(imageData)).
			Build()).
		Build()

	imgResp, err := b.client.Im.Image.Create(ctx, imgReq)
	if err != nil {
		return fmt.Errorf("上传图片失败: %w", err)
	}
	if !imgResp.Success() {
		return fmt.Errorf("上传图片失败: code=%d, msg=%s", imgResp.Code, imgResp.Msg)
	}

	imageKey := deref(imgResp.Data.ImageKey)
	content, _ := json.Marshal(map[string]string{"image_key": imageKey})

	msgReq := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType("image").
			ReceiveId(chatID).
			Content(string(content)).
			Build()).
		Build()

	resp, err := b.client.Im.Message.Create(ctx, msgReq)
	if err != nil {
		return fmt.Errorf("发送图片消息失败: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("发送图片消息失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// sendFileMessage 发送文件消息到飞书。
func (b *Bot) sendFileMessage(ctx context.Context, chatID string, fileData []byte, filename, fileType string) error {
	if fileType == "" {
		fileType = "stream"
	}
	fileReq := larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().
			FileType(fileType).
			FileName(filename).
			File(bytes.NewReader(fileData)).
			Build()).
		Build()

	fileResp, err := b.client.Im.File.Create(ctx, fileReq)
	if err != nil {
		return fmt.Errorf("上传文件失败: %w", err)
	}
	if !fileResp.Success() {
		return fmt.Errorf("上传文件失败: code=%d, msg=%s", fileResp.Code, fileResp.Msg)
	}

	fileKey := deref(fileResp.Data.FileKey)
	content, _ := json.Marshal(map[string]string{"file_key": fileKey})

	msgReq := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType("file").
			ReceiveId(chatID).
			Content(string(content)).
			Build()).
		Build()

	resp, err := b.client.Im.Message.Create(ctx, msgReq)
	if err != nil {
		return fmt.Errorf("发送文件消息失败: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("发送文件消息失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// SendMediaToChat 统一的媒体发送方法，供 FeishuSendFileTool 调用。
// mediaType: "image" 走图片消息, "file" 走文件消息。
func (b *Bot) SendMediaToChat(ctx context.Context, chatID string, data []byte, filename, mediaType string) error {
	switch mediaType {
	case "image":
		return b.sendImageMessage(ctx, chatID, data, filename)
	default:
		return b.sendFileMessage(ctx, chatID, data, filename, "stream")
	}
}

// ExtractPostText 从飞书富文本(post)消息中提取纯文本。
func ExtractPostText(content string) string {
	var post struct {
		Title   string `json:"title"`
		Content [][]struct {
			Tag  string `json:"tag"`
			Text string `json:"text,omitempty"`
			Href string `json:"href,omitempty"`
		} `json:"content"`
	}

	// post 消息可能包裹在 locale key 下
	var wrapper map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &wrapper) == nil {
		for _, locale := range []string{"zh_cn", "en_us", "ja_jp"} {
			if raw, ok := wrapper[locale]; ok {
				if json.Unmarshal(raw, &post) == nil && len(post.Content) > 0 {
					break
				}
			}
		}
	}

	var sb strings.Builder
	if post.Title != "" {
		sb.WriteString(post.Title)
		sb.WriteString("\n")
	}
	for _, line := range post.Content {
		for _, elem := range line {
			switch elem.Tag {
			case "text":
				sb.WriteString(elem.Text)
			case "a":
				sb.WriteString(elem.Text)
				if elem.Href != "" {
					sb.WriteString(" (" + elem.Href + ")")
				}
			}
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
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

	if b.config.Wiki.AutoOrganize {
		go func() {
			orgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if result, err := b.wikiEngine.IncrementalOrganize(orgCtx); err == nil && result.UpdatedPages > 0 {
				b.sendTextMessage(context.Background(), chatID,
					fmt.Sprintf("📝 Wiki 增量整理完成: 更新 %d 个页面\n%s", result.UpdatedPages, result.Log))
			}
		}()
	}
}

// handleInteractiveMessage 处理 interactive 类型消息（飞书卡片消息、第三方分享卡片如今日头条等）。
func (b *Bot) handleInteractiveMessage(chatID, messageID string, msg *larkim.EventMessage) {
	ctx := context.Background()
	content := deref(msg.Content)
	if content == "" {
		return
	}

	text, urls := ExtractInteractiveContent(content)
	if text == "" && len(urls) == 0 {
		b.sendTextReply(ctx, messageID, "收到卡片消息，但未能提取到有效内容。")
		return
	}

	if len(urls) > 0 && b.wikiEngine != nil && b.config.Wiki.Enabled {
		b.sendTextReply(ctx, messageID, "📚 检测到链接，正在提取到知识库...")
		go b.handleWikiIngest(chatID, messageID, urls)
	}

	if text != "" {
		go b.processAndReply(chatID, messageID, text)
	}
}

// handleShareMessage 处理 share_chat / share_user 类型消息。
func (b *Bot) handleShareMessage(chatID, messageID string, msg *larkim.EventMessage, shareType string) {
	ctx := context.Background()
	content := deref(msg.Content)
	b.sendTextReply(ctx, messageID, fmt.Sprintf("收到 %s 分享，内容: %s", shareType, truncateResult(content, 200)))
}

// 飞书 interactive 消息格式复杂，支持卡片、审批流、第三方分享等。
// ExtractInteractiveContent 从 interactive 消息 JSON 提取文本和 URL (导出供测试)。
func ExtractInteractiveContent(content string) (text string, urls []string) {
	var sb strings.Builder
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return content, extractURLs(content)
	}

	// 递归提取文本和 URL
	extractFromJSON(raw, &sb, &urls)

	// 同时从原始 JSON 字符串中提取 URL
	for _, u := range extractURLs(content) {
		found := false
		for _, existing := range urls {
			if existing == u {
				found = true
				break
			}
		}
		if !found {
			urls = append(urls, u)
		}
	}

	return strings.TrimSpace(sb.String()), urls
}

func extractFromJSON(m map[string]json.RawMessage, sb *strings.Builder, urls *[]string) {
	textKeys := []string{"title", "content", "text", "value", "label", "tag_content", "alt", "description", "summary"}
	urlKeys := []string{"url", "href", "multi_url", "android_url", "ios_url", "pc_url"}

	for key, val := range m {
		var s string
		if json.Unmarshal(val, &s) == nil {
			for _, tk := range textKeys {
				if key == tk && s != "" {
					sb.WriteString(s)
					sb.WriteString("\n")
				}
			}
			for _, uk := range urlKeys {
				if key == uk && (strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")) {
					*urls = append(*urls, s)
				}
			}
			continue
		}

		var subMap map[string]json.RawMessage
		if json.Unmarshal(val, &subMap) == nil {
			extractFromJSON(subMap, sb, urls)
			continue
		}

		var arr []json.RawMessage
		if json.Unmarshal(val, &arr) == nil {
			for _, item := range arr {
				var itemMap map[string]json.RawMessage
				if json.Unmarshal(item, &itemMap) == nil {
					extractFromJSON(itemMap, sb, urls)
				} else {
					var itemStr string
					if json.Unmarshal(item, &itemStr) == nil && itemStr != "" {
						sb.WriteString(itemStr)
						sb.WriteString("\n")
					}
				}
			}
		}
	}
}

func truncateResult(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// DetectWikiIntent 检测消息中的 wiki 意图 (导出供测试)。
func DetectWikiIntent(text string) string {
	lower := strings.ToLower(text)
	ingestKW := []string{"收藏到wiki", "收藏到知识库", "保存到wiki", "保存到知识库", "存到wiki", "添加到wiki", "加入wiki", "wiki收藏"}
	for _, kw := range ingestKW {
		if strings.Contains(lower, kw) {
			return "ingest"
		}
	}
	organizeKW := []string{"整理wiki", "整理知识库", "wiki整理", "知识库整理", "维护wiki", "维护知识库"}
	for _, kw := range organizeKW {
		if strings.Contains(lower, kw) {
			return "organize"
		}
	}
	queryKW := []string{"从wiki查", "从知识库查", "wiki查询", "知识库查询", "问问wiki", "问下知识库", "wiki里有没有"}
	for _, kw := range queryKW {
		if strings.Contains(lower, kw) {
			return "query"
		}
	}
	return ""
}

func removeWikiKeywords(text string) string {
	lower := strings.ToLower(text)
	for _, kw := range []string{"收藏到wiki", "收藏到知识库", "保存到wiki", "保存到知识库", "存到wiki", "添加到wiki", "加入wiki", "wiki收藏"} {
		lower = strings.ReplaceAll(lower, kw, "")
	}
	return strings.TrimSpace(lower)
}

func removeWikiQueryKeywords(text string) string {
	lower := strings.ToLower(text)
	for _, kw := range []string{"从wiki查", "从知识库查", "wiki查询", "知识库查询", "问问wiki", "问下知识库", "wiki里有没有"} {
		lower = strings.ReplaceAll(lower, kw, "")
	}
	return strings.TrimSpace(lower)
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
			"- /skill show <名称> - 查看技能详情\n" +
			"- /skill install <名称> - 创建技能模板\n" +
			"- /skill uninstall <名称> - 卸载技能\n" +
			"- /skill reload - 重新加载技能\n\n" +
			"**角色查看:**\n" +
			"- /role list - 列出角色\n" +
			"- /role show <名称> - 查看角色最终技能绑定\n\n" +
			"**Dreaming:**\n" +
			"- /dream - 手动触发记忆整理\n\n" +
			"**定时任务 (Cron):**\n" +
			"- /cron list - 列出所有定时任务\n" +
			"- /cron add <表达式> <类型> <内容> - 添加\n" +
			"- /cron remove <ID> - 删除\n" +
			"- /cron pause/resume <ID> - 暂停/恢复\n" +
			"- 直接说「每天9点帮我分析XXX」自动创建\n\n" +
			"**Wiki 知识库:**\n" +
			"- /wiki status - 查看知识库状态\n" +
			"- /wiki query <问题> - 查询知识库\n" +
			"- /wiki organize - 全量整理知识库\n" +
			"- /wiki organize inc - 增量整理\n" +
			"- /wiki lint - 检查链接健康\n" +
			"- /wiki health - LLM 健康检查\n" +
			"- 发送链接 → 自动提取到 raw 层\n" +
			"- 说「收藏到wiki」→ 提取并整理\n\n" +
			"**Agent Teams (多Agent协作):**\n" +
			"*快捷命令 (推荐):*\n" +
			"- /go <工作流> <目标> — 一键创建启动团队\n" +
			"  例: /go research 调研k8s最佳实践\n" +
			"  例: /go creative 画一个日落海报\n" +
			"  例: /go finance 分析特斯拉股票\n\n" +
			"*管理命令:*\n" +
			"- /team create <名称> <工作流> - 创建团队\n" +
			"- /team run <名称> <目标> - 启动执行\n" +
			"- /team status [名称] - 查看状态\n" +
			"- /team stop <名称> - 停止\n" +
			"- /team list - 列出所有\n" +
			"- /team delete <名称> - 删除\n" +
			"- /team workflows - 查看可用工作流\n\n" +
			"*自然语言 (仅查询/停止):*\n" +
			"- 说「团队进展如何」→ 查看状态\n" +
			"- 说「停止团队」→ 停止执行\n\n" +
			"**群体智能预测:**\n" +
			"- /predict <问题> — 群体智能5阶段预测\n" +
			"  例: /predict 2026年中国GDP增速\n" +
			"  例: /predict BTC半年内趋势\n" +
			"- /simulate [模式] <目标> — 场景模拟\n" +
			"  模式: social(默认) | game | montecarlo\n" +
			"  例: /simulate 新能源汽车市场竞争\n" +
			"  例: /simulate game 中美贸易谈判"
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

	case strings.HasPrefix(lower, "/role"):
		b.handleRoleCommand(ctx, messageID, text)
		return true

	case lower == "/dream":
		b.handleDreamCommand(ctx, messageID)
		return true

	case strings.HasPrefix(lower, "/go "):
		// 快捷命令: /go <工作流> <目标> — 一键创建并启动团队
		b.handleGoCommand(ctx, chatID, messageID, text)
		return true

	case strings.HasPrefix(lower, "/team"):
		b.handleTeamCommand(ctx, chatID, messageID, text)
		return true

	case strings.HasPrefix(lower, "/cron"):
		b.handleCronCommand(ctx, chatID, messageID, text)
		return true

	case strings.HasPrefix(lower, "/wiki"):
		b.handleWikiCommand(ctx, chatID, messageID, text)
		return true

	case strings.HasPrefix(lower, "/predict"):
		b.handlePredictCommand(ctx, chatID, messageID, text)
		return true

	case strings.HasPrefix(lower, "/simulate"):
		b.handleSimulateCommand(ctx, chatID, messageID, text)
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
// 支持: /skill list, /skill show <name>, /skill install <name>, /skill uninstall <name>, /skill reload
func (b *Bot) handleSkillCommand(ctx context.Context, chatID, messageID, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.sendTextReply(ctx, messageID, "/skill list - 列出技能\n/skill show <name> - 查看技能详情\n/skill reload - 重载技能\n/skill install <name> - 安装\n/skill uninstall <name> - 卸载")
		return
	}

	sub := strings.ToLower(parts[1])
	switch sub {
	case "list":
		allSkills := b.skillReg.All()
		if len(allSkills) == 0 {
			b.sendTextReply(ctx, messageID, "无已加载技能。可放在 .claude/skills/ 或 .claude-go/skills/ 下。")
			return
		}
		var sb strings.Builder
		sb.WriteString("**已加载技能:**\n")
		for _, s := range allSkills {
			sb.WriteString(fmt.Sprintf("- **%s**: %s (来源: %s)\n", s.Name, s.Description, s.LoadedFrom))
		}
		b.sendTextReply(ctx, messageID, sb.String())

	case "show":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /skill show <name>")
			return
		}
		skill, ok := b.skillReg.Get(parts[2])
		if !ok {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("未找到技能: %s", parts[2]))
			return
		}
		body := skill.Body
		if len(body) > 2500 {
			body = body[:2500] + "\n...(truncated)"
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("**%s**\n来源: %s\n用途: %s\n\n%s", skill.Name, skill.LoadedFrom, skill.WhenToUse, body))

	case "reload":
		count := b.skillReg.Reload()
		b.sendTextReply(ctx, messageID, fmt.Sprintf("已重载 %d 个技能。", count))

	case "install":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /skill install <name>\n(在 .claude-go/skills/<name>/SKILL.md 中创建文件)")
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
			relPath := ".claude/skills/" + name + "/SKILL.md"
			if b.layout != nil {
				relPath = ".claude-go/skills/" + name + "/SKILL.md"
			}
			b.sendTextReply(ctx, messageID, fmt.Sprintf("已创建技能模板: %s\n请编辑内容后发送 /skill reload。", relPath))
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
		b.sendTextReply(ctx, messageID, "未知 /skill 子命令。用法: /skill [list|show|reload|install|uninstall]")
	}
}

func (b *Bot) handleRoleCommand(ctx context.Context, messageID, text string) {
	if b.sessions == nil || b.sessions.roleRegistry == nil {
		b.sendTextReply(ctx, messageID, "角色注册表未初始化。")
		return
	}

	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.sendTextReply(ctx, messageID, "/role list - 列出角色\n/role show <name> - 查看角色最终技能绑定")
		return
	}

	switch strings.ToLower(parts[1]) {
	case "list":
		var sb strings.Builder
		sb.WriteString("**可用角色:**\n")
		for _, role := range b.sessions.roleRegistry.ListByCategory("") {
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n", role.Name, role.Description))
		}
		b.sendTextReply(ctx, messageID, sb.String())
	case "show":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /role show <name>")
			return
		}
		info := b.sessions.roleRegistry.DescribeRole(parts[2])
		if info == nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("未找到角色: %s", parts[2]))
			return
		}
		msg := fmt.Sprintf("**角色:** %s\n**解析后角色:** %s\n**描述:** %s\n**文件技能:** %s\n**内置技能:** %s\n**推荐技能:** %s\n**最终技能集:** %s",
			info.Requested,
			info.Resolved,
			info.Description,
			formatNames(info.FileSkills),
			formatNames(info.BuiltinSkills),
			formatNames(info.RecommendedSkills),
			formatNames(b.sessions.roleRegistry.RoleSkills(parts[2])),
		)
		b.sendTextReply(ctx, messageID, msg)
	default:
		b.sendTextReply(ctx, messageID, "未知 /role 子命令。用法: /role [list|show]")
	}
}

func formatNames(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

// handleGoCommand 一键创建并启动团队: /go <工作流> <目标>
// 示例:
//
//	/go research 调研 kubernetes 最佳实践
//	/go creative 画一个日落风景
//	/go finance 分析特斯拉财报
func (b *Bot) handleGoCommand(ctx context.Context, chatID, messageID, text string) {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		b.sendTextReply(ctx, messageID, "用法: /go <工作流> <目标>\n"+
			"工作流: research, development, debate, creative, finance, techblog, swarm\n"+
			"示例: /go research 调研 kubernetes 最佳实践")
		return
	}

	workflow := strings.ToLower(parts[1])
	objective := strings.Join(parts[2:], " ")
	teamName := fmt.Sprintf("go-%s-%d", workflow, time.Now().Unix()%10000)

	team, err := b.teamMgr.CreateTeam(teamName, workflow, objective, chatID)
	if err != nil {
		b.sendTextReply(ctx, messageID, fmt.Sprintf("创建失败: %v", err))
		return
	}

	// 将当前会话的近期上下文注入团队 Blackboard，确保 Agent 有完整背景
	b.injectSessionContext(chatID, team)

	b.sendTextReply(ctx, messageID, fmt.Sprintf(
		"🚀 快速启动:\n- 团队: **%s**\n- 工作流: %s\n- Agent数: %d\n- 目标: %s",
		team.Name, workflow, len(team.Agents), objective))

	if err := b.teamMgr.RunTeam(teamName, objective); err != nil {
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("启动失败: %v", err))
	}
}

// injectSessionContext 从当前会话中提取近期对话上下文，写入团队 Blackboard。
// 确保 Agent 能看到用户的完整背景（包括最近的对话历史摘要）。
func (b *Bot) injectSessionContext(chatID string, team *agent.ProductionTeam) {
	if team.Blackboard == nil {
		return
	}
	sess := b.sessions.Get(chatID)
	if sess == nil {
		return
	}

	eng := sess.Engine
	if eng == nil {
		return
	}
	recentMsgs := eng.RecentMessages(5)
	if len(recentMsgs) == 0 {
		return
	}

	var sessionCtx strings.Builder
	sessionCtx.WriteString("### 用户近期对话上下文 (团队启动前的对话)\n")
	for _, m := range recentMsgs {
		role := string(m.Type)
		for _, block := range m.Content {
			if block.Text != "" {
				text := block.Text
				if len(text) > 500 {
					text = text[:500] + "...(截断)"
				}
				sessionCtx.WriteString(fmt.Sprintf("[%s]: %s\n", role, text))
			}
		}
	}
	team.Blackboard.Write("session-context", sessionCtx.String(), "system", "context")
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
		// 检查是否有相同工作流的团队在运行中（防止重复创建）
		for _, t := range b.teamMgr.ListAllTeams() {
			if t.Workflow == intent.Workflow && t.Status == agent.TeamStatusRunning {
				b.sendTextReply(ctx, messageID, fmt.Sprintf(
					"⚠️ 已有同类型的 **%s** 团队(%s)正在运行。\n发送「团队进展如何」查看进度，或「停止团队」后重新创建。",
					intent.Workflow, t.Name))
				return
			}
		}

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

// handleWikiCommand 处理 /wiki 命令族。
func (b *Bot) handleWikiCommand(ctx context.Context, chatID, messageID, text string) {
	if b.wikiEngine == nil {
		b.sendTextReply(ctx, messageID, "Wiki 引擎未初始化。请在配置中启用 wiki 功能。")
		return
	}
	parts := strings.Fields(text)
	sub := "status"
	if len(parts) >= 2 {
		sub = strings.ToLower(parts[1])
	}

	switch sub {
	case "status":
		st := b.wikiEngine.Status()
		b.sendTextReply(ctx, messageID, fmt.Sprintf("📚 **Wiki 状态**\n"+
			"- 仓库: %v\n- Raw 文件: %v\n- Wiki 页面: %v\n- LLM 就绪: %v",
			st["repoDir"], st["rawCount"], st["wikiCount"], st["hasLLM"]))

	case "query":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /wiki query <问题>")
			return
		}
		question := strings.Join(parts[2:], " ")
		b.sendTextReply(ctx, messageID, "🔍 正在查询知识库...")
		go func() {
			qCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			answer, archived, err := b.wikiEngine.QueryAndArchive(qCtx, question)
			if err != nil {
				b.sendTextMessage(context.Background(), chatID, fmt.Sprintf("查询失败: %v", err))
				return
			}
			suffix := ""
			if archived {
				suffix = "\n\n_📝 此回答已自动归档到 wiki_"
			}
			b.sendLongMessage(context.Background(), chatID, "📖 **知识库回答:**\n\n"+answer+suffix)
		}()

	case "organize":
		mode := "full"
		if len(parts) >= 3 && (parts[2] == "inc" || parts[2] == "incremental") {
			mode = "incremental"
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("📝 开始 %s 整理...", mode))
		go func() {
			oCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			var result *wiki.OrganizeResult
			var err error
			if mode == "incremental" {
				result, err = b.wikiEngine.IncrementalOrganize(oCtx)
			} else {
				result, err = b.wikiEngine.Organize(oCtx)
			}
			if err != nil {
				b.sendTextMessage(context.Background(), chatID, fmt.Sprintf("整理失败: %v", err))
				return
			}
			b.sendLongMessage(context.Background(), chatID,
				fmt.Sprintf("✅ Wiki 整理完成\n- 更新页面: %d\n- 日志: %s", result.UpdatedPages, result.Log))
		}()

	case "lint":
		go func() {
			lCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			report, err := b.wikiEngine.Lint(lCtx)
			if err != nil {
				b.sendTextMessage(context.Background(), chatID, fmt.Sprintf("Lint 失败: %v", err))
				return
			}
			var sb strings.Builder
			sb.WriteString("🔗 **Wiki Lint 报告**\n")
			sb.WriteString(fmt.Sprintf("- Wiki 页面: %d\n- Raw 文件: %d\n", report.TotalPages, report.TotalRaw))
			if len(report.BrokenLinks) > 0 {
				sb.WriteString(fmt.Sprintf("- ⚠️ 坏链: %d\n", len(report.BrokenLinks)))
				for _, bl := range report.BrokenLinks {
					sb.WriteString(fmt.Sprintf("  %s → [[%s]]\n", bl.SourcePage, bl.TargetPage))
				}
			}
			if len(report.OrphanedPages) > 0 {
				sb.WriteString(fmt.Sprintf("- 🏝️ 孤立页: %d\n", len(report.OrphanedPages)))
			}
			b.sendTextMessage(context.Background(), chatID, sb.String())
		}()

	case "health":
		b.sendTextReply(ctx, messageID, "🏥 正在进行 Wiki 健康检查...")
		go func() {
			hCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			report, err := b.wikiEngine.HealthCheck(hCtx)
			if err != nil {
				b.sendTextMessage(context.Background(), chatID, fmt.Sprintf("健康检查失败: %v", err))
				return
			}
			var sb strings.Builder
			sb.WriteString("🏥 **Wiki 健康检查报告**\n\n")
			sb.WriteString(report.Summary)
			if len(report.Contradictions) > 0 {
				sb.WriteString(fmt.Sprintf("\n\n⚠️ **矛盾数据**: %d 处", len(report.Contradictions)))
			}
			if len(report.MissingConcepts) > 0 {
				sb.WriteString(fmt.Sprintf("\n📝 **缺失概念**: %s", strings.Join(report.MissingConcepts, ", ")))
			}
			if len(report.ResearchSuggestions) > 0 {
				sb.WriteString("\n\n💡 **建议研究方向**:")
				for _, s := range report.ResearchSuggestions {
					sb.WriteString("\n- " + s)
				}
			}
			b.sendLongMessage(context.Background(), chatID, sb.String())
		}()

	default:
		b.sendTextReply(ctx, messageID, "Wiki 命令: status | query <问题> | organize [inc] | lint | health")
	}
}

// handlePredictCommand 处理 /predict <目标> — 群体智能预测
func (b *Bot) handlePredictCommand(ctx context.Context, chatID, messageID, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.sendTextReply(ctx, messageID, "用法: /predict <预测问题>\n\n"+
			"示例:\n"+
			"- /predict 2026年中国GDP增速\n"+
			"- /predict 下一代iPhone发布时间\n"+
			"- /predict BTC半年内趋势\n\n"+
			"引擎将执行5阶段流水线: 分解→侦察→预测→辩论→融合")
		return
	}

	objective := strings.Join(parts[1:], " ")
	b.sendTextReply(ctx, messageID, fmt.Sprintf("🧠 群体智能引擎启动，正在预测: %s\n\n"+
		"5阶段流水线: 分解→侦察→预测→辩论→融合\n请稍候...", objective))

	go func() {
		pCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		result, err := b.swarmEngine.Predict(pCtx, chatID, objective)
		if err != nil {
			b.sendTextMessage(context.Background(), chatID, fmt.Sprintf("❌ 预测失败: %v", err))
			return
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("## 🔮 群体智能预测报告\n\n**问题**: %s\n\n", result.Question))
		sb.WriteString("### 📊 预测结果\n\n| 结果 | 概率 | 95%置信区间 |\n|------|------|------------|\n")
		for _, o := range result.Outcomes {
			sb.WriteString(fmt.Sprintf("| %s | **%.1f%%** | [%.1f%%, %.1f%%] |\n",
				o.Outcome, o.Probability*100, o.Lower95*100, o.Upper95*100))
		}
		sb.WriteString(fmt.Sprintf("\n### 📈 融合指标\n- 共识度: %.2f\n- 校准分数(Brier): %.4f\n- 辩论轮数: %d\n- 融合方法: %s\n",
			result.Consensus, result.BrierScore, result.Rounds, result.Method))
		if result.Summary != "" {
			sb.WriteString(fmt.Sprintf("\n### 💡 综合分析\n%s\n", result.Summary))
		}
		b.sendLongMessage(context.Background(), chatID, sb.String())
	}()
}

// handleSimulateCommand 处理 /simulate <模式> <目标> — 群体智能模拟
func (b *Bot) handleSimulateCommand(ctx context.Context, chatID, messageID, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.sendTextReply(ctx, messageID, "用法: /simulate [模式] <模拟目标>\n\n"+
			"**9种模式**:\n"+
			"- social — 社会模拟 (默认)\n"+
			"- game — 博弈论模拟\n"+
			"- montecarlo — 蒙特卡洛场景树\n"+
			"- crisis — 危机推演\n"+
			"- org — 组织动力学\n"+
			"- creative — 创意涌现\n"+
			"- market — 市场竞争\n"+
			"- policy — 政策推演\n"+
			"- tech — 技术演进\n\n"+
			"示例:\n"+
			"- /simulate 新能源汽车市场竞争\n"+
			"- /simulate game 中美贸易谈判\n"+
			"- /simulate crisis 全球芯片供应中断\n"+
			"- /simulate creative AI+教育的未来\n"+
			"- /simulate tech 量子计算 vs 经典计算")
		return
	}

	mode := "social"
	var objective string
	knownModes := map[string]bool{
		"social": true, "game": true, "montecarlo": true,
		"crisis": true, "org": true, "creative": true,
		"market": true, "policy": true, "tech": true,
	}
	if knownModes[strings.ToLower(parts[1])] {
		mode = strings.ToLower(parts[1])
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "请指定模拟目标，例如: /simulate game 中美贸易谈判")
			return
		}
		objective = strings.Join(parts[2:], " ")
	} else {
		objective = strings.Join(parts[1:], " ")
	}

	b.sendTextReply(ctx, messageID, fmt.Sprintf("🌐 群体智能模拟启动\n模式: %s | 目标: %s\n请稍候...", mode, objective))

	go func() {
		sCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		cfg := swarm_intel.SimulationConfig{
			Mode:   mode,
			Agents: 5,
			Rounds: 3,
		}
		result, err := b.swarmEngine.Simulate(sCtx, chatID, objective, cfg)
		if err != nil {
			b.sendTextMessage(context.Background(), chatID, fmt.Sprintf("❌ 模拟失败: %v", err))
			return
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("## 🌐 群体智能模拟报告\n\n**模式**: %s | **轮数**: %d\n\n", result.Mode, result.Rounds))
		sb.WriteString("### 📊 场景结果\n\n| 场景 | 概率 | 描述 |\n|------|------|------|\n")
		for _, sc := range result.Scenarios {
			desc := sc.Description
			if len(desc) > 60 {
				desc = desc[:60] + "…"
			}
			sb.WriteString(fmt.Sprintf("| %s | **%.1f%%** | %s |\n", sc.Name, sc.Probability*100, desc))
		}
		if len(result.Emergent) > 0 {
			sb.WriteString("\n### 🌊 涌现行为\n")
			for _, em := range result.Emergent {
				sb.WriteString("- " + em + "\n")
			}
		}
		if result.Summary != "" {
			sb.WriteString(fmt.Sprintf("\n### 💡 综合分析\n%s\n", result.Summary))
		}
		b.sendLongMessage(context.Background(), chatID, sb.String())
	}()
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

	// Auto Plan: LLM 判断复杂度 → 自动注入 Plan+Build 指令
	if b.llmDetectComplexity(ctx, userText) {
		userText = "[Auto Plan+Build] 这是一个复杂任务。\n" +
			"阶段1(Plan): 调用 EnterPlanMode，深入分析需求，设计详细方案(架构、模块拆分、接口定义、风险点)。\n" +
			"阶段2(Switch): 调用 ExitPlanMode 附带完整计划摘要。\n" +
			"阶段3(Build): 按计划逐步执行实现(编写代码、创建文件、运行命令)，每完成一步验证结果。\n\n" +
			"原始任务:\n" + userText
		b.sendTextReply(ctx, messageID, "🧠 LLM 判定为复杂任务，自动启用 Plan→Build 流程...")
	}

	// 发送「正在思考」提示
	if b.config.ThinkingMessage != "" {
		b.sendTextReply(ctx, messageID, b.config.ThinkingMessage)
	}

	// 处理消息 (支持排队: 会话繁忙时消息入队, 处理完自动消费)
	replyFn := func(resp string) { b.sendLongMessage(context.Background(), chatID, resp) }
	response, queued, err := b.sessions.ProcessMessageWithQueue(ctx, chatID, userText, replyFn)
	if err != nil {
		log.Printf("[飞书Bot] 处理消息失败: chat=%s, err=%v", chatID, err)
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("处理失败: %v", err))
		return
	}
	if queued {
		b.sendTextReply(ctx, messageID, response)
		return
	}

	// 检测输出中是否包含 SVG（vision 生成结果）→ 自动转为图片发回
	if svgContent := ExtractSVGFromResponse(response); svgContent != "" {
		textPart := RemoveSVGFromResponse(response)
		if textPart != "" {
			b.sendLongMessage(ctx, chatID, textPart)
		}
		pngData := svgToPNGData(svgContent)
		if len(pngData) > 0 {
			if err := b.sendImageMessage(ctx, chatID, pngData, "generated.png"); err != nil {
				log.Printf("[飞书Bot] 发送生成图片失败: %v, 回退到文本", err)
				b.sendLongMessage(ctx, chatID, response)
			}
		} else {
			// 无转换工具，发送 SVG 文件
			if err := b.sendFileMessage(ctx, chatID, []byte(svgContent), "generated.svg", "stream"); err != nil {
				b.sendLongMessage(ctx, chatID, response)
			}
		}
		return
	}

	// 分段发送 (飞书文本消息有长度限制)
	b.sendLongMessage(ctx, chatID, response)
}

// ExtractSVGFromResponse 从 AI 回复中提取 SVG 内容 (导出供测试)。
func ExtractSVGFromResponse(s string) string {
	lower := strings.ToLower(s)
	start := strings.Index(lower, "<svg")
	if start < 0 {
		return ""
	}
	after := s[start:]
	end := strings.Index(strings.ToLower(after), "</svg>")
	if end < 0 {
		return ""
	}
	return after[:end+len("</svg>")]
}

// RemoveSVGFromResponse 移除回复中的 SVG 代码块，保留文本说明 (导出供测试)。
func RemoveSVGFromResponse(s string) string {
	lower := strings.ToLower(s)
	start := strings.Index(lower, "<svg")
	if start < 0 {
		return s
	}
	endTag := strings.Index(lower[start:], "</svg>")
	if endTag < 0 {
		return s
	}
	before := strings.TrimSpace(s[:start])
	after := strings.TrimSpace(s[start+endTag+len("</svg>"):])
	result := before
	if after != "" {
		result += "\n" + after
	}
	return strings.TrimSpace(result)
}

// svgToPNGData 使用系统工具将 SVG 转为 PNG。
func svgToPNGData(svg string) []byte {
	type converter struct {
		cmd  string
		args []string
	}
	converters := []converter{
		{"rsvg-convert", []string{"-f", "png", "-w", "800"}},
		{"inkscape", []string{"--export-type=png", "--export-width=800", "--pipe"}},
	}
	for _, c := range converters {
		path, err := exec.LookPath(c.cmd)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, path, c.args...)
		cmd.Stdin = strings.NewReader(svg)
		out, err := cmd.Output()
		cancel()
		if err == nil && len(out) > 0 {
			return out
		}
	}
	return nil
}

// llmDetectComplexity 使用 LLM 判断任务复杂度。
// 先做快速启发式筛选(极短消息直接跳过)，再调用 LLM 做精确判断。
func (b *Bot) llmDetectComplexity(ctx context.Context, text string) bool {
	runeLen := len([]rune(text))
	if runeLen < 15 {
		return false
	}

	sysPrompt := `你是一个任务复杂度分类器。用户发来一条消息，你需要判断它是"简单任务"还是"复杂任务"。

复杂任务的特征(满足任一即可):
- 涉及多个步骤或子任务(>2步)
- 需要架构设计、方案评估
- 涉及多文件或多模块改动
- 需要调研、对比、分析
- 包含明确的编号列表(1.2.3.)
- 同时涉及编码+测试+部署等多阶段
- 需要协作(多角色参与)

简单任务: 单一查询、简单指令、一句话修改、翻译、问答等。

只回复一个单词: COMPLEX 或 SIMPLE`

	timeoutCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	resp, err := b.apiClient.SimpleComplete(timeoutCtx, sysPrompt, text)
	if err != nil {
		return heuristicComplexity(text)
	}

	resp = strings.TrimSpace(strings.ToUpper(resp))
	if strings.Contains(resp, "COMPLEX") {
		return true
	}
	if strings.Contains(resp, "SIMPLE") {
		return false
	}
	return heuristicComplexity(text)
}

// heuristicComplexity 启发式降级: LLM 不可用时的快速判断。
func heuristicComplexity(text string) bool {
	runeLen := len([]rune(text))
	if runeLen > 300 {
		return true
	}
	conjunctions := []string{"并且", "然后", "同时", "另外", "还需要", "以及", "此外", "接着"}
	conjCount := 0
	for _, c := range conjunctions {
		if strings.Contains(text, c) {
			conjCount++
		}
	}
	if conjCount >= 2 {
		return true
	}
	numberedItems := 0
	for _, prefix := range []string{"1.", "2.", "3.", "4.", "5.", "1、", "2、", "3、"} {
		if strings.Contains(text, prefix) {
			numberedItems++
		}
	}
	return numberedItems >= 3
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
				Tag:     "markdown",
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
