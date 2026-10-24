package global

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// fredProvider 适配 FRED 的 series/observations API。
//
// 设计文档 §3.7.4: fredgraph.csv 直连与代理都不可达, 只有 api.stlouisfed.org
// 可用且必须带正式 key。集群内未实测, 按设计固定 direct。
// 无 key 时工厂直接跳过该源, 不会构造一个永远失败的 provider。
type fredProvider struct {
	*gBase
	apiKey string
}

// fredAliases 让常见的宏观口语名落到 FRED series id。
var fredAliases = map[string]string{
	"CPI":          "CPIAUCSL",
	"CORE_CPI":     "CPILFESL",
	"GDP":          "GDPC1",
	"UNRATE":       "UNRATE",
	"UNEMPLOYMENT": "UNRATE",
	"FEDFUNDS":     "FEDFUNDS",
	"FED_FUNDS":    "FEDFUNDS",
	"M2":           "M2SL",
	"PAYEMS":       "PAYEMS",
	"NONFARM":      "PAYEMS",
	"DGS10":        "DGS10",
	"10Y":          "DGS10",
}

// newFRED 构造 FRED 适配器; key 同时接受显式配置与环境变量。
func newFRED(pc market.ProviderConfig, cfg market.Config, client *http.Client) *fredProvider {
	key := strings.TrimSpace(pc.APIKey)
	if key == "" {
		key = strings.TrimSpace(os.Getenv("FRED_API_KEY"))
	}
	b := newBase("fred", pc, cfg, client, "",
		[]market.Market{market.MarketMacro},
		gCaps(market.CapMacro), "https://api.stlouisfed.org")
	return &fredProvider{gBase: b, apiKey: key}
}

// EgressMode 返回 FRED 的固定出口; 设计文档将它归为 direct。
func (p *fredProvider) EgressMode() string { return "direct" }

// fredObservations 是 series/observations 的响应子集。
type fredObservations struct {
	RealtimeStart string `json:"realtime_start"`
	RealtimeEnd   string `json:"realtime_end"`
	Observations  []struct {
		Date  string `json:"date"`
		Value string `json:"value"`
	} `json:"observations"`
}

// Macro 返回一条 FRED 宏观序列。缺失值 (".") 跳过, 不插值、不补齐。
// SourceURL 只保留公开参数, 绝不把 api_key 写进可上报字段或日志。
func (p *fredProvider) Macro(ctx context.Context, series string, from, to time.Time) ([]market.MarketFact, error) {
	if p.apiKey == "" {
		return nil, market.Wrap(p.Name(), market.ErrUnavailable,
			fmt.Errorf("FRED_API_KEY 未配置"))
	}
	sid := fredSeriesID(series)
	if sid == "" {
		return nil, market.Wrap(p.Name(), market.ErrUnresolved, fmt.Errorf("empty FRED series"))
	}

	q := url.Values{}
	q.Set("series_id", sid)
	q.Set("api_key", p.apiKey)
	q.Set("file_type", "json")
	if !from.IsZero() {
		q.Set("observation_start", from.UTC().Format("2006-01-02"))
	}
	if !to.IsZero() {
		q.Set("observation_end", to.UTC().Format("2006-01-02"))
	}
	q.Set("limit", "100000")
	rawURL := p.base + "/fred/series/observations?" + q.Encode()

	var out fredObservations
	if err := p.getJSON(ctx, rawURL, &out); err != nil {
		return nil, err
	}

	displayURL := p.base + "/fred/series/observations?series_id=" +
		url.QueryEscape(sid) + "&file_type=json"
	now := time.Now().UTC()
	facts := make([]market.MarketFact, 0, len(out.Observations))
	for _, o := range out.Observations {
		value := strings.TrimSpace(o.Value)
		if value == "" || value == "." {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			continue
		}
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(o.Date), time.UTC)
		if err != nil {
			continue
		}
		facts = append(facts, market.MarketFact{
			Symbol:    sid,
			Field:     sid,
			Value:     v,
			AsOf:      d,
			Period:    d.Format("2006-01-02"),
			Delay:     "eod",
			Source:    p.Name(),
			SourceURL: displayURL,
			FetchedAt: now,
		})
	}
	sort.SliceStable(facts, func(i, j int) bool { return facts[i].AsOf.Before(facts[j].AsOf) })
	return facts, nil
}

// fredSeriesID 归一 FRED series id: 常见别名展开, 其余按大写原样传递。
func fredSeriesID(series string) string {
	s := strings.ToUpper(strings.TrimSpace(series))
	if s == "" {
		return ""
	}
	if id, ok := fredAliases[s]; ok {
		return id
	}
	return s
}
