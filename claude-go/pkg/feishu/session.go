package feishu

import (
	"context"
	"os"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/complexity"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/engine"
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
	"github.com/anthropic/claude-go/pkg/weakmodel"
	"github.com/anthropic/claude-go/pkg/types"
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

	// 消息队列: 当 processing=true 时, 后续消息入队等待, 处理完自动消费
	pendingMsg   *string      // 最多缓存 1 条待处理消息 (最新的覆盖旧的)
	pendingReply func(string) // 队列消息的回复回调

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

	// stateStore 进程内唯一的状态存储实例 (由 Bot 注入, 见 WithStateStore)。
	// 承载: TraceStore 轨迹底座 (design/03 §4.1 E1) + 会话历史快照 (design/02 R2)。
	stateStore statestore.StateStore

	// Advisor 顾问工具 (设计文档 docs/advisor-tool-design.md)
	advisorClient *api.Client                     // advisor 模型专用客户端 (nil=禁用)
	advisorTools  map[string]*builtin.AdvisorTool // chatID → 工具实例 (预算按会话隔离, profile 切换不重置)
	advisorMu     sync.Mutex

	// planClient 规划期独立模型客户端 (Tag=plan, nil=禁用)。plan flag ON (规划期) 时
	// createSession 把该客户端塞进 engine.Config.PlanClient, queryLoop 全程路由到它,
	// 类似 advisor 独立客户端。由 Bot 初始化时经 SetPlanClient 注入。
	planClient *api.Client
	planMu     sync.Mutex
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

// SetDefaultModelConfig 注入默认模型解析配置。
func (sm *SessionManager) SetDefaultModelConfig(cfg modelconfig.ResolvedConfig) {
	sm.defaultResolved = cfg
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
	sm.roleRegistry = agent.NewRoleRegistry(cwd)
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

func (sm *SessionManager) newProfileRegistry(profile builtin.ToolProfile, opts registryOptions) *tool.Registry {
	profile = builtin.NormalizeToolProfile(profile)
	reg := tool.NewRegistry()
	builtin.RegisterProfileToolsWithStore(reg, sm.taskStore, sm.searcher, profile)

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
		pool := builtin.RegisterPoolTools(reg, sm.skillReg, nil, nil, pp)
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
// TeamMailbox/TeamCreate/TeamDelete 是团队内部 Agent 间通信工具, 在飞书对话中无
// 意义, 且会被 LLM 误用 (如把 TeamMailbox 当成"发消息给用户")。
// 区别只在于: 名单现在声明在 ConstraintSet 里 (可审计、可被子约束继续收窄),
// 执行点仍然是 engine 的 toolExposed。
func feishuSessionConstraints() *agent.ConstraintSet {
	return agent.NewConstraintSet("feishu-session").
		WithDeniedTools("TeamCreate", "TeamDelete", "TeamMailbox")
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
	profile := builtin.ToolProfileChat
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
	promptMgr := prompt.NewManager(sm.config.Cwd)
	if sm.config.SystemPrompt != "" {
		promptMgr.CustomPrompt = sm.config.SystemPrompt
	}
	promptMgr.Model = cli.Model
	promptMgr.SkillListing = shortSkillListingInDir(sm.skillReg, sm.config.Cwd)
	promptMgr.ProductName = "Claude Code (Go) - Feishu Bot"
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
		Cwd:              sm.config.Cwd,
		PermissionMode:   permMode,
		IsNonInteractive: true, // 飞书模式始终为非交互式
		Debug:            sm.config.Debug,
		MetricsSource:    "feishu_main",
		MetricsPurpose:   chatID,
		SessionID:        chatID, // EnterPlanMode/ExitPlanMode 写会话级 plan flag
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
	eng.MemoryStore = sm.memoryStore
	eng.TraceStore = sm.traceStore // design/03 §4.1 E1
	if sm.config.EnableFrontierOptimizations {
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
		nestedAPIClient = sm.apiClient.ConfiguredCloneFull(
			mcfg.BaseURL, mcfg.APIKey, mcfg.ProviderName, mcfg.FallbackModels,
			mcfg.FallbackBaseURL, mcfg.FallbackAPIKey,
		)
		nestedModel = mcfg.ProviderName
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
	hookRunner := hooks.NewRunnerWithContext(ctx, sm.hookConfigs, "")
	compactor := compact.NewCompactor(nestedAPIClient, contextWindow)
	promptMgr := prompt.NewManager(sm.config.Cwd)
	promptMgr.Model = nestedModel
	promptMgr.SkillListing = shortSkillListingInDir(sm.skillReg, sm.config.Cwd)

	model := nestedModel
	if opts.Model != "" {
		model = opts.Model
	}
	runMeta := agent.RunMetadataFromContext(ctx)
	nestedPurpose := firstNonEmpty(opts.SubagentType, runMeta.Purpose, runMeta.Team)

	// 工具画像: 同样经 ConstraintSet 决策 (design/01 §4.6)。显式声明来自外层图节点
	// (ctx 里的 NodeExecHints), 但只能"取更窄的那个" —— 理由见 nestedExplicitProfile。
	nestedFallbackProfile := profileForRunOptions(opts, runMeta)
	nestedConstraints := agent.NewConstraintSet("nested-agent:" + firstNonEmpty(opts.SubagentType, runMeta.Role, "anonymous"))
	nestedConstraints.WithToolProfile(string(nestedExplicitProfile(
		mapExplicitToolProfile(agent.NodeExecHintsFromContext(ctx).ToolProfile),
		nestedFallbackProfile,
	)))
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
	sm.configureSessionTools(session, resolveSessionToolProfile(userText))

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

	return response, nil
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

// sessionAgentRunner 基于 SessionManager 的 Agent 执行器
type sessionAgentRunner struct {
	sm           *SessionManager
	role         string
	systemPrompt string
	resolvedCfg  modelconfig.ResolvedConfig // 从创建时 context 捕获的 plan/role 模型配置
	runMeta      agent.RunMetadata
	hints        agent.NodeExecHints // 图节点显式声明的 ToolProfile/MaxTurns (空 = 回落既有推断)
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
		apiClient = r.sm.apiClient.ConfiguredCloneFull(
			mcfg.BaseURL, mcfg.APIKey, mcfg.ProviderName, mcfg.FallbackModels,
			mcfg.FallbackBaseURL, mcfg.FallbackAPIKey,
		)
		modelOverride = mcfg.ProviderName
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

	// 工具画像: **显式声明优先于角色名推断**, 决策收敛到 ConstraintSet 单一真源
	// (design/01 §4.6)。
	// profileForTeamRole 是按角色名子串匹配的 (session.go 上方), 那套推断有真实
	// 误判——例如 world-builder 因含 "build" 被判成 Coding 档而拿到 Shell。
	// 图节点若声明了 tool_profile 就用它; 没声明时**行为与改造前逐项等价**
	// (仍走 profileForTeamRole), 只多一条 deprecation 痕迹 —— 6 个下游平台的
	// 现行行为一点不变。
	teamRole := firstNonEmpty(r.runMeta.Role, r.role)
	constraints := agent.NewConstraintSet("team-role:" + teamRole).
		WithToolProfile(string(mapExplicitToolProfile(r.hints.ToolProfile)))
	prof := resolveToolProfile(constraints, agent.ProfileSourceRoleNameFallback, func() builtin.ToolProfile {
		return profileForTeamRole(teamRole, r.runMeta.Workflow)
	})
	nestedReg := r.sm.newProfileRegistry(prof, registryOptions{})

	permMode := types.PermissionMode(r.sm.config.PermissionMode)
	permChecker := permissions.NewChecker(permMode)
	// 带观测 ctx (design/01 §4.5 ExternalHook 适配器)。**这是生产的主产生方**:
	// 图路径的每个 agent 节点都经 CreateAgentRunner 走到这里, 而这个 Runner 是
	// per-Execute 构造的 ⇒ 观测 ctx 天然是 per-run 的, 不会串台 (hooks/bus.go 边界 2)。
	// ctx 上没有 hooks.Observer 时 (非图模式 / 灰度未开) 逐字节等价于 NewRunner。
	hookRunner := hooks.NewRunnerWithContext(ctx, r.sm.hookConfigs, "")
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
	promptMgr.SkillListing = shortSkillListingInDir(r.sm.skillReg, r.sm.config.Cwd)

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
			if promptMgr.AppendPrompt != "" {
				promptMgr.AppendPrompt = promptMgr.AppendPrompt + "\n" + expContext
			} else {
				promptMgr.AppendPrompt = expContext
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
	eng.MemoryStore = r.sm.memoryStore
	eng.TraceStore = r.sm.traceStore // design/03 §4.1 E1: team stage 轨迹采集
	// 将任务描述从 user message 移到 system prompt 末尾，避免 msg[0] 膨胀
	// 同时让 system prompt 前缀享受 prompt caching。
	eng.TaskInstruction = userPrompt
	// 与主会话一致, 给团队 agent 也启用前沿优化:
	//   - PromptCache: 稳定前缀布局 (利于 Kimi 自动前缀缓存命中) —— A
	//   - Budget: 上下文超限时降级隐藏旧 tool_result, 不再每轮重发整文件 —— C
	//   - LoopDetector + StopSignal(CaRT): 检测同文件重复读/搜索收益递减, 注入收敛提示, 止住无脑搜索 —— D
	if r.sm.config.EnableFrontierOptimizations {
		eng.Config.EnableStopSignal = true // CaRT 停止信号 (EnableFrontierOptimizations 不含, 需单独开)
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
