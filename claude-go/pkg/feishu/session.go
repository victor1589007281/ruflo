package feishu

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

// Session 单个飞书会话。
// 每个 chat_id 对应一个独立的 Session，包含:
//   - 独立的 QueryEngine (维护对话历史)
//   - 独立的工具注册表
//   - 活跃时间追踪 (用于超时清理)
//
// 对应概念: 类似 Claude Code 中每个 terminal tab 的独立会话。
type Session struct {
	ChatID     string
	Engine     *engine.QueryEngine
	LastActive time.Time
	mu         sync.Mutex
	processing bool // 是否正在处理消息 (防止并发请求)
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
// MediaSendFunc 飞书媒体发送回调（图片/文件）。
type MediaSendFunc func(ctx context.Context, chatID string, data []byte, filename, mediaType string) error

type SessionManager struct {
	sessions       map[string]*Session
	mu             sync.RWMutex
	config         *BotConfig
	apiClient      *api.Client
	maxSessions    int
	sessionTimeout time.Duration
	mcpMgr         *dynmcp.Manager
	skillReg       *skills.Registry
	dreamer        *dreaming.Dreamer
	memoryStore    *memory.TieredStore
	hookConfigs    []types.HookConfig
	taskStore      *builtin.TaskStore     // 共享 V2 Task 存储 (Teams + LLM 工具共用)
	evolution      *agent.EvolutionEngine // 进化引擎 (注入 agent runner hooks)
	roleRegistry   *agent.RoleRegistry    // 角色注册表
	mediaSendFn    MediaSendFunc          // 飞书发送图片/文件的回调
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

	// 启动后台清理 goroutine
	go sm.cleanupLoop()

	return sm
}

// SetMediaSendFn 注入飞书媒体发送回调。在 Bot 初始化完成后调用。
func (sm *SessionManager) SetMediaSendFn(fn MediaSendFunc) {
	sm.mediaSendFn = fn
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
	reg := tool.NewRegistry()
	builtin.RegisterBaseToolsWithStore(reg, sm.taskStore)

	// 注册飞书发送工具: 让 LLM 能直接通过飞书 SDK 发送图片/文件给用户
	if sm.mediaSendFn != nil {
		reg.Register(NewFeishuSendFileTool(chatID, sm.mediaSendFn))
	}

	// 注册 MCP 工具 (动态, 对应 TS: assembleToolPool + refreshTools)
	if sm.mcpMgr != nil {
		sm.mcpMgr.RefreshToolsForRegistry(reg)
	}

	// 注册 Skill 工具 (如果有已加载技能)
	if sm.skillReg != nil && sm.skillReg.Count() > 0 {
		reg.Register(skills.NewSkillTool(sm.skillReg))
	}

	permMode := types.PermissionMode(sm.config.PermissionMode)
	permChecker := permissions.NewChecker(permMode)

	// Hook 配置 (从 JSON config 加载)
	hookRunner := hooks.NewRunner(sm.hookConfigs, "")

	compactor := compact.NewCompactor(sm.apiClient, 200000)
	promptMgr := prompt.NewManager(sm.config.Cwd)
	if sm.config.SystemPrompt != "" {
		promptMgr.CustomPrompt = sm.config.SystemPrompt
	}
	promptMgr.Model = sm.config.Model
	promptMgr.ProductName = "Claude Code (Go) - Feishu Bot"
	promptMgr.HookConfigs = sm.hookConfigs
	if sm.dreamer != nil {
		promptMgr.DreamMemoryDir = sm.dreamer.Stats().MemoryDir
	}

	cfg := &engine.Config{
		Model:            sm.config.Model,
		MaxTokens:        sm.config.MaxTokens,
		MaxTurns:         sm.config.MaxTurns,
		Cwd:              sm.config.Cwd,
		PermissionMode:   permMode,
		IsNonInteractive: true, // 飞书模式始终为非交互式
		Debug:            sm.config.Debug,
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

	// 注册 Agent 工具 (对应 TS: AgentTool → runAgent → 嵌套 queryLoop)
	// Agent 需要能创建嵌套 QueryEngine, 因此使用闭包注入 runAgent 函数
	var runAgentFn agent.RunAgentFunc
	runAgentFn = func(ctx context.Context, agentPrompt string, opts agent.RunOptions) (string, error) {
		return sm.runNestedAgent(ctx, runAgentFn, agentPrompt, opts)
	}
	reg.Register(agent.NewAgentTool(runAgentFn))

	eng := engine.NewQueryEngine(cfg, sm.apiClient, reg, hookRunner, permChecker, compactor, promptMgr)
	eng.MemoryStore = sm.memoryStore

	return &Session{
		ChatID:     chatID,
		Engine:     eng,
		LastActive: time.Now(),
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
	nestedReg := tool.NewRegistry()
	builtin.RegisterBaseToolsWithStore(nestedReg, sm.taskStore)
	if sm.mcpMgr != nil {
		sm.mcpMgr.RefreshToolsForRegistry(nestedReg)
	}
	if sm.skillReg != nil && sm.skillReg.Count() > 0 {
		nestedReg.Register(skills.NewSkillTool(sm.skillReg))
	}
	nestedReg.Register(agent.NewAgentTool(runAgentFn))

	permMode := types.PermissionMode(sm.config.PermissionMode)
	permChecker := permissions.NewChecker(permMode)
	if opts.ReadOnly {
		permMode = types.PermissionModePlan
		permChecker = permissions.NewChecker(permMode)
	}

	hookRunner := hooks.NewRunner(sm.hookConfigs, "")
	compactor := compact.NewCompactor(sm.apiClient, 200000)
	promptMgr := prompt.NewManager(sm.config.Cwd)
	promptMgr.Model = sm.config.Model

	model := sm.config.Model
	if opts.Model != "" {
		model = opts.Model
	}

	cfg := &engine.Config{
		Model:            model,
		MaxTokens:        sm.config.MaxTokens,
		MaxTurns:         sm.config.MaxTurns,
		Cwd:              sm.config.Cwd,
		PermissionMode:   permMode,
		IsNonInteractive: true,
		Debug:            sm.config.Debug,
	}

	nested := engine.NewQueryEngine(cfg, sm.apiClient, nestedReg, hookRunner, permChecker, compactor, promptMgr)

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
	}
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
		}
		s.mu.Unlock()
	}
}

// ClearSession 清除指定会话 (用于 /clear 命令)
func (sm *SessionManager) ClearSession(chatID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, chatID)
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
	session := sm.GetOrCreate(chatID)

	if session.IsProcessing() {
		return "上一条消息还在处理中，请稍候...", nil
	}

	session.SetProcessing(true)
	defer session.SetProcessing(false)
	session.Touch()

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
	return &sessionAgentRunner{sm: sm, role: role, systemPrompt: systemPrompt}, nil
}

// sessionAgentRunner 基于 SessionManager 的 Agent 执行器
type sessionAgentRunner struct {
	sm           *SessionManager
	role         string
	systemPrompt string
}

// Execute 执行 agent 任务 (创建独立 QueryEngine, 复用主会话运行模式)。
// 集成: Role Skills + Evolution 经验 + Dreaming 记录 (通过 Hook 注入)。
func (r *sessionAgentRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	nestedReg := tool.NewRegistry()
	builtin.RegisterBaseToolsWithStore(nestedReg, r.sm.taskStore)
	if r.sm.mcpMgr != nil {
		r.sm.mcpMgr.RefreshToolsForRegistry(nestedReg)
	}
	if r.sm.skillReg != nil && r.sm.skillReg.Count() > 0 {
		nestedReg.Register(skills.NewSkillTool(r.sm.skillReg))
	}

	var runAgentFn agent.RunAgentFunc
	runAgentFn = func(aCtx context.Context, prompt string, opts agent.RunOptions) (string, error) {
		return r.sm.runNestedAgent(aCtx, runAgentFn, prompt, opts)
	}
	nestedReg.Register(agent.NewAgentTool(runAgentFn))

	permMode := types.PermissionMode(r.sm.config.PermissionMode)
	permChecker := permissions.NewChecker(permMode)
	hookRunner := hooks.NewRunner(r.sm.hookConfigs, "")
	compactor := compact.NewCompactor(r.sm.apiClient, 200000)
	promptMgr := prompt.NewManager(r.sm.config.Cwd)
	promptMgr.Model = r.sm.config.Model

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
			promptMgr.CustomPrompt = expContext + "\n" + promptMgr.CustomPrompt
		}
	}

	cfg := &engine.Config{
		Model:            r.sm.config.Model,
		MaxTokens:        r.sm.config.MaxTokens,
		MaxTurns:         r.sm.config.MaxTurns,
		Cwd:              r.sm.config.Cwd,
		PermissionMode:   permMode,
		IsNonInteractive: true,
		Debug:            r.sm.config.Debug,
	}

	eng := engine.NewQueryEngine(cfg, r.sm.apiClient, nestedReg, hookRunner, permChecker, compactor, promptMgr)
	eng.MemoryStore = r.sm.memoryStore

	start := time.Now()
	var sb strings.Builder
	for msg := range eng.SubmitMessage(ctx, userPrompt) {
		if msg.Type != types.MessageTypeAssistant {
			continue
		}
		for _, b := range msg.Content {
			if b.Type == types.ContentBlockText {
				sb.WriteString(b.Text)
			}
		}
	}
	result := sb.String()

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
