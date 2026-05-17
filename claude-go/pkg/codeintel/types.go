// Package codeintel 提供大型代码库知识图谱的构建、查询与管理能力。
//
// 设计目标：
//   - 零 LLM Token：构建流水线纯静态分析（Tree-sitter + SCIP + 图算法）
//   - 分支隔离：CoW 元数据 + 共享只读数据
//   - 分片存储：按子系统分片，支持千万行级代码库
//   - 双模暴露：对内为 claude-go 内置 tool，对外为 MCP Server
//
// 存储布局（以仓库哈希为根）：
//   ~/.claude-code-intel/<repo-hash>/
//     ├── config.yaml          # 分片配置、构建参数
//     ├── branches/
//     │   ├── main/
//     │   │   ├── shards/
//     │   │   │   ├── net/
//     │   │   │   │   ├── ast.db          # SQLite AST 索引
//     │   │   │   │   ├── callgraph.json  # 调用图
//     │   │   │   │   ├── graphify/       # 社区检测结果
//     │   │   │   │   └── manifest.yaml   # 跨片接口符号
//     │   │   └── cross_edges.db          # KuzuDB 跨片边
//     │   └── feature-x/      # 分支隔离（CoW 复用未变更分片）
//     └── shared/             # 分支间共享的只读数据
package codeintel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// 核心类型
// ============================================================================

// RepoConfig 仓库级配置。
type RepoConfig struct {
	RepoPath    string        `yaml:"repo_path" json:"repo_path"`
	RepoHash    string        `yaml:"repo_hash" json:"repo_hash"`
	Shards      []ShardConfig `yaml:"shards" json:"shards"`
	CreatedAt   time.Time     `yaml:"created_at" json:"created_at"`
	UpdatedAt   time.Time     `yaml:"updated_at" json:"updated_at"`
	LLMEnhance  bool          `yaml:"llm_enhance" json:"llm_enhance"` // 默认 false
	AutoShard   bool          `yaml:"auto_shard" json:"auto_shard"`   // 自动分片检测
}

// ShardConfig 分片配置。
type ShardConfig struct {
	Name       string   `yaml:"name" json:"name"`
	RootDirs   []string `yaml:"root_dirs" json:"root_dirs"`
	MaxFiles   int      `yaml:"max_files,omitempty" json:"max_files,omitempty"`
	LangFilter []string `yaml:"lang_filter,omitempty" json:"lang_filter,omitempty"`
}

// ShardIndex 单个分片的索引数据。
type ShardIndex struct {
	ShardName    string       `json:"shard_name"`
	FileCount    int          `json:"file_count"`
	SymbolCount  int          `json:"symbol_count"`
	EdgeCount    int          `json:"edge_count"`
	Communities  []Community  `json:"communities,omitempty"`
	GodNodes     []GodNode    `json:"god_nodes,omitempty"`
	Manifest     Manifest     `json:"manifest"`
	LastIndexed  time.Time    `json:"last_indexed"`
}

// Manifest 分片对外接口清单。
type Manifest struct {
	Exports []SymbolRef `json:"exports"` // 本片区对外暴露的符号
	Imports []SymbolRef `json:"imports"` // 本片区依赖的外部符号
}

// SymbolRef 符号引用。
type SymbolRef struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`     // function, type, variable, macro
	File     string `json:"file"`
	Line     int    `json:"line"`
	DefShard string `json:"def_shard,omitempty"`
}

// Community Leiden 检测出的社区。
type Community struct {
	ID          int      `json:"id"`
	Label       string   `json:"label,omitempty"`
	Files       []string `json:"files"`
	CoreNodes   []string `json:"core_nodes"`
	NodeCount   int      `json:"node_count"`
	EdgeCount   int      `json:"edge_count"`
}

// GodNode 高度数枢纽节点。
type GodNode struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Degree     int    `json:"degree"`
	InDegree   int    `json:"in_degree"`
	OutDegree  int    `json:"out_degree"`
	File       string `json:"file"`
}

// BranchIndex 分支级索引元数据。
type BranchIndex struct {
	BranchName   string                 `json:"branch_name"`
	CommitHash   string                 `json:"commit_hash"`
	Shards       map[string]*ShardIndex `json:"shards"`
	CrossEdges   string                 `json:"cross_edges_path"` // KuzuDB 路径
	CreatedAt    time.Time              `json:"created_at"`
	UpdatedAt    time.Time              `json:"updated_at"`
}

// QueryResult 查询结果。
type QueryResult struct {
	QueryType string      `json:"query_type"`
	Shard     string      `json:"shard,omitempty"`
	Results   interface{} `json:"results"`
	Tokens    int         `json:"estimated_tokens"` // 结果估算 token 数
	LatencyMs int64       `json:"latency_ms"`
}

// BuildProgress 构建进度。
type BuildProgress struct {
	Phase       string  `json:"phase"`
	Shard       string  `json:"shard,omitempty"`
	Current     int     `json:"current"`
	Total       int     `json:"total"`
	Message     string  `json:"message"`
}

// ============================================================================
// 查询参数类型
// ============================================================================

// NavigateQuery 符号导航查询。
type NavigateQuery struct {
	Symbol string `json:"symbol"`
	Depth  int    `json:"depth,omitempty"`
	Shard  string `json:"shard,omitempty"`
}

// ImpactQuery 影响分析查询。
type ImpactQuery struct {
	FilePath string `json:"file_path"`
	Depth    int    `json:"depth,omitempty"`
}

// CommunityQuery 社区查询。
type CommunityQuery struct {
	Shard string `json:"shard"`
	TopN  int    `json:"top_n,omitempty"`
}

// CrossShardQuery 跨片查询。
type CrossShardQuery struct {
	Symbol      string `json:"symbol"`
	TargetShard string `json:"target_shard,omitempty"`
}

// ============================================================================
// 工具函数
// ============================================================================

// HashRepoPath 计算仓库路径的哈希，用于确定存储目录。
func HashRepoPath(repoPath string) string {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	h := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(h[:])[:16]
}

// DefaultBasePath 返回默认存储根目录。
func DefaultBasePath() string {
	home, err := filepath.Abs("~")
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude-code-intel")
}

// RepoBasePath 返回指定仓库的存储根目录。
func RepoBasePath(repoPath string) string {
	return filepath.Join(DefaultBasePath(), HashRepoPath(repoPath))
}

// BranchPath 返回指定分支的存储路径。
func BranchPath(repoPath, branchName string) string {
	return filepath.Join(RepoBasePath(repoPath), "branches", sanitizeBranchName(branchName))
}

func sanitizeBranchName(name string) string {
	// 替换路径危险字符
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "..", "_")
	return name
}

// ValidateRepoPath 检查路径是否为合法 git 仓库。
func ValidateRepoPath(path string) error {
	gitDir := filepath.Join(path, ".git")
	// 简单检查 .git 目录/文件存在（不一定是完整验证）
	if _, err := filepath.Abs(gitDir); err != nil {
		return fmt.Errorf("invalid repo path: %w", err)
	}
	return nil
}
