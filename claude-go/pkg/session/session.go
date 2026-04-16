package session

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// TranscriptEntry JSONL 会话记录条目 (兼容 Claude Client 格式)。
type TranscriptEntry struct {
	Type       string        `json:"type"`
	UUID       string        `json:"uuid"`
	ParentUUID string        `json:"parentUuid,omitempty"`
	Message    types.Message `json:"message,omitempty"`
	SessionID  string        `json:"sessionId"`
	Timestamp  string        `json:"timestamp"`
	Cwd        string        `json:"cwd,omitempty"`
	Model      string        `json:"model,omitempty"`
	Version    string        `json:"version"`
	Summary    string        `json:"summary,omitempty"`
}

// SessionMeta 会话元数据 (用于列表展示)。
type SessionMeta struct {
	SessionID    string    `json:"sessionId"`
	CreatedAt    time.Time `json:"createdAt"`
	LastActive   time.Time `json:"lastActive"`
	MessageCount int       `json:"messageCount"`
	FirstPrompt  string    `json:"firstPrompt"`
	Model        string    `json:"model"`
}

func generateSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
