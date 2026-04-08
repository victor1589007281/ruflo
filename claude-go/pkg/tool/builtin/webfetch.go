// webfetch.go 实现 WebFetch 工具：拉取 URL 内容并转为可读纯文本。
// 对应 TS 源码: review/claude/src/tools/WebFetchTool/WebFetchTool.ts
//
// 设计要点:
//   - 只读、并发安全；在产品侧通常标记为 shouldDefer（网络出站需用户确认后再执行）。
//   - 本实现中 CheckPermissions 返回 nil，与 Glob/Grep 等只读工具一致；
//     外层权限/编排可单独对 WebFetch 加规则。
//
// 算法说明:
//  1. 校验 URL 必须为 http/https，拒绝 file: 等以避免误用与简单 SSRF 面。
//  2. 使用 net/http + context 超时发起 GET；限制响应体读取长度，避免内存爆炸。
//  3. 将 body 按 UTF-8 解码为字符串后截断至 maxWebFetchChars（字符数以 rune 计）。
//  4. 去除 <script>/<style> 块，再用正则去掉其余标签，空白折叠为单空格，得到近似“可读文本”。
//     （非完整 HTML 解析器，复杂页面可能残留实体或未闭合标签碎片，属简化实现。）
package builtin

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
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const WebFetchToolName = "WebFetch"

// maxWebFetchChars 与 TS 侧常见截断一致的量级：防止超大页面撑爆上下文。
const maxWebFetchChars = 50000

const webFetchHTTPTimeout = 60 * time.Second

// 预编译正则，避免每次 Call 重复编译。
var (
	webFetchReScript = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	webFetchReStyle  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	webFetchReTag    = regexp.MustCompile(`<[^>]+>`)
)

// webFetchInput 对应 LLM 传入的 JSON。
type webFetchInput struct {
	URL    string `json:"url"`
	Prompt string `json:"prompt,omitempty"`
}

// WebFetchTool HTTP 拉取并文本化网页内容的工具。
type WebFetchTool struct{}

// NewWebFetchTool 构造 WebFetch 工具实例。
func NewWebFetchTool() *WebFetchTool {
	return &WebFetchTool{}
}

func (t *WebFetchTool) Name() string { return WebFetchToolName }

func (t *WebFetchTool) Description() string {
	return `Fetches a URL via HTTP/HTTPS and returns a simplified plain-text extraction of the page (HTML tags stripped, truncated). Optional prompt can supply extra instructions for how to use the fetched content.`
}

func (t *WebFetchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "Absolute HTTP or HTTPS URL to fetch."
			},
			"prompt": {
				"type": "string",
				"description": "Optional context or instructions related to why this URL is being fetched."
			}
		},
		"required": ["url"]
	}`)
}

func (t *WebFetchTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *WebFetchTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 只读网络工具：不附加 Deny。
// shouldDefer 语义由上层编排/权限策略处理；此处返回 nil 表示工具自身不拒绝执行。
func (t *WebFetchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 执行 HTTP GET、截断与 HTML→文本简化提取。
func (t *WebFetchTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in webFetchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
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

	reqCtx, cancel := context.WithTimeout(ctx, webFetchHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("构造请求失败: %v", err), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "claude-go-WebFetch/1.0")

	client := &http.Client{Timeout: webFetchHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("请求失败: %v", err), IsError: true}, nil
	}
	defer resp.Body.Close()

	// 最多多读约 2*maxWebFetchChars 字节，在 UTF-8 边界上再按 rune 截断。
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
	if strings.TrimSpace(in.Prompt) != "" {
		fmt.Fprintf(&b, "（附加上下文 prompt）\n%s\n\n", strings.TrimSpace(in.Prompt))
	}
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
