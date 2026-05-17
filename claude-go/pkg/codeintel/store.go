// store.go — 存储抽象层。
//
// 封装 SQLite (AST 索引)、文件系统 (JSON 图数据)、YAML (配置/清单) 的读写。
// 未来可扩展 KuzuDB (跨片图边)。
package codeintel

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"gopkg.in/yaml.v3"
)

// Store 代码智能存储管理器。
type Store struct {
	RepoPath string
}

// NewStore 创建存储管理器。
func NewStore(repoPath string) *Store {
	return &Store{RepoPath: repoPath}
}

// EnsureDirs 创建必要的目录结构。
func (s *Store) EnsureDirs(branchName string) error {
	dirs := []string{
		s.ConfigPath(),
		s.BranchPath(branchName),
		filepath.Join(s.BranchPath(branchName), "shards"),
		s.SharedPath(),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

// RepoBase 返回仓库存储根目录。
func (s *Store) RepoBase() string {
	return RepoBasePath(s.RepoPath)
}

// ConfigPath 返回配置文件路径。
func (s *Store) ConfigPath() string {
	return filepath.Join(s.RepoBase(), "config.yaml")
}

// BranchPath 返回指定分支的存储路径。
func (s *Store) BranchPath(branchName string) string {
	return BranchPath(s.RepoPath, branchName)
}

// SharedPath 返回共享数据路径。
func (s *Store) SharedPath() string {
	return filepath.Join(s.RepoBase(), "shared")
}

// ShardDir 返回指定分片的存储目录。
func (s *Store) ShardDir(branchName, shardName string) string {
	return filepath.Join(s.BranchPath(branchName), "shards", shardName)
}

// ShardDBPath 返回分片 SQLite 索引路径。
func (s *Store) ShardDBPath(branchName, shardName string) string {
	return filepath.Join(s.ShardDir(branchName, shardName), "ast.db")
}

// CallgraphPath 返回调用图 JSON 路径。
func (s *Store) CallgraphPath(branchName, shardName string) string {
	return filepath.Join(s.ShardDir(branchName, shardName), "callgraph.json")
}

// GraphifyPath 返回 Graphify 输出目录。
func (s *Store) GraphifyPath(branchName, shardName string) string {
	return filepath.Join(s.ShardDir(branchName, shardName), "graphify")
}

// ManifestPath 返回分片清单路径。
func (s *Store) ManifestPath(branchName, shardName string) string {
	return filepath.Join(s.ShardDir(branchName, shardName), "manifest.yaml")
}

// CrossEdgesPath 返回跨片边存储路径。
func (s *Store) CrossEdgesPath(branchName string) string {
	return filepath.Join(s.BranchPath(branchName), "cross_edges.db")
}

// BranchMetaPath 返回分支元数据路径。
func (s *Store) BranchMetaPath(branchName string) string {
	return filepath.Join(s.BranchPath(branchName), "branch.json")
}

// ============================================================================
// 配置读写
// ============================================================================

// LoadConfig 加载仓库配置。
func (s *Store) LoadConfig() (*RepoConfig, error) {
	data, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config not found, run init first")
		}
		return nil, err
	}
	var cfg RepoConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// SaveConfig 保存仓库配置。
func (s *Store) SaveConfig(cfg *RepoConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.ConfigPath(), data, 0644); err != nil {
		return err
	}
	return nil
}

// ============================================================================
// 分支元数据读写
// ============================================================================

// LoadBranchIndex 加载分支索引元数据。
func (s *Store) LoadBranchIndex(branchName string) (*BranchIndex, error) {
	path := s.BranchMetaPath(branchName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("branch %q not indexed", branchName)
		}
		return nil, err
	}
	var idx BranchIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse branch index: %w", err)
	}
	return &idx, nil
}

// SaveBranchIndex 保存分支索引元数据。
func (s *Store) SaveBranchIndex(idx *BranchIndex) error {
	path := s.BranchMetaPath(idx.BranchName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ============================================================================
// 分片索引读写
// ============================================================================

// LoadShardIndex 加载分片索引。
func (s *Store) LoadShardIndex(branchName, shardName string) (*ShardIndex, error) {
	path := filepath.Join(s.ShardDir(branchName, shardName), "index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("shard %q not indexed", shardName)
		}
		return nil, err
	}
	var idx ShardIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse shard index: %w", err)
	}
	return &idx, nil
}

// SaveShardIndex 保存分片索引。
func (s *Store) SaveShardIndex(branchName string, idx *ShardIndex) error {
	dir := s.ShardDir(branchName, idx.ShardName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "index.json")
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ============================================================================
// SQLite AST 索引（stub：表结构已创建，数据由 builder 填充）
// ============================================================================

// OpenShardDB 打开分片 SQLite 数据库。
func (s *Store) OpenShardDB(branchName, shardName string) (*sql.DB, error) {
	dbPath := s.ShardDBPath(branchName, shardName)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	if err := initASTSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func initASTSchema(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS symbols (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    file TEXT NOT NULL,
    line INTEGER,
    col INTEGER,
    parent_id INTEGER,
    FOREIGN KEY (parent_id) REFERENCES symbols(id)
);
CREATE INDEX IF NOT EXISTS idx_symbols_name ON symbols(name);
CREATE INDEX IF NOT EXISTS idx_symbols_file ON symbols(file);

CREATE TABLE IF NOT EXISTS edges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    src_symbol TEXT NOT NULL,
    dst_symbol TEXT NOT NULL,
    kind TEXT NOT NULL,  -- call, ref, type, inherit
    file TEXT NOT NULL,
    line INTEGER
);
CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src_symbol);
CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst_symbol);
`
	_, err := db.Exec(schema)
	return err
}

// ============================================================================
// 分支列表
// ============================================================================

// ListBranches 列出已索引的分支。
func (s *Store) ListBranches() ([]string, error) {
	branchesDir := filepath.Join(s.RepoBase(), "branches")
	entries, err := os.ReadDir(branchesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var branches []string
	for _, e := range entries {
		if e.IsDir() {
			branches = append(branches, e.Name())
		}
	}
	return branches, nil
}

// BranchExists 检查分支索引是否存在。
func (s *Store) BranchExists(branchName string) bool {
	_, err := os.Stat(s.BranchMetaPath(branchName))
	return err == nil
}

// DeleteBranch 删除分支索引。
func (s *Store) DeleteBranch(branchName string) error {
	return os.RemoveAll(s.BranchPath(branchName))
}
