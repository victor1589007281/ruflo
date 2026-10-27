package search

import (
	"context"
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

func TestNeedsJSRenderIsConservative(t *testing.T) {
	static := `<html><body><main><p>` + strings.Repeat("static article body ", 30) + `</p></main><script>console.log("enhancement")</script></body></html>`
	page := extractHTML(static)
	if needsJSRender(static, page.Markdown) {
		t.Fatal("static page with sufficient body must not launch Chrome")
	}
	for _, raw := range []string{
		`<html><body><div id="root">Loading...</div><script src="/app.js"></script></body></html>`,
		`<html><body><script src="/app.js"></script></body></html>`,
		`<html><body><div>You need to enable JavaScript to run this app.</div><script src="/app.js"></script></body></html>`,
	} {
		if !needsJSRender(raw, extractHTML(raw).Markdown) {
			t.Fatalf("JS shell did not trigger rendering: %s", raw)
		}
	}
	shortStatic := `<html><body><p>Short but complete.</p><script>console.log("analytics")</script></body></html>`
	if needsJSRender(shortStatic, extractHTML(shortStatic).Markdown) {
		t.Fatal("an unrelated script must not turn a short static page into a JS shell")
	}
}

func TestDocumentFromDOMLimitsAndChallenge(t *testing.T) {
	if _, err := documentFromDOM("https://example.test", "limit", "<html>too large</html>", 8); KindOf(err) != ErrUnavailable {
		t.Fatalf("DOM limit kind=%s err=%v", KindOf(err), err)
	}
	challenge := `<html><head><title>Just a moment...</title></head><body><div class="cf-chl">Checking your browser</div></body></html>`
	if _, err := documentFromDOM("https://example.test", "Just a moment...", challenge, 4096); !IsBlocked(err) {
		t.Fatalf("challenge was not blocked: %v", err)
	}
}

func TestBrowserRendererExtractsFinalDOM(t *testing.T) {
	requireRenderChrome(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><meta property="og:title" content="Metadata title"><title>Static title</title></head><body><main><p id="content">Loading</p></main><script>document.getElementById("content").textContent="Rendered final DOM body with useful content.";document.title="Rendered title";</script></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	guard := recordingGuardFor(srv.URL)
	renderer := NewBrowserRenderer(BrowserRenderOptions{Timeout: 10 * time.Second, Settle: 50 * time.Millisecond, Guard: guard.check})
	doc, err := renderer.Render(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if doc.URL != srv.URL+"/final" || doc.Title != "Rendered title" || doc.Via != "chromedp" {
		t.Fatalf("final document identity: %+v", doc)
	}
	if !strings.Contains(doc.Markdown, "Rendered final DOM body") || strings.Contains(doc.Markdown, "Loading") {
		t.Fatalf("final DOM was not extracted: %q", doc.Markdown)
	}
	if doc.Hash != hashDocument(doc.Markdown) {
		t.Fatalf("hash does not cover rendered markdown: %+v", doc)
	}
	if !guard.saw(srv.URL + "/final") {
		t.Fatalf("redirect target did not pass through guard: %v", guard.urls())
	}
}

func TestBrowserRendererBlocksPrivateSubresource(t *testing.T) {
	requireRenderChrome(t)
	var calls atomic.Int32
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, "window.privateRequestReached=true")
	}))
	defer blocked.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<html><body><main>Rendered shell</main><script src="%s/payload.js"></script></body></html>`, blocked.URL)
	}))
	defer srv.Close()
	guard := recordingGuardFor(srv.URL)
	renderer := NewBrowserRenderer(BrowserRenderOptions{Timeout: 10 * time.Second, Settle: 50 * time.Millisecond, Guard: guard.check})
	doc, err := renderer.Render(context.Background(), srv.URL)
	if err == nil || doc != nil || KindOf(err) != ErrInvalid {
		t.Fatalf("private subresource was not rejected: doc=%+v err=%v", doc, err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("blocked subresource reached private server %d times", got)
	}
	if !guard.saw(blocked.URL + "/payload.js") {
		t.Fatalf("subresource bypassed guard: %v", guard.urls())
	}
}

func TestBrowserRendererBlocksPrivateRedirect(t *testing.T) {
	requireRenderChrome(t)
	var calls atomic.Int32
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, "must not arrive")
	}))
	defer blocked.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blocked.URL+"/private", http.StatusFound)
	}))
	defer srv.Close()
	guard := recordingGuardFor(srv.URL)
	renderer := NewBrowserRenderer(BrowserRenderOptions{Timeout: 10 * time.Second, Settle: 50 * time.Millisecond, Guard: guard.check})
	doc, err := renderer.Render(context.Background(), srv.URL)
	if err == nil || doc != nil || KindOf(err) != ErrInvalid {
		t.Fatalf("private redirect was not rejected: doc=%+v err=%v", doc, err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("blocked redirect reached private server %d times", got)
	}
	if !guard.saw(blocked.URL + "/private") {
		t.Fatalf("redirect bypassed guard: %v", guard.urls())
	}
}

type jsRendererFunc func(context.Context, string) (*Document, error)

func (f jsRendererFunc) Render(ctx context.Context, rawURL string) (*Document, error) {
	return f(ctx, rawURL)
}

func TestManagerFetchUsesRendererOnlyForJSShell(t *testing.T) {
	allowAnyFetchURL(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/shell", http.StatusFound)
	})
	mux.HandleFunc("/shell", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<html><body><div id="root">Loading...</div><script src="/app.js"></script></body></html>`)
	})
	mux.HandleFunc("/static", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<html><body><main>%s</main><script>console.log("enhancement")</script></body></html>`, strings.Repeat("static production body ", 20))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var renders atomic.Int32
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	m.renderer = jsRendererFunc(func(_ context.Context, rawURL string) (*Document, error) {
		renders.Add(1)
		if rawURL != srv.URL+"/shell" {
			t.Fatalf("renderer did not receive final response URL: %q", rawURL)
		}
		return &Document{URL: rawURL, Markdown: "rendered production body", Via: "chromedp"}, nil
	})
	rendered, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/start"})
	if err != nil || rendered.Document.Markdown != "rendered production body" || renders.Load() != 1 {
		t.Fatalf("JS shell did not use production renderer: resp=%+v err=%v renders=%d", rendered, err, renders.Load())
	}
	static, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/static"})
	if err != nil || static.Document.Via != "http" || renders.Load() != 1 {
		t.Fatalf("static page launched renderer: resp=%+v err=%v renders=%d", static, err, renders.Load())
	}
}

func TestManagerFetchRendersDOMWritingShellButNotShortStatic(t *testing.T) {
	allowAnyFetchURL(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/shell", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<html><body><div class="container"><h1>Delayed entries</h1><script>var entries = [{body: "Rendered entry"}]; for (var i = 0; i < entries.length; i++) { document.write("<article>" + entries[i].body + "</article>"); }</script></div><p>Static attribution</p></body></html>`)
	})
	mux.HandleFunc("/static", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<html><body><p>Short but complete static page.</p><script>console.log("analytics")</script></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var renders atomic.Int32
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	m.renderer = jsRendererFunc(func(_ context.Context, rawURL string) (*Document, error) {
		renders.Add(1)
		if rawURL != srv.URL+"/shell" {
			t.Fatalf("unexpected renderer URL: %q", rawURL)
		}
		return &Document{URL: rawURL, Markdown: "rendered DOM-written body", Via: "chromedp"}, nil
	})

	rendered, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/shell"})
	if err != nil || rendered.Document.Via != "chromedp" || rendered.Document.Markdown != "rendered DOM-written body" || renders.Load() != 1 {
		t.Fatalf("DOM-writing shell did not use production renderer: resp=%+v err=%v renders=%d", rendered, err, renders.Load())
	}
	static, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/static"})
	if err != nil || static.Document.Via != "http" || !strings.Contains(static.Document.Markdown, "Short but complete static page.") || renders.Load() != 1 {
		t.Fatalf("short static page launched renderer: resp=%+v err=%v renders=%d", static, err, renders.Load())
	}
}

func TestManagerFetchRenderedHeadingOnlySelectionFallsBackToBody(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<html><body><script>document.write("rendered body")</script></body></html>`)
	}))
	defer srv.Close()

	const quotation = "The world as we have created it is a process of our thinking. It cannot be changed without changing our thinking."
	rendered := "# Quotes to Scrape\n\nLogin\n\n“" + quotation + "”by Albert EinsteinTags: thinking" + strings.Repeat(" filler", 40)
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	m.renderer = jsRendererFunc(func(_ context.Context, rawURL string) (*Document, error) {
		return &Document{URL: rawURL, Markdown: rendered, Via: "chromedp"}, nil
	})
	resp, err := m.Fetch(context.Background(), FetchRequest{
		URL: srv.URL, Prompt: "Extract the first quotation rendered by JavaScript and its author. Return the quote and author verbatim.",
		MaxContentTokens: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := resp.Document
	if doc.Via != "chromedp" || doc.Selection != "prefix" ||
		!strings.Contains(doc.Markdown, quotation) || !strings.Contains(doc.Markdown, "Albert Einstein") {
		t.Fatalf("heading-only match hid rendered body: %+v", doc)
	}
	if doc.EstimatedTokens != len([]byte(doc.Markdown)) || doc.EstimatedTokens > 300 {
		t.Fatalf("fallback exceeded byte budget: %+v", doc)
	}
	full := []rune(rendered)
	for _, ev := range doc.Evidence {
		quote := []rune(ev.Quote)
		if ev.Offset < 0 || ev.Offset+len(quote) > len(full) || string(full[ev.Offset:ev.Offset+len(quote)]) != ev.Quote {
			t.Errorf("evidence offset does not resolve to rendered full text: %+v", ev)
		}
	}
}

func TestManagerFetchRendersJSWithChrome(t *testing.T) {
	requireRenderChrome(t)
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<html><body><div id="root">Loading...</div><script>document.getElementById("root").textContent="Production Manager rendered this JS body.";</script></body></html>`)
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.FetchTimeout = 15 * time.Second
	m := NewManager(cfg, testEgress(t), nil)
	defer m.Close()
	resp, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL})
	if err != nil {
		t.Fatalf("production Fetch with Chrome: %v", err)
	}
	if resp.Document.Via != "chromedp" || !strings.Contains(resp.Document.Markdown, "Production Manager rendered") {
		t.Fatalf("production JS document: %+v", resp.Document)
	}
}

func TestBrowserOptionsFollowSelectedEgressClient(t *testing.T) {
	eg, err := NewEgress(EgressConfig{
		Mode: ModeAuto, ProxyURL: "http://proxy.example:8080",
		NoProxy: []string{"localhost", "127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eg.Close()
	direct, err := eg.browserOptions(eg.Client(ModeDirect))
	if err != nil || direct.ProxyServer != "" {
		t.Fatalf("direct client acquired proxy: opts=%+v err=%v", direct, err)
	}
	proxied, err := eg.browserOptions(eg.Client(ModeProxy))
	if err != nil || proxied.ProxyServer != "http://proxy.example:8080" || len(proxied.ProxyBypassList) != 2 {
		t.Fatalf("proxy client did not reach Chromium unchanged: opts=%+v err=%v", proxied, err)
	}

	authenticated, err := NewEgress(EgressConfig{Mode: ModeProxy, ProxyURL: "http://user:do-not-print@proxy.example:8080"})
	if err != nil {
		t.Fatal(err)
	}
	defer authenticated.Close()
	_, err = authenticated.browserOptions(authenticated.Client(ModeProxy))
	if KindOf(err) != ErrUnsupported || strings.Contains(err.Error(), "do-not-print") {
		t.Fatalf("proxy credentials were not rejected safely: %v", err)
	}
}

func TestBrowserRendererRejectsUnsafeInitialSchemes(t *testing.T) {
	allowAll := func(raw string) (*url.URL, error) { return url.Parse(raw) }
	for _, raw := range []string{"file:///etc/passwd", "ws://127.0.0.1/socket", "data:text/html,hello"} {
		if _, err := guardRenderURL(allowAll, raw); KindOf(err) != ErrInvalid {
			t.Fatalf("unsafe scheme %q bypassed renderer boundary: kind=%s err=%v", raw, KindOf(err), err)
		}
	}
}

func requireRenderChrome(t *testing.T) {
	t.Helper()
	if _, err := findRenderChrome(""); err != nil {
		t.Skipf("Chrome/Chromium is not installed: %v", err)
	}
}

// recordingRenderGuard 仅放行指定 httptest origin，其它目标仍走真实生产校验。
type recordingRenderGuard struct {
	allowed map[string]bool
	mu      sync.Mutex
	seen    []string
}

func recordingGuardFor(origins ...string) *recordingRenderGuard {
	g := &recordingRenderGuard{allowed: make(map[string]bool)}
	for _, raw := range origins {
		if u, err := url.Parse(raw); err == nil {
			g.allowed[u.Scheme+"://"+u.Host] = true
		}
	}
	return g
}

func (g *recordingRenderGuard) check(raw string) (*url.URL, error) {
	g.mu.Lock()
	g.seen = append(g.seen, raw)
	g.mu.Unlock()
	u, err := url.Parse(raw)
	if err == nil && g.allowed[u.Scheme+"://"+u.Host] {
		return u, nil
	}
	return validateFetchURL(raw)
}

func (g *recordingRenderGuard) saw(raw string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, seen := range g.seen {
		if seen == raw {
			return true
		}
	}
	return false
}

func (g *recordingRenderGuard) urls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.seen...)
}
