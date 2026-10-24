package global

import (
	"context"
	"net/http"
	"testing"

	"github.com/anthropic/claude-go/pkg/market"
)

// TestProvidersFactory 验证源集合、顺序、出口声明与 fred 无 key 跳过。
func TestProvidersFactory(t *testing.T) {
	t.Setenv("FRED_API_KEY", "")
	cfg := market.DefaultConfig()
	cfg.Providers["fred"] = market.ProviderConfig{
		Enabled: true, BaseURL: "https://api.stlouisfed.org",
	}

	modes := map[string]int{}
	ps := Providers(cfg, func(mode string) *http.Client {
		modes[mode]++
		return http.DefaultClient
	})

	wantOrder := []string{"stooq", "yahoo", "sec", "binance", "coingecko", "worldbank"}
	if len(ps) != len(wantOrder) {
		t.Fatalf("providers = %d, want %d (fred 无 key 应跳过)", len(ps), len(wantOrder))
	}
	for i, want := range wantOrder {
		if ps[i].Name() != want {
			t.Fatalf("providers[%d] = %q, want %q", i, ps[i].Name(), want)
		}
	}
	for _, p := range ps {
		if p.Name() == "fred" {
			t.Fatal("fred 无 key 时不应出现在工厂结果里")
		}
		ep, ok := p.(market.EgressPinner)
		if !ok {
			t.Fatalf("%s 未实现 EgressPinner", p.Name())
		}
		if ep.EgressMode() == "" {
			t.Fatalf("%s 出口为空", p.Name())
		}
		if len(p.Markets()) == 0 {
			t.Fatalf("%s 未声明市场", p.Name())
		}
	}
	if modes["direct"] == 0 || modes["proxy"] == 0 {
		t.Fatalf("出口客户端未被按 mode 请求: %v", modes)
	}

	// 有 key 时 fred 加入。
	cfg.Providers["fred"] = market.ProviderConfig{
		Enabled: true, BaseURL: "https://api.stlouisfed.org", APIKey: "k",
	}
	ps = Providers(cfg, func(string) *http.Client { return http.DefaultClient })
	var fred market.Provider
	for _, p := range ps {
		if p.Name() == "fred" {
			fred = p
		}
	}
	if fred == nil {
		t.Fatal("有 key 时 fred 应加入")
	}
	if got := fred.(market.EgressPinner).EgressMode(); got != "direct" {
		t.Fatalf("fred egress = %q", got)
	}

	// Enabled=false 的源被跳过。
	cfg.Providers["yahoo"] = market.ProviderConfig{Enabled: false}
	ps = Providers(cfg, func(string) *http.Client { return http.DefaultClient })
	for _, p := range ps {
		if p.Name() == "yahoo" {
			t.Fatal("Enabled=false 的 yahoo 不应出现")
		}
	}
}

// TestProviderCapabilities 验证能力矩阵: 只声明真实具备的能力。
func TestProviderCapabilities(t *testing.T) {
	t.Setenv("FRED_API_KEY", "")
	cfg := market.DefaultConfig()
	ps := Providers(cfg, func(string) *http.Client { return http.DefaultClient })
	byName := map[string]market.Provider{}
	for _, p := range ps {
		byName[p.Name()] = p
	}

	want := map[string][]market.Capability{
		"stooq":     {market.CapQuote, market.CapHistory},
		"yahoo":     {market.CapQuote, market.CapHistory, market.CapNews},
		"sec":       {market.CapFiling},
		"binance":   {market.CapQuote, market.CapHistory},
		"coingecko": {market.CapQuote, market.CapHistory},
		"worldbank": {market.CapMacro},
	}
	for name, caps := range want {
		p, ok := byName[name]
		if !ok {
			t.Fatalf("缺少源 %s", name)
		}
		got := p.Capabilities()
		if len(got) != len(caps) {
			t.Fatalf("%s capabilities = %v, want %v", name, got, caps)
		}
		for _, c := range caps {
			if !got.Has(c) {
				t.Fatalf("%s 缺少能力 %s", name, c)
			}
		}
	}

	// SEC 的 Fundamentals 方法存在但能力未声明, 调用必须返回 ErrUnsupported。
	fp, ok := byName["sec"].(market.FundamentalsProvider)
	if !ok {
		t.Fatal("sec 的 Fundamentals 方法应存在")
	}
	if _, err := fp.Fundamentals(context.Background(), market.Instrument{
		Canonical: "US:AAPL", Market: market.MarketUS, Code: "AAPL",
	}, "2024Q1"); market.KindOf(err) != market.ErrUnsupported {
		t.Fatalf("sec Fundamentals kind = %q", market.KindOf(err))
	}
}

// TestProvidersNilClientFunc 验证接线层传 nil 时不 panic。
func TestProvidersNilClientFunc(t *testing.T) {
	t.Setenv("FRED_API_KEY", "")
	ps := Providers(market.DefaultConfig(), nil)
	if len(ps) == 0 {
		t.Fatal("nil client func 应回退默认客户端")
	}
}
