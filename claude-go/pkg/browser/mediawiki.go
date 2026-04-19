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
func (m *MediaWikiClient) Fetch(ctx context.Context, rawURL string) (*FetchResult, error) {
	apiBase, pageTitle, err := parseWikiURL(rawURL)
	if err != nil {
		return nil, err
	}

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
