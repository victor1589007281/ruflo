// Package search 提供零成本优先的网页检索、抓取与有界研究能力。
//
// 设计文档: /home/victor/base/gateway/opencode/search-agent-design.md
//
// 分层:
//
//	L0 免 key 托管 (Parallel / Exa 匿名 MCP)
//	L1 免费 key 配额 (Exa / Tavily / Firecrawl / Jina / Linkup / Serper)
//	L2 领域直连 + HTML 兜底 (GitHub / OpenAlex / Wikipedia / Bing RSS ...)
//	L3 付费 (默认不可达, MaxCostUSD=0)
//
// 本文件只放跨包共享的类型与接口; 实现见 manager.go / scheduler.go / providers/*。
package search

import (
	"context"
	"time"
)

// Tier 是供给层级。数值越小越优先。
type Tier int

const (
	TierL0 Tier = iota // 免 key
	TierL1             // 免费 key
	TierL2             // 直连 / HTML
	TierL3             // 付费
)

// String 返回层级的可读名, 用于日志与指标标签。
func (t Tier) String() string {
	switch t {
	case TierL0:
		return "L0"
	case TierL1:
		return "L1"
	case TierL2:
		return "L2"
	case TierL3:
		return "L3"
	default:
		return "unknown"
	}
}

// Kind 是 provider 能力位。
type Kind uint8

const (
	KindSearch Kind = 1 << iota
	KindFetch
	KindAnswer
)

// KindSet 是能力位集合。
type KindSet uint8

// Has 判断集合是否包含某能力。
func (k KindSet) Has(x Kind) bool { return k&KindSet(x) != 0 }

// SearchRequest 是一次检索请求。Deadline 为零值时由调度器按 SLO 补齐。
type SearchRequest struct {
	Query            string
	OriginalQuestion string   // 原始问题, 用于 RRF 后的相关性重排
	Limit            int      // 0 = 默认 8, 最大 50
	Language         string   // zh / en; 空 = 自动
	Freshness        string   // any | day | week | month | year
	AllowedDomains   []string // Manager 硬过滤, 包含子域名
	BlockedDomains   []string // Manager 硬过滤, 优先于 AllowedDomains
	Deadline         time.Time
}

// Result 是单条检索结果。
type Result struct {
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Snippet   string    `json:"snippet"`
	Provider  string    `json:"provider"`
	Rank      int       `json:"rank"`
	Score     float64   `json:"score"`
	Published time.Time `json:"published,omitempty"`
	Lang      string    `json:"lang,omitempty"`
	Cost      float64   `json:"cost_usd"`
}

// FetchRequest 是一次抓取请求。
type FetchRequest struct {
	URL              string
	Prompt           string // 只用于相关段落选择, 不作为指令执行
	MaxChars         int    // 0 = 16000; 显式 MaxContentTokens 可进一步收紧
	MaxContentTokens int    // UTF-8 字节上界预算, 保证不超过实际 token 数; 0 = 不另设限制
	Offset           int    // 规范化全文的 rune 偏移; >0 时顺序续读
	Deadline         time.Time
}

// Document 是抓取并提取正文后的文档。
type Document struct {
	URL             string
	Title           string
	Markdown        string // 本次返回片段; Hash/Offset 以规范化全文为基准
	Byline          string
	Published       time.Time
	FetchedAt       time.Time
	Hash            string
	Truncated       bool
	Via             string // http | firecrawl | chromedp | pdftotext
	Offset          int
	NextOffset      int
	TotalChars      int
	EstimatedTokens int // 兼容字段名; 实际为返回片段的 UTF-8 字节 token 上界
	CacheHit        bool
	Stale           bool
	Selection       string
	Evidence        []Evidence
}

// Provider 是单个检索/抓取源的适配器。
//
// 实现约定:
//   - Search/Fetch 在能力未声明时必须返回 ErrUnsupported, 不要静默返回空;
//   - 所有错误用 *Error 包装, 便于调度器分类处理;
//   - Healthy 只反映本地熔断状态, 不做网络探测 (探测由 Egress 负责)。
type Provider interface {
	Name() string
	Tier() Tier
	Kinds() KindSet
	Search(ctx context.Context, req SearchRequest) ([]Result, error)
	Fetch(ctx context.Context, req FetchRequest) (*Document, error)
	Healthy() bool
}

// EgressPinned 由 provider 可选实现, 声明它必须走的出口。
// 未实现时按 Config 的默认 mode 处理。
type EgressPinned interface {
	EgressMode() Mode
}

// Weighted 由 provider 可选实现, 返回 RRF 融合权重 (默认 1.0)。
type Weighted interface {
	Weight() float64
}

// Closer 由持有长连接的 provider (MCP) 可选实现。
type Closer interface {
	Close() error
}

// SearchResponse 是 Manager 返回的完整结果, 含降级信息。
type SearchResponse struct {
	Status     string   `json:"status"`
	Results    []Result `json:"results"`
	Providers  []string `json:"providers,omitempty"`
	Degraded   []string `json:"degraded,omitempty"` // 例: proxy_down / quota_exhausted / provider_timeout
	CostUSD    float64  `json:"cost_usd"`
	CacheHit   bool     `json:"cache_hit"`
	ElapsedMS  int64    `json:"elapsed_ms"`
	GatePassed bool     `json:"gate_passed"`
	// GateReason 是质量门的失败维度 (domains/snippet/lang/duplicate/fresh);
	// 通过时为 "pass"。只暴露判据, 不暴露内部指标值。
	GateReason string `json:"gate_reason"`
}

// FetchResponse 是 Manager 抓取结果。
type FetchResponse struct {
	Document  *Document
	Degraded  []string
	ElapsedMS int64
}

// Evidence 是逐字可核验的证据片段。
type Evidence struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Quote     string    `json:"quote"`
	Offset    int       `json:"offset"`
	Hash      string    `json:"hash"`
	FetchedAt time.Time `json:"fetched_at"`
}
