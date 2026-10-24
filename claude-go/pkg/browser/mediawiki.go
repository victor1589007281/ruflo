package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// MediaWikiClient 通过 MediaWiki API 提取内容，避免页面抓取被拦截。
// 适用于 Wikipedia、Fandom、百度百科(有 API 的 wiki 系统)。
type MediaWikiClient struct {
	client *http.Client
}

// NewMediaWikiClient 构造 MediaWiki API 客户端, 内置 30 秒超时的 http.Client (每次请求各自计时)。
func NewMediaWikiClient() *MediaWikiClient {
	return &MediaWikiClient{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

var reWikiDomain = regexp.MustCompile(`(?i)(wikipedia\.org|wikimedia\.org|fandom\.com|mediawiki\.org|wiktionary\.org|wikiquote\.org|wikibooks\.org)`)

// IsMediaWiki 检测 URL 是否是 MediaWiki 站点。
func IsMediaWiki(rawURL string) bool {
	return reWikiDomain.MatchString(rawURL)
}

// Fetch 通过 MediaWiki API 获取文章内容。
//
// 算法解读：
//
//	原理：先用轻量的 action=query&prop=extracts 取纯文本, 失败再退到 action=parse 取渲染后的
//	      HTML 并用 extractMainText 抽正文 —— 两条路都走不通才报 all methods failed。
//	关键不变量：两条路径共用 parseWikiURL 解析出的同一组 (apiBase, pageTitle) —— 若各自解析,
//	      fallback 可能打到另一个页面, 拿到"成功但不对"的内容, 这种错最隐蔽。
//	代价与边界：失败路径要发两次 HTTP (extracts 的失败多来自该 wiki 未启用扩展或页面不存在,
//	      后者两条路都会失败); HTML 抽取的正文质量低于 extracts 纯文本, 会带进更多噪声。
//	设计权衡：不直接走 action=parse —— extracts 更干净也更省带宽, 但 Fandom 一类站点常未启用,
//	      所以保留 HTML 兜底而不是只留一条路。
func (m *MediaWikiClient) Fetch(ctx context.Context, rawURL string) (*FetchResult, error) {
	apiBase, pageTitle, err := parseWikiURL(rawURL)
	if err != nil {
		return nil, err
	}

	// 只有 extracts 失败才退 HTML: 成功即短路, 不为了"更全"再多抓一次。
	text, err := m.fetchExtract(ctx, apiBase, pageTitle)
	if err != nil {
		text, err = m.fetchParsedHTML(ctx, apiBase, pageTitle)
		if err != nil {
			return nil, fmt.Errorf("mediawiki: all methods failed: %w", err)
		}
	}

	return &FetchResult{
		URL:   rawURL,
		Title: strings.ReplaceAll(pageTitle, "_", " "),
		Text:  text,
	}, nil
}

// fetchExtract 使用 action=query&prop=extracts 获取纯文本。
func (m *MediaWikiClient) fetchExtract(ctx context.Context, apiBase, title string) (string, error) {
	params := url.Values{
		"action":      {"query"},
		"titles":      {title},
		"prop":        {"extracts"},
		"explaintext": {"1"},
		"format":      {"json"},
	}
	apiURL := apiBase + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "claude-go/1.0 (https://github.com/ruvnet/claude-flow)")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var result struct {
		Query struct {
			Pages map[string]struct {
				Title   string `json:"title"`
				Extract string `json:"extract"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	for id, page := range result.Query.Pages {
		if id == "-1" {
			return "", fmt.Errorf("page not found: %s", title)
		}
		if strings.TrimSpace(page.Extract) != "" {
			return page.Extract, nil
		}
	}
	return "", fmt.Errorf("empty extract for %s", title)
}

// fetchParsedHTML 使用 action=parse 获取渲染后的 HTML 再提取文本。
func (m *MediaWikiClient) fetchParsedHTML(ctx context.Context, apiBase, title string) (string, error) {
	params := url.Values{
		"action": {"parse"},
		"page":   {title},
		"prop":   {"text"},
		"format": {"json"},
	}
	apiURL := apiBase + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "claude-go/1.0 (https://github.com/ruvnet/claude-flow)")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var result struct {
		Parse struct {
			Title string `json:"title"`
			Text  struct {
				Content string `json:"*"`
			} `json:"text"`
		} `json:"parse"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	html := result.Parse.Text.Content
	if html == "" {
		return "", fmt.Errorf("empty parsed HTML for %s", title)
	}
	return extractMainText(html), nil
}

// parseWikiURL 从 Wikipedia URL 解析出 API 端点和页面标题。
func parseWikiURL(rawURL string) (apiBase, pageTitle string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}

	// https://en.wikipedia.org/wiki/Artificial_intelligence
	// → API: https://en.wikipedia.org/w/api.php
	// → Title: Artificial_intelligence
	path := u.Path
	if idx := strings.Index(path, "/wiki/"); idx >= 0 {
		pageTitle = path[idx+6:]
		pageTitle = strings.TrimSuffix(pageTitle, "/")
		apiBase = fmt.Sprintf("%s://%s/w/api.php", u.Scheme, u.Host)
		return apiBase, pageTitle, nil
	}

	// Fandom: https://xxx.fandom.com/wiki/PageName
	if strings.Contains(u.Host, "fandom.com") {
		if idx := strings.Index(path, "/wiki/"); idx >= 0 {
			pageTitle = path[idx+6:]
			apiBase = fmt.Sprintf("%s://%s/api.php", u.Scheme, u.Host)
			return apiBase, pageTitle, nil
		}
	}

	return "", "", fmt.Errorf("unsupported wiki URL format: %s", rawURL)
}
