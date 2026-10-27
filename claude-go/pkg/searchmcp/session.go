package searchmcp

// session.go —— MCP 会话身份与 max_uses 机械预算。
//
// HTTP 会话由服务端在 initialize/server/discover 时签发；stdio 在进程启动时
// 签发一个私有会话。预算按 WebSearch + WebFetch 合计，在实际工具调用前原子
// 扣减，因此缓存命中、参数错误和工具错误都占次数，超限请求不会进入工具。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// SessionHeader 是 MCP Streamable HTTP 的服务端会话头。
	SessionHeader = "Mcp-Session-Id"
	// MaxUsesEnv 限制单个 MCP 会话的 WebSearch + WebFetch 总次数；0/空为不限。
	MaxUsesEnv       = "SEARCH_MCP_MAX_USES"
	sessionIdleTTL   = 24 * time.Hour
	maxSessionStates = 4096
)

var (
	errSessionRequired = errors.New("Mcp-Session-Id is required when max_uses is enabled")
	errSessionInvalid  = errors.New("unknown or expired MCP session")
)

type sessionContextKey struct{}

func withSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, id)
}

func sessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(sessionContextKey{}).(string)
	return id
}

type sessionState struct {
	uses     int
	lastSeen time.Time
}

// sessionBudget 的零值表示不限次数；map 在首次创建会话时延迟初始化。
// 状态只属于本 MCP 进程及其签发的会话，不把不同客户端匿名合并成一个计数器。
type sessionBudget struct {
	maxUses int

	mu       sync.Mutex
	sessions map[string]sessionState
	now      func() time.Time
}

func newSessionBudget(maxUses int) sessionBudget {
	return sessionBudget{maxUses: maxUses, sessions: make(map[string]sessionState), now: time.Now}
}

func (b *sessionBudget) enabled() bool { return b != nil && b.maxUses > 0 }

func (b *sessionBudget) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *sessionBudget) create() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create MCP session: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	if b.sessions == nil {
		b.sessions = make(map[string]sessionState)
	}
	b.pruneLocked(now)
	b.sessions[id] = sessionState{lastSeen: now}
	return id, nil
}

// valid 只验明服务端确实签发过该 ID；次数扣减在 consume 中原子完成。
func (b *sessionBudget) valid(id string) bool {
	if id == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state, ok := b.sessions[id]
	now := b.clock()
	if !ok || b.expired(state, now) {
		delete(b.sessions, id)
		return false
	}
	state.lastSeen = now
	b.sessions[id] = state
	return true
}

func (b *sessionBudget) consume(id string) error {
	if !b.enabled() {
		return nil
	}
	if id == "" {
		return errSessionRequired
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state, ok := b.sessions[id]
	now := b.clock()
	if !ok || b.expired(state, now) {
		delete(b.sessions, id)
		return errSessionInvalid
	}
	if state.uses >= b.maxUses {
		return fmt.Errorf("max_uses exhausted: %d WebSearch/WebFetch calls per MCP session", b.maxUses)
	}
	state.uses++
	state.lastSeen = now
	b.sessions[id] = state
	return nil
}

func (b *sessionBudget) remove(id string) {
	b.mu.Lock()
	delete(b.sessions, id)
	b.mu.Unlock()
}

func (b *sessionBudget) expired(state sessionState, now time.Time) bool {
	return !state.lastSeen.IsZero() && now.Sub(state.lastSeen) > sessionIdleTTL
}

// pruneLocked 限制匿名 initialize 造成的内存增长；满员时淘汰最久空闲会话。
// 调用方必须持有 b.mu。
func (b *sessionBudget) pruneLocked(now time.Time) {
	for id, state := range b.sessions {
		if b.expired(state, now) {
			delete(b.sessions, id)
		}
	}
	if len(b.sessions) < maxSessionStates {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, state := range b.sessions {
		if oldestID == "" || state.lastSeen.Before(oldest) {
			oldestID, oldest = id, state.lastSeen
		}
	}
	delete(b.sessions, oldestID)
}

// MaxUsesFromEnv 解析进程启动配置。非法/负数必须阻止服务启动，不能静默变成
// 不限次数；模型工具参数中没有 max_uses，无法在请求侧放大这个服务端预算。
func MaxUsesFromEnv() (int, error) {
	raw := strings.TrimSpace(os.Getenv(MaxUsesEnv))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", MaxUsesEnv, raw)
	}
	return n, nil
}
