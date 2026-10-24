package global

import (
	"net/http"

	"github.com/anthropic/claude-go/pkg/market"
)

// 编译期确认各源实现了 Provider、能力接口与出口声明。
var (
	_ market.Provider             = (*stooqProvider)(nil)
	_ market.QuoteProvider        = (*stooqProvider)(nil)
	_ market.HistoryProvider      = (*stooqProvider)(nil)
	_ market.EgressPinner         = (*stooqProvider)(nil)
	_ market.Provider             = (*yahooProvider)(nil)
	_ market.QuoteProvider        = (*yahooProvider)(nil)
	_ market.HistoryProvider      = (*yahooProvider)(nil)
	_ market.EgressPinner         = (*yahooProvider)(nil)
	_ market.Provider             = (*secProvider)(nil)
	_ market.FilingsProvider      = (*secProvider)(nil)
	_ market.FundamentalsProvider = (*secProvider)(nil) // 能力未声明, 方法恒返回 ErrUnsupported
	_ market.EgressPinner         = (*secProvider)(nil)
	_ market.Provider             = (*binanceProvider)(nil)
	_ market.QuoteProvider        = (*binanceProvider)(nil)
	_ market.HistoryProvider      = (*binanceProvider)(nil)
	_ market.EgressPinner         = (*binanceProvider)(nil)
	_ market.Provider             = (*coingeckoProvider)(nil)
	_ market.QuoteProvider        = (*coingeckoProvider)(nil)
	_ market.HistoryProvider      = (*coingeckoProvider)(nil)
	_ market.EgressPinner         = (*coingeckoProvider)(nil)
	_ market.Provider             = (*fredProvider)(nil)
	_ market.MacroProvider        = (*fredProvider)(nil)
	_ market.EgressPinner         = (*fredProvider)(nil)
	_ market.Provider             = (*worldbankProvider)(nil)
	_ market.MacroProvider        = (*worldbankProvider)(nil)
	_ market.EgressPinner         = (*worldbankProvider)(nil)
)

// Providers 构造美股、加密与宏观的海外数据源。
//
// client 由接线层注入, 按 "direct" | "proxy" | "auto" 返回对应出口的 *http.Client;
// 数据面本身不依赖 pkg/search。返回顺序表达的是集群实测下的美股兜底偏好:
// Stooq(EOD) 在前, Yahoo(盘中价, 常被 429) 在后。
//
// 出口与集群实测 (2026-10-24, k8s agents pod):
//   - Yahoo: proxy, 直连与代理都可能 429, 429 不重试, registry 应快速切 Stooq;
//   - Stooq: direct, 失败时由本包用 proxy 客户端回落一次;
//   - Binance / CoinGecko: proxy, 直连 000;
//   - SEC: direct, 未实测, 需可识别 UA 且 ≤10 req/s;
//   - FRED: direct, 仅当配置或环境中存在 FRED_API_KEY 时才加入;
//   - World Bank: proxy, 集群内直连不可达。
func Providers(cfg market.Config, client func(mode string) *http.Client) []market.Provider {
	if client == nil {
		client = func(string) *http.Client { return http.DefaultClient }
	}
	out := make([]market.Provider, 0, 7)

	// 美股报价现实主源: Stooq 的 EOD 收盘价。
	if pc, ok := gProviderConfig(cfg, "stooq", "https://stooq.com"); ok {
		out = append(out, newStooq(pc, cfg, client("direct"), client("proxy")))
	}
	// Yahoo 提供盘中价, 但集群内共享出口 IP 常被限流, 只作补充。
	if pc, ok := gProviderConfig(cfg, "yahoo", "https://query1.finance.yahoo.com"); ok {
		out = append(out, newYahoo(pc, cfg, client("proxy")))
	}
	if pc, ok := gProviderConfig(cfg, "sec", "https://data.sec.gov"); ok {
		out = append(out, newSEC(pc, cfg, client("direct")))
	}
	if pc, ok := gProviderConfig(cfg, "binance", "https://api.binance.com"); ok {
		out = append(out, newBinance(pc, cfg, client("proxy")))
	}
	if pc, ok := gProviderConfig(cfg, "coingecko", "https://api.coingecko.com"); ok {
		out = append(out, newCoinGecko(pc, cfg, client("proxy")))
	}
	if pc, ok := gProviderConfig(cfg, "fred", "https://api.stlouisfed.org"); ok {
		// 无 key 时直接跳过, 不构造一个注定失败的源。
		if fp := newFRED(pc, cfg, client("direct")); fp.apiKey != "" {
			out = append(out, fp)
		}
	}
	if pc, ok := gProviderConfig(cfg, "worldbank", "https://api.worldbank.org"); ok {
		// 集群实测 (2026-10-24): 直连 api.worldbank.org 25s 无响应, 经代理 6.2s 返回;
		// 这里必须钉死代理, 否则宏观查询稳定超时。
		out = append(out, newWorldBank(pc, cfg, client("proxy")))
	}
	return out
}

// gProviderConfig 取单个源配置: 未在 map 中出现时按启用处理, BaseURL 缺失时用默认值。
func gProviderConfig(cfg market.Config, name, defBase string) (market.ProviderConfig, bool) {
	pc, ok := cfg.Providers[name]
	if !ok {
		pc = market.ProviderConfig{Enabled: true}
	}
	if pc.BaseURL == "" {
		pc.BaseURL = defBase
	}
	return pc, pc.Enabled
}
