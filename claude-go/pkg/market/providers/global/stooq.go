package global

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// stooqProvider 适配 Stooq 的 EOD CSV。
//
// 集群实测 (2026-10-24, k8s agents pod): Stooq 经代理返回 200, 直连未实测。
// 因此这里仍按 direct 声明出口, 但基类配置了 proxy 作为一次性回落客户端:
// direct 失败 (被拒/超时/不可用/429) 时换 proxy 重试一次, 不做多次重试。
//
// Stooq 只有日线与 EOD, 没有盘中价。美股报价的现实主源顺序是 Stooq(EOD) →
// Yahoo(盘中价, 429 恢复后), 故本 provider 同时提供 CapQuote 作为零成本兜底。
type stooqProvider struct{ *gBase }

// newStooq 构造 Stooq 适配器。fallback 通常是 proxy 客户端, 可为 nil。
func newStooq(pc market.ProviderConfig, cfg market.Config, client, fallback *http.Client) *stooqProvider {
	b := newBase("stooq", pc, cfg, client, "",
		[]market.Market{market.MarketUS},
		gCaps(market.CapQuote, market.CapHistory), "https://stooq.com")
	if fallback != nil && fallback != client {
		b.fallback = fallback
	}
	return &stooqProvider{gBase: b}
}

// EgressMode 返回 Stooq 的首选出口; 失败时的代理回落由基类负责。
func (p *stooqProvider) EgressMode() string { return "direct" }

// Quote 用最近两个交易日的 EOD 收盘价构造报价。
// 这是收盘后的静态价, 不是可成交价: Session=closed, Tradable=false, Delay=eod。
func (p *stooqProvider) Quote(ctx context.Context, inst market.Instrument) (*market.Quote, error) {
	bars, rawURL, err := p.fetch(ctx, inst, market.HistoryParams{Interval: "1d", Adjust: market.AdjustNone})
	if err != nil {
		return nil, err
	}
	if len(bars) == 0 {
		return nil, market.Wrap(p.Name(), market.ErrNotAvailable,
			fmt.Errorf("stooq: no rows for %s", gCode(inst)))
	}
	last := bars[len(bars)-1]
	var prev float64
	if len(bars) >= 2 {
		prev = bars[len(bars)-2].Close
	}
	out := &market.Quote{
		Inst:      inst,
		Last:      last.Close,
		PrevClose: prev,
		Open:      last.Open,
		High:      last.High,
		Low:       last.Low,
		Volume:    last.Volume,
		Session:   market.SessionClosed,
		Tradable:  false,
		AsOf:      last.Date,
		Delay:     "eod",
		Source:    p.Name(),
		SourceURL: rawURL,
	}
	if out.Inst.Currency == "" {
		out.Inst.Currency = "USD"
	}
	if out.Inst.Timezone == "" {
		out.Inst.Timezone = "America/New_York"
	}
	if out.PrevClose > 0 {
		out.ChangePct = (out.Last - out.PrevClose) / out.PrevClose * 100
	}
	return out, nil
}

// History 返回 Stooq 日线; 周/月线与复权不在该源能力范围内。
func (p *stooqProvider) History(ctx context.Context, inst market.Instrument, hp market.HistoryParams) ([]market.Bar, error) {
	if strings.ToLower(strings.TrimSpace(hp.Interval)) != "" &&
		strings.ToLower(strings.TrimSpace(hp.Interval)) != "1d" {
		return nil, market.Wrap(p.Name(), market.ErrUnsupported,
			fmt.Errorf("stooq 只提供日线, 不支持 interval=%q", hp.Interval))
	}
	bars, _, err := p.fetch(ctx, inst, hp)
	return bars, err
}

// fetch 请求并解析 CSV, 同时返回请求 URL 供 SourceURL 使用。
func (p *stooqProvider) fetch(ctx context.Context, inst market.Instrument, hp market.HistoryParams) ([]market.Bar, string, error) {
	if inst.Market != market.MarketUS {
		return nil, "", market.Wrap(p.Name(), market.ErrUnresolved,
			fmt.Errorf("stooq does not serve market %s", inst.Market))
	}
	code := strings.ToLower(strings.TrimSpace(gCode(inst)))
	if code == "" {
		return nil, "", market.Wrap(p.Name(), market.ErrUnresolved, fmt.Errorf("empty symbol"))
	}
	rawURL := p.endpoint(code, hp.From, hp.To)
	body, err := p.get(ctx, rawURL)
	if err != nil {
		return nil, rawURL, err
	}
	bars, err := parseStooqCSV(body)
	if err != nil {
		return nil, rawURL, err
	}
	// 区间过滤按 UTC 日比较 (Stooq 的日期本身没有时分秒)。
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
	return bars, rawURL, nil
}

// endpoint 拼接 Stooq CSV 下载地址; d1/d2 存在时顺带收窄时间窗。
func (p *stooqProvider) endpoint(code string, from, to time.Time) string {
	q := url.Values{}
	q.Set("s", code+".us")
	q.Set("i", "d")
	if !from.IsZero() {
		q.Set("d1", from.UTC().Format("20060102"))
	}
	if !to.IsZero() {
		q.Set("d2", to.UTC().Format("20060102"))
	}
	return p.base + "/q/d/l/?" + q.Encode()
}

// parseStooqCSV 解析 Date,Open,High,Low,Close,Volume 固定列 CSV。
// 上游的 "No data"、限流提示与 HTML 错误页会被转换成可分类错误。
func parseStooqCSV(body []byte) ([]market.Bar, error) {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil, market.Wrap("stooq", market.ErrNotAvailable, fmt.Errorf("stooq: empty response"))
	}
	lower := strings.ToLower(text)
	switch {
	case strings.HasPrefix(lower, "no data"):
		return nil, market.Wrap("stooq", market.ErrNotAvailable, fmt.Errorf("stooq: no data"))
	case strings.Contains(lower, "exceeded the daily hits limit"):
		return nil, market.Wrap("stooq", market.ErrRateLimited, fmt.Errorf("stooq: daily hits limit"))
	case strings.HasPrefix(lower, "<!doctype html"), strings.HasPrefix(lower, "<html"):
		return nil, market.Wrap("stooq", market.ErrBlocked, fmt.Errorf("stooq: html challenge page"))
	}

	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	r.LazyQuotes = true

	header, err := r.Read()
	if err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, market.Wrap("stooq", market.ErrUnavailable, fmt.Errorf("stooq: read header: %w", err))
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"date", "open", "high", "low", "close"} {
		if _, ok := idx[need]; !ok {
			return nil, market.Wrap("stooq", market.ErrUnavailable,
				fmt.Errorf("stooq: missing column %q", need))
		}
	}

	var bars []market.Bar
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 单行坏数据不终止整个序列, 继续读下一行。
			continue
		}
		at := func(name string) string {
			i, ok := idx[name]
			if !ok || i >= len(row) {
				return ""
			}
			return strings.TrimSpace(row[i])
		}
		d, err := time.ParseInLocation("2006-01-02", at("date"), time.UTC)
		if err != nil {
			continue
		}
		bars = append(bars, market.Bar{
			Date:   gDay(d),
			Open:   gFloat(at("open")),
			High:   gFloat(at("high")),
			Low:    gFloat(at("low")),
			Close:  gFloat(at("close")),
			Volume: gRoundInt(gFloat(at("volume"))),
			Adjust: market.AdjustNone,
		})
	}
	return bars, nil
}
