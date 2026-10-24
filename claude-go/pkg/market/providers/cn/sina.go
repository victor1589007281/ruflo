package cn

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// 新浪财经 provider。
//
// 报价端点: https://hq.sinajs.cn/list=sh600519 (GBK)。
// **必须带 Referer: https://finance.sina.com.cn**, 否则返回 403。
//
// 响应形如 var hq_str_sh600519="贵州茅台,1695.00,..."; 字段按 "," 切分:
//
//	[0] 名称  [1] 开盘  [2] 昨收  [3] 现价  [4] 最高  [5] 最低
//	[8] 成交量(股)  [9] 成交额(元)  [30] 日期  [31] 时间
//
// 集群实测直连 200, EgressMode="direct"。
// 注意: 新浪成交量单位是"股", 与东财/腾讯的"手"不同, 不做隐式换算。

// sinaProvider 是新浪财经源。
type sinaProvider struct {
	base
}

// newSina 构造新浪财经 provider。只覆盖 A 股 (沪深北)。
func newSina(cfg market.Config, client *http.Client) *sinaProvider {
	pc := providerConf(cfg, "sina")
	baseURL := pc.BaseURL
	if baseURL == "" {
		baseURL = defaultSinaBase
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
	return &sinaProvider{
		base: base{
			name:    "sina",
			markets: []market.Market{market.MarketCN},
			caps:    market.CapSet{market.CapQuote: true},
			client:  client,
			baseURL: baseURL,
			ua:      ua,
			timeout: timeout,
		},
	}
}

// Quote 取 A 股报价。请求必须带 Referer, 响应按 GBK 转码。
func (s *sinaProvider) Quote(ctx context.Context, inst market.Instrument) (*market.Quote, error) {
	if inst.Market != market.MarketCN {
		return nil, market.Wrap(s.name, market.ErrUnsupported, nil)
	}
	prefix, ok := matchSymbol(inst)
	if !ok {
		return nil, market.Wrap(s.name, market.ErrUnsupported, nil)
	}
	symbol := prefix + inst.Code
	rawURL := joinURL(s.baseURL, "list="+symbol)

	body, err := s.get(ctx, rawURL, map[string]string{
		"Referer": "https://finance.sina.com.cn",
	})
	if err != nil {
		return nil, err
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(body)
	if err != nil {
		decoded = body
	}
	payload, err := sinaPayload(string(decoded))
	if err != nil {
		return nil, market.Wrap(s.name, market.ErrNotAvailable, err)
	}
	parts := strings.Split(payload, ",")
	if len(parts) < 10 {
		return nil, market.Wrap(s.name, market.ErrNotAvailable,
			fmt.Errorf("新浪报价字段不足 (%d 段): %s", len(parts), inst.Canonical))
	}
	lastField := strings.TrimSpace(safeField(parts, 3))
	if lastField == "" {
		return nil, market.Wrap(s.name, market.ErrNotAvailable,
			fmt.Errorf("新浪报价缺少现价: %s", inst.Canonical))
	}
	if inst.Name == "" {
		inst.Name = strings.TrimSpace(safeField(parts, 0))
	}
	quote := &market.Quote{
		Inst:      inst,
		Last:      parseFloat(lastField),
		PrevClose: parseFloat(safeField(parts, 2)),
		Open:      parseFloat(safeField(parts, 1)),
		High:      parseFloat(safeField(parts, 4)),
		Low:       parseFloat(safeField(parts, 5)),
		Volume:    int64(parseFloat(safeField(parts, 8))), // 股
		Turnover:  parseFloat(safeField(parts, 9)),        // 元
		AsOf:      parseSinaTime(safeField(parts, 30), safeField(parts, 31), financeLocation(inst)),
		Delay:     "realtime",
		Source:    s.name,
		SourceURL: rawURL,
	}
	withSession(quote, inst)
	return quote, nil
}

// sinaPayload 从 `var hq_str_sh600519="..."` 中取出引号内的内容。
func sinaPayload(s string) (string, error) {
	start := strings.Index(s, `="`)
	if start < 0 {
		return "", fmt.Errorf("新浪响应格式异常: %q", truncate(s, 80))
	}
	rest := s[start+2:]
	end := strings.LastIndex(rest, `"`)
	if end < 0 {
		return "", fmt.Errorf("新浪响应缺少结束引号: %q", truncate(s, 80))
	}
	return rest[:end], nil
}

// safeField 越界安全地取逗号切分后的字段。
func safeField(parts []string, idx int) string {
	if idx < 0 || idx >= len(parts) {
		return ""
	}
	return parts[idx]
}

// parseSinaTime 解析日期 [30] 与时间 [31]; 失败时退回当前时间。
func parseSinaTime(date, clock string, loc *time.Location) time.Time {
	date = strings.TrimSpace(date)
	clock = strings.TrimSpace(clock)
	if date == "" {
		return time.Now().In(loc)
	}
	if clock != "" {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock, loc); err == nil {
			return t
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", date, loc); err == nil {
		return t
	}
	return time.Now().In(loc)
}
