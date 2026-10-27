package search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSearchEnforcesDomainsLimitAndCacheIdentity(t *testing.T) {
	var seen SearchRequest
	p := newFake("p", TierL0, func(_ context.Context, req SearchRequest) ([]Result, error) {
		seen = req
		return []Result{
			{URL: "https://docs.example.com/a", Title: "allowed first", Snippet: "s"},
			{URL: "https://other.example/b", Title: "blocked", Snippet: "s"},
			{URL: "https://sub.docs.example.com/c", Title: "allowed second", Snippet: "s"},
		}, nil
	})
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{p})
	defer m.Close()

	resp, err := m.Search(context.Background(), SearchRequest{
		Query: "contract", Limit: 1, AllowedDomains: []string{"docs.example.com"},
		BlockedDomains: []string{"other.example"}, OriginalQuestion: "original",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://docs.example.com/a" {
		t.Fatalf("domain filter/top-k not enforced: %+v", resp.Results)
	}
	if seen.OriginalQuestion != "original" || len(seen.BlockedDomains) != 1 {
		t.Fatalf("provider request lost fields: %+v", seen)
	}
	if _, err = m.Search(context.Background(), SearchRequest{Query: "contract", Limit: 2}); err != nil {
		t.Fatal(err)
	}
	if p.callCount() != 2 {
		t.Fatalf("limit/domain fields must partition cache, calls=%d", p.callCount())
	}
}

func TestSearchTrackingParametersDoNotCreateDuplicateResults(t *testing.T) {
	p := newFake("p", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{
			{URL: "https://arxiv.org/abs/1706.03762", Title: "paper", Snippet: "text", Rank: 1},
			{URL: "https://arxiv.org/abs/1706.03762?trk=article-ssr-frontend-pulse_little-text-block&utm_source=feed", Title: "duplicate", Snippet: "text", Rank: 2},
			{URL: "https://arxiv.org/abs/1706.03762?file=1706.03762&spm=a2c6h.publish-article", Title: "duplicate file", Snippet: "text", Rank: 3},
			{URL: "https://arxiv.org/abs/1706.03762?download=true", Title: "distinct download", Snippet: "text", Rank: 4},
		}, nil
	})
	m := NewManager(DefaultConfig(), testEgress(t), []Provider{p})
	defer m.Close()
	resp, err := m.Search(context.Background(), SearchRequest{Query: "paper"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 || resp.Results[0].URL != "https://arxiv.org/abs/1706.03762" {
		t.Fatalf("click tracking created duplicate results: %+v", resp.Results)
	}
	if CanonicalURL("https://example.com/read?fbclid=click&spm=feed&id=42") != "https://example.com/read?id=42" {
		t.Fatal("tracking key was retained or business key was discarded")
	}
	if got := CanonicalURL("https://arxiv.org/abs/1706.03762?file=other&spm=feed"); got != "https://arxiv.org/abs/1706.03762?file=other" {
		t.Fatalf("non-redundant arXiv file parameter was changed: %q", got)
	}
}

func TestSearchDomainFilterNeverFallsBack(t *testing.T) {
	p := newFake("p", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://outside.example/a", Title: "outside", Snippet: "s"}}, nil
	})
	m := NewManager(DefaultConfig(), testEgress(t), []Provider{p})
	defer m.Close()
	resp, err := m.Search(context.Background(), SearchRequest{Query: "contract", AllowedDomains: []string{"only.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("returned unfiltered fallback: %+v", resp.Results)
	}
	if resp.GateReason != "domain_filter" {
		t.Fatalf("gate reason=%q", resp.GateReason)
	}
}

func TestSearchRejectsInvalidParameters(t *testing.T) {
	p := newFake("p", TierL0, nil)
	m := NewManager(DefaultConfig(), testEgress(t), []Provider{p})
	defer m.Close()
	for _, req := range []SearchRequest{
		{Query: "x", Limit: -1},
		{Query: "x", Limit: 1000},
		{Query: "x", Freshness: "fortnight"},
		{Query: "x", AllowedDomains: []string{"https://example.com/path"}},
		{Query: "x", BlockedDomains: []string{"bad domain"}},
	} {
		if _, err := m.Search(context.Background(), req); KindOf(err) != ErrInvalid {
			t.Errorf("request %+v: error=%v", req, err)
		}
	}
	if p.callCount() != 0 {
		t.Fatal("invalid input must not reach providers")
	}
}

func TestSearchQualityFailureIsExplicitWithoutHiddenExtraSources(t *testing.T) {
	newProvider := func(name string, results []Result) *fakeProvider {
		return newFake(name, TierL0, func(context.Context, SearchRequest) ([]Result, error) {
			return results, nil
		})
	}
	a := newProvider("a", nil)
	b := newProvider("b", nil)
	c := newProvider("c", []Result{{URL: "https://three.example/c", Title: "three", Snippet: "evidence"}})
	d := newProvider("d", nil)
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{a, b, c, d})
	defer m.Close()
	resp, err := m.Search(context.Background(), SearchRequest{Query: "recovery"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "empty" || resp.GateReason != "empty" || c.callCount() != 0 || d.callCount() != 0 {
		t.Fatalf("quality failure was hidden or exceeded source budget: %+v calls=%d/%d", resp, c.callCount(), d.callCount())
	}
}

func TestSearchOriginalQuestionReranksResults(t *testing.T) {
	p := newFake("p", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{
			{URL: "https://example.com/generic", Title: "generic", Snippet: "focused"},
			{URL: "https://example.com/mars", Title: "unique mars", Snippet: "focused unique mars evidence"},
		}, nil
	})
	m := NewManager(DefaultConfig(), testEgress(t), []Provider{p})
	defer m.Close()
	resp, err := m.Search(context.Background(), SearchRequest{
		Query: "focused", OriginalQuestion: "unique mars",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 || resp.Results[0].URL != "https://example.com/mars" || resp.Results[0].Rank != 1 {
		t.Fatalf("original question did not affect ranking: %+v", resp.Results)
	}
}

func TestSearchDomainOverfetchReranksSpecificOfficialPage(t *testing.T) {
	providerLimit := 0
	p := newFake("p", TierL0, func(_ context.Context, req SearchRequest) ([]Result, error) {
		providerLimit = req.Limit
		home := Result{
			URL: "https://kubernetes.io/docs/home", Title: "Kubernetes Documentation",
			Snippet: "Official Kubernetes documentation.", Rank: 1,
		}
		if req.Limit == 1 {
			return []Result{home}, nil
		}
		return []Result{home, {
			URL: "https://kubernetes.io/docs/concepts/architecture/controller", Title: "Controllers",
			Snippet: "In Kubernetes, a controller runs a reconciliation loop.", Rank: 2,
		}}, nil
	})
	m := NewManager(DefaultConfig(), testEgress(t), []Provider{p})
	defer m.Close()
	resp, err := m.Search(context.Background(), SearchRequest{
		Query:            "Kubernetes controller reconciliation official documentation",
		OriginalQuestion: "查找 Kubernetes 官方控制器与协调循环文档",
		AllowedDomains:   []string{"kubernetes.io"}, Limit: 1, Language: "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	if providerLimit <= 1 {
		t.Fatalf("domain-scoped limit=1 was not overfetched: provider limit=%d", providerLimit)
	}
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://kubernetes.io/docs/concepts/architecture/controller" {
		t.Fatalf("specific official page must win and final limit must remain one: %+v", resp.Results)
	}
}

func TestSearchPlannerFreshnessReachesProvider(t *testing.T) {
	var seen []SearchRequest
	p := newFake("p", TierL0, func(_ context.Context, req SearchRequest) ([]Result, error) {
		seen = append(seen, req)
		return []Result{{URL: "https://example.com/news", Title: "新闻", Snippet: "新闻"}}, nil
	})
	m := NewManager(DefaultConfig(), testEgress(t), []Provider{p})
	defer m.Close()
	if _, err := m.Search(context.Background(), SearchRequest{Query: "今天有什么新闻"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Search(context.Background(), SearchRequest{Query: "今天有什么新闻", Freshness: "day"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0].Freshness != "week" || seen[0].Language != "zh" || seen[1].Freshness != "day" {
		t.Fatalf("planner semantics lost before provider: %+v", seen)
	}
}

func TestSearchDomainScopeDisablesOnlyDiversityGate(t *testing.T) {
	quality := EvaluateQuality([]Result{{URL: "https://docs.example.com/a", Title: "guide", Snippet: "text"}}, SearchRequest{
		AllowedDomains: []string{"docs.example.com"},
	})
	if !quality.Pass(false) || quality.gateReason(false) != "pass" {
		t.Fatalf("explicit domain scope must not fail diversity gate: %+v", quality)
	}
	quality.HasSnippet = false
	if quality.Pass(false) || quality.gateReason(false) != "snippet" {
		t.Fatalf("domain scope accidentally disabled other quality gates: %+v", quality)
	}
}

func TestSearchJSONHasStableLowercaseContract(t *testing.T) {
	p := newFake("p", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://example.com/a", Title: "A", Snippet: "text"}}, nil
	})
	m := NewManager(DefaultConfig(), testEgress(t), []Provider{p})
	defer m.Close()
	out, err := NewLegacySearcher(m).SearchRequestJSON(context.Background(), SearchRequest{Query: "json"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"results"`, `"url"`, `"title"`, `"snippet"`, `"gate_passed"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	for _, unwanted := range []string{`"Results"`, `"URL"`, `"Title"`, `"GatePassed"`} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unstable field %s in %s", unwanted, out)
		}
	}
}

func TestSearchEmptyResultsStayJSONArrayAcrossCache(t *testing.T) {
	p := newFake("p", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://outside.example/a", Title: "outside", Snippet: "text"}}, nil
	})
	m := NewManager(DefaultConfig(), testEgress(t), []Provider{p})
	defer m.Close()
	legacy := NewLegacySearcher(m)
	req := SearchRequest{Query: "empty array", AllowedDomains: []string{"only.example"}}
	for call := 0; call < 2; call++ {
		out, err := legacy.SearchRequestJSON(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Results  []Result `json:"results"`
			CacheHit bool     `json:"cache_hit"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Results == nil || len(payload.Results) != 0 {
			t.Fatalf("call %d results must be [], got %s", call+1, out)
		}
		if payload.CacheHit != (call == 1) {
			t.Fatalf("call %d cache_hit=%v, response=%s", call+1, payload.CacheHit, out)
		}
	}
	if p.callCount() != 1 {
		t.Fatalf("provider calls=%d, want one cached search", p.callCount())
	}
}

func TestSearchStatusUsesOnlyScheduledProviderFailures(t *testing.T) {
	result := func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://example.com/a", Title: "A", Snippet: "text"}}, nil
	}
	down := newFake("down", TierL0, result)
	down.healthy.Store(false)
	ok := newFake("ok", TierL0, result)
	m := NewManager(DefaultConfig(), testEgress(t), []Provider{down, ok})
	resp, err := m.Search(context.Background(), SearchRequest{Query: "health"})
	_ = m.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "success" || !contains(resp.Degraded, "provider_unhealthy:down") {
		t.Fatalf("unscheduled unhealthy provider changed status: %+v", resp)
	}

	failed := newFake("afailed", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return nil, Wrap("afailed", ErrUnavailable, errors.New("down"))
	})
	good := newFake("zgood", TierL0, result)
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m = NewManager(cfg, testEgress(t), []Provider{failed, good})
	resp, err = m.Search(context.Background(), SearchRequest{Query: "partial"})
	_ = m.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "partial" || len(resp.Results) == 0 {
		t.Fatalf("actual provider failure must remain partial: %+v", resp)
	}
}
