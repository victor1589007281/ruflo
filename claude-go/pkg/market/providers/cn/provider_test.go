package cn

import (
	"net/http"
	"testing"

	"github.com/anthropic/claude-go/pkg/market"
)

// TestProvidersFactory 验证工厂顺序、出口模式与能力声明。
func TestProvidersFactory(t *testing.T) {
	called := map[string]int{}
	client := func(mode string) *http.Client {
		called[mode]++
		return http.DefaultClient
	}
	got := Providers(market.DefaultConfig(), client)
	if len(got) != 3 {
		t.Fatalf("Providers = %d, want 3", len(got))
	}
	wantNames := []string{"tencent", "sina", "eastmoney"}
	for i, want := range wantNames {
		if got[i].Name() != want {
			t.Errorf("Providers[%d] = %s, want %s", i, got[i].Name(), want)
		}
	}
	// 腾讯/新浪直连, 东财 auto + 资讯专用直连。
	if called["direct"] != 3 || called["auto"] != 1 {
		t.Errorf("client modes = %v, want direct*3 + auto*1", called)
	}
	// 腾讯/新浪直连, 东财 auto。
	if pin, ok := got[0].(market.EgressPinner); !ok || pin.EgressMode() != "direct" {
		t.Errorf("tencent egress 错误")
	}
	if pin, ok := got[1].(market.EgressPinner); !ok || pin.EgressMode() != "direct" {
		t.Errorf("sina egress 错误")
	}
	if pin, ok := got[2].(market.EgressPinner); !ok || pin.EgressMode() != "auto" {
		t.Errorf("eastmoney egress 错误")
	}
	if !got[2].Capabilities().Has(market.CapHistory) ||
		!got[2].Capabilities().Has(market.CapFundamental) {
		t.Errorf("eastmoney 能力声明缺失: %v", got[2].Capabilities())
	}
}

// TestProvidersRespectDisabled 验证 cfg.Providers[x].Enabled=false 时跳过。
func TestProvidersRespectDisabled(t *testing.T) {
	cfg := market.DefaultConfig()
	pc := cfg.Providers["tencent"]
	pc.Enabled = false
	cfg.Providers["tencent"] = pc
	got := Providers(cfg, func(string) *http.Client { return http.DefaultClient })
	if len(got) != 2 {
		t.Fatalf("Providers = %d, want 2", len(got))
	}
	for _, p := range got {
		if p.Name() == "tencent" {
			t.Fatal("tencent 已被禁用, 不应注册")
		}
	}
}

// TestProvidersNilClientFallback 验证 client 工厂返回 nil 时使用默认客户端。
func TestProvidersNilClientFallback(t *testing.T) {
	got := Providers(market.DefaultConfig(), func(string) *http.Client { return nil })
	if len(got) != 3 {
		t.Fatalf("Providers = %d, want 3", len(got))
	}
	for _, p := range got {
		if !p.Healthy() {
			t.Errorf("%s 应使用默认客户端并健康", p.Name())
		}
	}
}
