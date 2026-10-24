package global

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

const stooqCSV = `Date,Open,High,Low,Close,Volume
2024-01-02,185.0,187.0,184.0,186.5,50000000
2024-01-03,186.5,188.0,185.5,187.0,60000000
`

// failingTransport 模拟直连不可达。
type failingTransport struct{}

// RoundTrip 恒返回拨号失败, 用于验证 Stooq 的 proxy 回落。
func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial failed")
}

// newTestStooq 构造指向 httptest 服务的 Stooq provider。
func newTestStooq(t *testing.T, baseURL string, direct, fallback *http.Client) *stooqProvider {
	t.Helper()
	pc := market.ProviderConfig{Enabled: true, BaseURL: baseURL, Timeout: 2 * time.Second}
	return newStooq(pc, market.DefaultConfig(), direct, fallback)
}

// TestStooqHistory 验证 CSV 解析、区间过滤与 EOD 口径。
func TestStooqHistory(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/csv")
		io.WriteString(w, stooqCSV)
	}))
	defer srv.Close()

	p := newTestStooq(t, srv.URL, srv.Client(), nil)
	inst := market.Instrument{Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL"}
	bars, err := p.History(context.Background(), inst, market.HistoryParams{
		From:     time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC),
		Interval: "1d",
		Adjust:   market.AdjustNone,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if !strings.Contains(gotQuery, "s=aapl.us") || !strings.Contains(gotQuery, "i=d") {
		t.Fatalf("query = %q", gotQuery)
	}
	if len(bars) != 1 {
		t.Fatalf("bars = %d, want 1 (区间过滤后)", len(bars))
	}
	b := bars[0]
	if !b.Date.Equal(time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Date = %v", b.Date)
	}
	if b.Open != 186.5 || b.High != 188 || b.Low != 185.5 || b.Close != 187 || b.Volume != 60000000 {
		t.Fatalf("bar = %+v", b)
	}
	if b.Adjust != market.AdjustNone {
		t.Fatalf("Adjust = %q", b.Adjust)
	}
}

// TestStooqQuote 验证用最近两根 EOD 构造报价, 并标记 closed/eod。
func TestStooqQuote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, stooqCSV)
	}))
	defer srv.Close()

	p := newTestStooq(t, srv.URL, srv.Client(), nil)
	q, err := p.Quote(context.Background(), market.Instrument{
		Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
	})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Last != 187 || q.PrevClose != 186.5 {
		t.Fatalf("Last/PrevClose = %v/%v", q.Last, q.PrevClose)
	}
	want := (187.0 - 186.5) / 186.5 * 100
	if math.Abs(q.ChangePct-want) > 1e-9 {
		t.Fatalf("ChangePct = %v, want %v", q.ChangePct, want)
	}
	if q.Delay != "eod" || q.Session != market.SessionClosed || q.Tradable {
		t.Fatalf("delay/session/tradable = %q/%q/%v", q.Delay, q.Session, q.Tradable)
	}
	if q.Source != "stooq" || q.SourceURL == "" {
		t.Fatalf("source/url = %q/%q", q.Source, q.SourceURL)
	}
}

// TestStooqDirectFallbackToProxy 验证 direct 失败后只回落 proxy 一次。
func TestStooqDirectFallbackToProxy(t *testing.T) {
	var proxyHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		io.WriteString(w, stooqCSV)
	}))
	defer srv.Close()

	direct := &http.Client{Transport: failingTransport{}}
	p := newTestStooq(t, srv.URL, direct, srv.Client())
	q, err := p.Quote(context.Background(), market.Instrument{
		Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
	})
	if err != nil {
		t.Fatalf("Quote via fallback: %v", err)
	}
	if q.Last != 187 {
		t.Fatalf("Last = %v", q.Last)
	}
	if proxyHits != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxyHits)
	}
}

// TestStooqNoData 验证上游 "No data" 映射为不可用而非空序列。
func TestStooqNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "No data")
	}))
	defer srv.Close()

	p := newTestStooq(t, srv.URL, srv.Client(), nil)
	_, err := p.History(context.Background(), market.Instrument{
		Canonical: "US:NOPE", Market: market.MarketUS, Code: "NOPE",
	}, market.HistoryParams{Interval: "1d"})
	if market.KindOf(err) != market.ErrNotAvailable {
		t.Fatalf("kind = %q", market.KindOf(err))
	}
}

// TestStooqUnsupportedInterval 验证仅支持日线。
func TestStooqUnsupportedInterval(t *testing.T) {
	p := newTestStooq(t, "http://127.0.0.1:1", nil, nil)
	_, err := p.History(context.Background(), market.Instrument{
		Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
	}, market.HistoryParams{Interval: "1w"})
	if market.KindOf(err) != market.ErrUnsupported {
		t.Fatalf("kind = %q", market.KindOf(err))
	}
}
