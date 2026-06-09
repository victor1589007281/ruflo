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

// listItemRe 匹配有序(1. / 1)) 或无序(- / * / +) 列表项。
var listItemRe = regexp.MustCompile(`^\s*(?:\d+[.)]|[-*+])\s`)

// LintLists 列表门禁: 删除"列表项之间/列表项与其缩进续行之间"的空行 (这种空行会让 markdown
// 渲染成 loose list 或被公众号当成段落间距 → 看起来像空行)。返回 (修复后文本, 删除的空行数)。
// 类似 golang lint: 既能检测(返回计数)也能自动修复。
func LintLists(md string) (string, int) {
	lines := strings.Split(md, "\n")
	keep := make([]bool, len(lines))
	for i := range keep {
		keep[i] = true
	}
	isListCtx := func(s string) bool {
		if listItemRe.MatchString(s) {
			return true
		}
		// 缩进续行(属于某个列表项的后续内容)
		return strings.HasPrefix(s, "  ") || strings.HasPrefix(s, "\t")
	}
	removed := 0
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			continue
		}
		p := i - 1
		for p >= 0 && strings.TrimSpace(lines[p]) == "" {
			p--
		}
		n := i + 1
		for n < len(lines) && strings.TrimSpace(lines[n]) == "" {
			n++
		}
		// 上一非空是列表项/续行, 且下一非空是列表项 → 该空行在列表内部, 删除
		if p >= 0 && n < len(lines) && isListCtx(lines[p]) && listItemRe.MatchString(lines[n]) {
			keep[i] = false
			removed++
		}
	}
	var out []string
	for i, l := range lines {
		if keep[i] {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n"), removed
}

// TightenOrderedLists 兼容旧调用点。
func TightenOrderedLists(md string) string {
	out, _ := LintLists(md)
	return out
}

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
	"ul":         "margin:12px 0;padding-left:22px;color:#3a3a3a;font-size:15px;line-height:1.7;",
	"ol":         "margin:12px 0;padding-left:22px;color:#3a3a3a;font-size:15px;line-height:1.7;",
	"li":         "margin:0;padding:0;", // 紧凑: 避免公众号给 li 加间距形成"空行"
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

var (
	codeBlockRe = regexp.MustCompile(`(?s)<pre[^>]*>\s*<code[^>]*>(.*?)</code>\s*</pre>`)
	inlineCodeRe = regexp.MustCompile(`<code(?:\s[^>]*)?>`)
	olBlockRe    = regexp.MustCompile(`(?s)<ol[^>]*>(.*?)</ol>`)
	ulBlockRe    = regexp.MustCompile(`(?s)<ul[^>]*>(.*?)</ul>`)
	liItemRe     = regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`)
)

// renderCodeBlock 把单个代码块改造成公众号能保留排版的形式。
// 关键: 公众号会**删掉 <pre> 里的 <br>、折叠换行**, 所以不能用 <pre>+<br>。
// 改为: 一个 <section> 容器, **每行一个 <p>**(公众号保留 <p> 块 → 必然换行), 空格→&nbsp; 保对齐。
func renderCodeBlock(m string) string {
	inner := strings.Trim(codeBlockRe.FindStringSubmatch(m)[1], "\n")
	var b strings.Builder
	b.WriteString(`<section style="background:#0d1117;border-radius:8px;padding:12px 14px;overflow-x:auto;margin:16px 0;">`)
	for _, ln := range strings.Split(inner, "\n") {
		esc := strings.ReplaceAll(ln, " ", "&nbsp;")
		if esc == "" {
			esc = "&nbsp;"
		}
		b.WriteString(`<p style="margin:0;padding:0;color:#e6edf3;font-size:12px;line-height:1.7;` +
			`white-space:nowrap;font-family:Consolas,Menlo,'Courier New',monospace;">` + esc + `</p>`)
	}
	b.WriteString(`</section>`)
	return b.String()
}

// listsToParagraphs 把 <ol>/<ul> 转成紧凑的带编号/项目符的 <p> (公众号对 <li> 会强加间距 →
// 直接不用列表标签, 改用 <p> + 手动编号, 彻底消除"序号列表空行")。仅处理扁平列表。
func listsToParagraphs(html string) string {
	const pStyle = "font-size:15px;color:#3a3a3a;line-height:1.75;margin:3px 0;"
	conv := func(body string, ordered bool) string {
		items := liItemRe.FindAllStringSubmatch(body, -1)
		var b strings.Builder
		for i, it := range items {
			marker := "• "
			if ordered {
				marker = fmt.Sprintf("%d. ", i+1)
			}
			b.WriteString(`<p style="` + pStyle + `"><strong style="color:#1a5fb4;">` + marker + `</strong>` +
				strings.TrimSpace(it[1]) + `</p>`)
		}
		return b.String()
	}
	html = olBlockRe.ReplaceAllStringFunc(html, func(m string) string {
		return conv(olBlockRe.FindStringSubmatch(m)[1], true)
	})
	html = ulBlockRe.ReplaceAllStringFunc(html, func(m string) string {
		return conv(ulBlockRe.FindStringSubmatch(m)[1], false)
	})
	return html
}

// InlineWechatStyles 把公众号安全样式内联到各标签 (公众号会删 <style>/class, 只认内联 style)。
func InlineWechatStyles(html string) string {
	// 1. 抽出代码块 → 占位符, 避免它的逐行 <p> 被后面的样式循环二次加样式
	var codeBlocks []string
	html = codeBlockRe.ReplaceAllStringFunc(html, func(m string) string {
		codeBlocks = append(codeBlocks, renderCodeBlock(m))
		return fmt.Sprintf("@@WXCODE:%d@@", len(codeBlocks)-1)
	})
	// 2. 普通标签内联样式
	for tag, style := range inlineStyles {
		if tag == "code" || tag == "pre" {
			continue
		}
		re := inlineTagRe[tag]
		html = re.ReplaceAllString(html, "<"+tag+" style=\""+style+"\"$1")
	}
	html = inlineCodeRe.ReplaceAllString(html, "<code style=\""+inlineStyles["code"]+"\">") // 行内 code
	html = listsToParagraphs(html)                                                          // ol/ul → 紧凑 <p>
	// 3. 还原代码块
	for i, cb := range codeBlocks {
		html = strings.ReplaceAll(html, fmt.Sprintf("@@WXCODE:%d@@", i), cb)
	}
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
