// Package browser 提供基于 Chrome CDP 的浏览器自动化能力。
// 用于网页内容提取（绕过防爬虫）、截图等。
// 不依赖第三方 CDP 库，通过 os/exec 启动 Chrome + WebSocket JSON 通信。
// 若系统无 Chrome，优雅降级到 net/http 简单抓取。
package browser

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Config 浏览器配置。
type Config struct {
	Headless     bool          `json:"headless"`
	Timeout      time.Duration `json:"timeout"`
	UserAgent    string        `json:"userAgent"`
	ProxyURL     string        `json:"proxyUrl,omitempty"`
	ChromePath   string        `json:"chromePath,omitempty"`
	WaitSeconds  int           `json:"waitSeconds"`
	// SearchEngine 搜索引擎 URL 模板，用 {query} 占位符。
	// 内置预设: "bing", "baidu"，也可传入完整模板如 "https://www.bing.com/search?q={query}"
	SearchEngine string `json:"searchEngine,omitempty"`
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Headless:     true,
		Timeout:      30 * time.Second,
		UserAgent:    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		WaitSeconds:  3,
		SearchEngine: "bing",
	}
}

// 内置搜索引擎 URL 模板。
var searchEngines = map[string]string{
	"bing":   "https://www.bing.com/search?q={query}&count=10",
	"baidu":  "https://www.baidu.com/s?wd={query}",
	"sogou":  "https://www.sogou.com/web?query={query}",
}

// resolveSearchURL 将搜索引擎配置解析为完整 URL。
func (c *Client) resolveSearchURL(query string) string {
	engine := c.cfg.SearchEngine
	// 先查内置预设
	if template, ok := searchEngines[engine]; ok {
		return strings.ReplaceAll(template, "{query}", url.QueryEscape(query))
	}
	// 如果用户传入的是完整 URL 模板
	if strings.HasPrefix(engine, "http") {
		return strings.ReplaceAll(engine, "{query}", url.QueryEscape(query))
	}
	// 兜底: Bing
	return strings.ReplaceAll(searchEngines["bing"], "{query}", url.QueryEscape(query))
}

// Search 使用配置的搜索引擎搜索 query 并返回结果。
func (c *Client) Search(ctx context.Context, query string) (*FetchResult, error) {
	searchURL := c.resolveSearchURL(query)
	return c.Fetch(ctx, searchURL)
}

// FetchResult 网页抓取结果。
type FetchResult struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Text  string `json:"text"`
	HTML  string `json:"html"`
}

// Client 浏览器客户端。
type Client struct {
	cfg    Config
	mu     sync.Mutex
	chrome string
}

// NewClient 创建浏览器客户端。
func NewClient(cfg Config) *Client {
	c := &Client{cfg: cfg}
	if cfg.ChromePath != "" {
		c.chrome = cfg.ChromePath
	} else {
		c.chrome = findChrome()
	}
	return c
}

// Available 检查是否有可用的 Chrome/Chromium。
func (c *Client) Available() bool {
	return c.chrome != ""
}

// Fetch 使用浏览器加载 URL 并提取主要文本内容。
// 若 Chrome 可用，用 headless Chrome；否则降级到 net/http。
func (c *Client) Fetch(ctx context.Context, url string) (*FetchResult, error) {
	if c.chrome != "" {
		return c.fetchWithChrome(ctx, url)
	}
	return c.fetchWithHTTP(ctx, url)
}

// fetchWithChrome 使用 headless Chrome 的 --dump-dom 模式。
func (c *Client) fetchWithChrome(ctx context.Context, url string) (*FetchResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	timeout := c.cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-blink-features=AutomationControlled",
		fmt.Sprintf("--user-agent=%s", c.cfg.UserAgent),
		"--dump-dom",
	}
	if c.cfg.ProxyURL != "" {
		args = append(args, fmt.Sprintf("--proxy-server=%s", c.cfg.ProxyURL))
	}
	if c.cfg.WaitSeconds > 0 {
		args = append(args, fmt.Sprintf("--virtual-time-budget=%d", c.cfg.WaitSeconds*1000))
	}
	args = append(args, url)

	cmd := exec.CommandContext(ctx, c.chrome, args...)
	cmd.Env = append(os.Environ(), "DISPLAY=:0")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("chrome fetch failed: %w", err)
	}

	htmlStr := string(output)
	title := extractTitle(htmlStr)
	text := extractMainText(htmlStr)

	return &FetchResult{
		URL:   url,
		Title: title,
		Text:  text,
		HTML:  htmlStr,
	}, nil
}

// fetchWithHTTP 降级: 使用 net/http 抓取（不支持 JS 渲染）。
func (c *Client) fetchWithHTTP(ctx context.Context, url string) (*FetchResult, error) {
	timeout := c.cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}

	htmlStr := string(body)
	return &FetchResult{
		URL:   url,
		Title: extractTitle(htmlStr),
		Text:  extractMainText(htmlStr),
		HTML:  htmlStr,
	}, nil
}

// Close 释放资源。
func (c *Client) Close() {}

// --- HTML 解析辅助 ---

var (
	reScriptTag  = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyleTag   = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reNavTag     = regexp.MustCompile(`(?is)<nav[^>]*>.*?</nav>`)
	reHeaderTag  = regexp.MustCompile(`(?is)<header[^>]*>.*?</header>`)
	reFooterTag  = regexp.MustCompile(`(?is)<footer[^>]*>.*?</footer>`)
	reAsideTag   = regexp.MustCompile(`(?is)<aside[^>]*>.*?</aside>`)
	reIframeTag  = regexp.MustCompile(`(?is)<iframe[^>]*>.*?</iframe>`)
	reHTMLTags   = regexp.MustCompile(`<[^>]+>`)
	reWhitespace = regexp.MustCompile(`\s+`)
	reTitleTag   = regexp.MustCompile(`(?is)<title[^>]*>([^<]*)</title>`)
)

func extractTitle(html string) string {
	m := reTitleTag.FindStringSubmatch(html)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// extractMainText 从 HTML 中提取正文 (v2: 文本密度分析算法)。
//
// 算法参考:
//   - Readability (Mozilla): 基于 DOM 节点评分的正文检测
//   - CETR (Weninger et al. 2010): Content Extraction via Tag Ratios
//   - Trafilatura (Barbaresi 2021): 结合启发式规则 + 信号分析
//
// 核心思路: 将 HTML 按块级标签分割为文本块,
// 计算每个块的"文本密度"(纯文本字符数 / 总字符数),
// 高密度连续区域即为正文，低密度区域为导航/广告/评论。
func extractMainText(html string) string {
	// Phase 1: 移除噪声元素 (script/style/nav/footer/aside/iframe + 已知噪声容器)
	s := reScriptTag.ReplaceAllString(html, "")
	s = reStyleTag.ReplaceAllString(s, "")
	s = reNavTag.ReplaceAllString(s, "")
	s = reHeaderTag.ReplaceAllString(s, "")
	s = reFooterTag.ReplaceAllString(s, "")
	s = reAsideTag.ReplaceAllString(s, "")
	s = reIframeTag.ReplaceAllString(s, "")
	s = reNoiseContainers.ReplaceAllString(s, "")

	// Phase 2: 尝试 <article> 或 role=main 精确提取 (优先级最高)
	if article := reArticleTag.FindStringSubmatch(s); len(article) > 1 {
		text := stripTags(article[1])
		if len([]rune(text)) > 200 {
			return text
		}
	}
	if mainRole := reMainRole.FindStringSubmatch(s); len(mainRole) > 1 {
		text := stripTags(mainRole[1])
		if len([]rune(text)) > 200 {
			return text
		}
	}

	// Phase 3: 文本密度分析 (CETR 算法简化版)
	blocks := reBlockTag.Split(s, -1)
	type textBlock struct {
		text    string
		density float64
	}
	var scored []textBlock
	for _, block := range blocks {
		raw := block
		plain := stripTags(raw)
		plainRunes := len([]rune(strings.TrimSpace(plain)))
		if plainRunes < 10 {
			scored = append(scored, textBlock{"", 0})
			continue
		}
		rawLen := len([]rune(raw))
		if rawLen == 0 {
			rawLen = 1
		}
		density := float64(plainRunes) / float64(rawLen)
		// 链接密度惩罚: 如果块内大量文本在 <a> 标签内，降低密度分
		linkText := stripTags(reAnchorContent.FindString(raw))
		linkRatio := float64(len([]rune(linkText))) / float64(plainRunes+1)
		if linkRatio > 0.5 {
			density *= 0.3 // 链接占比 >50% 的块很可能是导航
		}
		scored = append(scored, textBlock{plain, density})
	}

	// Phase 4: 滑动窗口找最大密度连续区间 (类 Kadane 算法)
	threshold := 0.25
	var bestStart, bestEnd int
	var bestScore float64
	for i := range scored {
		if scored[i].density < threshold {
			continue
		}
		sum := 0.0
		for j := i; j < len(scored) && j < i+50; j++ {
			if scored[j].density >= threshold {
				sum += scored[j].density
			} else {
				sum -= 0.2 // 低密度块的惩罚 (允许少量穿插)
			}
			if sum > bestScore {
				bestScore = sum
				bestStart = i
				bestEnd = j + 1
			}
		}
	}

	if bestEnd > bestStart {
		var result strings.Builder
		for i := bestStart; i < bestEnd && i < len(scored); i++ {
			if scored[i].text != "" {
				result.WriteString(strings.TrimSpace(scored[i].text))
				result.WriteString("\n\n")
			}
		}
		text := strings.TrimSpace(result.String())
		if len([]rune(text)) > 100 {
			return text
		}
	}

	// Phase 5: 降级 — 原始全文剥离 (兜底)
	return stripTags(s)
}

// stripTags 剥离所有 HTML 标签并合并空白。
func stripTags(s string) string {
	s = reHTMLTags.ReplaceAllString(s, " ")
	s = reWhitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var (
	// 噪声容器: 评论区、推荐、侧边栏、广告等常见 class/id 模式
	reNoiseContainers = regexp.MustCompile(`(?is)<(?:div|section)[^>]*(?:class|id)\s*=\s*"[^"]*(?:comment|recommend|sidebar|related|trending|hotlist|ad-|advertisement|footer-widget|social-share|breadcrumb)[^"]*"[^>]*>.*?</(?:div|section)>`)
	// <article> 语义标签 — 最可靠的正文信号
	reArticleTag = regexp.MustCompile(`(?is)<article[^>]*>(.*?)</article>`)
	// role="main" 语义属性
	reMainRole = regexp.MustCompile(`(?is)<[^>]*role\s*=\s*"main"[^>]*>(.*?)</[^>]+>`)
	// 块级标签用于分割文本块
	reBlockTag = regexp.MustCompile(`(?i)</?(?:div|p|section|article|blockquote|h[1-6]|ul|ol|li|table|tr|td|th|dl|dd|dt|pre|hr|br)[^>]*>`)
	// 锚点内容 (用于计算链接密度)
	reAnchorContent = regexp.MustCompile(`(?is)<a[^>]*>.*?</a>`)
)

// findChrome 查找系统中的 Chrome/Chromium 可执行文件。
func findChrome() string {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/snap/bin/chromium",
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	if path, err := exec.LookPath("google-chrome"); err == nil {
		return path
	}
	if path, err := exec.LookPath("chromium"); err == nil {
		return path
	}
	log.Printf("[browser] 未找到 Chrome/Chromium，将降级到 HTTP 抓取")
	return ""
}
