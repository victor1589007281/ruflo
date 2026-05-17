// builder.go — 构建流水线。
//
// 零 LLM 设计：所有阶段均为纯静态分析/图算法。
//   Phase 1: 文件枚举 + Tree-sitter AST 解析
//   Phase 2: 调用图/类型层级提取
//   Phase 3: Leiden 社区检测
//   Phase 4: God Node 识别
//   Phase 5: 跨片边提取 + 清单生成
//
// LLM 仅用于可选的语义摘要增强（config.LLMEnhance=true，默认关闭）。
package codeintel

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// BuildAll 全量构建所有分片（并行）。
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

	// 并行构建各分片
	var mu sync.Mutex
	var wg sync.WaitGroup
	errChan := make(chan error, len(b.Config.Shards))

	for i, shard := range b.Config.Shards {
		wg.Add(1)
		go func(shardIdx int, cfg ShardConfig) {
			defer wg.Done()
			if progress != nil {
				progress <- BuildProgress{Phase: "shard", Shard: cfg.Name, Current: shardIdx + 1, Total: len(b.Config.Shards), Message: "indexing shard"}
			}

			si, err := b.buildShard(branchName, cfg, progress)
			if err != nil {
				errChan <- fmt.Errorf("build shard %q: %w", cfg.Name, err)
				return
			}
			mu.Lock()
			idx.Shards[cfg.Name] = si
			mu.Unlock()
		}(i, shard)
	}

	wg.Wait()
	close(errChan)
	for err := range errChan {
		if err != nil {
			return nil, err
		}
	}

	// 提取跨片边
	if progress != nil {
		progress <- BuildProgress{Phase: "cross", Message: "extracting cross-shard edges"}
	}
	if err := b.extractCrossEdges(branchName, idx); err != nil {
		_ = err // 跨片边失败不阻断主流程
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

// IncrementalUpdate 增量更新：检测变更文件，仅重索引受影响分片（并行）。
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

	affectedShards := b.mapFilesToShards(changedFiles)
	if progress != nil {
		progress <- BuildProgress{Phase: "plan", Message: fmt.Sprintf("affected shards: %v", affectedShards), Total: len(affectedShards)}
	}

	idx, err := b.Store.LoadBranchIndex(branchName)
	if err != nil {
		return nil, err
	}

	// 并行重建受影响分片
	var mu sync.Mutex
	var wg sync.WaitGroup
	errChan := make(chan error, len(affectedShards))

	for i, shardName := range affectedShards {
		shardCfg := b.findShardConfig(shardName)
		if shardCfg == nil {
			continue
		}
		wg.Add(1)
		go func(shardIdx int, name string, cfg ShardConfig) {
			defer wg.Done()
			if progress != nil {
				progress <- BuildProgress{Phase: "shard", Shard: name, Current: shardIdx + 1, Total: len(affectedShards), Message: "incremental reindex"}
			}
			si, err := b.buildShard(branchName, cfg, progress)
			if err != nil {
				errChan <- fmt.Errorf("rebuild shard %q: %w", name, err)
				return
			}
			mu.Lock()
			idx.Shards[name] = si
			mu.Unlock()
		}(i, shardName, *shardCfg)
	}

	wg.Wait()
	close(errChan)
	for err := range errChan {
		if err != nil {
			return nil, err
		}
	}

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
// 分片构建（真实实现）
// ============================================================================

func (b *Builder) buildShard(branchName string, cfg ShardConfig, progress chan<- BuildProgress) (*ShardIndex, error) {
	// 1. 枚举文件
	files, err := b.enumerateFiles(cfg)
	if err != nil {
		return nil, err
	}

	// 2. Tree-sitter AST 解析
	if progress != nil {
		progress <- BuildProgress{Phase: "parse", Shard: cfg.Name, Current: 0, Total: len(files), Message: "parsing files with tree-sitter"}
	}
	parsedFiles, err := ParseFiles(files)
	if err != nil {
		return nil, fmt.Errorf("parse files: %w", err)
	}

	// 3. 构建调用图
	graph := BuildGraphFromParsed(parsedFiles)

	// 4. 创建/打开 SQLite AST 索引
	db, err := b.Store.OpenShardDB(branchName, cfg.Name)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// 5. 写入 SQLite
	if err := b.insertSymbols(db, parsedFiles); err != nil {
		return nil, err
	}
	if err := b.insertEdges(db, parsedFiles); err != nil {
		return nil, err
	}

	// 6. Leiden 社区检测
	communities := graph.LeidenCommunities(1.0, 10)
	communityList := graph.ToCommunities(communities)

	// 7. God Node 识别
	godNodeResults := graph.IdentifyGodNodes(50)
	godNodes := make([]GodNode, len(godNodeResults))
	for i, gn := range godNodeResults {
		godNodes[i] = GodNode{
			Name:      gn.Name,
			Kind:      gn.Kind,
			Degree:    gn.Degree,
			InDegree:  gn.InDegree,
			OutDegree: gn.OutDegree,
			File:      gn.File,
		}
	}

	// 8. 调用图 JSON
	cg := buildCallgraph(graph)
	cgPath := b.Store.CallgraphPath(branchName, cfg.Name)
	if err := saveJSON(cgPath, cg); err != nil {
		return nil, err
	}

	// 9. 清单生成
	manifest := b.buildManifestFromParsed(cfg.Name, parsedFiles)
	mfPath := b.Store.ManifestPath(branchName, cfg.Name)
	_ = saveYAML(mfPath, manifest)

	// 10. 统计
	symbolCount := 0
	edgeCount := 0
	for _, pf := range parsedFiles {
		symbolCount += len(pf.Symbols) + len(pf.Types)
		edgeCount += len(pf.Calls)
	}

	// 11. 保存分片索引元数据
	si := &ShardIndex{
		ShardName:   cfg.Name,
		FileCount:   len(files),
		SymbolCount: symbolCount,
		EdgeCount:   edgeCount,
		Communities: communityList,
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
// 跨片边提取
// ============================================================================

func (b *Builder) extractCrossEdges(branchName string, idx *BranchIndex) error {
	// 收集所有分片的 exports 和 imports
	exportMap := make(map[string]string) // symbol -> shard
	for shardName, si := range idx.Shards {
		for _, exp := range si.Manifest.Exports {
			exportMap[exp.Name] = shardName
		}
	}

	// 识别跨片调用
	var crossEdges []edgeRef
	for shardName, si := range idx.Shards {
		for _, imp := range si.Manifest.Imports {
			if targetShard, ok := exportMap[imp.Name]; ok && targetShard != shardName {
				crossEdges = append(crossEdges, edgeRef{
					Src:  fmt.Sprintf("%s:%s", shardName, imp.Name),
					Dst:  fmt.Sprintf("%s:%s", targetShard, imp.Name),
					Kind: "cross_shard_call",
				})
			}
		}
	}

	// 保存跨片边（JSON 格式，后续可替换为 KuzuDB）
	path := b.Store.CrossEdgesPath(branchName)
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	return saveJSON(path+".json", map[string]interface{}{
		"cross_edges": crossEdges,
		"extracted_at": time.Now(),
	})
}

// ============================================================================
// 增量更新辅助
// ============================================================================

func (b *Builder) detectChangedFiles(branchName string) ([]string, error) {
	// 获取上次索引的 commit hash
	idx, err := b.Store.LoadBranchIndex(branchName)
	if err != nil {
		// 首次更新，无法检测，返回空（全量重建由调用方决定）
		return nil, nil
	}

	// 使用 git diff 检测变更
	baseCommit := idx.CommitHash
	if baseCommit == "" {
		baseCommit = "HEAD~1"
	}

	cmd := exec.Command("git", "-C", b.Config.RepoPath, "diff", "--name-only", baseCommit)
	out, err := cmd.Output()
	if err != nil {
		// git 命令失败，返回空（安全降级）
		return nil, nil
	}

	var changed []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			changed = append(changed, filepath.Join(b.Config.RepoPath, line))
		}
	}
	return changed, nil
}

func (b *Builder) mapFilesToShards(files []string) []string {
	shardSet := make(map[string]bool)
	for _, f := range files {
		for _, shard := range b.Config.Shards {
			for _, root := range shard.RootDirs {
				rootPath := filepath.Join(b.Config.RepoPath, root)
				if strings.HasPrefix(f, rootPath) {
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
			if ext == ".c" || ext == ".cc" || ext == ".cpp" || ext == ".h" || ext == ".hpp" || ext == ".go" || ext == ".rs" || ext == ".java" || ext == ".py" || ext == ".js" || ext == ".ts" || ext == ".tsx" || ext == ".jsx" {
				files = append(files, path)
			}
			return nil
		})
	}
	return files, nil
}

// ============================================================================
// 数据库辅助（真实实现）
// ============================================================================

func (b *Builder) insertSymbols(db *sql.DB, files []ParsedFile) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO symbols (name, kind, file, line, col, parent_id) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, pf := range files {
		for _, sym := range pf.Symbols {
			_, err := stmt.Exec(sym.Name, sym.Kind, sym.File, sym.Line, 0, nil)
			if err != nil {
				return err
			}
		}
		for _, t := range pf.Types {
			_, err := stmt.Exec(t.Name, t.Kind, t.File, t.Line, 0, nil)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (b *Builder) insertEdges(db *sql.DB, files []ParsedFile) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO edges (src_symbol, dst_symbol, kind, file, line) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, pf := range files {
		for _, call := range pf.Calls {
			_, err := stmt.Exec(call.Caller, call.Callee, "call", call.File, call.Line)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ============================================================================
// 清单生成
// ============================================================================

func (b *Builder) buildManifestFromParsed(shardName string, files []ParsedFile) Manifest {
	var exports []SymbolRef
	var imports []SymbolRef
	importSet := make(map[string]bool)

	for _, pf := range files {
		// 所有符号都视为 exports（简化版，后续可做可见性分析）
		for _, sym := range pf.Symbols {
			exports = append(exports, sym)
		}
		for _, t := range pf.Types {
			exports = append(exports, SymbolRef{
				Name: t.Name,
				Kind: t.Kind,
				File: t.File,
				Line: t.Line,
			})
		}
		// imports
		for _, imp := range pf.Imports {
			if !importSet[imp] {
				importSet[imp] = true
				imports = append(imports, SymbolRef{
					Name: imp,
					Kind: "import",
					File: pf.Path,
				})
			}
		}
	}
	return Manifest{Exports: exports, Imports: imports}
}

// ============================================================================
// 通用辅助
// ============================================================================

type edgeRef struct {
	Src  string
	Dst  string
	Kind string
	File string
	Line int
}

func buildCallgraph(graph *Graph) map[string]interface{} {
	var nodes []map[string]interface{}
	for id, node := range graph.Nodes {
		nodes = append(nodes, map[string]interface{}{
			"id":         id,
			"label":      node.Label,
			"kind":       node.Kind,
			"file":       node.File,
			"line":       node.Line,
			"degree":     node.Degree,
			"in_degree":  node.InDegree,
			"out_degree": node.OutDegree,
		})
	}
	var edges []map[string]interface{}
	for src, dsts := range graph.Edges {
		for dst, w := range dsts {
			edges = append(edges, map[string]interface{}{
				"source": src,
				"target": dst,
				"weight": w,
			})
		}
	}
	return map[string]interface{}{
		"nodes": nodes,
		"edges": edges,
		"node_count": len(graph.Nodes),
		"edge_count": len(edges),
	}
}

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
