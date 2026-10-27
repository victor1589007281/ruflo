// webfetch.go 实现 WebFetch 工具：拉取 URL 内容并转为可读纯文本。
// 对应 TS 源码: review/claude/src/tools/WebFetchTool/WebFetchTool.ts
//
// 设计要点:
//   - 只读、并发安全；在产品侧通常标记为 shouldDefer（网络出站需用户确认后再执行）。
//   - 本实现中 CheckPermissions 返回 nil，与 Glob/Grep 等只读工具一致；
//     外层权限/编排可单独对 WebFetch 加规则。
//
// 算法说明:
//  1. 严格解析 url/prompt/max_content_tokens/offset，并拒绝未知字段。
//  2. 默认走搜索数据面：逐跳 SSRF 校验、全文缓存、相关原文窗口和 continuation。
//  3. 旧 fetcher/浏览器/裸 HTTP 只兼容简单抓取；高级参数在不支持时明确报错，
//     绝不靠回显 prompt 或忽略 offset 伪装成功。
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/search"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const WebFetchToolName = "WebFetch"

// maxWebFetchChars 单次抓取注入上下文的纯文本上限。
// 50000 字符 (~12.5K token) 对"背景调研"过大: 实测 tech-investigator 多次抓取整页,
// 每页都是一次 cache-miss 计费, 占团队 token 的 80%+。因此保留 16000 rune 默认上限；
// 收紧到 16000 字符，避免背景调研反复注入整页。max_content_tokens 是独立的
// UTF-8 字节上界预算，不再用 chars/4 假定不同语言的真实 token 消耗。
const maxWebFetchChars = 16000

const webFetchHTTPTimeout = 60 * time.Second

// 预编译正则，避免每次 Call 重复编译。
var (
	webFetchReScript = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	webFetchReStyle  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	webFetchReTag    = regexp.MustCompile(`<[^>]+>`)
)

// webFetchInput 对应 LLM 传入的 JSON。
type webFetchInput struct {
	URL              string `json:"url"`
	Prompt           string `json:"prompt,omitempty"`
	MaxContentTokens int    `json:"max_content_tokens,omitempty"`
	Offset           int    `json:"offset,omitempty"`
}

// BrowserClient 浏览器客户端接口（可选注入，增强抓取能力）。
type BrowserClient interface {
	Available() bool
	Fetch(ctx context.Context, url string) (title, text, html string, err error)
}

// PageFetcher 是搜索数据面的抓取接口 (SSRF 校验、出口切换、readability 提取)。
type PageFetcher interface {
	FetchJSON(ctx context.Context, url string, maxChars int) (string, error)
}

// WebFetchTool HTTP 拉取并文本化网页内容的工具。
type WebFetchTool struct {
	browser BrowserClient
	fetcher PageFetcher
}

// NewWebFetchTool 构造 WebFetch 工具实例。
//
// 默认接入进程级搜索数据面; 数据面未装配时退回本文件内置的裸 HTTP 实现。
func NewWebFetchTool() *WebFetchTool {
	t := &WebFetchTool{}
	if d := DefaultDataStack(); d != nil {
		t.fetcher = d.Search
	}
	return t
}

// SetBrowser 注入浏览器客户端，增强抓取能力 (stealth + JS 渲染)。
func (t *WebFetchTool) SetBrowser(b BrowserClient) {
	t.browser = b
}

// SetFetcher 注入搜索数据面的抓取实现 (优先于浏览器与内置 HTTP)。
func (t *WebFetchTool) SetFetcher(f PageFetcher) {
	t.fetcher = f
}

// Name 返回工具对外名。
func (t *WebFetchTool) Name() string { return WebFetchToolName }

// Description 明确 prompt 是选段依据，不是需要在网页上执行的指令。
func (t *WebFetchTool) Description() string {
	return `Fetches an HTTP/HTTPS URL and returns structured JSON with normalized Markdown, full-document hash, verbatim evidence and cache/degradation metadata. prompt selects a relevant contiguous original-text window; it is never executed as an instruction. max_content_tokens limits the returned excerpt using a conservative UTF-8 byte upper bound (not an estimate from another model's tokenizer). estimated_content_tokens reports that upper bound and token_count_method identifies it. offset and next_offset are rune positions for sequential continuation. When citing evidence, copy a quote from the same response whose offset/hash you report; other Markdown sentences are not evidence entries.`
}

// InputSchema 与真实 FetchRequest 对齐，offset 使用规范化全文的 rune 索引。
func (t *WebFetchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"url": {
				"type": "string",
				"description": "Absolute HTTP or HTTPS URL to fetch."
			},
			"prompt": {
				"type": "string",
				"description": "Question or topic used to select a relevant contiguous original-text window; not an instruction to execute."
			},
			"max_content_tokens": {
				"type": "integer",
				"minimum": 1,
				"description": "Optional returned-excerpt budget enforced with a conservative UTF-8 byte token upper bound."
			},
			"offset": {
				"type": "integer",
				"minimum": 0,
				"description": "Rune offset in the normalized full document; use a previous next_offset for continuation."
			}
		},
		"required": ["url"]
	}`)
}

// IsReadOnly 恒只读: 只发 GET, 不改远端状态。
func (t *WebFetchTool) IsReadOnly(_ json.RawMessage) bool { return true }

// IsConcurrencySafe 恒可并发: 只读且无共享可变状态。
func (t *WebFetchTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 只读网络工具：不附加 Deny。
// shouldDefer 语义由上层编排/权限策略处理；此处返回 nil 表示工具自身不拒绝执行。
func (t *WebFetchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 执行 HTTP GET、截断与 HTML→文本简化提取。
func (t *WebFetchTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in webFetchInput
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("WebFetch input error: %v", err), IsError: true}, nil
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return &tool.ToolResult{Content: fmt.Sprintf("WebFetch input error: %v", err), IsError: true}, nil
	}
	if in.MaxContentTokens < 0 || in.Offset < 0 {
		return &tool.ToolResult{Content: "WebFetch input error: max_content_tokens and offset must not be negative", IsError: true}, nil
	}
	if strings.TrimSpace(in.URL) == "" {
		return &tool.ToolResult{Content: "错误: url 不能为空", IsError: true}, nil
	}

	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return &tool.ToolResult{Content: fmt.Sprintf("错误: 无效的 URL: %q", in.URL), IsError: true}, nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return &tool.ToolResult{Content: fmt.Sprintf("错误: 仅支持 http/https，拒绝协议 %q", u.Scheme), IsError: true}, nil
	}

	// 路由 0: 搜索数据面 (SSRF 校验 + 出口切换 + 正文提取)。
	//
	// 数据面已装配时不再回落到下面的裸 HTTP: 那条路径没有逐跳重定向校验,
	// 也不能按站点切代理, 回退会把一个明确的 blocked/timeout 变成一次
	// 侥幸成功或一次更慢的失败。
	if t.fetcher != nil {
		req := search.FetchRequest{
			URL: u.String(), Prompt: strings.TrimSpace(in.Prompt), MaxChars: maxWebFetchChars,
			MaxContentTokens: in.MaxContentTokens, Offset: in.Offset,
		}
		if structured, ok := t.fetcher.(interface {
			FetchRequestJSON(context.Context, search.FetchRequest) (string, error)
		}); ok {
			out, ferr := structured.FetchRequestJSON(ctx, req)
			if ferr != nil {
				return &tool.ToolResult{Content: fmt.Sprintf("WebFetch error: %v", ferr), IsError: true}, nil
			}
			return &tool.ToolResult{Content: out}, nil
		}
		if req.Prompt != "" || req.MaxContentTokens != 0 || req.Offset != 0 {
			return &tool.ToolResult{Content: "WebFetch error: this legacy fetcher does not support prompt selection, token budgets or continuation", IsError: true}, nil
		}
		out, ferr := t.fetcher.FetchJSON(ctx, u.String(), maxWebFetchChars)
		if ferr != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("WebFetch error: %v", ferr), IsError: true}, nil
		}
		return &tool.ToolResult{Content: out}, nil
	}

	// 浏览器/裸 HTTP 兼容路径没有能力兑现高级契约，明确失败比只回显 prompt
	// 或忽略 offset 更安全；生产 search-mcp 默认走上面的结构化数据面。
	if strings.TrimSpace(in.Prompt) != "" || in.MaxContentTokens != 0 || in.Offset != 0 {
		return &tool.ToolResult{Content: "WebFetch error: prompt selection, token budgets and continuation require the search data plane", IsError: true}, nil
	}

	// 路由 1: 尝试浏览器抓取 (stealth + JS 渲染 + MediaWiki API 自动路由)
	if t.browser != nil && t.browser.Available() {
		bTitle, bText, _, bErr := t.browser.Fetch(ctx, u.String())
		if bErr == nil && strings.TrimSpace(bText) != "" {
			fullText := truncateRunes(bText, maxWebFetchChars)
			var b strings.Builder
			fmt.Fprintf(&b, "URL: %s\n", u.String())
			if bTitle != "" {
				fmt.Fprintf(&b, "Title: %s\n", bTitle)
			}
			b.WriteString("\n")
			b.WriteString(fullText)
			if utf8.RuneCountInString(bText) > maxWebFetchChars {
				fmt.Fprintf(&b, "\n\n[已截断: 提取后的纯文本超过 %d 个 Unicode 字符]", maxWebFetchChars)
			}
			return &tool.ToolResult{Content: b.String()}, nil
		}
		// 浏览器失败, 降级到 HTTP
	}

	// 路由 2: HTTP 直连 (兜底)
	reqCtx, cancel := context.WithTimeout(ctx, webFetchHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("构造请求失败: %v", err), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	client := &http.Client{Timeout: webFetchHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("请求失败: %v", err), IsError: true}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &tool.ToolResult{Content: fmt.Sprintf("WebFetch error: HTTP %d", resp.StatusCode), IsError: true}, nil
	}

	const byteCap = maxWebFetchChars * 4
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(byteCap)))
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("读取响应体失败: %v", err), IsError: true}, nil
	}

	raw := string(body)
	fullText := stripHTMLToPlain(raw)
	text := truncateRunes(fullText, maxWebFetchChars)

	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d\nURL: %s\n\n", resp.StatusCode, u.String())
	b.WriteString(text)

	if utf8.RuneCountInString(fullText) > maxWebFetchChars {
		fmt.Fprintf(&b, "\n\n[已截断: 提取后的纯文本超过 %d 个 Unicode 字符]", maxWebFetchChars)
	}

	return &tool.ToolResult{Content: b.String()}, nil
}

// stripHTMLToPlain 简化去标签：先删 script/style，再去其余标签，折叠空白。
func stripHTMLToPlain(s string) string {
	s = webFetchReScript.ReplaceAllString(s, " ")
	s = webFetchReStyle.ReplaceAllString(s, " ")
	s = webFetchReTag.ReplaceAllString(s, " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// truncateRunes 按 Unicode 标量值截断（与“字符数”语义一致）。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	var b strings.Builder
	b.Grow(max * utf8.UTFMax)
	n := 0
	for _, r := range s {
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
