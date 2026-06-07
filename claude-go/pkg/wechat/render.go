package wechat

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	ghtml "github.com/yuin/goldmark/renderer/html"
)

// MermaidBlock 从 Markdown 抽取出来的一个 mermaid 图。
type MermaidBlock struct {
	Index int
	Code  string
}

var mermaidFence = regexp.MustCompile("(?s)```mermaid\\s*\\n(.*?)```")

// ExtractMermaid 把 markdown 里的 ```mermaid 代码块替换成占位符, 返回处理后的 md 和图列表。
// 占位符形如独立一行 @@WXMERMAID:N@@, 经 goldmark 后变成 <p>@@WXMERMAID:N@@</p>, 便于回填 <img>。
func ExtractMermaid(md string) (string, []MermaidBlock) {
	var blocks []MermaidBlock
	i := 0
	out := mermaidFence.ReplaceAllStringFunc(md, func(m string) string {
		sub := mermaidFence.FindStringSubmatch(m)
		code := ""
		if len(sub) > 1 {
			code = strings.TrimSpace(sub[1])
		}
		blocks = append(blocks, MermaidBlock{Index: i, Code: code})
		ph := fmt.Sprintf("\n\n@@WXMERMAID:%d@@\n\n", i)
		i++
		return ph
	})
	return out, blocks
}

// stripCodeFenceLang 去掉非 mermaid 代码块里残留的对公众号无意义的语言标注由 goldmark 处理即可。

// MarkdownToHTML 用 goldmark 把 markdown 渲染为基础 HTML (GFM: 表格/删除线/任务列表)。
func MarkdownToHTML(md string) (string, error) {
	gm := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(ghtml.WithUnsafe()), // 允许占位段落原样输出
	)
	var buf bytes.Buffer
	if err := gm.Convert([]byte(md), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// 公众号安全内联样式: 仅用受支持的属性 (color/background/font/margin/padding/border/text-align/line-height)。
var inlineStyles = map[string]string{
	"h1":         "font-size:22px;font-weight:700;color:#1a1a1a;margin:28px 0 16px;line-height:1.4;border-left:4px solid #1a5fb4;padding-left:12px;",
	"h2":         "font-size:19px;font-weight:700;color:#1a5fb4;margin:26px 0 14px;line-height:1.4;border-left:4px solid #1a5fb4;padding-left:10px;",
	"h3":         "font-size:17px;font-weight:600;color:#0d3a6e;margin:22px 0 12px;line-height:1.4;",
	"h4":         "font-size:15px;font-weight:600;color:#333;margin:18px 0 10px;",
	"p":          "font-size:15px;color:#3a3a3a;line-height:1.85;margin:14px 0;letter-spacing:0.3px;",
	"blockquote": "border-left:4px solid #ff6b35;background:#fff7f3;color:#5e5e5e;padding:12px 16px;margin:16px 0;border-radius:4px;font-size:14px;",
	"ul":         "margin:14px 0;padding-left:22px;color:#3a3a3a;font-size:15px;line-height:1.85;",
	"ol":         "margin:14px 0;padding-left:22px;color:#3a3a3a;font-size:15px;line-height:1.85;",
	"li":         "margin:6px 0;",
	"table":      "border-collapse:collapse;width:100%;margin:18px 0;font-size:14px;",
	"th":         "border:1px solid #d0d7de;background:#eef3f8;color:#1a5fb4;padding:8px 10px;text-align:left;font-weight:600;",
	"td":         "border:1px solid #d0d7de;padding:8px 10px;color:#3a3a3a;",
	"pre":        "background:#0d1117;color:#e6edf3;padding:14px 16px;border-radius:8px;overflow-x:auto;font-size:13px;line-height:1.6;margin:16px 0;",
	"code":       "background:#f0f4f8;color:#c7254e;padding:2px 6px;border-radius:4px;font-size:13px;",
	"strong":     "color:#1a1a1a;font-weight:700;",
	"a":          "color:#1a5fb4;text-decoration:none;",
	"hr":         "border:none;border-top:1px solid #e5e7eb;margin:24px 0;",
}

// pre>code 不重复加 code 的内联背景 (会和 pre 冲突), 单独处理。
var inlineTagRe = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for tag := range inlineStyles {
		m[tag] = regexp.MustCompile("<" + tag + "(\\s|>)")
	}
	return m
}()

// InlineWechatStyles 把公众号安全样式内联到各标签 (公众号会删 <style>/class, 只认内联 style)。
func InlineWechatStyles(html string) string {
	// 先处理 pre>code: 去掉内层 code 的样式 (用 pre 的)
	html = strings.ReplaceAll(html, "<pre><code", "<pre><code data-raw")
	for tag, style := range inlineStyles {
		if tag == "code" {
			continue // 单独处理, 避免覆盖 pre 内的 code
		}
		re := inlineTagRe[tag]
		html = re.ReplaceAllString(html, "<"+tag+" style=\""+style+"\"$1")
	}
	// 行内 code (非 pre 内): goldmark 输出 <code>...; pre 内的已标记 data-raw
	html = regexp.MustCompile("<code(?: )?>").ReplaceAllString(html, "<code style=\""+inlineStyles["code"]+"\">")
	html = strings.ReplaceAll(html, "<code data-raw", "<code")
	// 整体包一层 section 容器 (公众号常见做法)
	return "<section style=\"font-family:-apple-system,BlinkMacSystemFont,'PingFang SC','Microsoft YaHei',sans-serif;color:#3a3a3a;\">" + html + "</section>"
}

// ReplaceMermaidPlaceholders 把 <p>@@WXMERMAID:N@@</p> 替换为已上传图片的 <img>。
func ReplaceMermaidPlaceholders(html string, imgURLs map[int]string) string {
	re := regexp.MustCompile(`<p[^>]*>@@WXMERMAID:(\d+)@@</p>`)
	return re.ReplaceAllStringFunc(html, func(m string) string {
		sub := re.FindStringSubmatch(m)
		idx := 0
		fmt.Sscanf(sub[1], "%d", &idx)
		u, ok := imgURLs[idx]
		if !ok || u == "" {
			return "<p style=\"color:#999;font-size:13px;\">[图表渲染失败 #" + sub[1] + "]</p>"
		}
		return "<p style=\"text-align:center;margin:18px 0;\"><img src=\"" + u + "\" style=\"max-width:100%;border-radius:8px;\"/></p>"
	})
}
