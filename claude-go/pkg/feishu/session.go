package feishu

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
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

	// Advisor 顾问工具 (设计文档 docs/advisor-tool-design.md)
	advisorClient *api.Client                     // advisor 模型专用客户端 (nil=禁用)
	advisorTools  map[string]*builtin.AdvisorTool // chatID → 工具实例 (预算按会话隔离, profile 切换不重置)
	advisorMu     sync.Mutex
}

// NewSessionManager 创建会话管理器。
// 所有共享组件由 Bot 创建并传入。
func NewSessionManager(config *BotConfig, apiClient *api.Client, mcpMgr *dynmcp.Manager, skillReg *skills.Registry, dreamer *dreaming.Dreamer, memStore *memory.TieredStore, hookConfigs []types.HookConfig, taskStore *builtin.TaskStore, evolution *agent.EvolutionEngine, roleRegistry *agent.RoleRegistry) *SessionManager {
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

	// TraceStore 轨迹底座 (design/03 §4.1 E1): 各 stage 隔离引擎共享一个 Store,
	// 落 <state>/statestore/。stateDir 从 config.Cwd 推导 (与 basedir 约定一致)。
	if stateDir := strings.TrimSpace(config.StateDir); stateDir != "" {
		sm.traceStore = tracestore.New(statestore.NewFileStore(filepath.Join(stateDir, "statestore")))
	} else if config.Cwd != "" {
		sm.traceStore = tracestore.New(statestore.NewFileStore(filepath.Join(config.Cwd, ".claude-go", "statestore")))
	}

	// 启动后台清理 goroutine
	go sm.cleanupLoop()

	return sm
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

// SetTeamManager 注入团队管理器，让 LLM 能通过 TeamQuery 工具查询团队信息。
func (sm *SessionManager) SetTeamManager(mgr *agent.ProductionTeamManager) {
	sm.teamMgr = mgr
}

// SetSearcher 注入 Web 搜索适配器，让 WebSearchTool 能发起真实搜索。
func (sm *SessionManager) SetSearcher(s builtin.WebSearcher) {
	sm.searcher = s
}

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
	if opts.includeAgent && opts.runAgentFn != nil {
		reg.Register(agent.NewAgentTool(opts.runAgentFn))
	}
	// Advisor 顾问工具: 仅主会话 (chatID 非空) 注册; 嵌套 agent 不注册避免预算翻倍。
	if advTool := sm.advisorToolFor(opts.chatID); advTool != nil {
		reg.Register(advTool)
	}
	return reg
}

func shortSkillListing(reg *skills.Registry) string {
	if reg == nil || reg.Count() == 0 {
		return ""
	}
	// 0 = 不设软上限, 由描述字符预算约束; 所有技能名称始终可见 (可发现性)。
	return reg.FormatShortListing(0)
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
	compactor := compact.NewCompactor(sm.apiClient, contextWindow)
	promptMgr := prompt.NewManager(sm.config.Cwd)
	if sm.config.SystemPrompt != "" {
		promptMgr.CustomPrompt = sm.config.SystemPrompt
	}
	promptMgr.Model = sm.apiClient.Model
	promptMgr.SkillListing = shortSkillListing(sm.skillReg)
	promptMgr.ProductName = "Claude Code (Go) - Feishu Bot"
	promptMgr.HookConfigs = sm.hookConfigs
	if sm.dreamer != nil {
		promptMgr.DreamMemoryDir = sm.dreamer.Stats().MemoryDir
	}

	cfg := &engine.Config{
		Model:            sm.apiClient.Model,
		MaxTokens:        sm.defaultResolved.MaxTokens,
		MaxTurns:         clampTurns(sm.defaultResolved.MaxTurns),
		ContextWindow:    contextWindow,
		Cwd:              sm.config.Cwd,
		PermissionMode:   permMode,
		IsNonInteractive: true, // 飞书模式始终为非交互式
		Debug:            sm.config.Debug,
		MetricsSource:    "feishu_main",
		MetricsPurpose:   chatID,
		DynamicPlanCheck: builtin.PlanModeActive,
		// 飞书会话中禁止 LLM 自主调用团队内部工具。
		// 团队操作只能通过 /team、/go 命令触发。
		// TeamMailbox/TeamCreate/TeamDelete 是团队内部 Agent 间通信工具，
		// 在飞书对话中无意义，且会被 LLM 误用（如把 TeamMailbox 当成"发消息给用户"）。
		DisabledTools: map[string]bool{
			"TeamCreate":  true,
			"TeamDelete":  true,
			"TeamMailbox": true,
		},
	}

	// Advisor push 模式 (Phase 3): checkpoint hook 与 pull 模式共享同一工具实例的预算
	if advTool := sm.advisorToolFor(chatID); advTool != nil && sm.config.Advisor != nil {
		if sm.config.Advisor.CheckpointEveryTurns > 0 || sm.config.Advisor.CheckpointOnLoop {
			cfg.AdvisorConsultFn = advTool.Consult
			cfg.AdvisorCheckpointEveryTurns = sm.config.Advisor.CheckpointEveryTurns
			cfg.AdvisorCheckpointOnLoop = sm.config.Advisor.CheckpointOnLoop
		}
	}

	eng := engine.NewQueryEngine(cfg, sm.apiClient, reg, hookRunner, permChecker, compactor, promptMgr)
	eng.MemoryStore = sm.memoryStore
	eng.TraceStore = sm.traceStore // design/03 §4.1 E1
	if sm.config.EnableFrontierOptimizations {
		eng.EnableFrontierOptimizations()
	}

	return &Session{
		ChatID:      chatID,
		Engine:      eng,
		LastActive:  time.Now(),
		ToolProfile: profile,
	}
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

	hookRunner := hooks.NewRunner(sm.hookConfigs, "")
	compactor := compact.NewCompactor(nestedAPIClient, contextWindow)
	promptMgr := prompt.NewManager(sm.config.Cwd)
	promptMgr.Model = nestedModel
	promptMgr.SkillListing = shortSkillListing(sm.skillReg)

	model := nestedModel
	if opts.Model != "" {
		model = opts.Model
	}
	runMeta := agent.RunMetadataFromContext(ctx)
	nestedPurpose := firstNonEmpty(opts.SubagentType, runMeta.Purpose, runMeta.Team)
	nestedReg := sm.newProfileRegistry(profileForRunOptions(opts, runMeta), registryOptions{})

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
	}

	nested := engine.NewQueryEngine(cfg, nestedAPIClient, nestedReg, hookRunner, permChecker, compactor, promptMgr)

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

// evictOldest 淘汰最早活跃的会话 (需在锁内调用)
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

// cleanup 清理超时会话
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

// ClearSession 清除指定会话 (用于 /clear 命令)
func (sm *SessionManager) ClearSession(chatID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, chatID)
	sm.dropAdvisorTool(chatID)
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
	session := sm.GetOrCreate(chatID)

	if session.IsProcessing() {
		return "上一条消息还在处理中，请稍候...", nil
	}

	session.SetProcessing(true)
	defer func() {
		session.SetProcessing(false)
		// 自动消费队列中的下一条消息
		if pendingText, pendingReply := session.DequeuePending(); pendingText != nil && pendingReply != nil {
			go func() {
				resp, err := sm.processMessageInternal(context.Background(), chatID, *pendingText)
				if err != nil {
					pendingReply(fmt.Sprintf("处理排队消息失败: %v", err))
				} else {
					pendingReply(resp)
				}
			}()
		}
	}()
	session.Touch()
	sm.configureSessionTools(session, inferFeishuToolProfile(userText))

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
			ChatID:     chatID,
			Topics:     extractTopics(response),
		})
	}

	// 触发 Dreaming 检查 (对应 TS: stopHooks.ts → executeAutoDream)
	if sm.dreamer != nil {
		sm.dreamer.RecordSession(dreaming.SessionRecord{
			ChatID:  chatID,
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
	return r, nil
}

// sessionAgentRunner 基于 SessionManager 的 Agent 执行器
type sessionAgentRunner struct {
	sm           *SessionManager
	role         string
	systemPrompt string
	resolvedCfg  modelconfig.ResolvedConfig // 从创建时 context 捕获的 plan/role 模型配置
	runMeta      agent.RunMetadata
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

	nestedReg := r.sm.newProfileRegistry(profileForTeamRole(firstNonEmpty(r.runMeta.Role, r.role), r.runMeta.Workflow), registryOptions{})

	permMode := types.PermissionMode(r.sm.config.PermissionMode)
	permChecker := permissions.NewChecker(permMode)
	hookRunner := hooks.NewRunner(r.sm.hookConfigs, "")
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
	promptMgr.SkillListing = shortSkillListing(r.sm.skillReg)

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
	}

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
