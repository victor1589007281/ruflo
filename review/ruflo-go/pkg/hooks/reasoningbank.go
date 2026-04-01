// 推理银行（ReasoningBank）：存储从历史任务中提取的经验模式（GuidancePattern），
// 用于任务路由（RouteTask）、指导生成（GenerateGuidance）以及模式检索（SearchPatterns）。
//
// # 设计思路
//
// ReasoningBank 是 Ruflo 自学习闭环的核心组件之一，与 SONA 和 EWC 配合：
//   - SONA 收集轨迹 → 产出模式 → 存入 ReasoningBank
//   - ReasoningBank 根据模式的 Embedding 向量做余弦相似度检索（类 HNSW 线性扫描）
//   - 路由时优先匹配正则规则（AGENT_PATTERNS），其次用 Embedding 近邻补充
//   - 去重机制：新模式入库时若与已有模式余弦相似度 ≥ 0.95，合并使用计数
//   - 晋升机制：UsageCount ≥ 3 且 Quality ≥ 0.6 的模式晋升为长期模式（LongTerm）
//
// # 会话边界
//
// OnSessionStart/OnSessionEnd 追踪当前会话的变更次数，会话结束时对所有模式尝试晋升。
package hooks

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/ruflo/ruflo-go/api"
)

// dedupSimilarityThreshold 去重相似度阈值：余弦相似度 ≥ 0.95 视为同一模式，合并而非新增。
const dedupSimilarityThreshold = 0.95

// GuidancePattern 可学习的路由/指导模式单元。
//   - ID: 唯一标识符（"pat_" + 16 位十六进制）
//   - Strategy: 关联的策略名称（可用于存储推荐的 Agent 类型）
//   - Domain: 所属领域（如 "security"、"memory"、"swarm"）
//   - Embedding: 可选的向量嵌入，用于语义相似度检索
//   - Quality: 模式质量评分 [0, 1]
//   - UsageCount: 被引用次数（越高 → Fisher 信息越大 → EWC 保护越强）
//   - SuccessCount: 成功应用次数
//   - LongTerm: 是否已晋升为长期模式
type GuidancePattern struct {
	ID           string    `json:"id"`
	Strategy     string    `json:"strategy"`
	Domain       string    `json:"domain"`
	Embedding    []float32 `json:"embedding,omitempty"`
	Quality      float64   `json:"quality"`
	UsageCount   int       `json:"usage_count"`
	SuccessCount int       `json:"success_count"`
	LongTerm     bool      `json:"long_term"`
}

// RoutingResult 任务路由结果：推荐的 Agent 类型、置信度、备选方案和决策原因。
type RoutingResult struct {
	Agent        api.AgentType   `json:"agent"`
	Confidence   float64         `json:"confidence"`
	Alternatives []api.AgentType `json:"alternatives,omitempty"`
	Reason       string          `json:"reason,omitempty"`
}

// AGENT_PATTERNS 任务描述正则 → 推荐 Agent 类型的静态路由表。
// RouteTask 优先使用此表做规则匹配（置信度 0.75），未命中再用 Embedding 近邻补充。
var AGENT_PATTERNS = map[string]api.AgentType{
	`(?i)\bsecurity\b|\baudit\b|\bcve\b|\bvulnerabilit`:  api.AgentTypeSecurityArchitect,
	`(?i)\btest\b|\bqa\b|\bcoverage\b|\bspec\b`:          api.AgentType("test-architect"),
	`(?i)\bperf\b|\bperformance\b|\blatency\b|\bprofile`: api.AgentTypePerformanceEngineer,
	`(?i)\brefactor\b|\bclean\b|\btech debt\b`:           api.AgentTypeArchitect,
	`(?i)\bdoc\b|\breadme\b|\bmarkdown\b`:                api.AgentTypeResearcher,
	`(?i)\bimplement\b|\bcode\b|\bfix bug\b|\bfeature\b`: api.AgentTypeCoder,
	`(?i)\breview\b|\bpr\b|\bpull request\b`:             api.AgentTypeReviewer,
}

// DOMAIN_GUIDANCE 领域关键字 → 指导文案模板。GenerateGuidance 按子串包含匹配，拼接对应文案。
var DOMAIN_GUIDANCE = map[string]string{
	"security": "Apply least privilege, validate inputs, avoid secrets in code, and document threat assumptions.",
	"memory":   "Prefer namespaces, TTL for ephemeral data, and embed for semantic recall when available.",
	"swarm":    "Use hierarchical topology for anti-drift; cap concurrent agents; checkpoint often.",
	"neural":   "Record trajectories; judge outcomes; distill patterns; consolidate with EWC to avoid forgetting.",
	"default":  "Validate at system boundaries; keep changes minimal and test-critical paths.",
}

// ReasoningBankStats 推理银行的统计指标：模式总数、短期/长期数量、平均质量。
type ReasoningBankStats struct {
	TotalPatterns int
	ShortTerm     int
	LongTerm      int
	AvgQuality    float64
}

// ReasoningBank 经验模式库，支持基于余弦相似度的向量检索和基于正则的任务路由。
//   - patterns: 模式存储（线性扫描，适用于中等规模；大规模可替换为 HNSW 索引）
//   - compiled/keys: AGENT_PATTERNS 正则的预编译缓存
//   - activeSession/sessionMutations: 当前会话追踪
type ReasoningBank struct {
	mu               sync.RWMutex
	patterns         []*GuidancePattern
	compiled         []*regexp.Regexp
	keys             []string
	activeSession    string
	sessionMutations int
}

// NewReasoningBank 构造空的推理银行，并预编译 AGENT_PATTERNS 中的所有正则表达式。
func NewReasoningBank() *ReasoningBank {
	rb := &ReasoningBank{}
	for pat := range AGENT_PATTERNS {
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		rb.compiled = append(rb.compiled, re)
		rb.keys = append(rb.keys, pat)
	}
	return rb
}

// StorePattern 插入新模式或按 Embedding 余弦相似度去重（≥ 0.95 时合并到已有模式的使用计数和质量中）。
func (rb *ReasoningBank) StorePattern(p *GuidancePattern) (*GuidancePattern, error) {
	if p == nil {
		return nil, ErrInvalidRegistration
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if p.ID == "" {
		p.ID = randomPatternID()
	}
	if len(p.Embedding) > 0 {
		for _, ex := range rb.patterns {
			if len(ex.Embedding) == len(p.Embedding) {
				if cosine32(p.Embedding, ex.Embedding) >= dedupSimilarityThreshold {
					ex.UsageCount++
					ex.Quality = math.Max(ex.Quality, p.Quality)
					rb.maybePromoteLocked(ex)
					return ex, nil
				}
			}
		}
	}
	cp := *p
	if cp.Embedding != nil {
		cp.Embedding = append([]float32(nil), cp.Embedding...)
	}
	rb.patterns = append(rb.patterns, &cp)
	rb.maybePromoteLocked(&cp)
	return &cp, nil
}

func (rb *ReasoningBank) maybePromoteLocked(p *GuidancePattern) {
	if p.UsageCount >= 3 && p.Quality >= 0.6 {
		p.LongTerm = true
	}
}

// SearchPatterns 按 Embedding 余弦相似度检索 top-K 模式（当前为线性扫描，可替换为 HNSW 加速）。
// 返回深拷贝切片，按相似度降序排列。时间复杂度 O(N·d)，N 为模式数，d 为向量维度。
func (rb *ReasoningBank) SearchPatterns(queryEmbedding []float32, topK int) []*GuidancePattern {
	if topK <= 0 {
		topK = 8
	}
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	type scored struct {
		p *GuidancePattern
		s float64
	}
	var buf []scored
	for _, p := range rb.patterns {
		if len(queryEmbedding) == 0 || len(p.Embedding) != len(queryEmbedding) {
			continue
		}
		buf = append(buf, scored{p: p, s: cosine32(queryEmbedding, p.Embedding)})
	}
	sort.Slice(buf, func(i, j int) bool { return buf[i].s > buf[j].s })
	out := make([]*GuidancePattern, 0, topK)
	for i := 0; i < len(buf) && i < topK; i++ {
		pc := *buf[i].p
		if pc.Embedding != nil {
			pc.Embedding = append([]float32(nil), pc.Embedding...)
		}
		out = append(out, &pc)
	}
	return out
}

// RouteTask 根据任务描述选择推荐的 Agent 类型。
// 路由策略：先用 AGENT_PATTERNS 正则匹配（置信度 0.75）；若未命中或有 Embedding，
// 则用 SearchPatterns 检索近邻的 Strategy 字段补充（置信度 0.72）。
// 默认回退到 Coder（置信度 0.4）。
func (rb *ReasoningBank) RouteTask(description string, queryEmb []float32) RoutingResult {
	desc := strings.TrimSpace(description)
	var best api.AgentType
	var bestRe string
	conf := 0.35
	for i, re := range rb.compiled {
		if re.MatchString(desc) {
			key := rb.keys[i]
			ag := AGENT_PATTERNS[key]
			best = ag
			bestRe = key
			conf = 0.75
			break
		}
	}
	if len(queryEmb) > 0 {
		neighbors := rb.SearchPatterns(queryEmb, 3)
		if len(neighbors) > 0 && neighbors[0].Strategy != "" {
			// Strategy field may hold agent type name
			if t := api.AgentType(neighbors[0].Strategy); t != "" {
				if conf < 0.8 {
					best = t
					conf = 0.72
				}
			}
		}
	}
	alts := []api.AgentType{api.AgentTypeCoder, api.AgentTypeArchitect, api.AgentTypeReviewer}
	if best == "" {
		best = api.AgentTypeCoder
		conf = 0.4
	}
	return RoutingResult{
		Agent:        best,
		Confidence:   conf,
		Alternatives: alts,
		Reason:       bestRe,
	}
}

// PromotePattern 手动晋升指定模式为长期模式（条件：UsageCount ≥ 3 且 Quality ≥ 0.6）。
func (rb *ReasoningBank) PromotePattern(id string) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for _, p := range rb.patterns {
		if p.ID == id {
			if p.UsageCount >= 3 && p.Quality >= 0.6 {
				p.LongTerm = true
				return true
			}
			return false
		}
	}
	return false
}

// cosine32 计算两个 float32 向量的余弦相似度：dot(a,b) / (||a|| · ||b||)，返回值范围 [-1, 1]。
// 时间复杂度 O(d)。
func cosine32(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func randomPatternID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "pat_" + hex.EncodeToString(b[:])
}

// RecordOutcome 记录模式的使用结果（成功/失败），递增使用和成功计数，并尝试晋升。
func (rb *ReasoningBank) RecordOutcome(patternID string, success bool) error {
	if patternID == "" {
		return ErrInvalidRegistration
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for _, p := range rb.patterns {
		if p != nil && p.ID == patternID {
			p.UsageCount++
			if success {
				p.SuccessCount++
			}
			rb.maybePromoteLocked(p)
			if rb.activeSession != "" {
				rb.sessionMutations++
			}
			return nil
		}
	}
	return ErrPatternNotFound
}

// Consolidate 双重循环合并：对每对 Embedding 维度相同且余弦≥threshold 的模式，将后者计数合并入前者并删除后者；返回合并次数。
func (rb *ReasoningBank) Consolidate(threshold float64) int {
	if threshold <= 0 {
		threshold = dedupSimilarityThreshold
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	merged := 0
	for i := 0; i < len(rb.patterns); i++ {
		pi := rb.patterns[i]
		if pi == nil {
			continue
		}
		j := i + 1
		for j < len(rb.patterns) {
			pj := rb.patterns[j]
			if pj == nil {
				j++
				continue
			}
			sim := 0.0
			if len(pi.Embedding) > 0 && len(pi.Embedding) == len(pj.Embedding) {
				sim = cosine32(pi.Embedding, pj.Embedding)
			}
			if sim >= threshold {
				pi.UsageCount += pj.UsageCount
				pi.SuccessCount += pj.SuccessCount
				pi.Quality = math.Max(pi.Quality, pj.Quality)
				if pj.LongTerm {
					pi.LongTerm = true
				}
				rb.patterns = append(rb.patterns[:j], rb.patterns[j+1:]...)
				merged++
				continue
			}
			j++
		}
	}
	return merged
}

// GenerateGuidance 将描述转小写后按子串包含匹配 DOMAIN_GUIDANCE 键，去重拼接；无命中返回 default 文案。
func (rb *ReasoningBank) GenerateGuidance(taskDescription string) string {
	desc := strings.ToLower(strings.TrimSpace(taskDescription))
	var parts []string
	seen := map[string]struct{}{}
	for dom, text := range DOMAIN_GUIDANCE {
		if dom == "default" {
			continue
		}
		if strings.Contains(desc, dom) {
			if _, ok := seen[text]; ok {
				continue
			}
			seen[text] = struct{}{}
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return DOMAIN_GUIDANCE["default"]
	}
	return strings.Join(parts, " ")
}

// GetStats 遍历 patterns 计算总量、长期数量与平均质量。
func (rb *ReasoningBank) GetStats() ReasoningBankStats {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	var sum float64
	lt := 0
	for _, p := range rb.patterns {
		if p == nil {
			continue
		}
		sum += p.Quality
		if p.LongTerm {
			lt++
		}
	}
	n := len(rb.patterns)
	avg := 0.0
	if n > 0 {
		avg = sum / float64(n)
	}
	return ReasoningBankStats{
		TotalPatterns: n,
		ShortTerm:     n - lt,
		LongTerm:      lt,
		AvgQuality:    avg,
	}
}

// ExportPatterns 深拷贝导出全部模式（含 Embedding 切片拷贝）。
func (rb *ReasoningBank) ExportPatterns() []GuidancePattern {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	out := make([]GuidancePattern, 0, len(rb.patterns))
	for _, p := range rb.patterns {
		if p == nil {
			continue
		}
		cp := *p
		if cp.Embedding != nil {
			cp.Embedding = append([]float32(nil), cp.Embedding...)
		}
		out = append(out, cp)
	}
	return out
}

// ImportPatterns 批量导入外部模式：逐条加锁检查 Embedding 相似度；
// 相似度 ≥ 阈值时合并到已有模式（计数+质量取 max），否则追加新模式。返回新插入条数。
func (rb *ReasoningBank) ImportPatterns(patterns []GuidancePattern) int {
	n := 0
	for i := range patterns {
		p := patterns[i]
		cp := p
		if cp.Embedding != nil {
			cp.Embedding = append([]float32(nil), cp.Embedding...)
		}
		rb.mu.Lock()
		if cp.ID == "" {
			cp.ID = randomPatternID()
		}
		merged := false
		if len(cp.Embedding) > 0 {
			for _, ex := range rb.patterns {
				if ex == nil {
					continue
				}
				if len(ex.Embedding) == len(cp.Embedding) && cosine32(ex.Embedding, cp.Embedding) >= dedupSimilarityThreshold {
					ex.UsageCount++
					ex.Quality = math.Max(ex.Quality, cp.Quality)
					rb.maybePromoteLocked(ex)
					merged = true
					break
				}
			}
		}
		if !merged {
			rb.patterns = append(rb.patterns, &cp)
			rb.maybePromoteLocked(&cp)
			n++
		}
		rb.mu.Unlock()
	}
	return n
}

// OnSessionStart 记录当前会话并重置 sessionMutations。
func (rb *ReasoningBank) OnSessionStart(sessionID string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.activeSession = sessionID
	rb.sessionMutations = 0
}

// OnSessionEnd 若 sessionID 匹配当前会话则对所有模式尝试晋升并清空会话状态。
func (rb *ReasoningBank) OnSessionEnd(sessionID string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.activeSession != sessionID && sessionID != "" {
		return
	}
	for _, p := range rb.patterns {
		if p != nil {
			rb.maybePromoteLocked(p)
		}
	}
	rb.activeSession = ""
	rb.sessionMutations = 0
}
