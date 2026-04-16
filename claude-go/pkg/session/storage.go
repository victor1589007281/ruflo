package session

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// SessionStore 管理 JSONL 会话持久化和加载。
type SessionStore struct {
	mu         sync.Mutex
	baseDir    string // ~/.claude-go/projects/
	projectKey string // 项目目录 hash
	sessionID  string
	file       *os.File
	version    string
}

// NewSessionStore 创建新的会话存储。projectDir 是当前工作目录的绝对路径。
func NewSessionStore(projectDir string) (*SessionStore, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("session: cannot get home dir: %w", err)
	}
	baseDir := filepath.Join(homeDir, ".claude-go", "projects")
	projectKey := hashDir(projectDir)
	storePath := filepath.Join(baseDir, projectKey)
	if err := os.MkdirAll(storePath, 0755); err != nil {
		return nil, fmt.Errorf("session: mkdir %s: %w", storePath, err)
	}

	sid := generateSessionID()
	return &SessionStore{
		baseDir:    baseDir,
		projectKey: projectKey,
		sessionID:  sid,
		version:    "claude-go/1.0",
	}, nil
}

func (s *SessionStore) SessionID() string { return s.sessionID }

// AppendEntry 追加一条记录到 JSONL 文件。
func (s *SessionStore) AppendEntry(entry TranscriptEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		path := s.sessionPath(s.sessionID)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("session: open %s: %w", path, err)
		}
		s.file = f
	}

	entry.SessionID = s.sessionID
	entry.Version = s.version
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.file, "%s\n", data)
	return err
}

// AppendUserMessage 便捷方法: 记录用户消息。
func (s *SessionStore) AppendUserMessage(msg types.Message, cwd, model string) {
	_ = s.AppendEntry(TranscriptEntry{
		Type:    "user",
		UUID:    msg.UUID,
		Message: msg,
		Cwd:     cwd,
		Model:   model,
	})
}

// AppendAssistantMessage 便捷方法: 记录助手消息。
func (s *SessionStore) AppendAssistantMessage(msg types.Message) {
	_ = s.AppendEntry(TranscriptEntry{
		Type:    "assistant",
		UUID:    msg.UUID,
		Message: msg,
		Model:   msg.Model,
	})
}

// LoadSession 从 JSONL 文件加载会话。
func (s *SessionStore) LoadSession(sessionID string) ([]TranscriptEntry, error) {
	path := s.sessionPath(sessionID)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("session: load %s: %w", sessionID, err)
	}
	defer f.Close()

	var entries []TranscriptEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024) // 4MB per line
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry TranscriptEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

// ResumeSession 加载并恢复一个已有会话。返回消息列表。
func (s *SessionStore) ResumeSession(sessionID string) ([]types.Message, error) {
	entries, err := s.LoadSession(sessionID)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.sessionID = sessionID
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
	s.mu.Unlock()

	return BuildMessageChain(entries), nil
}

// BuildMessageChain 从 TranscriptEntry 列表构建消息链。
func BuildMessageChain(entries []TranscriptEntry) []types.Message {
	var msgs []types.Message
	for _, e := range entries {
		if e.Type == "user" || e.Type == "assistant" {
			msgs = append(msgs, e.Message)
		}
	}
	return msgs
}

// ListSessions 列出项目的所有会话。
func (s *SessionStore) ListSessions() ([]SessionMeta, error) {
	dir := filepath.Join(s.baseDir, s.projectKey)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var metas []SessionMeta
	for _, de := range dirEntries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		sid := strings.TrimSuffix(de.Name(), ".jsonl")
		info, _ := de.Info()
		meta := SessionMeta{SessionID: sid}
		if info != nil {
			meta.LastActive = info.ModTime()
		}

		entries, err := s.LoadSession(sid)
		if err != nil || len(entries) == 0 {
			continue
		}
		meta.MessageCount = len(entries)
		meta.CreatedAt, _ = time.Parse(time.RFC3339, entries[0].Timestamp)
		for _, e := range entries {
			if e.Type == "user" {
				for _, cb := range e.Message.Content {
					if cb.Type == types.ContentBlockText && cb.Text != "" {
						meta.FirstPrompt = cb.Text
						if len(meta.FirstPrompt) > 100 {
							meta.FirstPrompt = meta.FirstPrompt[:100] + "..."
						}
						break
					}
				}
				break
			}
		}
		if len(entries) > 0 {
			meta.Model = entries[0].Model
		}
		metas = append(metas, meta)
	}

	sort.Slice(metas, func(i, j int) bool {
		return metas[i].LastActive.After(metas[j].LastActive)
	})
	return metas, nil
}

// MostRecentSessionID 返回最近活跃的 session ID。
func (s *SessionStore) MostRecentSessionID() (string, error) {
	metas, err := s.ListSessions()
	if err != nil || len(metas) == 0 {
		return "", fmt.Errorf("no sessions found")
	}
	return metas[0].SessionID, nil
}

// Close 关闭文件句柄。
func (s *SessionStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
}

func (s *SessionStore) sessionPath(sid string) string {
	return filepath.Join(s.baseDir, s.projectKey, sid+".jsonl")
}

func hashDir(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(h[:8])
}
