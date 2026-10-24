package global

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// worldbankProvider 适配 World Bank Indicators API。
//
// 集群实测 (2026-10-24, k8s agents pod): 直连 200, 固定 direct。
// World Bank 是年度指标, 不要与 FRED 的日/月频序列混在同一时间轴上比较。
type worldbankProvider struct{ *gBase }

// worldbankAliases 是常见宏观口语名到 World Bank 指标代码的映射。
var worldbankAliases = map[string]string{
	"GDP":             "NY.GDP.MKTP.CD",
	"GDP_PC":          "NY.GDP.PCAP.CD",
	"GDP_PER_CAPITA":  "NY.GDP.PCAP.CD",
	"CPI":             "FP.CPI.TOTL.ZG",
	"POPULATION":      "SP.POP.TOTL",
	"POP":             "SP.POP.TOTL",
	"UNEMPLOYMENT":    "SL.UEM.TOTL.ZS",
	"LIFE_EXPECTANCY": "SP.DYN.LE00.IN",
	"URBAN_POP":       "SP.URB.TOTL.IN.ZS",
}

// newWorldBank 构造 World Bank 适配器。
func newWorldBank(pc market.ProviderConfig, cfg market.Config, client *http.Client) *worldbankProvider {
	b := newBase("worldbank", pc, cfg, client, "",
		[]market.Market{market.MarketMacro},
		gCaps(market.CapMacro), "https://api.worldbank.org")
	return &worldbankProvider{gBase: b}
}

// EgressMode 返回 World Bank 的固定出口。
func (p *worldbankProvider) EgressMode() string { return "direct" }

// wbObservation 是 World Bank data 数组中的一项。
type wbObservation struct {
	Indicator struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"indicator"`
	Country struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"country"`
	CountryISO3 string   `json:"countryiso3code"`
	Date        string   `json:"date"`
	Value       *float64 `json:"value"`
	Unit        string   `json:"unit"`
}

// Macro 返回一条 World Bank 年度指标序列。
//
// series 支持 "US:NY.GDP.MKTP.CD" 与 "US/NY.GDP.MKTP.CD";
// 省略国家时默认 US, 并支持 GDP/CPI/POPULATION 等别名。
func (p *worldbankProvider) Macro(ctx context.Context, series string, from, to time.Time) ([]market.MarketFact, error) {
	country, indicator, err := worldbankSeries(series)
	if err != nil {
		return nil, market.Wrap(p.Name(), market.ErrUnresolved, err)
	}
	start := 1960
	if !from.IsZero() {
		start = from.Year()
	}
	end := time.Now().UTC().Year()
	if !to.IsZero() {
		end = to.Year()
	}

	q := url.Values{}
	q.Set("format", "json")
	q.Set("per_page", "1000")
	q.Set("date", fmt.Sprintf("%d:%d", start, end))
	rawURL := p.base + "/v2/country/" + url.PathEscape(country) +
		"/indicator/" + url.PathEscape(indicator) + "?" + q.Encode()

	body, err := p.get(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	return parseWorldBank(body, country, indicator, rawURL)
}

// parseWorldBank 解析 World Bank 的两段式响应 [meta, data]。
func parseWorldBank(body []byte, country, indicator, rawURL string) ([]market.MarketFact, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, market.Wrap("worldbank", market.ErrUnavailable,
			fmt.Errorf("worldbank: decode json: %w", err))
	}
	if len(raw) == 0 {
		return nil, market.Wrap("worldbank", market.ErrNotAvailable,
			fmt.Errorf("worldbank: empty response"))
	}
	// 错误响应形如 [{"message":[{"id":"...","value":"..."}]}]。
	var meta struct {
		Message []struct {
			ID    string `json:"id"`
			Value string `json:"value"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw[0], &meta); err == nil && len(meta.Message) > 0 {
		msg := strings.TrimSpace(meta.Message[0].Value)
		if msg == "" {
			msg = meta.Message[0].ID
		}
		return nil, market.Wrap("worldbank", market.ErrUnresolved,
			fmt.Errorf("worldbank: %s", msg))
	}

	var obs []wbObservation
	if len(raw) > 1 && string(raw[1]) != "null" {
		if err := json.Unmarshal(raw[1], &obs); err != nil {
			return nil, market.Wrap("worldbank", market.ErrUnavailable,
				fmt.Errorf("worldbank: decode data: %w", err))
		}
	}

	now := time.Now().UTC()
	symbol := strings.ToUpper(country) + ":" + indicator
	facts := make([]market.MarketFact, 0, len(obs))
	for _, o := range obs {
		if o.Value == nil {
			// 缺失就是缺失, 不插值 (设计文档 §7.4 第 6 条)。
			continue
		}
		asOf, ok := worldbankDate(o.Date)
		if !ok {
			continue
		}
		field := o.Indicator.ID
		if field == "" {
			field = indicator
		}
		facts = append(facts, market.MarketFact{
			Symbol:    symbol,
			Field:     field,
			Value:     *o.Value,
			Text:      o.Indicator.Value,
			Unit:      o.Unit,
			AsOf:      asOf,
			Period:    o.Date,
			Delay:     "annual",
			Source:    "worldbank",
			SourceURL: rawURL,
			FetchedAt: now,
		})
	}
	sort.SliceStable(facts, func(i, j int) bool { return facts[i].AsOf.Before(facts[j].AsOf) })
	return facts, nil
}

// worldbankSeries 解析 "国家:指标" / "国家/指标"; 省略国家时默认 US。
func worldbankSeries(series string) (string, string, error) {
	s := strings.TrimSpace(series)
	if s == "" {
		return "", "", fmt.Errorf("empty World Bank series")
	}
	for _, sep := range []string{":", "/"} {
		if i := strings.Index(s, sep); i > 0 && i < len(s)-1 {
			country := strings.ToUpper(strings.TrimSpace(s[:i]))
			indicator := strings.TrimSpace(s[i+1:])
			if country != "" && indicator != "" {
				return country, indicator, nil
			}
		}
	}
	if alias, ok := worldbankAliases[strings.ToUpper(s)]; ok {
		s = alias
	}
	return "US", s, nil
}

// worldbankDate 把年度/日期字符串还原为 UTC 时间; 年度取 1 月 1 日。
func worldbankDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return time.Time{}, false
	}
	y, err := strconv.Atoi(s[:4])
	if err != nil || y <= 0 {
		return time.Time{}, false
	}
	if len(s) >= 10 {
		if t, err := time.ParseInLocation("2006-01-02", s[:10], time.UTC); err == nil {
			return t, true
		}
	}
	return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC), true
}
