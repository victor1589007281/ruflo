package neural

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PatternStore persists neural patterns to JSON with a mutex.
type PatternStore struct {
	mu       sync.RWMutex
	path     string
	patterns []*Pattern
}

// NewPatternStore targets path like .claude-flow/neural/patterns.json.
func NewPatternStore(path string) *PatternStore {
	return &PatternStore{path: path}
}

type diskPatterns struct {
	UpdatedAt time.Time  `json:"updated_at"`
	Patterns  []*Pattern `json:"patterns"`
}

// Save writes all patterns to disk (atomic replace).
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

// Load reads patterns from disk into memory.
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

// SetMemory replaces the in-memory slice (e.g. after Load).
func (s *PatternStore) SetMemory(patterns []*Pattern) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patterns = patterns
}

// MemorySnapshot returns a shallow copy of in-memory patterns.
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
