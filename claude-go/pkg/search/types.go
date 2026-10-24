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
	Query          string
	Limit          int      // 0 = 默认 8
	Language       string   // zh / en; 空 = 自动
	Freshness      string   // any | day | week | month | year
	AllowedDomains []string // 仅作偏好提示, 不保证所有 provider 支持
	Deadline       time.Time
}

// Result 是单条检索结果。
type Result struct {
	URL       string
	Title     string
	Snippet   string
	Provider  string
	Rank      int
	Score     float64
	Published time.Time
	Lang      string
	Cost      float64
}

// FetchRequest 是一次抓取请求。
type FetchRequest struct {
	URL      string
	MaxChars int // 0 = 16000
	Deadline time.Time
}

// Document 是抓取并提取正文后的文档。
type Document struct {
	URL       string
	Title     string
	Markdown  string
	Byline    string
	Published time.Time
	FetchedAt time.Time
	Hash      string
	Truncated bool
	Via       string // http | firecrawl | chromedp
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
	Results    []Result
	Providers  []string
	Degraded   []string // 例: proxy_down / quota_exhausted / provider_timeout
	CostUSD    float64
	CacheHit   bool
	ElapsedMS  int64
	GatePassed bool
	// GateReason 是质量门的失败维度 (domains/snippet/lang/duplicate/fresh);
	// 通过时为 "pass"。只暴露判据, 不暴露内部指标值。
	GateReason string
}

// FetchResponse 是 Manager 抓取结果。
type FetchResponse struct {
	Document  *Document
	Degraded  []string
	ElapsedMS int64
}

// Evidence 是逐字可核验的证据片段。
type Evidence struct {
	ID        string
	URL       string
	Quote     string
	Offset    int
	Hash      string
	FetchedAt time.Time
}
