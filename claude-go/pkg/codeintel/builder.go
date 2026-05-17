// builder.go — 构建流水线。
//
// 零 LLM 设计：所有阶段均为纯静态分析/图算法。
//   Phase 1: 文件枚举 + Tree-sitter AST 解析（占位，待 go-tree-sitter 集成）
//   Phase 2: 调用图/类型层级提取
//   Phase 3: Leiden 社区检测（占位，纯算法）
//   Phase 4: God Node 识别（度数/中心性统计）
//   Phase 5: 跨片边提取 + 清单生成
//
// LLM 仅用于可选的语义摘要增强（config.LLMEnhance=true，默认关闭）。
package codeintel

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Builder 代码智能构建器。
type Builder struct {
	Store  *Store
	Config *RepoConfig
}

// NewBuilder 创建构建器。
func NewBuilder(repoPath string) (*Builder, error) {
	store := NewStore(repoPath)
	cfg, err := store.LoadConfig()
	if err != nil {
		return nil, err
	}
	return &Builder{Store: store, Config: cfg}, nil
}

// BuildAll 全量构建所有分片。
func (b *Builder) BuildAll(branchName string, progress chan<- BuildProgress) (*BranchIndex, error) {
	if progress != nil {
		progress <- BuildProgress{Phase: "init", Message: "starting full build", Total: len(b.Config.Shards)}
	}

	if err := b.Store.EnsureDirs(branchName); err != nil {
		return nil, err
	}

	idx := &BranchIndex{
		BranchName: branchName,
		Shards:     make(map[string]*ShardIndex),
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	for i, shard := range b.Config.Shards {
		if progress != nil {
			progress <- BuildProgress{Phase: "shard", Shard: shard.Name, Current: i + 1, Total: len(b.Config.Shards), Message: "indexing shard"}
		}

		si, err := b.buildShard(branchName, shard, progress)
		if err != nil {
			return nil, fmt.Errorf("build shard %q: %w", shard.Name, err)
		}
		idx.Shards[shard.Name] = si
	}

	// 提取跨片边
	if progress != nil {
		progress <- BuildProgress{Phase: "cross", Message: "extracting cross-shard edges"}
	}
	if err := b.extractCrossEdges(branchName, idx); err != nil {
		// 跨片边失败不阻断主流程
		_ = err
	}

	idx.UpdatedAt = time.Now()
	if err := b.Store.SaveBranchIndex(idx); err != nil {
		return nil, fmt.Errorf("save branch index: %w", err)
	}

	b.Config.UpdatedAt = time.Now()
	_ = b.Store.SaveConfig(b.Config)

	if progress != nil {
		progress <- BuildProgress{Phase: "done", Message: "build complete"}
	}
	return idx, nil
}

// IncrementalUpdate 增量更新：检测变更文件，仅重索引受影响分片。
func (b *Builder) IncrementalUpdate(branchName string, progress chan<- BuildProgress) (*BranchIndex, error) {
	if progress != nil {
		progress <- BuildProgress{Phase: "diff", Message: "detecting changed files"}
	}

	changedFiles, err := b.detectChangedFiles(branchName)
	if err != nil {
		return nil, fmt.Errorf("detect changes: %w", err)
	}
	if len(changedFiles) == 0 {
		if progress != nil {
			progress <- BuildProgress{Phase: "done", Message: "no changes detected"}
		}
		return b.Store.LoadBranchIndex(branchName)
	}

	// 识别受影响的分片
	affectedShards := b.mapFilesToShards(changedFiles)
	if progress != nil {
		progress <- BuildProgress{Phase: "plan", Message: fmt.Sprintf("affected shards: %v", affectedShards), Total: len(affectedShards)}
	}

	idx, err := b.Store.LoadBranchIndex(branchName)
	if err != nil {
		return nil, err
	}

	for i, shardName := range affectedShards {
		shardCfg := b.findShardConfig(shardName)
		if shardCfg == nil {
			continue
		}
		if progress != nil {
			progress <- BuildProgress{Phase: "shard", Shard: shardName, Current: i + 1, Total: len(affectedShards), Message: "incremental reindex"}
		}
		si, err := b.buildShard(branchName, *shardCfg, progress)
		if err != nil {
			return nil, fmt.Errorf("rebuild shard %q: %w", shardName, err)
		}
		idx.Shards[shardName] = si
	}

	// 增量更新跨片边
	_ = b.extractCrossEdges(branchName, idx)

	idx.UpdatedAt = time.Now()
	if err := b.Store.SaveBranchIndex(idx); err != nil {
		return nil, err
	}

	if progress != nil {
		progress <- BuildProgress{Phase: "done", Message: "incremental update complete"}
	}
	return idx, nil
}

// ============================================================================
// 分片构建（核心占位：待 Tree-sitter + 图算法集成）
// ============================================================================

func (b *Builder) buildShard(branchName string, cfg ShardConfig, progress chan<- BuildProgress) (*ShardIndex, error) {
	// 1. 枚举文件
	files, err := b.enumerateFiles(cfg)
	if err != nil {
		return nil, err
	}

	// 2. 创建/打开 SQLite AST 索引
	db, err := b.Store.OpenShardDB(branchName, cfg.Name)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// 3. Tree-sitter AST 解析 + 符号提取（占位）
	// TODO: integrate go-tree-sitter for real AST parsing
	symbols, edges := b.parseASTPlaceholder(files, cfg, progress)

	// 4. 写入 SQLite
	if err := b.insertSymbols(db, symbols); err != nil {
		return nil, err
	}
	if err := b.insertEdges(db, edges); err != nil {
		return nil, err
	}

	// 5. 调用图 JSON
	cg := b.buildCallgraph(symbols, edges)
	cgPath := b.Store.CallgraphPath(branchName, cfg.Name)
	if err := saveJSON(cgPath, cg); err != nil {
		return nil, err
	}

	// 6. Leiden 社区检测（占位，纯算法）
	communities := b.detectCommunitiesPlaceholder(symbols, edges)

	// 7. God Node 识别
	godNodes := b.identifyGodNodes(symbols, edges)

	// 8. 清单生成
	manifest := b.buildManifest(cfg.Name, symbols, edges)
	mfPath := b.Store.ManifestPath(branchName, cfg.Name)
	_ = saveYAML(mfPath, manifest)

	// 9. 保存分片索引元数据
	si := &ShardIndex{
		ShardName:   cfg.Name,
		FileCount:   len(files),
		SymbolCount: len(symbols),
		EdgeCount:   len(edges),
		Communities: communities,
		GodNodes:    godNodes,
		Manifest:    manifest,
		LastIndexed: time.Now(),
	}
	if err := b.Store.SaveShardIndex(branchName, si); err != nil {
		return nil, err
	}
	return si, nil
}

// ============================================================================
// 占位实现（后续替换为真实算法）
// ============================================================================

type edgeRef struct {
	Src  string
	Dst  string
	Kind string
	File string
	Line int
}

// parseASTPlaceholder 占位 AST 解析：模拟提取符号和边。
func (b *Builder) parseASTPlaceholder(files []string, cfg ShardConfig, progress chan<- BuildProgress) ([]SymbolRef, []edgeRef) {
	var symbols []SymbolRef
	var edges []edgeRef
	for i, f := range files {
		if progress != nil && i%100 == 0 {
			progress <- BuildProgress{Phase: "parse", Shard: cfg.Name, Current: i, Total: len(files), Message: "parsing files"}
		}
		// 简单启发式：从文件名推导出一些符号
		base := filepath.Base(f)
		name := strings.TrimSuffix(base, filepath.Ext(base))
		symbols = append(symbols, SymbolRef{
			Name: name,
			Kind: "function",
			File: f,
			Line: 1,
		})
	}
	return symbols, edges
}

// detectCommunitiesPlaceholder 占位社区检测。
func (b *Builder) detectCommunitiesPlaceholder(symbols []SymbolRef, edges []edgeRef) []Community {
	// TODO: replace with real Leiden algorithm (gonum/graph or custom implementation)
	if len(symbols) == 0 {
		return nil
	}
	return []Community{{
		ID:        0,
		Label:     "default",
		Files:     []string{},
		CoreNodes: []string{},
		NodeCount: len(symbols),
		EdgeCount: len(edges),
	}}
}

// identifyGodNodes 基于度数统计识别枢纽节点。
func (b *Builder) identifyGodNodes(symbols []SymbolRef, edges []edgeRef) []GodNode {
	deg := make(map[string]int)
	inDeg := make(map[string]int)
	outDeg := make(map[string]int)
	for _, e := range edges {
		deg[e.Src]++
		deg[e.Dst]++
		outDeg[e.Src]++
		inDeg[e.Dst]++
	}
	var gods []GodNode
	for _, s := range symbols {
		d := deg[s.Name]
		if d > 10 { // 阈值可配置
			gods = append(gods, GodNode{
				Name:      s.Name,
				Kind:      s.Kind,
				Degree:    d,
				InDegree:  inDeg[s.Name],
				OutDegree: outDeg[s.Name],
				File:      s.File,
			})
		}
	}
	return gods
}

// buildManifest 构建分片接口清单。
func (b *Builder) buildManifest(shardName string, symbols []SymbolRef, edges []edgeRef) Manifest {
	// TODO: real export/import detection via visibility analysis
	var exports []SymbolRef
	var imports []SymbolRef
	for _, s := range symbols {
		// 占位：假设所有符号都是 exports
		exports = append(exports, s)
	}
	return Manifest{Exports: exports, Imports: imports}
}

// buildCallgraph 构建调用图结构（JSON 序列化用）。
func (b *Builder) buildCallgraph(symbols []SymbolRef, edges []edgeRef) map[string]interface{} {
	return map[string]interface{}{
		"nodes": symbols,
		"edges": edges,
	}
}

// ============================================================================
// 跨片边提取
// ============================================================================

func (b *Builder) extractCrossEdges(branchName string, idx *BranchIndex) error {
	// TODO: real cross-shard edge extraction using symbol resolution
	// Placeholder: create an empty KuzuDB or SQLite cross_edges file
	path := b.Store.CrossEdgesPath(branchName)
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	// KuzuDB placeholder
	return nil
}

// ============================================================================
// 增量更新辅助
// ============================================================================

func (b *Builder) detectChangedFiles(branchName string) ([]string, error) {
	// TODO: use git diff to detect changed files since last indexed commit
	// Placeholder: return empty (no changes)
	return []string{}, nil
}

func (b *Builder) mapFilesToShards(files []string) []string {
	shardSet := make(map[string]bool)
	for _, f := range files {
		for _, shard := range b.Config.Shards {
			for _, root := range shard.RootDirs {
				if strings.HasPrefix(f, root) {
					shardSet[shard.Name] = true
					break
				}
			}
		}
	}
	var result []string
	for s := range shardSet {
		result = append(result, s)
	}
	return result
}

func (b *Builder) findShardConfig(name string) *ShardConfig {
	for i := range b.Config.Shards {
		if b.Config.Shards[i].Name == name {
			return &b.Config.Shards[i]
		}
	}
	return nil
}

// ============================================================================
// 文件枚举
// ============================================================================

func (b *Builder) enumerateFiles(cfg ShardConfig) ([]string, error) {
	var files []string
	for _, root := range cfg.RootDirs {
		rootPath := filepath.Join(b.Config.RepoPath, root)
		_ = filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext == ".c" || ext == ".cc" || ext == ".cpp" || ext == ".h" || ext == ".hpp" || ext == ".go" || ext == ".rs" || ext == ".java" || ext == ".py" || ext == ".js" || ext == ".ts" {
				files = append(files, path)
			}
			return nil
		})
	}
	return files, nil
}

// ============================================================================
// 数据库辅助
// ============================================================================

func (b *Builder) insertSymbols(db *sql.DB, symbols []SymbolRef) error {
	// Placeholder: batch insert into SQLite
	return nil
}

func (b *Builder) insertEdges(db *sql.DB, edges []edgeRef) error {
	// Placeholder: batch insert into SQLite
	return nil
}

// ============================================================================
// 通用序列化辅助
// ============================================================================

func saveJSON(path string, v interface{}) error {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func saveYAML(path string, v interface{}) error {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
