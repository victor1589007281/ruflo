package hooks

import (
	"fmt"
	"sync"
	"time"
)

// 本文件：Swarm 内 Agent 间内存消息总线。每 Agent 注册带缓冲 channel；Send 先同步调用订阅 Handler 再非阻塞入队；
// 队列满则返回错误。Broadcast 对每个已注册通道复制消息并逐个 Send。

// AgentMessage Hook 中介的 Agent 间消息体。
type AgentMessage struct {
	From      string         `json:"from"`              // 发送方 ID
	To        string         `json:"to"`                // 接收方 ID（路由键）
	Type      string         `json:"type"`              // 消息类型
	Payload   map[string]any `json:"payload,omitempty"` // 任意载荷
	Timestamp time.Time      `json:"timestamp"`         // UTC 时间（零值时 Send 内填当前时间）
}

// MessageHandler 订阅者在 Send 入队前同步调用的回调（错误被忽略）。
type MessageHandler func(msg AgentMessage) error

// SwarmCommunication 内存 channel + 处理器列表，实现多 Agent 消息路由。
type SwarmCommunication struct {
	mu       sync.RWMutex                 // 保护 channels 与 handlers
	channels map[string]chan AgentMessage // agentID -> 缓冲 channel
	handlers map[string][]MessageHandler  // agentID -> 处理器切片（拷贝后调用避免长时间持锁）
}

// NewSwarmCommunication 创建空总线。
func NewSwarmCommunication() *SwarmCommunication {
	return &SwarmCommunication{
		channels: make(map[string]chan AgentMessage),
		handlers: make(map[string][]MessageHandler),
	}
}

const swarmChanBuf = 64

// RegisterChannel allocates a receive channel for an agent.
func (s *SwarmCommunication) RegisterChannel(agentID string) {
	if s == nil || agentID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.channels[agentID]; ok {
		return
	}
	s.channels[agentID] = make(chan AgentMessage, swarmChanBuf)
}

// Send delivers a message to the recipient's channel (must be registered).
func (s *SwarmCommunication) Send(msg AgentMessage) error {
	if s == nil {
		return fmt.Errorf("hooks: nil SwarmCommunication")
	}
	if msg.To == "" {
		return fmt.Errorf("hooks: message missing To")
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now().UTC()
	}
	s.mu.RLock()
	ch, ok := s.channels[msg.To]
	hlist := append([]MessageHandler(nil), s.handlers[msg.To]...)
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("hooks: unknown channel %q", msg.To)
	}
	for _, h := range hlist {
		if h != nil {
			_ = h(msg)
		}
	}
	select {
	case ch <- msg:
		return nil
	default:
		return fmt.Errorf("hooks: channel %q full", msg.To)
	}
}

// Broadcast sends the same message to every registered channel except optional empty From.
func (s *SwarmCommunication) Broadcast(msg AgentMessage) {
	if s == nil {
		return
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now().UTC()
	}
	s.mu.RLock()
	ids := make([]string, 0, len(s.channels))
	for id := range s.channels {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		m := msg
		m.To = id
		_ = s.Send(m)
	}
}

// Subscribe 为某 Agent 追加 Send 前置处理器（入队前同步调用）。
func (s *SwarmCommunication) Subscribe(agentID string, handler MessageHandler) {
	if s == nil || agentID == "" || handler == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[agentID] = append(s.handlers[agentID], handler)
}

// Close 关闭并删除指定 Agent 的 channel 与全部 handlers。
func (s *SwarmCommunication) Close(agentID string) {
	if s == nil || agentID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.channels[agentID]; ok {
		close(ch)
		delete(s.channels, agentID)
	}
	delete(s.handlers, agentID)
}

// ListChannels returns registered agent IDs.
func (s *SwarmCommunication) ListChannels() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.channels))
	for id := range s.channels {
		out = append(out, id)
	}
	return out
}
