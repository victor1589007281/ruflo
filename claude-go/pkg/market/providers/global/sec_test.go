package global

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

const secTickersJSON = `{
	"0":{"cik_str":320193,"ticker":"AAPL","title":"Apple Inc."},
	"1":{"cik_str":1067983,"ticker":"BRK-B","title":"Berkshire Hathaway Inc."}
}`

const secSubmissionsJSON = `{
	"name":"Apple Inc.",
	"filings":{"recent":{
		"accessionNumber":["0000320193-24-000001","0000320193-24-000002","0000320193-24-000003"],
		"filingDate":["2024-02-01","2024-01-15","2024-01-10"],
		"reportDate":["2023-12-30","2024-01-15","2024-01-10"],
		"form":["10-K","8-K","DEF 14A"],
		"primaryDocument":["aapl-20231230.htm","doc8k.htm","proxy.htm"],
		"primaryDocDescription":["Annual report","Current report","Proxy statement"]
	}}
}`

// TestSECFilings 验证 ticker→CIK、submissions 解析、UA 与 since 过滤。
func TestSECFilings(t *testing.T) {
	var mu sync.Mutex
	tickerHits := 0
	var tickerUA, submissionsUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/files/company_tickers.json":
			tickerHits++
			tickerUA = r.Header.Get("User-Agent")
			io.WriteString(w, secTickersJSON)
		case "/submissions/CIK0000320193.json":
			submissionsUA = r.Header.Get("User-Agent")
			io.WriteString(w, secSubmissionsJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	pc := market.ProviderConfig{
		Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second,
		UserAgent: "claude-go-test sec@example.com", QPS: 10000,
	}
	p := newSEC(pc, market.DefaultConfig(), srv.Client())
	if p.wwwBase != srv.URL {
		t.Fatalf("wwwBase = %q, want %q", p.wwwBase, srv.URL)
	}

	since := time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)
	filings, err := p.Filings(context.Background(), market.Instrument{
		Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
	}, since)
	if err != nil {
		t.Fatalf("Filings: %v", err)
	}
	if len(filings) != 1 {
		t.Fatalf("filings = %d, want 1 (since 过滤)", len(filings))
	}
	f := filings[0]
	if f.ID != "0000320193-24-000001" || f.Type != "10-K" {
		t.Fatalf("filing = %+v", f)
	}
	if f.Title != "10-K Annual report" {
		t.Fatalf("title = %q", f.Title)
	}
	wantURL := srv.URL + "/Archives/edgar/data/320193/000032019324000001/aapl-20231230.htm"
	if f.URL != wantURL {
		t.Fatalf("URL = %q, want %q", f.URL, wantURL)
	}
	if !f.Published.Equal(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Published = %v", f.Published)
	}
	if f.Source != "sec" || f.Symbol != "US:AAPL" {
		t.Fatalf("source/symbol = %q/%q", f.Source, f.Symbol)
	}
	if tickerUA != pc.UserAgent || submissionsUA != pc.UserAgent {
		t.Fatalf("UA = %q / %q, want %q", tickerUA, submissionsUA, pc.UserAgent)
	}

	// 第二次调用应命中 24h 进程内缓存, 不再抓 company_tickers.json。
	if _, err := p.Filings(context.Background(), market.Instrument{
		Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
	}, since); err != nil {
		t.Fatalf("Filings second: %v", err)
	}
	mu.Lock()
	hits := tickerHits
	mu.Unlock()
	if hits != 1 {
		t.Fatalf("ticker hits = %d, want 1 (24h 缓存)", hits)
	}

	// BRK.B 应归一为 SEC 使用的 BRK-B。
	cik, err := p.cikFor(context.Background(), normalizeTicker("BRK.B"))
	if err != nil || cik != "0001067983" {
		t.Fatalf("BRK.B cik = %q, err = %v", cik, err)
	}
}

// TestSECCapabilities 验证财务能力未声明且调用返回 ErrUnsupported。
func TestSECCapabilities(t *testing.T) {
	pc := market.ProviderConfig{Enabled: true, BaseURL: "https://data.sec.gov", QPS: 10000}
	p := newSEC(pc, market.DefaultConfig(), nil)
	caps := p.Capabilities()
	if !caps.Has(market.CapFiling) {
		t.Fatal("SEC 应声明 filing")
	}
	if caps.Has(market.CapFundamental) {
		t.Fatal("SEC 不应声明 fundamental")
	}
	_, err := p.Fundamentals(context.Background(), market.Instrument{
		Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
	}, "2024Q1")
	if market.KindOf(err) != market.ErrUnsupported {
		t.Fatalf("kind = %q", market.KindOf(err))
	}
}

// TestSECRateLimitWait 验证限速器会按 QPS 拉开两次请求的间隔。
func TestSECRateLimitWait(t *testing.T) {
	p := newSEC(market.ProviderConfig{Enabled: true, QPS: 20}, market.DefaultConfig(), nil)
	ctx := context.Background()
	start := time.Now()
	if err := p.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("两次请求间隔 = %v, 限速未生效", elapsed)
	}
}

// TestSECUserAgentSources 验证 UA 选择顺序: provider > SEC_USER_AGENT > 全局。
func TestSECUserAgentSources(t *testing.T) {
	t.Setenv("SEC_USER_AGENT", "env-agent env@example.com")
	got := secUserAgent(market.ProviderConfig{}, market.Config{UserAgent: "global-agent"})
	if got != "env-agent env@example.com" {
		t.Fatalf("env UA = %q", got)
	}
	got = secUserAgent(market.ProviderConfig{UserAgent: "explicit-agent"}, market.Config{UserAgent: "global-agent"})
	if got != "explicit-agent" {
		t.Fatalf("explicit UA = %q", got)
	}
	t.Setenv("SEC_USER_AGENT", "")
	got = secUserAgent(market.ProviderConfig{}, market.Config{UserAgent: "global-agent"})
	if got != "global-agent" {
		t.Fatalf("global UA = %q", got)
	}
}

// TestNormalizeTicker 验证美股代码归一。
func TestNormalizeTicker(t *testing.T) {
	if got := normalizeTicker(" brk.b "); got != "BRK-B" {
		t.Fatalf("normalizeTicker = %q", got)
	}
}
