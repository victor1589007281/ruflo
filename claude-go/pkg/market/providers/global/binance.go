package global

import (
	"context"
	"encoding/json"
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

// binanceProvider 适配 Binance 现货公开行情。
//
// 集群实测 (2026-10-24, k8s agents pod): 直连 000 (超时/不可达), 经代理 200。
// 因此固定走 proxy。Binance 提供 tick 级实时价, 可作为加密报价的主源;
// 但不要与 CoinGecko 的聚合价直接比较 (口径与交易所不同)。
type binanceProvider struct{ *gBase }

// newBinance 构造 Binance 适配器。
func newBinance(pc market.ProviderConfig, cfg market.Config, client *http.Client) *binanceProvider {
	b := newBase("binance", pc, cfg, client, "",
		[]market.Market{market.MarketCrypto},
		gCaps(market.CapQuote, market.CapHistory), "https://api.binance.com")
	return &binanceProvider{gBase: b}
}

// EgressMode 返回 Binance 的固定出口; 直连在集群内不可达。
func (p *binanceProvider) EgressMode() string { return "proxy" }

// binanceTicker 是 /api/v3/ticker/24hr 的字段子集; Binance 数值均为字符串。
type binanceTicker struct {
	Symbol         string `json:"symbol"`
	LastPrice      string `json:"lastPrice"`
	PrevClosePrice string `json:"prevClosePrice"`
	OpenPrice      string `json:"openPrice"`
	HighPrice      string `json:"highPrice"`
	LowPrice       string `json:"lowPrice"`
	Volume         string `json:"volume"`
	QuoteVolume    string `json:"quoteVolume"`
	CloseTime      int64  `json:"closeTime"`
}

// Quote 返回 24h ticker 中的最新价。
// 涨跌幅由 Last/PrevClose 重算; PrevClose 缺失时退回开盘价。
func (p *binanceProvider) Quote(ctx context.Context, inst market.Instrument) (*market.Quote, error) {
	sym, err := binanceSymbol(inst)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("symbol", sym)
	rawURL := p.base + "/api/v3/ticker/24hr?" + q.Encode()

	var t binanceTicker
	if err := p.getJSON(ctx, rawURL, &t); err != nil {
		return nil, err
	}
	last := gFloat(t.LastPrice)
	if last == 0 {
		return nil, market.Wrap(p.Name(), market.ErrNotAvailable,
			fmt.Errorf("binance: empty price for %s", sym))
	}
	prev := gFloat(t.PrevClosePrice)
	if prev == 0 {
		prev = gFloat(t.OpenPrice)
	}
	asOf := time.Time{}
	if t.CloseTime > 0 {
		asOf = time.UnixMilli(t.CloseTime).UTC()
	}
	out := &market.Quote{
		Inst:      inst,
		Last:      last,
		PrevClose: prev,
		Open:      gFloat(t.OpenPrice),
		High:      gFloat(t.HighPrice),
		Low:       gFloat(t.LowPrice),
		Volume:    gRoundInt(gFloat(t.Volume)),
		Turnover:  gFloat(t.QuoteVolume),
		Session:   market.SessionRegular,
		Tradable:  true,
		AsOf:      asOf,
		Delay:     "realtime",
		Source:    p.Name(),
		SourceURL: rawURL,
	}
	if out.Inst.Currency == "" {
		out.Inst.Currency = "USDT"
	}
	if out.Inst.Timezone == "" {
		out.Inst.Timezone = "UTC"
	}
	if out.PrevClose > 0 {
		out.ChangePct = (out.Last - out.PrevClose) / out.PrevClose * 100
	}
	return out, nil
}

// History 返回加密 K 线。Binance klines 的 12 列中本层使用
// openTime/OHLCV/closeTime/quoteAssetVolume, 其余列忽略。
func (p *binanceProvider) History(ctx context.Context, inst market.Instrument, hp market.HistoryParams) ([]market.Bar, error) {
	sym, err := binanceSymbol(inst)
	if err != nil {
		return nil, err
	}
	interval, err := binanceInterval(hp.Interval)
	if err != nil {
		return nil, market.Wrap(p.Name(), market.ErrUnsupported, err)
	}
	q := url.Values{}
	q.Set("symbol", sym)
	q.Set("interval", interval)
	q.Set("limit", strconv.Itoa(binanceLimit(hp, interval)))
	rawURL := p.base + "/api/v3/klines?" + q.Encode()

	var rows [][]json.RawMessage
	if err := p.getJSON(ctx, rawURL, &rows); err != nil {
		return nil, err
	}
	bars := make([]market.Bar, 0, len(rows))
	for _, row := range rows {
		if len(row) < 7 {
			continue
		}
		openMs := gRawInt64(row[0])
		if openMs <= 0 {
			continue
		}
		bars = append(bars, market.Bar{
			Date:   gDay(time.UnixMilli(openMs).UTC()),
			Open:   gRawFloat(row[1]),
			High:   gRawFloat(row[2]),
			Low:    gRawFloat(row[3]),
			Close:  gRawFloat(row[4]),
			Volume: gRoundInt(gRawFloat(row[5])),
			Amount: gRawFloat(row[7]),
			Adjust: market.AdjustNone,
		})
	}
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
	return bars, nil
}

// binanceSymbol 把 CRYPTO:BTC 或 BTC/USDT 归一成 Binance 交易对 (默认 USDT 计价)。
func binanceSymbol(inst market.Instrument) (string, error) {
	if inst.Market != market.MarketCrypto {
		return "", market.Wrap("binance", market.ErrUnresolved,
			fmt.Errorf("binance does not serve market %s", inst.Market))
	}
	code := strings.ToUpper(strings.TrimSpace(gCode(inst)))
	code = strings.ReplaceAll(code, "/", "")
	code = strings.ReplaceAll(code, "-", "")
	if code == "" {
		return "", market.Wrap("binance", market.ErrUnresolved, fmt.Errorf("empty crypto symbol"))
	}
	for _, quote := range []string{"USDT", "USDC", "FDUSD", "TUSD", "BUSD", "BTC", "ETH", "BNB"} {
		// len(code) > len(quote) 排除 "BTC" 本身被误判成 BTC/BTC 交易对。
		if len(code) > len(quote) && strings.HasSuffix(code, quote) {
			return code, nil
		}
	}
	return code + "USDT", nil
}

// binanceInterval 把通用周期映射到 Binance interval。
func binanceInterval(interval string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "", "1d", "d", "day":
		return "1d", nil
	case "1w", "w", "week":
		return "1w", nil
	case "1m", "m", "month":
		return "1M", nil
	default:
		return "", fmt.Errorf("unsupported interval %q", interval)
	}
}

// binanceLimit 按查询跨度估算 K 线根数; 无跨度时默认 365 根。
func binanceLimit(hp market.HistoryParams, interval string) int {
	if hp.From.IsZero() {
		switch interval {
		case "1w":
			return 260
		case "1M":
			return 120
		default:
			return 365
		}
	}
	to := hp.To
	if to.IsZero() {
		to = time.Now()
	}
	span := to.Sub(hp.From).Hours() / 24
	if span < 1 {
		span = 1
	}
	n := int(math.Ceil(span)) + 1
	switch interval {
	case "1w":
		n = int(math.Ceil(span/7)) + 1
	case "1M":
		n = int(math.Ceil(span/30)) + 1
	}
	if n < 1 {
		n = 1
	}
	if n > 1000 {
		n = 1000
	}
	return n
}
