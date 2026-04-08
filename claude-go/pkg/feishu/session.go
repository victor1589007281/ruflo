package feishu

import (
	"context"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
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
type SessionManager struct {
	sessions       map[string]*Session
	mu             sync.RWMutex
	config         *BotConfig
	apiClient      *api.Client
	maxSessions    int
	sessionTimeout time.Duration
}

// NewSessionManager 创建会话管理器
func NewSessionManager(config *BotConfig, apiClient *api.Client) *SessionManager {
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
	}

	// 启动后台清理 goroutine
	go sm.cleanupLoop()

	return sm
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
// 为每个会话创建独立的:
//   - tool.Registry (工具注册表)
//   - permissions.Checker (权限检查器)
//   - hooks.Runner (Hook 运行器)
//   - compact.Compactor (上下文压缩器)
//   - prompt.Manager (提示词管理器)
//   - engine.QueryEngine (查询引擎)
func (sm *SessionManager) createSession(chatID string) *Session {
	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg)

	permMode := types.PermissionMode(sm.config.PermissionMode)
	permChecker := permissions.NewChecker(permMode)
	hookRunner := hooks.NewRunner(nil, "")
	compactor := compact.NewCompactor(sm.apiClient, 200000)
	promptMgr := prompt.NewManager(sm.config.Cwd)
	if sm.config.SystemPrompt != "" {
		promptMgr.CustomPrompt = sm.config.SystemPrompt
	}
	promptMgr.Model = sm.config.Model
	promptMgr.ProductName = "Claude Code (Go) - Feishu Bot"

	cfg := &engine.Config{
		Model:            sm.config.Model,
		MaxTokens:        sm.config.MaxTokens,
		MaxTurns:         sm.config.MaxTurns,
		Cwd:              sm.config.Cwd,
		PermissionMode:   permMode,
		IsNonInteractive: true, // 飞书模式始终为非交互式
		Debug:            sm.config.Debug,
	}

	eng := engine.NewQueryEngine(cfg, sm.apiClient, reg, hookRunner, permChecker, compactor, promptMgr)

	return &Session{
		ChatID:     chatID,
		Engine:     eng,
		LastActive: time.Now(),
	}
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

	return response, nil
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
