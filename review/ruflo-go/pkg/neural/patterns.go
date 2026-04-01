// 本文件实现神经模式（Pattern）的 JSON 持久化存储 PatternStore，供 SONA 协调器等组件将学习到的模式落盘与恢复。
//
// 设计要点：写路径采用临时文件 + Rename 实现近似原子替换；读写均受互斥锁保护，避免与并发 Save/Load 竞态。
package neural

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PatternStore 将模式列表持久化为 JSON；mu 保护内存快照 path/patterns。
type PatternStore struct {
	mu       sync.RWMutex
	path     string
	patterns []*Pattern
}

// NewPatternStore 指定文件路径（如 .claude-flow/neural/patterns.json）。O(1)。
func NewPatternStore(path string) *PatternStore {
	return &PatternStore{path: path}
}

// diskPatterns 为磁盘 JSON 信封结构。
type diskPatterns struct {
	UpdatedAt time.Time  `json:"updated_at"`
	Patterns  []*Pattern `json:"patterns"`
}

// Save 将 patterns 序列化写入 path：先写 .tmp 再 Rename。O(P) 序列化主导；空间 O(输出字节)。
func (s *PatternStore) Save(patterns []*Pattern) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	dp := diskPatterns{UpdatedAt: time.Now().UTC(), Patterns: patterns}
	data, err := json.MarshalIndent(dp, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Load 从磁盘读取；文件不存在返回 (nil, nil)。O(文件大小)。
func (s *PatternStore) Load() ([]*Pattern, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var dp diskPatterns
	if err := json.Unmarshal(data, &dp); err != nil {
		return nil, err
	}
	s.patterns = dp.Patterns
	return dp.Patterns, nil
}

// SetMemory 替换内存中的 patterns 引用。O(1)。
func (s *PatternStore) SetMemory(patterns []*Pattern) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patterns = patterns
}

// MemorySnapshot 返回指针切片的浅拷贝（元素仍为原指针）。O(P)。
func (s *PatternStore) MemorySnapshot() []*Pattern {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Pattern, len(s.patterns))
	copy(out, s.patterns)
	return out
}
