package cn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// 腾讯财经 provider。
//
// 报价端点: https://qt.gtimg.cn/q=sh600519,hk00700 (GBK, 必须转码)。
// 响应形如 v_sh600519="1~贵州茅台~600519~1700.00~..."; 字段按 "~" 切分:
//
//	[1] 名称  [3] 现价  [4] 昨收  [5] 开盘  [6] 成交量(手)
//	[30] 时间 [33] 最高 [34] 最低
//
// 长度保护: 上游字段数量会随品种变化, 所有下标访问都先检查长度。
// K 线作为东方财富的备源: web.ifzq.gtimg.cn/appstock/app/fqkline/get。
//
// 集群实测 qt.gtimg.cn 直连 200, 因此 EgressMode="direct"。

const tencentHistoryBase = "https://web.ifzq.gtimg.cn"

// tencentProvider 是腾讯财经源。
type tencentProvider struct {
	base
	historyBase string
}

// newTencent 构造腾讯财经 provider。
func newTencent(cfg market.Config, client *http.Client) *tencentProvider {
	pc := providerConf(cfg, "tencent")
	baseURL := pc.BaseURL
	if baseURL == "" {
		baseURL = defaultTencentBase
	}
	ua := pc.UserAgent
	if ua == "" {
		ua = cfg.UserAgent
	}
	if ua == "" {
		ua = defaultUserAgent
	}
	timeout := pc.Timeout
	if timeout <= 0 {
		timeout = cfg.Timeout
	}
	if timeout <= 0 || timeout > sourceTimeout {
		timeout = sourceTimeout
	}
	return &tencentProvider{
		base: base{
			name:    "tencent",
			markets: []market.Market{market.MarketCN, market.MarketHK},
			caps: market.CapSet{
				market.CapQuote:   true,
				market.CapHistory: true,
			},
			client:  client,
			baseURL: baseURL,
			ua:      ua,
			timeout: timeout,
		},
		historyBase: tencentHistoryBase,
	}
}

// Quote 取单个标的报价。响应是 GBK, 必须先转码再按 "~" 切分。
func (t *tencentProvider) Quote(ctx context.Context, inst market.Instrument) (*market.Quote, error) {
	prefix, ok := matchSymbol(inst)
	if !ok {
		return nil, market.Wrap(t.name, market.ErrUnsupported, nil)
	}
	symbol := prefix + inst.Code
	rawURL := joinURL(t.baseURL, "q="+symbol)

	body, err := t.get(ctx, rawURL, map[string]string{"Referer": "https://gu.qq.com/"})
	if err != nil {
		return nil, err
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(body)
	if err != nil {
		// 个别响应本身就是 UTF-8/ASCII; 解码失败时退回原始字节。
		decoded = body
	}
	payload, err := tencentPayload(string(decoded))
	if err != nil {
		return nil, market.Wrap(t.name, market.ErrNotAvailable, err)
	}
	parts := strings.Split(payload, "~")
	// 至少要有名称/现价/昨收/开盘/量 (下标 6); 高/低/时间缺失时保留零值。
	if len(parts) < 7 {
		return nil, market.Wrap(t.name, market.ErrNotAvailable,
			fmt.Errorf("腾讯报价字段不足 (%d 段): %s", len(parts), inst.Canonical))
	}
	last := parseFloat(tencentField(parts, 3))
	if last == 0 && strings.TrimSpace(tencentField(parts, 3)) == "" {
		return nil, market.Wrap(t.name, market.ErrNotAvailable,
			fmt.Errorf("腾讯报价缺少现价: %s", inst.Canonical))
	}
	if inst.Name == "" {
		inst.Name = strings.TrimSpace(tencentField(parts, 1))
	}
	quote := &market.Quote{
		Inst:      inst,
		Last:      last,
		PrevClose: parseFloat(tencentField(parts, 4)),
		Open:      parseFloat(tencentField(parts, 5)),
		High:      parseFloat(tencentField(parts, 33)),
		Low:       parseFloat(tencentField(parts, 34)),
		Volume:    int64(parseFloat(tencentField(parts, 6))), // 手
		AsOf:      parseTencentTime(tencentField(parts, 30), financeLocation(inst)),
		Delay:     "realtime",
		Source:    t.name,
		SourceURL: rawURL,
	}
	withSession(quote, inst)
	return quote, nil
}

// History 取 K 线 (东方财富的备源)。周期 1d/1w/1m, 复权 none/qfq/hfq。
func (t *tencentProvider) History(ctx context.Context, inst market.Instrument, p market.HistoryParams) ([]market.Bar, error) {
	prefix, ok := matchSymbol(inst)
	if !ok {
		return nil, market.Wrap(t.name, market.ErrUnsupported, nil)
	}
	period, ok := tencentPeriod(p.Interval)
	if !ok {
		return nil, market.Wrap(t.name, market.ErrUnsupported, fmt.Errorf("不支持的 K 线周期 %q", p.Interval))
	}
	adjust := p.Adjust
	if adjust == "" {
		adjust = market.AdjustQFQ
	}
	switch adjust {
	case market.AdjustNone, market.AdjustQFQ, market.AdjustHFQ:
	default:
		return nil, market.Wrap(t.name, market.ErrUnsupported, fmt.Errorf("不支持的复权口径 %q", p.Adjust))
	}
	symbol := prefix + inst.Code
	adjustParam := adjust
	if adjust == market.AdjustNone {
		adjustParam = ""
	}
	param := fmt.Sprintf("%s,%s,,,%d,%s", symbol, period, historyLimit(p), adjustParam)
	rawURL := joinURL(t.historyBase, "appstock/app/fqkline/get") + "?param=" + url.QueryEscape(param)

	body, err := t.get(ctx, rawURL, map[string]string{"Referer": "https://gu.qq.com/"})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code int                                   `json:"code"`
		Data map[string]map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, market.Wrap(t.name, market.ErrNotAvailable, fmt.Errorf("解析腾讯 K 线 JSON: %w", err))
	}
	perSymbol, ok := resp.Data[symbol]
	if !ok {
		return nil, market.Wrap(t.name, market.ErrNotAvailable,
			fmt.Errorf("腾讯 K 线无该标的: %s", symbol))
	}
	// qfq/hfq 的 key 形如 qfqday/hfqweek; 不复权直接用 day/week/month。
	keys := []string{adjust + period, period}
	var rows [][]json.RawMessage
	for _, key := range keys {
		raw, ok := perSymbol[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, &rows); err == nil && len(rows) > 0 {
			break
		}
	}
	if len(rows) == 0 {
		return nil, market.Wrap(t.name, market.ErrNotAvailable,
			fmt.Errorf("腾讯 K 线为空: %s", inst.Canonical))
	}
	loc := financeLocation(inst)
	bars := make([]market.Bar, 0, len(rows))
	for _, row := range rows {
		if len(row) < 6 {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", tencentJSONString(row[0]), loc)
		if err != nil {
			continue
		}
		bar := market.Bar{
			Date:   date,
			Open:   tencentJSONFloat(row[1]),
			Close:  tencentJSONFloat(row[2]),
			High:   tencentJSONFloat(row[3]),
			Low:    tencentJSONFloat(row[4]),
			Volume: int64(tencentJSONFloat(row[5])), // 手
			Adjust: adjust,
		}
		if !p.From.IsZero() && bar.Date.Before(p.From) {
			continue
		}
		if !p.To.IsZero() && bar.Date.After(p.To) {
			continue
		}
		bars = append(bars, bar)
	}
	if len(bars) == 0 {
		return nil, market.Wrap(t.name, market.ErrNotAvailable,
			fmt.Errorf("腾讯 K 线区间为空: %s", inst.Canonical))
	}
	return bars, nil
}

// tencentPayload 从 `v_sh600519="..."` 中取出引号内的内容。
func tencentPayload(s string) (string, error) {
	start := strings.Index(s, `="`)
	if start < 0 {
		return "", fmt.Errorf("腾讯响应格式异常: %q", truncate(s, 80))
	}
	rest := s[start+2:]
	end := strings.LastIndex(rest, `"`)
	if end < 0 {
		return "", fmt.Errorf("腾讯响应缺少结束引号: %q", truncate(s, 80))
	}
	return rest[:end], nil
}

// tencentField 越界安全地取 "~" 切分后的字段。
func tencentField(parts []string, idx int) string {
	if idx < 0 || idx >= len(parts) {
		return ""
	}
	return parts[idx]
}

// parseTencentTime 解析腾讯的时间字段; 无法解析时退回当前时间。
func parseTencentTime(s string, loc *time.Location) time.Time {
	s = strings.TrimSpace(s)
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"20060102150405",
		"2006/01/02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t
		}
	}
	return time.Now().In(loc)
}

// tencentPeriod 把周期映射到腾讯的参数。
func tencentPeriod(interval string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "", "1d", "day", "daily":
		return "day", true
	case "1w", "week", "weekly":
		return "week", true
	case "1m", "month", "monthly":
		return "month", true
	}
	return "", false
}

// tencentJSONString 取 JSON 数组元素的字符串值 (元素可能是字符串或数字)。
func tencentJSONString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// tencentJSONFloat 取 JSON 数组元素的浮点值。
func tencentJSONFloat(raw json.RawMessage) float64 {
	v, _ := parseJSONNumber(string(raw))
	return v
}

// truncate 截断过长的响应片段, 避免错误信息把日志刷爆。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
