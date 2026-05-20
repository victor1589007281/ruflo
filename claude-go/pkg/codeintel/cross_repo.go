// cross_repo.go — 跨仓库符号引用分析（GlobalIndex 驱动）。
//
// 能力:
//   1. 自动检测仓库依赖关系（go.mod / package.json / requirements.txt / Cargo.toml）
//   2. 在 GlobalIndex 中维护跨仓库依赖图
//   3. 跨仓库符号查询：遍历依赖链查找符号定义与引用
package codeintel

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ============================================================================
// 依赖检测
// ============================================================================

// DependencyEntry 单个依赖项。
type DependencyEntry struct {
	Name    string `json:"name"`    // 模块/包名
	Version string `json:"version"` // 版本（如有）
	Source  string `json:"source"`  // 来源文件（go.mod / package.json 等）
}

// DetectRepoDependencies 扫描仓库中的依赖文件，提取依赖列表。
func DetectRepoDependencies(repoPath string) []DependencyEntry {
	var deps []DependencyEntry

	// Go: go.mod
	if d := parseGoMod(repoPath); len(d) > 0 {
		deps = append(deps, d...)
	}
	// Node.js: package.json
	if d := parsePackageJSON(repoPath); len(d) > 0 {
		deps = append(deps, d...)
	}
	// Python: requirements.txt
	if d := parseRequirementsTxt(repoPath); len(d) > 0 {
		deps = append(deps, d...)
	}
	// Rust: Cargo.toml
	if d := parseCargoToml(repoPath); len(d) > 0 {
		deps = append(deps, d...)
	}

	return deps
}

// parseGoMod 解析 go.mod 中的 require 依赖。
func parseGoMod(repoPath string) []DependencyEntry {
	path := filepath.Join(repoPath, "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var deps []DependencyEntry
	lines := strings.Split(string(data), "\n")
	inRequire := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "require (") {
			inRequire = true
			continue
		}
		if inRequire && trimmed == ")" {
			inRequire = false
			continue
		}
		if inRequire && trimmed != "" && !strings.HasPrefix(trimmed, "//") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 1 {
				deps = append(deps, DependencyEntry{
					Name:   parts[0],
					Source: "go.mod",
				})
			}
		}
	}
	return deps
}

// parsePackageJSON 解析 package.json 中的 dependencies 和 devDependencies。
func parsePackageJSON(repoPath string) []DependencyEntry {
	path := filepath.Join(repoPath, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}
	var deps []DependencyEntry
	for name := range pkg.Dependencies {
		deps = append(deps, DependencyEntry{Name: name, Source: "package.json"})
	}
	for name := range pkg.DevDependencies {
		deps = append(deps, DependencyEntry{Name: name, Source: "package.json"})
	}
	return deps
}

// parseRequirementsTxt 解析 requirements.txt 中的包名。
func parseRequirementsTxt(repoPath string) []DependencyEntry {
	path := filepath.Join(repoPath, "requirements.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var deps []DependencyEntry
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		// 取包名（忽略版本约束）
		name := strings.FieldsFunc(line, func(r rune) bool {
			return r == '=' || r == '<' || r == '>' || r == '!' || r == '~' || r == '['
		})[0]
		deps = append(deps, DependencyEntry{Name: name, Source: "requirements.txt"})
	}
	return deps
}

// parseCargoToml 解析 Cargo.toml 中的 dependencies。
func parseCargoToml(repoPath string) []DependencyEntry {
	path := filepath.Join(repoPath, "Cargo.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cargo struct {
		Dependencies map[string]interface{} `toml:"dependencies"`
	}
	// 简单行解析（避免引入 toml 库）
	lines := strings.Split(string(data), "\n")
	inDeps := false
	var deps []DependencyEntry
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[dependencies]") {
			inDeps = true
			continue
		}
		if inDeps && strings.HasPrefix(trimmed, "[") {
			inDeps = false
			continue
		}
		if inDeps && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) >= 1 {
				name := strings.TrimSpace(parts[0])
				deps = append(deps, DependencyEntry{Name: name, Source: "Cargo.toml"})
			}
		}
	}
	_ = cargo
	return deps
}

// ============================================================================
// GlobalIndex 扩展：依赖管理
// ============================================================================

// UpdateDependencies 扫描并更新仓库的依赖关系。
func (gi *GlobalIndex) UpdateDependencies(repoID string) error {
	gi.mu.Lock()
	defer gi.mu.Unlock()

	repo, ok := gi.Repos[repoID]
	if !ok {
		return fmt.Errorf("repo %s not found", repoID)
	}

	deps := DetectRepoDependencies(repo.Path)
	// 尝试将依赖名匹配到已注册的仓库
	repo.Deps = make([]string, 0, len(deps))
	for _, dep := range deps {
		// 简单启发式：如果某个已注册仓库的路径或名称包含依赖名，视为匹配
		for _, other := range gi.Repos {
			if other.ID == repoID {
				continue
			}
			if strings.Contains(other.Path, dep.Name) || strings.Contains(dep.Name, other.Name) {
				repo.Deps = append(repo.Deps, other.ID)
				break
			}
		}
	}

	repo.UpdatedAt = time.Now().UTC()
	return gi.save()
}

// GetDependencyRepos 返回某仓库依赖的所有已注册仓库 ID。
func (gi *GlobalIndex) GetDependencyRepos(repoID string) []string {
	gi.mu.RLock()
	defer gi.mu.RUnlock()

	repo, ok := gi.Repos[repoID]
	if !ok || len(repo.Deps) == 0 {
		return nil
	}
	// 去重
	seen := make(map[string]struct{}, len(repo.Deps))
	var out []string
	for _, id := range repo.Deps {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// ============================================================================
// 跨仓库结果去重与置信度融合
// ============================================================================

// CrossRepoRefEntry 跨仓库归一化引用条目。
type CrossRepoRefEntry struct {
	RepoID      string  `json:"repo_id"`
	RepoName    string  `json:"repo_name"`
	File        string  `json:"file"`
	Line        int     `json:"line"`
	Text        string  `json:"text,omitempty"`
	Symbol      string  `json:"symbol"`
	Confidence  float64 `json:"confidence"`
	Engines     []string `json:"engines,omitempty"`
}

// dedupKey 生成去重键（忽略仓库，按文件+行号+文本内容）。
func (e *CrossRepoRefEntry) dedupKey() string {
	return fmt.Sprintf("%s:%d:%s", e.File, e.Line, e.Text)
}

// extractRefEntries 从单个仓库的 QueryResult 中提取归一化引用条目。
func extractRefEntries(repoID, repoName string, qr *QueryResult, baseConfidence float64) []CrossRepoRefEntry {
	if qr == nil || qr.Results == nil {
		return nil
	}
	m, ok := qr.Results.(map[string]interface{})
	if !ok {
		return nil
	}

	var refs []CrossRepoRefEntry
	engines := []string{}
	if eng, ok := m["engine"].(string); ok {
		engines = append(engines, eng)
	}

	// 尝试从 "references" 或 "definitions" 中提取
	for _, key := range []string{"references", "definitions"} {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var items []map[string]interface{}
		switch v := raw.(type) {
		case []map[string]interface{}:
			items = v
		case []interface{}:
			for _, it := range v {
				if m2, ok := it.(map[string]interface{}); ok {
					items = append(items, m2)
				}
			}
		}

		symbol := ""
		if s, ok := m["symbol"].(string); ok {
			symbol = s
		}

		for _, it := range items {
			file := ""
			if f, ok := it["file"].(string); ok {
				file = f
			}
			line := 0
			if l, ok := it["line"].(float64); ok {
				line = int(l)
			}
			if l, ok := it["line"].(int); ok {
				line = l
			}
			text := ""
			if t, ok := it["text"].(string); ok {
				text = t
			}
			if t, ok := it["match"].(string); ok {
				text = t
			}

			conf := baseConfidence
			// 完整性加成
			if file != "" && line > 0 {
				conf += 0.05
			}
			if text != "" {
				conf += 0.03
			}
			if conf > 1.0 {
				conf = 1.0
			}

			refs = append(refs, CrossRepoRefEntry{
				RepoID:     repoID,
				RepoName:   repoName,
				File:       file,
				Line:       line,
				Text:       text,
				Symbol:     symbol,
				Confidence: conf,
				Engines:    engines,
			})
		}
	}
	return refs
}

// DedupCrossRepoResults 对多仓库引用结果去重并融合置信度。
// 返回去重后的条目列表及统计信息。
func DedupCrossRepoResults(entries []CrossRepoRefEntry) ([]CrossRepoRefEntry, map[string]interface{}) {
	groups := make(map[string][]CrossRepoRefEntry)
	for _, e := range entries {
		key := e.dedupKey()
		if key == ":0:" {
			continue // 跳过无法定位的条目
		}
		groups[key] = append(groups[key], e)
	}

	var deduped []CrossRepoRefEntry
	for _, group := range groups {
		merged := MergeConfidence(group)
		deduped = append(deduped, merged)
	}

	stats := map[string]interface{}{
		"total_entries":      len(entries),
		"unique_entries":     len(deduped),
		"duplicate_count":    len(entries) - len(deduped),
		"duplicate_rate":     0.0,
	}
	if len(entries) > 0 {
		stats["duplicate_rate"] = float64(len(entries)-len(deduped)) / float64(len(entries))
	}
	return deduped, stats
}

// MergeConfidence 将同一引用在多个仓库中的结果融合为单一置信度。
// 融合策略:
//   1. 取最高基础置信度作为基准
//   2. 跨仓库验证加成：每多一个仓库确认，+0.08（上限 0.24）
//   3. 结果来源多样性加成：引擎种类 >1，+0.05
//   4. 最终截断到 [0, 1]
func MergeConfidence(group []CrossRepoRefEntry) CrossRepoRefEntry {
	if len(group) == 0 {
		return CrossRepoRefEntry{}
	}
	if len(group) == 1 {
		return group[0]
	}

	best := group[0]
	maxConf := best.Confidence
	repoSet := make(map[string]struct{})
	engineSet := make(map[string]struct{})
	var allEngines []string

	for _, e := range group {
		if e.Confidence > maxConf {
			maxConf = e.Confidence
			best = e
		}
		repoSet[e.RepoID] = struct{}{}
		for _, eng := range e.Engines {
			if _, ok := engineSet[eng]; !ok {
				engineSet[eng] = struct{}{}
				allEngines = append(allEngines, eng)
			}
		}
	}

	// 跨仓库验证加成
	crossRepoBonus := math.Min(float64(len(repoSet)-1)*0.08, 0.24)
	// 引擎多样性加成
	engineBonus := 0.0
	if len(engineSet) > 1 {
		engineBonus = 0.05
	}

	merged := best
	merged.Confidence = math.Min(maxConf+crossRepoBonus+engineBonus, 1.0)
	merged.Engines = allEngines
	return merged
}

// ============================================================================
// Engine 扩展：跨仓库符号查询
// ============================================================================

// CrossRepoFindRefs 在依赖链中跨仓库查找符号引用。
// 返回主仓库结果 + 每个依赖仓库的结果汇总，经过去重与置信度融合。
func (e *Engine) CrossRepoFindRefs(branchName string, q NavigateQuery) (*QueryResult, error) {
	start := time.Now()
	if e.globalIndex == nil || e.repoID == "" {
		// 无全局索引，退化为单仓库查询
		return e.FindRefs(branchName, q)
	}

	// 主仓库查询
	mainResult, mainErr := e.FindRefs(branchName, q)

	// 获取依赖仓库
	depIDs := e.globalIndex.GetDependencyRepos(e.repoID)
	if len(depIDs) == 0 {
		return mainResult, mainErr
	}

	// 收集所有仓库的归一化条目
	var allEntries []CrossRepoRefEntry
	allEntries = append(allEntries, extractRefEntries(e.repoID, "main", mainResult, 1.0)...)

	crossResults := []map[string]interface{}{
		{
			"repo_id":   e.repoID,
			"repo_name": "main",
			"result":    safeResult(mainResult),
			"error":     errString(mainErr),
		},
	}

	for _, depID := range depIDs {
		depRepo, ok := e.globalIndex.GetRepo(depID)
		if !ok {
			continue
		}
		// 为依赖仓库创建临时 Engine
		depEngine := NewEngine(depRepo.Path)
		depResult, depErr := depEngine.FindRefs(branchName, q)
		allEntries = append(allEntries, extractRefEntries(depID, depRepo.Name, depResult, 0.7)...)

		crossResults = append(crossResults, map[string]interface{}{
			"repo_id":   depID,
			"repo_name": depRepo.Name,
			"result":    safeResult(depResult),
			"error":     errString(depErr),
		})
	}

	// 去重与置信度融合
	deduped, dedupStats := DedupCrossRepoResults(allEntries)

	// 按置信度降序排列
	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].Confidence > deduped[j].Confidence
	})

	// 计算平均置信度
	var avgConfidence float64
	if len(deduped) > 0 {
		var sum float64
		for _, e := range deduped {
			sum += e.Confidence
		}
		avgConfidence = sum / float64(len(deduped))
	}

	// 构建汇总结果
	result := map[string]interface{}{
		"symbol":          q.Symbol,
		"cross_repo":      true,
		"repositories":    crossResults,
		"dep_repo_count":  len(depIDs),
		"dedup_stats":     dedupStats,
		"merged_refs":     deduped,
		"avg_confidence":  avgConfidence,
		"high_conf_count": countHighConfidence(deduped, 0.8),
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "cross_repo_find_refs",
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

func countHighConfidence(entries []CrossRepoRefEntry, threshold float64) int {
	count := 0
	for _, e := range entries {
		if e.Confidence >= threshold {
			count++
		}
	}
	return count
}
