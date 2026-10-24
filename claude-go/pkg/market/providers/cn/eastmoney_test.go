package cn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// mustInstrument 用生产解析器构造测试标的, 避免手写字段与真实身份不一致。
func mustInstrument(t *testing.T, input string) market.Instrument {
	t.Helper()
	inst, err := market.NewResolver().Resolve(context.Background(), input)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", input, err)
	}
	return inst
}

// TestEastMoneyQuote 验证 ulist 报价字段映射。
func TestEastMoneyQuote(t *testing.T) {
	var gotSecids, gotFields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/qt/ulist.np/get" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotSecids = r.URL.Query().Get("secids")
		gotFields = r.URL.Query().Get("fields")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"diff": []any{map[string]any{
					"f12": "600519", "f14": "贵州茅台",
					"f2": 1700.0, "f3": "0.59", "f4": 10.0,
					"f5": 12345, "f6": 2.1e9,
					"f15": 1710.0, "f16": 1680.0, "f17": 1695.0, "f18": 1690.0,
					"f20": 2.1e12, "f9": 28.5, "f23": 8.8,
				}},
			},
		})
	}))
	defer srv.Close()

	p := newEastMoney(market.DefaultConfig(), srv.Client(), srv.Client())
	p.baseURL = srv.URL
	p.historyBase = srv.URL
	inst := mustInstrument(t, "600519")

	q, err := p.Quote(context.Background(), inst)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if gotSecids != "1.600519" {
		t.Errorf("secids = %s, want 1.600519", gotSecids)
	}
	for _, f := range []string{"f12", "f14", "f2", "f3", "f4", "f5", "f6", "f15", "f16", "f17", "f18", "f20", "f9", "f23"} {
		if !strings.Contains(gotFields, f) {
			t.Errorf("fields 缺少 %s: %s", f, gotFields)
		}
	}
	if q.Last != 1700 || q.PrevClose != 1690 || q.Open != 1695 ||
		q.High != 1710 || q.Low != 1680 {
		t.Errorf("OHLC 映射错误: %+v", q)
	}
	if q.Volume != 12345 {
		t.Errorf("Volume = %d, want 12345", q.Volume)
	}
	if q.Turnover != 2.1e9 {
		t.Errorf("Turnover = %v, want 2.1e9", q.Turnover)
	}
	if q.Inst.Name != "贵州茅台" {
		t.Errorf("Name = %s, want 贵州茅台", q.Inst.Name)
	}
	if q.Source != "eastmoney" || q.SourceURL == "" {
		t.Errorf("Source/SourceURL = %s/%s", q.Source, q.SourceURL)
	}
}

// TestEastMoneyHaltedQuote 验证 f2 缺失、f18 存在时输出 halted 而不是报错。
func TestEastMoneyHaltedQuote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"diff": []any{map[string]any{
					"f12": "600519", "f14": "贵州茅台",
					"f2": "-", "f18": 1690.0, "f5": 0, "f6": 0,
				}},
			},
		})
	}))
	defer srv.Close()
	p := newEastMoney(market.DefaultConfig(), srv.Client(), srv.Client())
	p.baseURL = srv.URL
	p.historyBase = srv.URL

	q, err := p.Quote(context.Background(), mustInstrument(t, "600519"))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Last != 0 || q.PrevClose != 1690 || q.Volume != 0 {
		t.Errorf("原值应保留: %+v", q)
	}
	if q.Session != market.SessionHalted || q.Tradable {
		t.Errorf("Session/Tradable = %s/%v, want halted/false", q.Session, q.Tradable)
	}
}

// TestEastMoneySecID 验证 SH/SZ/BJ/HK 的 secid 前缀, 尤其是 SH:000001 指数。
func TestEastMoneySecID(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"600519", "1.600519"},
		{"SZ:000001", "0.000001"},
		{"SH:000001", "1.000001"},
		{"830799", "0.830799"},
		{"00700", "116.00700"},
	}
	for _, c := range cases {
		inst := mustInstrument(t, c.input)
		got, ok := secID(inst)
		if !ok || got != c.want {
			t.Errorf("secID(%s) = %s,%v want %s", c.input, got, ok, c.want)
		}
	}
}

// TestEastMoneyHistory 验证 K 线字段顺序与 klt/fqt 映射。
func TestEastMoneyHistory(t *testing.T) {
	var gotKLT, gotFQT, gotSecid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/qt/stock/kline/get" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		gotKLT, gotFQT, gotSecid = q.Get("klt"), q.Get("fqt"), q.Get("secid")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"code": "600519", "name": "贵州茅台",
				"klines": []string{
					"2025-06-30,1695.00,1700.00,1710.00,1680.00,12345,2100000000.00",
					"2025-07-01,1700.00,1712.00,1720.00,1690.00,22345,3100000000.00",
				},
			},
		})
	}))
	defer srv.Close()

	p := newEastMoney(market.DefaultConfig(), srv.Client(), srv.Client())
	p.baseURL = srv.URL
	p.historyBase = srv.URL
	inst := mustInstrument(t, "600519")

	from := time.Date(2025, 6, 30, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	bars, err := p.History(context.Background(), inst, market.HistoryParams{
		From: from, To: to, Interval: "1d", Adjust: market.AdjustQFQ,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if gotSecid != "1.600519" || gotKLT != "101" || gotFQT != "1" {
		t.Errorf("secid/klt/fqt = %s/%s/%s", gotSecid, gotKLT, gotFQT)
	}
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want 2", len(bars))
	}
	b := bars[0]
	if b.Open != 1695 || b.Close != 1700 || b.High != 1710 || b.Low != 1680 {
		t.Errorf("OHLC 顺序错误: %+v", b)
	}
	if b.Volume != 12345 || b.Amount != 2.1e9 {
		t.Errorf("量额 = %d/%v, want 12345/2.1e9", b.Volume, b.Amount)
	}
	if b.Adjust != market.AdjustQFQ {
		t.Errorf("Adjust = %s, want qfq", b.Adjust)
	}
	if b.Date.Format("2006-01-02") != "2025-06-30" {
		t.Errorf("Date = %s", b.Date.Format("2006-01-02"))
	}
}

// TestEastMoneyHistoryMapping 验证周/月线与复权口径参数映射。
func TestEastMoneyHistoryMapping(t *testing.T) {
	var gotKLT, gotFQT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKLT = r.URL.Query().Get("klt")
		gotFQT = r.URL.Query().Get("fqt")
		_, _ = w.Write([]byte(`{"data":{"klines":["2025-06-30,1,2,3,0.5,100,200"]}}`))
	}))
	defer srv.Close()
	p := newEastMoney(market.DefaultConfig(), srv.Client(), srv.Client())
	p.baseURL = srv.URL
	p.historyBase = srv.URL
	inst := mustInstrument(t, "600519")

	cases := []struct{ interval, adjust, klt, fqt string }{
		{"", "", "101", "1"},
		{"1w", "hfq", "102", "2"},
		{"1m", "none", "103", "0"},
	}
	for _, c := range cases {
		if _, err := p.History(context.Background(), inst, market.HistoryParams{
			Interval: c.interval, Adjust: c.adjust,
		}); err != nil {
			t.Fatalf("History(%s,%s): %v", c.interval, c.adjust, err)
		}
		if gotKLT != c.klt || gotFQT != c.fqt {
			t.Errorf("interval=%q adjust=%q -> klt/fqt %s/%s, want %s/%s",
				c.interval, c.adjust, gotKLT, gotFQT, c.klt, c.fqt)
		}
	}
}

// TestEastMoneyFundamentals 验证基础估值字段映射。
func TestEastMoneyFundamentals(t *testing.T) {
	var gotFields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/qt/stock/get" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotFields = r.URL.Query().Get("fields")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"f57": "600519", "f58": "贵州茅台",
				"f162": 28.5, "f167": 8.8, "f116": 2.1e12, "f117": 2.1e12, "f173": 31.2,
			},
		})
	}))
	defer srv.Close()
	p := newEastMoney(market.DefaultConfig(), srv.Client(), srv.Client())
	p.baseURL = srv.URL
	p.historyBase = srv.URL
	inst := mustInstrument(t, "600519")

	facts, err := p.Fundamentals(context.Background(), inst, "2025Q1")
	if err != nil {
		t.Fatalf("Fundamentals: %v", err)
	}
	for _, f := range []string{"f57", "f58", "f162", "f167", "f116", "f117", "f173"} {
		if !strings.Contains(gotFields, f) {
			t.Errorf("fields 缺少 %s: %s", f, gotFields)
		}
	}
	want := map[string]float64{
		"pe_dynamic": 28.5, "pb": 8.8, "market_cap": 2.1e12,
		"float_market_cap": 2.1e12, "roe": 31.2,
	}
	if len(facts) != len(want) {
		t.Fatalf("facts = %d, want %d", len(facts), len(want))
	}
	for _, f := range facts {
		if v, ok := want[f.Field]; !ok || f.Value != v {
			t.Errorf("fact %s = %v, want %v", f.Field, f.Value, want[f.Field])
		}
		if f.Symbol != "SH:600519" || f.Source != "eastmoney" || f.AsOf.IsZero() {
			t.Errorf("fact 元数据缺失: %+v", f)
		}
		if f.Period != "2025Q1" {
			t.Errorf("Period = %s, want 2025Q1", f.Period)
		}
	}
}

// TestEastMoneyEgress 验证东财出口必须是 auto。
func TestEastMoneyEgress(t *testing.T) {
	p := newEastMoney(market.DefaultConfig(), http.DefaultClient, http.DefaultClient)
	if got := p.EgressMode(); got != "auto" {
		t.Errorf("EgressMode = %s, want auto", got)
	}
	if p.timeout != sourceTimeout {
		t.Errorf("timeout = %s, want %s", p.timeout, sourceTimeout)
	}
}

// TestEastMoneyNews 验证 JSONP 剥壳、时间窗过滤与 HTML 标签清理。
func TestEastMoneyNews(t *testing.T) {
	now := time.Now().In(time.FixedZone("CST", 8*3600))
	recent := now.Add(-time.Hour).Format("2006-01-02 15:04:05")
	old := now.AddDate(0, 0, -30).Format("2006-01-02 15:04:05")

	var gotKeyword string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/jsonp" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var param struct {
			Keyword string `json:"keyword"`
			Param   struct {
				Article struct {
					PageSize int `json:"pageSize"`
				} `json:"cmsArticleWebOld"`
			} `json:"param"`
		}
		if err := json.Unmarshal([]byte(r.URL.Query().Get("param")), &param); err != nil {
			t.Errorf("param 不是合法 JSON: %v", err)
		}
		gotKeyword = param.Keyword
		_, _ = w.Write([]byte(`cb({"result":{"cmsArticleWebOld":[` +
			`{"date":"` + recent + `","title":"贵州茅台<em>600519</em>发布公告","content":"正文 <em>600519</em> &amp; 更多","mediaName":"测试媒体","url":"https://finance.eastmoney.com/a/1.html"},` +
			`{"date":"` + old + `","title":"旧闻","content":"x","mediaName":"测试媒体","url":"https://finance.eastmoney.com/a/2.html"}` +
			`]}})`))
	}))
	defer srv.Close()

	p := newEastMoney(market.DefaultConfig(), srv.Client(), srv.Client())
	p.searchBase = srv.URL

	items, err := p.News(context.Background(), mustInstrument(t, "600519"), 7)
	if err != nil {
		t.Fatalf("News: %v", err)
	}
	if gotKeyword != "600519" {
		t.Errorf("keyword = %q", gotKeyword)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (旧闻应被时间窗过滤): %+v", len(items), items)
	}
	it := items[0]
	if strings.Contains(it.Title, "<em>") || it.Title != "贵州茅台600519发布公告" {
		t.Errorf("title = %q", it.Title)
	}
	if it.Summary != "正文 600519 & 更多" {
		t.Errorf("summary = %q", it.Summary)
	}
	if it.Publisher != "测试媒体" || it.URL == "" || it.Symbol != "SH:600519" {
		t.Errorf("元数据 = %+v", it)
	}
}
