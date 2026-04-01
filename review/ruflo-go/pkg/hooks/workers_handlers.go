package hooks

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
	"github.com/ruflo/ruflo-go/pkg/memory"
	"github.com/ruflo/ruflo-go/pkg/neural"
	"github.com/ruflo/ruflo-go/pkg/security"
)

// 本文件为 12 个后台 Worker 的真实 Handler 实现与依赖注入。
// ultralearn/optimize/audit 等各自扫描内存、文件系统或索引；未注入 Memory/SONA 时退化为启发式或空结果。

// workerDeps 包级可选依赖：由 SetHookWorkerDeps 注入，供各 worker 使用。
var workerDeps struct {
	Memory memory.MemoryService    // 向量记忆检索（Search）
	SONA   *neural.SONACoordinator // 模式合并与相似模式查找
}

// SetHookWorkerDeps 注入 Memory 与 SONA，供后台 Worker 使用（线程安全由调用方在适当时机一次性设置）。
func SetHookWorkerDeps(mem memory.MemoryService, sona *neural.SONACoordinator) {
	workerDeps.Memory = mem
	workerDeps.SONA = sona
}

// attachRealHandlers 将具名 Worker 的 Handler 字段替换为本文件中的具体实现。
func attachRealHandlers(cfgs []WorkerConfig) []WorkerConfig {
	handlers := map[string]func(context.Context, WorkerContext) WorkerResult{
		"ultralearn":  workerUltralearn,
		"optimize":    workerOptimize,
		"consolidate": workerConsolidate,
		"predict":     workerPredict,
		"audit":       workerAudit,
		"map":         workerMap,
		"preload":     workerPreload,
		"deepdive":    workerDeepdive,
		"document":    workerDocument,
		"refactor":    workerRefactor,
		"benchmark":   workerBenchmark,
		"testgaps":    workerTestGaps,
	}
	for i := range cfgs {
		if h, ok := handlers[cfgs[i].Name]; ok {
			cfgs[i].Handler = h
		}
	}
	return cfgs
}

// argString 从 Args map 中读取字符串键，支持 string 或 fmt.Sprint 回退。
func argString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// workerRoot 解析工作目录：优先 Args["root"]，否则当前工作目录，失败则 "."。
func workerRoot(wc WorkerContext) string {
	r := argString(wc.Args, "root")
	if r != "" {
		return r
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// workerUltralearn 深度学习/模式建议：有 Memory 时做向量检索返回命中摘要；否则根据关键词给出固定启发式 tips。
func workerUltralearn(ctx context.Context, wc WorkerContext) WorkerResult {
	query := argString(wc.Args, "query")
	if query == "" {
		query = wc.Trigger
	}
	data := map[string]any{"query": query, "suggestions": []string{}}
	if workerDeps.Memory != nil {
		res, err := workerDeps.Memory.Search(query, api.SearchOptions{K: 8, EF: 64})
		if err != nil {
			return WorkerResult{OK: false, Message: err.Error(), Data: data}
		}
		var sug []string
		for _, r := range res {
			if r.Entry != nil {
				sug = append(sug, fmt.Sprintf("%s/%s: %s", r.Entry.Namespace, r.Entry.Key, truncate(r.Entry.Value, 120)))
			}
		}
		data["suggestions"] = sug
		data["hits"] = len(sug)
		return WorkerResult{OK: true, Message: "memory pattern suggestions", Data: data}
	}
	// Heuristic tips from trigger keywords
	kw := strings.Fields(strings.ToLower(query))
	tips := []string{"capture successful edits as reusable patterns", "store task context in namespace 'patterns' before long runs"}
	for _, k := range kw {
		if strings.Contains(k, "test") {
			tips = append(tips, "mirror production paths in test layout")
		}
		if strings.Contains(k, "sec") {
			tips = append(tips, "validate inputs at CLI and MCP boundaries")
		}
	}
	data["suggestions"] = tips
	return WorkerResult{OK: true, Message: "learning suggestions (no memory backend)", Data: data}
}

// truncate 截断字符串并加省略号，用于展示。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// workerOptimize 遍历 root 下文件（跳过常见大目录与二进制扩展名），统计文件数、总字节、最大文件路径。
func workerOptimize(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	var files int64
	var total int64
	var largest string
	var maxSz int64
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if skipExt(filepath.Ext(path)) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		files++
		sz := fi.Size()
		total += sz
		if sz > maxSz {
			maxSz = sz
			largest = path
		}
		return nil
	})
	avg := int64(0)
	if files > 0 {
		avg = total / files
	}
	return WorkerResult{OK: true, Message: "file size scan", Data: map[string]any{
		"root": root, "file_count": files, "total_bytes": total, "avg_bytes": avg,
		"largest_file": largest, "largest_bytes": maxSz,
	}}
}

// skipDir 判断是否跳过目录名（vendor、node_modules 等）。
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build":
		return true
	default:
		return false
	}
}

// skipExt 判断是否跳过该扩展名（图片、压缩包等）。
func skipExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".gif", ".zip", ".tar", ".gz":
		return true
	default:
		return false
	}
}

// workerConsolidate 调用 SONA.ConsolidatePatterns(0.92) 合并近似重复模式；无 SONA 时返回 merged=0。
func workerConsolidate(ctx context.Context, wc WorkerContext) WorkerResult {
	if workerDeps.SONA == nil {
		return WorkerResult{OK: true, Message: "no SONA coordinator configured", Data: map[string]any{"merged": 0}}
	}
	n := workerDeps.SONA.ConsolidatePatterns(0.92)
	return WorkerResult{OK: true, Message: fmt.Sprintf("merged %d duplicate pattern(s)", n), Data: map[string]any{"merged": n}}
}

// workerPredict 基于 query/trigger：优先 Memory.Search；否则 SONA.FindSimilarPatterns(HashEmbed384)；再否则占位提示。
func workerPredict(ctx context.Context, wc WorkerContext) WorkerResult {
	query := argString(wc.Args, "query")
	if query == "" {
		query = wc.Trigger
	}
	data := map[string]any{"query": query}
	if workerDeps.Memory != nil {
		hits, err := workerDeps.Memory.Search(query, api.SearchOptions{K: 5})
		if err != nil {
			return WorkerResult{OK: false, Message: err.Error()}
		}
		var preds []string
		for _, h := range hits {
			if h.Entry != nil {
				preds = append(preds, h.Entry.Value)
			}
		}
		data["predictions"] = preds
		return WorkerResult{OK: true, Message: "memory-based predictions", Data: data}
	}
	if workerDeps.SONA != nil {
		vec := embeddings.HashEmbed384(query)
		sim := workerDeps.SONA.FindSimilarPatterns(vec, 5)
		var preds []string
		for _, p := range sim {
			preds = append(preds, p.Content)
		}
		data["predictions"] = preds
		return WorkerResult{OK: true, Message: "SONA similarity predictions", Data: data}
	}
	data["predictions"] = []string{"configure memory or SONA for richer predictions"}
	return WorkerResult{OK: true, Message: "fallback prediction stub", Data: data}
}

// workerAudit 在 root 下扫描 .go/.md/.json：PathValidator 校验路径、正则检测疑似硬编码密钥、InputValidator 对文件头 8KB 做阻断级校验。
func workerAudit(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return WorkerResult{OK: false, Message: err.Error()}
	}
	pv := security.NewPathValidator(absRoot)
	v := security.NewInputValidator()
	secretLike := regexp.MustCompile(`(?i)(api[_-]?key|secret|password|bearer)\s*[=:]\s*['\"]?[a-z0-9]{16,}`)
	var issues []string
	var filesScanned int
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".json") {
			return nil
		}
		if _, err := pv.Validate(path, false); err != nil {
			issues = append(issues, fmt.Sprintf("path %s: %v", path, err))
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		filesScanned++
		s := string(b)
		if secretLike.MatchString(s) {
			issues = append(issues, fmt.Sprintf("%s: possible hardcoded credential pattern", path))
		}
		res := v.ValidateString("file", s[:min(len(s), 8192)])
		for _, iss := range res.Issues {
			if iss.Severity == security.SeverityBlock {
				issues = append(issues, fmt.Sprintf("%s: %s", path, iss.Message))
			}
		}
		return nil
	})
	return WorkerResult{OK: len(issues) == 0, Message: fmt.Sprintf("scanned %d files", filesScanned), Data: map[string]any{
		"root": absRoot, "files_scanned": filesScanned, "issues": issues,
	}}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type fileMapEntry struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
	Bytes int64  `json:"bytes"`
}

func workerMap(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	var entries []fileMapEntry
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := 0
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			lines++
		}
		rel, _ := filepath.Rel(root, path)
		entries = append(entries, fileMapEntry{Path: rel, Lines: lines, Bytes: int64(len(b))})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return WorkerResult{OK: true, Message: "code map", Data: map[string]any{"root": root, "files": entries, "count": len(entries)}}
}

// workerPreload 列出 root/.claude-flow 目录条目名（预加载清单），不存在则返回空列表。
func workerPreload(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	dir := filepath.Join(root, ".claude-flow")
	fi, err := os.ReadDir(dir)
	if err != nil {
		return WorkerResult{OK: true, Message: "no .claude-flow directory", Data: map[string]any{"dir": dir, "entries": []string{}}}
	}
	var names []string
	for _, e := range fi {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return WorkerResult{OK: true, Message: "preload manifest", Data: map[string]any{"dir": dir, "entries": names}}
}

// workerDeepdive 对单文件做轻量复杂度快照：行数、func 声明数、超长行数（>120）。
func workerDeepdive(ctx context.Context, wc WorkerContext) WorkerResult {
	path := argString(wc.Args, "file")
	if path == "" {
		path = argString(wc.Args, "path")
	}
	if path == "" {
		return WorkerResult{OK: false, Message: "missing args.file or args.path"}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return WorkerResult{OK: false, Message: err.Error()}
	}
	s := string(b)
	lines := strings.Count(s, "\n") + 1
	funcs := regexp.MustCompile(`(?m)^func\s+(\([^)]+\)\s+)?(\w+)\s*\(`).FindAllString(s, -1)
	longLines := 0
	for _, line := range strings.Split(s, "\n") {
		if len(line) > 120 {
			longLines++
		}
	}
	return WorkerResult{OK: true, Message: "complexity snapshot", Data: map[string]any{
		"path": path, "bytes": len(b), "lines": lines, "func_decls": len(funcs),
		"long_lines_gt_120": longLines,
	}}
}

var goFuncSigRE = regexp.MustCompile(`(?m)^func\s+(\([^)]*\)\s+)?(\w+)\s*\([^)]*\)`)

// workerDocument 扫描非测试 .go 文件，用正则提取函数签名，最多保留 200 条。
func workerDocument(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	var sigs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range goFuncSigRE.FindAllString(string(b), -1) {
			rel, _ := filepath.Rel(root, path)
			sigs = append(sigs, rel+": "+strings.TrimSpace(m))
		}
		return nil
	})
	sort.Strings(sigs)
	if len(sigs) > 200 {
		sigs = sigs[:200]
	}
	return WorkerResult{OK: true, Message: "signatures", Data: map[string]any{"signatures": sigs, "count": len(sigs)}}
}

// workerRefactor 启发式重构提示：超长 if、长参数列表、interface{} 使用等。
func workerRefactor(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	ifPat := regexp.MustCompile(`\bif\b[^\n]{200,}`)
	longParams := regexp.MustCompile(`func\s+\w+\s*\([^)]{120,}\)`)
	var hints []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		s := string(b)
		if ifPat.MatchString(s) {
			hints = append(hints, filepath.Base(path)+": very long if condition — consider guard clauses")
		}
		if longParams.MatchString(s) {
			hints = append(hints, filepath.Base(path)+": long parameter list — consider a struct")
		}
		if strings.Contains(s, "interface{}") && !strings.Contains(path, "vendor") {
			hints = append(hints, filepath.Base(path)+": replace interface{} with any or a concrete type")
		}
		return nil
	})
	return WorkerResult{OK: true, Message: "refactor hints", Data: map[string]any{"hints": hints}}
}

// workerBenchmark 向内存 HNSW 索引插入 n 条 HashEmbed384 向量并做一次 Search，测量延迟（Args["vectors"] 可调规模）。
func workerBenchmark(ctx context.Context, wc WorkerContext) WorkerResult {
	dim := embeddings.HashEmbeddingDim
	idx := memory.NewHNSWIndex(dim, memory.CosineDistance)
	n := 2000
	if s := argString(wc.Args, "vectors"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			n = v
		}
	}
	if n > 10000 {
		n = 10000
	}
	if n < 100 {
		n = 100
	}
	for i := 0; i < n; i++ {
		vec := embeddings.HashEmbed384(fmt.Sprintf("bench-%d", i))
		if err := idx.Insert(uint64(i+1), vec); err != nil {
			return WorkerResult{OK: false, Message: err.Error()}
		}
	}
	q := embeddings.HashEmbed384("bench-query")
	start := time.Now()
	hits := idx.Search(q, 10, 64)
	elapsed := time.Since(start)
	return WorkerResult{OK: true, Message: "HNSW search timing", Data: map[string]any{
		"vectors": n, "neighbors": len(hits), "duration_ns": elapsed.Nanoseconds(),
		"duration_ms": float64(elapsed.Microseconds()) / 1000,
	}}
}

// workerTestGaps 扫描所有 .go 文件，找出缺少同 stem 的 *_test.go 的源文件列表。
func workerTestGaps(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	allGo := map[string]bool{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			rel, _ := filepath.Rel(root, path)
			allGo[rel] = true
		}
		return nil
	})
	var missing []string
	for f := range allGo {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		testF := strings.TrimSuffix(f, ".go") + "_test.go"
		if !allGo[testF] {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	return WorkerResult{OK: true, Message: "test gap scan", Data: map[string]any{"missing_test_for": missing, "count": len(missing)}}
}
