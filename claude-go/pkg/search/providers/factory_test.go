package providers

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
)

// TestDefaultFactory 覆盖启用过滤、构造失败跳过与无 key 付费源跳过。
func TestDefaultFactory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := search.DefaultConfig()
	cfg.Providers = map[string]search.ProviderConfig{
		"parallel":  {Enabled: true, BaseURL: srv.URL + "/mcp"},
		"exa_anon":  {Enabled: false, BaseURL: srv.URL + "/mcp2"},
		"exa":       {Enabled: true}, // 付费源且无 key, 不应构造
		"tavily":    {Enabled: true, APIKey: "k"},
		"bing_rss":  {Enabled: true, BaseURL: "not-a-url"}, // 构造失败, 跳过
		"github":    {Enabled: true, BaseURL: srv.URL + "/github"},
		"pypi":      {Enabled: true, Disabled: true},
		"unknownxx": {Enabled: true},
	}

	got := Default(cfg, nil)
	names := make([]string, 0, len(got))
	for _, p := range got {
		names = append(names, p.Name())
	}
	sort.Strings(names)
	want := []string{"github", "parallel"}
	if len(names) != len(want) {
		t.Fatalf("want %v, got %v", want, names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("want %v, got %v", want, names)
		}
	}
}

// TestProviderDefaults 覆盖各源的默认出口、层级与权重。
func TestProviderDefaults(t *testing.T) {
	cfg := search.DefaultConfig()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	cases := []struct {
		name   string
		ctor   providerCtor
		mode   search.Mode
		tier   search.Tier
		weight float64
	}{
		{"parallel", newParallel, search.ModeDirect, search.TierL0, 1.0},
		{"exa_anon", newExa, search.ModeDirect, search.TierL0, 0.9},
		{"bing_rss", newBingRSS, search.ModeDirect, search.TierL2, 0.6},
		{"wikipedia", newWikipedia, search.ModeProxy, search.TierL2, 0.8},
		{"gnews", newGNews, search.ModeProxy, search.TierL2, 0.7},
		{"github", newGitHub, search.ModeDirect, search.TierL2, 1.0},
		{"stackexchange", newStackExchange, search.ModeDirect, search.TierL2, 1.0},
		{"openalex", newOpenAlex, search.ModeDirect, search.TierL2, 1.0},
		{"crossref", newCrossref, search.ModeDirect, search.TierL2, 1.0},
		{"pubmed", newPubMed, search.ModeDirect, search.TierL2, 1.0},
		{"hn", newHN, search.ModeDirect, search.TierL2, 0.7},
		{"npm", newNPM, search.ModeDirect, search.TierL2, 1.0},
		{"pypi", newPyPI, search.ModeDirect, search.TierL2, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.ctor(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			if p.Name() != tc.name {
				t.Errorf("name: want %s, got %s", tc.name, p.Name())
			}
			if got := p.Tier(); got != tc.tier {
				t.Errorf("tier: want %v, got %v", tc.tier, got)
			}
			ep, ok := p.(search.EgressPinned)
			if !ok || ep.EgressMode() != tc.mode {
				t.Errorf("egress: want %s", tc.mode)
			}
			w, ok := p.(search.Weighted)
			if !ok || w.Weight() != tc.weight {
				t.Errorf("weight: want %v", tc.weight)
			}
			if got := p.Kinds(); got != search.KindSet(search.KindSearch) {
				t.Errorf("kinds: want only search, got %v", got)
			}
		})
	}
}

// TestConfigOverridesProvider 覆盖配置对出口、层级与权重的覆盖。
func TestConfigOverridesProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	p, err := newParallel(search.DefaultConfig(), search.ProviderConfig{
		Enabled: true, BaseURL: srv.URL,
		Egress: search.ModeProxy, Tier: search.TierL1, Weight: 2.5,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tier() != search.TierL1 {
		t.Errorf("tier override ignored: %v", p.Tier())
	}
	if p.(search.EgressPinned).EgressMode() != search.ModeProxy {
		t.Errorf("egress override ignored")
	}
	if p.(search.Weighted).Weight() != 2.5 {
		t.Errorf("weight override ignored")
	}
}
