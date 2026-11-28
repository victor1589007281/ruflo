// semantic_search.go — 符号级语义检索（query_type=semantic）。
//
// 为什么要有它：intake/建模阶段的问题是「哪里做 X」——调用方只有自然语言意图，
// 没有符号名。navigate/find_refs 都要求先猜中符号名再查，猜不中就要多轮 rg 兜底。
// semantic 用「符号名 + 种类 + 签名 + 上方文档注释」组成的语料做向量检索，
// 让「哪里做压缩 / compaction」一次命中 runCompaction，把"猜符号→navigate"的
// 多轮循环压成一次查询。
//
// 设计约束（与 vector_cache.go 同源思想）：
//   - 向量提供方双模：优先 Ollama /api/embed（真语义，跨语言）；不可达时退化为
//     字符 3-gram TF 哈希向量（纯词面相似，仍可用）。选择结果随索引持久化，
//     提供方变化触发全量重嵌入。
//   - 语料抽取不依赖 gitnexus 图 schema：直接按语言正则扫定义行 + 上方连续注释块，
//     跳过 vendor/node_modules/.git，单文件 >2MB 跳过。支持 Go/TS/JS/Python/Rust。
//   - 索引持久化在 <repo>/.claude-code-intel/semantic_index.json，按符号内容 hash
//     增量复用旧向量；首次 semantic 查询时惰性构建，不动 init/update 管线。
//   - 检索为暴力余弦 top-K：10 万符号 ×768 维 ≈ 几千万次乘加，毫秒级，无需 LSH。
//
// 纪律：semantic 只产出**候选**，file:line 必须回到 navigate/find_refs/源码行为行
// 核实后才能写进任何审计结论。
package codeintel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 配置
// ============================================================================

const (
	semanticIndexFileName = "semantic_index.json"
	semanticMaxFileBytes  = 2 << 20 // 单文件 >2MB 跳过
	semanticDefaultTopN   = 8
	semanticMaxTopN       = 25
	ngramVectorDims       = 1024
	semanticEmbedBatch    = 32
	semanticEmbedTimeout  = 60 * time.Second
)

// ============================================================================
// 语料：符号文档
// ============================================================================

// SymbolDoc 一条可检索的符号文档。
type SymbolDoc struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // func/method/type/class/struct
	File string `json:"file"` // 仓库相对路径
	Line int    `json:"line"`
	Sig  string `json:"sig,omitempty"`  // 定义行原文（截断）
	Doc  string `json:"doc,omitempty"`  // 定义行上方连续注释块（截断）
	Hash string `json:"hash"`           // name+kind+sig+doc 的内容指纹
}

// embedText 拼出送嵌入的文本：符号名（驼峰/下划线拆开）+ 种类 + 文档 + 签名。
func (d *SymbolDoc) embedText() string {
	var b strings.Builder
	b.WriteString(splitIdentifier(d.Name))
	b.WriteString(" (")
	b.WriteString(d.Kind)
	b.WriteString(")")
	if d.Doc != "" {
		b.WriteString(": ")
		b.WriteString(d.Doc)
	}
	if d.Sig != "" {
		b.WriteString(" | ")
		b.WriteString(d.Sig)
	}
	return b.String()
}

var identifierSplitRe = regexp.MustCompile(`([a-z0-9])([A-Z])|_+`)

// splitIdentifier 把 camelCase/snake_case 拆成空格分隔的小写词，提升词面召回。
func splitIdentifier(s string) string {
	s = identifierSplitRe.ReplaceAllString(s, "$1 $2 ")
	return strings.ToLower(s)
}

// defPattern 一种语言的定义行模式。group 1 = 符号名，kind 固定。
type defPattern struct {
	re   *regexp.Regexp
	kind string
}

// 每种语言按「函数 / 类型」给两条模式；宁缺毋滥——误报的定义会污染语料。
var langDefPatterns = map[string][]defPattern{
	".go": {
		{regexp.MustCompile(`^func\s+(?:\([^)]*\)\s+)?([A-Za-z_]\w*)\s*\(`), "func"},
		{regexp.MustCompile(`^type\s+([A-Za-z_]\w*)\s+(?:struct|interface)`), "type"},
	},
	".ts": {
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_]\w*)`), "func"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:abstract\s+)?class\s+([A-Za-z_]\w*)`), "class"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let)\s+([A-Za-z_]\w*)\s*=\s*(?:async\s*)?\(`), "func"},
		{regexp.MustCompile(`^\s*(?:export\s+)?interface\s+([A-Za-z_]\w*)`), "type"},
	},
	".py": {
		{regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`), "func"},
		{regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`), "class"},
	},
	".rs": {
		{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?fn\s+([A-Za-z_]\w*)`), "func"},
		{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait)\s+([A-Za-z_]\w*)`), "type"},
	},
}
// .tsx/.jsx/.mjs/.cjs 复用 .ts 模式。
var tsLikeExts = map[string]bool{".tsx": true, ".jsx": true, ".mjs": true, ".cjs": true}

var semanticSkipDirs = map[string]bool{
	"vendor": true, "node_modules": true, ".git": true, "dist": true,
	"build": true, "out": true, "testdata": true, ".claude-code-intel": true,
}

// extractSymbolDocs 扫描仓库，抽出全部符号文档。
func extractSymbolDocs(repoPath string) ([]SymbolDoc, error) {
	var docs []SymbolDoc
	err := filepath.WalkDir(repoPath, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个目录不可读不终止全扫
		}
		name := d.Name()
		if d.IsDir() {
			if semanticSkipDirs[name] || strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		patterns, ok := langDefPatterns[ext]
		if !ok && tsLikeExts[ext] {
			patterns, ok = langDefPatterns[".ts"], true
		}
		if !ok {
			return nil
		}
		if fi, e := d.Info(); e == nil && fi.Size() > semanticMaxFileBytes {
			return nil
		}
		rel, e := filepath.Rel(repoPath, p)
		if e != nil {
			return nil
		}
		docs = append(docs, extractFromFile(p, filepath.ToSlash(rel), patterns)...)
		return nil
	})
	return docs, err
}

// extractFromFile 逐行匹配定义模式，并向上收集连续注释块作为 Doc。
func extractFromFile(absPath, relPath string, patterns []defPattern) []SymbolDoc {
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	var out []SymbolDoc
	for i, line := range lines {
		for _, dp := range patterns {
			m := dp.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			doc := collectLeadingComment(lines, i)
			sig := strings.TrimSpace(line)
			if len(sig) > 160 {
				sig = sig[:160]
			}
			d := SymbolDoc{Name: m[1], Kind: dp.kind, File: relPath, Line: i + 1, Sig: sig, Doc: doc}
			d.Hash = symbolDocHash(&d)
			out = append(out, d)
			break
		}
	}
	return out
}

// collectLeadingComment 向上收集紧邻的 // 与 /* */ 注释块（最多 6 行，去注释符）。
// 空行或非注释行终止——隔着空行的注释不算这条定义的文档。
func collectLeadingComment(lines []string, defIdx int) string {
	var buf []string
	for j := defIdx - 1; j >= 0 && len(buf) < 6; j-- {
		t := strings.TrimSpace(lines[j])
		isComment := strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") ||
			strings.HasPrefix(t, "*") || strings.HasSuffix(t, "*/")
		if !isComment {
			break
		}
		c := strings.TrimSpace(strings.TrimPrefix(strings.Trim(t, "/* "), "//"))
		if c != "" {
			buf = append([]string{c}, buf...)
		}
	}
	doc := strings.Join(buf, " ")
	if len(doc) > 240 {
		doc = doc[:240]
	}
	return doc
}

func symbolDocHash(d *SymbolDoc) string {
	h := sha256.Sum256([]byte(d.Name + "|" + d.Kind + "|" + d.Sig + "|" + d.Doc))
	return hex.EncodeToString(h[:8])
}

// ============================================================================
// 嵌入提供方
// ============================================================================

// EmbeddingProvider 把文本批量映射为向量。
type EmbeddingProvider interface {
	Name() string
	Embed(texts []string) ([][]float32, error)
}

// ollamaProvider 走本机 Ollama /api/embed（nomic-embed-text 等）。
// 服务端跑在宿主，Pod 里的调用方经 HTTP 查 codeintel，嵌入只发生在服务端，
// 因此 127.0.0.1 默认对两类调用方都成立。
type ollamaProvider struct {
	base  string
	model string
	hc    *http.Client
}

func newOllamaProvider() *ollamaProvider {
	base := os.Getenv("CODEINTEL_EMBED_BASE")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	model := os.Getenv("CODEINTEL_EMBED_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}
	return &ollamaProvider{base: strings.TrimRight(base, "/"), model: model, hc: &http.Client{Timeout: semanticEmbedTimeout}}
}

func (p *ollamaProvider) Name() string { return "ollama:" + p.model }

func (p *ollamaProvider) Embed(texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]interface{}{"model": p.model, "input": texts})
	resp, err := p.hc.Post(p.base+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed status %d", resp.StatusCode)
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed 返回 %d 条向量，输入 %d 条", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}

// ngramProvider 字符 3-gram TF 哈希向量（词面相似兜底，无外部依赖）。
type ngramProvider struct{}

func (ngramProvider) Name() string { return "ngram3" }

func (ngramProvider) Embed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = ngramVector(t)
	}
	return out, nil
}

// ngramVector 与 ExtractFeatures 同源：字符 3-gram TF，哈希进固定维度后 L2 归一。
func ngramVector(text string) []float32 {
	v := make([]float32, ngramVectorDims)
	runes := []rune(strings.ToLower(text))
	if len(runes) < 3 {
		runes = append(runes, []rune("   ")[:3-len(runes)]...)
	}
	for i := 0; i+3 <= len(runes); i++ {
		h := sha256.Sum256([]byte(string(runes[i : i+3])))
		idx := int(h[0])<<8 | int(h[1])
		v[idx%ngramVectorDims] += 1
	}
	normalizeVec(v)
	return v
}

func normalizeVec(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	n := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= n
	}
}

func cosineVec(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// pickProvider 优先 Ollama（探测一次 /api/tags），失败退化 ngram。
func pickProvider() EmbeddingProvider {
	p := newOllamaProvider()
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get(p.base + "/api/tags")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return p
		}
	}
	return ngramProvider{}
}

// ============================================================================
// 持久化索引
// ============================================================================

// semanticIndex 落盘结构：符号 + 向量 + 提供方名（提供方变化触发全量重嵌）。
type semanticIndex struct {
	Provider  string      `json:"provider"`
	BuiltAt   time.Time   `json:"built_at"`
	Symbols   []SymbolDoc `json:"symbols"`
	Vectors   [][]float32 `json:"vectors"`
	DocCount  int         `json:"doc_count"`
	ReusedVec int         `json:"reused_vectors"` // 构建时复用的旧向量数（观测增量效果）
}

func semanticIndexPath(repoPath string) string {
	return filepath.Join(repoPath, vectorCacheDir, semanticIndexFileName)
}

func loadSemanticIndex(repoPath string) *semanticIndex {
	raw, err := os.ReadFile(semanticIndexPath(repoPath))
	if err != nil {
		return nil
	}
	var idx semanticIndex
	if json.Unmarshal(raw, &idx) != nil || len(idx.Symbols) != len(idx.Vectors) {
		return nil
	}
	return &idx
}

// buildSemanticIndex 重扫语料，按 hash 复用旧向量，只嵌新增/变化的符号。
func buildSemanticIndex(repoPath string, old *semanticIndex, provider EmbeddingProvider) (*semanticIndex, error) {
	docs, err := extractSymbolDocs(repoPath)
	if err != nil {
		return nil, err
	}
	oldVec := map[string][]float32{}
	if old != nil && old.Provider == provider.Name() {
		for i, d := range old.Symbols {
			oldVec[d.Hash] = old.Vectors[i]
		}
	}
	idx := &semanticIndex{Provider: provider.Name(), BuiltAt: time.Now(), Symbols: docs, Vectors: make([][]float32, len(docs))}
	var pending []int
	for i, d := range docs {
		if v, ok := oldVec[d.Hash]; ok {
			idx.Vectors[i] = v
			idx.ReusedVec++
		} else {
			pending = append(pending, i)
		}
	}
	for start := 0; start < len(pending); start += semanticEmbedBatch {
		end := start + semanticEmbedBatch
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]
		texts := make([]string, len(batch))
		for j, bi := range batch {
			texts[j] = docs[bi].embedText()
		}
		vecs, err := provider.Embed(texts)
		if err != nil {
			return nil, fmt.Errorf("embed 批次 %d-%d 失败: %w", start, end, err)
		}
		for j, bi := range batch {
			idx.Vectors[bi] = vecs[j]
		}
	}
	idx.DocCount = len(docs)
	if dir := filepath.Dir(semanticIndexPath(repoPath)); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if raw, err := json.Marshal(idx); err == nil {
		_ = os.WriteFile(semanticIndexPath(repoPath), raw, 0o644)
	}
	return idx, nil
}

// ============================================================================
// 查询
// ============================================================================

// semanticIndexLocks 按仓库串行化构建，避免并发查询重复全量嵌入。
var (
	semanticIndexLocksMu sync.Mutex
	semanticIndexLocks   = map[string]*sync.Mutex{}
)

func semanticLock(repoPath string) *sync.Mutex {
	semanticIndexLocksMu.Lock()
	defer semanticIndexLocksMu.Unlock()
	l, ok := semanticIndexLocks[repoPath]
	if !ok {
		l = &sync.Mutex{}
		semanticIndexLocks[repoPath] = l
	}
	return l
}

// SemanticHit 一条检索命中。
type SemanticHit struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	File  string  `json:"file"`
	Line  int     `json:"line"`
	Score float64 `json:"score"`
	Doc   string  `json:"doc,omitempty"`
}

// SemanticSearch 语义检索入口：惰性构建/增量更新索引后做余弦 top-K。
//
// 每次查询都重扫语料（纯词法扫描，agentDB 规模 <1s）再按 hash 复用向量，
// 保证结果反映当前工作区，而不是上次构建时的快照。
func (e *Engine) SemanticSearch(query string, topN int) (*QueryResult, error) {
	start := time.Now()
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: query_type=semantic missing symbol(自然语言查询)", ErrEmptyQueryTarget)
	}
	if topN <= 0 {
		topN = semanticDefaultTopN
	}
	if topN > semanticMaxTopN {
		topN = semanticMaxTopN
	}
	lock := semanticLock(e.repoPath)
	lock.Lock()
	defer lock.Unlock()

	provider := pickProvider()
	idx, err := buildSemanticIndex(e.repoPath, loadSemanticIndex(e.repoPath), provider)
	if err != nil {
		return nil, err
	}
	if len(idx.Symbols) == 0 {
		return &QueryResult{QueryType: "semantic", Results: map[string]interface{}{
			"hits": []SemanticHit{}, "corpus": 0,
			"note": "语料为空：仓库可能只有未支持语言（当前支持 Go/TS/JS/Python/Rust）",
		}}, nil
	}
	qv, err := provider.Embed([]string{query})
	if err != nil {
		return nil, fmt.Errorf("查询嵌入失败: %w", err)
	}
	type scored struct {
		i int
		s float64
	}
	all := make([]scored, len(idx.Symbols))
	for i := range idx.Symbols {
		all[i] = scored{i, cosineVec(qv[0], idx.Vectors[i])}
	}
	sort.Slice(all, func(a, b int) bool { return all[a].s > all[b].s })
	hits := make([]SemanticHit, 0, topN)
	for _, sc := range all[:min(topN, len(all))] {
		d := idx.Symbols[sc.i]
		hits = append(hits, SemanticHit{Name: d.Name, Kind: d.Kind, File: d.File, Line: d.Line,
			Score: math.Round(sc.s*1000) / 1000, Doc: d.Doc})
	}
	qr := &QueryResult{QueryType: "semantic", Results: map[string]interface{}{
		"hits": hits, "corpus": len(idx.Symbols), "provider": idx.Provider,
		"reused_vectors": idx.ReusedVec,
		"note":           "候选非证据：命中需回 navigate/find_refs/源码行为行核实",
	}, LatencyMs: time.Since(start).Milliseconds()}
	return qr, nil
}
