package global

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

const yahooFixture = `{"chart":{"result":[{"meta":{
	"symbol":"0700.HK","currency":"HKD","exchangeTimezoneName":"Asia/Hong_Kong",
	"shortName":"TENCENT","regularMarketPrice":110,"previousClose":105,
	"chartPreviousClose":104,"regularMarketTime":1700086400,
	"regularMarketVolume":2000,"regularMarketOpen":106,
	"regularMarketDayHigh":112,"regularMarketDayLow":104
},"timestamp":[1700000000,1700086400],"indicators":{
	"quote":[{"open":[100,106],"high":[110,112],"low":[90,104],
	          "close":[105,110],"volume":[1000,2000]}],
	"adjclose":[{"adjclose":[90,110]}]
}}],"error":null}}`

// newTestYahoo 构造指向 httptest 服务的 Yahoo provider。
func newTestYahoo(t *testing.T, baseURL string) *yahooProvider {
	t.Helper()
	return newTestYahooTimeout(t, baseURL, 2*time.Second)
}

// newTestYahooTimeout 构造带自定义超时的 Yahoo provider。
func newTestYahooTimeout(t *testing.T, baseURL string, timeout time.Duration) *yahooProvider {
	t.Helper()
	pc := market.ProviderConfig{Enabled: true, BaseURL: baseURL, Timeout: timeout}
	cfg := market.DefaultConfig()
	cfg.UserAgent = "test-agent"
	return newYahoo(pc, cfg, &http.Client{})
}

// TestYahooQuote 验证 chart meta 解析、港股代码转换与涨跌幅重算。
func TestYahooQuote(t *testing.T) {
	var gotPath, gotUA, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, yahooFixture)
	}))
	defer srv.Close()

	p := newTestYahoo(t, srv.URL)
	inst := market.Instrument{Canonical: "HK:00700", Market: market.MarketHK, Code: "00700"}
	q, err := p.Quote(context.Background(), inst)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if gotPath != "/v8/finance/chart/0700.HK" {
		t.Fatalf("path = %q, want 0700.HK", gotPath)
	}
	if !strings.Contains(gotQuery, "range=1d") || !strings.Contains(gotQuery, "interval=1d") {
		t.Fatalf("query = %q", gotQuery)
	}
	if !strings.Contains(gotUA, "Mozilla/5.0") {
		t.Fatalf("Yahoo 必须带浏览器 UA, got %q", gotUA)
	}
	if q.Last != 110 || q.PrevClose != 105 {
		t.Fatalf("Last/PrevClose = %v/%v", q.Last, q.PrevClose)
	}
	wantPct := (110.0 - 105.0) / 105.0 * 100
	if math.Abs(q.ChangePct-wantPct) > 1e-9 {
		t.Fatalf("ChangePct = %v, want %v", q.ChangePct, wantPct)
	}
	if q.Inst.Currency != "HKD" || q.Inst.Timezone != "Asia/Hong_Kong" {
		t.Fatalf("currency/timezone = %q/%q", q.Inst.Currency, q.Inst.Timezone)
	}
	if q.Inst.Name != "TENCENT" {
		t.Fatalf("name = %q", q.Inst.Name)
	}
	if !q.AsOf.Equal(time.Unix(1700086400, 0).UTC()) {
		t.Fatalf("AsOf = %v", q.AsOf)
	}
	if q.Delay != "15min" {
		t.Fatalf("Delay = %q", q.Delay)
	}
	if q.Volume != 2000 {
		t.Fatalf("Volume = %d", q.Volume)
	}
}

// TestYahooHistoryAdjust 验证 none/qfq/hfq 三档可区分且带口径警告。
func TestYahooHistoryAdjust(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, yahooFixture)
	}))
	defer srv.Close()

	p := newTestYahoo(t, srv.URL)
	inst := market.Instrument{Canonical: "HK:00700", Market: market.MarketHK, Code: "00700"}
	from := time.Date(2023, 11, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2023, 11, 30, 0, 0, 0, 0, time.UTC)

	none, warnNone, err := p.HistoryWithWarnings(context.Background(), inst,
		market.HistoryParams{From: from, To: to, Interval: "1d", Adjust: market.AdjustNone})
	if err != nil {
		t.Fatalf("none: %v", err)
	}
	if len(none) != 2 || len(warnNone) != 0 {
		t.Fatalf("none bars=%d warnings=%v", len(none), warnNone)
	}
	if none[0].Close != 105 || none[0].Adjust != market.AdjustNone {
		t.Fatalf("none[0] = %+v", none[0])
	}
	if !none[0].Date.Equal(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("none[0].Date = %v", none[0].Date)
	}

	qfq, warnQ, err := p.HistoryWithWarnings(context.Background(), inst,
		market.HistoryParams{From: from, To: to, Interval: "1d", Adjust: market.AdjustQFQ})
	if err != nil {
		t.Fatalf("qfq: %v", err)
	}
	if len(warnQ) == 0 {
		t.Fatal("qfq 必须写口径警告")
	}
	if math.Abs(qfq[0].Close-90) > 1e-9 || qfq[0].Adjust != market.AdjustQFQ {
		t.Fatalf("qfq[0] = %+v", qfq[0])
	}
	if math.Abs(qfq[0].High-110*(90.0/105.0)) > 1e-9 {
		t.Fatalf("qfq[0].High = %v", qfq[0].High)
	}

	hfq, warnH, err := p.HistoryWithWarnings(context.Background(), inst,
		market.HistoryParams{From: from, To: to, Interval: "1d", Adjust: market.AdjustHFQ})
	if err != nil {
		t.Fatalf("hfq: %v", err)
	}
	if len(warnH) == 0 {
		t.Fatal("hfq 必须写窗口基准警告")
	}
	// 以窗口首日为基准: 第 1 根不变, 第 2 根放大 110/90 倍附近。
	if math.Abs(hfq[0].Close-105) > 1e-9 {
		t.Fatalf("hfq[0].Close = %v", hfq[0].Close)
	}
	want := 110.0 * (105.0 / 90.0)
	if math.Abs(hfq[1].Close-want) > 1e-6 {
		t.Fatalf("hfq[1].Close = %v, want %v", hfq[1].Close, want)
	}
	if hfq[0].Adjust != market.AdjustHFQ {
		t.Fatalf("hfq adjust = %q", hfq[0].Adjust)
	}
}

// TestYahooAdjCloseAlignmentWithNullRows 验证跳过 null 行后 adjclose 仍按下标对齐。
func TestYahooAdjCloseAlignmentWithNullRows(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	res := &yahooChartResult{Timestamp: []int64{1700000000, 1700086400, 1700172800}}
	res.Indicators.Quote = []yahooQuoteSeries{{
		Open:   []*float64{f(100), f(100), f(200)},
		High:   []*float64{f(100), f(100), f(200)},
		Low:    []*float64{f(100), f(100), f(200)},
		Close:  []*float64{nil, f(100), f(200)},
		Volume: []*float64{f(1), f(2), f(3)},
	}}
	res.Indicators.AdjClose = []yahooAdjClose{{
		AdjClose: []*float64{f(50), f(100), f(200)},
	}}
	bars, _ := yahooBuildBars(res, market.AdjustQFQ, market.HistoryParams{})
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want 2 (首行 close=null 被跳过)", len(bars))
	}
	if bars[0].Close != 100 || bars[1].Close != 200 {
		t.Fatalf("closes = %v/%v, want 100/200 (adjclose 下标错位)", bars[0].Close, bars[1].Close)
	}
}

// TestYahooErrors 验证 429/403 分类且 429 不做重试。
func TestYahooErrors(t *testing.T) {
	cases := []struct {
		status int
		want   market.ErrorKind
	}{
		{http.StatusTooManyRequests, market.ErrRateLimited},
		{http.StatusForbidden, market.ErrBlocked},
	}
	for _, tc := range cases {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(tc.status)
		}))
		p := newTestYahoo(t, srv.URL)
		_, err := p.Quote(context.Background(), market.Instrument{
			Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
		})
		if got := market.KindOf(err); got != tc.want {
			t.Fatalf("status %d kind = %q, want %q", tc.status, got, tc.want)
		}
		if hits.Load() != 1 {
			t.Fatalf("status %d 请求次数 = %d, 不允许重试", tc.status, hits.Load())
		}
		srv.Close()
	}
}

// TestYahooTimeoutClassification 验证超时归类为 ErrTimeout。
func TestYahooTimeoutClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		io.WriteString(w, yahooFixture)
	}))
	defer srv.Close()

	p := newTestYahooTimeout(t, srv.URL, 20*time.Millisecond)
	_, err := p.Quote(context.Background(), market.Instrument{
		Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
	})
	if market.KindOf(err) != market.ErrTimeout {
		t.Fatalf("kind = %q, want timeout", market.KindOf(err))
	}
}

// TestYahooUnsupportedMarket 验证非 US/HK 标的返回未解析错误。
func TestYahooUnsupportedMarket(t *testing.T) {
	p := newTestYahoo(t, "http://127.0.0.1:1")
	_, err := p.Quote(context.Background(), market.Instrument{
		Canonical: "CRYPTO:BTC", Market: market.MarketCrypto, Code: "BTC",
	})
	if market.KindOf(err) != market.ErrUnresolved {
		t.Fatalf("kind = %q", market.KindOf(err))
	}
}

// TestYahooNews 验证 search 资讯解析与时间窗过滤。
func TestYahooNews(t *testing.T) {
	var gotQuery, gotNewsCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/finance/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotQuery = r.URL.Query().Get("q")
		gotNewsCount = r.URL.Query().Get("newsCount")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"news":[` +
			`{"title":"Apple news","publisher":"Test Wire","link":"https://example.com/a","providerPublishTime":` +
			strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) + `},` +
			`{"title":"Old news","publisher":"Test Wire","link":"https://example.com/b","providerPublishTime":` +
			strconv.FormatInt(time.Now().AddDate(0, 0, -30).Unix(), 10) + `}` +
			`]}`))
	}))
	defer srv.Close()

	p := newTestYahoo(t, srv.URL)
	items, err := p.News(context.Background(), market.Instrument{
		Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
	}, 7)
	if err != nil {
		t.Fatalf("News: %v", err)
	}
	if gotQuery != "AAPL" || gotNewsCount != "20" {
		t.Errorf("q=%q newsCount=%q", gotQuery, gotNewsCount)
	}
	if len(items) != 1 || items[0].Title != "Apple news" || items[0].Publisher != "Test Wire" {
		t.Fatalf("items = %+v", items)
	}
}
