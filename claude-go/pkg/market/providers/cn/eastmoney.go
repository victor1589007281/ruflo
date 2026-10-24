package cn

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// 东方财富 provider。
//
// 端点与字段 (2026-10 实测):
//   - 批量报价: push2 /api/qt/ulist.np/get, secid 前缀 SH=1 / SZ=0 / HK=116,
//     字段 f12 代码, f14 名称, f2 最新价, f3 涨跌幅, f4 涨跌额, f5 成交量(手),
//     f6 成交额(元), f15 最高, f16 最低, f17 开盘, f18 昨收, f20 总市值,
//     f9 市盈率(动), f23 市净率; 一次最多 50 个 secid。
//   - K 线: push2his /api/qt/stock/kline/get, klt 101/102/103 = 日/周/月,
//     fqt 0/1/2 = none/qfq/hfq, fields2 顺序为
//     f51 日期, f52 开, f53 收, f54 高, f55 低, f56 量(手), f57 额(元)。
//   - 基础估值: push2 /api/qt/stock/get, f57 代码, f58 名称, f162 市盈率(动),
//     f167 市净率, f116 总市值, f117 流通市值, f173 ROE。
//
// 集群内直连 push2 不可达、push2his 走代理可用, 因此 EgressMode="auto" 且
// 单源超时固定 1.5s, 避免拖垮 SnapshotDeadline。

const eastMoneyHistoryBase = "https://push2his.eastmoney.com"

// eastMoneyProvider 是东方财富源。
type eastMoneyProvider struct {
	base
	historyBase string
	searchBase  string
	// searchClient 是资讯搜索的专用出口。实测集群内直连 0.13s、走代理 2.1s,
	// 而 base.timeout=1.5s; 资讯必须直连, 否则稳定超时。
	searchClient *http.Client
}

// newEastMoney 构造东方财富 provider。
func newEastMoney(cfg market.Config, client, searchClient *http.Client) *eastMoneyProvider {
	pc := providerConf(cfg, "eastmoney")
	baseURL := pc.BaseURL
	if baseURL == "" {
		baseURL = defaultEastMoneyBase
	}
	ua := pc.UserAgent
	if ua == "" {
		ua = cfg.UserAgent
	}
	if ua == "" {
		ua = defaultUserAgent
	}
	timeout := sourceTimeout
	if pc.Timeout > 0 && pc.Timeout < timeout {
		timeout = pc.Timeout
	}
	return &eastMoneyProvider{
		base: base{
			name:    "eastmoney",
			markets: []market.Market{market.MarketCN, market.MarketHK},
			caps: market.CapSet{
				market.CapQuote:       true,
				market.CapHistory:     true,
				market.CapFundamental: true,
				market.CapNews:        true,
			},
			client:  client,
			baseURL: baseURL,
			ua:      ua,
			timeout: timeout,
		},
		historyBase:  eastMoneyHistoryBase,
		searchBase:   eastMoneySearchBase,
		searchClient: searchClient,
	}
}

// EgressMode 返回 auto: 集群内 push2 直连失败, 代理可用。
func (e *eastMoneyProvider) EgressMode() string { return "auto" }

// eastMoneySearchBase 是资讯搜索端点。它与 push2 不同域, 集群内直连可用
// (2026-10-24 实测 1s 内返回), 但仍复用 provider 的 auto 出口。
const eastMoneySearchBase = "https://search-api-web.eastmoney.com"

// News 取个股资讯。关键词用证券代码精确匹配, 按发布时间倒序返回最近 days 天。
//
// 数据源返回 JSONP (`cb({...})`), 不是 JSON; 这里剥掉回调外壳再解析。
// 正文带 <em> 高亮标签, 只做去标签与实体反转义, 不改写内容。
func (e *eastMoneyProvider) News(ctx context.Context, inst market.Instrument, days int) ([]market.NewsItem, error) {
	if inst.Code == "" {
		return nil, market.Wrap(e.name, market.ErrUnresolved, fmt.Errorf("空代码"))
	}
	if days <= 0 {
		days = 7
	}
	pageSize := days * 5
	if pageSize < 10 {
		pageSize = 10
	}
	if pageSize > 50 {
		pageSize = 50
	}
	inner := map[string]any{
		"searchScope": "default",
		"sort":        "time",
		"pageIndex":   1,
		"pageSize":    pageSize,
	}
	param := map[string]any{
		"uid": "", "keyword": inst.Code,
		"type":          []string{"cmsArticleWebOld"},
		"client":        "web",
		"clientType":    "web",
		"clientVersion": "curr",
		"param":         map[string]any{"cmsArticleWebOld": inner},
	}
	raw, err := json.Marshal(param)
	if err != nil {
		return nil, market.Wrap(e.name, market.ErrNotAvailable, err)
	}
	rawURL := e.searchBase + "/search/jsonp?cb=cb&param=" + url.QueryEscape(string(raw))

	if e.searchClient == nil {
		e.searchClient = e.client
	}
	body, err := e.getWith(ctx, e.searchClient, rawURL, map[string]string{"Referer": "https://so.eastmoney.com/"})
	if err != nil {
		return nil, err
	}
	payload := body
	if i := strings.IndexByte(string(payload), '('); i >= 0 {
		if j := strings.LastIndexByte(string(payload), ')'); j > i {
			payload = payload[i+1 : j]
		}
	}
	var resp struct {
		Result struct {
			Articles []struct {
				Date      string `json:"date"`
				Title     string `json:"title"`
				Content   string `json:"content"`
				MediaName string `json:"mediaName"`
				URL       string `json:"url"`
			} `json:"cmsArticleWebOld"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, market.Wrap(e.name, market.ErrNotAvailable, fmt.Errorf("解析资讯 JSONP: %w", err))
	}

	loc := financeLocation(inst)
	cutoff := time.Now().In(loc).AddDate(0, 0, -days)
	out := make([]market.NewsItem, 0, len(resp.Result.Articles))
	for _, a := range resp.Result.Articles {
		pub, perr := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(a.Date), loc)
		if perr != nil {
			// 时间解析不了的条目无法做时间窗过滤与排序, 直接丢弃而不是猜一个时间。
			continue
		}
		if pub.Before(cutoff) {
			continue
		}
		summary := a.Content
		summary = strings.ReplaceAll(summary, "<em>", "")
		summary = strings.ReplaceAll(summary, "</em>", "")
		summary = html.UnescapeString(summary)
		title := strings.ReplaceAll(a.Title, "<em>", "")
		title = strings.ReplaceAll(title, "</em>", "")
		out = append(out, market.NewsItem{
			Symbol:    inst.Canonical,
			Title:     html.UnescapeString(strings.TrimSpace(title)),
			URL:       strings.TrimSpace(a.URL),
			Publisher: strings.TrimSpace(a.MediaName),
			Published: pub,
			Summary:   strings.TrimSpace(summary),
		})
	}
	if len(out) == 0 {
		return nil, market.Wrap(e.name, market.ErrNotAvailable,
			fmt.Errorf("最近 %d 天没有资讯: %s", days, inst.Canonical))
	}
	return out, nil
}

// eastMoneyQuoteRow 是批量报价接口的一行。
type eastMoneyQuoteRow struct {
	F2  flexFloat  `json:"f2"`
	F3  flexFloat  `json:"f3"`
	F4  flexFloat  `json:"f4"`
	F5  flexFloat  `json:"f5"`
	F6  flexFloat  `json:"f6"`
	F9  flexFloat  `json:"f9"`
	F12 flexString `json:"f12"`
	F14 flexString `json:"f14"`
	F15 flexFloat  `json:"f15"`
	F16 flexFloat  `json:"f16"`
	F17 flexFloat  `json:"f17"`
	F18 flexFloat  `json:"f18"`
	F20 flexFloat  `json:"f20"`
	F23 flexFloat  `json:"f23"`
}

// Quote 取单个标的的最新报价。批量接口一次最多 50 个 secid, 这里只发一个。
func (e *eastMoneyProvider) Quote(ctx context.Context, inst market.Instrument) (*market.Quote, error) {
	secid, ok := secID(inst)
	if !ok {
		return nil, market.Wrap(e.name, market.ErrUnsupported, nil)
	}
	q := url.Values{}
	q.Set("fltt", "2")
	q.Set("invt", "2")
	q.Set("secids", secid)
	q.Set("fields", "f12,f14,f2,f3,f4,f5,f6,f15,f16,f17,f18,f20,f9,f23")
	rawURL := joinURL(e.baseURL, "api/qt/ulist.np/get") + "?" + q.Encode()

	body, err := e.get(ctx, rawURL, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data *struct {
			Diff []eastMoneyQuoteRow `json:"diff"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, market.Wrap(e.name, market.ErrNotAvailable, fmt.Errorf("解析报价 JSON: %w", err))
	}
	if resp.Data == nil || len(resp.Data.Diff) == 0 {
		return nil, market.Wrap(e.name, market.ErrNotAvailable, fmt.Errorf("报价为空: %s", inst.Canonical))
	}
	row := resp.Data.Diff[0]
	// f2 缺失但 f18 (昨收) 存在时, 停牌股仍可返回 halted 状态; 两者都缺失才报错。
	if !row.F2.Set && !row.F18.Set {
		return nil, market.Wrap(e.name, market.ErrNotAvailable,
			fmt.Errorf("报价缺少最新价与昨收 (f2/f18): %s", inst.Canonical))
	}
	if inst.Name == "" && row.F14 != "" {
		inst.Name = string(row.F14)
	}
	quote := &market.Quote{
		Inst:      inst,
		Last:      row.F2.Value,
		PrevClose: row.F18.Value,
		Open:      row.F17.Value,
		High:      row.F15.Value,
		Low:       row.F16.Value,
		Volume:    int64(row.F5.Value), // f5 单位: 手
		Turnover:  row.F6.Value,        // f6 单位: 元
		AsOf:      time.Now(),
		Delay:     "realtime",
		Source:    e.name,
		SourceURL: rawURL,
	}
	withSession(quote, inst)
	return quote, nil
}

// History 取 K 线。Interval 支持 1d/1w/1m, Adjust 支持 none/qfq/hfq。
func (e *eastMoneyProvider) History(ctx context.Context, inst market.Instrument, p market.HistoryParams) ([]market.Bar, error) {
	secid, ok := secID(inst)
	if !ok {
		return nil, market.Wrap(e.name, market.ErrUnsupported, nil)
	}
	klt, ok := eastMoneyKLT(p.Interval)
	if !ok {
		return nil, market.Wrap(e.name, market.ErrUnsupported, fmt.Errorf("不支持的 K 线周期 %q", p.Interval))
	}
	adjust := p.Adjust
	if adjust == "" {
		adjust = market.AdjustQFQ
	}
	fqt, ok := eastMoneyFQT(adjust)
	if !ok {
		return nil, market.Wrap(e.name, market.ErrUnsupported, fmt.Errorf("不支持的复权口径 %q", p.Adjust))
	}

	q := url.Values{}
	q.Set("secid", secid)
	q.Set("fields1", "f1,f2,f3,f4,f5,f6")
	q.Set("fields2", "f51,f52,f53,f54,f55,f56,f57")
	q.Set("klt", klt)
	q.Set("fqt", fqt)
	q.Set("end", eastMoneyDate(p.To))
	if !p.From.IsZero() {
		q.Set("beg", eastMoneyDate(p.From))
	}
	q.Set("lmt", strconv.Itoa(historyLimit(p)))
	rawURL := joinURL(e.historyBase, "api/qt/stock/kline/get") + "?" + q.Encode()

	body, err := e.get(ctx, rawURL, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data *struct {
			Klines []string `json:"klines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, market.Wrap(e.name, market.ErrNotAvailable, fmt.Errorf("解析 K 线 JSON: %w", err))
	}
	if resp.Data == nil || len(resp.Data.Klines) == 0 {
		return nil, market.Wrap(e.name, market.ErrNotAvailable, fmt.Errorf("K 线为空: %s", inst.Canonical))
	}
	loc := financeLocation(inst)
	bars := make([]market.Bar, 0, len(resp.Data.Klines))
	for _, line := range resp.Data.Klines {
		parts := strings.Split(line, ",")
		if len(parts) < 6 {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(parts[0]), loc)
		if err != nil {
			continue
		}
		bar := market.Bar{
			Date:   date,
			Open:   parseFloat(parts[1]),
			Close:  parseFloat(parts[2]),
			High:   parseFloat(parts[3]),
			Low:    parseFloat(parts[4]),
			Volume: int64(parseFloat(parts[5])), // 手
			Adjust: adjust,
		}
		if len(parts) > 6 {
			bar.Amount = parseFloat(parts[6]) // 元
		}
		bars = append(bars, bar)
	}
	if len(bars) == 0 {
		return nil, market.Wrap(e.name, market.ErrNotAvailable, fmt.Errorf("K 线全部无法解析: %s", inst.Canonical))
	}
	return bars, nil
}

// Fundamentals 取基础估值。返回 pe_dynamic / pb / market_cap /
// float_market_cap / roe 五类事实; 缺失字段不补零。
func (e *eastMoneyProvider) Fundamentals(ctx context.Context, inst market.Instrument, period string) ([]market.MarketFact, error) {
	secid, ok := secID(inst)
	if !ok {
		return nil, market.Wrap(e.name, market.ErrUnsupported, nil)
	}
	q := url.Values{}
	q.Set("fltt", "2")
	q.Set("invt", "2")
	q.Set("secid", secid)
	q.Set("fields", "f57,f58,f162,f167,f116,f117,f173")
	rawURL := joinURL(e.baseURL, "api/qt/stock/get") + "?" + q.Encode()

	body, err := e.get(ctx, rawURL, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data *struct {
			F57  flexString `json:"f57"`
			F58  flexString `json:"f58"`
			F162 flexFloat  `json:"f162"`
			F167 flexFloat  `json:"f167"`
			F116 flexFloat  `json:"f116"`
			F117 flexFloat  `json:"f117"`
			F173 flexFloat  `json:"f173"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, market.Wrap(e.name, market.ErrNotAvailable, fmt.Errorf("解析估值 JSON: %w", err))
	}
	if resp.Data == nil {
		return nil, market.Wrap(e.name, market.ErrNotAvailable, fmt.Errorf("估值为空: %s", inst.Canonical))
	}
	now := time.Now()
	mkFact := func(field, unit string, fv flexFloat) market.MarketFact {
		return market.MarketFact{
			Symbol:    inst.Canonical,
			Field:     field,
			Value:     fv.Value,
			Unit:      unit,
			Currency:  inst.Currency,
			AsOf:      now,
			Period:    period,
			Delay:     "eod",
			Source:    e.name,
			SourceURL: rawURL,
			FetchedAt: now,
		}
	}
	var facts []market.MarketFact
	if resp.Data.F162.Set {
		facts = append(facts, mkFact("pe_dynamic", "x", resp.Data.F162))
	}
	if resp.Data.F167.Set {
		facts = append(facts, mkFact("pb", "x", resp.Data.F167))
	}
	if resp.Data.F116.Set {
		facts = append(facts, mkFact("market_cap", inst.Currency, resp.Data.F116))
	}
	if resp.Data.F117.Set {
		facts = append(facts, mkFact("float_market_cap", inst.Currency, resp.Data.F117))
	}
	if resp.Data.F173.Set {
		facts = append(facts, mkFact("roe", "%", resp.Data.F173))
	}
	if len(facts) == 0 {
		return nil, market.Wrap(e.name, market.ErrNotAvailable,
			fmt.Errorf("估值字段全部缺失: %s", inst.Canonical))
	}
	return facts, nil
}

// eastMoneyKLT 把周期映射到东财 klt 参数。
func eastMoneyKLT(interval string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "", "1d", "day", "daily":
		return "101", true
	case "1w", "week", "weekly":
		return "102", true
	case "1m", "month", "monthly":
		return "103", true
	}
	return "", false
}

// eastMoneyFQT 把复权口径映射到 fqt 参数。
func eastMoneyFQT(adjust string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(adjust)) {
	case market.AdjustNone:
		return "0", true
	case market.AdjustQFQ:
		return "1", true
	case market.AdjustHFQ:
		return "2", true
	}
	return "", false
}

// eastMoneyDate 把时间格式化成东财的 YYYYMMDD; 零值返回 20500101 (取到最新)。
func eastMoneyDate(t time.Time) string {
	if t.IsZero() {
		return "20500101"
	}
	return t.Format("20060102")
}

// historyLimit 根据请求区间估算需要多少根 K 线, 默认 320。
func historyLimit(p market.HistoryParams) int {
	if p.From.IsZero() {
		return 320
	}
	to := p.To
	if to.IsZero() {
		to = time.Now()
	}
	days := int(to.Sub(p.From).Hours()/24) + 10
	if days < 20 {
		days = 20
	}
	if days > 5000 {
		days = 5000
	}
	return days
}
