package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// 本文件把 pkg/market 的数据面暴露成模型可调用的工具。
//
// 设计约束:
//   - 结构化数字只走这里, 不靠 WebSearch 摘要 (搜索摘要没有复权口径与时点);
//   - 每个返回值都带 source/as_of/adjust, 缺失字段显式写在 missing 里;
//   - 错误分类原样传给模型 (symbol_unresolved / unsupported / rate_limited),
//     让模型改查询而不是编一个数字。

const (
	MarketSnapshotToolName     = "market_snapshot"
	MarketQuoteToolName        = "market_quote"
	MarketHistoryToolName      = "market_history"
	MarketFundamentalsToolName = "market_fundamentals"
	MarketFilingsToolName      = "market_filings"
	MarketNewsToolName         = "market_news"
	MarketMacroToolName        = "market_macro"
)

// RegisterMarketTools 注册全部行情工具; reg 为 nil 时原样返回 (供未装配数据面的进程跳过)。
//
// RegisterMarketTools registers the point-in-time market data tools.
func RegisterMarketTools(reg *tool.Registry, data *DataStack) {
	if reg == nil || data == nil || data.Market == nil {
		return
	}
	base := marketToolBase{registry: data.Market}
	reg.Register(&MarketSnapshotTool{base})
	reg.Register(&MarketQuoteTool{base})
	reg.Register(&MarketHistoryTool{base})
	reg.Register(&MarketFundamentalsTool{base})
	reg.Register(&MarketFilingsTool{base})
	reg.Register(&MarketNewsTool{base})
	reg.Register(&MarketMacroTool{base})
}

// registerMarketDataTools 用进程级数据面注册行情工具; 数据面未装配时静默跳过。
func registerMarketDataTools(reg *tool.Registry) {
	RegisterMarketTools(reg, DefaultDataStack())
}

// marketToolBase 收敛只读、并发安全、无权限确认这些共同行为。
type marketToolBase struct{ registry *market.Registry }

// IsReadOnly 恒为 true: 只读远端公开数据。
func (b *marketToolBase) IsReadOnly(_ json.RawMessage) bool { return true }

// IsConcurrencySafe 恒为 true: 工具无共享可变状态, 缓存由 registry 内部加锁。
func (b *marketToolBase) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 不附加额外权限要求。
func (b *marketToolBase) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// marketErrorResult 把数据面错误转成模型可读的结果, 保留错误类别。
func marketErrorResult(err error) *tool.ToolResult {
	kind := market.KindOf(err)
	return &tool.ToolResult{
		Content: fmt.Sprintf("market data error [%s]: %v", kind, err),
		IsError: true,
	}
}

// jsonResult 序列化成功结果。
func jsonResult(v any) *tool.ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("encoding error: %v", err), IsError: true}
	}
	return &tool.ToolResult{Content: string(b)}
}

// ---------------- snapshot ----------------

// MarketSnapshotTool 返回一次时点快照。
type MarketSnapshotTool struct{ marketToolBase }

// Name 返回工具名。
func (t *MarketSnapshotTool) Name() string { return MarketSnapshotToolName }

// Description 返回工具说明。
func (t *MarketSnapshotTool) Description() string {
	return `Return a point-in-time snapshot for a listed instrument: quote, K-lines, fundamentals, filings and news, all filtered to the requested as_of. Fields that cannot be obtained are listed in "missing" instead of being estimated. Use this before any multi-step equity analysis so every downstream step shares one as_of and one adjustment convention.`
}

// InputSchema 返回入参 schema。
func (t *MarketSnapshotTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"symbol": {"type": "string", "description": "Canonical or human symbol, e.g. SH:600519, SZ:000001, HK:00700, US:AAPL, CRYPTO:BTC, 贵州茅台."},
			"as_of": {"type": "string", "description": "RFC3339 timestamp or YYYY-MM-DD. Defaults to now. Historical values are filtered to this instant."},
			"adjust": {"type": "string", "enum": ["none", "qfq", "hfq"], "description": "Price adjustment for K-lines. Defaults to qfq."},
			"fields": {
				"type": "array",
				"items": {"type": "string", "enum": ["quote", "history", "fundamental", "filing", "news"]},
				"description": "Capabilities to fetch. Defaults to quote, history, fundamental, filing."
			}
		},
		"required": ["symbol"]
	}`)
}

type snapshotInput struct {
	Symbol string   `json:"symbol"`
	AsOf   string   `json:"as_of,omitempty"`
	Adjust string   `json:"adjust,omitempty"`
	Fields []string `json:"fields,omitempty"`
}

// Call 生成快照。
func (t *MarketSnapshotTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in snapshotInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	asOf, err := parseTime(in.AsOf)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	fields := make([]market.Capability, 0, len(in.Fields))
	for _, f := range in.Fields {
		fields = append(fields, market.Capability(strings.ToLower(strings.TrimSpace(f))))
	}
	snap, err := t.registry.Snapshot(ctx, in.Symbol, market.SnapshotOptions{
		AsOf: asOf, Adjust: strings.ToLower(in.Adjust), Fields: fields,
	})
	if err != nil {
		return marketErrorResult(err), nil
	}
	return jsonResult(toSnapshotDTO(snap)), nil
}

// ---------------- quote ----------------

// MarketQuoteTool 返回最新报价。
type MarketQuoteTool struct{ marketToolBase }

// Name 返回工具名。
func (t *MarketQuoteTool) Name() string { return MarketQuoteToolName }

// Description 返回工具说明。
func (t *MarketQuoteTool) Description() string {
	return `Return the latest quote for one instrument with explicit session, delay, source and as_of. Multi-source quotes are cross-checked; a divergence above the configured threshold is reported in warnings rather than averaged. For historical prices use market_history or market_snapshot.`
}

// InputSchema 返回入参 schema。
func (t *MarketQuoteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"symbol": {"type": "string", "description": "e.g. SH:600519, US:AAPL, CRYPTO:BTC."}
		},
		"required": ["symbol"]
	}`)
}

// Call 查询报价。
func (t *MarketQuoteTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Symbol string `json:"symbol"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	q, err := t.registry.Quote(ctx, in.Symbol, time.Time{})
	if err != nil {
		return marketErrorResult(err), nil
	}
	return jsonResult(toQuoteDTO(q)), nil
}

// ---------------- history ----------------

// MarketHistoryTool 返回 K 线。
type MarketHistoryTool struct{ marketToolBase }

// Name 返回工具名。
func (t *MarketHistoryTool) Name() string { return MarketHistoryToolName }

// Description 返回工具说明。
func (t *MarketHistoryTool) Description() string {
	return `Return OHLCV bars for an instrument with an explicit interval and adjustment (none/qfq/hfq). Adjusted and unadjusted series are not comparable: always carry the returned adjust value into comparisons and reports.`
}

// InputSchema 返回入参 schema。
func (t *MarketHistoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"symbol": {"type": "string"},
			"from": {"type": "string", "description": "RFC3339 or YYYY-MM-DD. Defaults to 180 days ago."},
			"to": {"type": "string", "description": "RFC3339 or YYYY-MM-DD. Defaults to now."},
			"interval": {"type": "string", "enum": ["1d", "1w", "1m"], "description": "Defaults to 1d."},
			"adjust": {"type": "string", "enum": ["none", "qfq", "hfq"], "description": "Defaults to qfq."},
			"limit": {"type": "integer", "description": "Return at most the last N bars. Defaults to 120, max 1000."}
		},
		"required": ["symbol"]
	}`)
}

type historyInput struct {
	Symbol   string `json:"symbol"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Interval string `json:"interval,omitempty"`
	Adjust   string `json:"adjust,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// Call 查询 K 线。
func (t *MarketHistoryTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in historyInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	now := time.Now()
	from, err := parseTimeOr(in.From, now.AddDate(0, -6, 0))
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	to, err := parseTimeOr(in.To, now)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	if from.After(to) {
		return &tool.ToolResult{Content: "invalid range: from is after to", IsError: true}, nil
	}
	interval := strings.ToLower(strings.TrimSpace(in.Interval))
	if interval == "" {
		interval = "1d"
	}
	switch interval {
	case "1d", "1w", "1m":
	default:
		return &tool.ToolResult{Content: "invalid interval: use 1d, 1w or 1m", IsError: true}, nil
	}
	bars, err := t.registry.History(ctx, in.Symbol, market.HistoryParams{
		From: from, To: to, Interval: interval, Adjust: strings.ToLower(in.Adjust),
	})
	if err != nil {
		return marketErrorResult(err), nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 120
	}
	if limit > 1000 {
		limit = 1000
	}
	truncated := false
	if len(bars) > limit {
		bars = bars[len(bars)-limit:]
		truncated = true
	}
	return jsonResult(map[string]any{
		"symbol":    in.Symbol,
		"interval":  interval,
		"adjust":    firstNonEmpty(strings.ToLower(in.Adjust), market.AdjustQFQ),
		"bars":      toBarDTOs(bars),
		"truncated": truncated,
	}), nil
}

// ---------------- fundamentals ----------------

// MarketFundamentalsTool 返回财务/估值事实。
type MarketFundamentalsTool struct{ marketToolBase }

// Name 返回工具名。
func (t *MarketFundamentalsTool) Name() string { return MarketFundamentalsToolName }

// Description 返回工具说明。
func (t *MarketFundamentalsTool) Description() string {
	return `Return fundamental facts (valuation, profitability, market cap) as explicitly sourced fields with as_of, period and unit. Missing fields are absent, never filled from peers or memory.`
}

// InputSchema 返回入参 schema。
func (t *MarketFundamentalsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"symbol": {"type": "string"},
			"period": {"type": "string", "description": "Reporting period, e.g. 2025Q3 or 2024. Empty means latest available."}
		},
		"required": ["symbol"]
	}`)
}

// Call 查询基本面。
func (t *MarketFundamentalsTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Symbol string `json:"symbol"`
		Period string `json:"period,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	facts, err := t.registry.Fundamentals(ctx, in.Symbol, in.Period)
	if err != nil {
		return marketErrorResult(err), nil
	}
	return jsonResult(map[string]any{"symbol": in.Symbol, "facts": toFactDTOs(facts)}), nil
}

// ---------------- filings ----------------

// MarketFilingsTool 返回公告/监管披露。
type MarketFilingsTool struct{ marketToolBase }

// Name 返回工具名。
func (t *MarketFilingsTool) Name() string { return MarketFilingsToolName }

// Description 返回工具说明。
func (t *MarketFilingsTool) Description() string {
	return `Return regulatory filings and announcements for an instrument, newest first, with publication time and source URL. A-share announcement sources are not enabled yet; the tool returns unsupported rather than inventing filings.`
}

// InputSchema 返回入参 schema。
func (t *MarketFilingsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"symbol": {"type": "string"},
			"since": {"type": "string", "description": "RFC3339 or YYYY-MM-DD. Defaults to 90 days ago."}
		},
		"required": ["symbol"]
	}`)
}

// Call 查询公告。
func (t *MarketFilingsTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Symbol string `json:"symbol"`
		Since  string `json:"since,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	since, err := parseTimeOr(in.Since, time.Now().AddDate(0, 0, -90))
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	filings, err := t.registry.Filings(ctx, in.Symbol, since)
	if err != nil {
		return marketErrorResult(err), nil
	}
	return jsonResult(map[string]any{"symbol": in.Symbol, "filings": toFilingDTOs(filings)}), nil
}

// ---------------- news ----------------

// MarketNewsTool 返回个股新闻。
type MarketNewsTool struct{ marketToolBase }

// Name 返回工具名。
func (t *MarketNewsTool) Name() string { return MarketNewsToolName }

// Description 返回工具说明。
func (t *MarketNewsTool) Description() string {
	return `Return recent news items for an instrument with publisher, publication time and URL. Use market_snapshot instead when a historical as_of must be enforced.`
}

// InputSchema 返回入参 schema。
func (t *MarketNewsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"symbol": {"type": "string"},
			"days": {"type": "integer", "description": "Look-back window in days. Defaults to 7, max 90."}
		},
		"required": ["symbol"]
	}`)
}

// Call 查询新闻。
func (t *MarketNewsTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Symbol string `json:"symbol"`
		Days   int    `json:"days,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	if in.Days > 90 {
		in.Days = 90
	}
	news, err := t.registry.News(ctx, in.Symbol, in.Days)
	if err != nil {
		return marketErrorResult(err), nil
	}
	return jsonResult(map[string]any{"symbol": in.Symbol, "news": toNewsDTOs(news)}), nil
}

// ---------------- macro ----------------

// MarketMacroTool 返回宏观序列。
type MarketMacroTool struct{ marketToolBase }

// Name 返回工具名。
func (t *MarketMacroTool) Name() string { return MarketMacroToolName }

// Description 返回工具说明。
func (t *MarketMacroTool) Description() string {
	return `Return observations for a macroeconomic series (FRED or World Bank). Series names are source-native, e.g. CPIAUCSL (FRED) or NY.GDP.MKTP.CD (World Bank). FRED requires FRED_API_KEY; World Bank is keyless.`
}

// InputSchema 返回入参 schema。
func (t *MarketMacroTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"series": {"type": "string", "description": "Source-native series id, e.g. CPIAUCSL or NY.GDP.MKTP.CD."},
			"from": {"type": "string", "description": "RFC3339 or YYYY-MM-DD. Defaults to 2 years ago."},
			"to": {"type": "string", "description": "RFC3339 or YYYY-MM-DD. Defaults to now."}
		},
		"required": ["series"]
	}`)
}

// Call 查询宏观序列。
func (t *MarketMacroTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Series string `json:"series"`
		From   string `json:"from,omitempty"`
		To     string `json:"to,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	now := time.Now()
	from, err := parseTimeOr(in.From, now.AddDate(-2, 0, 0))
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	to, err := parseTimeOr(in.To, now)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	facts, err := t.registry.Macro(ctx, in.Series, from, to)
	if err != nil {
		return marketErrorResult(err), nil
	}
	return jsonResult(map[string]any{"series": in.Series, "facts": toFactDTOs(facts)}), nil
}

// ---------------- DTO ----------------

type quoteDTO struct {
	Symbol    string  `json:"symbol"`
	Last      float64 `json:"last"`
	PrevClose float64 `json:"prev_close,omitempty"`
	Open      float64 `json:"open,omitempty"`
	High      float64 `json:"high,omitempty"`
	Low       float64 `json:"low,omitempty"`
	Volume    int64   `json:"volume,omitempty"`
	Turnover  float64 `json:"turnover,omitempty"`
	ChangePct float64 `json:"change_pct"`
	Session   string  `json:"session"`
	Tradable  bool    `json:"tradable"`
	AsOf      string  `json:"as_of"`
	Delay     string  `json:"delay,omitempty"`
	Source    string  `json:"source,omitempty"`
	SourceURL string  `json:"source_url,omitempty"`
}

func toQuoteDTO(q *market.Quote) quoteDTO {
	if q == nil {
		return quoteDTO{}
	}
	return quoteDTO{
		Symbol: q.Inst.Canonical, Last: q.Last, PrevClose: q.PrevClose, Open: q.Open,
		High: q.High, Low: q.Low, Volume: q.Volume, Turnover: q.Turnover,
		ChangePct: q.ChangePct, Session: string(q.Session), Tradable: q.Tradable,
		AsOf: fmtTime(q.AsOf), Delay: q.Delay, Source: q.Source, SourceURL: q.SourceURL,
	}
}

type barDTO struct {
	Date   string  `json:"date"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume,omitempty"`
	Amount float64 `json:"amount,omitempty"`
	Adjust string  `json:"adjust,omitempty"`
}

func toBarDTOs(bars []market.Bar) []barDTO {
	out := make([]barDTO, 0, len(bars))
	for _, b := range bars {
		out = append(out, barDTO{Date: fmtTime(b.Date), Open: b.Open, High: b.High,
			Low: b.Low, Close: b.Close, Volume: b.Volume, Amount: b.Amount})
	}
	return out
}

type factDTO struct {
	Symbol    string  `json:"symbol,omitempty"`
	Field     string  `json:"field"`
	Value     float64 `json:"value"`
	Text      string  `json:"text,omitempty"`
	Unit      string  `json:"unit,omitempty"`
	Currency  string  `json:"currency,omitempty"`
	AsOf      string  `json:"as_of,omitempty"`
	Period    string  `json:"period,omitempty"`
	Delay     string  `json:"delay,omitempty"`
	Adjust    string  `json:"adjust,omitempty"`
	Source    string  `json:"source,omitempty"`
	SourceURL string  `json:"source_url,omitempty"`
	Verified  bool    `json:"verified"`
}

func toFactDTOs(facts []market.MarketFact) []factDTO {
	out := make([]factDTO, 0, len(facts))
	for _, f := range facts {
		out = append(out, factDTO{Symbol: f.Symbol, Field: f.Field, Value: f.Value,
			Text: f.Text, Unit: f.Unit, Currency: f.Currency, AsOf: fmtTime(f.AsOf),
			Period: f.Period, Delay: f.Delay, Adjust: f.Adjust, Source: f.Source,
			SourceURL: f.SourceURL, Verified: f.Verified})
	}
	return out
}

type filingDTO struct {
	ID        string `json:"id,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Title     string `json:"title"`
	Type      string `json:"type,omitempty"`
	URL       string `json:"url,omitempty"`
	Published string `json:"published,omitempty"`
	Source    string `json:"source,omitempty"`
}

func toFilingDTOs(filings []market.Filing) []filingDTO {
	out := make([]filingDTO, 0, len(filings))
	for _, f := range filings {
		out = append(out, filingDTO{ID: f.ID, Symbol: f.Symbol, Title: f.Title,
			Type: f.Type, URL: f.URL, Published: fmtTime(f.Published), Source: f.Source})
	}
	return out
}

type newsDTO struct {
	Symbol    string `json:"symbol,omitempty"`
	Title     string `json:"title"`
	URL       string `json:"url,omitempty"`
	Publisher string `json:"publisher,omitempty"`
	Published string `json:"published,omitempty"`
	Summary   string `json:"summary,omitempty"`
}

func toNewsDTOs(news []market.NewsItem) []newsDTO {
	out := make([]newsDTO, 0, len(news))
	for _, n := range news {
		out = append(out, newsDTO{Symbol: n.Symbol, Title: n.Title, URL: n.URL,
			Publisher: n.Publisher, Published: fmtTime(n.Published), Summary: n.Summary})
	}
	return out
}

type snapshotDTO struct {
	Symbol   string      `json:"symbol"`
	AsOf     string      `json:"as_of"`
	Quote    *quoteDTO   `json:"quote,omitempty"`
	Bars     []barDTO    `json:"bars,omitempty"`
	Facts    []factDTO   `json:"facts,omitempty"`
	Filings  []filingDTO `json:"filings,omitempty"`
	News     []newsDTO   `json:"news,omitempty"`
	Missing  []string    `json:"missing,omitempty"`
	Partial  bool        `json:"partial"`
	Stale    bool        `json:"stale"`
	Sources  []string    `json:"sources,omitempty"`
	Warnings []string    `json:"warnings,omitempty"`
}

func toSnapshotDTO(s *market.Snapshot) snapshotDTO {
	if s == nil {
		return snapshotDTO{}
	}
	out := snapshotDTO{
		Symbol: s.Inst.Canonical, AsOf: fmtTime(s.AsOf), Missing: s.Missing,
		Partial: s.Partial, Stale: s.Stale, Sources: s.Sources, Warnings: s.Warnings,
	}
	if s.Quote != nil {
		q := toQuoteDTO(s.Quote)
		out.Quote = &q
	}
	out.Bars = toBarDTOs(s.Bars)
	out.Facts = toFactDTOs(s.Facts)
	out.Filings = toFilingDTOs(s.Filings)
	out.News = toNewsDTOs(s.News)
	return out
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// parseTime 解析可选的 RFC3339 或 YYYY-MM-DD; 空值返回零值 (由调用方决定默认)。
func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	return parseTimeFlexible(s)
}

// parseTimeOr 解析时间, 空值时返回默认值。
func parseTimeOr(s string, def time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return parseTimeFlexible(s)
}

func parseTimeFlexible(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006/01/02", "20060102"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q: use RFC3339 or YYYY-MM-DD", s)
}

func firstNonEmpty(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
