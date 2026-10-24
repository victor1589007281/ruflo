// Package market 提供带时点 (point-in-time) 的行情、K 线、财务、公告与宏观数据。
//
// 设计文档: /home/victor/base/gateway/opencode/search-agent-design.md §3.7 / §7.4 / §13.4.1
//
// 与 pkg/search 的分工: 搜索返回文档, 数据面返回带 as_of 与来源的数值。
// 结构化字段永远不来自搜索摘要; 缺失字段返回缺口, 不推断。
package market

import (
	"context"
	"time"
)

// Market 是市场标识。
type Market string

const (
	MarketCN     Market = "CN"     // A 股 (SH/SZ/BJ)
	MarketHK     Market = "HK"     // 港股
	MarketUS     Market = "US"     // 美股
	MarketCrypto Market = "CRYPTO" // 加密资产
	MarketMacro  Market = "MACRO"  // 宏观序列
)

// Capability 是数据面能力。
type Capability string

const (
	CapQuote       Capability = "quote"
	CapHistory     Capability = "history"
	CapFundamental Capability = "fundamental"
	CapFiling      Capability = "filing"
	CapNews        Capability = "news"
	CapMacro       Capability = "macro"
)

// CapSet 是能力集合。
type CapSet map[Capability]bool

// Has 判断能力是否声明。
func (c CapSet) Has(x Capability) bool { return c != nil && c[x] }

// Session 是交易时段状态。
type Session string

const (
	SessionPre     Session = "pre"
	SessionRegular Session = "regular"
	SessionPost    Session = "post"
	SessionClosed  Session = "closed"
	SessionHalted  Session = "halted"
)

// Adjust 是复权口径。
const (
	AdjustNone = "none"
	AdjustQFQ  = "qfq" // 前复权
	AdjustHFQ  = "hfq" // 后复权
)

// Instrument 是消歧后的标的标识。
type Instrument struct {
	Canonical string // SH:600519 | SZ:000001 | HK:00700 | US:AAPL | CRYPTO:BTC
	Market    Market
	Code      string
	Name      string
	Currency  string
	Timezone  string
}

// Quote 是最新报价。Last 与 Tradable 必须分开: 停牌/涨跌停时 Last 不是可成交价。
type Quote struct {
	Inst      Instrument
	Last      float64
	PrevClose float64
	Open      float64
	High      float64
	Low       float64
	Volume    int64
	Turnover  float64
	ChangePct float64 // 由 Last/PrevClose 重算, 不采信第三方值
	Session   Session
	Tradable  bool
	AsOf      time.Time
	Delay     string // realtime | 15min | eod
	Source    string
	SourceURL string
}

// Bar 是一根 K 线。
type Bar struct {
	Date   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume int64
	Amount float64
	Adjust string
}

// MarketFact 是一条带时点的事实。
type MarketFact struct {
	Symbol    string
	Field     string
	Value     float64
	Text      string // 非数值字段 (如会计口径说明)
	Unit      string
	Currency  string
	AsOf      time.Time
	Period    string
	Delay     string
	Adjust    string
	Source    string
	SourceURL string
	FetchedAt time.Time
	Verified  bool
}

// Filing 是公告/监管披露。
type Filing struct {
	ID        string
	Symbol    string
	Title     string
	Type      string // annual | quarterly | 8-K | 10-K | announcement
	URL       string
	Published time.Time
	Source    string
}

// NewsItem 是个股新闻。
type NewsItem struct {
	Symbol    string
	Title     string
	URL       string
	Publisher string
	Published time.Time
	Summary   string
}

// HistoryParams 是 K 线查询参数。
type HistoryParams struct {
	From     time.Time
	To       time.Time
	Interval string // 1d | 1w | 1m
	Adjust   string
}

// SnapshotOptions 是快照参数。
type SnapshotOptions struct {
	AsOf     time.Time // 零值 = 现在
	Adjust   string    // 空 = qfq
	Fields   []Capability
	Deadline time.Duration
}

// Snapshot 是一组共享给多个 agent 的时点事实。
type Snapshot struct {
	Inst     Instrument
	AsOf     time.Time
	Quote    *Quote
	Bars     []Bar
	Facts    []MarketFact
	Filings  []Filing
	News     []NewsItem
	Missing  []string
	Partial  bool
	Stale    bool
	Sources  []string
	Warnings []string
}

// Resolver 把用户输入解析成规范标的。
type Resolver interface {
	Resolve(ctx context.Context, input string) (Instrument, error)
	Candidates(ctx context.Context, input string) []Instrument
}

// Provider 是数据面单源适配器。能力未声明时调用对应方法应返回 ErrUnsupported。
type Provider interface {
	Name() string
	Markets() []Market
	Capabilities() CapSet
	Healthy() bool
}

// EgressPinner 由 provider 可选实现, 返回它必须走的出口: direct | proxy | auto。
// 组装层据此从 search.Egress 取对应 *http.Client, 数据面本身不依赖 search 包。
type EgressPinner interface {
	EgressMode() string
}

// QuoteProvider 是支持实时/延迟报价的源。
type QuoteProvider interface {
	Quote(ctx context.Context, inst Instrument) (*Quote, error)
}

// HistoryProvider 是支持 K 线的源。
type HistoryProvider interface {
	History(ctx context.Context, inst Instrument, p HistoryParams) ([]Bar, error)
}

// FundamentalsProvider 是支持财务与估值的源。
type FundamentalsProvider interface {
	Fundamentals(ctx context.Context, inst Instrument, period string) ([]MarketFact, error)
}

// FilingsProvider 是支持公告/披露的源。
type FilingsProvider interface {
	Filings(ctx context.Context, inst Instrument, since time.Time) ([]Filing, error)
}

// NewsProvider 是支持个股新闻的源。
type NewsProvider interface {
	News(ctx context.Context, inst Instrument, days int) ([]NewsItem, error)
}

// MacroProvider 是支持宏观序列的源。
type MacroProvider interface {
	Macro(ctx context.Context, series string, from, to time.Time) ([]MarketFact, error)
}
