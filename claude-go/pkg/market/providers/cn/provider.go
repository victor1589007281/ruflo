// Package cn 提供 A 股/港股的零成本数据源: 东方财富、腾讯财经、新浪财经。
//
// 三个源都是网页接口而非契约 API, 字段编号随页面改版可能变化; 因此字段映射
// 集中在各 provider 文件里, 并用离线 httptest 固化为契约测试。
//
// 出口策略 (集群实测 2026-10-24):
//   - 东方财富 push2 直连失败/代理 502, push2his 走代理可用 → EgressMode="auto";
//   - 腾讯 qt.gtimg.cn 直连 200 (GBK) → EgressMode="direct";
//   - 新浪 hq.sinajs.cn 直连带 Referer 200 (GBK) → EgressMode="direct"。
package cn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

const (
	defaultEastMoneyBase = "https://push2.eastmoney.com"
	defaultTencentBase   = "https://qt.gtimg.cn"
	defaultSinaBase      = "https://hq.sinajs.cn"
	defaultUserAgent     = "claude-go-market/1.0 (+https://github.com/victor1589007281/ruflo)"

	// maxBody 是单个响应体的硬上限 (5MB), 防止异常页面把内存打满。
	maxBody = 5 << 20
	// sourceTimeout 与根包 sourceTimeout 对齐: 东财失败慢, 必须在此截断。
	sourceTimeout = 1500 * time.Millisecond
)

// Providers 按引用顺序构造 A 股/港股数据源。
//
// 顺序即注册顺序。腾讯排在东方财富之前: 集群实测腾讯直连稳定且快, A 股报价
// 主值可以来自腾讯, 新浪用于核价; 东方财富保留为 K 线/估值源 (走代理)。
// client("direct"|"proxy"|"auto") 由接线层注入, 返回 nil 时使用默认客户端。
func Providers(cfg market.Config, client func(mode string) *http.Client) []market.Provider {
	pickClient := func(mode string) *http.Client {
		if client != nil {
			if c := client(mode); c != nil {
				return c
			}
		}
		t := cfg.Timeout
		if t <= 0 {
			t = 8 * time.Second
		}
		return &http.Client{Timeout: t}
	}

	var out []market.Provider
	if enabled(cfg, "tencent") {
		out = append(out, newTencent(cfg, pickClient("direct")))
	}
	if enabled(cfg, "sina") {
		out = append(out, newSina(cfg, pickClient("direct")))
	}
	if enabled(cfg, "eastmoney") {
		// 东财走 auto: 直连 push2 在集群内不可达, 代理可用。
		out = append(out, newEastMoney(cfg, pickClient("auto"), pickClient("direct")))
	}
	return out
}

// enabled 判断 provider 是否启用。cfg 未列出时视为启用, 便于最小配置与测试。
func enabled(cfg market.Config, name string) bool {
	pc, ok := cfg.Providers[name]
	if !ok {
		return true
	}
	return pc.Enabled
}

// base 是三个源的公共字段。
type base struct {
	name    string
	markets []market.Market
	caps    market.CapSet
	client  *http.Client
	baseURL string
	ua      string
	timeout time.Duration
}

// Name 返回 provider 名称。
func (b *base) Name() string { return b.name }

// Markets 返回支持的市场 (副本, 防止调用方改写内部状态)。
func (b *base) Markets() []market.Market { return append([]market.Market(nil), b.markets...) }

// Capabilities 返回能力声明。未声明的能力不会被注册表路由。
func (b *base) Capabilities() market.CapSet {
	out := make(market.CapSet, len(b.caps))
	for k, v := range b.caps {
		out[k] = v
	}
	return out
}

// Healthy 只表示客户端可用; 具体站点的可达性由请求结果决定。
func (b *base) Healthy() bool { return b.client != nil }

// EgressMode 默认直连; 东方财富覆盖为 auto。
func (b *base) EgressMode() string { return "direct" }

// get 发起带 ctx、UA 与单源超时的 GET, 响应体上限 5MB。
func (b *base) get(ctx context.Context, rawURL string, headers map[string]string) ([]byte, error) {
	return b.getWith(ctx, b.client, rawURL, headers)
}

// getWith 用指定 client 发起请求 (同一套超时/UA/上限), 供需要钉死出口的调用复用。
func (b *base) getWith(ctx context.Context, client *http.Client, rawURL string, headers map[string]string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return nil, market.Wrap(b.name, market.ErrUnavailable, errors.New("http client 未初始化"))
	}
	if b.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, market.Wrap(b.name, market.ErrUnavailable, err)
	}
	ua := b.ua
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, classifyError(b.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(b.name, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, classifyError(b.name, err)
	}
	if len(body) > maxBody {
		return nil, market.Wrap(b.name, market.ErrNotAvailable,
			fmt.Errorf("响应体超过 %d 字节上限", maxBody))
	}
	return body, nil
}

// classifyError 把网络错误归一化成数据面错误类别。
func classifyError(provider string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return market.Wrap(provider, market.ErrTimeout, err)
	}
	return market.Wrap(provider, market.ErrUnavailable, err)
}

// statusError 按 HTTP 状态码归类。
func statusError(provider string, code int) error {
	err := fmt.Errorf("HTTP %d", code)
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return market.Wrap(provider, market.ErrBlocked, err)
	case http.StatusTooManyRequests:
		return market.Wrap(provider, market.ErrRateLimited, err)
	default:
		return market.Wrap(provider, market.ErrUnavailable, err)
	}
}

// providerConf 返回某个源的配置覆盖 (BaseURL/UA/Timeout)。
func providerConf(cfg market.Config, name string) market.ProviderConfig {
	if cfg.Providers == nil {
		return market.ProviderConfig{}
	}
	return cfg.Providers[name]
}

// joinURL 拼接 base 与 path, 处理尾斜杠。
func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// parseFloat 尽力解析数值; 空串、"-"、"null" 都返回 0 (呼叫方用 flexFloat 判别缺失)。
func parseFloat(s string) float64 {
	s = strings.TrimSpace(strings.Trim(s, `"`))
	if s == "" || s == "-" || s == "null" || s == "N/A" {
		return 0
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return 0
	}
	return v
}

// flexFloat 兼容 JSON 里数字与字符串混用的字段, 并记录字段是否存在。
// 东财/腾讯/新浪在停牌或字段缺失时常返回 "-", 不能当成 0 参与计算。
type flexFloat struct {
	Value float64
	Set   bool
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	v, ok := parseJSONNumber(s)
	if !ok {
		return nil
	}
	f.Value = v
	f.Set = true
	return nil
}

// parseJSONNumber 解析 JSON 数字或数字字符串。
func parseJSONNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if s == "" || s == "-" || s == "null" {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// flexString 兼容 JSON 里字符串与数字混用的字段。
type flexString string

// UnmarshalJSON 实现 json.Unmarshaler。
func (s *flexString) UnmarshalJSON(b []byte) error {
	v := strings.TrimSpace(string(b))
	if v == "null" {
		*s = ""
		return nil
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	*s = flexString(v)
	return nil
}

// exchangeOf 从 Canonical 前缀取交易所。Instrument 本身没有 exchange 字段,
// SH:000001 (上证指数) 与 SZ:000001 (平安银行) 只能靠 Canonical 区分。
func exchangeOf(inst market.Instrument) string {
	if i := strings.IndexByte(inst.Canonical, ':'); i > 0 {
		return strings.ToUpper(inst.Canonical[:i])
	}
	switch {
	case strings.HasPrefix(inst.Code, "6"):
		return "SH"
	case strings.HasPrefix(inst.Code, "0"), strings.HasPrefix(inst.Code, "3"):
		return "SZ"
	case strings.HasPrefix(inst.Code, "8"), strings.HasPrefix(inst.Code, "4"):
		return "BJ"
	}
	return ""
}

// financeLocation 返回标的所在时区; 加载失败时退回 UTC。
func financeLocation(inst market.Instrument) *time.Location {
	if inst.Timezone != "" {
		if loc, err := time.LoadLocation(inst.Timezone); err == nil {
			return loc
		}
	}
	return time.UTC
}

// matchSymbol 返回腾讯/新浪使用的市场前缀。
func matchSymbol(inst market.Instrument) (string, bool) {
	switch inst.Market {
	case market.MarketCN:
		switch exchangeOf(inst) {
		case "SH":
			return "sh", true
		case "SZ":
			return "sz", true
		case "BJ":
			return "bj", true
		}
	case market.MarketHK:
		return "hk", true
	}
	return "", false
}

// secID 返回东方财富 secid。SH=1, SZ/BJ=0, HK=116, US=105。
func secID(inst market.Instrument) (string, bool) {
	switch inst.Market {
	case market.MarketCN:
		if exchangeOf(inst) == "SH" {
			return "1." + inst.Code, true
		}
		return "0." + inst.Code, true
	case market.MarketHK:
		return "116." + inst.Code, true
	case market.MarketUS:
		return "105." + inst.Code, true
	}
	return "", false
}

// withSession 依据本地时间与成交量启发式设置 session/tradable。
//
// **启发式**: 成交量 0 且 现价==昨收 (或现价为 0) 视为停牌/无成交。真实停牌
// 应以交易所公告为准; 这里保留原始 last/volume 值, 只改状态字段。
func withSession(q *market.Quote, inst market.Instrument) {
	loc := financeLocation(inst)
	if q.AsOf.IsZero() {
		q.AsOf = time.Now().In(loc)
	} else {
		q.AsOf = q.AsOf.In(loc)
	}
	if q.Volume == 0 && q.PrevClose > 0 && (q.Last == 0 || q.Last == q.PrevClose) {
		q.Session = market.SessionHalted
		q.Tradable = false
		return
	}
	q.Session = market.SessionOf(inst.Market, q.AsOf)
	q.Tradable = q.Session == market.SessionRegular
}
