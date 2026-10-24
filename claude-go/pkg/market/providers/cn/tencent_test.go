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
	"golang.org/x/text/encoding/simplifiedchinese"
)

// gbkEncode 把测试字符串编码成 GBK, 模拟腾讯/新浪的真实响应。
func gbkEncode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("GBK encode: %v", err)
	}
	return b
}

// tencentQuoteParts 构造 35 段腾讯报价字段。
func tencentQuoteParts(last, prev, open, volume, high, low string) []string {
	parts := make([]string, 35)
	parts[0] = "1"
	parts[1] = "贵州茅台"
	parts[2] = "600519"
	parts[3] = last
	parts[4] = prev
	parts[5] = open
	parts[6] = volume
	parts[30] = "2025-06-30 15:10:00"
	parts[33] = high
	parts[34] = low
	return parts
}

// TestTencentQuote 验证 GBK 转码与 "~" 字段映射。
func TestTencentQuote(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body := `v_sh600519="` + strings.Join(tencentQuoteParts("1700.00", "1690.00", "1695.00", "12345", "1710.00", "1680.00"), "~") + `";`
		w.Header().Set("Content-Type", "text/plain; charset=GBK")
		_, _ = w.Write(gbkEncode(t, body))
	}))
	defer srv.Close()

	p := newTencent(market.DefaultConfig(), srv.Client())
	p.baseURL = srv.URL
	inst := mustInstrument(t, "600519")

	q, err := p.Quote(context.Background(), inst)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if gotPath != "/q=sh600519" {
		t.Errorf("path = %s, want /q=sh600519", gotPath)
	}
	if q.Inst.Name != "贵州茅台" {
		t.Errorf("GBK 名称解码失败: %q", q.Inst.Name)
	}
	if q.Last != 1700 || q.PrevClose != 1690 || q.Open != 1695 || q.High != 1710 || q.Low != 1680 {
		t.Errorf("字段映射错误: %+v", q)
	}
	if q.Volume != 12345 {
		t.Errorf("Volume = %d, want 12345", q.Volume)
	}
	wantTime := time.Date(2025, 6, 30, 15, 10, 0, 0, mustLocation(t, "Asia/Shanghai"))
	if !q.AsOf.Equal(wantTime) {
		t.Errorf("AsOf = %s, want %s", q.AsOf, wantTime)
	}
	if q.Session != market.SessionPost || q.Tradable {
		t.Errorf("Session/Tradable = %s/%v, want post/false (15:00)", q.Session, q.Tradable)
	}
	if q.Source != "tencent" {
		t.Errorf("Source = %s", q.Source)
	}
}

// TestTencentQuoteHaltedHeuristic 验证无量且现价==昨收时判为 halted。
func TestTencentQuoteHaltedHeuristic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `v_sh600519="` + strings.Join(tencentQuoteParts("1700.00", "1700.00", "1700.00", "0", "1700.00", "1700.00"), "~") + `";`
		_, _ = w.Write(gbkEncode(t, body))
	}))
	defer srv.Close()
	p := newTencent(market.DefaultConfig(), srv.Client())
	p.baseURL = srv.URL
	q, err := p.Quote(context.Background(), mustInstrument(t, "600519"))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Session != market.SessionHalted || q.Tradable {
		t.Errorf("Session/Tradable = %s/%v, want halted/false", q.Session, q.Tradable)
	}
	if q.Last != 1700 || q.Volume != 0 {
		t.Errorf("启发式不得改写原值: last=%v volume=%d", q.Last, q.Volume)
	}
}

// TestTencentQuoteShortPayload 验证字段不足时不 panic。
func TestTencentQuoteShortPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只有 7 段: 高/低/时间缺失。
		body := `v_sh600519="1~贵州茅台~600519~1700.00~1690.00~1695.00~12345";`
		_, _ = w.Write(gbkEncode(t, body))
	}))
	defer srv.Close()
	p := newTencent(market.DefaultConfig(), srv.Client())
	p.baseURL = srv.URL
	q, err := p.Quote(context.Background(), mustInstrument(t, "600519"))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.High != 0 || q.Low != 0 {
		t.Errorf("缺失高/低应为 0, 实际 %v/%v", q.High, q.Low)
	}
	if q.AsOf.IsZero() {
		t.Error("时间缺失时应退回当前时间")
	}
}

// TestTencentQuoteHK 验证港股前缀与 5 位代码。
func TestTencentQuoteHK(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		parts := tencentQuoteParts("400.00", "398.00", "399.00", "100", "402.00", "397.00")
		parts[1] = "腾讯控股"
		body := `v_hk00700="` + strings.Join(parts, "~") + `";`
		_, _ = w.Write(gbkEncode(t, body))
	}))
	defer srv.Close()
	p := newTencent(market.DefaultConfig(), srv.Client())
	p.baseURL = srv.URL
	q, err := p.Quote(context.Background(), mustInstrument(t, "00700"))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if gotPath != "/q=hk00700" {
		t.Errorf("path = %s, want /q=hk00700", gotPath)
	}
	if q.Inst.Name != "腾讯控股" || q.Inst.Market != market.MarketHK {
		t.Errorf("港股标的错误: %+v", q.Inst)
	}
}

// TestTencentHistory 验证备源 K 线的 JSON 字段顺序与区间过滤。
func TestTencentHistory(t *testing.T) {
	var gotParam string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/appstock/app/fqkline/get" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotParam = r.URL.Query().Get("param")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"sh600519": map[string]any{
					"qfqday": [][]string{
						{"2025-06-30", "1695.00", "1700.00", "1710.00", "1680.00", "12345"},
						{"2025-07-01", "1700.00", "1712.00", "1720.00", "1690.00", "22345"},
					},
				},
			},
		})
	}))
	defer srv.Close()
	p := newTencent(market.DefaultConfig(), srv.Client())
	p.baseURL = srv.URL
	p.historyBase = srv.URL

	to := time.Date(2025, 6, 30, 0, 0, 0, 0, mustLocation(t, "Asia/Shanghai"))
	bars, err := p.History(context.Background(), mustInstrument(t, "600519"), market.HistoryParams{
		To: to, Interval: "1d", Adjust: market.AdjustQFQ,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if !strings.HasPrefix(gotParam, "sh600519,day,") || !strings.HasSuffix(gotParam, ",qfq") {
		t.Errorf("param = %s", gotParam)
	}
	if len(bars) != 1 {
		t.Fatalf("bars = %d, want 1 (07-01 被区间过滤)", len(bars))
	}
	b := bars[0]
	if b.Open != 1695 || b.Close != 1700 || b.High != 1710 || b.Low != 1680 || b.Volume != 12345 {
		t.Errorf("字段映射错误: %+v", b)
	}
	if b.Adjust != market.AdjustQFQ {
		t.Errorf("Adjust = %s", b.Adjust)
	}
}

// TestTencentEgressAndCaps 验证出口与能力声明。
func TestTencentEgressAndCaps(t *testing.T) {
	p := newTencent(market.DefaultConfig(), http.DefaultClient)
	if p.EgressMode() != "direct" {
		t.Errorf("EgressMode = %s, want direct", p.EgressMode())
	}
	caps := p.Capabilities()
	if !caps.Has(market.CapQuote) || !caps.Has(market.CapHistory) {
		t.Errorf("Capabilities = %v", caps)
	}
}

// mustLocation 加载测试时区。
func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%s): %v", name, err)
	}
	return loc
}
