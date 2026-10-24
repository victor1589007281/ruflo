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

const binanceTickerJSON = `{
	"symbol":"BTCUSDT","lastPrice":"50000.5","prevClosePrice":"49000.0",
	"openPrice":"49500.0","highPrice":"51000.0","lowPrice":"48000.0",
	"volume":"1234.5","quoteVolume":"62000000.0","closeTime":1700086400000
}`

const binanceKlinesJSON = `[
	[1700000000000,"49000.0","50000.0","48000.0","49500.0","100.5",
	 1700086399000,"5000000.0",123,"50.0","2500000.0","0"]
]`

// TestBinanceQuote 验证 24h ticker 解析、交易对推导与涨跌幅重算。
func TestBinanceQuote(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/api/v3/ticker/24hr" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, binanceTickerJSON)
	}))
	defer srv.Close()

	pc := market.ProviderConfig{Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second}
	p := newBinance(pc, market.DefaultConfig(), srv.Client())
	q, err := p.Quote(context.Background(), market.Instrument{
		Canonical: "CRYPTO:BTC", Market: market.MarketCrypto, Code: "BTC",
	})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if !strings.Contains(gotQuery, "symbol=BTCUSDT") {
		t.Fatalf("query = %q", gotQuery)
	}
	if q.Last != 50000.5 || q.PrevClose != 49000 || q.Open != 49500 {
		t.Fatalf("quote = %+v", q)
	}
	want := (50000.5 - 49000.0) / 49000.0 * 100
	if math.Abs(q.ChangePct-want) > 1e-9 {
		t.Fatalf("ChangePct = %v, want %v", q.ChangePct, want)
	}
	if q.Volume != 1235 || q.Turnover != 62000000 {
		t.Fatalf("volume/turnover = %d/%v", q.Volume, q.Turnover)
	}
	if !q.AsOf.Equal(time.UnixMilli(1700086400000).UTC()) {
		t.Fatalf("AsOf = %v", q.AsOf)
	}
	if q.Delay != "realtime" {
		t.Fatalf("Delay = %q", q.Delay)
	}
}

// TestBinanceHistory 验证 klines 数组解析与周期/条数映射。
func TestBinanceHistory(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/api/v3/klines" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, binanceKlinesJSON)
	}))
	defer srv.Close()

	pc := market.ProviderConfig{Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second}
	p := newBinance(pc, market.DefaultConfig(), srv.Client())
	bars, err := p.History(context.Background(), market.Instrument{
		Canonical: "CRYPTO:BTC", Market: market.MarketCrypto, Code: "BTC",
	}, market.HistoryParams{Interval: "1d"})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if !strings.Contains(gotQuery, "symbol=BTCUSDT") ||
		!strings.Contains(gotQuery, "interval=1d") ||
		!strings.Contains(gotQuery, "limit=365") {
		t.Fatalf("query = %q", gotQuery)
	}
	if len(bars) != 1 {
		t.Fatalf("bars = %d", len(bars))
	}
	b := bars[0]
	if !b.Date.Equal(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Date = %v", b.Date)
	}
	if b.Open != 49000 || b.High != 50000 || b.Low != 48000 || b.Close != 49500 {
		t.Fatalf("bar = %+v", b)
	}
	if b.Volume != 101 || b.Amount != 5000000 {
		t.Fatalf("volume/amount = %d/%v", b.Volume, b.Amount)
	}
	if b.Adjust != market.AdjustNone {
		t.Fatalf("Adjust = %q", b.Adjust)
	}
}

// TestBinanceSymbol 验证交易对归一规则。
func TestBinanceSymbol(t *testing.T) {
	cases := map[string]string{
		"BTC":      "BTCUSDT",
		"BTCUSDT":  "BTCUSDT",
		"ETH/USDC": "ETHUSDC",
		"SOL-USDT": "SOLUSDT",
	}
	for code, want := range cases {
		got, err := binanceSymbol(market.Instrument{
			Canonical: "CRYPTO:" + code, Market: market.MarketCrypto, Code: code,
		})
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if got != want {
			t.Fatalf("%s -> %s, want %s", code, got, want)
		}
	}
}

// TestBinanceUnsupportedInterval 验证只支持 1d/1w/1m。
func TestBinanceUnsupportedInterval(t *testing.T) {
	pc := market.ProviderConfig{Enabled: true, BaseURL: "http://127.0.0.1:1", Timeout: time.Second}
	p := newBinance(pc, market.DefaultConfig(), nil)
	_, err := p.History(context.Background(), market.Instrument{
		Canonical: "CRYPTO:BTC", Market: market.MarketCrypto, Code: "BTC",
	}, market.HistoryParams{Interval: "5m"})
	if market.KindOf(err) != market.ErrUnsupported {
		t.Fatalf("kind = %q", market.KindOf(err))
	}
}
