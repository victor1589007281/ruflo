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
	"path/filepath"
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
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/browser"
	"github.com/anthropic/claude-go/pkg/cluster"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/hotreload"
	"github.com/anthropic/claude-go/pkg/llmgw"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/observability"
	"github.com/anthropic/claude-go/pkg/sandbox"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/statestore"
	swarm_intel "github.com/anthropic/claude-go/pkg/swarm_intel"
	claudesync "github.com/anthropic/claude-go/pkg/sync"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
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
func (d *dagTaskAdapter) GetAllTasks() []agent.DAGTaskSummary {
	v2Tasks := d.store.GetAllTasks()
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
func (d *dagTaskAdapter) ReevaluateBlockedTasks() int {
	return d.store.ReevaluateBlockedTasks()
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

func (w *wikiBrowserAdapter) Search(ctx context.Context, query string) (title, text, html string, err error) {
	result, err := w.client.Search(ctx, query)
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

func (da *dreamAdapter) AfterQuery(ctx context.Context) {
	if da.dreamer == nil {
		return
	}
	da.dreamer.AfterQuery(ctx)
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
	config        *BotConfig
	client        *lark.Client                       // 飞书 API 客户端 (用于发送消息)
	wsClient      *larkws.Client                     // WebSocket 长连接客户端
	sessions      *SessionManager                    // 会话管理器
	apiClient     *api.Client                        // AI API 客户端
	llmGW         llmgw.LLMGateway                   // L1 网关视图 (design/02 §3.1): apiClient 的接口形态
	mcpMgr        *dynmcp.Manager                    // 动态 MCP 管理器 (进程级别共享)
	skillReg      *skills.Registry                   // 技能注册表 (进程级别共享)
	dreamer       *dreaming.Dreamer                  // Dreaming 记忆整理引擎
	memStore      *memory.TieredStore                // 多层记忆存储 (进程级别共享)
	factStore     *memory.FactStore                  // L2 结构化记忆 (V3 Anti-Amnesia)
	teamMgr       *agent.ProductionTeamManager       // 生产级 Agent Teams 管理器
	intentRec     *agent.IntentRecognizer            // 自然语言意图识别器
	taskStore     *builtin.TaskStore                 // 共享 V2 Task 存储
	evolution     *agent.EvolutionEngine             // 自动进化引擎
	cfgWatcher    *hotreload.Watcher                 // 配置热加载监控器
	llmEventUnsub func()                             // EventBus 上 llm.event.* 的退订函数 (见 llm_event_bus.go)
	cronSched     *agent.CronScheduler               // 定时任务调度器
	layout        *basedir.Layout                    // 统一目录布局
	stateStore    statestore.StateStore              // 进程内唯一 StateStore (轨迹底座 + 会话历史快照共用)
	wikiEngine    *wiki.Engine                       // LLM Wiki 知识库引擎
	swarmEngine   *swarm_intel.Engine                // 群体智能预测引擎
	visionCli     *vision.Client                     // 视觉能力客户端
	skillAuto     *skills.AutoCreator                // 技能自动创建器
	aliasMetrics  *modelconfig.AliasMetricsCollector // 别名级 LLM 指标采集器
	modelRegistry *modelconfig.ProviderRegistry      // 模型注册表 (新模式)
	modelResolver *modelconfig.ConfigResolver        // 模型配置解析器 (新模式)
	syncScheduler *claudesync.Scheduler              // 外部数据源同步调度器
	syncSubmitter claudesync.TaskSubmitter           // 非 nil = 同步执行体交 TaskService (design/02 §3.5)
	startTime     time.Time                          // 启动时间

	// 消息去重: 防止同一条消息触发多个团队
	processedMsgs sync.Map // messageID → timestamp

	// 图片+文字关联: 飞书中图片+文字是两条独立消息
	// 存储最近 30 秒内每个 chat 的文本消息，供图片消息关联
	recentTexts sync.Map // chatID → []textEntry
}

type textEntry struct {
	text string
	ts   time.Time
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
	if config.Headless {
		// headless serve 模式: 不连飞书, 凭证给占位值让 lark SDK 构造不炸 (从不真正调用)
		if config.AppID == "" {
			config.AppID = "headless"
		}
		if config.AppSecret == "" {
			config.AppSecret = "headless"
		}
	} else if config.AppID == "" || config.AppSecret == "" {
		return nil, fmt.Errorf("飞书 AppID 和 AppSecret 不能为空 (headless 部署请用 serve 子命令)")
	}
	if config.ModelAlias == "" {
		return nil, fmt.Errorf("AI ModelAlias 不能为空")
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

	// 1. 先创建模型配置注册表和解析器 (唯一配置路径)
	registry, resolver, err := modelconfig.LoadFromConfig(buildModelConfigJSON(config))
	if err != nil {
		return nil, fmt.Errorf("加载 providers 配置失败: %w", err)
	}
	if errs := modelconfig.Validate(registry, resolver); len(errs) > 0 {
		for _, e := range errs {
			log.Printf("[Bot] 配置验证警告: %s", e)
		}
	}

	// 2. 解析默认模型别名，获取完整的 API 连接参数
	defaultResolved := resolver.Resolve("", "")
	if defaultResolved.BaseURL == "" || defaultResolved.APIKey == "" || defaultResolved.ProviderName == "" {
		return nil, fmt.Errorf("无法解析默认模型别名 %q: 请检查 providers 配置", config.ModelAlias)
	}

	// 3. 创建 AI API 客户端 (从解析后的配置)
	aiClient := api.NewClient(defaultResolved.BaseURL, defaultResolved.APIKey, defaultResolved.ProviderName)
	aiClient.Tag = "feishu"
	if len(defaultResolved.FallbackModels) > 0 {
		aiClient.FallbackModels = defaultResolved.FallbackModels
		log.Printf("[Bot] 已配置 %d 个备用模型: %v", len(defaultResolved.FallbackModels), defaultResolved.FallbackModels)
	}
	if defaultResolved.FallbackBaseURL != "" {
		aiClient.FallbackBaseURL = defaultResolved.FallbackBaseURL
	}
	if defaultResolved.FallbackAPIKey != "" {
		aiClient.FallbackAPIKey = defaultResolved.FallbackAPIKey
	}
	if defaultResolved.PromptCacheMode != "" {
		aiClient.PromptCacheMode = defaultResolved.PromptCacheMode
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
	if config.PromptDebug {
		aiClient.PromptDebugEnabled = true
		aiClient.PromptDebugDir = config.PromptDebugDir
		aiClient.PromptDebugMaxFiles = config.PromptDebugMaxFiles
		aiClient.PromptDebugMaxBytes = config.PromptDebugMaxBytes
		aiClient.PromptDebugSampleRate = config.PromptDebugSampleRate
		aiClient.PromptDebugRedact = config.PromptDebugRedact
		if aiClient.PromptDebugDir == "" {
			aiClient.PromptDebugDir = filepath.Join(layout.Root, "prompt-debug")
		}
		log.Printf("[Bot] PromptDebug 已开启: %s", aiClient.PromptDebugDir)
	}

	// 初始化统一日志系统
	_ = logging.Init(&logging.LogConfig{
		Dir:     layout.Logs,
		Level:   "info",
		Console: true,
	})

	// 初始化可观测体系 (Hook Bus + MetricsEmitter + JSONL Store + Prompt Observatory)
	if _, err := observability.InitSystem(layout.Root); err != nil {
		log.Printf("[Bot] 可观测体系初始化失败: %v", err)
	}

	// 进程内唯一的 StateStore 实例 (design/02 §3.4.1): <state>/statestore/。
	// 必须唯一并共享 —— FileStore 的 bucket 锁表挂在实例上 (filestore.go:29-32),
	// 是 per-instance 而不是 per-path, 同一 root 建两个实例等于没有互斥;
	// KV 又是"读整桶 → 改一个 key → 整文件原子写", 交错写必然丢更新。
	// 使用方: TraceStore 轨迹底座 + 飞书会话历史快照 (经 WithStateStore 注入)。
	stateStore := statestore.NewFileStore(filepath.Join(layout.Root, "statestore"))

	// 轨迹 TTL 清理 (design/03 §4.1): SweepTraceFiles 此前全仓零调用方, 等于没有
	// TTL——常驻进程尤其需要它, 否则 statestore/log 与 blob 只增不减。
	// 由 CLAUDE_GO_TRACE_TTL 控制, 未设则不启动 (保持既有语义)。
	// 在此启动而非 SessionManager 内: 这一层才知道落盘路径。
	if ttl := tracestore.TTLFromEnv(); ttl > 0 {
		tracestore.StartJanitor(context.Background(),
			filepath.Join(layout.Root, "statestore", "log"), ttl, time.Hour)
	}

	// L1 网关档位选择 (design/02 §3.1 "实现两态" / §四 T1)。
	// 默认 local ⇒ 与改造前逐字节一致; CLAUDE_GO_LLM_GATEWAY_MODE=remote 时上层
	// 拿到的是 llmgw.Remote (本进程不做重试/熔断/fallback, 全部归网关进程)。
	// 档位配错 (remote 但没给网关地址 / 档位名拼错) **直接启动失败**, 不静默退回
	// local —— 那会让"流量经网关"这个部署事实变成谎话而运维看不出来。
	llmGW, err := llmgw.NewFromEnv(aiClient)
	if err != nil {
		return nil, fmt.Errorf("装配 L1 LLM 网关失败: %w", err)
	}

	bot := &Bot{
		config:    config,
		client:    larkClient,
		apiClient: aiClient,
		// L1 网关视图 (design/02 §3.1): bot 的非引擎类 LLM 调用只依赖
		// llmgw.LLMGateway 接口 —— evolution / skillAuto / intentRec / visionCli /
		// swarmEngine / dreamer / wikiEngine 七处注入点全部经 llmgw.SimpleClient。
		//
		// ② TeamManagerConfig.LLM 已收编 (2026-07-25): 先给 LLMGateway 补了 Diag
		// (= api.Client.CompleteDiag 的等价方法), 再把 pkg/agent 那处
		// `we.llm.(*api.Client)` 具体类型断言改成 agent.DiagLLMClient 鸭子类型,
		// 于是网关包装同样能取到 stop/outTok/blocks/tail 那组诊断元数据 —— 不换这
		// 两步就直接套接口, 断言会失败并**静默**回落 SimpleComplete: 产出照出、
		// 日志照打, 只有诊断那一栏永远空着。见本文件 teamMgr 装配处的 LLM 字段。
		//
		// ① SessionManager (引擎主链路) **保持不收编, 这是结论不是待办**:
		// QueryEngine 的流式 + 工具循环拿的不是"发一次请求"这种能力, 而是
		// api.Client 上三十余个具体字段/方法 —— Guard (RateLimitGuard 全局准入,
		// 还要被 advisor 客户端共享同一实例以免双客户端各自打满 RPM)、熔断三件套
		// (cbThreshold/circuitOpen/circuitOpenUntil)、FallbackModels +
		// FallbackBaseURL/FallbackAPIKey + 429 连续计数触发的模型切换与冷却回退、
		// PromptCacheMode 及其"因 API 错误自适应关闭"的状态位、
		// FirstTokenTimeout/CallTimeout/DeadlineRetryBase 三档超时、
		// PromptDebug 六件套、CallInterceptors、TotalRetries/CircuitTrips 计数器,
		// 以及 WithModel/ConfiguredCloneFull 这类**克隆语义** (共享 http.Client 与
		// Guard, 只换端点/模型)。把这些塞进 LLMGateway 等于把接口写成 api.Client
		// 的镜像, 抽象收益为零; 只挑几个塞进去则是更坏的结果 —— 熔断/配额这类
		// 状态**跨克隆共享**才有意义, 接口里丢一个字段不会编译报错, 只会让线上
		// 少一层保护而没人知道。真要收编的前置条件是 design/02 §3.1 的 remote 网关
		// (熔断/配额/fallback/prompt cache 集中到网关进程内), 那时上层才**不需要**
		// 这些字段; 在那之前, 包一个降级接口比不包更糟。
		llmGW:      llmGW,
		layout:     layout,
		stateStore: stateStore,
		startTime:  time.Now(),
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

	// 4b. V3 Anti-Amnesia: L2 结构化记忆
	bot.factStore = memory.NewFactStore(bot.layout.Memory)

	// 5. 解析 Hook 配置
	hookConfigs := bot.parseHookConfigs(config)

	// 6. 创建共享 V2 Task 存储 (Teams + LLM 工具共用同一实例)
	bot.taskStore = builtin.NewTaskStore(layout.TasksFilePath())

	// 7. 创建 Evolution 自动进化引擎 + Role Registry
	// 经 L1 网关注入 (design/02 §3.1 / §6「SimpleComplete 消费方全部改经 LLMGateway」):
	// EvolutionEngine 的 LLM 端口只有 SimpleComplete。
	bot.evolution = agent.NewEvolutionEngine(layout.Evolution, llmgw.SimpleClient{GW: bot.llmGW})
	roleReg := agent.NewRoleRegistry(config.Cwd)

	// 8. 创建会话管理器 (传入共享组件, 包括 Evolution + Roles + 唯一 StateStore)
	bot.sessions = NewSessionManager(config, aiClient, bot.mcpMgr, bot.skillReg, bot.dreamer, bot.memStore, hookConfigs, bot.taskStore, bot.evolution, roleReg,
		WithStateStore(stateStore))
	bot.sessions.SetMediaSendFn(bot.SendMediaToChat)

	// 9. 创建 Agent Pool (动态扩缩, 参考 ruflo v3)
	agentPool := agent.NewAgentPool(bot.sessions.CreateAgentRunner, 8)

	// 9b. 创建 PlanConfigResolver (modelconfig 的唯一包装)
	planCfgResolver := agent.NewPlanConfigResolver(resolver)
	log.Printf("[Bot] ProviderRegistry 已初始化: %d providers, %d aliases", len(registry.AllAliases()), len(registry.AllAliases()))

	// 注入默认解析配置到 SessionManager (用于 createSession 的 maxTokens/maxTurns)
	bot.sessions.SetDefaultModelConfig(defaultResolved)

	// Advisor 顾问工具客户端 (设计文档 docs/advisor-tool-design.md)
	if adv := config.Advisor; adv != nil && adv.Enabled && adv.ModelAlias != "" {
		advResolved := resolver.ResolveAlias(adv.ModelAlias)
		if advResolved.BaseURL == "" || advResolved.APIKey == "" || advResolved.ProviderName == "" {
			log.Printf("[Bot] Advisor 配置无效: 无法解析别名 %q (缺 baseURL/apiKey), advisor 已禁用", adv.ModelAlias)
		} else {
			advisorClient := api.NewClient(advResolved.BaseURL, advResolved.APIKey, advResolved.ProviderName)
			advisorClient.Tag = "advisor"
			if advResolved.CallTimeoutSec > 0 {
				advisorClient.CallTimeout = time.Duration(advResolved.CallTimeoutSec) * time.Second
			}
			if advResolved.FirstTokenTimeoutSec > 0 {
				advisorClient.FirstTokenTimeout = time.Duration(advResolved.FirstTokenTimeoutSec) * time.Second
			}
			// advisor 与主模型共享同一 provider 时复用限流 guard, 避免双客户端各自满额打爆 RPM
			if advResolved.Provider == defaultResolved.Provider {
				advisorClient.Guard = aiClient.Guard
			}
			bot.sessions.SetAdvisorClient(advisorClient)
			if advResolved.ProviderName == defaultResolved.ProviderName {
				log.Printf("[Bot] Advisor 已启用: %s (警告: 与主模型相同, self-consult 模式)", adv.ModelAlias)
			} else {
				log.Printf("[Bot] Advisor 已启用: %s (checkpoint: every=%d, onLoop=%v)", adv.ModelAlias, adv.CheckpointEveryTurns, adv.CheckpointOnLoop)
			}
		}
	}

	// Plan 模型客户端: 规划期独立模型 (类似 advisor 独立客户端, Tag=plan)。
	// config.PlanModel 非空时解析并注入; 规划期 (plan flag ON) queryLoop 全程路由到该客户端。
	if config.PlanModel != "" {
		planResolved := resolver.ResolveAlias(config.PlanModel)
		if planResolved.BaseURL == "" || planResolved.APIKey == "" || planResolved.ProviderName == "" {
			log.Printf("[Bot] Plan 模型配置无效: 无法解析别名 %q (缺 baseURL/apiKey), 规划期将沿用主模型", config.PlanModel)
		} else {
			planClient := api.NewClient(planResolved.BaseURL, planResolved.APIKey, planResolved.ProviderName)
			planClient.Tag = "plan"
			if planResolved.CallTimeoutSec > 0 {
				planClient.CallTimeout = time.Duration(planResolved.CallTimeoutSec) * time.Second
			}
			if planResolved.FirstTokenTimeoutSec > 0 {
				planClient.FirstTokenTimeout = time.Duration(planResolved.FirstTokenTimeoutSec) * time.Second
			}
			// plan 与主模型共享同一 provider 时复用限流 guard, 避免双客户端各自满额打爆 RPM
			if planResolved.Provider == defaultResolved.Provider {
				planClient.Guard = aiClient.Guard
			}
			bot.sessions.SetPlanClient(planClient)
			if planResolved.ProviderName == defaultResolved.ProviderName {
				log.Printf("[Bot] Plan 模型已启用: %s (警告: 与主模型相同)", config.PlanModel)
			} else {
				log.Printf("[Bot] Plan 模型已启用: %s (规划期专用)", config.PlanModel)
			}
		}
	}

	// 初始化全局 LLM 指标采集器 (让 feishu bot 的指标走全局 JSONL + Prometheus 路径)
	metrics.InitGlobalLLMCollector(layout.Root)
	if config.Sandbox != nil {
		sbxCfg := *config.Sandbox
		sbxCfg.StateDir = layout.Root
		sandbox.Configure(sbxCfg)
	} else {
		sandbox.Configure(sandbox.Config{StateDir: layout.Root})
	}

	// 设置全局 alias 解析器，让 recordLLMCall 能自动把 model 名解析为完整 alias
	metrics.SetAliasResolver(registry.LookupAliasByModelName)

	// 初始化别名级 Metrics 采集器 (按 alias 聚合 token/耗时/错误)
	aliasMetrics := modelconfig.NewAliasMetricsCollector(registry, layout.Root)
	oldHook := aiClient.OnLLMMetrics
	aiClient.OnLLMMetrics = func(rec api.LLMCallRecord) {
		if oldHook != nil {
			oldHook(rec)
		}
		aliasMetrics.Record(rec)
		// 发射到 observability bus (非侵入, 供 hook 体系消费)
		observability.Emit(observability.Event{
			Type:      observability.EvtLLMCallComplete,
			Timestamp: rec.Timestamp,
			Module:    "llm",
			Name:      rec.Status,
			Payload: map[string]interface{}{
				"record":        rec,
				"model":         rec.Model,
				"status":        rec.Status,
				"source":        rec.Source,
				"purpose":       rec.Purpose,
				"workflow":      rec.Workflow,
				"role":          rec.Role,
				"duration_sec":  rec.DurationSec,
				"input_tokens":  rec.InputTokens,
				"output_tokens": rec.OutputTokens,
				"retries":       rec.Retries,
				"error_kind":    rec.ErrorKind,
			},
		})
	}
	bot.aliasMetrics = aliasMetrics
	bot.modelRegistry = registry
	bot.modelResolver = resolver

	// 9b. 技能自动创建器 (提前到 teamMgr 之前构造, 供 SkillCreator 注入;
	// 历史上在第 15 步构造导致 teamMgr 拿不到 —— design/03 §1.2 开环2)
	// 经 L1 网关注入 (skills.LLMClient 亦只有 SimpleComplete); model 名仍取自具体客户端。
	bot.skillAuto = skills.NewAutoCreator(layout.Skills, llmgw.SimpleClient{GW: bot.llmGW}, aiClient.Model, bot.skillReg)

	// 9c. 统一学习循环 (design/03 §4.3)。此前 NewEvolutionLoop 全仓零生产调用方,
	// submitLearn 永远走回落直调 —— 循环写完了但没通电, 于是去重/预算闸/空闲期深度整理
	// 与学习器 d/e 的运行相位全都不存在。常驻进程尤其需要它 (空闲期才是做梦的时机)。
	botEvoLoop := agent.NewEvolutionLoop(bot.evolution, nil, agent.EvolutionLoopConfig{})
	botEvoLoop.EnableStructureLearning(agent.StructureConfig{
		StateDir:  layout.Root,
		Reflector: agent.FallbackReflector(aiClient), // 无 fallback 档位时为 nil, prompt 进化跳过 (§4.2 H3)
	})
	botEvoLoop.Start(context.Background())

	// 9d. H6 多档冒烟的真实档位构造器 (design/03 §4.5)。
	// SetEvoTierFactory 此前**零生产调用方** —— 于是 evo_smoke 只有一个确定性"装配档",
	// 而装配档不是模型档位, MinTiers>=2 必然拒绝: H6 的闸语义写完了但从没真跑过多档。
	// 注入点必须在装配处: pkg/tool/builtin 不能 import pkg/agent (那正是刚修掉的测试
	// 导入环的方向)。没配 fallback 模型时只会给出 primary 一档, Smoke 因档位不足而
	// 拒绝 —— 那是正确行为, 不是缺陷。
	if f := agent.EvoTierFactory(aiClient); f != nil {
		builtin.SetEvoTierFactory(f)
	}

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
		// 经 L1 网关 (design/02 §3.1 例外 ② 已收编): 合议扇出要的 CompleteDiag
		// 由 llmgw.SimpleClient 转调 LLMGateway.Diag, 诊断元数据一字不少。
		LLM:                llmgw.SimpleClient{GW: bot.llmGW},
		Evolution:          bot.evolution,
		EvolutionLoop:      botEvoLoop,
		TraceStore:         bot.sessions.TraceStore(), // gate Span (design/03 §4.1 第 5 种 Kind)
		Dreamer:            &dreamAdapter{dreamer: bot.dreamer},
		Roles:              roleReg,
		PlanConfigResolver: planCfgResolver,
		HookConfigs:        hookConfigs,
		SkillCreator:       bot.skillAuto,
	})

	// 10-extra. 加载用户自定义动态工作流 (~/.claude-go/workflows/*.json), 启动即注册到进程内,
	// 与 dashboard POST /api/workflows 注册的共用同一进程注册表 (此进程即 :18080 挂载 dashboard 的进程)。
	if loaded, failed, details := agent.LoadWorkflowsFromDir(filepath.Join(bot.layout.Root, "workflows"), roleReg); loaded > 0 || failed > 0 {
		log.Printf("[bot] 动态工作流加载: 成功 %d, 失败 %d %v", loaded, failed, details)
	}

	// 10a. 修复: SetTeamManager 必须在 teamMgr 创建后调用 (之前因时序 bug 注入了 nil)
	bot.sessions.SetTeamManager(bot.teamMgr)

	// 10b. 注入记忆写入 (团队完成后高权重记忆可被检索)
	bot.teamMgr.SetMemoryWriter(&memoryAdapter{store: bot.memStore})

	// 10c-extra. LLM 运行事件 (限流/熔断/致命错误) → EventBus → 飞书播报。
	// design/02 §3.4.2 L4 通信系统的第一条生产链路, 详见 startLLMEventBridge。
	bot.startLLMEventBridge()

	// 10c. 注入持续观测指标到 Dreamer (复用 teamMgr 的 Collector)
	if bot.dreamer != nil && bot.teamMgr.Metrics() != nil {
		bot.dreamer.MetricsRecorder = bot.teamMgr.Metrics()
	}

	// 11. 初始化意图识别器 (中文自然语言 → 自动拆解团队命令)
	bot.intentRec = agent.NewIntentRecognizer(llmgw.SimpleClient{GW: bot.llmGW}) // 经 L1 网关

	// 12. 初始化 Cron 定时任务调度器
	bot.cronSched = agent.NewCronScheduler(layout.Cron, &botCronExecutor{bot: bot})
	// 分布式 cron 选主 (design/02 §3.4.4): 设了 CLAUDE_GO_CRON_LEASE_DIR (K8s 多副本
	// 挂共享 PVC 到该目录) 时, 用文件租约防多副本重复触发同一定时任务。单副本不设即原样。
	if leaseDir := strings.TrimSpace(os.Getenv("CLAUDE_GO_CRON_LEASE_DIR")); leaseDir != "" {
		bot.cronSched.SetLease(cluster.NewFileLease(leaseDir, 5*time.Minute))
		log.Printf("[Cron] 分布式选主已启用, 租约目录: %s", leaseDir)
	}
	bot.cronSched.Start()

	// 13. 初始化 Vision 客户端 (经 L1 网关: vision.LLMClient = SimpleComplete + RawComplete,
	//     两件套都由 llmgw.SimpleClient 转调, 故网关接口必须带 Raw)
	bot.visionCli = vision.NewClient(llmgw.SimpleClient{GW: bot.llmGW})

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
		// 经 L1 网关注入 (design/02 §3.1): wiki.LLMClient 是"只有 SimpleComplete"
		// 的最小端口, llmgw.SimpleClient 把网关适配成该端口 —— wiki 包的接口定义
		// 一字未改, 但 /wiki/query·lint·organize·health-check 四个端点的 LLM 出站
		// 从此走网关接口。
		bot.wikiEngine = wiki.NewEngineWithLLM(wikiRepoDir, llmgw.SimpleClient{GW: bot.llmGW})
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

		// 初始化外部数据源同步调度器。
		// syncSubmitter 必须在 registerSyncJobs 之前就位: 注册的 job 闭包按它
		// 决定"提交任务"还是"进程内直跑", 晚一步注册的就全是老路。
		bot.syncSubmitter = config.Wiki.SyncTaskSubmitter
		bot.syncScheduler = claudesync.NewScheduler(config.Sync)
		bot.registerSyncJobs(config.Sync)
		bot.syncScheduler.Start()

		// 启动 Wiki HTTP API (供 Obsidian 插件调用)
		// 同端口还可以通过 config.Wiki.APIExtensions 扩展挂载 dashboard 等 HTTP 服务,
		// 避免飞书 bot / dashboard 同时监听多个端口。
		if config.Wiki.APIPort > 0 {
			wikiAPI := wiki.NewAPIServer(bot.wikiEngine, config.Wiki.APISecret)
			wikiAPI.SetBindHost(config.Wiki.APIHost)
			wikiAPI.SetScheduler(bot.syncScheduler)
			// design/02 §3.5: /sync/* 端点路径与响应形态不变, 执行体改提交 TaskService。
			// 必须在 Start 之前设置 —— 之后再设会留一个"已在听但还走老路"的窗口。
			if config.Wiki.SyncTaskSubmitter != nil {
				wikiAPI.SetSyncSubmitter(config.Wiki.SyncTaskSubmitter)
			}
			for _, ext := range config.Wiki.APIExtensions {
				if ext == nil {
					continue
				}
				ext(wikiAPI.Mux())
			}
			if err := wikiAPI.Start(config.Wiki.APIPort); err != nil {
				log.Printf("[Wiki API] 启动失败: %v", err)
			} else if len(config.Wiki.APIExtensions) > 0 {
				log.Printf("[Wiki API] 已挂载 %d 个外部扩展 (如 dashboard)", len(config.Wiki.APIExtensions))
			}
		}
	}

	// 14b. 初始化群体智能预测引擎
	{
		siCfg := swarm_intel.DefaultConfig()
		siCfg.Notify = func(chatID, msg string) {
			bot.sendLongMessage(context.Background(), chatID, msg)
		}
		bot.swarmEngine = swarm_intel.NewEngine(llmgw.SimpleClient{GW: bot.llmGW}, siCfg) // 经 L1 网关
		log.Printf("[SwarmIntel] 群体智能引擎已初始化")
	}

	// 15. (技能自动创建器已提前到 9b 构造)

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
			for _, cfg := range configs {
				b.addMCPServerWithTimeout(ctx, cfg)
			}
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
		b.addMCPServerWithTimeout(ctx, sc)
	}
}

func (b *Bot) addMCPServerWithTimeout(parent context.Context, sc mcp.ServerConfig) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := b.mcpMgr.AddServer(ctx, sc); err != nil {
		log.Printf("[飞书Bot] MCP 连接失败 (%s): %v", sc.Name, err)
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

	// 始终注入 LLM 客户端，dreamer 自动判断: 有 APIClient 则 LLM 整理, 否则本地整理。
	// 经 L1 网关 (dreaming.LLMClient 只有 SimpleComplete)。
	b.dreamer.SetAPIClient(llmgw.SimpleClient{GW: b.llmGW})
}

// parseHookConfigs 解析 Hook 配置
func (b *Bot) parseHookConfigs(config *BotConfig) []types.HookConfig {
	var hookConfigs []types.HookConfig
	for _, h := range config.Hooks {
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
			b.addMCPServerWithTimeout(ctx, cfg)
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
		b.config.AppID, b.config.Domain, b.apiClient.Model)
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
		go b.periodicMemoryMetrics(ctx)
	}
	if b.cronSched != nil {
		ct, ce, _ := b.cronSched.Stats()
		if ct > 0 {
			log.Printf("[飞书Bot] Cron: %d 个定时任务 (%d 启用)", ct, ce)
		}
	}

	if b.config.Headless {
		// headless serve 模式 (design/02 §四): 全栈已就绪 (:18080/cron/sync),
		// 不连接飞书 WS, 阻塞至 ctx 取消。
		log.Printf("[serve] headless 模式就绪: 不连接飞书 WS, HTTP 栈已启动")
		<-ctx.Done()
		return ctx.Err()
	}
	return b.wsClient.Start(ctx)
}

// Shutdown 关闭所有资源
func (b *Bot) Shutdown() {
	if b.cronSched != nil {
		b.cronSched.Stop()
	}
	if b.syncScheduler != nil {
		b.syncScheduler.Stop()
	}
	if b.cfgWatcher != nil {
		b.cfgWatcher.Stop()
	}
	b.stopLLMEventBridge()
	b.mcpMgr.Shutdown()
}

// periodicMemoryMetrics 定期采集 Dreaming/FactStore/失忆风险指标
func (b *Bot) periodicMemoryMetrics(ctx context.Context) {
	mc := b.teamMgr.Metrics()
	if mc == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	collect := func() {
		if b.dreamer != nil {
			stats := b.dreamer.Stats()
			mc.Record("dreaming", "dream_sessions_pending", float64(stats.SessionsSinceDream))
			mc.Record("dreaming", "dream_hours_since_last", stats.HoursSinceLast)
		}
		if b.factStore != nil {
			b.factStore.CollectMetrics(mc)
			fStats := b.factStore.Stats()
			dStats := b.dreamer.Stats()
			risk := memory.ComputeAmnesiaRisk(
				dStats.HoursSinceLast,
				dStats.SessionsSinceDream,
				fStats.AvgRetention,
				fStats.ActiveFacts,
				0, // compactLossRate: 需要 compact 模块配合，暂用 0
			)
			mc.Record("memory", "amnesia_risk_score", risk)
		}
	}

	collect()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

// DashboardTeamAction 供 dashboard 直接调用的团队操作 (create/run/stop/restart/delete/resume)。
func (b *Bot) DashboardTeamAction(action, teamName string, payload map[string]interface{}) error {
	if b.teamMgr == nil {
		return fmt.Errorf("teamMgr 未初始化")
	}
	switch action {
	case "create":
		workflow := ""
		objective := ""
		if payload != nil {
			if w, ok := payload["workflow"].(string); ok {
				workflow = w
			}
			if o, ok := payload["objective"].(string); ok {
				objective = o
			}
		}
		if workflow == "" {
			return fmt.Errorf("create 操作需要 workflow 参数")
		}
		if _, err := b.teamMgr.CreateTeam(teamName, workflow, objective, "dashboard"); err != nil {
			return err
		}
		return b.teamMgr.RunTeam(teamName, objective)
	case "run":
		objective := ""
		if payload != nil {
			if o, ok := payload["objective"].(string); ok {
				objective = o
			}
		}
		return b.teamMgr.RunTeam(teamName, objective)
	case "stop":
		return b.teamMgr.StopTeam(teamName)
	case "delete":
		return b.teamMgr.DeleteTeam(teamName)
	case "restart":
		if err := b.teamMgr.StopTeam(teamName); err != nil {
			return fmt.Errorf("停止失败: %w", err)
		}
		objective := ""
		if payload != nil {
			if o, ok := payload["objective"].(string); ok {
				objective = o
			}
		}
		return b.teamMgr.RunTeam(teamName, objective)
	case "resume":
		return b.teamMgr.ResumeTeam(teamName)
	case "refine":
		feedback, targetStage := "", ""
		if payload != nil {
			if f, ok := payload["feedback"].(string); ok {
				feedback = f
			}
			if ts, ok := payload["targetStage"].(string); ok {
				targetStage = ts
			}
		}
		if strings.TrimSpace(feedback) == "" {
			return fmt.Errorf("refine 操作需要 feedback 参数")
		}
		return b.teamMgr.RefineTeam(teamName, feedback, targetStage)
	case "fork":
		newName := ""
		if payload != nil {
			if n, ok := payload["newName"].(string); ok {
				newName = n
			}
		}
		if strings.TrimSpace(newName) == "" {
			return fmt.Errorf("fork 操作需要 newName 参数")
		}
		_, err := b.teamMgr.ForkTeam(teamName, newName)
		return err
	case "blackboard.write":
		// dashboard 的 POST /api/teams/:name/blackboard 排进 :7777 队列的动作
		// (design/01 §4.11)。此前这一支**不存在** ⇒ 落 default 报"未知操作" ⇒ 那个
		// 接口只改了磁盘文件, 而运行中团队的内存黑板会在下一次 debounce 刷盘时把它
		// 静默覆盖掉。理由与细节见 agent.WriteTeamBlackboard 的注释。
		key, value := "", ""
		if payload != nil {
			if k, ok := payload["key"].(string); ok {
				key = k
			}
			// value 只认字符串: 该接口在写队列前已把任意 JSON 压成字符串
			// (v13_handlers.go 的 valueStr), 这里再做一次类型分派等于两处各自解释
			// 同一个字段 —— 两套解释必然漂移。
			if v, ok := payload["value"].(string); ok {
				value = v
			}
		}
		return b.teamMgr.WriteTeamBlackboard(teamName, key, value, "dashboard", "dashboard_write")
	default:
		return fmt.Errorf("未知操作: %s", action)
	}
}

// DashboardMCPServers 供 dashboard 列出运行态 MCP 服务器 (返回可序列化的 ServerInfo 列表)。
func (b *Bot) DashboardMCPServers() interface{} {
	if b.mcpMgr == nil {
		return []interface{}{}
	}
	return b.mcpMgr.ListServers()
}

// DashboardReloadSkills 供 dashboard 创建 skill 后让 bot 的技能库即时重载。
func (b *Bot) DashboardReloadSkills() {
	if b.skillReg != nil {
		b.skillReg.Reload()
		if b.layout != nil {
			b.skillReg.LoadFromDirs([]string{b.layout.Skills}, "state")
		}
	}
}

// CronScheduler 返回 bot 的活动定时任务调度器, 供挂载的 dashboard 注入以启用 cron 写接口。
func (b *Bot) CronScheduler() *agent.CronScheduler { return b.cronSched }

// TeamManager 返回 bot 的生产团队管理器, 供主进程构造 TaskService 的运行器
// (design/01 §4.12): 动作队列里的 team.run 需要一个真能跑团队的执行体,
// 否则任务只能建档停在 pending。与 CronScheduler 同一模式: 只读暴露, 不转移所有权。
func (b *Bot) TeamManager() *agent.ProductionTeamManager { return b.teamMgr }

// LLMGateway 返回 bot 的 L1 网关 (design/02 §3.1)。
// 供需要 LLM 但不需要 api.Client 具体能力的子系统注入, 只读暴露不转移所有权
// (与 TeamManager/CronScheduler 同一模式)。
func (b *Bot) LLMGateway() llmgw.LLMGateway { return b.llmGW }

// DashboardLLMComplete 供 dashboard 调用 bot 的 LLM 客户端 (用于 LLM 生成工作流编排)。
// 经 L1 网关接口发出, 不再直接触碰 api.Client。
func (b *Bot) DashboardLLMComplete(ctx context.Context, sys, user string) (string, error) {
	if b.llmGW == nil {
		return "", fmt.Errorf("LLM 客户端未初始化")
	}
	return b.llmGW.Simple(ctx, sys, user)
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

// TriggerSync 触发外部数据源(ima/weread)同步 (供定时任务 jobType=sync), 返回结果摘要。
//
// design/02 §3.5: 这是同步的第三条触发路径 (cron job 的 jobType=sync)。接了
// TaskService 就提交给它 —— 摘要文案由 synctask.formatResult 产出, 与本函数
// 原来那行 Sprintf 逐字相同 (那条文案会进飞书通知, 改了用户就看得见)。
func (e *botCronExecutor) TriggerSync(ctx context.Context, source string) (string, error) {
	if e.bot.syncSubmitter != nil {
		job, err := e.bot.syncSubmitter.SubmitSync(source)
		if err != nil {
			return "", err
		}
		done, err := e.bot.syncSubmitter.WaitSync(ctx, job.ID)
		if err != nil {
			return "", err
		}
		if done.Status != claudesync.JobCompleted {
			return "", fmt.Errorf("同步失败: %s", done.Message)
		}
		return done.Message, nil
	}
	if e.bot.syncScheduler == nil {
		return "", fmt.Errorf("同步调度器未初始化")
	}
	cfg := e.bot.syncScheduler.Config()
	adapter, err := syncAdapterFor(cfg, source)
	if err != nil {
		return "", err
	}
	res, err := claudesync.RunSync(cfg, adapter.Source(), adapter)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("新增 %d · 更新 %d · 不变 %d · 删除 %d · 错误 %d",
		res.Created, res.Updated, res.Unchanged, res.Deleted, res.Errors), nil
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
	case "post": // 富文本消息: 检测图片+文字组合
		go b.handlePostMessage(chatID, messageID, msg)
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

	// 完成态消息→精修: 仅当会话中存在"已结束团队"时, 强精修措辞才路由到 RefineTeam。
	// 这样既实现"团队跑完后直接说哪里要改就继续优化", 又不会劫持没有团队上下文的普通聊天。
	if b.teamMgr != nil {
		if rIntent := b.intentRec.RecognizeRefine(ctx, userText); rIntent != nil {
			if latest := b.teamMgr.LatestFinishedTeam(chatID); latest != nil {
				go b.handleTeamRefineIntent(chatID, messageID, latest.Name, rIntent.Objective)
				return nil
			}
		}
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

	// 存储到 recentTexts: 供图片消息关联上下文 (飞书图片+文字是两条独立消息)
	b.storeRecentText(chatID, userText)

	// 异步处理消息 (避免阻塞飞书回调的 3 秒超时)
	go b.processAndReply(chatID, messageID, userText)

	return nil
}

// handlePostMessage 处理富文本(post)消息: 检测图片+文字组合。
// 飞书在用户发送图片+文字时, 会作为一个 post 消息送达, 内容包含 img 和 text 元素。
func (b *Bot) handlePostMessage(chatID, messageID string, msg *larkim.EventMessage) {
	content := deref(msg.Content)

	// 检测是否包含图片
	var imgKey string
	var textParts []string

	var raw struct {
		Title   string              `json:"title"`
		Content [][]json.RawMessage `json:"content"`
	}

	// 先尝试 locale wrapper
	var wrapper map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &wrapper) == nil {
		for _, locale := range []string{"zh_cn", "en_us", "ja_jp"} {
			if raw, ok := wrapper[locale]; ok {
				json.Unmarshal(raw, &raw)
				break
			}
		}
	}
	if len(raw.Content) == 0 {
		// 直接解析
		if json.Unmarshal([]byte(content), &raw) != nil {
			return
		}
	}

	for _, line := range raw.Content {
		for _, elemRaw := range line {
			var elem struct {
				Tag      string `json:"tag"`
				Text     string `json:"text"`
				ImageKey string `json:"image_key"`
			}
			if json.Unmarshal(elemRaw, &elem) != nil {
				continue
			}
			switch elem.Tag {
			case "img":
				if imgKey == "" {
					imgKey = elem.ImageKey
				}
			case "text":
				if strings.TrimSpace(elem.Text) != "" {
					textParts = append(textParts, strings.TrimSpace(elem.Text))
				}
			}
		}
	}

	if imgKey != "" && len(textParts) > 0 {
		// 图片+文字: 视觉理解 + 文字上下文
		b.handleImageWithText(chatID, messageID, imgKey, strings.Join(textParts, " "), deref(msg.ChatType))
	} else if imgKey != "" {
		// 纯图片
		b.handleImageMessage(chatID, messageID, msg)
	} else if len(textParts) > 0 {
		// 纯文字
		userText := strings.Join(textParts, "\n")
		b.processAndReply(chatID, messageID, userText)
	}
}

// handleImageWithText 处理图片+文字组合: 下载图片进行视觉理解, 结合文字上下文回复。
func (b *Bot) handleImageWithText(chatID, messageID, imageKey, text, chatType string) {
	ctx := context.Background()

	if b.visionCli == nil {
		b.sendTextMessage(ctx, chatID, "视觉能力未初始化。")
		return
	}

	b64, err := b.downloadImageAsBase64(ctx, messageID, imageKey)
	if err != nil {
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("下载图片失败: %v", err))
		return
	}

	prompt := fmt.Sprintf("用户发送了一张图片，并附言: %s\n\n请分析图片内容，并结合用户的附言进行回答。", text)

	response, err := b.visionCli.Understand(ctx, b64, prompt)
	if err != nil {
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("视觉分析失败: %v", err))
		return
	}

	b.sendTextMessage(ctx, chatID, response)
}

// handleImageMessage 处理图片消息：下载为 base64 → Vision 理解。
func (b *Bot) handleImageMessage(chatID, messageID string, msg *larkim.EventMessage) {
	ctx := context.Background()

	// 群聊中检查 @机器人 (与文本消息保持一致)
	chatType := deref(msg.ChatType)
	if chatType == "group" && b.config.MentionOnly {
		mentions := msg.Mentions
		if len(mentions) == 0 {
			if b.config.Debug {
				log.Printf("[飞书Bot] 图片消息忽略(群聊未@): chat=%s, msg=%s", chatID, messageID)
			}
			return
		}
	}

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

	// 检查是否有关联的文本消息 (飞书图片+文字是两条独立消息)
	associatedText := b.getRecentText(chatID)
	var reply strings.Builder
	reply.WriteString("🖼️ **图片分析结果:**\n\n")
	reply.WriteString(response)
	if associatedText != "" {
		reply.WriteString("\n\n📝 **关联文字:** " + associatedText)
		// 如果有图片描述+文字，交给 AI 综合处理
		go b.processAndReply(chatID, messageID, "用户发送了一张图片和一段文字:\n图片内容: "+response+"\n文字: "+associatedText)
		return
	}
	b.sendLongMessage(ctx, chatID, reply.String())
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
	if b.config.Headless {
		log.Printf("[serve][media %s] 跳过发送图片 %s (%d 字节)", chatID, filename, len(imageData))
		return nil
	}
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
	if b.config.Headless {
		log.Printf("[serve][media %s] 跳过发送文件 %s (%d 字节)", chatID, filename, len(fileData))
		return nil
	}
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

	// post 消息可能包裹在 locale key 下, 也可能是直接结构
	var wrapper map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &wrapper) == nil {
		for _, locale := range []string{"zh_cn", "en_us", "ja_jp"} {
			if raw, ok := wrapper[locale]; ok {
				if json.Unmarshal(raw, &post) == nil && len(post.Content) > 0 {
					goto extract
				}
			}
		}
		// 不是 locale wrapper, 直接解析
		if json.Unmarshal([]byte(content), &post) != nil || len(post.Content) == 0 {
			return ""
		}
	} else {
		return ""
	}

extract:
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
			"**Advisor (顾问):**\n" +
			"- /advisor - 查看 advisor 状态\n" +
			"- /advisor <provider:model> - 启用/切换 advisor 模型\n" +
			"- /advisor off - 关闭 advisor\n\n" +
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
			"- /team resume <名称> - 恢复执行 (从检查点继续)\n" +
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
		advisorInfo := "未启用"
		if b.sessions.AdvisorEnabled() {
			advisorInfo = b.sessions.AdvisorModel()
		}
		status := fmt.Sprintf("**运行状态**\n"+
			"- 运行时长: %v\n"+
			"- 总会话数: %d\n"+
			"- 活跃处理: %d\n"+
			"- AI 模型: %s\n"+
			"- Advisor: %s\n"+
			"- MCP: %s\n"+
			"- Skills: %s\n"+
			"- Dreaming: %s\n"+
			"- Evolution: %s\n"+
			"- Cron: %s\n"+
			"- 工作目录: %s",
			uptime, total, active, b.apiClient.Model, advisorInfo, mcpInfo, skillInfo, dreamInfo, evoInfo, cronInfo, b.config.Cwd)
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

	case strings.HasPrefix(lower, "/cwd"):
		b.handleCwdCommand(ctx, chatID, messageID, text)
		return true

	case lower == "/dream":
		b.handleDreamCommand(ctx, messageID)
		return true

	case strings.HasPrefix(lower, "/advisor"):
		b.handleAdvisorCommand(ctx, messageID, text)
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
		msg := fmt.Sprintf("**角色:** %s\n**解析后角色:** %s\n**描述:** %s\n**文件技能:** %s\n**内置技能:** %s\n**推荐技能:** %s\n**实际注入技能:** %s\n**预计注入字符:** %d",
			info.Requested,
			info.Resolved,
			info.Description,
			formatNames(info.FileSkills),
			formatNames(info.BuiltinSkills),
			formatNames(info.RecommendedSkills),
			formatNames(info.InjectedSkills),
			info.InjectedSkillChars,
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
			"工作流: research, development, debate, creative, finance, techblog, swarm, ml-training, app, game\n"+
			"示例: /go research 调研 kubernetes 最佳实践")
		return
	}

	workflow := strings.ToLower(parts[1])
	objective := strings.Join(parts[2:], " ")
	team, err := b.teamMgr.CreateTeamWithUniquePrefix("go-"+workflow, workflow, objective, chatID)
	if err != nil {
		b.sendTextReply(ctx, messageID, fmt.Sprintf("创建失败: %v", err))
		return
	}
	teamName := team.Name

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
		b.sendTextReply(ctx, messageID, "用法: /team [create|run|resume|status|stop|list|delete|workflows]")
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

	case "resume":
		if len(parts) < 3 {
			b.sendTextReply(ctx, messageID, "用法: /team resume <名称>")
			return
		}
		name := parts[2]
		if err := b.teamMgr.ResumeTeam(name); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("恢复失败: %v", err))
			return
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("♻️ 团队 **%s** 已从检查点恢复执行", name))

	case "refine":
		if len(parts) < 4 {
			b.sendTextReply(ctx, messageID, "用法: /team refine <名称> <反馈> [--stage 阶段名]\n说明: 让已完成/失败的团队带反馈继续优化; --stage 指定从某阶段起增量重跑(省 token)。")
			return
		}
		name := parts[2]
		stage := ""
		var fbParts []string
		for i := 3; i < len(parts); i++ {
			if parts[i] == "--stage" && i+1 < len(parts) {
				stage = parts[i+1]
				i++
			} else {
				fbParts = append(fbParts, parts[i])
			}
		}
		if err := b.teamMgr.RefineTeam(name, strings.Join(fbParts, " "), stage); err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("精修失败: %v", err))
			return
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("🛠️ 团队 **%s** 已带反馈进入精修迭代, 完成后通知。", name))

	case "fork":
		if len(parts) < 4 {
			b.sendTextReply(ctx, messageID, "用法: /team fork <源团队> <新团队名>")
			return
		}
		nt, err := b.teamMgr.ForkTeam(parts[2], parts[3])
		if err != nil {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("fork 失败: %v", err))
			return
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("🍴 已从 **%s** 复制出团队 **%s** (状态: %s)\n用 `/team refine %s <反馈>` 继续迭代", parts[2], nt.Name, nt.Status, nt.Name))

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

// handleTeamRefineIntent 把"完成态消息"作为反馈, 对最近结束的团队发起精修迭代。
func (b *Bot) handleTeamRefineIntent(chatID, messageID, teamName, feedback string) {
	ctx := context.Background()
	if err := b.teamMgr.RefineTeam(teamName, feedback, ""); err != nil {
		b.sendTextReply(ctx, messageID, fmt.Sprintf("精修失败: %v", err))
		return
	}
	b.sendTextReply(ctx, messageID, fmt.Sprintf(
		"🛠️ 已把你的反馈作为团队 **%s** 的精修目标, 带反馈重新迭代中, 完成后通知。\n"+
			"(如需指定从某阶段起重跑, 用 `/team refine %s <反馈> --stage <阶段>`)", teamName, teamName))
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

// handleAdvisorCommand 处理 /advisor 命令: 查看状态 / 启用 / 切换模型 / 关闭。
// 对齐 Claude Code 官方 /advisor 命令语义 (设计文档 docs/advisor-tool-design.md)。
func (b *Bot) handleAdvisorCommand(ctx context.Context, messageID, text string) {
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/advisor"))
	switch {
	case arg == "":
		if b.sessions.AdvisorEnabled() {
			b.sendTextReply(ctx, messageID, fmt.Sprintf(
				"**Advisor 状态**: 已启用\n- 模型: %s\n- 主模型在关键时刻可调用 advisor() 咨询该模型\n\n用法: `/advisor off` 关闭, `/advisor <provider:model>` 切换模型",
				b.sessions.AdvisorModel()))
		} else {
			b.sendTextReply(ctx, messageID,
				"**Advisor 状态**: 未启用\n\n用法: `/advisor <provider:model>` 启用 (如 `/advisor kimi:kimi-k2-thinking`)")
		}
	case strings.EqualFold(arg, "off") || strings.EqualFold(arg, "unset"):
		b.sessions.SetAdvisorClient(nil)
		b.sendTextReply(ctx, messageID, "Advisor 已关闭。现有会话发送 /clear 后完全生效。")
	default:
		if b.modelResolver == nil {
			b.sendTextReply(ctx, messageID, "无法切换: 模型解析器未初始化")
			return
		}
		resolved := b.modelResolver.ResolveAlias(arg)
		if resolved.BaseURL == "" || resolved.APIKey == "" || resolved.ProviderName == "" {
			b.sendTextReply(ctx, messageID, fmt.Sprintf("无法解析别名 `%s`: 请检查 providers 配置", arg))
			return
		}
		client := api.NewClient(resolved.BaseURL, resolved.APIKey, resolved.ProviderName)
		client.Tag = "advisor"
		if resolved.CallTimeoutSec > 0 {
			client.CallTimeout = time.Duration(resolved.CallTimeoutSec) * time.Second
		}
		if resolved.FirstTokenTimeoutSec > 0 {
			client.FirstTokenTimeout = time.Duration(resolved.FirstTokenTimeoutSec) * time.Second
		}
		b.sessions.SetAdvisorClient(client)
		note := ""
		if resolved.ProviderName == b.apiClient.Model {
			note = "\n⚠️ 与主模型相同 (self-consult 模式)，建议配置更强的模型作为 advisor。"
		}
		b.sendTextReply(ctx, messageID, fmt.Sprintf("Advisor 已设置为 **%s**。新会话立即生效; 现有会话发送 /clear 后生效。%s", arg, note))
	}
}

func (b *Bot) handleCwdCommand(ctx context.Context, chatID, messageID, text string) {
	parts := strings.Fields(text)
	if len(parts) == 1 {
		b.sendTextReply(ctx, messageID, fmt.Sprintf("当前工作目录: `%s`\n用法: `/cwd set <绝对路径或~路径>`", b.config.Cwd))
		return
	}
	if len(parts) < 3 || strings.ToLower(parts[1]) != "set" {
		b.sendTextReply(ctx, messageID, "用法: `/cwd` 查看当前工作目录，或 `/cwd set <path>` 临时切换。")
		return
	}
	target := strings.TrimSpace(strings.Join(parts[2:], " "))
	if strings.HasPrefix(target, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			target = filepath.Join(home, strings.TrimPrefix(target, "~"))
		}
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		b.sendTextReply(ctx, messageID, fmt.Sprintf("路径解析失败: %v", err))
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		b.sendTextReply(ctx, messageID, fmt.Sprintf("目录不存在或不可访问: `%s`", abs))
		return
	}

	b.config.Cwd = abs
	if b.sessions != nil {
		b.sessions.SetCwd(abs)
		b.sessions.ClearSession(chatID)
	}
	if b.teamMgr != nil {
		b.teamMgr.SetCwd(abs)
	}
	b.sendTextReply(ctx, messageID, fmt.Sprintf("已临时切换工作目录为: `%s`\n当前会话已清空，后续新团队会使用该目录。", abs))
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

	// 复杂任务自动 Plan Mode 已内建于引擎 (AutoModeHook): 首轮自动注入 system-reminder
	// 引导 + 会话级只读规划, 由模型自行 ExitPlanMode 进入实施。此处不再注入
	// [AutoPlanBuild] 文本前缀 (旧 hack, 不进真正的 Plan Mode)。

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
	if b.config.Headless {
		// headless 模式: 出站飞书消息降级为日志 (无有效凭证, 也无人在飞书侧接收)
		log.Printf("[serve][notify %s] %s", chatID, truncateResult(content, 400))
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

// storeRecentText 存储最近的文本消息，供图片消息关联上下文。
func (b *Bot) storeRecentText(chatID, text string) {
	if chatID == "" || text == "" {
		return
	}
	var texts []textEntry
	if v, ok := b.recentTexts.Load(chatID); ok {
		if ts, ok := v.([]textEntry); ok {
			texts = ts
		}
	}
	now := time.Now()
	texts = append(texts, textEntry{text: text, ts: now})
	// 清理超过 60 秒的旧条目
	var kept []textEntry
	for _, t := range texts {
		if now.Sub(t.ts) < 60*time.Second {
			kept = append(kept, t)
		}
	}
	b.recentTexts.Store(chatID, kept)
}

// getRecentText 获取最近 30 秒内的文本消息，用于关联图片上下文。
func (b *Bot) getRecentText(chatID string) string {
	v, ok := b.recentTexts.Load(chatID)
	if !ok {
		return ""
	}
	texts, ok := v.([]textEntry)
	if !ok || len(texts) == 0 {
		return ""
	}
	now := time.Now()
	// 从后往前找 30 秒内的第一条
	for i := len(texts) - 1; i >= 0; i-- {
		if now.Sub(texts[i].ts) < 30*time.Second {
			return texts[i].text
		}
	}
	return ""
}

// deref 安全解引用字符串指针
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// registerSyncJobs 根据配置注册 IMA / 微信读书定时同步任务。
//
// design/02 §3.5「执行体改提交 TaskService」：syncSubmitter 非 nil 时 tick 只负责
// **提交 + 等结果**，真正的 RunSync 由 TaskService 的执行器跑。日志口径保持不变。
//
// 这是本仓的第二条同步执行路径（第一条是 /sync/* 端点）。**两条必须一起换**：
// 只换端点会留下"手动触发经任务服务、cron tick 仍进程内直跑"的局面，而 RunSync
// 在 LoadIndex 与 SaveIndex 之间无锁 —— 两条路同时跑同一个 source 就是
// index.json 互相覆盖。走同一个 TaskService 后，活跃索引会把后来那次归并掉。
func (b *Bot) registerSyncJobs(cfg claudesync.Config) {
	if cfg.KnowledgeRepo == "" {
		return
	}
	// runOnce 单一执行入口：接了 TaskService 就提交，否则维持历史的进程内直跑。
	runOnce := func(ctx context.Context, source, label string) error {
		if b.syncSubmitter != nil {
			job, err := b.syncSubmitter.SubmitSync(source)
			if err != nil {
				log.Printf("[Sync] %s 提交同步任务失败: %v", label, err)
				return err
			}
			done, err := b.syncSubmitter.WaitSync(ctx, job.ID)
			if err != nil {
				log.Printf("[Sync] %s 同步任务 %s 等待失败: %v", label, job.ID, err)
				return err
			}
			if done.Status != claudesync.JobCompleted {
				log.Printf("[Sync] %s 同步失败: %s", label, done.Message)
				return fmt.Errorf("%s 同步失败: %s", label, done.Message)
			}
			log.Printf("[Sync] %s 同步完成 (task %s): %s", label, job.ID, done.Message)
			return nil
		}
		adapter, err := syncAdapterFor(cfg, source)
		if err != nil {
			return err
		}
		res, err := claudesync.RunSync(cfg, adapter.Source(), adapter)
		if err != nil {
			log.Printf("[Sync] %s 同步失败: %v", label, err)
			return err
		}
		log.Printf("[Sync] %s 同步完成: %+v", label, res)
		return nil
	}
	if cfg.IMA.Enabled && cfg.IMA.Cron != "" {
		if err := b.syncScheduler.Register("ima", cfg.IMA.Cron, func(ctx context.Context) error {
			return runOnce(ctx, "ima", "IMA")
		}); err != nil {
			log.Printf("[Sync] 注册 IMA 任务失败: %v", err)
		} else {
			log.Printf("[Sync] IMA 定时同步已注册: %s", cfg.IMA.Cron)
		}
	}
	if cfg.WeRead.Enabled && cfg.WeRead.Cron != "" {
		if err := b.syncScheduler.Register("weread", cfg.WeRead.Cron, func(ctx context.Context) error {
			return runOnce(ctx, "weread", "微信读书")
		}); err != nil {
			log.Printf("[Sync] 注册微信读书任务失败: %v", err)
		} else {
			log.Printf("[Sync] 微信读书定时同步已注册: %s", cfg.WeRead.Cron)
		}
	}
}

// syncAdapterFor 按 source 造适配器。集中在一处, 免得"加一个源要改三个 switch"。
func syncAdapterFor(cfg claudesync.Config, source string) (claudesync.Adapter, error) {
	switch source {
	case "ima":
		return claudesync.NewIMAAdapter(cfg.IMA.ClientID, cfg.IMA.APIKey), nil
	case "weread":
		return claudesync.NewWeReadAdapter(cfg.WeRead.APIKey), nil
	default:
		return nil, fmt.Errorf("未知同步源 %q (应为 ima 或 weread)", source)
	}
}

// buildModelConfigJSON 将 BotConfig 转换为 modelconfig.ConfigJSON。
func buildModelConfigJSON(cfg *BotConfig) modelconfig.ConfigJSON {
	var out modelconfig.ConfigJSON
	out.Providers = make(map[string]modelconfig.ProviderConfig, len(cfg.Providers))
	for name, p := range cfg.Providers {
		models := make(map[string]modelconfig.ModelConfig, len(p.Models))
		for alias, mc := range p.Models {
			models[alias] = modelconfig.ModelConfig{
				MaxTokens:       mc.MaxTokens,
				MaxTurns:        mc.MaxTurns,
				PromptCacheMode: mc.PromptCacheMode,
				ContextWindow:   mc.ContextWindow,
			}
		}
		out.Providers[name] = modelconfig.ProviderConfig{
			Name:    p.Name,
			BaseURL: p.BaseURL,
			APIKey:  p.APIKey,
			Models:  models,
		}
	}

	out.AI.GlobalConfig = modelconfig.GlobalConfig{
		DefaultModelAlias:      cfg.ModelAlias,
		DefaultFallbackAliases: cfg.FallbackAliases,
		DefaultPromptCacheMode: cfg.PromptCacheMode,
	}
	out.AI.Plans = make(map[string]modelconfig.PlanConfig, len(cfg.Plans))
	for planName, pc := range cfg.Plans {
		plan := modelconfig.PlanConfig{
			ModelAlias:      pc.ModelAlias,
			FallbackAliases: pc.FallbackAliases,
		}
		plan.Roles = make(map[string]modelconfig.RoleConfig, len(pc.RoleAliases))
		for role, alias := range pc.RoleAliases {
			plan.Roles[role] = modelconfig.RoleConfig{ModelAlias: alias}
		}
		out.AI.Plans[planName] = plan
	}

	return out
}
