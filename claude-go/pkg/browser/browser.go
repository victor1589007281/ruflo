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
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Headless:    true,
		Timeout:     30 * time.Second,
		UserAgent:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		WaitSeconds: 3,
	}
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

func extractMainText(html string) string {
	s := reScriptTag.ReplaceAllString(html, " ")
	s = reStyleTag.ReplaceAllString(s, " ")
	s = reNavTag.ReplaceAllString(s, " ")
	s = reHeaderTag.ReplaceAllString(s, " ")
	s = reFooterTag.ReplaceAllString(s, " ")
	s = reAsideTag.ReplaceAllString(s, " ")
	s = reIframeTag.ReplaceAllString(s, " ")
	s = reHTMLTags.ReplaceAllString(s, " ")
	s = reWhitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

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
