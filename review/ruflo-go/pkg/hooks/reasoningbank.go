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

const dedupSimilarityThreshold = 0.95

// GuidancePattern is a learnable routing / guidance unit with optional embedding for HNSW-like retrieval.
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

// RoutingResult is produced by task routing heuristics.
type RoutingResult struct {
	Agent        api.AgentType   `json:"agent"`
	Confidence   float64         `json:"confidence"`
	Alternatives []api.AgentType `json:"alternatives,omitempty"`
	Reason       string          `json:"reason,omitempty"`
}

// AGENT_PATTERNS maps task-description regexes to preferred agent types.
var AGENT_PATTERNS = map[string]api.AgentType{
	`(?i)\bsecurity\b|\baudit\b|\bcve\b|\bvulnerabilit`:  api.AgentTypeSecurityArchitect,
	`(?i)\btest\b|\bqa\b|\bcoverage\b|\bspec\b`:          api.AgentType("test-architect"),
	`(?i)\bperf\b|\bperformance\b|\blatency\b|\bprofile`: api.AgentTypePerformanceEngineer,
	`(?i)\brefactor\b|\bclean\b|\btech debt\b`:           api.AgentTypeArchitect,
	`(?i)\bdoc\b|\breadme\b|\bmarkdown\b`:                api.AgentTypeResearcher,
	`(?i)\bimplement\b|\bcode\b|\bfix bug\b|\bfeature\b`: api.AgentTypeCoder,
	`(?i)\breview\b|\bpr\b|\bpull request\b`:             api.AgentTypeReviewer,
}

// DOMAIN_GUIDANCE holds domain-specific template strings keyed by domain label.
var DOMAIN_GUIDANCE = map[string]string{
	"security": "Apply least privilege, validate inputs, avoid secrets in code, and document threat assumptions.",
	"memory":   "Prefer namespaces, TTL for ephemeral data, and embed for semantic recall when available.",
	"swarm":    "Use hierarchical topology for anti-drift; cap concurrent agents; checkpoint often.",
	"neural":   "Record trajectories; judge outcomes; distill patterns; consolidate with EWC to avoid forgetting.",
	"default":  "Validate at system boundaries; keep changes minimal and test-critical paths.",
}

// ReasoningBankStats summarizes stored guidance patterns.
type ReasoningBankStats struct {
	TotalPatterns int
	ShortTerm     int
	LongTerm      int
	AvgQuality    float64
}

// ReasoningBank stores and retrieves guidance patterns with vector similarity (cosine; pluggable HNSW).
type ReasoningBank struct {
	mu               sync.RWMutex
	patterns         []*GuidancePattern
	compiled         []*regexp.Regexp
	keys             []string
	activeSession    string
	sessionMutations int
}

// NewReasoningBank returns an empty bank with compiled regex keys.
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

// StorePattern inserts or deduplicates by embedding similarity (>= dedup threshold merges usage).
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

// SearchPatterns returns top-K patterns by cosine similarity to queryEmbedding (HNSW-like linear scan).
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

// RouteTask picks an agent type from description regexes and optional embedding neighbors.
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

// PromotePattern marks short-term patterns as long-term when thresholds met (also applied in StorePattern).
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

// RecordOutcome updates usage and success tallies for a pattern id.
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

// Consolidate merges embedding-similar patterns (cosine >= threshold); returns number of merges performed.
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

// GenerateGuidance returns domain-specific guidance text inferred from the task description.
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

// GetStats returns aggregate pattern statistics.
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

// ExportPatterns returns a deep copy of all patterns.
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

// ImportPatterns inserts patterns with deduplication by embedding similarity; returns count of newly stored rows.
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

// OnSessionStart resets session-scoped bookkeeping.
func (rb *ReasoningBank) OnSessionStart(sessionID string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.activeSession = sessionID
	rb.sessionMutations = 0
}

// OnSessionEnd finalizes session bookkeeping and attempts promotion passes.
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
