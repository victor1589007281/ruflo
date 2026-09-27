package feishu

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/agentbind"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/complexity"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	sessionstore "github.com/anthropic/claude-go/pkg/session"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/sessionapi"
	"github.com/anthropic/claude-go/pkg/types"
	"github.com/anthropic/claude-go/pkg/weakmodel"
)

// Session 单个飞书会话。
// 每个 chat_id 对应一个独立的 Session，包含:
//   - 独立的 QueryEngine (维护对话历史)
//   - 独立的工具注册表
//   - 活跃时间追踪 (用于超时清理)
//   - 消息队列 (排队等待, 防止消息丢失)
//
// 对应概念: 类似 Claude Code 中每个 terminal tab 的独立会话。
type Session struct {
	ChatID      string
	Engine      *engine.QueryEngine
	LastActive  time.Time
	ToolProfile builtin.ToolProfile
	mu          sync.Mutex
	processing  bool // 是否正在处理消息 (防止并发请求)

	// DisplayName 会话的展示名 (轨迹分组用, 见 session_trace.go 的 displayName)。
	// 空 = 回落成 ChatID —— 回落值本身有意义, 不是占位符。
	// 由 spec 装配 (session_spec.go) 在构造期写一次, 之后只读, 故不纳入 mu 保护。
	DisplayName string

	// 消息队列: 当 processing=true 时, 后续消息入队等待, 处理完自动消费
	pendingMsg   *string      // 最多缓存 1 条待处理消息 (最新的覆盖旧的)
	pendingReply func(string) // 队列消息的回复回调

	// reservedRunID 是 HTTP 面 (pkg/sessionapi) **同步**占位时分配的这次运行的
	// runID (见 session_host.go 的 Reserve)。非空 = 已被占住, 后续消息一律 409。
	//
	// 为什么需要它, 而不是复用 processing: processing 是在 runSessionMessage
	// **内部**才置上的, 从 HTTP 收到请求到那一刻之间有窗口 —— 两条同时到达的消息
	// 会双双通过检查、双双拿到 202, 然后其中一条在 runSessionMessage 开头看到
	// IsProcessing 就返回 "上一条消息还在处理中" 并当成**成功**返回。调用方握着两个
	// runId, 等一个永远不会完成的运行。
	//
	// 同时它也是 runID 的唯一来源: ctx 里的 trace.IDs.RunID 与回给调用方的轮询键
	// 必须是同一个字符串 (见 session_host.go Reserve 的注释)。
	// reservedAt 是占位的置上时刻, 用于租约过期 (见 session_host.go 的
	// reservationLive): 占位只有一个释放出口, 而进程在 Reserve 之后、Run 之前
	// 崩掉的那条路径走不到它 —— 租约让那种占位自己过期, 而不是让会话永久 409。
	reservedAt time.Time

	reservedRunID string

	// transcript 会话历史快照句柄 (nil = 未注入 StateStore, 持久化关闭)。
	// 引擎每轮末经 Engine.SnapshotFn 调它落盘; /clear 经它删盘。
	transcript *sessionstore.StateStoreTranscript
}

// IsProcessing 检查当前会话是否正在处理消息
func (s *Session) IsProcessing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processing
}

// SetProcessing 设置处理状态
func (s *Session) SetProcessing(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processing = v
}

// EnqueuePending 将消息加入等待队列 (仅保留最新一条, 覆盖旧消息)
func (s *Session) EnqueuePending(text string, reply func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingMsg = &text
	s.pendingReply = reply
}

// DequeuePending 取出并清空等待队列, 返回 nil 表示队列为空
func (s *Session) DequeuePending() (*string, func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := s.pendingMsg
	reply := s.pendingReply
	s.pendingMsg = nil
	s.pendingReply = nil
	return msg, reply
}

// Touch 更新最后活跃时间
func (s *Session) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastActive = time.Now()
}

// SessionManager 会话管理器。
// 维护 chat_id → Session 的映射，支持:
//   - 自动创建: 首次收到某 chat_id 消息时创建会话
//   - 超时清理: 定期清理超过 SessionTimeout 的闲置会话
//   - 并发限制: MaxSessions 控制最大并发会话数
//
// 算法:
//
//	GetOrCreate(chatID):
//	  if sessions[chatID] exists → return it
//	  if len(sessions) >= maxSessions → evict oldest
//	  create new Session with fresh QueryEngine
//	  sessions[chatID] = newSession
//	  return newSession
//
// MediaSendFunc 飞书媒体发送回调（图片/文件）。
type MediaSendFunc func(ctx context.Context, chatID string, data []byte, filename, mediaType string) error

type SessionManager struct {
	sessions        map[string]*Session
	mu              sync.RWMutex
	config          *BotConfig
	apiClient       *api.Client
	maxSessions     int
	sessionTimeout  time.Duration
	mcpMgr          *dynmcp.Manager
	skillReg        *skills.Registry
	dreamer         *dreaming.Dreamer
	memoryStore     *memory.TieredStore
	hookConfigs     []types.HookConfig
	taskStore       *builtin.TaskStore           // 共享 V2 Task 存储 (Teams + LLM 工具共用)
	evolution       *agent.EvolutionEngine       // 进化引擎 (注入 agent runner hooks)
	traceStore      *tracestore.Store            // design/03 §4.1 E1: 共享轨迹底座 (注入各 stage 隔离引擎)
	roleRegistry    *agent.RoleRegistry          // 角色注册表
	mediaSendFn     MediaSendFunc                // 飞书发送图片/文件的回调
	teamMgr         *agent.ProductionTeamManager // 团队管理器 (供 TeamQuery 工具使用)
	searcher        builtin.WebSearcher          // Web 搜索适配器 (浏览器)
	defaultResolved modelconfig.ResolvedConfig   // 默认模型解析配置 (从 alias 解析)

	// modelResolver 是默认模型的**解析器**, 与 defaultResolved 配对: 后者是
	// "默认别名解析成了什么", 只有前者能回答"另一个别名会解析成什么"。
	//
	// 会话身份声明 (spec) 里的 ModelAlias 需要后者 —— 只换 cli.Model 而不换
	// baseURL/apiKey/protocol 的话, 别名一旦指向另一个厂商端点, 请求会带着别人的
	// 模型名发到默认端点去。可为 nil (未注入): 此时 spec 的模型声明**不生效并记日志**,
	// 而不是悄悄按默认模型跑 (那会让人以为模型配上了)。
	modelResolver *modelconfig.ConfigResolver

	// stateStore 进程内唯一的状态存储实例 (由 Bot 注入, 见 WithStateStore)。
	// 承载: TraceStore 轨迹底座 (design/03 §4.1 E1) + 会话历史快照 (design/02 R2)。
	stateStore statestore.StateStore

	// assetSet 进程级资产索引 (由 Bot 注入, 见 WithAssetSet)。
	//
	// 目前唯一的消费者是池装配 (newProfileRegistry 把 hook 资产登记进池)。
	// nil = 池里不收 hook, 与改造前逐字节一致。
	//
	// ★ 必须是 atomic.Pointer 而不是 sm.mu 保护: 读侧 (newProfileRegistry) 运行在
	// **已持有 sm.mu 写锁**的会话创建路径上 (GetOrCreate→createSession), 再用
	// sm.mu.RLock 就是同锁重入 —— RWMutex 不可重入, 写锁内的 RLock 永久自死锁
	// (2026-09-27 实测: 一条 POST /api/sessions/{id}/messages 把整个会话面挂死,
	// goroutine dump 显示 AssetSet 的 RLock 等在自己路径持有的写锁上)。
	assetSet atomic.Pointer[asset.Set]

	// Advisor 顾问工具 (设计文档 docs/advisor-tool-design.md)
	advisorClient *api.Client                     // advisor 模型专用客户端 (nil=禁用)
	advisorTools  map[string]*builtin.AdvisorTool // chatID → 工具实例 (预算按会话隔离, profile 切换不重置)
	advisorMu     sync.Mutex

	// planClient 规划期独立模型客户端 (Tag=plan, nil=禁用)。plan flag ON (规划期) 时
	// createSession 把该客户端塞进 engine.Config.PlanClient, queryLoop 全程路由到它,
	// 类似 advisor 独立客户端。由 Bot 初始化时经 SetPlanClient 注入。
	planClient *api.Client
	planMu     sync.Mutex

	// runSeq 会话运行的发号器 (trace 桶名用, 见 session_trace.go)。
	// atomic 而非普通 uint64: 多个会话的消息可能并发跑 (每个 Session 自己的
	// processing 闸门只保证**同一会话**串行), 计数器是跨会话共享的。
	runSeq atomic.Uint64

	// specs 是会话身份声明 (人格/工具白名单/cwd/模型) 的落盘存储, 见 session_spec.go。
	// 为 nil 时 (测试里直接构造的 SessionManager) 所有 spec 操作降级成 no-op。
	specs *specStore
}

// SessionManagerOption NewSessionManager 的可选装配项。
// 用 options 而不是继续加位置参数: 构造签名已经 10 个参数, 再堆无法读。
type SessionManagerOption func(*SessionManager)

// WithStateStore 注入进程内唯一的 StateStore 实例。
//
// 为什么必须是"唯一实例"而不是各处按同一 root 各建一个 FileStore:
// FileStore 的 bucket 锁表挂在实例上 (filestore.go:29-32), 锁是 per-instance
// 而非 per-path (包注释 filestore.go:25 也明说跨进程互斥不在保证范围)。
// 同进程对同一 root 建两个 FileStore, 两边的写就完全没有互斥 —— 而 KV 又是
// "读整桶→改一个 key→整文件原子写", 交错写必然丢更新。
func WithStateStore(ss statestore.StateStore) SessionManagerOption {
	return func(sm *SessionManager) { sm.stateStore = ss }
}

// WithAssetSet 注入进程级资产索引 (21.8.12 P3: hook 进池的输入)。
//
// 与 agent.SetAgentCatalog 的关系: 那一个是**查表**用的进程级单例, 这一个把同一份
// 索引交给 SessionManager, 供**池装配**时枚举 hook 资产。同一份实例、两个消费者,
// 不各建一份 —— 两份索引会出现"agent 查得到、池里没有它"这种只在一个方向上现形的
// 差异, 而它不会报错。
//
// 未注入 (nil) = 池里不收 hook 资产, 与改造前逐字节一致。
func WithAssetSet(s *asset.Set) SessionManagerOption {
	return func(sm *SessionManager) { sm.assetSet.Store(s) }
}

// SetAssetSet 在运行期换掉资产索引 (agent 资产被创建/删除之后, 见 Bot.DashboardReloadAssets)。
//
// 存在的理由: 构造期的 WithAssetSet 只在 NewSessionManager 那一刻生效, 而"新建一个
// agent"发生在进程跑起来之后。没有这个 setter 的话, 新 agent 只对 --agent 那条路
// (读 agent 包里的单例) 可见, 池装配看到的仍是启动时那份快照 —— 表现为"飞书/团队里
// 查不到刚建的 agent", 而 CLI 里能跑, 两个入口给出不同答案且都不报错。
func (sm *SessionManager) SetAssetSet(s *asset.Set) {
	sm.assetSet.Store(s)
}

// AssetSet 取当前资产索引 (可能为 nil)。无锁 (atomic.Pointer) —— 它**必须**能在
// sm.mu 写锁已被持有的会话创建路径上调用, 见 assetSet 字段注释里的自死锁记录。
func (sm *SessionManager) AssetSet() *asset.Set {
	return sm.assetSet.Load()
}

// NewSessionManager 创建会话管理器。
// 所有共享组件由 Bot 创建并传入。
func NewSessionManager(config *BotConfig, apiClient *api.Client, mcpMgr *dynmcp.Manager, skillReg *skills.Registry, dreamer *dreaming.Dreamer, memStore *memory.TieredStore, hookConfigs []types.HookConfig, taskStore *builtin.TaskStore, evolution *agent.EvolutionEngine, roleRegistry *agent.RoleRegistry, opts ...SessionManagerOption) *SessionManager {
	maxSessions := config.MaxSessions
	if maxSessions <= 0 {
		maxSessions = 100
	}
	timeout := config.SessionTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	sm := &SessionManager{
		sessions:       make(map[string]*Session),
		config:         config,
		apiClient:      apiClient,
		maxSessions:    maxSessions,
		sessionTimeout: timeout,
		mcpMgr:         mcpMgr,
		skillReg:       skillReg,
		dreamer:        dreamer,
		memoryStore:    memStore,
		hookConfigs:    hookConfigs,
		taskStore:      taskStore,
		evolution:      evolution,
		roleRegistry:   roleRegistry,
		// 会话身份声明落盘 (<state>/sessions/<id>.json), 见 session_spec.go。
		// 用 config 里的 StateDir/Cwd 推导, 与 Bot 侧 basedir 同一套规则。
		specs: newSpecStore(config.StateDir, config.Cwd),
	}

	for _, opt := range opts {
		if opt != nil {
			opt(sm)
		}
	}

	// TraceStore 轨迹底座 (design/03 §4.1 E1) 与会话历史快照共用 Bot 注入的那一个
	// StateStore 实例 —— 原先这里按 StateDir/Cwd 自行推导路径再 new 一个 FileStore,
	// 与 Bot 侧的实例是两把互不相干的锁 (见 WithStateStore 注释), 已删除。
	// 未注入时两者都禁用 (无声降级: 轨迹与续聊都是增强项, 不阻塞对话)。
	if sm.stateStore != nil {
		sm.traceStore = tracestore.New(sm.stateStore)
		// TTL 清理由 Bot 侧启动 (它知道 statestore 的落盘路径), 见 bot.go。
	}

	// 启动后台清理 goroutine
	go sm.cleanupLoop()

	return sm
}

// TraceStore 返回会话共享的轨迹底座 (可能为 nil)。
//
// 供 Bot 把同一个 Store 交给 ProductionTeamManager 写 gate Span
// (design/03 §4.1 第 5 种 Kind: 门禁跑在团队层, 会话引擎看不到它)。
// 刻意共享而不是让团队侧再 new 一个: Store 背后的 FileStore 锁表是 per-instance,
// 两个实例写同一个目录等于没有互斥 (见上面 stateStore 的注释)。
func (sm *SessionManager) TraceStore() *tracestore.Store {
	if sm == nil {
		return nil
	}
	return sm.traceStore
}

// stateRoot 池 bandit 的持久化状态根 (13.7-P1): 取 sm.evolution.StateDir()
// (= <state>, 与 rewards.jsonl/poolbandit.json/轨迹同根)。evolution 未注入
// (未配进化) 时返回空串, 调用方据此跳过 bandit 初始化。
func (sm *SessionManager) stateRoot() string {
	if sm == nil {
		return ""
	}
	return sm.evolution.StateDir()
}

// teamStageMaxTurnsFloor 是 agent 工具循环的硬下限兜底。
// 修复: 模型配置 maxTurns=0 经 `if mcfg.MaxTurns>0` 守卫被忽略后, 若 defaultResolved
// 也为 0, 会让 engine.go 把 0 当"无限", agent 一直循环到 stageTimeout(10min) 才停,
// 单个团队曾因此烧掉数百万 token。这里强制兜底, 任何 <=0 都收敛到该值。
const teamStageMaxTurnsFloor = 40

// clampTurns 保证 agent 回合数始终有界 (>0)。
func clampTurns(n int) int {
	if n <= 0 {
		return teamStageMaxTurnsFloor
	}
	return n
}

// resolveStageMaxTurns 走完整条链路: 判档 → 取上限 → 定优先级 (H4-3)。
//
// 把"判档"与"取上限"也收进来而不是留在 Execute 里, 是为了让**整条链**有一个能直接
// 喂输入的地方。分成两半的代价在 M99 那次变异里显形了: 只测 stageMaxTurns 的用例
// 在"SessionManager 根本没问解析器"时照样全绿 —— 而那一处的失效正是最典型的
// "配了没生效", 在运行读数上完全看不出来 (只是 stage 早停了几轮)。
func (r *sessionAgentRunner) resolveStageMaxTurns(modelTurns, baseTurns int) int {
	producesCode := agent.WorkflowGateMetaByName(r.runMeta.Workflow).ProducesCode
	class := modelconfig.ClassifyTask(modelconfig.TaskSignals{
		Role: firstNonEmpty(r.runMeta.Role, r.role), ProducesCode: producesCode,
	})
	return stageMaxTurns(r.hints.MaxTurns, modelTurns, baseTurns, r.sm.longHorizonMaxTurns(class))
}

// stageMaxTurns 按优先级决定一个团队 stage 的回合上限 (H4-3)。
//
// 抽成纯函数而不是留在 Execute 的 switch 里: 三档优先级各自有一个"写错了也不报错"
// 的失效形态 (写反了顺序 = 节点声明被忽略; longTurns 判据写成 >= = 配置写小了反而
// 缩小上限; 忘了 modelTurns = kimi 的 40 被档位顶掉), 而这些失效在**一次真实
// run 的读数上完全看不出来** —— 只表现为"某个 stage 早停了几轮"。所以要能直接喂
// 四个数进去断言输出。
//
//	baseTurns  调用方已算出的基线 (= 模型级 maxTurns 或全局 defaultResolved.MaxTurns)
//	hintTurns  图节点显式声明的回合数 (0 = 未声明)
//	modelTurns 模型级 maxTurns 原始值 (0 = 未声明)
//	longTurns  长程档解析出的值 (0 = 未开启分层)
func stageMaxTurns(hintTurns, modelTurns, baseTurns, longTurns int) int {
	// 1. 图节点显式声明 —— 最具体的声明, 赢。
	//    (此字段此前被捕获但从未被读过, 于是节点级回合数静默失效。)
	if hintTurns > 0 {
		return hintTurns
	}
	// 2. 模型级 maxTurns —— 按具体模型写死的值是有意为之 (kimi 系 40 就是因为
	//    那个模型回合开销与别人不同), 不该被按任务时程算出来的档位覆盖。
	if modelTurns > 0 {
		return modelTurns
	}
	// 3. 长程档 —— 只在上面两条都没声明时生效, 且判据是"严格更大"。
	//    >= 会让一个写小的配置把上限**缩小**, 而"配了长程档反而跑得更短"是
	//    最难归因的一种失效 (配置看起来是对的)。
	if longTurns > baseTurns {
		return longTurns
	}
	return baseTurns
}

// longHorizonMaxTurns 按任务时程档取回合上限 (H4-3)。
//
// 只有一种情况返回 0: 解析器未装配 (nil) —— 没有配置就没有档位, 也就没有放开的
// 依据。这个 0 不是"上限为零", 而是"本函数没有意见, 调用方保持原样", 由
// stageMaxTurns 的第一个 `longTurns > baseTurns` 判据消费。
//
// 装配好的解析器返回值**恒 ≥ 1** (见 modelconfig.GlobalConfig.MaxTurnsFor:
// 常规档在 DefaultMaxTurns<=0 时兜 25; 长程档只在 LongHorizonMaxTurns>0 时才
// 取它, 否则回落常规档), 所以这里不需要再挡一次非正值 —— 那会是一条任何读数
// 都观察不到的分支 (变异 M100: 加上它所有用例照样全绿, 因为它不可达)。
func (sm *SessionManager) longHorizonMaxTurns(class modelconfig.TaskClass) int {
	if sm == nil || sm.modelResolver == nil {
		return 0
	}
	return sm.modelResolver.MaxTurns(class)
}

// SetDefaultModelConfig 注入默认模型解析配置。
func (sm *SessionManager) SetDefaultModelConfig(cfg modelconfig.ResolvedConfig) {
	sm.defaultResolved = cfg
}

// SetModelResolver 注入模型解析器 (会话身份声明里的 modelAlias 靠它解析)。
// 与 SetDefaultModelConfig 成对, 在 Bot 装配期同一处调用。
func (sm *SessionManager) SetModelResolver(r *modelconfig.ConfigResolver) {
	sm.modelResolver = r
}

// clientForAlias 按别名构造一个会话专用客户端; 别名不可用/未注入解析器时返回 nil。
//
// 为什么返回 nil 而不是回落到默认客户端: 调用方的语义是"这个会话声明了模型 X",
// 悄悄跑成默认模型 X′ 会让"配了没生效"看起来与"配了生效了"完全一样 (本仓纪律不许
// 留这种假开关)。调用方拿到 nil 必须**记日志**再决定怎么走。
func (sm *SessionManager) clientForAlias(alias string) *api.Client {
	if sm.modelResolver == nil {
		return nil
	}
	rc := sm.modelResolver.ResolveAlias(alias)
	// 与 /advisor 同一套完整性判据 (bot.go:3510): 缺任一项就不是一个能用的端点。
	if rc.BaseURL == "" || rc.APIKey == "" || rc.ProviderName == "" {
		return nil
	}
	cli := api.NewClient(rc.BaseURL, rc.APIKey, rc.OutboundModelName())
	cli.Protocol = rc.Protocol
	cli.SetProxy(rc.Proxy)
	if rc.CallTimeoutSec > 0 {
		cli.CallTimeout = time.Duration(rc.CallTimeoutSec) * time.Second
	}
	if rc.FirstTokenTimeoutSec > 0 {
		cli.FirstTokenTimeout = time.Duration(rc.FirstTokenTimeoutSec) * time.Second
	}
	return cli
}

// SetMemoryIndexMode 把记忆注入形态推到**所有已存活会话**的引擎上。
//
// 存活的会话各自持有自己的 QueryEngine (createSession 里每会话一个), 配置热加载
// 若只改 Bot 的字段, 那些已经在跑的会话要等下次重建引擎才吃得到 —— 而会话可能
// 存活数小时。所以这里遍历推一遍。
//
// 加读锁即可: 目标是 engine 内部的 atomic.Bool (见 MemoryInjectHook.indexMode),
// 不修改 SessionManager 的任何字段, 也就不需要写锁。持锁期间调 Set 是安全的 ——
// 它不回调回 SessionManager, 不构成锁重入。
//
// 只影响各引擎的**下一次检索**: 正在跑的那一轮 t0 已经把 memory 块定死了,
// 后续轮次复用同一块 (刻意如此, 中途换形态会让前缀缓存当场全废)。
func (sm *SessionManager) SetMemoryIndexMode(on bool) {
	if sm == nil {
		return
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, s := range sm.sessions {
		if s != nil && s.Engine != nil {
			s.Engine.SetMemoryIndexMode(on)
		}
	}
}

// SetAdvisorClient 注入 advisor 模型客户端。在 Bot 初始化完成后调用；nil 表示禁用。
func (sm *SessionManager) SetAdvisorClient(client *api.Client) {
	sm.advisorMu.Lock()
	defer sm.advisorMu.Unlock()
	sm.advisorClient = client
	if client == nil {
		sm.advisorTools = nil
	}
}

// SetPlanClient 注入规划期独立模型客户端 (Tag=plan)。在 Bot 初始化完成后调用；
// nil 表示禁用 (规划期沿用主模型)。
func (sm *SessionManager) SetPlanClient(client *api.Client) {
	sm.planMu.Lock()
	defer sm.planMu.Unlock()
	sm.planClient = client
}

// AdvisorEnabled 返回 advisor 是否已启用。
func (sm *SessionManager) AdvisorEnabled() bool {
	sm.advisorMu.Lock()
	defer sm.advisorMu.Unlock()
	return sm.advisorClient != nil
}

// AdvisorModel 返回 advisor 模型名 (未启用时为空)。
func (sm *SessionManager) AdvisorModel() string {
	sm.advisorMu.Lock()
	defer sm.advisorMu.Unlock()
	if sm.advisorClient == nil {
		return ""
	}
	return sm.advisorClient.Model
}

// advisorOptionsFromConfig 从 BotConfig.Advisor 段构造工具护栏配置。
func (sm *SessionManager) advisorOptionsFromConfig() builtin.AdvisorOptions {
	opts := builtin.AdvisorOptions{}
	if sm.config != nil && sm.config.Advisor != nil {
		opts.MaxCallsPerSession = sm.config.Advisor.MaxCallsPerSession
		opts.CooldownTurns = sm.config.Advisor.CooldownTurns
		opts.MaxTranscriptTokens = sm.config.Advisor.MaxTranscriptTokens
		opts.MaxOutputTokens = sm.config.Advisor.MaxOutputTokens
	}
	return opts
}

// advisorToolFor 返回 chatID 对应的 advisor 工具实例 (惰性创建)。
// 实例按会话缓存: 预算/冷却状态在 profile 切换重建 registry 时保持。
// advisor 未启用时返回 nil。
func (sm *SessionManager) advisorToolFor(chatID string) *builtin.AdvisorTool {
	sm.advisorMu.Lock()
	defer sm.advisorMu.Unlock()
	if sm.advisorClient == nil || chatID == "" {
		return nil
	}
	if sm.advisorTools == nil {
		sm.advisorTools = make(map[string]*builtin.AdvisorTool)
	}
	if t, ok := sm.advisorTools[chatID]; ok {
		return t
	}
	t := builtin.NewAdvisorTool(sm.advisorClient, sm.advisorOptionsFromConfig())
	sm.advisorTools[chatID] = t
	return t
}

// SetMediaSendFn 注入飞书媒体发送回调。在 Bot 初始化完成后调用。
func (sm *SessionManager) SetMediaSendFn(fn MediaSendFunc) {
	sm.mediaSendFn = fn
}

// complexityLLMClassifier 构造 LLM 复杂度分类器 (llm/hybrid 模式用)。
// 用主 apiClient.SimpleComplete + pkg/complexity 的分类器 system prompt,
// 失败时回落启发式判定 (由 complexity.Judge 内部处理)。
func complexityLLMClassifier(client *api.Client) func(ctx context.Context, text string) (bool, string, error) {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, text string) (bool, string, error) {
		out, err := client.SimpleComplete(ctx, complexity.ClassifierSystemPrompt, text)
		if err != nil {
			return false, "", err
		}
		// 分类器只回 COMPLEX / SIMPLE (+ reason), 保守判定: 含 COMPLEX 即复杂。
		return strings.Contains(out, "COMPLEX"), strings.TrimSpace(out), nil
	}
}

// SetTeamManager 注入团队管理器，让 LLM 能通过 TeamQuery 工具查询团队信息。
func (sm *SessionManager) SetTeamManager(mgr *agent.ProductionTeamManager) {
	sm.teamMgr = mgr
}

// SetSearcher 注入 Web 搜索适配器，让 WebSearchTool 能发起真实搜索。
func (sm *SessionManager) SetSearcher(s builtin.WebSearcher) {
	sm.searcher = s
}

// SetCwd 切换工作目录并丢弃所有内存会话 (工具的执行根变了, 旧引擎的工具注册表已过期)。
// 同 evictOldest/cleanup: 只卸内存, 不删持久化快照 —— 调用方 (/cwd 命令) 会另外对
// 当前 chat 调 ClearSession 表达"这个会话清空", 其余 chat 的历史必须留着。
func (sm *SessionManager) SetCwd(cwd string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.config.Cwd = cwd
	// 换 cwd 只换 project 那一档, state 档不变 —— 与启动时的来源对保持一致
	// (21.8.12 W1)。这里若退回 NewRoleRegistry(cwd), 一次 /cwd 就会把注册表
	// 悄悄降级成"只看项目目录"的那张, 而校验侧 (dashboard) 看的仍是两档俱全的
	// 那一张: 同一条工作流在 /cwd 前后表现不同, 且没有任何读数指得出原因。
	sm.roleRegistry = agent.NewRoleRegistryFor(cwd, sm.config.StateDir)
	sm.sessions = make(map[string]*Session)
}

func contextWindowOrDefault(v int) int {
	if v > 0 {
		return v
	}
	return 200000
}

type registryOptions struct {
	chatID             string
	includeFeishuTools bool
	includeTeamQuery   bool
	includeAgent       bool
	runAgentFn         agent.RunAgentFunc
}

// todoRosterSource 从注册表里找出 TodoWrite 工具, 包成引擎侧要的清单来源。
//
// 包一层的必要性: 引擎 import 不了 pkg/tool/builtin (builtin 经 evotools →
// evolution/govern 与引擎侧成环), 所以两边只能靠函数签名对接。
//
// **从注册表按名字查, 而不是把工具句柄一路传下来**: 各档位 (chat / research /
// admin) 注册 todo 工具的位置不同, 有的档位根本没有 (chat profile 就没有)。
// 从注册表查是唯一一处能同时覆盖"有"和"没有"两种情况的写法 —— 传句柄要么在
// 中间每一层加参数, 要么在某条分支上漏传而静默失去清单。
//
// 查不到时返回 nil: 引擎侧 nil 即不注入, 行为与改造前逐字节相同。
func todoRosterSource(reg *tool.Registry) func() []internal_hook.TaskRosterItem {
	if reg == nil {
		return nil
	}
	t, ok := reg.Get(builtin.TodoWriteToolName)
	if !ok {
		return nil
	}
	todoTool, ok := t.(*builtin.TodoWriteTool)
	if !ok {
		return nil
	}
	// 清单为空时这个函数返回空切片, 引擎侧据此跳过注入 (renderTaskRoster 对空
	// 清单返回空串)。之所以做成每次调用都现取, 而不是构造期取一次: 清单是活的,
	// 压缩随时可能发生。
	return func() []internal_hook.TaskRosterItem {
		snaps := todoTool.Snapshot()
		out := make([]internal_hook.TaskRosterItem, 0, len(snaps))
		for _, s := range snaps {
			out = append(out, internal_hook.TaskRosterItem{ID: s.ID, Content: s.Content, Status: s.Status})
		}
		return out
	}
}

func (sm *SessionManager) newProfileRegistry(profile builtin.ToolProfile, opts registryOptions) *tool.Registry {
	profile = builtin.NormalizeToolProfile(profile)
	reg := tool.NewRegistry()
	builtin.RegisterProfileToolsWithStore(reg, sm.taskStore, sm.searcher, profile)

	// 记忆按需取回两件套 (memory_search / memory_read)。与 CLI 侧同一处收口:
	// 记忆库是**进程级**共享的 (sm.memoryStore), 每次 profile 装配都重注入同一句柄,
	// 所以各档位/嵌套/stage 会话看到的都是同一批记忆 —— 这正是我们要的 (团队产出
	// 写进 L1 之后, 后续任何会话都该查得到)。
	//
	// 在 profile 工具之后、治理之前注册: 与 builtin 基础工具同级, 于是 DisabledTools
	// / AllowedTools 治理集对它们同样生效 (受限档位可以按需关掉)。
	builtin.RegisterMemoryTools(reg, sm.memoryStore)

	if opts.includeFeishuTools && opts.chatID != "" && sm.mediaSendFn != nil {
		reg.Register(NewFeishuSendFileTool(opts.chatID, sm.mediaSendFn))
	}
	if opts.includeTeamQuery && sm.teamMgr != nil {
		reg.Register(NewTeamQueryTool(sm.teamMgr))
	}
	if sm.mcpMgr != nil {
		if profile == builtin.ToolProfileAdmin {
			sm.mcpMgr.RefreshToolsForRegistry(reg)
		} else {
			registerMCPProxyTools(reg, sm.mcpMgr)
		}
	}
	if sm.skillReg != nil && sm.skillReg.Count() > 0 {
		reg.Register(skills.NewSkillTool(sm.skillReg))
	}
	// 13.7-P0 统一池 (§13.7.2): env 开关默认关; 开启后自训练/动态资产下沉池内,
	// 广告面=基础+包装三件+已加载。worker/k8s 路径的引擎构建全在 NewBot→这里,
	// 接线点选注册末尾保证覆盖全部 profile 与嵌套/stage 会话面。
	if tool.PoolEnabled(os.Getenv) {
		// 治理集: 会话级 DisabledTools/AllowedTools 在 engine.Config 里, 池侧
		// 拷贝同名集 (受限档位 profile 已把工具排除在注册表外, 治理集为空=全放行)。
		// 13.7-P1 选择策略: bandit 持久化自状态根 (sm.evolution.StateDir() =
		// <state>, 与 rewards.jsonl/轨迹同根, 冷启动回灌直接扫这两处);
		// traceStore 直传 (装配时已就绪, 见 NewSessionManager), 留痕即刻生效。
		// sm.evolution 为 nil (未配进化) 时无状态根, bandit 不初始化 —— 留痕
		// (sm.traceStore) 与排序增强仍按各自的 nil 安全语义独立降级。
		pp := builtin.NewPoolPolicy(sm.stateRoot(), sm.traceStore)
		pool := builtin.RegisterPoolTools(reg, sm.skillReg, sm.AssetSet(), nil, nil, pp)
		// 13.7-P2 L1 配给: 描述位按 bandit 后验配给 (skillReg 是进程级共享注册表,
		// 每次 profile 装配都重注入, 以最后一次装配的 policy 为准)。
		sm.skillReg.SetRanker(pp.RankScore)
		if sm.config != nil && sm.config.Debug {
			log.Printf("[toolpool] profile=%s 池已启用: %d 项下沉", profile, len(pool.MemberNames()))
		}
	} else if sm.skillReg != nil {
		// 未开池: 清掉可能残留的 ranker (上一会话 profile 开过池), 回名称序。
		sm.skillReg.SetRanker(nil)
	}
	if opts.includeAgent && opts.runAgentFn != nil {
		// 带轨迹底座 (design/01 §4.8 图外派生可见性): 每次 Agent 工具派生写一条
		// KindNode/subagent Span。sm.traceStore 为 nil 时与 NewAgentTool 完全等价。
		reg.Register(agent.NewAgentToolWithTrace(opts.runAgentFn, sm.traceStore))
	}
	// Advisor 顾问工具: 仅主会话 (chatID 非空) 注册; 嵌套 agent 不注册避免预算翻倍。
	if advTool := sm.advisorToolFor(opts.chatID); advTool != nil {
		reg.Register(advTool)
	}
	return reg
}

func shortSkillListing(reg *skills.Registry) string {
	return shortSkillListingInDir(reg, "")
}

// shortSkillListingInDir 带工作目录的变体: paths 声明命中的技能才进清单
// (第七章缺口修复: SKILL.md paths 从死字段变为运行期可见性条件)。
func shortSkillListingInDir(reg *skills.Registry, dir string) string {
	if reg == nil || reg.Count() == 0 {
		return ""
	}
	// 0 = 不设软上限, 由描述字符预算约束; 所有技能名称始终可见 (可发现性)。
	return reg.FormatShortListingForDir(0, dir)
}

// interactiveOnlyTools 在团队 stage 里**结构性不可能用上**的工具。
//
// 判据是"能不能用", 不是"用得少":
//   - AskUserQuestion: 工具自己的描述就写着 "In non-interactive mode the tool cannot
//     run", 而团队 stage 的引擎配置恒为 IsNonInteractive: true —— 暴露它等于摆一个
//     必然失败的动作。
//   - EnterPlanMode/ExitPlanMode: 计划模式的生命线是 cfg.DynamicPlanCheck 与
//     PlanFileDir, 而这两个只在主会话路径 (createSessionWithClient) 里赋值, 团队
//     stage 的 cfg 里是零值 —— 进了计划模式也既无状态也无落盘出口。
//
// 两条都有 8516 次工具调用的数据背书: 三者合计 **0 次**被调用。
// 每个省下的 schema 都会在**每一轮**重发, 所以这属于纯赚的减法。
func dropInteractiveOnlyTools(reg *tool.Registry) {
	if reg == nil {
		return
	}
	for _, name := range []string{
		builtin.AskUserQuestionToolName,
		builtin.EnterPlanModeToolName,
		builtin.ExitPlanModeToolName,
	} {
		reg.Remove(name)
	}
}

// roleScopedSkillListing 只列**该角色声明过或推荐过的**技能 (团队 stage 用)。
//
// 为什么不能列全量: 内置技能库 (pkg/skills/builtin, 28 个 go:embed 的 SKILL.md) 是
// 跨项目沉淀的, 全局清单会把 Django / C++ / C# / Dart / 并发审查 / 品牌规范 之类
// 与当前任务毫无关系的条目塞进**每一次**请求。实测某个出题 stage 的 system[0] 里
// <available_skills> 占 3133 字节 / 35 条, 而 Skill 工具在全部 8516 次工具调用里
// **0 次**被使用 —— 花了成本, 一次都没换来收益。
//
// 这不是"砍掉能力": 没列出的技能仍在统一池里 (RegisterPoolTools 把
// ModelVisibleActive 全量登记为池成员), 模型用 pool_search 按关键词就能找回来,
// 再 pool_load 展开正文。池对技能这一路此前形同虚设, 恰恰因为清单本就全量可见 ——
// 既然什么都在眼前, 就没有"检索"这一步。
//
// 角色未声明任何技能时不再倒整个技能库, 只留一句"其余在池里"的指路 (见下)。
func (sm *SessionManager) roleScopedSkillListing(role string) string {
	if sm.skillReg == nil || sm.skillReg.Count() == 0 {
		return ""
	}
	if sm.roleRegistry == nil {
		// 没有角色注册表就没有"角色相关"这个判据, 此时保持旧行为 (全量短清单),
		// 不要因为缺一个可选项就把技能面整段抹掉。
		return shortSkillListingInDir(sm.skillReg, sm.config.Cwd)
	}
	out := sm.skillReg.FormatShortListingForNames(sm.roleRegistry.RoleSkills(role))
	if out != "" {
		return out
	}
	return poolPointerListing()
}

// stageSkillListing 在角色专属技能之外并入**阶段声明的技能名** (21.8.12 P4 / X1)。
//
// 声明是**加项不是替换**: 角色技能来自这个 role 是谁, 阶段技能来自这一步要做什么 ——
// 两者无可比性, 任何一方的覆盖都会静默剥掉另一方的能力面。出问题时宁可多一个技能
// 在清单里 (模型还要 pool_load 才拿到正文), 也不要少一个而没人发现。
//
// 未注册的名字**点名报告**: 一个拼错的技能名与"压根没声明"在注入面上完全一样
// (两者都没有它), 只有这条日志能把它们分开 —— 与 profileForTeamRole 那条
// [deprecated] 痕迹同一口径。区分"没注册"与"注册了但不可注入"(shadow/archived)
// 是有意的: 后者是运维已知的状态, 前者一定是打错了字。
func (sm *SessionManager) stageSkillListing(role string, extra []string) string {
	base := sm.roleScopedSkillListing(role)
	if len(extra) == 0 || sm.skillReg == nil || sm.skillReg.Count() == 0 {
		return base
	}
	var unknown []string
	for _, n := range extra {
		if _, known := sm.skillReg.GetAny(n); !known {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		log.Printf("[Skills] 阶段 (角色 %s) 声明的技能未在注册表里, 这部分声明不生效: %s",
			role, strings.Join(unknown, ", "))
	}
	listed := sm.skillReg.FormatShortListingForNames(extra)
	if listed == "" {
		return base
	}
	if base == "" {
		return listed
	}
	return base + "\n" + listed
}

// poolPointerListing 在清单被裁空时给一句指路。
//
// 为什么必须有: 裁剪把技能藏起来之后, 模型**不知道自己不知道什么** —— 眼前没有的
// 东西它不会去找。实测某次增量出题 run 的 4 次工具调用全是 Read/Write/Bash, 一次
// pool_search 都没有: 不是池坏了, 是没有任何线索提示"还有可检索的东西"。
// 池开着时才有意义 (没开池就真的没得搜), 所以按 CLAUDE_GO_TOOLS_POOL 判。
//
// 成本 ~110 字节, 对比原来全量清单的 3133 字节可以忽略。
func poolPointerListing() string {
	if !tool.PoolEnabled(os.Getenv) {
		return ""
	}
	return "<available_skills summary=\"short\">\n" +
		"（本角色没有专属技能；其余技能与已下沉的工具都在统一池中，" +
		"可用 pool_search 按关键词检索，命中后再 pool_load 展开完整说明。）\n" +
		"</available_skills>\n"
}

func (sm *SessionManager) configureSessionTools(session *Session, profile builtin.ToolProfile) {
	profile = builtin.NormalizeToolProfile(profile)
	if session.ToolProfile == profile && session.Engine != nil && session.Engine.Tools != nil {
		return
	}
	var runAgentFn agent.RunAgentFunc
	runAgentFn = func(ctx context.Context, agentPrompt string, opts agent.RunOptions) (string, error) {
		return sm.runNestedAgent(ctx, runAgentFn, agentPrompt, opts)
	}
	session.Engine.Tools = sm.newProfileRegistry(profile, registryOptions{
		chatID:             session.ChatID,
		includeFeishuTools: true,
		includeTeamQuery:   true,
		includeAgent:       true,
		runAgentFn:         runAgentFn,
	})
	session.ToolProfile = profile
}

func inferFeishuToolProfile(text string) builtin.ToolProfile {
	lower := strings.ToLower(text)
	if strings.HasPrefix(strings.TrimSpace(lower), "/") {
		return builtin.ToolProfileAdmin
	}
	codingHints := []string{
		"claude-go", "golang", " go ", ".go", "代码", "开发", "实现", "重构", "测试", "bug",
		"仓库", "项目", "架构", "编译", "修复", "todo 应用", "api", "backend",
	}
	padded := " " + lower + " "
	for _, hint := range codingHints {
		if strings.Contains(padded, hint) || strings.Contains(lower, hint) {
			return builtin.ToolProfileCoding
		}
	}
	researchHints := []string{"分析", "调研", "搜索", "查找", "资料", "网页", "web", "最新", "新闻"}
	for _, hint := range researchHints {
		if strings.Contains(lower, hint) {
			return builtin.ToolProfileResearch
		}
	}
	return builtin.ToolProfileChat
}

func profileForRunOptions(opts agent.RunOptions, meta agent.RunMetadata) builtin.ToolProfile {
	role := strings.ToLower(firstNonEmpty(opts.SubagentType, meta.Role, meta.Purpose))
	if opts.ReadOnly || strings.Contains(role, "research") || strings.Contains(role, "review") || strings.Contains(role, "plan") {
		return builtin.ToolProfileResearch
	}
	if strings.Contains(role, "coder") || strings.Contains(role, "tester") || strings.Contains(role, "architect") ||
		strings.Contains(role, "implement") || strings.Contains(role, "build") {
		return builtin.ToolProfileCoding
	}
	return builtin.ToolProfileChat
}

// profileForTeamRole 按**角色名子串**猜工具档位 —— 21.8.12 P3/W5 的待退役项。
//
// 它现在只作为最后回退 (ConstraintSet.ResolveToolProfile 的 fallback, 来源标记
// ProfileSourceRoleNameFallback), 且用了就打一条 [deprecated] 日志。取代它的是
// StageDef.Tools 的显式声明 (经 ApplyStageTools ⇒ ProfileSourceExplicit), 声明一出现
// 这个函数就不会被调用。
//
// 之所以**留着而不是删掉**: 6 个下游平台的既有工作流一个字节都没改, 它们不声明任何
// 东西 ⇒ 走的还是这条路。删掉等于替生产改行为, 而那是本次改造明确排除的
// (design/01 §4.6「一刀切删掉等于替生产改行为」)。
//
// 两处已知误判, 是这条推断必须退役的实际理由 (不是理论洁癖):
//
//	world-builder    含 "build" ⇒ coding 档 ⇒ 拿到 Shell。一个写世界观设定的角色
//	                 因此能执行任意命令。
//	tech-investigator 含 "investigat" 但既不含 research 也不含 review ⇒ 落到 team 档,
//	                 而它是一个只读调研角色 —— 缺的是联网能力。
func profileForTeamRole(role, workflow string) builtin.ToolProfile {
	role = strings.ToLower(role)
	workflow = strings.ToLower(workflow)
	if workflow == "development" && (strings.Contains(role, "research") ||
		strings.Contains(role, "review") ||
		strings.Contains(role, "planner") ||
		strings.Contains(role, "architect")) {
		return builtin.ToolProfileTeam
	}
	// 写代码类 → Coding (含写/编辑/Bash/建索引)
	if strings.Contains(role, "coder") || strings.Contains(role, "tester") || strings.Contains(role, "architect") ||
		strings.Contains(role, "implement") || strings.Contains(role, "build") {
		return builtin.ToolProfileCoding
	}
	// 只读分析/调研/审阅类 → Analysis (精简只读: Read/Grep/Glob + CodeIntel只读 + Web)。
	// 既给 tech-investigator 联网调研能力 (解决与 source-analyst 重复读码), 又裁掉重型工具 schema 每轮重发。
	if strings.Contains(role, "research") || strings.Contains(role, "review") || strings.Contains(role, "planner") ||
		strings.Contains(role, "investigat") || strings.Contains(role, "analyst") ||
		strings.Contains(role, "critic") || strings.Contains(role, "fact") {
		return builtin.ToolProfileAnalysis
	}
	return builtin.ToolProfileTeam
}

// ==================== 约束单一真源接线 (design/01 §4.6) ====================
//
// 上面三个 profileForXxx / inferFeishuToolProfile 是**启发式**, 各有各的输入
// (角色名 / RunOptions / 用户文本)。此前它们各自直接决定档位, 没有一个地方能回答
// "这个 agent 的约束是什么、从哪来、能不能放宽"。现在它们退化为
// agent.ConstraintSet.ResolveToolProfile 的 fallback: 显式声明优先, 没有声明才回退,
// 且回退会留痕。**启发式的实现仍只有这一份**, 一行都没有复制到 pkg/agent —— 复制
// 一份就等于造出第二个真源, 正是 §4.6 要消除的问题。

// 编译期断言: pkg/agent.ConstraintSet 结构性满足 engine 的最小约束接口。
// 放在 feishu (全仓唯一同时 import 两者的包) 让契约破裂在编译期暴露, 而不是等到
// 运行时某个调用点才发现装配不上。
var _ engine.ToolConstraintSource = (*agent.ConstraintSet)(nil)

// feishuSessionConstraints 飞书主会话的工具约束。
//
// 语义与改造前 engine.Config.DisabledTools 的 map 字面量**逐项等价**: 飞书会话中
// 禁止 LLM 自主调用团队内部工具, 团队操作只能通过 /team、/go 命令触发。
// TeamCreate/TeamDelete 是团队编排工具, 在飞书对话中无意义, 且会被 LLM 误用。
// (TeamMailbox 已于 2026-09-25 删除 —— 它是个零读者的假投递工具, 不再存在于任何工具池,
//
//	继续留在黑名单里会让"这工具被拦住了"成为一条查无实据的断言。见 21.8.9-D6。)
//
// 区别只在于: 名单现在声明在 ConstraintSet 里 (可审计、可被子约束继续收窄),
// 执行点仍然是 engine 的 toolExposed。
func feishuSessionConstraints() *agent.ConstraintSet {
	return agent.NewConstraintSet("feishu-session").
		WithDeniedTools("TeamCreate", "TeamDelete")
}

// roleInferWarned 记录已经提醒过的 (来源, 档位) 组合。
// 高频阶段会反复建 agent, 不去重会刷爆日志。
var (
	roleInferWarned      sync.Map
	roleInferWarnedCount atomic.Int64
)

// roleInferWarnCap 去重表的容量上限。
// 为什么需要上限: 嵌套 agent 的来源含 opts.SubagentType, 那是**模型可控的任意字符串**;
// 无上限的去重表会随模型胡编的 subagent 名无界增长 (慢性内存泄漏)。到顶后停止提醒 ——
// 到那时该收集的信号早已收齐, 而这条日志只是观测手段, 丢掉不影响任何判定。
const roleInferWarnCap = 512

// warnRoleNameInference 在真的用了"角色名子串推断"时留下可观测痕迹。
// design/01 §4.6 要求 profileForTeamRole 仅作缺省回退并打 deprecation 日志 ——
// 这条日志就是将来退役它的依据: 线上还有哪些角色在依赖猜测, 一目了然。
func warnRoleNameInference(origin string, prof builtin.ToolProfile) {
	key := origin + "|" + string(prof)
	if _, dup := roleInferWarned.Load(key); dup {
		return
	}
	if roleInferWarnedCount.Load() >= roleInferWarnCap {
		return
	}
	if _, dup := roleInferWarned.LoadOrStore(key, struct{}{}); dup {
		return
	}
	roleInferWarnedCount.Add(1)
	log.Printf("[Constraint][deprecated] 工具档位由角色名子串推断得出 (%s → profile=%s); "+
		"design/01 §4.6 要求显式声明 tool_profile, 该推断将退役", origin, prof)
}

// stageConstraints 组装一个团队阶段生效的工具约束集 (21.8.12 P3 / W5)。
//
// 抽成函数而不是留在 Execute 里, 与 stageMaxTurns 是同一条理由: 三条优先级各自有
// 一个"写错了也不报错"的失效形态, 而它们在**一次真实 run 的读数上完全看不出来**
// (都只表现为一组可见工具)。要能直接喂输入断言输出, 就得有个纯函数。
//
// 优先级:
//
//  1. 图覆盖表 (hints.ToolProfile) —— 运维/灰度期的最后一跳, 压得住工作流定义里的
//     声明。与 mergeStageOverride 的"覆盖表优先"是同一条规则, 只是那次合并在
//     直译期做 (ToolProfile), 这次的 Tools 名单在运行期才落到 ConstraintSet 上。
//  2. 阶段自己的显式声明 (StageDef.Tools, 经 ctx 下发) —— 定义者的意图。
//  3. 角色名子串猜测 —— 仅在 1、2 都没有时由调用方用作 fallback 闭包 (不在本函数里)。
//
// ★ 覆盖表与阶段声明**只在档位这一维**冲突 (图路径下两者描述的是同一个 stage, 图节点
// 正是由 StageDef 直译 + 合并而来); 名单与黑名单是另外两维, 各自只会收窄, 没有冲突,
// 所以无条件应用 —— 若把这三项一起"覆盖表优先", 结果是一个只想改档位的覆盖表会
// 顺手把定义里那份白名单**整份丢掉**, 而丢掉白名单等于放宽。
//
// 顺序写在这里而不是让两条来源共用一个字段, 是为了让"谁赢"读得出来: 挤进同一个键
// 之后结果取决于哪个先写, 而那个顺序在以后任何一次重构里都可能被无声地改掉,
// 表现恰好是"覆盖表里写了不生效"。
func stageConstraints(ctx context.Context, hints agent.NodeExecHints, stage, teamRole string) *agent.ConstraintSet {
	constraints := agent.NewConstraintSet("team-role:" + teamRole)

	overrideDeclaredProfile := strings.TrimSpace(hints.ToolProfile) != ""
	if overrideDeclaredProfile {
		constraints.WithToolProfile(string(mapExplicitToolProfile(hints.ToolProfile)))
	}
	if stageTools := agent.StageToolsFromContext(ctx); stageTools != nil {
		toApply := stageTools
		if overrideDeclaredProfile {
			// 覆盖表压住定义的档位; 名单/黑名单照常应用 (见函数头注释)。
			toApply = &agent.StageTools{Allow: stageTools.Allow, Deny: stageTools.Deny}
		}
		if applied := agent.ApplyStageTools(constraints, toApply); len(applied) > 0 {
			// 留痕而不是静默应用: "这个 stage 的档位是声明的还是猜的" 在运行读数上
			// 完全看不出来 (两者都表现为一组可见工具), 只有这条日志能把它们分开。
			// 与 warnRoleNameInference 那条 [deprecated] 互为对照: 有这条就没有那条。
			log.Printf("[Constraint] 阶段 %q (角色 %s) 应用了显式工具声明: %s",
				stage, teamRole, strings.Join(applied, " "))
		}
	}
	return constraints
}

// resolveToolProfile 飞书侧**唯一**的工具档位决策入口。
// 优先级: ConstraintSet 里的显式声明 > fallback 启发式。用了角色名推断就打一次
// deprecation 日志 (按来源去重)。返回值经 NormalizeToolProfile 兜底, 与改造前一致。
func resolveToolProfile(cs *agent.ConstraintSet, fallbackSource agent.ProfileSource, fallback func() builtin.ToolProfile) builtin.ToolProfile {
	name, src := cs.ResolveToolProfile(fallbackSource, func() string {
		return string(builtin.NormalizeToolProfile(fallback()))
	})
	prof := builtin.NormalizeToolProfile(builtin.ToolProfile(name))
	if src == agent.ProfileSourceRoleNameFallback {
		warnRoleNameInference(cs.ConstraintOrigin(), prof)
	}
	return prof
}

// nestedExplicitProfile 判定嵌套 agent 能否采用外层图节点的显式档位声明;
// 返回 "" 表示不采用, 由调用方回退到启发式结果。
//
// 为什么不能直接采用: 嵌套 agent 的档位来自本次派生的意图 (opts.ReadOnly /
// SubagentType), 节点声明可能比它**更宽** —— 例如节点声明 coding, 而这次派生带了
// opts.ReadOnly=true (原本得 research 档), 直接覆盖就是放宽, 违反 §4.6 的约束单调性,
// 后果是一个只读子代理拿到了 Shell。
// 因此只在声明确实是收窄时采用; 声明更宽、或两个档位不可比 (工具集互不包含) 时
// 一律不采用 —— 保持回退结果, 也就是改造前的行为 (fail-closed)。
func nestedExplicitProfile(declared, fallback builtin.ToolProfile) builtin.ToolProfile {
	if declared == "" {
		return ""
	}
	if agent.ProfileNarrows(string(declared), string(builtin.NormalizeToolProfile(fallback))) {
		return declared
	}
	return ""
}

// resolveSessionToolProfile 主会话 (人在飞书里直接对话) 的档位决策。
// 用户不会声明档位, 所以恒走文本启发式; 走同一入口是为了让**所有**档位决策只有
// 一个地方, 且来源可审计。文本启发式不是角色名推断, 不打 deprecation 日志。
func resolveSessionToolProfile(userText string) builtin.ToolProfile {
	return resolveToolProfile(
		agent.NewConstraintSet("feishu-chat"),
		agent.ProfileSourceTextHeuristic,
		func() builtin.ToolProfile { return inferFeishuToolProfile(userText) },
	)
}

// ==================== P5: subagent_type 的资产查表 (21.8.12) ====================
//
// 改造前, 模型在 Agent 工具里填的 subagent_type 只被当作一个**待猜测的字符串**:
// profileForRunOptions 拿它 lower + strings.Contains 一遍, 猜出工具档位。
// 于是一份真实存在的 agents/<name>.md 声明**存在也读不到** —— 它声明的 tool-profile
// 与正文提示词都不参与任何决策, 而"我写了这份 agent 定义怎么没生效"从一个外部视角
// 完全成立 (目录在、文件在、没有任何报错)。
//
// 这里补的是查表那一半。清单那一半在 pkg/agent.AgentManifest (喂进 Agent 工具的
// Description)。两者必须同时在位: 只有清单 ⇒ 模型按清单填了名字而没人按它查表,
// 那份清单变成一串"说了不生效"的承诺; 只有查表 ⇒ 模型猜不出名字, 绝大多数派生
// 走的还是猜的那条路。判定这两件事**接上了**的测试直接驱动下面的入口本身
// (p5_agent_lookup_test.go), 而不是另写一份平行逻辑自己测自己。

// bindAgentAsset 按 subagent_type 装配出这份 agent 的全部声明 (21.8.12 P2)。
//
// 返回 nil 表示"没有可用声明" (没资产体系 / 没这个名字 / 非 active) —— 调用方照旧
// 走角色名猜测。**非 active 也返回 nil**: 一份 shadow 资产被模型直接调用等于绕过
// 整个灰度机制, 而 P5 的清单本就把它排除在外, 两侧必须一致。
//
// 返回值**永远不为空壳**: 只要名字查到了, 读数 (Bindings) 就在里面, 即使四条声明
// 全都没写。降级 (非法档位 / 查不到的技能 / 没有消费方的 plugin) 一律走
// bind.Warnings(), 调用方负责印出来 —— 组装器自己不打日志: 它同时被 CLI 与飞书
// 两条路径调用, 而两条路径的日志去处不同。
func (sm *SessionManager) bindAgentAsset(subagentType string) *agentbind.AssetBinding {
	if strings.TrimSpace(subagentType) == "" {
		return nil
	}
	return agentbind.BindAgentAsset(subagentType, sm.skillReg)
}

// logAgentBindingWarnings 把一份装配里的降级项印进日志。
//
// 只在**有**降级项时打印, 且每条按资产名去重 (subagent_type 是模型可控的字符串,
// 一份坏资产可能被反复派生) —— 与 warnUnknownSubagentType 同一个理由。
func logAgentBindingWarnings(b *agentbind.AssetBinding) {
	if b == nil {
		return
	}
	for _, w := range b.Warnings() {
		key := "agent-binding|" + b.Name + "|" + w
		if _, dup := roleInferWarned.LoadOrStore(key, struct{}{}); dup {
			continue
		}
		if roleInferWarnedCount.Load() >= roleInferWarnCap {
			return
		}
		roleInferWarnedCount.Add(1)
		log.Printf("[P2][warn] %s", w)
	}
}

// warnUnknownSubagentType 在模型填了一个查不到的名字时留痕。
//
// 这条日志是 P3 那条待退役猜测的**依据**: 如果模型填的名字长期都是查不到的,
// 说明该补的是清单 (模型不知道有哪些名字), 不是继续加固猜测。
// 按名字去重并设上限 —— subagent_type 是模型可控的任意字符串, 无上限的去重表
// 会随它胡编的名字无界增长 (与 roleInferWarned 同一个理由)。
func warnUnknownSubagentType(name, origin string) {
	name = strings.TrimSpace(name)
	if name == "" || agent.AgentCatalog() == nil {
		return // 没有资产体系时这不是异常, 而是当前常态, 打出来只会淹掉日志
	}
	key := "subagent-type|" + name
	if _, dup := roleInferWarned.Load(key); dup {
		return
	}
	if roleInferWarnedCount.Load() >= roleInferWarnCap {
		return
	}
	if _, dup := roleInferWarned.LoadOrStore(key, struct{}{}); dup {
		return
	}
	roleInferWarnedCount.Add(1)
	log.Printf("[P5][warn] subagent_type=%q (%s) 在 agent 资产表里查不到, 已回退到角色名子串猜测;"+
		" 清单里能填的名字见 Agent 工具的 Available agent types", name, origin)
}

// applyConstraints 把约束装配进引擎配置。约束按构造只会收窄, 返回错误意味着声明里
// 有放宽意图并已被丢弃 —— 记日志, 保持更严的现状 (fail-closed), 不回滚也不放行。
func applyConstraints(cfg *engine.Config, cs *agent.ConstraintSet) {
	if err := cfg.ApplyConstraints(cs); err != nil {
		log.Printf("[Constraint] 约束装配拒绝了放宽声明, 已保持更严现状: %v", err)
	}
}

// Get 获取已有会话（不创建）。如果不存在返回 nil。
func (sm *SessionManager) Get(chatID string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[chatID]
}

// GetOrCreate 获取或创建会话。
// 如果 chat_id 已存在会话则返回；否则创建新的 QueryEngine 会话。
// 当会话数达到上限时，淘汰最早活跃的会话 (LRU 策略)。
func (sm *SessionManager) GetOrCreate(chatID string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if s, ok := sm.sessions[chatID]; ok {
		s.Touch()
		return s
	}

	// 淘汰策略: LRU (Least Recently Used)
	if len(sm.sessions) >= sm.maxSessions {
		sm.evictOldest()
	}

	s := sm.createSession(chatID)
	sm.sessions[chatID] = s
	return s
}

// createSession 创建新会话 (内部方法, 需在锁内调用)。
// 对应 TS: REPL.tsx 中 getToolUseContext → assembleToolPool(permCtx, mcp.tools)
//
// 为每个会话创建独立的:
//   - tool.Registry (工具注册表) — 包含内置工具 + MCP 工具 + Agent 工具
//   - permissions.Checker (权限检查器)
//   - hooks.Runner (Hook 运行器，加载配置中的 hooks)
//   - compact.Compactor (上下文压缩器)
//   - prompt.Manager (提示词管理器)
//   - engine.QueryEngine (查询引擎)
//
// MCP 工具注册流程 (对应 TS: assembleToolPool):
//  1. 注册内置工具 (RegisterBaseTools)
//  2. 从共享的 MCP 连接中获取工具列表 (RegisterMCPTools)
//  3. 注册 Agent 工具 (支持嵌套 queryLoop)
func (sm *SessionManager) createSession(chatID string) *Session {
	return sm.createSessionWithClient(chatID, sm.apiClient)
}

// createSessionWithClient 同 createSession, 但用指定的 API 客户端构造引擎。
// 用途: cron query 任务级模型覆盖 (apiClient.WithModel(别名) 的轻量副本) ——
// 一次性会话独占该客户端, 不改动共享默认客户端, 既有会话模型不受污染。
func (sm *SessionManager) createSessionWithClient(chatID string, cli *api.Client) *Session {
	// 会话身份声明 (session_spec.go): 有 spec 的会话按它构造, 没有的走原路径。
	// 取在**最前面**——下面这几十行里有三处要读它 (promptMgr.Cwd/CustomPrompt、
	// cfg.Cwd/AllowedTools), 分散去取会在将来加第四处时漏掉。
	spec, hasSpec := sm.specFor(chatID)
	if hasSpec {
		log.Printf("[Session] 按身份声明构造会话: %s", specString(spec))
	}
	// 生效 cwd: spec 指定则用 spec 的。promptMgr 与 engine.Config 必须取**同一个值**
	// ——只改一处会让"模型以为的工作目录"与"工具实际的工作目录"分叉, 而提示词里
	// 印着前者、命令跑在后者。
	effCwd := sm.config.Cwd
	if hasSpec && spec.Cwd != "" {
		effCwd = spec.Cwd
	}

	// 模型: spec 声明了则按别名另建一个客户端 —— 与 /advisor 是同一手法 (只有换
	// **客户端**才能同时换掉 baseURL/apiKey/protocol; 改 cli.Model 只是换了个名字,
	// 别名指向别的厂商时会把别人的模型名发到默认端点上)。
	//
	// ★ 这一段必须在下面 `cli` 被用到的每一处**之前**。createSessionWithClient 里
	// cli 参与了 compactor / promptMgr.Model / weakProvider 判定 / cfg.Model 五处,
	// 放在中间会让"部分按新模型、部分按默认模型"构造出一个自相矛盾的会话。
	//
	// 解不出来时**不静默回落**: 声明了模型却跑成默认模型, 与"配了且生效"在轨迹上
	// 一模一样 —— 那正是一个建成未通电的假开关。
	if hasSpec && spec.ModelAlias != "" {
		if byAlias := sm.clientForAlias(spec.ModelAlias); byAlias != nil {
			cli = byAlias
			log.Printf("[Session] %s 按身份声明使用模型 %q", chatID, spec.ModelAlias)
		} else {
			log.Printf("[Session] %s 声明了模型 %q 但解析不出可用端点 (或解析器未注入), 本次按默认模型 %q 运行",
				chatID, spec.ModelAlias, cli.Model)
		}
	}

	profile := builtin.ToolProfileChat
	if hasSpec && spec.ToolProfile != "" {
		// spec 钉死档位 (机器调用方不许按消息文本漂移, 见 Spec.ToolProfile 注释)。
		if !builtin.ValidToolProfile(builtin.ToolProfile(spec.ToolProfile)) {
			log.Printf("[Session] ⚠️ %s 声明了非法 toolProfile %q, 回落 chat 档", chatID, spec.ToolProfile)
		}
		profile = builtin.NormalizeToolProfile(builtin.ToolProfile(spec.ToolProfile))
	}
	var runAgentFn agent.RunAgentFunc
	runAgentFn = func(ctx context.Context, agentPrompt string, opts agent.RunOptions) (string, error) {
		return sm.runNestedAgent(ctx, runAgentFn, agentPrompt, opts)
	}
	reg := sm.newProfileRegistry(profile, registryOptions{
		chatID:             chatID,
		includeFeishuTools: true,
		includeTeamQuery:   true,
		includeAgent:       true,
		runAgentFn:         runAgentFn,
	})

	permMode := types.PermissionMode(sm.config.PermissionMode)
	permChecker := permissions.NewChecker(permMode)

	// Hook 配置 (从 JSON config 加载)
	hookRunner := hooks.NewRunner(sm.hookConfigs, "")

	contextWindow := contextWindowOrDefault(sm.defaultResolved.ContextWindow)
	compactor := compact.NewCompactor(cli, contextWindow)
	// 文件级检查点: Write/StrReplace 每次改动前留修订版本, 供回退。
	builtin.SetFileHistory(builtin.NewFileHistory(sm.config.StateDir, effCwd))
	promptMgr := prompt.NewManager(effCwd)
	// spec 的人格优先于全局 SystemPrompt —— 会话身份是更具体的那个声明。
	//
	// 与 claude-go run --system-prompt 同语义: 它是**替换**而不是追加
	// (prompt.go:113, CustomPrompt 非空时直接用它当整个系统提示, 环境段/CLAUDE.md
	// 都不再拼)。选这个语义是为了与既有的 CLI 面一致 —— 同一个字段名在两处
	// 行为不同, 会让人照着 CLI 的文档配 HTTP 面然后得到完全不同的提示词。
	if hasSpec && spec.SystemPrompt != "" {
		promptMgr.CustomPrompt = spec.SystemPrompt
	} else if sm.config.SystemPrompt != "" {
		promptMgr.CustomPrompt = sm.config.SystemPrompt
	}
	promptMgr.Model = cli.Model
	promptMgr.SkillListing = shortSkillListingInDir(sm.skillReg, effCwd)
	promptMgr.ProductName = "Claude Code (Go) - Feishu Bot"
	promptMgr.ShowEnvironmentDate = sm.config.EnvironmentDate
	promptMgr.HookConfigs = sm.hookConfigs
	if sm.dreamer != nil {
		promptMgr.DreamMemoryDir = sm.dreamer.Stats().MemoryDir
	}

	// 计划文件目录: ExitPlanMode 将计划落盘, 实施阶段模型读取。
	// 未显式配置时按 <StateDir>/plans 推导, StateDir 缺失则回落 <Cwd>/plans。
	planFileDir := sm.config.PlanFileDir
	if planFileDir == "" {
		base := sm.config.StateDir
		if base == "" {
			base = sm.config.Cwd
		}
		if base != "" {
			planFileDir = base + "/plans"
		}
	}

	cfg := &engine.Config{
		Model:            cli.Model,
		ExecutionModel:   sm.config.ExecutionModel,
		MaxTokens:        sm.defaultResolved.MaxTokens,
		MaxTurns:         clampTurns(sm.defaultResolved.MaxTurns),
		ContextWindow:    contextWindow,
		Cwd:              effCwd,
		PermissionMode:   permMode,
		IsNonInteractive: true, // 飞书模式始终为非交互式
		Debug:            sm.config.Debug,
		MetricsSource:    "feishu_main",
		MetricsPurpose:   chatID,
		SessionID:        chatID, // EnterPlanMode/ExitPlanMode 写会话级 plan flag
		// H1-3: 压缩时把待办清单嵌进摘要, 否则折掉历史之后模型看不见"还剩什么"。
		// 传 Snapshot (有序) 而非 GetTodos (map 顺序随机): 清单每轮重发, 顺序一抖
		// 前缀缓存就从插入点起全废。chat profile 没有 todo 工具 → nil, 行为不变。
		TaskRoster: todoRosterSource(reg),
		// 单一 plan 状态源: AutoModeHook 自动开启 / 模型 EnterPlanMode/ExitPlanMode 工具
		// 共用同一会话级 flag, 多会话互不污染 (此前 PlanModeActive 扫描所有会话会跨会话泄漏)。
		DynamicPlanCheck: builtin.NewPlanModeChecker(chatID),
		PlanModeSetter:   func(on bool) { builtin.SetPlanModeForSession(chatID, on) },
		// AutoMode: 引擎级复杂度自判断 → 自动 Plan + Advisor (内建能力)。
		AutoPlanMode:   sm.config.AutoPlanMode,
		ComplexityMode: sm.config.ComplexityMode,
		AutoAdvisor:    sm.config.AutoAdvisor,
		// PlanFileDir: ExitPlanMode 将计划落盘, 实施阶段模型读取。默认 <StateDir>/plans。
		PlanFileDir: planFileDir,
		// PlanClient: 规划期独立模型客户端 (Tag=plan); nil = 规划期沿用主模型。
		PlanClient: sm.planClient,
		// DisabledTools 不再在这里写 map 字面量: 名单已收敛到
		// feishuSessionConstraints() 一处声明, 下面 applyConstraints 编译下发
		// (design/01 §4.6)。行为与改造前逐项等价 —— 同样是那三个团队内部工具,
		// 同样不设白名单。
		//
		// 记忆注入形态的**初值**。不接线的话新建会话恒为正文形态: RegisterInternalHooks
		// 在构造时按 resolvedMemoryIndexMode 定形态, 而它读的是这个字段 —— 漏掉这条,
		// 开关就只剩热加载推活跃会话这一半, 新会话全落回默认。
		//
		// 直接转交指针 (含 nil): nil 表示「没配」, 由 resolvedMemoryIndexMode 回落到
		// CLAUDE_GO_MEMORY_INDEX_MODE; 在这里写成 boolPtr(false) 会把兜底打死。
		// 运行期翻转走 SessionManager.SetMemoryIndexMode, 不经这个字段。
		MemoryIndexMode: sm.config.MemoryIndexMode,
	}
	// 方案三: 弱模型 harness 增强装配 (与 CLI 同规则——env 显式 > provider=="ollama" 自动)。
	// kimi 等云端 provider 恒为关, 生产零变化; ollama 别名会话自动获得 L2/L3/L4/L6b/L8 护栏。
	// 注意按**生效客户端**的别名解析 provider: cron query 任务用 WithModel 覆盖成
	// ollama 别名时, 护栏必须跟着别名走, 不能还看默认模型的 provider。
	weakProvider := sm.defaultResolved.Provider
	if cli != sm.apiClient {
		if p, _, ok := strings.Cut(cli.Model, ":"); ok && p != "" {
			weakProvider = p
		}
	}
	cfg.WeakModel = weakmodel.Resolve(weakProvider, os.Getenv)
	// llm/hybrid 复杂度判定: 用生效客户端的 SimpleComplete + 分类器 system prompt。
	// heuristic 模式零 LLM 开销, 不注入。
	if sm.config.ComplexityMode == "llm" || sm.config.ComplexityMode == "hybrid" {
		cfg.LLMComplexityFn = complexityLLMClassifier(cli)
	}
	// spec 的工具白名单必须在 applyConstraints **之前**落进 cfg:
	// ApplyConstraints 对已有的 AllowedTools 取**交集** (engine.go:391-409),
	// 所以先设 spec 的白名单, 约束层再收窄 —— 顺序反过来的话, 约束层会先建一个
	// 名录, spec 的白名单就完全落不下去了 (而且不报错)。
	//
	// 反向也成立且是刻意的: spec 不能**放宽**约束层已经收窄的东西 ——
	// ApplyConstraints 只做交集, 不做并集。
	if hasSpec {
		applySpecTo(cfg, spec)
	}
	applyConstraints(cfg, feishuSessionConstraints())

	// Advisor push 模式 (Phase 3): checkpoint hook 与 pull 模式共享同一工具实例的预算
	if advTool := sm.advisorToolFor(chatID); advTool != nil && sm.config.Advisor != nil {
		if sm.config.Advisor.CheckpointEveryTurns > 0 || sm.config.Advisor.CheckpointOnLoop {
			cfg.AdvisorConsultFn = advTool.Consult
			cfg.AdvisorCheckpointEveryTurns = sm.config.Advisor.CheckpointEveryTurns
			cfg.AdvisorCheckpointOnLoop = sm.config.Advisor.CheckpointOnLoop
		}
	}

	eng := engine.NewQueryEngine(cfg, cli, reg, hookRunner, permChecker, compactor, promptMgr)

	// 装配自报 (成功路径也必须报数): 这次会话**实际**能让模型看见几个工具。
	//
	// 为什么必须报: 白名单是"收窄"而不是"放大" —— 它只能从本档位**已注册**的工具里
	// 挑。于是 `allowedTools:["Bash"]` 若落在一个不含 Bash 的档位上 (chat 档只注册
	// AskUserQuestion / 代码检索 / ToolSearch), 交出来的可见集是**空** —— 而那个空
	// 与 `allowedTools:[]` 的"全拒"在建会话这一步完全同形、零报错: 模型只会回一句
	// "我这个会话里没有工具", 日志里一行异常都没有。
	// 与 VisibleTools 的注释同一理由: 装配自报要的是"模型能看见什么", 而它读的正是
	// 过滤后的结果 (拿注册表去报会把这件事完全掩盖掉)。
	if hasSpec && spec.AllowedTools != nil {
		visible := eng.VisibleTools()
		// 只点出"声明了却没落地"的那些 —— 泛泛报个"可见 N 个"没人会去比对数字。
		var missing []string
		for _, want := range *spec.AllowedTools {
			found := false
			for _, v := range visible {
				if v == want {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			log.Printf("[Session] ⚠️ %s 的 allowedTools 声明了本档位 (%s) 不存在的工具 %v —— "+
				"白名单只能收窄不能放大, 这些声明不会生效 (本档位可见工具共 %d 个)",
				chatID, profile, missing, len(visible))
		}
		if len(visible) == 0 {
			log.Printf("[Session] ⚠️ %s 本次装配下**没有任何工具对模型可见** (声明 tools=[%s]); "+
				"模型将无法执行任何操作", chatID, allowedToolsSummary(spec))
		}
	}

	eng.MemoryStore = sm.memoryStore
	eng.TraceStore = sm.traceStore // design/03 §4.1 E1
	if sm.config.EnableFrontierOptimizations {
		// 与 CLI 路径同一口径 (方案 20.6.5 项 2): CaRT 停止信号由配置声明,
		// 缺省按本路径原行为 (主会话不开)。必须设在 EnableFrontierOptimizations 之前
		// —— 它内部的 applyFeatureFlags 按构造时的字段状态懒初始化组件。
		if sm.config.EnableStopSignal != nil {
			eng.Config.EnableStopSignal = *sm.config.EnableStopSignal
		}
		eng.EnableFrontierOptimizations()
	}

	// ==================== 会话历史持久化 (design/02 §1.4 + §七 R2) ====================
	// 修复"飞书会话历史纯内存, 重启即丢": 此前 eng.SessionStore 从未赋值, 历史只活在
	// QueryEngine.Messages 里, 重启后 sessions map 为空 → 造一个 Messages 为空的新引擎,
	// 上下文彻底丢失且用户无提示。
	// 选 KV 整份快照而非 design/02 表里写的 Log 追加日志, 三条理由见
	// pkg/session/statestore_transcript.go 头部注释 (自截断 / Log 无删除原语表达不了
	// /clear / 顺带修 tool_result 从不落盘的老缺陷)。
	tr := sessionstore.NewStateStoreTranscript(sm.stateStore, chatID)
	sess := &Session{
		ChatID:      chatID,
		Engine:      eng,
		LastActive:  time.Now(),
		ToolProfile: profile,
		transcript:  tr,
	}
	if hasSpec {
		// 轨迹分组用 (见 session_trace.go 的 displayName)。
		sess.DisplayName = spec.DisplayName()
	}
	if tr != nil {
		// 写: 每轮末一次整份快照, fail-open (Snapshot 内部已计数, 这里吞掉错误,
		// 绝不让落盘失败阻塞或污染用户回复)。
		eng.SnapshotFn = func(msgs []types.Message) { _ = tr.Snapshot(msgs) }
		// 读: 语义与 CLI --resume/--continue 对齐 (cmd/claude-go/main.go 同样是直接
		// eng.Messages = msgs)。读失败只记日志, 按空历史继续。
		if msgs, err := tr.Load(); err != nil {
			log.Printf("[Session] 读回会话历史失败 chat=%s: %v", chatID, err)
		} else if len(msgs) > 0 {
			eng.Messages = msgs
			log.Printf("[Session] 已从快照恢复会话历史 chat=%s, %d 条消息", chatID, len(msgs))
		}
	}
	return sess
}

// runNestedAgent 创建嵌套 QueryEngine 执行子代理。
// 对应 TS: tools/AgentTool/runAgent.ts 中的 runAgent()
//
// 流程:
//  1. 创建新的 tool.Registry (共享 MCP 连接)
//  2. 递归注册 Agent 工具 (子代理也能派生子代理)
//  3. 创建独立的 QueryEngine
//  4. 执行查询循环，收集所有 assistant 文本
//  5. 返回合并后的结果
func (sm *SessionManager) runNestedAgent(ctx context.Context, runAgentFn agent.RunAgentFunc, agentPrompt string, opts agent.RunOptions) (string, error) {
	permMode := types.PermissionMode(sm.config.PermissionMode)
	permChecker := permissions.NewChecker(permMode)
	if opts.ReadOnly {
		permMode = types.PermissionModePlan
		permChecker = permissions.NewChecker(permMode)
	}

	// 从 context 读取 ModelConfigKey, 使嵌套 agent 继承外层 agent 的 plan 配置
	nestedAPIClient := sm.apiClient
	nestedModel := sm.apiClient.Model
	maxTokens := sm.defaultResolved.MaxTokens
	maxTurns := sm.defaultResolved.MaxTurns
	contextWindow := contextWindowOrDefault(sm.defaultResolved.ContextWindow)
	promptCacheMode := sm.config.PromptCacheMode
	if mcfg, ok := ctx.Value(agent.ModelConfigKey{}).(modelconfig.ResolvedConfig); ok && mcfg.ProviderName != "" {
		// 出站模型名走 OutboundModelName (经网关=完整别名, 直连厂商=裸名) ——
		// 见 ResolvedConfig.OutboundModelName 的注释。
		nestedModel0 := mcfg.OutboundModelName()
		nestedAPIClient = sm.apiClient.ConfiguredCloneFull(
			mcfg.BaseURL, mcfg.APIKey, nestedModel0, mcfg.FallbackModels,
			mcfg.FallbackBaseURL, mcfg.FallbackAPIKey,
		)
		nestedModel = nestedModel0
		if mcfg.MaxTokens > 0 {
			maxTokens = mcfg.MaxTokens
		}
		if mcfg.MaxTurns > 0 {
			maxTurns = mcfg.MaxTurns
		}
		if mcfg.ContextWindow > 0 {
			contextWindow = mcfg.ContextWindow
		}
		if mcfg.PromptCacheMode != "" {
			promptCacheMode = mcfg.PromptCacheMode
		}
	}
	if promptCacheMode != "" {
		nestedAPIClient.PromptCacheMode = promptCacheMode
	}

	// 带观测 ctx (design/01 §4.5 ExternalHook 适配器): 这个 Runner 是 per-调用 构造的,
	// 且这条路径 (Agent 工具派生的嵌套 agent) 可能发生在图节点内部 —— ctx 上有
	// hooks.Observer 时它的外部 hook 事件就进图总线, 没有时逐字节等价于 NewRunner。
	// hook 的构造**推后**到资产装配之后 (见下面的 assetBind 段): 资产声明的 hook
	// 必须与配置 hook 一起进同一个 Runner。分两个 Runner 是不行的 —— 引擎只收一个,
	// 分出来的那个要么没人跑, 要么得在引擎里加第二条链 (那是"同一条声明两条送达
	// 路径"的翻版)。
	var hookRunner *hooks.Runner
	compactor := compact.NewCompactor(nestedAPIClient, contextWindow)
	promptMgr := prompt.NewManager(sm.config.Cwd)
	promptMgr.Model = nestedModel
	promptMgr.SkillListing = sm.stageSkillListing("", agent.StageDeclFromContext(ctx).Skills)

	model := nestedModel
	if opts.Model != "" {
		model = opts.Model
	}
	runMeta := agent.RunMetadataFromContext(ctx)
	nestedPurpose := firstNonEmpty(opts.SubagentType, runMeta.Purpose, runMeta.Team)

	// P2 组装 + P5 查表: subagent_type 先按 agent 资产**装配**, 装出来的东西直接
	// 决定正文/档位/hook; 查不到才回落到既有的角色名子串猜测 (并留痕 —— 那条猜测
	// 正是 P3 的待退役项)。
	//
	// 顺序不能反: 先猜测再查表会让"资产声明"变成一次对猜测结果的覆盖, 而猜测对
	// 每份资产都必然给得出一个答案 —— 于是"资产声明的档位"几乎永远不会是最终档位,
	// 声明等于摆设。先查表则天然是"显式 > 猜测"。
	//
	// 装配读数 (bind) 一路带到下面: 档位、hook、技能、权限档、模型、回合上限都从
	// 它取, 而不是从资产上各查一遍 —— 一处查、一处判合法性, 才不会出现"档位校验
	// 过了但权限档没校验"这种半边接线的形态。
	assetBind := sm.bindAgentAsset(opts.SubagentType)
	logAgentBindingWarnings(assetBind)

	// hook 绑定: 资产声明的外部 hook **追加**在配置 hook 之后。
	//
	// 追加而不是替换, 也不去重: 两者来源不同 (配置是部署方的, 资产是 job 作者的),
	// 合并成一份再交给同一个 Runner 才是"一条链"。为什么带 ctx: 这个 Runner 是
	// per-调用 构造的, 而这条路径 (Agent 工具派生的嵌套 agent) 可能发生在图节点内部
	// —— ctx 上有 hooks.Observer 时它的外部 hook 事件就进图总线, 没有时逐字节等价
	// 于 NewRunner (design/01 §4.5 ExternalHook 适配器)。
	hookConfigs := sm.hookConfigs
	if assetBind != nil && len(assetBind.Hooks) > 0 {
		merged := make([]types.HookConfig, 0, len(hookConfigs)+len(assetBind.Hooks))
		merged = append(merged, hookConfigs...)
		merged = append(merged, assetBind.Hooks...)
		hookConfigs = merged
	}
	// 阶段声明的 hook (21.8.12 P4/X1) 同样**追加**在最后。
	//
	// 三段来源写成一个接一个的 append 而不是"求并集", 是因为优先级在这里没有意义 ——
	// hook 是**并集语义**: 三段都跑。唯一需要保序的理由是可读性 (读数里的顺序要与
	// 叠加顺序一致), 以及"配置在最前"这条口径 (部署方的审计 hook 先于作者的)。
	//
	// 解析用 hooks.ParseHookDecls —— 与资产 hooks: 段**同一份实现**。各自解析一遍
	// 的话, `<event>:<type>:<target>[:<if>]` 的冒号切分规则会在两处漂移, 而漂移的
	// 表现是"同一种写法在资产里读得出、在 stage 里读不出", 不报错。
	if decl := agent.StageDeclFromContext(ctx); len(decl.Hooks) > 0 {
		stageCfgs, probs := hooks.ParseHookDecls(decl.Hooks)
		for _, p := range probs {
			log.Printf("[Hooks] 阶段声明的 hook 问题: %s", p)
		}
		if len(stageCfgs) > 0 {
			merged := make([]types.HookConfig, 0, len(hookConfigs)+len(stageCfgs))
			merged = append(merged, hookConfigs...)
			merged = append(merged, stageCfgs...)
			hookConfigs = merged
		}
	}
	hookRunner = hooks.NewRunnerWithContext(ctx, hookConfigs, "")

	// 生命周期 hook 的**开始**两拍。
	//
	// 这两行不是"顺手补个功能": 改造前本函数一个生命周期 hook 都不触发 —— 触发它们
	// 的是另一条路径 (CreateAgentRunner.Execute, 图节点用, 见本文件 1986 那一处)。
	// 而 P2 让 agent 资产能声明外部 hook, 其中作者最可能写的恰好就是 SessionStart 与
	// SubagentStart。只把配置装进 Runner 而不触发的话, 一份声明了 SessionStart hook 的
	// 资产在装配读数上是 wired、对账是干净的、日志里一个字不打 —— 而那个脚本永远不跑。
	// 这正是本批次反复要防的"声明了但没人读"。
	//
	// 语义与图节点那条路径**逐条对齐** (含 block/deny 即中止本次派生): 同一条 hook
	// 声明在两条路径上必须给出同一个后果, 否则"这个 hook 管不管用"取决于子代理是被
	// 图调起来的还是被 Agent 工具派出来的 —— 那对写 hook 的人不可预测。
	//
	// 对存量部署这是零行为变化: 本仓的 config.json 里没有 hooks 键 (实测)。变的是
	// "资产声明的那两条从此真的会跑"。
	if hookRunner != nil {
		if hookOut := hookRunner.ExecuteSessionHooks(types.HookEventSessionStart); hookOut != nil &&
			(hookOut.Decision == "block" || hookOut.Decision == "deny") {
			return "", fmt.Errorf("SessionStart blocked by hook: %s", hookOut.Reason)
		}
		if hookOut := hookRunner.ExecuteSubagentStartHooks(nestedPurpose, agentPrompt); hookOut != nil &&
			(hookOut.Decision == "block" || hookOut.Decision == "deny") {
			return "", fmt.Errorf("SubagentStart blocked by hook: %s", hookOut.Reason)
		}
	}

	assetProfile, hasAssetProfile := builtin.ToolProfile(""), false
	if assetBind != nil && assetBind.HasToolProfile {
		assetProfile, hasAssetProfile = assetBind.ToolProfile, true
	}
	// 正文进 AppendPrompt 而**不是** AgentPrompt: 后者会把整个默认系统提示词
	// (环境块 / 工具使用指南 / CLAUDE.md 记忆) 换掉, 那是一次比"读到一份 agent 定义"
	// 大得多的行为变更。追加是纯增量的, 也正是这里要的。
	if assetBind != nil && assetBind.Prompt != "" {
		if promptMgr.AppendPrompt != "" {
			promptMgr.AppendPrompt = promptMgr.AppendPrompt + "\n\n" + assetBind.Prompt
		} else {
			promptMgr.AppendPrompt = assetBind.Prompt
		}
	}

	// 技能面 (P2 的 skill 绑定): 资产声明的技能并进这份子代理的技能清单。
	//
	// 并进**已有清单**而不是替换: 那条清单来自 roleScopedSkillListing 的角色池
	// (团队 stage 用), 把它整个换掉等于让资产声明顺带撤销了角色的技能面 —— 那是
	// 一次没人会预期的收窄。空声明则一个字都不动, 与改造前逐字节一致。
	//
	// ★ 这条绑定的实际作用面比它看起来窄, 写在这里以免被误当成一条强断言:
	// FormatShortListingForNames 用的仍是**全池视图** (只过滤 shadow/archived 与
	// 模型不可调用), 它并不等于"只暴露声明的这几条"。所以当清单本来就非空时
	// (promptMgr.SkillListing 由 shortSkillListingInDir 用全池填过), 这次并进是
	// **幂等的** —— 两条路径对"这条技能在不在清单里"给同一个答案。
	//
	// 它真正独有的场景只有一个, 但那个场景是实的: **按 paths 裁剪的清单**。
	// FormatShortListingForDir 会把 paths 不命中当前目录的技能整个剔掉, 而这里走的
	// 是 FormatShortListingForNames —— **不看 paths**。于是"资产显式声明要一条技能,
	// 而那条技能的 paths 与这个工作目录不匹配"时, 只有这一行能让它进清单。
	// 这正是 P2 要的语义: 显式声明 > 目录推断。
	if assetBind != nil && len(assetBind.Skills) > 0 {
		extra := sm.skillReg.FormatShortListingForNames(assetBind.Skills)
		if strings.TrimSpace(extra) != "" {
			if promptMgr.SkillListing != "" {
				promptMgr.SkillListing = promptMgr.SkillListing + "\n" + extra
			} else {
				promptMgr.SkillListing = extra
			}
		}
	}

	// 工具画像: 同样经 ConstraintSet 决策 (design/01 §4.6)。显式声明来自外层图节点
	// (ctx 里的 NodeExecHints), 但只能"取更窄的那个" —— 理由见 nestedExplicitProfile。
	nestedFallbackProfile := profileForRunOptions(opts, runMeta)
	nestedConstraints := agent.NewConstraintSet("nested-agent:" + firstNonEmpty(opts.SubagentType, runMeta.Role, "anonymous"))
	// 资产档位**只有这一条通路** (WithToolProfile ⇒ ProfileSourceExplicit)。
	//
	// 不要在下面的 fallback 闭包里也返回它: 同一条声明有两条送达路径时, 拆掉任意
	// 一条都不改变行为 —— 一份"看起来接了、实际可以整条删掉"的接线, 比明摆着没接
	// 更难发现。这个坑是我写这一版时踩的: 第一版两处都返回 assetProfile, 把
	// WithToolProfile 那行删掉后测试**依然全绿** (实测)。
	if hasAssetProfile {
		nestedConstraints.WithToolProfile(string(assetProfile))
	} else {
		// 查不到才回落到下面的角色名子串猜测, 那条路正是 P3 的待退役项。
		warnUnknownSubagentType(opts.SubagentType, "nested-agent")
	}
	nestedConstraints.WithToolProfile(string(nestedExplicitProfile(
		mapExplicitToolProfile(agent.NodeExecHintsFromContext(ctx).ToolProfile),
		nestedFallbackProfile,
	)))
	// 工具名单 (P2′): 资产声明的 `tools:` 白名单与 `disallowed-tools:` 黑名单。
	//
	// ★ 这里补的是一个**已经存在的洞**, 不是新功能。改造前 assetBind 已经把这两份名单
	// 解析出来、也写进了装配读数 (Binding.Declared), 但**没有任何一行把它们送进运行时**
	// —— 于是 `disallowed-tools: [Bash]` 的效果是"报告里写着挡了, 实际一个没挡"。
	// 这正是本仓最贵的那类缺陷 (声明了但没人读), 只不过这一次它藏在"读数看起来对"后面。
	//
	// 走 ConstraintSet 而不是直接写 cfg.DisabledTools: 名单的**合并规则只有一份**
	// (CompiledToolAllow/Deny ⇒ ApplyConstraints), 在那里白名单取交集、黑名单取并集,
	// 按构造只会收窄。绕过它自己拼一份就是对同一个字段有两个写者。
	//
	// 两条路的收紧方向不同, 这不是笔误:
	//   WithDeniedTools  —— 追加黑名单 (并集), 只会让可见面更小。
	//   WithToolAllowList —— 设白名单。ApplyConstraints 对已有白名单取**交集**,
	//                        所以资产声明的名单只会裁得更窄, 不会因为"资产说它允许"
	//                        就把外层已经禁掉的工具放回来。
	//
	// 空切片与 nil 的差别在这里有实际后果, 所以不能用一个 len()>0 把两者合并:
	// WithToolAllowList(nil) 是 no-op (不设白名单), 而 []string{} 是"显式空名单" =
	// fail-closed 拒绝一切。前者是"资产没写 tools:", 后者是"资产写了 tools: []"。
	//
	// ★ 判据用 HasTools / HasDisallowedTools (**键写没写**), 不是 `Tools != nil`。
	//
	// 之所以不靠 nil-ness: 那是一条隐式约定 —— 它要求 bindTools 每次解析后都记得把
	// 显式空名单补成非 nil 空切片。约定一旦在别处被改掉 (比如有人顺手把 dedupe 换成
	// 了别的写法), 这里的 `!= nil` 会静默变成"没声明", fail-closed 退化成完全放行,
	// 而且没有一条测试会红 —— 因为声明与放行在读数上都"正常"。用存在性这一位就把
	// 那条约定变成了接口的一部分, 改不动也忘不掉。
	if assetBind != nil {
		if assetBind.HasTools {
			nestedConstraints.WithToolAllowList(assetBind.Tools)
		}
		if assetBind.HasDisallowedTools {
			nestedConstraints.WithDeniedTools(assetBind.DisallowedTools...)
		}
	}
	nestedProfile := resolveToolProfile(nestedConstraints, agent.ProfileSourceRoleNameFallback, func() builtin.ToolProfile {
		return nestedFallbackProfile
	})
	nestedReg := sm.newProfileRegistry(nestedProfile, registryOptions{})

	cfg := &engine.Config{
		Model:            model,
		MaxTokens:        maxTokens,
		MaxTurns:         clampTurns(maxTurns),
		ContextWindow:    contextWindow,
		Cwd:              sm.config.Cwd,
		PermissionMode:   permMode,
		IsNonInteractive: true,
		Debug:            sm.config.Debug,
		MetricsSource:    "nested_agent",
		MetricsPurpose:   nestedPurpose,
		Workflow:         runMeta.Workflow,
		Role:             firstNonEmpty(opts.SubagentType, runMeta.Role),
		Team:             runMeta.Team, // 13.7-P1 池个性化身份 (嵌套子代理继承团队名)
	}
	// P2 声明三连: 模型 / 回合上限 / 权限档。三者都**只在资产真的声明了**的时候
	// 才覆盖, 且权限档只能往严里走 (见下)。
	//
	// 顺序在这里有实际后果: 这一段必须排在 cfg 字面量**之后**、applyConstraints 之前
	// —— 权限档要由 permChecker 与 cfg.PermissionMode 同时换掉 (引擎构造时用 checker,
	// 报告里读 cfg), 漏掉任何一边都会让"装配自报"与"实际判定"对不上。
	if assetBind != nil {
		if assetBind.Model != "" {
			cfg.Model = assetBind.Model
			model = assetBind.Model
		}
		if assetBind.MaxTurns > 0 {
			cfg.MaxTurns = clampTurns(assetBind.MaxTurns)
		}
		if assetBind.HasPermissionMode {
			// ★ 只许收窄。资产目录是 state/project/user 三层都能写的地方 (evo 与
			// 面板也会落盘), 一份 .md 里写 `permission-mode: bypass` 若无条件生效就是
			// 一条从文件到权限的提升路径 —— 而且完全静默。口径与 ApplyConstraints 的
			// "约束按构造只会收窄" 一致: 更严的声明照收, 更宽的把**外层的档**保持住
			// 并留痕 (降级可见)。
			if permissions.Widens(cfg.PermissionMode, assetBind.PermissionMode) {
				log.Printf("[P2][warn] agent 资产 %q 声明的 permission-mode=%q 比外层 (%q) 更宽, 已拒绝"+
					" —— 声明只能收窄权限面", assetBind.Name, assetBind.PermissionMode, cfg.PermissionMode)
			} else {
				cfg.PermissionMode = assetBind.PermissionMode
				permMode = assetBind.PermissionMode
				permChecker = permissions.NewChecker(permMode)
			}
		}
	}
	// 方案三: 弱模型 harness 增强装配 (与 CLI 同规则——env 显式 > provider=="ollama" 自动)。
	// kimi 等云端 provider 恒为关, 生产零变化; ollama 别名会话自动获得 L2/L3/L4/L6b/L8 护栏。
	cfg.WeakModel = weakmodel.Resolve(sm.defaultResolved.Provider, os.Getenv)
	applyConstraints(cfg, nestedConstraints)

	nested := engine.NewQueryEngine(cfg, nestedAPIClient, nestedReg, hookRunner, permChecker, compactor, promptMgr)
	// 轨迹底座 (design/01 §4.8 / design/03 §4.1 E1)。
	//
	// 改造前这一行**不存在**: createSession(:679) 与 stage 引擎(:1211) 都赋了
	// TraceStore, 唯独嵌套子代理没有 —— 于是"父会话的每次调用有 llm_call Span,
	// 它派生的子代理一条都没有"。子代理恰恰是烧钱大户 (它自己也跑完整的工具循环),
	// 这个缺口让 §4.8 那句"一个节点可以在里面烧掉任意多 token 而图这一层什么都
	// 看不到"在**轨迹**这一维上字面成立。
	//
	// 只赋字段、不调 RefreshHooks: 与上面两处赋值口径完全一致 (llm_call Span 由
	// engine 直接读 e.TraceStore 写出, 不经 HookChain)。turn/tool_call 那两种 Span
	// 靠 TraceCaptureHook, 而本进程里它从未被注册过 —— 那是本文件之外的既有缺口,
	// 在这里单独把嵌套引擎打开会让子代理的轨迹形态比父会话还全, 反而不可比对。
	nested.TraceStore = sm.traceStore

	// P2 的另一半: **对账**。装完之后立刻把"这份资产声明要什么"与"这台引擎实际装成了
	// 什么"逐项比一次 (方案 20.6.4 的顺序: 先自报, 再对账, 最后才轮到面板)。
	//
	// 自报取自**活引擎**而不是配置: 本仓修过的那个真缺陷 (CLI 路径六个组件为 nil)
	// 的表现正是"配置文件里一切都在, 引擎里那一半是空的", 读配置得到的报告会一路全绿。
	//
	// 只在真有资产声明时才对账 —— 没有声明的常态路径 (绝大多数派生) 不该为一次
	// 无意义的比对付出采集成本, 也不该在日志里多出一行恒空的结论。
	if decl := assetBind.AssemblyDeclaration(); decl != nil {
		// Config 留 nil: 嵌套子代理不是从配置查找链装出来的 (它的模型/预算/档位
		// 都继承自父会话)。留 nil 与"给了个空路径"是两回事 —— 后者会在报告里报成
		// "六环都没命中", 那是一条凭空多出来的假线索。
		_, rec := engine.ReconcileAssembly(nested, engine.AssemblyReportInput{}, decl)
		if rec != nil {
			if len(rec.Diffs) > 0 {
				// 有差异 = 声明与生效对不上 (声明了没装上 / 装上了没声明)。这是
				// 最该被看见的一类, 逐条印出来, 且**不静默**。
				log.Printf("[P2] agent 资产 %q 的装配对账: %s", assetBind.Name, rec.Summary)
				for _, d := range rec.Diffs {
					log.Printf("[P2][diff] %s %s/%s 声明=%q 实际=%q",
						d.Kind, d.Facet, d.Item, d.Declared, d.Actual)
				}
			} else {
				log.Printf("[P2] agent 资产 %q 的装配对账: %s", assetBind.Name, rec.Summary)
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

// evictOldest 淘汰最早活跃的会话 (需在锁内调用)。
//
// **淘汰 ≠ 遗忘**: 这里只从内存 map 里卸载会话 (释放引擎/工具注册表/advisor 预算),
// 绝不删持久化快照。LRU 淘汰是资源管理, 不是用户意图; 该 chat 下次说话时
// GetOrCreate → createSession 会把历史从快照读回来。只有 /clear (ClearSession)
// 才代表用户显式"忘记", 才允许删盘。
func (sm *SessionManager) evictOldest() {
	var oldestID string
	var oldestTime time.Time

	for id, s := range sm.sessions {
		s.mu.Lock()
		if oldestID == "" || s.LastActive.Before(oldestTime) {
			oldestID = id
			oldestTime = s.LastActive
		}
		s.mu.Unlock()
	}

	if oldestID != "" {
		delete(sm.sessions, oldestID)
		sm.dropAdvisorTool(oldestID)
	}
}

// dropAdvisorTool 移除会话对应的 advisor 工具实例 (预算状态随会话生命周期释放)。
func (sm *SessionManager) dropAdvisorTool(chatID string) {
	sm.advisorMu.Lock()
	defer sm.advisorMu.Unlock()
	delete(sm.advisorTools, chatID)
}

// cleanupLoop 后台定期清理超时会话
func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		sm.cleanup()
	}
}

// cleanup 清理超时会话。
// 与 evictOldest 同一条纪律: 只卸载内存, 绝不删持久化快照 —— 闲置 30 分钟被回收的
// 会话, 用户第二天回来接着聊时历史必须还在。
func (sm *SessionManager) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	for id, s := range sm.sessions {
		s.mu.Lock()
		if now.Sub(s.LastActive) > sm.sessionTimeout && !s.processing {
			delete(sm.sessions, id)
			sm.dropAdvisorTool(id)
		}
		s.mu.Unlock()
	}
}

// ClearSession 清除指定会话 (用于 /clear 命令)。
//
// 这是唯一允许删持久化快照的路径 (对比 evictOldest/cleanup 只卸内存): /clear 是
// 用户显式"忘掉之前的对话", 若只删内存, 下一条消息重建会话时历史会从快照里复活。
func (sm *SessionManager) ClearSession(chatID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s := sm.sessions[chatID]
	delete(sm.sessions, chatID)
	sm.dropAdvisorTool(chatID)

	// 常驻会话直接用它的句柄; 已被淘汰/清理掉 (内存里没有) 的 chat 也必须能删盘,
	// 所以按 chatID 现建一个句柄 (Delete 幂等)。
	tr := s.transcriptHandle()
	if tr == nil && sm.stateStore != nil {
		tr = sessionstore.NewStateStoreTranscript(sm.stateStore, chatID)
	}
	if err := tr.Delete(); err != nil {
		log.Printf("[Session] 删除会话历史快照失败 chat=%s: %v", chatID, err)
	}
}

// transcriptHandle 空安全地取会话的快照句柄。
func (s *Session) transcriptHandle() *sessionstore.StateStoreTranscript {
	if s == nil {
		return nil
	}
	return s.transcript
}

// Stats 返回会话统计
func (sm *SessionManager) Stats() (total int, active int) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	total = len(sm.sessions)
	for _, s := range sm.sessions {
		if s.IsProcessing() {
			active++
		}
	}
	return
}

// ProcessMessage 在指定会话中处理用户消息。
// 这是会话级别的消息处理入口:
//  1. 获取或创建会话
//  2. 检查是否正在处理 (防止并发)
//  3. 提交消息到 QueryEngine
//  4. 收集所有响应
//  5. 返回格式化的文本
func (sm *SessionManager) ProcessMessage(ctx context.Context, chatID, userText string) (string, error) {
	return sm.processMessageInternal(ctx, chatID, userText)
}

// ProcessMessageWithModel 用指定模型别名处理一次性查询 (cron query 任务的模型覆盖)。
// 别名为空时退化为 ProcessMessage。覆盖路径与缓存会话完全隔离:
//   - 一次性会话, 不进 sm.sessions 缓存, 用完即弃;
//   - 合成会话键 (cron:<chatID>:<别名>) —— 不与用户真实会话共享 plan flag /
//     历史文件 (两个会话对象同写一个 chatID 的历史会互相覆盖);
//   - 客户端是 apiClient.WithModel(别名) 的轻量副本, 共享底层 HTTP/限流器。
func (sm *SessionManager) ProcessMessageWithModel(ctx context.Context, chatID, userText, modelAlias string) (string, error) {
	if strings.TrimSpace(modelAlias) == "" {
		return sm.ProcessMessage(ctx, chatID, userText)
	}
	cli := sm.apiClient.WithModel(modelAlias)
	synthID := "cron:" + chatID + ":" + modelAlias
	sm.mu.Lock()
	session := sm.createSessionWithClient(synthID, cli)
	sm.mu.Unlock()
	return sm.runSessionMessage(ctx, session, userText)
}

// ProcessMessageWithQueue 处理消息, 如果会话繁忙则排队等待
// reply 回调用于异步发送排队消息的回复
func (sm *SessionManager) ProcessMessageWithQueue(ctx context.Context, chatID, userText string, reply func(string)) (string, bool, error) {
	session := sm.GetOrCreate(chatID)
	if session.IsProcessing() {
		session.EnqueuePending(userText, reply)
		return "上一条消息还在处理中，你的消息已排队，处理完后会自动继续。", true, nil
	}
	resp, err := sm.processMessageInternal(ctx, chatID, userText)
	return resp, false, err
}

func (sm *SessionManager) processMessageInternal(ctx context.Context, chatID, userText string) (string, error) {
	return sm.runSessionMessage(ctx, sm.GetOrCreate(chatID), userText)
}

// runSessionMessage 在指定会话上跑一条消息 (processMessageInternal 的会话来源是
// 缓存, ProcessMessageWithModel 的是一次性会话; 流程本体两者共用)。
func (sm *SessionManager) runSessionMessage(ctx context.Context, session *Session, userText string) (string, error) {

	if session.IsProcessing() {
		return "上一条消息还在处理中，请稍候...", nil
	}

	// 会话级轨迹 (session_trace.go): 给这一次运行开 trace 作用域, 让引擎内的
	// turn/tool_call/llm_call Span 落到 trace-run-<会话>-<序号> 桶而不是 trace-orphan。
	// 收尾在**下面那个 defer 里**调用 —— 与 SetProcessing(false) 同一处收口,
	// 因为那是唯一"无论从哪条路径退出都会走一遍"的位置。
	var runErr error
	ctx, endRun := sm.beginSessionRun(ctx, session)
	defer func() {
		status := runSpanStatusOK
		if runErr != nil {
			status = runSpanStatusFailed
		}
		endRun(status, errTextOf(runErr))
	}()

	session.SetProcessing(true)
	defer func() {
		session.SetProcessing(false)
		// 自动消费队列中的下一条消息
		if pendingText, pendingReply := session.DequeuePending(); pendingText != nil && pendingReply != nil {
			// 第七章缺口修复: user.steer 奖励源——用户在处理中追加消息 = 对上一轮
			// 产出的实时修正信号 (设计值 -0.5)。只在该会话确有团队 run 时记录,
			// 纯聊天打断不记 (无上下文的打断不是奖励证据)。
			if sm.evolution != nil && sm.teamMgr != nil {
				if teamName, runID, ok := sm.teamMgr.LastRunForChat(session.ChatID); ok {
					sm.evolution.RecordReward(agent.RewardEvent{
						RunID:  runID,
						Source: agent.RewardSourceUserSteer,
						Value:  -0.5,
						Raw:    map[string]any{"steer_text": *pendingText, "via": "pending_consume"},
						Team:   teamName,
					})
				}
			}
			go func() {
				resp, err := sm.processMessageInternal(context.Background(), session.ChatID, *pendingText)
				if err != nil {
					pendingReply(fmt.Sprintf("处理排队消息失败: %v", err))
				} else {
					pendingReply(resp)
				}
			}()
		}
	}()
	session.Touch()
	// 工具档位: spec 钉死 (ToolProfile) 的会话按钉死的来, 不允许按消息文本漂移;
	// 没钉的维持原启发式 (inferFeishuToolProfile)。
	if pinned, ok := sm.pinnedProfileFor(session.ChatID); ok {
		sm.configureSessionTools(session, pinned)
	} else {
		sm.configureSessionTools(session, resolveSessionToolProfile(userText))
	}

	ch := session.Engine.SubmitMessage(ctx, userText)

	var response string
	for msg := range ch {
		text := extractMessageText(msg)
		if text != "" {
			response += text
		}
	}

	if response == "" {
		response = "(无回复内容)"
	}

	// 提取关键事实到 Episodic Memory
	if sm.memoryStore != nil && response != "" && response != "(无回复内容)" {
		sm.memoryStore.Add(&memory.MemoryEntry{
			Content:    truncateForDream(response),
			Source:     "extraction",
			Importance: 0.6,
			ChatID:     session.ChatID,
			Topics:     extractTopics(response),
		})
	}

	// 触发 Dreaming 检查 (对应 TS: stopHooks.ts → executeAutoDream)
	if sm.dreamer != nil {
		sm.dreamer.RecordSession(dreaming.SessionRecord{
			ChatID:  session.ChatID,
			EndTime: time.Now(),
			Summary: truncateForDream(response),
		})
		sm.dreamer.AfterQuery(ctx)
	}

	_ = runErr // 走到这里就是成功, deferred endRun 会写 runSpanStatusOK
	return response, nil
}

// ProcessMessageStream 跑一条消息并把中间事件 (文本增量/工具调用) 实时 emit 出去
// (sessionapi.StreamHost 的实现底座, design 03 §G1)。
//
// 事件映射刻意留在**这里** (types.StreamEvent → sessionapi.Event): feishu 与
// sessionapi 的接缝就是这条适配, 让 sessionapi 反向 import types 会把"协议类型"
// 与"引擎内部表示"焊死, 之后引擎改枚举值就变成线上事故。
func (sm *SessionManager) ProcessMessageStream(ctx context.Context, chatID, userText string, emit func(sessionapi.Event)) (string, error) {
	return sm.runSessionMessageStream(ctx, sm.GetOrCreate(chatID), userText, emit)
}

func (sm *SessionManager) runSessionMessageStream(ctx context.Context, session *Session, userText string, emit func(sessionapi.Event)) (string, error) {
	if session.IsProcessing() {
		return "上一条消息还在处理中，请稍候...", nil
	}

	// 与 runSessionMessage 同一套收口纪律: trace 作用域 + SetProcessing 都在
	// defer 里, 无论从哪条路径退出都会走一遍。
	var runErr error
	ctx, endRun := sm.beginSessionRun(ctx, session)
	defer func() {
		status := runSpanStatusOK
		if runErr != nil {
			status = runSpanStatusFailed
		}
		endRun(status, errTextOf(runErr))
	}()

	session.SetProcessing(true)
	defer session.SetProcessing(false)
	session.Touch()
	// 工具档位: spec 钉死 (ToolProfile) 的会话按钉死的来, 不允许按消息文本漂移;
	// 没钉的维持原启发式 (inferFeishuToolProfile)。
	if pinned, ok := sm.pinnedProfileFor(session.ChatID); ok {
		sm.configureSessionTools(session, pinned)
	} else {
		sm.configureSessionTools(session, resolveSessionToolProfile(userText))
	}

	streamCh := session.Engine.SubmitStream(ctx, userText)
	var response strings.Builder
	for ev := range streamCh {
		if emit != nil {
			if mapped := streamEventToAPI(ev); mapped.Kind != "" {
				emit(mapped)
			}
		}
		// 回复累积只认**非 thinking** 的 delta (与 consumeStreamEvents 的 quiet
		// 模式同口径): thinking 是过程, 不是回复。
		if ev.Kind == types.StreamEventDelta && !ev.IsThinking {
			response.WriteString(ev.DeltaText)
		}
	}

	out := response.String()
	if strings.TrimSpace(out) == "" {
		out = "(无回复内容)"
	}

	if sm.memoryStore != nil && out != "(无回复内容)" {
		sm.memoryStore.Add(&memory.MemoryEntry{
			Content:    truncateForDream(out),
			Source:     "extraction",
			Importance: 0.6,
			ChatID:     session.ChatID,
			Topics:     extractTopics(out),
		})
	}
	if sm.dreamer != nil {
		sm.dreamer.RecordSession(dreaming.SessionRecord{
			ChatID:  session.ChatID,
			EndTime: time.Now(),
			Summary: truncateForDream(out),
		})
		sm.dreamer.AfterQuery(ctx)
	}

	_ = runErr
	return out, nil
}

// streamEventToAPI 把引擎内部事件映射成 sessionapi 的线上协议类型。
// Error 接口在这里字符串化 —— error 过不了 JSON。
func streamEventToAPI(ev types.StreamEvent) sessionapi.Event {
	out := sessionapi.Event{At: time.Now()}
	switch ev.Kind {
	case types.StreamEventDelta:
		if ev.IsThinking {
			out.Kind = sessionapi.EventThinking
		} else {
			out.Kind = sessionapi.EventDelta
		}
		out.Text = ev.DeltaText
	case types.StreamEventToolStart:
		out.Kind = sessionapi.EventToolStart
		out.Tool = ev.ToolName
		out.Input = ev.ToolInput
	case types.StreamEventToolDone:
		out.Kind = sessionapi.EventToolDone
		out.Tool = ev.ToolName
		out.Result = ev.ToolResult
	case types.StreamEventError:
		// 运行中错误原样透出为 error 事件; run 的**终态**哨兵由 sessionapi 在
		// goroutine 收口处统一追加 (它知道最终是 completed/failed/cancelled,
		// 这里不知道), 两处都发 run_done 会出现两个"结束"。
		out.Kind = sessionapi.EventError
		if ev.Error != nil {
			out.Error = ev.Error.Error()
		}
	default:
		// BlockDone / MessageDone 对订阅方没有信息量 (MessageDone 的全文
		// 已经由 delta 流过了), 折叠成一个 delta 空事件不如不发。
		return sessionapi.Event{}
	}
	return out
}

// truncateForDream 截断响应用于 Dreaming 记录
func truncateForDream(s string) string {
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

// extractTopics 从文本中提取主题关键词 (简单实现)
func extractTopics(text string) []string {
	keywords := map[string]bool{
		"api": true, "database": true, "auth": true, "test": true,
		"bug": true, "error": true, "config": true, "deploy": true,
		"file": true, "function": true, "class": true, "module": true,
		"security": true, "performance": true, "refactor": true,
	}
	lower := strings.ToLower(text)
	var topics []string
	seen := make(map[string]bool)
	for word := range keywords {
		if strings.Contains(lower, word) && !seen[word] {
			topics = append(topics, word)
			seen[word] = true
		}
	}
	if len(topics) > 5 {
		topics = topics[:5]
	}
	return topics
}

// CreateAgentRunner 为 Agent Teams 提供的工厂方法。
// 创建一个独立的 QueryEngine 作为后台 Agent 执行器。
// 实现 agent.CreateAgentFunc 签名, 由 ProductionTeamManager 调用。
func (sm *SessionManager) CreateAgentRunner(ctx context.Context, role, systemPrompt string) (agent.AgentRunner, error) {
	r := &sessionAgentRunner{sm: sm, role: role, systemPrompt: systemPrompt}
	if mcfg, ok := ctx.Value(agent.ModelConfigKey{}).(modelconfig.ResolvedConfig); ok && mcfg.ProviderName != "" {
		r.resolvedCfg = mcfg
	}
	r.runMeta = agent.RunMetadataFromContext(ctx)
	// 图节点的显式执行提示 (design/01 §4.1 AgentSpec.ToolProfile/MaxTurns)。
	// 必须在**创建时**捕获: Execute 时的 ctx 已被 runAgent 换过, 拿不到。
	r.hints = agent.NodeExecHintsFromContext(ctx)
	return r, nil
}

// mapExplicitToolProfile 把图节点声明的 tool_profile 字符串映射为内部档位。
// 无法识别时返回空串, 由调用方回落到角色名推断——**未知值不应导致降权或报错**,
// 那会让一个拼错的声明静默剥掉 agent 的工具。
func mapExplicitToolProfile(s string) builtin.ToolProfile {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "chat":
		return builtin.ToolProfileChat
	case "research":
		return builtin.ToolProfileResearch
	case "coding":
		return builtin.ToolProfileCoding
	case "team":
		return builtin.ToolProfileTeam
	case "analysis":
		return builtin.ToolProfileAnalysis
	case "admin":
		return builtin.ToolProfileAdmin
	default:
		return ""
	}
}

// CanInjectMidRun / InjectMidRun 实现 agent.MidRunMailInjector (21.8.12 P5)。
//
// 状态放在 runner 上而不是让它返回引擎: 引擎在 Execute 的**局部作用域**里 (每次
// Execute 新建一个), 返回出去等于把一个 run 内的生命期交给调用方管 —— 调用方漏掉
// 清理就是一条永远不释放的引用。而中继要的只是"往当前这次运行塞一句话", 两个方法
// 刚好够, 生命期仍由 Execute 自己管 (defer 里清空指针)。
//
// 这把锁是必须的: 中继在**另一个 goroutine** 上跑, 裸读 r.eng 就是数据竞态。
func (r *sessionAgentRunner) CanInjectMidRun() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.eng != nil && r.eng.Running()
}

func (r *sessionAgentRunner) InjectMidRun(text string) bool {
	r.mu.Lock()
	eng := r.eng
	r.mu.Unlock()
	if eng == nil {
		return false
	}
	return eng.InjectMessage(midRunMailMessage(text))
}

// midRunMailMessage 把一段注入正文包成 user 消息。
//
// 包在这里**只有一处**, 两条路径 (feishu 的 sessionAgentRunner / CLI 的 cliAgentRunner)
// 共用 —— 各写一份的话, 差别会落在 role、UUID 之类的字段上, 而 role 写错是**静默降级**
// (InjectQueue 按 role 拒, 调用方只看到"没送出去")。
func midRunMailMessage(text string) types.Message {
	return types.Message{
		Type:      types.MessageTypeUser,
		UUID:      internal_hook.GenerateUUID(),
		CreatedAt: time.Now(),
		Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: text}},
	}
}

// sessionAgentRunner 基于 SessionManager 的 Agent 执行器
type sessionAgentRunner struct {
	sm           *SessionManager
	role         string
	systemPrompt string
	resolvedCfg  modelconfig.ResolvedConfig // 从创建时 context 捕获的 plan/role 模型配置
	runMeta      agent.RunMetadata
	hints        agent.NodeExecHints // 图节点显式声明的 ToolProfile/MaxTurns (空 = 回落既有推断)

	// mu 保护 eng。中继 (agent 包的 startMidRunMailRelay) 在另一个 goroutine 上
	// 读它, 而 Execute 在起/收时写它 —— 不锁就是数据竞态。
	mu  sync.Mutex
	eng *engine.QueryEngine // 仅在 Execute 进行中非 nil (21.8.12 P5)
}

// Execute 执行 agent 任务 (创建独立 QueryEngine, 复用主会话运行模式)。
// 集成: Role Skills + Evolution 经验 + Dreaming 记录 (通过 Hook 注入)。
func (r *sessionAgentRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	// 优先使用创建时捕获的 plan/role 模型配置 (Orchestrator 阶段 Execute ctx 与创建 ctx 不同)
	apiClient := r.sm.apiClient
	modelOverride := r.sm.apiClient.Model
	maxTokens := r.sm.defaultResolved.MaxTokens
	maxTurns := r.sm.defaultResolved.MaxTurns
	contextWindow := contextWindowOrDefault(r.sm.defaultResolved.ContextWindow)
	promptCacheMode := r.sm.config.PromptCacheMode
	mcfg := r.resolvedCfg
	if mcfg.ProviderName == "" {
		// 回退: 尝试从执行时 context 读取 (非 Orchestrator 路径)
		if mcfg2, ok := ctx.Value(agent.ModelConfigKey{}).(modelconfig.ResolvedConfig); ok && mcfg2.ProviderName != "" {
			mcfg = mcfg2
		}
	}
	if mcfg.ProviderName != "" {
		// 出站模型名走 OutboundModelName —— 只发裸厂商模型名会让**经网关**的
		// 请求落到默认池; 但**直连厂商**时反而是发前缀会被 401 拒。两者的分界
		// 就是这条链路走不走网关, 见 ResolvedConfig.OutboundModelName。
		modelName := mcfg.OutboundModelName()
		apiClient = r.sm.apiClient.ConfiguredCloneFull(
			mcfg.BaseURL, mcfg.APIKey, modelName, mcfg.FallbackModels,
			mcfg.FallbackBaseURL, mcfg.FallbackAPIKey,
		)
		modelOverride = modelName
		if mcfg.MaxTokens > 0 {
			maxTokens = mcfg.MaxTokens
		}
		if mcfg.MaxTurns > 0 {
			maxTurns = mcfg.MaxTurns
		}
		if mcfg.ContextWindow > 0 {
			contextWindow = mcfg.ContextWindow
		}
		if mcfg.PromptCacheMode != "" {
			promptCacheMode = mcfg.PromptCacheMode
		}
	}
	if promptCacheMode != "" {
		apiClient.PromptCacheMode = promptCacheMode
	}

	// H4-3 任务时程分层: 长程任务 (产码/测试/修复) 的回合上限由
	// .ai.longHorizonMaxTurns 给, 常规任务仍取 .ai.maxTurns。
	//
	// 优先级 (每一条都必须能独立解释为什么在这个位置):
	//   1. 图节点显式声明的 MaxTurns —— 最具体的声明。此前这个字段**被捕获但
	//      从来没有被读过** (r.hints 只用了 ToolProfile), 于是节点级声明的回合数
	//      静默失效。这里把它接上。
	//   2. 模型级 maxTurns (mcfg.MaxTurns>0) —— 按具体模型写死的值是有意为之
	//      (kimi 系 40 就是因为那个模型回合开销与别人不同), 不该被档位覆盖。
	//   3. 档位 —— 只在上面两条都没声明时生效。
	//
	// 第 3 条的取值判据是 "比常规档**大**才覆盖": 配置里没写 longHorizonMaxTurns
	// (或写得比 maxTurns 还小) 时, maxTurns 一个字节都不动 —— 于是"没开分层"与
	// "改造前"逐字相同, 这次改动不会在没人配置的情况下改掉任何一个任务的止损线。
	maxTurns = r.resolveStageMaxTurns(mcfg.MaxTurns, maxTurns)

	// 工具画像: **显式声明优先于角色名推断**, 决策收敛到 ConstraintSet 单一真源
	// (design/01 §4.6)。
	// profileForTeamRole 是按角色名子串匹配的 (session.go 上方), 那套推断有真实
	// 误判——例如 world-builder 因含 "build" 被判成 Coding 档而拿到 Shell。
	// 图节点若声明了 tool_profile 就用它; 没声明时**行为与改造前逐项等价**
	// (仍走 profileForTeamRole), 只多一条 deprecation 痕迹 —— 6 个下游平台的
	// 现行行为一点不变。
	teamRole := firstNonEmpty(r.runMeta.Role, r.role)
	constraints := stageConstraints(ctx, r.hints, r.runMeta.Stage, teamRole)
	prof := resolveToolProfile(constraints, agent.ProfileSourceRoleNameFallback, func() builtin.ToolProfile {
		return profileForTeamRole(teamRole, r.runMeta.Workflow)
	})
	nestedReg := r.sm.newProfileRegistry(prof, registryOptions{})
	dropInteractiveOnlyTools(nestedReg)

	permMode := types.PermissionMode(r.sm.config.PermissionMode)
	permChecker := permissions.NewChecker(permMode)
	// 带观测 ctx (design/01 §4.5 ExternalHook 适配器)。**这是生产的主产生方**:
	// 图路径的每个 agent 节点都经 CreateAgentRunner 走到这里, 而这个 Runner 是
	// per-Execute 构造的 ⇒ 观测 ctx 天然是 per-run 的, 不会串台 (hooks/bus.go 边界 2)。
	// ctx 上没有 hooks.Observer 时 (非图模式 / 灰度未开) 逐字节等价于 NewRunner。
	//
	// 阶段声明的 hook (21.8.12 P4/X1) 追加在配置 hook 之后 —— 与 runNestedAgent 那条
	// 路径**逐字同一套规则** (三段来源: 配置 / 资产 / 阶段, 后两者都只追加不替换)。
	// 阶段声明只影响本阶段这一次 Execute: hookRunner 是 per-Execute 构造的, 装进来的
	// 阶段 hook 不会渗到下一个阶段去。
	hookConfigs := r.sm.hookConfigs
	if decl := agent.StageDeclFromContext(ctx); len(decl.Hooks) > 0 {
		stageCfgs, probs := hooks.ParseHookDecls(decl.Hooks)
		for _, p := range probs {
			log.Printf("[Hooks] 阶段声明的 hook 问题 (stage=%s): %s", r.runMeta.Stage, p)
		}
		if len(stageCfgs) > 0 {
			merged := make([]types.HookConfig, 0, len(hookConfigs)+len(stageCfgs))
			merged = append(merged, hookConfigs...)
			merged = append(merged, stageCfgs...)
			hookConfigs = merged
		}
	}
	hookRunner := hooks.NewRunnerWithContext(ctx, hookConfigs, "")
	if hookRunner != nil {
		hookOut := hookRunner.ExecuteSessionHooks(types.HookEventSessionStart)
		if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
			return "", fmt.Errorf("SessionStart blocked by hook: %s", hookOut.Reason)
		}
		hookOut = hookRunner.ExecuteSubagentStartHooks(r.role, userPrompt)
		if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
			return "", fmt.Errorf("SubagentStart blocked by hook: %s", hookOut.Reason)
		}
	}
	compactor := compact.NewCompactor(apiClient, contextWindow)
	promptMgr := prompt.NewManager(r.sm.config.Cwd)
	promptMgr.Model = modelOverride
	// 角色相关技能才列出; 其余走统一池按需检索 (见 roleScopedSkillListing 注释)。
	// 阶段声明的技能并入 (不替换角色技能) —— 见 stageSkillListing。
	promptMgr.SkillListing = r.sm.stageSkillListing(r.role, agent.StageDeclFromContext(ctx).Skills)

	// 角色提示词: 优先使用外部传入的, 再尝试从 RoleRegistry 获取 (含专属 Skills)
	effectivePrompt := r.systemPrompt
	if effectivePrompt == "" && r.sm.roleRegistry != nil {
		effectivePrompt = r.sm.roleRegistry.MergedPrompt(r.role, "", "")
	}
	if effectivePrompt != "" {
		promptMgr.CustomPrompt = effectivePrompt
	}

	// Hook 注入: Evolution 经验检索 (SessionStart 时注入到 PostSampling)
	if r.sm.evolution != nil {
		exps := r.sm.evolution.RetrieveFor(r.role, userPrompt, 3)
		if len(exps) > 0 {
			expContext := agent.FormatExperiencesForPrompt(exps)
			hookRunner.RegisterPostSamplingHook(func(_ []types.Message) {
				// 经验 ID 记录, 后续由 workflow/swarm 层面反馈
			})
			// 前缀缓存修复: 经验按 userPrompt 检索, 每次都不同。若前置到
			// CustomPrompt(=system block 0), 会让本应稳定的 system 前缀每次变化,
			// 自动前缀缓存彻底失效(实测 97% 角色 system 前缀不稳)。改为放入
			// AppendPrompt → 成为独立尾部 system 块, block 0(角色提示词)保持稳定可缓存。
			//
			// 去重: WorkflowEngine 组装 stage prompt 时也会检索并注入同一批经验
			// (pkg/agent/workflow.go 的 "注入进化经验" 段, 检索键同为 role), 而那份
			// stage prompt 就是这里的 userPrompt、最终作为 TaskInstruction 追加成
			// **另一个** system 块。两条路径互不知情, 于是同一段经验文本在请求里出现
			// 两次 —— 实测 system[1] 与 system[2] 前 1444B 逐字节相同。这里判一次
			// 包含关系: 已在 stage prompt 里就不再重复追加, 少一个块、也少一次
			// 无谓的重复 token。
			if !strings.Contains(userPrompt, expContext) {
				if promptMgr.AppendPrompt != "" {
					promptMgr.AppendPrompt = promptMgr.AppendPrompt + "\n" + expContext
				} else {
					promptMgr.AppendPrompt = expContext
				}
			}
		}
	}

	cfg := &engine.Config{
		Model:            modelOverride,
		ExecutionModel:   r.sm.config.ExecutionModel,
		MaxTokens:        maxTokens,
		MaxTurns:         clampTurns(maxTurns),
		ContextWindow:    contextWindow,
		Cwd:              r.sm.config.Cwd,
		PermissionMode:   permMode,
		IsNonInteractive: true,
		Debug:            r.sm.config.Debug,
		MetricsSource:    firstNonEmpty(r.runMeta.Source, "team_stage"),
		MetricsPurpose:   firstNonEmpty(r.runMeta.Purpose, r.runMeta.Team),
		Workflow:         r.runMeta.Workflow,
		Role:             firstNonEmpty(r.runMeta.Role, r.role),
		// 13.7-P1 池个性化身份: 团队 stage 的引擎配置带 Team, 池工具经 ToolContext
		// 拿到 (team, role, asset) 三元组记 trial。
		Team: r.runMeta.Team,
	}
	// 方案三: 弱模型 harness 增强装配 (与 CLI 同规则——env 显式 > provider=="ollama" 自动)。
	// kimi 等云端 provider 恒为关, 生产零变化; ollama 别名会话自动获得 L2/L3/L4/L6b/L8 护栏。
	cfg.WeakModel = weakmodel.Resolve(r.sm.defaultResolved.Provider, os.Getenv)
	// 同一个 ConstraintSet 既决定了工具档位 (上面 newProfileRegistry), 也在这里编译
	// 成引擎的名单/权限档 —— 这就是 §4.6 的"单一真源": 档位决策与 toolExposed 读的
	// 是同一份声明。当前团队角色约束只带档位、不带名单, 因此这一步对现有行为是
	// no-op, 只把来源链写进 cfg.ConstraintOrigin 供审计。
	applyConstraints(cfg, constraints)

	eng := engine.NewQueryEngine(cfg, apiClient, nestedReg, hookRunner, permChecker, compactor, promptMgr)
	// 运行中入站通道 (21.8.12 P5 / D6-3 缺口 2 级 2): 把本 run 的引擎交给中继。
	//
	// 赋值在**创建之后、SubmitMessage 之前** —— 早于创建会让 CanInjectMidRun 读到
	// 一个还没跑起来的引擎 (Running() 为假, 中继空转); 晚于 SubmitMessage 则整段
	// 都在竞态里。收尾用 defer 清空, 且**必须在** SubmitMessage 返回之后: 那时
	// Running() 已归零, 迟到的注入会被引擎自己按 role/running 拒掉, 而不是被这里
	// 抢在中间制造一个"指针在、引擎已收摊"的状态。
	r.mu.Lock()
	r.eng = eng
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.eng = nil
		r.mu.Unlock()
	}()
	eng.MemoryStore = r.sm.memoryStore
	eng.TraceStore = r.sm.traceStore // design/03 §4.1 E1: team stage 轨迹采集
	// 注: 这里**不**装配 H3-2 的 SetRunStateDir。本引擎的 Execute 走 SubmitMessage
	// (主循环), 从不调 RunIsolated —— 装了也永远不会被读到。那条路径的历史持久化
	// 是另一套机制 (SessionStore), 见 createSessionWithClient 里的 transcript;
	// 缺的是这一处 (nested runner 没有 SessionStore), 与 RunIsolated 无关。
	// 将任务描述从 user message 移到 system prompt 末尾，避免 msg[0] 膨胀
	// 同时让 system prompt 前缀享受 prompt caching。
	eng.TaskInstruction = userPrompt
	// 与主会话一致, 给团队 agent 也启用前沿优化:
	//   - PromptCache: 稳定前缀布局 (利于 Kimi 自动前缀缓存命中) —— A
	//   - Budget: 上下文超限时降级隐藏旧 tool_result, 不再每轮重发整文件 —— C
	//   - LoopDetector + StopSignal(CaRT): 检测同文件重复读/搜索收益递减, 注入收敛提示, 止住无脑搜索 —— D
	if r.sm.config.EnableFrontierOptimizations {
		eng.Config.EnableStopSignal = true // CaRT 停止信号 (EnableFrontierOptimizations 不含, 需单独开)
		// 配置显式声明时以配置为准 (含显式 false) —— 本路径的 true 只是**缺省值**,
		// 不是不可覆盖的硬编码。这样三条路径 (CLI / 飞书主会话 / 飞书团队) 的差异
		// 全部收敛到一个开关上, 而不是散在三处各自的 if 里。
		if r.sm.config.EnableStopSignal != nil {
			eng.Config.EnableStopSignal = *r.sm.config.EnableStopSignal
		}
		eng.EnableFrontierOptimizations()
	}

	start := time.Now()
	var sb strings.Builder
	var hasApiError bool
	for msg := range eng.SubmitMessage(ctx, "") {
		if msg.Type != types.MessageTypeAssistant {
			continue
		}
		// 检测是否包含 API 错误消息 (限流/超时等导致的 withheld error)
		if msg.IsApiErrorMessage {
			hasApiError = true
		}
		for _, b := range msg.Content {
			if b.Type == types.ContentBlockText {
				sb.WriteString(b.Text)
			}
		}
	}
	result := sb.String()

	// V2 关键修复: context 被取消且产出为空/过短时, 返回 API 错误而非空产出。
	// 否则限流/超时导致的空产出会被 validateAgentOutput 标记为"产出验证失败"(永久错误),
	// 不会触发自动重试, 导致团队持续失败。
	if ctx.Err() != nil {
		if hasApiError {
			return result, fmt.Errorf("API 调用被中断 (限流/超时/熔断): %w", ctx.Err())
		}
		if len(result) < 100 {
			return result, fmt.Errorf("context 取消且产出不完整 (%d 字符): %w", len(result), ctx.Err())
		}
	}

	// Hook: Dreaming 记录 (覆盖 team agent 会话)
	if r.sm.dreamer != nil && result != "" {
		r.sm.dreamer.RecordSession(dreaming.SessionRecord{
			ChatID:  "agent:" + r.role,
			EndTime: time.Now(),
			Summary: truncateForDream(result),
		})
	}

	// Hook: 记忆提取 (复用主会话的 Episodic Memory 逻辑)
	if r.sm.memoryStore != nil && result != "" {
		r.sm.memoryStore.Add(&memory.MemoryEntry{
			Content:    truncateForDream(result),
			Source:     "agent:" + r.role,
			Importance: 0.5,
			Topics:     extractTopics(result),
		})
	}

	_ = start // used by evolution trajectory in workflow layer
	if hookRunner != nil {
		hookOut := hookRunner.ExecuteSubagentStopHooks(r.role, result)
		if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
			// SubagentStop blocked: still return result but log the reason
		}
		hookRunner.ExecuteSessionHooks(types.HookEventSessionEnd)
	}
	return result, nil
}

// extractMessageText 从 Message 中提取文本内容
func extractMessageText(msg types.Message) string {
	var text string
	if msg.Type == types.MessageTypeAssistant {
		for _, block := range msg.Content {
			switch block.Type {
			case types.ContentBlockText:
				text += block.Text
			case types.ContentBlockToolUse:
				// 工具调用 - 不直接展示给用户
			}
		}
	}
	return text
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
