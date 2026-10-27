package search

// browser_render.go —— 用标准 headless Chrome 渲染 JS 页面并提取最终 DOM。
//
// 每个 HTTP(S) 请求（含重定向和子资源）都在 CDP Fetch 请求阶段经过与生产
// Fetch 相同的 urlGuard；其它协议由浏览器黑名单和事件检查双重拦截。这里不
// 使用 stealth、WAF 绕过或网页指令，只把渲染后的 DOM 交给 extractHTML。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	cdpfetch "github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	cdptarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const (
	defaultRenderTimeout = 12 * time.Second
	defaultRenderSettle  = 300 * time.Millisecond
	defaultMaxDOMBytes   = maxFetchBodyBytes
	staticContentRunes   = 240
	shellContentRunes    = 80
)

// JSRenderer 是生产 Fetch 可注入的渲染接口。
type JSRenderer interface {
	Render(context.Context, string) (*Document, error)
}

// BrowserRenderOptions 配置浏览器；Guard 默认使用包内生产 urlGuard。
// ProxyServer/ProxyBypassList 必须来自本次实际选中的 Egress client；空代理会
// 显式传 --no-proxy-server，防止 Chrome 继承宿主代理而与 HTTP 直连分叉。
type BrowserRenderOptions struct {
	Executable      string
	Timeout         time.Duration
	Settle          time.Duration
	MaxDOMBytes     int
	Guard           func(string) (*url.URL, error)
	ProxyServer     string
	ProxyBypassList []string
}

// BrowserRenderer 每次渲染使用独立浏览器和单页面，避免跨请求共享状态。
type BrowserRenderer struct {
	opts BrowserRenderOptions
}

// NewBrowserRenderer 创建可注入 guard/可执行文件的渲染器。
func NewBrowserRenderer(opts BrowserRenderOptions) *BrowserRenderer {
	return &BrowserRenderer{opts: opts}
}

// RenderJS 使用生产默认 guard 渲染一个 URL。
func RenderJS(ctx context.Context, rawURL string) (*Document, error) {
	return NewBrowserRenderer(BrowserRenderOptions{}).Render(ctx, rawURL)
}

// Render 渲染并提取最终 DOM；CAPTCHA/WAF 只报告 blocked，不尝试绕过。
func (r *BrowserRenderer) Render(ctx context.Context, rawURL string) (*Document, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts := r.opts
	if opts.Timeout <= 0 {
		opts.Timeout = defaultRenderTimeout
	}
	if opts.Settle <= 0 {
		opts.Settle = defaultRenderSettle
	}
	if opts.MaxDOMBytes <= 0 {
		opts.MaxDOMBytes = defaultMaxDOMBytes
	}
	if opts.Guard == nil {
		opts.Guard = urlGuard
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	u, err := guardRenderURL(opts.Guard, rawURL)
	if err != nil {
		return nil, err
	}
	executable, err := findRenderChrome(opts.Executable)
	if err != nil {
		return nil, err
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, renderAllocatorOptions(executable, opts)...)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()
	state := newRenderState(opts.Guard)
	listenRenderRequests(browserCtx, state)

	err = chromedp.Run(browserCtx,
		network.Enable(),
		blockUnsafeSchemes(),
		network.SetBypassServiceWorker(true),
		cdpfetch.Enable().WithPatterns([]*cdpfetch.RequestPattern{{
			URLPattern: "*", RequestStage: cdpfetch.RequestStageRequest,
		}}),
	)
	if err != nil {
		return nil, renderError(browserCtx, state, err)
	}
	closeExtraPages(browserCtx, state)

	var finalURL, title, dom string
	err = chromedp.Run(browserCtx,
		chromedp.Navigate(u.String()),
		chromedp.WaitReady("body", chromedp.ByQuery),
		renderDelay(opts.Settle),
		emulation.SetScriptExecutionDisabled(true),
		chromedp.Stop(),
	)
	if err != nil {
		return nil, renderError(browserCtx, state, err)
	}
	state.wait(browserCtx)
	if err := state.Err(); err != nil {
		return nil, err
	}
	err = chromedp.Run(browserCtx,
		chromedp.Location(&finalURL),
		chromedp.Title(&title),
		chromedp.OuterHTML("html", &dom, chromedp.ByQuery),
	)
	if err != nil {
		return nil, renderError(browserCtx, state, err)
	}
	if err := state.Err(); err != nil {
		return nil, err
	}
	if _, err := guardRenderURL(opts.Guard, finalURL); err != nil {
		return nil, err
	}
	if status, ok := state.documentStatus(finalURL); ok && (status < 200 || status >= 300) {
		return nil, Wrap("chromedp", statusKind(int(status)),
			fmt.Errorf("main document HTTP status %d", status))
	}
	return documentFromDOM(finalURL, title, dom, opts.MaxDOMBytes)
}

func renderAllocatorOptions(executable string, opts BrowserRenderOptions) []chromedp.ExecAllocatorOption {
	out := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(executable),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("no-pings", true),
	}
	if opts.ProxyServer == "" {
		out = append(out, chromedp.Flag("no-proxy-server", true))
	} else {
		out = append(out, chromedp.ProxyServer(opts.ProxyServer))
		if len(opts.ProxyBypassList) > 0 {
			out = append(out, chromedp.Flag("proxy-bypass-list", strings.Join(opts.ProxyBypassList, ",")))
		}
	}
	return out
}

// browserOptions 把已经选定的 HTTP 出口原样映射到 Chromium。带凭据的代理
// 不放进进程命令行；CIDR bypass 也不假定 Chrome 与 Go 语义相同，明确拒绝
// JS 渲染而不是悄悄换一条出口。
func (e *Egress) browserOptions(client *http.Client) (BrowserRenderOptions, error) {
	var opts BrowserRenderOptions
	if e == nil || client == nil || client != e.proxy {
		return opts, nil
	}
	proxyURL, err := url.Parse(e.proxyURL)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return opts, Wrap("chromedp", ErrUnavailable, errors.New("invalid proxy configuration for JS rendering"))
	}
	if proxyURL.User != nil {
		return opts, Wrap("chromedp", ErrUnsupported, errors.New("authenticated proxy is not supported for JS rendering"))
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https", "socks5":
	default:
		return opts, Wrap("chromedp", ErrUnsupported, errors.New("proxy scheme is not supported for JS rendering"))
	}
	for _, rule := range e.noProxy {
		if strings.Contains(rule, "/") {
			return opts, Wrap("chromedp", ErrUnsupported, errors.New("CIDR proxy bypass is not supported for JS rendering"))
		}
	}
	opts.ProxyServer = e.proxyURL
	opts.ProxyBypassList = append([]string(nil), e.noProxy...)
	return opts, nil
}

func (m *Manager) renderJS(ctx context.Context, client *http.Client, rawURL string) (*Document, error) {
	if m.renderer != nil {
		return m.renderer.Render(ctx, rawURL)
	}
	opts, err := m.egress.browserOptions(client)
	if err != nil {
		return nil, err
	}
	return NewBrowserRenderer(opts).Render(ctx, rawURL)
}

func findRenderChrome(configured string) (string, error) {
	if configured != "" {
		path, err := exec.LookPath(configured)
		if err == nil {
			return path, nil
		}
		return "", Wrap("chromedp", ErrUnavailable, err)
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "google-chrome-stable"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", Wrap("chromedp", ErrUnavailable, errors.New("Chrome/Chromium executable not found"))
}

// blockUnsafeSchemes 使用兼容新旧 Chromium 的 urls 形式；Fetch 仍逐请求复核。
func blockUnsafeSchemes() chromedp.Action {
	patterns := []string{
		"file://*", "ws://*", "wss://*", "data:*", "blob:*", "filesystem:*",
		"ftp://*", "about:*", "chrome:*", "chrome-extension:*", "view-source:*",
	}
	return chromedp.ActionFunc(func(ctx context.Context) error {
		params := struct {
			URLs []string `json:"urls"`
		}{URLs: patterns}
		return cdp.Execute(ctx, network.CommandSetBlockedURLs, params, nil)
	})
}

type renderState struct {
	guard    func(string) (*url.URL, error)
	mu       sync.Mutex
	err      error
	statuses map[string]int64
	pending  int
	idle     chan struct{}
}

func newRenderState(guard func(string) (*url.URL, error)) *renderState {
	return &renderState{guard: guard, statuses: make(map[string]int64)}
}

func (s *renderState) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *renderState) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *renderState) check(raw string) {
	if _, err := guardRenderURL(s.guard, raw); err != nil {
		s.fail(Wrap("chromedp", KindOf(err), fmt.Errorf("request %s: %w", requestLabel(raw), err)))
	}
}

func (s *renderState) spawn(fn func()) {
	s.mu.Lock()
	if s.pending == 0 {
		s.idle = make(chan struct{})
	}
	s.pending++
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.pending--
			if s.pending == 0 {
				close(s.idle)
			}
			s.mu.Unlock()
		}()
		fn()
	}()
}

func (s *renderState) wait(ctx context.Context) {
	s.mu.Lock()
	if s.pending == 0 {
		s.mu.Unlock()
		return
	}
	idle := s.idle
	s.mu.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
	}
}

func (s *renderState) setDocumentStatus(raw string, status int64) {
	s.mu.Lock()
	s.statuses[responseKey(raw)] = status
	s.mu.Unlock()
}

func (s *renderState) documentStatus(raw string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, ok := s.statuses[responseKey(raw)]
	return status, ok
}

func listenRenderRequests(ctx context.Context, state *renderState) {
	chromedp.ListenTarget(ctx, func(ev any) {
		switch ev := ev.(type) {
		case *cdpfetch.EventRequestPaused:
			state.spawn(func() {
				requestCtx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target)
				if ev.Request == nil {
					state.fail(Wrap("chromedp", ErrUnavailable, errors.New("request paused without URL")))
					_ = cdpfetch.FailRequest(ev.RequestID, network.ErrorReasonFailed).Do(requestCtx)
					return
				}
				if _, err := guardRenderURL(state.guard, ev.Request.URL); err != nil {
					state.fail(Wrap("chromedp", KindOf(err),
						fmt.Errorf("request %s: %w", requestLabel(ev.Request.URL), err)))
					_ = cdpfetch.FailRequest(ev.RequestID, network.ErrorReasonBlockedByClient).Do(requestCtx)
					return
				}
				if err := cdpfetch.ContinueRequest(ev.RequestID).Do(requestCtx); err != nil && ctx.Err() == nil {
					state.fail(Wrap("chromedp", ErrUnavailable, err))
				}
			})
		case *network.EventRequestWillBeSent:
			if ev.Request != nil && !approvedRequestScheme(ev.Request.URL) {
				raw := ev.Request.URL
				state.spawn(func() { state.check(raw) })
			}
		case *network.EventWebSocketCreated:
			raw := ev.URL
			state.spawn(func() { state.check(raw) })
		case *network.EventResponseReceived:
			if ev.Type == network.ResourceTypeDocument && ev.Response != nil {
				state.setDocumentStatus(ev.Response.URL, ev.Response.Status)
			}
		}
	})
}

func closeExtraPages(ctx context.Context, state *renderState) {
	mainID := chromedp.FromContext(ctx).Target.TargetID
	chromedp.ListenBrowser(ctx, func(ev any) {
		created, ok := ev.(*cdptarget.EventTargetCreated)
		if !ok || created.TargetInfo == nil || created.TargetInfo.Type != "page" ||
			created.TargetInfo.TargetID == mainID {
			return
		}
		id := created.TargetInfo.TargetID
		state.spawn(func() {
			browser := chromedp.FromContext(ctx).Browser
			_ = cdptarget.CloseTarget(id).Do(cdp.WithExecutor(ctx, browser))
		})
	})
}

func documentFromDOM(finalURL, title, dom string, maxBytes int) (*Document, error) {
	if len(dom) == 0 {
		return nil, Wrap("chromedp", ErrUnavailable, errors.New("empty rendered DOM"))
	}
	if len(dom) > maxBytes {
		return nil, Wrap("chromedp", ErrUnavailable, fmt.Errorf("rendered DOM exceeds %d bytes", maxBytes))
	}
	if isChallengeDOM(title, dom) {
		return nil, Wrap("chromedp", ErrBlocked, errors.New("CAPTCHA/WAF challenge detected"))
	}
	page := extractHTML(dom)
	if strings.TrimSpace(title) != "" {
		page.Title = strings.TrimSpace(title)
	}
	page.Markdown = strings.TrimSpace(normalizeMarkdown(page.Markdown))
	if page.Markdown == "" {
		return nil, Wrap("chromedp", ErrUnavailable, errors.New("rendered DOM has no readable text"))
	}
	return &Document{
		URL: finalURL, Title: page.Title, Markdown: page.Markdown,
		Byline: page.Byline, Published: page.Published, FetchedAt: time.Now(),
		Hash: hashDocument(page.Markdown), Via: "chromedp",
	}, nil
}

// needsJSRender 只在正文不足且有明确 JS shell 或脚本生成正文信号时触发，避免静态页开 Chrome。
func needsJSRender(rawHTML, markdown string) bool {
	markdown = strings.TrimSpace(markdown)
	if utf8.RuneCountInString(markdown) >= staticContentRunes {
		return false
	}
	lower := strings.ToLower(rawHTML)
	if !strings.Contains(lower, "<script") {
		return false
	}
	if markdown == "" {
		return true
	}
	if utf8.RuneCountInString(markdown) > shellContentRunes {
		return false
	}
	for _, marker := range []string{
		`id="root"`, `id='root'`, `id="app"`, `id='app'`, `id="__next"`,
		"data-reactroot", "ng-app", "ng-version", "data-v-", "enable javascript to run",
		"enable javascript and cookies", "javascript is required", "javascript to run this app",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// document.write/writeln 无需框架挂载点也能生成正文，是低正文量页面
	// 依赖 JS 的明确信号；普通 analytics/enhancement 脚本不包含该信号。
	return strings.Contains(lower, "document.write")
}

func isChallengeDOM(title, dom string) bool {
	lowerTitle := strings.ToLower(strings.TrimSpace(title))
	for _, marker := range []string{"just a moment", "attention required", "security check", "access denied", "captcha"} {
		if strings.Contains(lowerTitle, marker) {
			return true
		}
	}
	lower := strings.ToLower(dom)
	for _, marker := range []string{
		"cf-chl", "cf-turnstile", "challenge-platform", "g-recaptcha", "h-captcha",
		"data-sitekey", "captcha-form", "captcha-container", "aws-waf-token",
		"px-captcha", "waf-challenge", "incapsula", "verify you are human",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if len(dom) <= 20000 {
		for _, marker := range []string{
			"checking your browser", "enable javascript and cookies to continue",
			"unusual traffic", "too many automated requests", "captcha",
		} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return false
}

func guardRenderURL(guard func(string) (*url.URL, error), raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, Wrap("fetch", ErrInvalid, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, Wrap("fetch", ErrInvalid, fmt.Errorf("unsupported scheme %q", u.Scheme))
	}
	return guard(raw)
}

func approvedRequestScheme(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

func responseKey(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.Fragment = ""
		return u.String()
	}
	return raw
}

func requestLabel(raw string) string {
	if len(raw) > 200 {
		return fmt.Sprintf("%.200s...", raw)
	}
	return raw
}

func renderDelay(d time.Duration) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	})
}

func renderError(ctx context.Context, state *renderState, err error) error {
	if guardErr := state.Err(); guardErr != nil {
		return guardErr
	}
	kind := ErrUnavailable
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		kind = ErrTimeout
	}
	return Wrap("chromedp", kind, err)
}
