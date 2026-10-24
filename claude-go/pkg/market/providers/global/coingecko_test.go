package global

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

const coingeckoPriceJSON = `{"bitcoin":{"usd":50000.0,"usd_24h_change":2.0,"last_updated_at":1700086400}}`

const coingeckoChartJSON = `{
	"prices":[
		[1699920000000,100.0],
		[1699963200000,102.0],
		[1700006400000,105.0]
	],
	"market_caps":[],
	"total_volumes":[
		[1699920000000,10.0],
		[1699963200000,20.0],
		[1700006400000,30.0]
	]
}`

// TestCoinGeckoQuote 验证 simple/price 解析与昨收反推。
func TestCoinGeckoQuote(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/api/v3/simple/price" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, coingeckoPriceJSON)
	}))
	defer srv.Close()

	pc := market.ProviderConfig{Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second}
	p := newCoinGecko(pc, market.DefaultConfig(), srv.Client())
	q, err := p.Quote(context.Background(), market.Instrument{
		Canonical: "CRYPTO:BTC", Market: market.MarketCrypto, Code: "BTC",
	})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	for _, want := range []string{"ids=bitcoin", "vs_currencies=usd", "include_24hr_change=true", "include_last_updated_at=true"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query = %q, missing %q", gotQuery, want)
		}
	}
	if q.Last != 50000 {
		t.Fatalf("Last = %v", q.Last)
	}
	wantPrev := 50000.0 / 1.02
	if math.Abs(q.PrevClose-wantPrev) > 1e-9 {
		t.Fatalf("PrevClose = %v, want %v", q.PrevClose, wantPrev)
	}
	if math.Abs(q.ChangePct-2.0) > 1e-9 {
		t.Fatalf("ChangePct = %v, want 2", q.ChangePct)
	}
	if !q.AsOf.Equal(time.Unix(1700086400, 0).UTC()) {
		t.Fatalf("AsOf = %v, 应使用上游 last_updated_at", q.AsOf)
	}
	if q.Delay != "15min" || q.Inst.Currency != "USD" {
		t.Fatalf("quote = %+v", q)
	}
}

// TestCoinGeckoHistory 验证价格点按 UTC 日聚合为 OHLCV。
func TestCoinGeckoHistory(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/api/v3/coins/bitcoin/market_chart" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, coingeckoChartJSON)
	}))
	defer srv.Close()

	pc := market.ProviderConfig{Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second}
	p := newCoinGecko(pc, market.DefaultConfig(), srv.Client())
	bars, err := p.History(context.Background(), market.Instrument{
		Canonical: "CRYPTO:BTC", Market: market.MarketCrypto, Code: "BTC",
	}, market.HistoryParams{
		From:     time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2023, 11, 15, 0, 0, 0, 0, time.UTC),
		Interval: "1d",
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if !strings.Contains(gotQuery, "vs_currency=usd") || !strings.Contains(gotQuery, "days=1") {
		t.Fatalf("query = %q", gotQuery)
	}
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want 2", len(bars))
	}
	d1 := bars[0]
	if !d1.Date.Equal(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day1 date = %v", d1.Date)
	}
	if d1.Open != 100 || d1.High != 102 || d1.Low != 100 || d1.Close != 102 || d1.Volume != 30 {
		t.Fatalf("day1 = %+v", d1)
	}
	d2 := bars[1]
	if d2.Open != 105 || d2.Close != 105 || d2.Volume != 30 {
		t.Fatalf("day2 = %+v", d2)
	}
	if d1.Adjust != market.AdjustNone {
		t.Fatalf("Adjust = %q", d1.Adjust)
	}
}

// TestCoinGeckoUnknownIDFallback 验证未收录 symbol 回退为小写 id。
func TestCoinGeckoUnknownIDFallback(t *testing.T) {
	id, err := coingeckoID(market.Instrument{
		Canonical: "CRYPTO:PEPE2", Market: market.MarketCrypto, Code: "PEPE2",
	})
	if err != nil || id != "pepe2" {
		t.Fatalf("id = %q, err = %v", id, err)
	}
}
