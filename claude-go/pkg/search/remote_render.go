package search

// remote_render.go —— 远程 JS 渲染兜底: Firecrawl MCP 端点。
//
// 本地 Chromium 渲染有三类够不着的情况: 运行环境无浏览器 (容器镜像不含
// Chrome)、渲染超时、目标页是挑战壳 (CAPTCHA/WAF —— 本地策略明确不绕过,
// 只报告 blocked)。远程渲染把"执行 JS 拿最终 DOM"这件事外包给 Firecrawl
// 的渲染集群: 它不绕过任何挑战 (知乎这类站点 Firecrawl 同样拿到安全验证页,
// 本服务照样按低正文量拒绝), 只是把本地没有浏览器/够不着的渲染负载移出去。
//
// 凭据口径: 默认走 Firecrawl 托管 MCP 的 keyless 档 (2026-11-15 实测匿名
// scrape 可用, 服务端有日额度, 超额 429); 配 FIRECRAWL_API_KEY 自动升级为
// 付费档。Endpoint 也可指向自托管 firecrawl-mcp (同形 tools/call 协议)。
//
// 出口: 渲染请求经 Egress 的 auto 客户端发出 (代理健康走代理, 否则直连),
// 与 fetch 逐源钉出口的白名单机制一致 —— 部署侧只需按出口逐源表放通
// mcp.firecrawl.dev。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// remoteRenderVia 是远程渲染产出文档的 Via 标记, 与 chromedp/http/wayback 并列。
const remoteRenderVia = "firecrawl"

// FirecrawlRenderer 通过 Firecrawl MCP 的 firecrawl_scrape 工具渲染一个 URL。
//
// 实现的是 MCP streamable-HTTP 的单向 tools/call: Firecrawl 该端点实测
// 无会话状态, 每个 POST 独立 (initialize 非必需), 因此不为一次渲染
// 维护会话, 失败语义也更简单。
type FirecrawlRenderer struct {
	endpoint string
	apiKey   string
	timeout  time.Duration
	client   *http.Client
	ua       string
}

// NewFirecrawlRenderer 构造渲染器; client 为 nil 时用直连客户端。
func NewFirecrawlRenderer(cfg RemoteRenderConfig, client *http.Client) *FirecrawlRenderer {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = "https://mcp.firecrawl.dev/v2/mcp"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if client == nil {
		client = fallbackDirectClient()
	}
	return &FirecrawlRenderer{
		endpoint: endpoint,
		apiKey:   strings.TrimSpace(cfg.APIKey),
		timeout:  timeout,
		client:   client,
		ua:       "claude-go-search/1.0",
	}
}

var errRemoteRenderNoMarkdown = errors.New("firecrawl returned empty markdown")

// Render 调 firecrawl_scrape 并解析 SSE 响应里的 markdown。
func (r *FirecrawlRenderer) Render(ctx context.Context, rawURL string) (*Document, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := guardRenderURL(urlGuard, rawURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "firecrawl_scrape",
			"arguments": map[string]any{
				"url":     u.String(),
				"formats": []string{"markdown"},
			},
		},
	})
	if err != nil {
		return nil, Wrap(remoteRenderVia, ErrInvalid, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, Wrap(remoteRenderVia, ErrInvalid, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", r.ua)
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, wrapFetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, Wrap(remoteRenderVia, statusKind(resp.StatusCode),
			fmt.Errorf("firecrawl status %d", resp.StatusCode))
	}

	payload, err := readStreamableMessage(resp.Body)
	if err != nil {
		return nil, err
	}
	doc, err := firecrawlDocument(payload, u.String())
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// firecrawlScrapeResult 是 firecrawl_scrape 内层 JSON 的字段子集。
type firecrawlScrapeResult struct {
	Markdown string `json:"markdown"`
	Metadata struct {
		Title     string `json:"title"`
		URL       string `json:"url"`
		Published string `json:"published"`
	} `json:"metadata"`
}

// firecrawlDocument 把 tools/call 的响应解析成 Document。
//
// 响应是两层 JSON: 外层 result.content[0].text 里再包一个 scrape 结果
// JSON。markdown 为空视为渲染无正文 (挑战页/空壳), 与本地渲染同一口径。
func firecrawlDocument(payload json.RawMessage, requestedURL string) (*Document, error) {
	var outer struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &outer); err != nil {
		return nil, Wrap(remoteRenderVia, ErrUnavailable, fmt.Errorf("decode firecrawl message: %w", err))
	}
	if outer.Error != nil && outer.Error.Message != "" {
		return nil, Wrap(remoteRenderVia, ErrUnavailable, errors.New(outer.Error.Message))
	}
	if len(outer.Result.Content) == 0 {
		return nil, Wrap(remoteRenderVia, ErrUnavailable, errors.New("firecrawl returned no content"))
	}
	var scraped firecrawlScrapeResult
	if err := json.Unmarshal([]byte(outer.Result.Content[0].Text), &scraped); err != nil {
		return nil, Wrap(remoteRenderVia, ErrUnavailable, fmt.Errorf("decode scrape result: %w", err))
	}
	markdown := strings.TrimSpace(normalizeMarkdown(scraped.Markdown))
	if markdown == "" {
		return nil, Wrap(remoteRenderVia, ErrUnavailable, errRemoteRenderNoMarkdown)
	}
	title := strings.TrimSpace(scraped.Metadata.Title)
	finalURL := strings.TrimSpace(scraped.Metadata.URL)
	if finalURL == "" {
		finalURL = requestedURL
	}
	var published time.Time
	if scraped.Metadata.Published != "" {
		for _, layout := range publishedLayouts {
			if t, err := time.Parse(layout, scraped.Metadata.Published); err == nil {
				published = t
				break
			}
		}
	}
	return &Document{
		URL:       finalURL,
		Title:     title,
		Markdown:  markdown,
		Published: published,
		FetchedAt: time.Now(),
		Hash:      hashDocument(markdown),
		Via:       remoteRenderVia,
	}, nil
}

// readStreamableMessage 从 MCP streamable-HTTP 响应里取第一个 JSON-RPC message。
//
// 该端点以 SSE 帧回包 (每帧 "event: message" + "data: {...}"), 单行 JSON
// 超长时会按 SSE 规范拆多行 data: —— 拼接 data 段而不是按行 JSON 解码。
// 响应体上限与 fetch 一致 (5MB), 防异常端点拖垮内存; 截断会在 JSON 解码处
// 失败, 不会静默产出半截正文。
func readStreamableMessage(r io.Reader) (json.RawMessage, error) {
	var data strings.Builder
	scanner := bufio.NewScanner(io.LimitReader(r, maxFetchBodyBytes))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	seenData := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		seenData = true
		if data.Len() > 0 {
			data.WriteString("\n")
		}
		data.WriteString(payload)
	}
	if err := scanner.Err(); err != nil {
		return nil, Wrap(remoteRenderVia, ErrUnavailable, err)
	}
	if !seenData {
		return nil, Wrap(remoteRenderVia, ErrUnavailable, errors.New("no SSE data frame in response"))
	}
	return json.RawMessage(data.String()), nil
}

// remoteRenderEligible 判断本地渲染失败后是否值得尝试远程渲染。
//
// ErrInvalid 不兜底 (URL 已被 urlGuard 拒绝, 远程侧同样会拒); 其余 —
// 浏览器缺失/超时/挑战页/代理形态不支持 —— 都是"本地环境够不着", 远程
// 渲染用的是 HTTP 出口, 与浏览器限制正交。
func remoteRenderEligible(err error) bool {
	switch KindOf(err) {
	case ErrInvalid:
		return false
	}
	return err != nil
}

var publishedLayouts = []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"}

// firecrawlRenderClient 返回远程渲染使用的出口客户端: auto = 代理健康走代理。
// 渲染端点是国际服务, 国内直连部署通常需要代理出口; 无代理时直连由部署
// 的出口逐源白名单兜底 (放不通自然超时, 不会静默走错路)。
func (m *Manager) firecrawlRenderClient() *http.Client {
	if m.egress != nil {
		return m.egress.Client(ModeAuto)
	}
	return fallbackDirectClient()
}

var bareDirectOnce = sync.OnceValue(func() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{Transport: tr}
})

// fallbackDirectClient 是不经环境代理的直连客户端 (与 providers 包同策略,
// 避免宿主 HTTP_PROXY 污染按源钉死的出口)。
func fallbackDirectClient() *http.Client {
	return bareDirectOnce()
}
