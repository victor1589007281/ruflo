package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HistoryEntry 记录用户输入的命令历史。
type HistoryEntry struct {
	Prompt    string `json:"prompt"`
	Timestamp string `json:"timestamp"`
	SessionID string `json:"sessionId,omitempty"`
}

// PromptHistory 管理用户的输入历史。
type PromptHistory struct {
	mu      sync.Mutex
	path    string
	entries []HistoryEntry
	loaded  bool
}

// NewPromptHistory 创建历史管理器。文件位于 ~/.claude-go/history.jsonl。
func NewPromptHistory() (*PromptHistory, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(homeDir, ".claude-go")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &PromptHistory{
		path: filepath.Join(dir, "history.jsonl"),
	}, nil
}

// Append 添加新的历史条目。
func (h *PromptHistory) Append(prompt, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	entry := HistoryEntry{
		Prompt:    prompt,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		SessionID: sessionID,
	}
	h.entries = append(h.entries, entry)

	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(entry)
	fmt.Fprintf(f, "%s\n", data)
}

// Load 加载所有历史记录。
func (h *PromptHistory) Load() []HistoryEntry {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.loaded {
		return h.entries
	}
	h.loaded = true

	f, err := os.Open(h.path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry HistoryEntry
		if json.Unmarshal([]byte(line), &entry) == nil {
			h.entries = append(h.entries, entry)
		}
	}
	return h.entries
}

// Prompts 返回所有历史 prompt 字符串 (最新在后)。
func (h *PromptHistory) Prompts() []string {
	entries := h.Load()
	prompts := make([]string, len(entries))
	for i, e := range entries {
		prompts[i] = e.Prompt
	}
	return prompts
}
