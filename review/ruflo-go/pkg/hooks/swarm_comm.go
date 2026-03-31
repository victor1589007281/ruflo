package hooks

import (
	"fmt"
	"sync"
	"time"
)

// AgentMessage is a hook-mediated inter-agent payload.
type AgentMessage struct {
	From      string         `json:"from"`
	To        string         `json:"to"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

// MessageHandler receives messages for a subscribed agent.
type MessageHandler func(msg AgentMessage) error

// SwarmCommunication routes messages between agents via in-memory channels.
type SwarmCommunication struct {
	mu       sync.RWMutex
	channels map[string]chan AgentMessage
	handlers map[string][]MessageHandler
}

// NewSwarmCommunication constructs an empty communication hub.
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

// Subscribe registers a handler invoked on Send before the message is queued.
func (s *SwarmCommunication) Subscribe(agentID string, handler MessageHandler) {
	if s == nil || agentID == "" || handler == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[agentID] = append(s.handlers[agentID], handler)
}

// Close removes a channel and handlers.
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
