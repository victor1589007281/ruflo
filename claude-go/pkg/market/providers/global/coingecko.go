package global

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// coingeckoProvider 适配 CoinGecko 公开 API。
//
// 集群实测 (2026-10-24, k8s agents pod): 直连 000, 经代理 200, 固定走 proxy。
// CoinGecko 是聚合价而非交易所逐笔价, 更新频率约 1–5 分钟, 因此 Delay 标 15min,
// 不与 Binance 的实时价混用。
type coingeckoProvider struct{ *gBase }

// coingeckoIDs 是常见 symbol → CoinGecko id; 未命中的代码回退为小写 id。
var coingeckoIDs = map[string]string{
	"BTC":    "bitcoin",
	"ETH":    "ethereum",
	"SOL":    "solana",
	"BNB":    "binancecoin",
	"XRP":    "ripple",
	"ADA":    "cardano",
	"DOGE":   "dogecoin",
	"TRX":    "tron",
	"TON":    "the-open-network",
	"SHIB":   "shiba-inu",
	"USDT":   "tether",
	"USDC":   "usd-coin",
	"BCH":    "bitcoin-cash",
	"XLM":    "stellar",
	"ATOM":   "cosmos",
	"UNI":    "uniswap",
	"ETC":    "ethereum-classic",
	"FIL":    "filecoin",
	"ICP":    "internet-computer",
	"APT":    "aptos",
	"ARB":    "arbitrum",
	"OP":     "optimism",
	"NEAR":   "near",
	"SUI":    "sui",
	"HBAR":   "hedera-hashgraph",
	"VET":    "vechain",
	"ALGO":   "algorand",
	"LINK":   "chainlink",
	"DOT":    "polkadot",
	"AVAX":   "avalanche-2",
	"LTC":    "litecoin",
	"MATIC":  "polygon-ecosystem-token",
	"POL":    "polygon-ecosystem-token",
	"AAVE":   "aave",
	"MKR":    "maker",
	"XMR":    "monero",
	"XTZ":    "tezos",
	"EOS":    "eos",
	"CRV":    "curve-dao-token",
	"PEPE":   "pepe",
	"WIF":    "dogwifcoin",
	"JUP":    "jupiter-exchange-solana",
	"RENDER": "render-token",
	"INJ":    "injective-protocol",
	"SEI":    "sei-network",
	"TIA":    "celestia",
}

// newCoinGecko 构造 CoinGecko 适配器。
func newCoinGecko(pc market.ProviderConfig, cfg market.Config, client *http.Client) *coingeckoProvider {
	b := newBase("coingecko", pc, cfg, client, "",
		[]market.Market{market.MarketCrypto},
		gCaps(market.CapQuote, market.CapHistory), "https://api.coingecko.com")
	return &coingeckoProvider{gBase: b}
}

// EgressMode 返回 CoinGecko 的固定出口; 直连在集群内不可达。
func (p *coingeckoProvider) EgressMode() string { return "proxy" }

// cgSimpleEntry 是 simple/price 的单个币种结果。
type cgSimpleEntry struct {
	USD           float64  `json:"usd"`
	Change        *float64 `json:"usd_24h_change"`
	LastUpdatedAt int64    `json:"last_updated_at"`
}

// Quote 返回 vs_currency=usd 的最新聚合价。
// AsOf 优先用上游 last_updated_at; 昨收由 24h 涨跌幅反推,
// ChangePct 仍按 Last/PrevClose 重算, 不直接采信上游百分比。
func (p *coingeckoProvider) Quote(ctx context.Context, inst market.Instrument) (*market.Quote, error) {
	id, err := coingeckoID(inst)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("ids", id)
	q.Set("vs_currencies", "usd")
	q.Set("include_24hr_change", "true")
	q.Set("include_last_updated_at", "true")
	rawURL := p.base + "/api/v3/simple/price?" + q.Encode()

	var out map[string]cgSimpleEntry
	if err := p.getJSON(ctx, rawURL, &out); err != nil {
		return nil, err
	}
	entry, ok := out[id]
	if !ok || entry.USD <= 0 {
		return nil, market.Wrap(p.Name(), market.ErrNotAvailable,
			fmt.Errorf("coingecko: no price for %s", id))
	}

	var prev, change float64
	if entry.Change != nil && *entry.Change > -100 {
		prev = entry.USD / (1 + *entry.Change/100)
		if prev > 0 {
			change = (entry.USD - prev) / prev * 100
		}
	}
	asOf := time.Now().UTC()
	if entry.LastUpdatedAt > 0 {
		asOf = time.Unix(entry.LastUpdatedAt, 0).UTC()
	}
	quote := &market.Quote{
		Inst:      inst,
		Last:      entry.USD,
		PrevClose: prev,
		ChangePct: change,
		Session:   market.SessionRegular,
		Tradable:  true,
		AsOf:      asOf,
		Delay:     "15min",
		Source:    p.Name(),
		SourceURL: rawURL,
	}
	if quote.Inst.Currency == "" {
		quote.Inst.Currency = "USD"
	}
	if quote.Inst.Timezone == "" {
		quote.Inst.Timezone = "UTC"
	}
	return quote, nil
}

// cgChart 是 /coins/{id}/market_chart 的响应。
type cgChart struct {
	Prices       [][2]float64 `json:"prices"`
	MarketCaps   [][2]float64 `json:"market_caps"`
	TotalVolumes [][2]float64 `json:"total_volumes"`
}

// History 返回加密 K 线。CoinGecko 只给价格点与成交量点, 本层按请求周期
// 聚合成 OHLCV; 未请求周期时默认 1d。公开 API 最多回溯 365 天。
func (p *coingeckoProvider) History(ctx context.Context, inst market.Instrument, hp market.HistoryParams) ([]market.Bar, error) {
	id, err := coingeckoID(inst)
	if err != nil {
		return nil, err
	}
	interval, err := coingeckoInterval(hp.Interval)
	if err != nil {
		return nil, market.Wrap(p.Name(), market.ErrUnsupported, err)
	}
	days := coingeckoDays(hp)
	q := url.Values{}
	q.Set("vs_currency", "usd")
	q.Set("days", strconv.Itoa(days))
	rawURL := p.base + "/api/v3/coins/" + url.PathEscape(id) + "/market_chart?" + q.Encode()

	var out cgChart
	if err := p.getJSON(ctx, rawURL, &out); err != nil {
		return nil, err
	}
	bars := aggregateCoinGecko(out.Prices, out.TotalVolumes, interval, hp.From, hp.To)
	if len(bars) == 0 {
		return nil, market.Wrap(p.Name(), market.ErrNotAvailable,
			fmt.Errorf("coingecko: no prices for %s", id))
	}
	return bars, nil
}

// coingeckoID 把加密标的映射成 CoinGecko id; 未收录的代码回退为小写 id。
func coingeckoID(inst market.Instrument) (string, error) {
	if inst.Market != market.MarketCrypto {
		return "", market.Wrap("coingecko", market.ErrUnresolved,
			fmt.Errorf("coingecko does not serve market %s", inst.Market))
	}
	code := strings.TrimSpace(gCode(inst))
	if code == "" {
		return "", market.Wrap("coingecko", market.ErrUnresolved, fmt.Errorf("empty crypto symbol"))
	}
	if id, ok := coingeckoIDs[strings.ToUpper(code)]; ok {
		return id, nil
	}
	return strings.ToLower(code), nil
}

// coingeckoInterval 把通用周期映射成聚合粒度。
func coingeckoInterval(interval string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "", "1d", "d", "day":
		return "1d", nil
	case "1w", "w", "week":
		return "1w", nil
	case "1m", "m", "month":
		return "1m", nil
	default:
		return "", fmt.Errorf("unsupported interval %q", interval)
	}
}

// coingeckoDays 计算 market_chart 的 days 参数; 公开 API 上限 365 天。
func coingeckoDays(hp market.HistoryParams) int {
	if hp.From.IsZero() {
		return 365
	}
	to := hp.To
	if to.IsZero() {
		to = time.Now()
	}
	days := int(math.Ceil(to.Sub(hp.From).Hours() / 24))
	if days < 1 {
		days = 1
	}
	if days > 365 {
		days = 365
	}
	return days
}

// cgBucketState 是聚合中的单根 K 线状态。
type cgBucketState struct {
	date                   time.Time
	open, high, low, close float64
	count                  int
}

// aggregateCoinGecko 把价格点与成交量点按 UTC 日/周/月聚合为 OHLCV。
// 价格点决定开高低收, 成交量单独按同一桶求和, 避免两组时间戳不完全对齐。
func aggregateCoinGecko(prices, volumes [][2]float64, interval string, from, to time.Time) []market.Bar {
	states := make(map[int64]*cgBucketState)
	order := make([]int64, 0, len(prices))
	for _, pt := range prices {
		if pt[0] <= 0 {
			continue
		}
		t := time.UnixMilli(int64(pt[0])).UTC()
		key := cgBucket(t, interval)
		k := key.Unix()
		st, ok := states[k]
		if !ok {
			st = &cgBucketState{date: key}
			states[k] = st
			order = append(order, k)
		}
		v := pt[1]
		if st.count == 0 {
			st.open = v
			st.high = v
			st.low = v
		}
		if v > st.high {
			st.high = v
		}
		if v < st.low {
			st.low = v
		}
		st.close = v
		st.count++
	}

	vols := make(map[int64]float64)
	for _, pt := range volumes {
		if pt[0] <= 0 {
			continue
		}
		key := cgBucket(time.UnixMilli(int64(pt[0])).UTC(), interval)
		vols[key.Unix()] += pt[1]
	}

	bars := make([]market.Bar, 0, len(order))
	for _, k := range order {
		st := states[k]
		if !from.IsZero() && st.date.Before(gDay(from)) {
			continue
		}
		if !to.IsZero() && st.date.After(gDay(to)) {
			continue
		}
		bars = append(bars, market.Bar{
			Date:   st.date,
			Open:   st.open,
			High:   st.high,
			Low:    st.low,
			Close:  st.close,
			Volume: gRoundInt(vols[k]),
			Adjust: market.AdjustNone,
		})
	}
	sort.SliceStable(bars, func(i, j int) bool { return bars[i].Date.Before(bars[j].Date) })
	return bars
}

// cgBucket 返回时间所属的 UTC 日/周/月起点 (周一为一周起点)。
func cgBucket(t time.Time, interval string) time.Time {
	t = t.UTC()
	switch interval {
	case "1w":
		day := gDay(t)
		offset := (int(day.Weekday()) + 6) % 7 // Monday=0
		return day.AddDate(0, 0, -offset)
	case "1m":
		y, m, _ := t.Date()
		return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	default:
		return gDay(t)
	}
}
