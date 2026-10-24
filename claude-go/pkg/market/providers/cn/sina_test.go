package cn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// sinaQuoteParts 构造 33 段新浪报价字段 (含日期/时间)。
func sinaQuoteParts(last, prev, open, volume, turnover string) []string {
	parts := make([]string, 33)
	parts[0] = "贵州茅台"
	parts[1] = open
	parts[2] = prev
	parts[3] = last
	parts[4] = "1710.00"
	parts[5] = "1680.00"
	parts[8] = volume
	parts[9] = turnover
	parts[30] = "2025-06-30"
	parts[31] = "15:10:00"
	return parts
}

// TestSinaQuote 验证 Referer、GBK 转码与逗号字段映射。
func TestSinaQuote(t *testing.T) {
	var gotPath, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotReferer = r.Header.Get("Referer")
		body := `var hq_str_sh600519="` + strings.Join(sinaQuoteParts("1700.00", "1690.00", "1695.00", "1234567", "2100000000.00"), ",") + `";`
		w.Header().Set("Content-Type", "text/plain; charset=GBK")
		_, _ = w.Write(gbkEncode(t, body))
	}))
	defer srv.Close()

	p := newSina(market.DefaultConfig(), srv.Client())
	p.baseURL = srv.URL
	inst := mustInstrument(t, "600519")

	q, err := p.Quote(context.Background(), inst)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if gotPath != "/list=sh600519" {
		t.Errorf("path = %s, want /list=sh600519", gotPath)
	}
	if gotReferer != "https://finance.sina.com.cn" {
		t.Errorf("Referer = %q, want https://finance.sina.com.cn", gotReferer)
	}
	if q.Inst.Name != "贵州茅台" {
		t.Errorf("GBK 名称解码失败: %q", q.Inst.Name)
	}
	if q.Last != 1700 || q.PrevClose != 1690 || q.Open != 1695 || q.High != 1710 || q.Low != 1680 {
		t.Errorf("字段映射错误: %+v", q)
	}
	if q.Volume != 1234567 {
		t.Errorf("Volume = %d, want 1234567 (股)", q.Volume)
	}
	if q.Turnover != 2.1e9 {
		t.Errorf("Turnover = %v, want 2.1e9", q.Turnover)
	}
	wantTime := time.Date(2025, 6, 30, 15, 10, 0, 0, mustLocation(t, "Asia/Shanghai"))
	if !q.AsOf.Equal(wantTime) {
		t.Errorf("AsOf = %s, want %s", q.AsOf, wantTime)
	}
	if q.Session != market.SessionPost {
		t.Errorf("Session = %s, want post", q.Session)
	}
}

// TestSinaQuoteHaltedHeuristic 验证无量且现价==昨收判为 halted。
func TestSinaQuoteHaltedHeuristic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `var hq_str_sh600519="` + strings.Join(sinaQuoteParts("1700.00", "1700.00", "1700.00", "0", "0.00"), ",") + `";`
		_, _ = w.Write(gbkEncode(t, body))
	}))
	defer srv.Close()
	p := newSina(market.DefaultConfig(), srv.Client())
	p.baseURL = srv.URL
	q, err := p.Quote(context.Background(), mustInstrument(t, "600519"))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Session != market.SessionHalted || q.Tradable {
		t.Errorf("Session/Tradable = %s/%v, want halted/false", q.Session, q.Tradable)
	}
}

// TestSinaUnsupportedMarket 验证新浪只覆盖 A 股。
func TestSinaUnsupportedMarket(t *testing.T) {
	p := newSina(market.DefaultConfig(), http.DefaultClient)
	if _, err := p.Quote(context.Background(), mustInstrument(t, "00700")); err == nil {
		t.Fatal("港股应返回 ErrUnsupported")
	} else if market.KindOf(err) != market.ErrUnsupported {
		t.Fatalf("KindOf = %s, want unsupported", market.KindOf(err))
	}
}

// TestSinaEgressAndCaps 验证出口与能力声明。
func TestSinaEgressAndCaps(t *testing.T) {
	p := newSina(market.DefaultConfig(), http.DefaultClient)
	if p.EgressMode() != "direct" {
		t.Errorf("EgressMode = %s, want direct", p.EgressMode())
	}
	caps := p.Capabilities()
	if !caps.Has(market.CapQuote) || caps.Has(market.CapHistory) {
		t.Errorf("Capabilities = %v, want 仅 quote", caps)
	}
}
