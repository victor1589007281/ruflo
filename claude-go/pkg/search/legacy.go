package search

// legacy.go —— 旧 WebSearcher 接口到 Manager 的适配层。
//
// 历史调用方只认 Search(query) (title, text, html, err) 这一种形状; 新工具
// 走 SearchJSON/FetchJSON/ResearchJSON 以保留 URL、provider、降级与配额信息。
// 适配层只做格式转换, 不复制检索逻辑, 避免两套行为分叉。
import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// LegacySearcher 把 Manager 适配成 claude-go 既有的 WebSearcher 形状
// (Search 返回 title/text/html), 同时提供 JSON 版本供新工具使用。
//
// 旧接口只能承载文本, 会丢掉 URL 之外的 provider/时延/降级信息; 新的
// WebSearch/WebFetch 工具应优先调用 SearchJSON/FetchJSON。
type LegacySearcher struct {
	m *Manager
}

// NewLegacySearcher 包装一个 Manager; m 为 nil 时返回 nil, 调用方可据此
// 判定搜索能力未装配, 而不是拿到一个永远失败的对象。
func NewLegacySearcher(m *Manager) *LegacySearcher {
	if m == nil {
		return nil
	}
	return &LegacySearcher{m: m}
}

// Manager 返回底层管理器 (供指标/运维接口使用)。
func (l *LegacySearcher) Manager() *Manager { return l.m }

// Search 实现旧 WebSearcher 接口。
//
// 返回 text 为带 URL 的编号列表; 全部源失败时返回错误而不是空字符串,
// 避免上层把"搜索失败"误判成"没有结果"。
func (l *LegacySearcher) Search(ctx context.Context, query string) (title, text, html string, err error) {
	resp, err := l.m.Search(ctx, SearchRequest{Query: query})
	if err != nil {
		return "", "", "", err
	}
	return "Web search: " + query, formatResults(resp), "", nil
}

// SearchJSON 返回结构化检索结果, 供工具层直接交给模型。
func (l *LegacySearcher) SearchJSON(ctx context.Context, query string) (string, error) {
	req := SearchRequest{Query: query}
	resp, err := l.m.Search(ctx, req)
	if err != nil {
		return "", err
	}
	return mustJSON(struct {
		Query      string   `json:"query"`
		Results    []Result `json:"results"`
		Providers  []string `json:"providers,omitempty"`
		Degraded   []string `json:"degraded,omitempty"`
		CacheHit   bool     `json:"cache_hit"`
		GatePassed bool     `json:"gate_passed"`
		GateReason string   `json:"gate_reason,omitempty"`
		ElapsedMS  int64    `json:"elapsed_ms"`
		CostUSD    float64  `json:"cost_usd"`
	}{query, resp.Results, resp.Providers, resp.Degraded, resp.CacheHit, resp.GatePassed, resp.GateReason, resp.ElapsedMS, resp.CostUSD})
}

// SearchRequestJSON 是带参数的检索入口 (语言/时限/域偏好), 供 MCP 工具使用。
func (l *LegacySearcher) SearchRequestJSON(ctx context.Context, req SearchRequest) (string, error) {
	resp, err := l.m.Search(ctx, req)
	if err != nil {
		return "", err
	}
	return mustJSON(resp)
}

// FetchJSON 抓取并提取一个 URL 的正文。
func (l *LegacySearcher) FetchJSON(ctx context.Context, rawURL string, maxChars int) (string, error) {
	resp, err := l.m.Fetch(ctx, FetchRequest{URL: rawURL, MaxChars: maxChars})
	if err != nil {
		return "", err
	}
	return mustJSON(struct {
		URL       string    `json:"url"`
		Title     string    `json:"title,omitempty"`
		Byline    string    `json:"byline,omitempty"`
		Published time.Time `json:"published,omitempty"`
		FetchedAt time.Time `json:"fetched_at"`
		Markdown  string    `json:"markdown"`
		Truncated bool      `json:"truncated"`
		Via       string    `json:"via"`
		Hash      string    `json:"hash"`
		Degraded  []string  `json:"degraded,omitempty"`
		ElapsedMS int64     `json:"elapsed_ms"`
	}{resp.Document.URL, resp.Document.Title, resp.Document.Byline, resp.Document.Published,
		resp.Document.FetchedAt, resp.Document.Markdown, resp.Document.Truncated, resp.Document.Via,
		resp.Document.Hash, resp.Degraded, resp.ElapsedMS})
}

// ResearchJSON 执行有界研究并返回抽取式答案与逐字证据。
func (l *LegacySearcher) ResearchJSON(ctx context.Context, question string) (string, error) {
	res, err := l.m.Research(ctx, question, ResearchOptions{})
	if err != nil {
		return "", err
	}
	return mustJSON(res)
}

// formatResults 把检索响应排成模型友好的编号列表。
func formatResults(resp *SearchResponse) string {
	var b strings.Builder
	for i, r := range resp.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), r.URL)
		if s := strings.TrimSpace(r.Snippet); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
		if r.Provider != "" {
			fmt.Fprintf(&b, "   [source: %s]\n", r.Provider)
		}
	}
	if len(resp.Degraded) > 0 {
		fmt.Fprintf(&b, "\n[degraded: %s]", strings.Join(resp.Degraded, ", "))
	}
	return strings.TrimSpace(b.String())
}

// mustJSON 只用于已确定可序列化的结构, 失败说明代码有 bug。
func mustJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal search response: %w", err)
	}
	return string(b), nil
}
