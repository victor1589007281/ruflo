package global

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// yahooProvider 适配 Yahoo Finance chart 接口。
//
// 集群实测 (2026-10-24, k8s agents pod): 直连与代理都返回 429 —— 共享出口 IP 被限流。
// 因此本 provider 对 429 一律返回 ErrRateLimited 且不做任何重试; registry 应快速切到
// Stooq(EOD) 兜底。美股报价的现实主源顺序是 Stooq(EOD) → Yahoo(盘中价, 429 恢复后)。
// Yahoo 只能作为兜底/盘中补充, 不得成为唯一来源 (设计文档 §3.7.4 第 3 条)。
type yahooProvider struct{ *gBase }

// yahooMeta 是 chart.result[0].meta 中本层会用到的字段。
type yahooMeta struct {
	Symbol                string   `json:"symbol"`
	Currency              string   `json:"currency"`
	ExchangeTimezoneName  string   `json:"exchangeTimezoneName"`
	ExchangeName          string   `json:"exchangeName"`
	ShortName             string   `json:"shortName"`
	LongName              string   `json:"longName"`
	RegularMarketPrice    *float64 `json:"regularMarketPrice"`
	PreviousClose         *float64 `json:"previousClose"`
	ChartPreviousClose    *float64 `json:"chartPreviousClose"`
	RegularMarketTime     *int64   `json:"regularMarketTime"`
	RegularMarketVolume   *float64 `json:"regularMarketVolume"`
	RegularMarketOpen     *float64 `json:"regularMarketOpen"`
	RegularMarketDayHigh  *float64 `json:"regularMarketDayHigh"`
	RegularMarketDayLow   *float64 `json:"regularMarketDayLow"`
	ExchangeDataDelayedBy *int     `json:"exchangeDataDelayedBy"`
}

// yahooQuoteSeries 是 indicators.quote[0] 的 OHLCV 数组; null 表示停牌/无成交。
type yahooQuoteSeries struct {
	Open   []*float64 `json:"open"`
	High   []*float64 `json:"high"`
	Low    []*float64 `json:"low"`
	Close  []*float64 `json:"close"`
	Volume []*float64 `json:"volume"`
}

// yahooAdjClose 是 indicators.adjclose[0]。
type yahooAdjClose struct {
	AdjClose []*float64 `json:"adjclose"`
}

// yahooChartResult 是 chart.result 的单项。
type yahooChartResult struct {
	Meta       yahooMeta `json:"meta"`
	Timestamp  []int64   `json:"timestamp"`
	Indicators struct {
		Quote    []yahooQuoteSeries `json:"quote"`
		AdjClose []yahooAdjClose    `json:"adjclose"`
	} `json:"indicators"`
}

// yahooChartResponse 是 /v8/finance/chart 的完整响应。
type yahooChartResponse struct {
	Chart struct {
		Result []yahooChartResult `json:"result"`
		Error  *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// HistoryWarnings 是可选扩展接口: 返回 K 线同时上报口径警告。
//
// market.HistoryProvider 只能返回 []Bar, 无法携带 "复权为近似值" 这类信息。
// 上层可断言本接口, 把 warnings 合并进 market.Snapshot.Warnings。
type HistoryWarnings interface {
	HistoryWithWarnings(ctx context.Context, inst market.Instrument, p market.HistoryParams) ([]market.Bar, []string, error)
}

// newYahoo 构造 Yahoo 适配器。Yahoo 对非浏览器 UA 直接 403, 因此固定使用 gBrowserUA。
func newYahoo(pc market.ProviderConfig, cfg market.Config, client *http.Client) *yahooProvider {
	b := newBase("yahoo", pc, cfg, client, gBrowserUA,
		[]market.Market{market.MarketUS, market.MarketHK},
		gCaps(market.CapQuote, market.CapHistory, market.CapNews), "https://query1.finance.yahoo.com")
	return &yahooProvider{gBase: b}
}

// EgressMode 返回 Yahoo 的固定出口; 集群实测直连与代理均可能 429, 由 registry 降级。
func (p *yahooProvider) EgressMode() string { return "proxy" }

// yahooNewsResponse 是 /v1/finance/search 的资讯部分。
type yahooNewsResponse struct {
	News []struct {
		Title               string `json:"title"`
		Publisher           string `json:"publisher"`
		Link                string `json:"link"`
		ProviderPublishTime int64  `json:"providerPublishTime"`
		Type                string `json:"type"`
	} `json:"news"`
}

// News 取个股资讯。Yahoo 的 search 接口按标的代码返回相关新闻, 美股实测可用;
// 港股/中概可能为空, 空结果按 ErrNotAvailable 上报, 由 registry 汇总。
func (p *yahooProvider) News(ctx context.Context, inst market.Instrument, days int) ([]market.NewsItem, error) {
	sym, err := yahooSymbol(inst)
	if err != nil {
		return nil, err
	}
	if days <= 0 {
		days = 7
	}
	count := days * 3
	if count < 5 {
		count = 5
	}
	if count > 20 {
		count = 20
	}
	q := url.Values{}
	q.Set("q", sym)
	q.Set("newsCount", strconv.Itoa(count))
	q.Set("quotesCount", "0")
	q.Set("enableFuzzyQuery", "false")
	rawURL := p.base + "/v1/finance/search?" + q.Encode()

	var resp yahooNewsResponse
	if err := p.getJSON(ctx, rawURL, &resp); err != nil {
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	out := make([]market.NewsItem, 0, len(resp.News))
	for _, n := range resp.News {
		if n.Link == "" || n.Title == "" {
			continue
		}
		pub := time.Unix(n.ProviderPublishTime, 0).UTC()
		if n.ProviderPublishTime == 0 || pub.Before(cutoff) {
			continue
		}
		out = append(out, market.NewsItem{
			Symbol:    inst.Canonical,
			Title:     n.Title,
			URL:       n.Link,
			Publisher: n.Publisher,
			Published: pub,
		})
	}
	if len(out) == 0 {
		return nil, market.Wrap(p.name, market.ErrNotAvailable,
			fmt.Errorf("最近 %d 天没有资讯: %s", days, inst.Canonical))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Published.After(out[j].Published) })
	return out, nil
}

// Quote 返回最新报价。数据来自 chart meta, AsOf 用 regularMarketTime,
// Yahoo 未明确声明实时, 因此默认标 15min 延迟。
func (p *yahooProvider) Quote(ctx context.Context, inst market.Instrument) (*market.Quote, error) {
	sym, err := yahooSymbol(inst)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("range", "1d")
	q.Set("interval", "1d")
	q.Set("events", "div,split")
	rawURL := p.chartURL(sym, q)

	var resp yahooChartResponse
	if err := p.getJSON(ctx, rawURL, &resp); err != nil {
		return nil, err
	}
	res, err := yahooFirstResult(&resp)
	if err != nil {
		return nil, err
	}

	meta := res.Meta
	last := yahooValue(meta.RegularMarketPrice)
	prev := yahooValue(meta.PreviousClose)
	if prev == 0 {
		prev = yahooValue(meta.ChartPreviousClose)
	}
	lastBar := yahooLastBar(res)
	if last == 0 {
		last = lastBar.close
	}
	if last == 0 {
		return nil, market.Wrap(p.Name(), market.ErrNotAvailable, fmt.Errorf("yahoo: empty price for %s", sym))
	}

	open := yahooValue(meta.RegularMarketOpen)
	high := yahooValue(meta.RegularMarketDayHigh)
	low := yahooValue(meta.RegularMarketDayLow)
	vol := yahooValue(meta.RegularMarketVolume)
	if open == 0 {
		open = lastBar.open
	}
	if high == 0 {
		high = lastBar.high
	}
	if low == 0 {
		low = lastBar.low
	}
	if vol == 0 {
		vol = lastBar.volume
	}

	out := &market.Quote{
		Inst:      inst,
		Last:      last,
		PrevClose: prev,
		Open:      open,
		High:      high,
		Low:       low,
		Volume:    gRoundInt(vol),
		Session:   market.SessionRegular,
		Tradable:  true,
		AsOf:      yahooAsOf(meta, res),
		Delay:     yahooDelay(meta),
		Source:    p.Name(),
		SourceURL: rawURL,
	}
	if out.Inst.Currency == "" {
		out.Inst.Currency = meta.Currency
	}
	if out.Inst.Timezone == "" {
		out.Inst.Timezone = meta.ExchangeTimezoneName
	}
	if out.Inst.Name == "" {
		out.Inst.Name = yahooFirstNonEmpty(meta.LongName, meta.ShortName)
	}
	if out.PrevClose > 0 {
		// 涨跌幅一律由价差重算, 不采信上游字段 (设计文档 §7.4)。
		out.ChangePct = (out.Last - out.PrevClose) / out.PrevClose * 100
	}
	return out, nil
}

// History 实现 market.HistoryProvider; 复权警告可通过 HistoryWithWarnings 获取。
func (p *yahooProvider) History(ctx context.Context, inst market.Instrument, hp market.HistoryParams) ([]market.Bar, error) {
	bars, _, err := p.HistoryWithWarnings(ctx, inst, hp)
	return bars, err
}

// HistoryWithWarnings 返回日/周/月 K 线及复权口径警告。
//
// Yahoo 的 adjclose 是分红/拆股调整后的价格序列, 并非交易所官方复权因子:
//   - adjust=none: 返回原始 close, Adjust=none, 无警告;
//   - adjust=qfq/hfq: 用 adjclose/close 的比例缩放 OHLC, 并写警告由上层处理;
//     其中 hfq 只能以本次查询窗口首个交易日为基准, 跨窗口不可直接比较。
//
// 绝不把近似复权伪装成精确复权, 也不编造缺失的 adjclose。
func (p *yahooProvider) HistoryWithWarnings(ctx context.Context, inst market.Instrument, hp market.HistoryParams) ([]market.Bar, []string, error) {
	sym, err := yahooSymbol(inst)
	if err != nil {
		return nil, nil, err
	}
	interval, err := yahooInterval(hp.Interval)
	if err != nil {
		return nil, nil, market.Wrap(p.Name(), market.ErrUnsupported, err)
	}
	adjust, err := yahooAdjust(hp.Adjust, p.cfg.DefaultAdjust)
	if err != nil {
		return nil, nil, market.Wrap(p.Name(), market.ErrUnsupported, err)
	}

	q := url.Values{}
	q.Set("interval", interval)
	q.Set("events", "div,split")
	if !hp.From.IsZero() {
		q.Set("period1", strconv.FormatInt(hp.From.Unix(), 10))
		to := hp.To
		if to.IsZero() {
			to = time.Now()
		}
		q.Set("period2", strconv.FormatInt(to.Unix(), 10))
	} else {
		// 未给起点时用 Yahoo 的 range 语义; 1y 是报价/K 线最常用的窗口。
		q.Set("range", "1y")
	}
	rawURL := p.chartURL(sym, q)

	var resp yahooChartResponse
	if err := p.getJSON(ctx, rawURL, &resp); err != nil {
		return nil, nil, err
	}
	res, err := yahooFirstResult(&resp)
	if err != nil {
		return nil, nil, err
	}

	bars, warnings := yahooBuildBars(res, adjust, hp)
	return bars, warnings, nil
}

// chartURL 拼接 chart 接口 URL。
func (p *yahooProvider) chartURL(symbol string, q url.Values) string {
	return p.base + "/v8/finance/chart/" + url.PathEscape(symbol) + "?" + q.Encode()
}

// yahooSymbol 把 Instrument 转成 Yahoo 代码: 美股原样, 港股去掉前导零并补足 4 位。
func yahooSymbol(inst market.Instrument) (string, error) {
	code := gCode(inst)
	if code == "" {
		return "", market.Wrap("yahoo", market.ErrUnresolved, fmt.Errorf("empty symbol"))
	}
	switch inst.Market {
	case market.MarketUS:
		return strings.ToUpper(code), nil
	case market.MarketHK:
		digits := strings.TrimLeft(code, "0")
		if digits == "" {
			digits = "0"
		}
		for len(digits) < 4 {
			digits = "0" + digits
		}
		return digits + ".HK", nil
	default:
		return "", market.Wrap("yahoo", market.ErrUnresolved,
			fmt.Errorf("yahoo does not serve market %s", inst.Market))
	}
}

// yahooInterval 把通用周期映射到 Yahoo 的 interval 参数。
func yahooInterval(interval string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "", "1d", "d", "day":
		return "1d", nil
	case "1w", "w", "week":
		return "1wk", nil
	case "1m", "m", "month":
		return "1mo", nil
	default:
		return "", fmt.Errorf("unsupported interval %q", interval)
	}
}

// yahooAdjust 归一复权口径; 空值按配置默认 (默认 qfq)。
func yahooAdjust(adjust, def string) (string, error) {
	a := strings.ToLower(strings.TrimSpace(adjust))
	if a == "" {
		a = strings.ToLower(strings.TrimSpace(def))
	}
	if a == "" {
		a = market.AdjustQFQ
	}
	switch a {
	case market.AdjustNone, market.AdjustQFQ, market.AdjustHFQ:
		return a, nil
	default:
		return "", fmt.Errorf("unsupported adjust %q", adjust)
	}
}

// yahooFirstResult 取 chart.result[0], 并把上游 error 映射成可分类错误。
func yahooFirstResult(resp *yahooChartResponse) (*yahooChartResult, error) {
	if resp.Chart.Error != nil {
		desc := strings.TrimSpace(resp.Chart.Error.Description)
		if desc == "" {
			desc = resp.Chart.Error.Code
		}
		return nil, market.Wrap("yahoo", market.ErrUnresolved, fmt.Errorf("yahoo: %s", desc))
	}
	if len(resp.Chart.Result) == 0 {
		return nil, market.Wrap("yahoo", market.ErrNotAvailable, fmt.Errorf("yahoo: empty result"))
	}
	return &resp.Chart.Result[0], nil
}

// yahooBarPoint 是同一时间戳上的一组 OHLCV; idx 保留原始数组下标, 用于对齐 adjclose。
type yahooBarPoint struct {
	idx                            int
	date                           time.Time
	open, high, low, close, volume float64
}

// yahooBuildBars 把 timestamp/indicators 组装成 Bar, 并按需要做近似复权。
func yahooBuildBars(res *yahooChartResult, adjust string, hp market.HistoryParams) ([]market.Bar, []string) {
	points := yahooPoints(res)
	if len(points) == 0 {
		return nil, nil
	}

	factors := make([]float64, len(points))
	valid := make([]bool, len(points))
	for i := range factors {
		factors[i] = 1
	}
	// Yahoo 只有一个 adjclose 序列; 逐点计算 factor = adjclose / rawClose。
	validAdj := 0
	firstFactor := 1.0
	if len(res.Indicators.AdjClose) > 0 && len(res.Indicators.AdjClose[0].AdjClose) > 0 {
		adj := res.Indicators.AdjClose[0].AdjClose
		for i, pt := range points {
			// points 可能跳过 null 行, 必须用原始下标对齐 adjclose。
			ai := pt.idx
			if ai < 0 || ai >= len(adj) || adj[ai] == nil || pt.close == 0 {
				continue
			}
			f := *adj[ai] / pt.close
			if f <= 0 {
				continue
			}
			factors[i] = f
			valid[i] = true
			validAdj++
			if validAdj == 1 {
				firstFactor = f
			}
		}
	}
	hasAdj := validAdj > 0

	warnings := make([]string, 0, 3)
	if hasAdj && validAdj < len(points) {
		warnings = append(warnings, "yahoo: 部分 adjclose 缺失, 缺失行按不复权处理")
	}
	scales := make([]float64, len(points))
	for i := range scales {
		scales[i] = 1
	}
	switch adjust {
	case market.AdjustNone:
		// 原始 OHLC, 无需缩放。
	case market.AdjustQFQ:
		if !hasAdj {
			warnings = append(warnings, "yahoo: adjclose 缺失, 已回退不复权口径 (adjust=none)")
		} else {
			for i := range scales {
				if valid[i] {
					scales[i] = factors[i]
				}
			}
			warnings = append(warnings, "yahoo: qfq 由 adjclose/close 比例近似, 非交易所官方复权因子")
		}
	case market.AdjustHFQ:
		if !hasAdj {
			warnings = append(warnings, "yahoo: adjclose 缺失, 已回退不复权口径 (adjust=none)")
		} else {
			for i := range scales {
				if valid[i] {
					scales[i] = factors[i] / firstFactor
				}
			}
			warnings = append(warnings, "yahoo: hfq 由 adjclose 近似推导, 以查询窗口首个有效交易日为基准, 跨窗口不可直接比较")
		}
	}
	if adjust != market.AdjustNone && !hasAdj {
		adjust = market.AdjustNone
	}

	bars := make([]market.Bar, 0, len(points))
	for i, pt := range points {
		b := market.Bar{
			Date:   gDay(pt.date),
			Open:   pt.open * scales[i],
			High:   pt.high * scales[i],
			Low:    pt.low * scales[i],
			Close:  pt.close * scales[i],
			Volume: gRoundInt(pt.volume),
			Adjust: adjust,
		}
		// 同一 UTC 日内可能出现重复时间戳 (周/月线不会), 保持时间顺序即可。
		bars = append(bars, b)
	}

	// 区间过滤按 UTC 日比较, 避免 To 给的是当日零点却把盘中 bar 丢掉。
	if !hp.From.IsZero() || !hp.To.IsZero() {
		from := gDay(hp.From)
		to := gDay(hp.To)
		filtered := bars[:0]
		for _, b := range bars {
			if !hp.From.IsZero() && b.Date.Before(from) {
				continue
			}
			if !hp.To.IsZero() && b.Date.After(to) {
				continue
			}
			filtered = append(filtered, b)
		}
		bars = filtered
	}
	sort.SliceStable(bars, func(i, j int) bool { return bars[i].Date.Before(bars[j].Date) })
	return bars, warnings
}

// yahooPoints 把 quote[0] 与 timestamp 对齐, 跳过 close 为 null 的行。
// 单个 OHLC 缺失时用 close 兜底, 保证日线可用。
func yahooPoints(res *yahooChartResult) []yahooBarPoint {
	if len(res.Indicators.Quote) == 0 {
		return nil
	}
	q := res.Indicators.Quote[0]
	n := len(res.Timestamp)
	if n == 0 {
		return nil
	}
	points := make([]yahooBarPoint, 0, n)
	for i := 0; i < n; i++ {
		if res.Timestamp[i] <= 0 {
			continue
		}
		closeV := yahooAt(q.Close, i)
		if closeV == 0 {
			continue
		}
		points = append(points, yahooBarPoint{
			idx:    i,
			date:   time.Unix(res.Timestamp[i], 0).UTC(),
			open:   yahooFallback(yahooAt(q.Open, i), closeV),
			high:   yahooFallback(yahooAt(q.High, i), closeV),
			low:    yahooFallback(yahooAt(q.Low, i), closeV),
			close:  closeV,
			volume: yahooAt(q.Volume, i),
		})
	}
	return points
}

// yahooLastBar 返回最后一个有效 bar 的 OHLCV, 用于补齐 meta 缺失字段。
func yahooLastBar(res *yahooChartResult) yahooBarPoint {
	points := yahooPoints(res)
	if len(points) == 0 {
		return yahooBarPoint{}
	}
	return points[len(points)-1]
}

// yahooAsOf 取数据源自带时间; meta 缺失时退回最后一根 bar 的时间戳。
func yahooAsOf(meta yahooMeta, res *yahooChartResult) time.Time {
	if meta.RegularMarketTime != nil && *meta.RegularMarketTime > 0 {
		return time.Unix(*meta.RegularMarketTime, 0).UTC()
	}
	if n := len(res.Timestamp); n > 0 && res.Timestamp[n-1] > 0 {
		return time.Unix(res.Timestamp[n-1], 0).UTC()
	}
	return time.Time{}
}

// yahooDelay 根据上游给出的延迟信息标记 realtime/15min; 未声明时保守取 15min。
// 只有源明确写 exchangeDataDelayedBy=0 时才敢标 realtime。
func yahooDelay(meta yahooMeta) string {
	if meta.ExchangeDataDelayedBy != nil && *meta.ExchangeDataDelayedBy <= 0 {
		return "realtime"
	}
	return "15min"
}

// yahooAt 安全读取指针数组。
func yahooAt(v []*float64, i int) float64 {
	if i < 0 || i >= len(v) || v[i] == nil {
		return 0
	}
	return *v[i]
}

// yahooValue 解引用可空数值。
func yahooValue(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// yahooFallback 返回 v, 为零时用 fallback。
func yahooFallback(v, fallback float64) float64 {
	if v == 0 {
		return fallback
	}
	return v
}

// yahooFirstNonEmpty 返回第一个非空字符串。
func yahooFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
