package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProvider 是离线测试用的 Provider。
type fakeProvider struct {
	name       string
	tier       Tier
	kinds      KindSet
	weight     float64
	pin        Mode
	remaining  int
	hasRemain  bool
	healthy    atomic.Bool
	searchFn   func(context.Context, SearchRequest) ([]Result, error)
	fetchFn    func(context.Context, FetchRequest) (*Document, error)
	mu         sync.Mutex
	calls      int
	queryCalls []string
}

// newFake 创建默认健康、支持检索的假源。
func newFake(name string, tier Tier, fn func(context.Context, SearchRequest) ([]Result, error)) *fakeProvider {
	p := &fakeProvider{name: name, tier: tier, kinds: KindSet(KindSearch), searchFn: fn}
	p.healthy.Store(true)
	return p
}

func (f *fakeProvider) Name() string     { return f.name }
func (f *fakeProvider) Tier() Tier       { return f.tier }
func (f *fakeProvider) Kinds() KindSet   { return f.kinds }
func (f *fakeProvider) Healthy() bool    { return f.healthy.Load() }
func (f *fakeProvider) Weight() float64  { return f.weight }
func (f *fakeProvider) EgressMode() Mode { return f.pin }

func (f *fakeProvider) Remaining() int {
	if f.hasRemain {
		return f.remaining
	}
	return -1
}

func (f *fakeProvider) Search(ctx context.Context, req SearchRequest) ([]Result, error) {
	f.mu.Lock()
	f.calls++
	f.queryCalls = append(f.queryCalls, req.Query)
	fn := f.searchFn
	f.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(ctx, req)
}

func (f *fakeProvider) Fetch(ctx context.Context, req FetchRequest) (*Document, error) {
	f.mu.Lock()
	fn := f.fetchFn
	f.mu.Unlock()
	if fn == nil {
		return nil, Wrap(f.name, ErrUnsupported, errors.New("fetch unsupported"))
	}
	return fn(ctx, req)
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeProvider) queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queryCalls...)
}

// testEgress 返回无代理的直连出口。
func testEgress(t *testing.T) *Egress {
	t.Helper()
	eg, err := NewEgress(EgressConfig{Mode: ModeDirect})
	if err != nil {
		t.Fatalf("NewEgress: %v", err)
	}
	t.Cleanup(func() { _ = eg.Close() })
	return eg
}

// allowAnyFetchURL 在测试中放开 SSRF 校验, 以便访问 httptest 的环回地址。
func allowAnyFetchURL(t *testing.T) {
	t.Helper()
	old := urlGuard
	urlGuard = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	t.Cleanup(func() { urlGuard = old })
}

func TestSearchEmptyQuery(t *testing.T) {
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	if _, err := m.Search(context.Background(), SearchRequest{Query: "   "}); KindOf(err) != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestSearchFiltersTierAndHealth(t *testing.T) {
	result := []Result{{URL: "https://a.com/1", Title: "a", Snippet: "s"}}
	ok := newFake("ok", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return result, nil
	})
	paid := newFake("paid", TierL3, func(context.Context, SearchRequest) ([]Result, error) {
		return result, nil
	})
	sick := newFake("sick", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return result, nil
	})
	sick.healthy.Store(false)

	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{paid, sick, ok})
	defer m.Close()

	resp, err := m.Search(context.Background(), SearchRequest{Query: "test"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Provider != "ok" {
		t.Fatalf("unexpected results: %+v", resp.Results)
	}
	if paid.callCount() != 0 || sick.callCount() != 0 || ok.callCount() != 1 {
		t.Fatalf("calls paid=%d sick=%d ok=%d", paid.callCount(), sick.callCount(), ok.callCount())
	}
}

func TestSearchAllFailReturnsNoProvider(t *testing.T) {
	fail := func(name string) *fakeProvider {
		return newFake(name, TierL0, func(context.Context, SearchRequest) ([]Result, error) {
			return nil, Wrap(name, ErrUnavailable, errors.New("down"))
		})
	}
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{fail("a"), fail("b")})
	defer m.Close()

	_, err := m.Search(context.Background(), SearchRequest{Query: "test"})
	if KindOf(err) != ErrNoProvider {
		t.Fatalf("want ErrNoProvider, got %v", err)
	}
}

func TestSearchDegradesSingleTimeout(t *testing.T) {
	slow := newFake("slow", TierL0, func(ctx context.Context, _ SearchRequest) ([]Result, error) {
		select {
		case <-time.After(300 * time.Millisecond):
			return []Result{{URL: "https://slow.com/1"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	fast := newFake("fast", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://fast.com/1", Title: "fast", Snippet: "ok"}}, nil
	})

	cfg := DefaultConfig()
	cfg.SearchTimeout = 60 * time.Millisecond
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{slow, fast})
	defer m.Close()

	resp, err := m.Search(context.Background(), SearchRequest{Query: "test"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("want 1 result, got %+v", resp.Results)
	}
	if !contains(resp.Degraded, "provider_timeout:slow") {
		t.Fatalf("degraded = %v", resp.Degraded)
	}
	if len(resp.Providers) != 1 || resp.Providers[0] != "fast" {
		t.Fatalf("providers = %v", resp.Providers)
	}
}

func TestSearchCacheHitAndQueryNormalization(t *testing.T) {
	p := newFake("p", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://a.com/1", Title: "a", Snippet: "s"}}, nil
	})
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	cfg.CacheTTL = time.Minute
	m := NewManager(cfg, testEgress(t), []Provider{p})
	defer m.Close()

	first, err := m.Search(context.Background(), SearchRequest{Query: "Go  Search"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.CacheHit {
		t.Fatal("first call must not be a cache hit")
	}
	second, err := m.Search(context.Background(), SearchRequest{Query: "go search"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.CacheHit {
		t.Fatal("second call should hit cache after query normalization")
	}
	if p.callCount() != 1 {
		t.Fatalf("provider calls = %d", p.callCount())
	}
}

func TestSearchSingleflight(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	p := newFake("p", TierL0, func(ctx context.Context, _ SearchRequest) ([]Result, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return []Result{{URL: "https://a.com/1", Title: "a", Snippet: "s"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	cfg.SearchTimeout = 2 * time.Second
	m := NewManager(cfg, testEgress(t), []Provider{p})
	defer m.Close()

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Search(context.Background(), SearchRequest{Query: "single"})
		}(i)
	}
	<-started
	time.Sleep(30 * time.Millisecond) // 让其它 goroutine 进入等待队列
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if p.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", p.callCount())
	}
}

func TestSearchMaxTwoSources(t *testing.T) {
	mk := func(name string) *fakeProvider {
		return newFake(name, TierL0, func(context.Context, SearchRequest) ([]Result, error) {
			return []Result{{URL: "https://" + name + ".com/1", Snippet: "s"}}, nil
		})
	}
	a, b, c := mk("a"), mk("b"), mk("c")
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{a, b, c})
	defer m.Close()

	if _, err := m.Search(context.Background(), SearchRequest{Query: "limit"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if total := a.callCount() + b.callCount() + c.callCount(); total != 2 {
		t.Fatalf("total provider calls = %d, want 2", total)
	}
}

func TestSelectProvidersProxyDownFallsBack(t *testing.T) {
	p := newFake("proxied", TierL2, func(context.Context, SearchRequest) ([]Result, error) { return nil, nil })
	p.pin = ModeProxy

	eg, err := NewEgress(EgressConfig{Mode: ModeAuto, ProxyURL: "http://127.0.0.1:1", FailThreshold: 1})
	if err != nil {
		t.Fatalf("NewEgress: %v", err)
	}
	defer eg.Close()
	eg.markFail() // 摘除代理

	cfg := DefaultConfig()
	cfg.Providers = map[string]ProviderConfig{
		"proxied": {Enabled: true, Egress: ModeProxy, Tier: TierL2},
	}
	cands, degraded := selectProviders([]Provider{p}, PlanQuery("x"), cfg, eg, newQuotaTracker())
	if len(cands) != 1 {
		t.Fatalf("proxy source should still be selected for direct fallback, got %d", len(cands))
	}
	if !contains(degraded, "proxy_down") {
		t.Fatalf("degraded = %v, want proxy_down", degraded)
	}
}

func TestFuseRRFAndCanonicalDedupe(t *testing.T) {
	raw := []Result{
		{URL: "https://a.com/x?utm_source=1", Provider: "p1", Rank: 1},
		{URL: "https://b.com/y", Provider: "p1", Rank: 2},
		{URL: "https://b.com/y/#frag", Provider: "p2", Rank: 1},
		{URL: "https://c.com/z", Provider: "p2", Rank: 2},
		{URL: "https://a.com/x", Provider: "p1", Rank: 5}, // 同源重复, 忽略
	}
	out := Fuse(raw, map[string]float64{"p1": 1, "p2": 1})
	if len(out) != 3 {
		t.Fatalf("want 3 unique results, got %d: %+v", len(out), out)
	}
	if out[0].URL != "https://b.com/y" || out[1].URL != "https://a.com/x" || out[2].URL != "https://c.com/z" {
		t.Fatalf("order = %v", urls(out))
	}
	wantB := 1.0/62.0 + 1.0/61.0
	if diff := out[0].Score - wantB; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("score b = %v, want %v", out[0].Score, wantB)
	}
	if out[0].Rank != 1 || out[1].Rank != 2 || out[2].Rank != 3 {
		t.Fatalf("ranks = %d,%d,%d", out[0].Rank, out[1].Rank, out[2].Rank)
	}
}

func TestFuseWeighted(t *testing.T) {
	raw := []Result{
		{URL: "https://a.com/x", Provider: "p1", Rank: 1},
		{URL: "https://c.com/z", Provider: "p2", Rank: 2},
		{URL: "https://b.com/y", Provider: "p1", Rank: 2},
		{URL: "https://b.com/y", Provider: "p2", Rank: 1},
	}
	out := Fuse(raw, map[string]float64{"p1": 1, "p2": 2})
	if out[0].URL != "https://b.com/y" {
		t.Fatalf("weighted order = %v", urls(out))
	}
	if out[1].URL != "https://c.com/z" {
		t.Fatalf("want c second, got %v", urls(out))
	}
}

func TestCanonicalURL(t *testing.T) {
	got := CanonicalURL("HTTPS://Example.COM/path/?utm_source=x&b=2&a=1#frag")
	want := "https://example.com/path?a=1&b=2"
	if got != want {
		t.Fatalf("CanonicalURL = %q, want %q", got, want)
	}
	if CanonicalURL("not a url") == "" {
		t.Fatal("unparseable input should fall back to original text")
	}
}

func TestQualityGate(t *testing.T) {
	now := time.Now()
	results := []Result{
		{URL: "https://a.com/1", Snippet: "x", Lang: "en", Published: now.Add(-time.Hour), Provider: "p1"},
		{URL: "https://b.com/1", Snippet: "x", Lang: "en", Published: now.Add(-time.Hour), Provider: "p2"},
		{URL: "https://c.com/1", Snippet: "x", Lang: "en", Published: now.Add(-time.Hour), Provider: "p1"},
	}
	q := evaluateQualityAt(results, SearchRequest{Language: "en", Freshness: "day"}, now)
	if q.Domains != 3 || !q.HasSnippet || q.FreshRatio != 1 || q.LangMatch != 1 || q.DuplicateRatio != 0 {
		t.Fatalf("quality = %+v", q)
	}
	if !q.Pass(true) {
		t.Fatal("quality should pass")
	}

	old := []Result{
		{URL: "https://a.com/1", Snippet: "x", Lang: "en", Published: now.Add(-30 * 24 * time.Hour), Provider: "p1"},
		{URL: "https://b.com/1", Snippet: "x", Lang: "en", Published: now.Add(-30 * 24 * time.Hour), Provider: "p2"},
		{URL: "https://c.com/1", Snippet: "x", Lang: "en", Published: now.Add(-30 * 24 * time.Hour), Provider: "p1"},
	}
	oq := evaluateQualityAt(old, SearchRequest{Language: "en", Freshness: "week"}, now)
	if oq.Pass(true) {
		t.Fatal("stale results must fail when freshness is required")
	}
	if !oq.Pass(false) {
		t.Fatal("stale results may pass when freshness is not required")
	}

	zhWant := evaluateQualityAt(results, SearchRequest{Language: "zh", Freshness: "any"}, now)
	if zhWant.Pass(false) {
		t.Fatal("language mismatch should fail the gate")
	}

	dup := append([]Result(nil), results...)
	dup = append(dup, results[0])
	dq := evaluateQualityAt(dup, SearchRequest{Language: "en", Freshness: "any"}, now)
	if dq.DuplicateRatio != 0.25 || !dq.Pass(false) {
		t.Fatalf("duplicate quality = %+v", dq)
	}
}

func TestFetchRejectsSSRF(t *testing.T) {
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	cases := []string{
		"ftp://example.com/file",
		"http://127.0.0.1/",
		"http://localhost/",
		"http://10.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"",
	}
	for _, raw := range cases {
		_, err := m.Fetch(context.Background(), FetchRequest{URL: raw})
		if err == nil {
			t.Fatalf("%q: expected error", raw)
		}
		if KindOf(err) != ErrInvalid {
			t.Fatalf("%q: kind = %s (%v)", raw, KindOf(err), err)
		}
	}
	if _, err := validateFetchURL("http://8.8.8.8/"); err != nil {
		t.Fatalf("public IP should be allowed: %v", err)
	}
}

func TestFetchReadabilityLite(t *testing.T) {
	allowAnyFetchURL(t)
	page := `<!doctype html><html><head>
<title>Test Page</title>
<meta property="article:published_time" content="2025-01-02T03:04:05Z">
<meta name="author" content="Alice">
<script>var hidden = "secret";</script>
</head><body>
<nav>menu here</nav>
<article>
  <h1>Heading</h1>
  <p>Hello <b>world</b>.</p>
  <p>Second paragraph.</p>
  <pre><code class="language-go">fmt.Println("hi")</code></pre>
</article>
<footer>footer here</footer>
</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	resp, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	doc := resp.Document
	if doc == nil {
		t.Fatal("nil document")
	}
	if doc.Title != "Test Page" || doc.Byline != "Alice" {
		t.Fatalf("meta = %q / %q", doc.Title, doc.Byline)
	}
	if doc.Published.Year() != 2025 || doc.Published.Month() != time.January {
		t.Fatalf("published = %v", doc.Published)
	}
	if doc.Via != "http" || doc.Hash == "" || doc.FetchedAt.IsZero() {
		t.Fatalf("doc metadata = %+v", doc)
	}
	md := doc.Markdown
	for _, want := range []string{"# Heading", "Hello world.", "Second paragraph.", "```go", `fmt.Println("hi")`} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
	for _, bad := range []string{"menu here", "footer here", "secret"} {
		if strings.Contains(md, bad) {
			t.Fatalf("markdown should not contain %q:\n%s", bad, md)
		}
	}
	if doc.Truncated {
		t.Fatal("small document should not be truncated")
	}
}

func TestFetchTruncates(t *testing.T) {
	allowAnyFetchURL(t)
	body := strings.Repeat("文字", 200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html><body><article><p>%s</p></article></body></html>", body)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	resp, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL, MaxChars: 20})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !resp.Document.Truncated {
		t.Fatal("expected Truncated=true")
	}
	if n := len([]rune(resp.Document.Markdown)); n != 20 {
		t.Fatalf("markdown runes = %d, want 20", n)
	}
}

func TestFetchRedirectValidation(t *testing.T) {
	old := urlGuard
	urlGuard = func(raw string) (*url.URL, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		if u.Hostname() == "169.254.169.254" {
			return nil, Wrap("fetch", ErrInvalid, errors.New("blocked redirect"))
		}
		return u, nil
	}
	t.Cleanup(func() { urlGuard = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	_, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL})
	if err == nil {
		t.Fatal("redirect to link-local address must fail")
	}
	if KindOf(err) != ErrInvalid {
		t.Fatalf("kind = %s (%v)", KindOf(err), err)
	}
}

func TestResearchDeterministic(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/apple":
			fmt.Fprint(w, `<html><head><title>Apple</title></head><body><article><p>苹果是一种常见水果。</p><p>它富含维生素。</p></article></body></html>`)
		case "/banana":
			fmt.Fprint(w, `<html><head><title>Banana</title></head><body><article><p>香蕉是黄色的水果。</p></article></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newFake("fake", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{
			{URL: srv.URL + "/apple", Title: "Apple", Snippet: "苹果", Rank: 1},
			{URL: srv.URL + "/banana", Title: "Banana", Snippet: "香蕉", Rank: 2},
		}, nil
	})
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{p})
	defer m.Close()

	res, err := m.Research(context.Background(), "苹果 和 香蕉 对比", ResearchOptions{
		MaxRounds: 1, MaxSearches: 4, MaxFetches: 2, WallClock: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if res.StopReason != "done" && res.StopReason != "budget" {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	if res.Rounds != 1 {
		t.Fatalf("rounds = %d", res.Rounds)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v", res.Results)
	}
	if len(res.Evidence) != 2 {
		t.Fatalf("evidence = %+v", res.Evidence)
	}
	if !strings.Contains(res.Answer, "苹果") || !strings.Contains(res.Answer, "香蕉") {
		t.Fatalf("answer = %q", res.Answer)
	}
	q := p.queries()
	if len(q) != 2 {
		t.Fatalf("queries = %v", q)
	}
	for _, ev := range res.Evidence {
		if ev.Offset < 0 || ev.Hash == "" {
			t.Fatalf("bad evidence: %+v", ev)
		}
	}
}

func TestResearchBudgetStop(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><article><p>alpha is a letter.</p></article></body></html>`)
	}))
	defer srv.Close()

	p := newFake("fake", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: srv.URL + "/a", Snippet: "alpha", Rank: 1}}, nil
	})
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{p})
	defer m.Close()

	res, err := m.Research(context.Background(), "alpha 和 beta", ResearchOptions{
		MaxRounds: 1, MaxSearches: 1, MaxFetches: 1, WallClock: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if res.StopReason != "budget" {
		t.Fatalf("stop reason = %q, want budget", res.StopReason)
	}
	if len(res.Evidence) != 1 {
		t.Fatalf("evidence = %+v", res.Evidence)
	}
}

func TestEvidenceLedgerVerbatim(t *testing.T) {
	doc := &Document{
		URL:       "https://a.com/1",
		Markdown:  "hello world. second sentence.",
		FetchedAt: time.Now(),
	}
	l := NewEvidenceLedger()
	if _, ok := l.Add(doc, "not present"); ok {
		t.Fatal("non-verbatim quote must be rejected")
	}
	ev, ok := l.Add(doc, "hello world")
	if !ok {
		t.Fatal("verbatim quote rejected")
	}
	if ev.ID != "E1" || ev.Offset != 0 || ev.Hash == "" {
		t.Fatalf("evidence = %+v", ev)
	}
	if _, ok := l.Add(doc, "hello world"); ok {
		t.Fatal("duplicate quote must be rejected")
	}
	best, ok := l.AddBest(doc, []string{"second"})
	if !ok || !strings.Contains(best.Quote, "second sentence") {
		t.Fatalf("AddBest = %+v, %v", best, ok)
	}
}

func TestSplitSubQueries(t *testing.T) {
	cases := map[string][]string{
		"苹果和香蕉":            {"苹果", "香蕉"},
		"A vs B":           {"A", "B"},
		"golang 与 rust 对比": {"golang", "rust"},
		"和平":               {"和平"},
	}
	for in, want := range cases {
		got := splitSubQueries(in)
		if len(got) != len(want) {
			t.Fatalf("%q => %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q => %v, want %v", in, got, want)
			}
		}
	}
}

func TestPlanQuerySignals(t *testing.T) {
	code := PlanQuery("golang panic: nil map")
	if code.Intent != IntentCode || len(code.SiteHints) == 0 {
		t.Fatalf("code plan = %+v", code)
	}
	news := PlanQuery("今天有什么最新新闻")
	if news.Intent != IntentNews || news.Freshness != "week" {
		t.Fatalf("news plan = %+v", news)
	}
	zh := PlanQuery("苹果公司")
	if zh.Language != "zh" {
		t.Fatalf("language = %q", zh.Language)
	}
}

func TestCacheTTLSelection(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheTTL = 10 * time.Minute
	cfg.CacheFreshTTL = 5 * time.Second
	m := NewManager(cfg, testEgress(t), nil)
	defer m.Close()

	if got := m.cacheTTL(PlanQuery("普通查询"), SearchRequest{}); got != 10*time.Minute {
		t.Fatalf("normal ttl = %v", got)
	}
	if got := m.cacheTTL(PlanQuery("今天最新"), SearchRequest{}); got != 5*time.Second {
		t.Fatalf("news ttl = %v", got)
	}
	if got := m.cacheTTL(PlanQuery("普通查询"), SearchRequest{Freshness: "day"}); got != 5*time.Second {
		t.Fatalf("freshness=day ttl = %v", got)
	}
}

func TestMetricsSnapshot(t *testing.T) {
	mt := NewMetrics()
	mt.Inc("search_requests_total", map[string]string{"intent": "factual", "lang": "en", "tier": "L0"})
	mt.Inc("search_requests_total", map[string]string{"lang": "en", "intent": "factual", "tier": "L0"})
	mt.Observe("search_latency_seconds", 0.2, map[string]string{"provider": "p"})
	mt.Observe("search_latency_seconds", 0.4, map[string]string{"provider": "p"})
	mt.Set("provider_quota_remaining", 42, map[string]string{"provider": "p"})

	snap := mt.Snapshot()
	key := `search_requests_total{intent=factual,lang=en,tier=L0}`
	if got := snap[key]; got != int64(2) {
		t.Fatalf("counter = %v", got)
	}
	if got := snap[`provider_quota_remaining{provider=p}`]; got != float64(42) {
		t.Fatalf("gauge = %v", got)
	}
	h, ok := snap[`search_latency_seconds{provider=p}`].(map[string]any)
	if !ok || h["count"] != 2 {
		t.Fatalf("histogram = %v", snap[`search_latency_seconds{provider=p}`])
	}
}

func TestQuotaCooldown(t *testing.T) {
	q := newQuotaTracker()
	now := time.Now()
	if !q.available("p", now) {
		t.Fatal("fresh provider should be available")
	}
	q.recordError("p", ErrRateLimited, now)
	if q.available("p", now.Add(time.Second)) {
		t.Fatal("rate-limited provider should be cooling down")
	}
	if !q.available("p", now.Add(quotaPenalty+time.Second)) {
		t.Fatal("provider should recover after penalty")
	}
}

func TestProviderStatus(t *testing.T) {
	fast := newFake("zfast", TierL0, func(context.Context, SearchRequest) ([]Result, error) { return nil, nil })
	fast.hasRemain = true
	fast.remaining = 7
	slow := newFake("aslow", TierL2, func(context.Context, SearchRequest) ([]Result, error) { return nil, nil })

	m := NewManager(DefaultConfig(), testEgress(t), []Provider{slow, fast})
	defer m.Close()

	statuses := m.ProviderStatus()
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v", statuses)
	}
	if statuses[0].Name != "zfast" || statuses[0].Tier != TierL0 || statuses[0].Remaining != 7 {
		t.Fatalf("first status = %+v", statuses[0])
	}
	if statuses[1].Name != "aslow" || statuses[1].Remaining != -1 {
		t.Fatalf("second status = %+v", statuses[1])
	}
}

func TestClosedManagerRejects(t *testing.T) {
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := m.Search(context.Background(), SearchRequest{Query: "x"}); KindOf(err) != ErrUnavailable {
		t.Fatalf("search after close: %v", err)
	}
	if _, err := m.Fetch(context.Background(), FetchRequest{URL: "http://8.8.8.8/"}); KindOf(err) != ErrUnavailable {
		t.Fatalf("fetch after close: %v", err)
	}
}

func TestSearchProviderErrorDegraded(t *testing.T) {
	bad := newFake("bad", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return nil, Wrap("bad", ErrUnavailable, errors.New("boom"))
	})
	good := newFake("good", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://good.com/1", Snippet: "s"}}, nil
	})
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{bad, good})
	defer m.Close()

	resp, err := m.Search(context.Background(), SearchRequest{Query: "test"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !contains(resp.Degraded, "provider_error:bad") {
		t.Fatalf("degraded = %v", resp.Degraded)
	}
	if len(resp.Results) != 1 || resp.Results[0].Provider != "good" {
		t.Fatalf("results = %+v", resp.Results)
	}
}

func TestCacheLRUEviction(t *testing.T) {
	p := newFake("p", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://a.com/1", Snippet: "s"}}, nil
	})
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	cfg.CacheMaxEntries = 1
	m := NewManager(cfg, testEgress(t), []Provider{p})
	defer m.Close()

	for _, q := range []string{"alpha one", "beta two", "alpha one"} {
		if _, err := m.Search(context.Background(), SearchRequest{Query: q}); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if p.callCount() != 3 {
		t.Fatalf("provider calls = %d, want 3 (first entry evicted)", p.callCount())
	}
}

func TestExtractHTMLJSONLD(t *testing.T) {
	page := `<html><head>
<script type="application/ld+json">{"@type":"Article","headline":"LD Title","datePublished":"2024-05-06T07:08:09Z","author":{"name":"Bob"}}</script>
</head><body><article><p>Body text long enough to be selected as content.</p></article></body></html>`
	got := extractHTML(page)
	if got.Title != "LD Title" || got.Byline != "Bob" {
		t.Fatalf("meta = %q / %q", got.Title, got.Byline)
	}
	if got.Published.Year() != 2024 || got.Published.Month() != time.May {
		t.Fatalf("published = %v", got.Published)
	}
	if strings.Contains(got.Markdown, "LD Title") || !strings.Contains(got.Markdown, "Body text") {
		t.Fatalf("markdown = %q", got.Markdown)
	}
}

func TestFetchPlainText(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "Line one\nLine two")
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	resp, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.Document.Title != "Line one" {
		t.Fatalf("title = %q", resp.Document.Title)
	}
	if !strings.Contains(resp.Document.Markdown, "Line two") {
		t.Fatalf("markdown = %q", resp.Document.Markdown)
	}
}

func TestResearchSaturated(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><article><p>alpha is a letter of the alphabet.</p></article></body></html>`)
	}))
	defer srv.Close()

	p := newFake("fake", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: srv.URL + "/a", Snippet: "alpha", Rank: 1}}, nil
	})
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{p})
	defer m.Close()

	res, err := m.Research(context.Background(), "alpha", ResearchOptions{
		MaxRounds: 2, MaxSearches: 10, MaxFetches: 10, WallClock: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if res.StopReason != "saturated" {
		t.Fatalf("stop reason = %q, want saturated", res.StopReason)
	}
	if res.Rounds != 2 {
		t.Fatalf("rounds = %d", res.Rounds)
	}
}

// contains 判断切片是否包含指定字符串。
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// urls 提取结果里的 URL, 便于失败信息展示。
func urls(results []Result) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.URL)
	}
	return out
}

// TestSelectProvidersMarksUnhealthy 验证退避中的源被跳过时必须留下归因。
//
// 不标 degraded 的话, 召回变差会表现为"本来就只有这些源", 运维无从发现
// 某个源正在退避。
func TestSelectProvidersMarksUnhealthy(t *testing.T) {
	ok := newFake("ok", TierL2, nil)
	down := newFake("down", TierL0, nil)
	down.healthy.Store(false)

	cfg := DefaultConfig()
	cfg.Providers = map[string]ProviderConfig{
		"ok":   {Enabled: true, Tier: TierL2},
		"down": {Enabled: true, Tier: TierL0},
	}
	cands, degraded := selectProviders([]Provider{down, ok}, PlanQuery("x"), cfg, testEgress(t), newQuotaTracker())
	if len(cands) != 1 || cands[0].Name() != "ok" {
		t.Fatalf("candidates = %v", cands)
	}
	if !contains(degraded, "provider_unhealthy:down") {
		t.Fatalf("degraded = %v, want provider_unhealthy:down", degraded)
	}
}

// TestSearchWaitsForPrimaryWhenHedgeIsFaster 验证快的是对冲源时不能立即返回。
//
// 新闻意图的候选顺序是 parallel → bing_rss: bing 往往 400ms 就回来,
// parallel 冷启动要 2s。若按"任一源有结果"早退, 每次新闻检索都会丢掉主源,
// 质量门直接不过。
func TestSearchWaitsForPrimaryWhenHedgeIsFaster(t *testing.T) {
	primary := newFake("primary", TierL0, func(ctx context.Context, _ SearchRequest) ([]Result, error) {
		select {
		case <-time.After(300 * time.Millisecond):
			return []Result{{URL: "https://a.com/p", Title: "p", Snippet: "s"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	// 同 tier 按 name 升序, 用 z 开头保证 primary 是候选第一位。
	hedge := newFake("zhedge", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://b.com/h", Title: "h", Snippet: "s"}}, nil
	})
	if hedge.Name() < primary.Name() {
		t.Fatalf("测试前提不成立: hedge 必须排在 primary 之后")
	}

	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{primary, hedge})
	defer m.Close()

	start := time.Now()
	resp, err := m.Search(context.Background(), SearchRequest{Query: "news"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatalf("在 %s 就返回, 没等主源", time.Since(start))
	}
	if !contains(resp.Providers, "primary") || !contains(resp.Providers, "zhedge") {
		t.Fatalf("providers = %v, want both", resp.Providers)
	}
}

// TestSearchReturnsEarlyWhenPrimaryIsFast 验证主源先成功时仍只等 250ms 的聚合窗。
func TestSearchReturnsEarlyWhenPrimaryIsFast(t *testing.T) {
	primary := newFake("primary", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://a.com/p", Title: "p", Snippet: "s"}}, nil
	})
	slow := newFake("zslow", TierL0, func(ctx context.Context, _ SearchRequest) ([]Result, error) {
		select {
		case <-time.After(3 * time.Second):
			return []Result{{URL: "https://b.com/s", Title: "s", Snippet: "s"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	m := NewManager(cfg, testEgress(t), []Provider{primary, slow})
	defer m.Close()

	start := time.Now()
	resp, err := m.Search(context.Background(), SearchRequest{Query: "general"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("主源已成功却等了 %s, 聚合窗没有生效", d)
	}
	if !contains(resp.Providers, "primary") {
		t.Fatalf("providers = %v", resp.Providers)
	}
}

// TestPlanQueryVersionQueriesAreNotWeeklyNews 验证版本/发布说明类查询不被误判为
// "最近一周的新闻" —— 那会让质量门因为文档日期旧而恒不过。
func TestPlanQueryVersionQueriesAreNotWeeklyNews(t *testing.T) {
	for _, q := range []string{"postgres 18 release notes", "kubernetes 1.34 release date"} {
		plan := PlanQuery(q)
		if plan.Freshness == "week" || plan.Freshness == "day" {
			t.Errorf("%q freshness = %s, want any/year", q, plan.Freshness)
		}
	}
	plan := PlanQuery("kubernetes 1.34 release date 2025")
	if plan.Freshness != "year" {
		t.Errorf("带年份 freshness = %s, want year", plan.Freshness)
	}
	if got := PlanQuery("今天有什么新闻").Freshness; got != "week" {
		t.Errorf("新闻 freshness = %s, want week", got)
	}
}
