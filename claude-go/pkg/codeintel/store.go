// store.go — 极简状态存储。
//
// 不再管理 SQLite/KuzuDB/分片元数据，仅保存轻量的初始化状态。
package codeintel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Store 代码智能状态存储。
type Store struct {
	RepoPath string
}

// RepoState 仓库级状态（轻量 JSON）。
type RepoState struct {
	RepoPath        string `json:"repo_path"`
	GitNexusIndexed bool   `json:"gitnexus_indexed"`
	GraphifyIndexed bool   `json:"graphify_indexed"`
	LastUpdated     string `json:"last_updated,omitempty"`
}

// NewStore 创建状态存储器。
func NewStore(repoPath string) *Store {
	return &Store{RepoPath: repoPath}
}

// statePath 返回状态文件路径。
func (s *Store) statePath() string {
	return filepath.Join(s.RepoPath, ".claude-code-intel", "state.json")
}

// LoadState 加载仓库状态。
func (s *Store) LoadState() (*RepoState, error) {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &RepoState{RepoPath: s.RepoPath}, nil
		}
		return nil, err
	}
	var state RepoState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return &state, nil
}

// SaveState 保存仓库状态。
func (s *Store) SaveState(state *RepoState) error {
	path := s.statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// MarkGitNexusIndexed 标记 GitNexus 已索引。
func (s *Store) MarkGitNexusIndexed() error {
	state, _ := s.LoadState()
	state.GitNexusIndexed = true
	return s.SaveState(state)
}

// MarkGraphifyIndexed 标记 Graphify 已索引。
func (s *Store) MarkGraphifyIndexed() error {
	state, _ := s.LoadState()
	state.GraphifyIndexed = true
	return s.SaveState(state)
}
