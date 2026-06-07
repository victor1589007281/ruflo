package wechat

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TypesetLocal 离线排版 (不调用任何公众号 API): mermaid 渲染成本地 PNG, 产出内联 HTML。
// 用于在 IP 白名单生效前先验证渲染/排版效果。imgRefBase 为空则用 file:// 绝对路径。
func TypesetLocal(ctx context.Context, md, chromePath, imgDir, imgRefBase string) (htmlOut string, count, ok int, warnings []string, err error) {
	if mkErr := os.MkdirAll(imgDir, 0o755); mkErr != nil {
		return "", 0, 0, nil, mkErr
	}
	md = TightenOrderedLists(md)
	md2, blocks := ExtractMermaid(md)
	urls := map[int]string{}
	codes := make([]string, len(blocks))
	for i, b := range blocks {
		codes[i] = b.Code
	}
	pngs, errs := RenderMermaidBatch(ctx, chromePath, codes, 45*time.Second, nil)
	for i, b := range blocks {
		if errs[i] != nil {
			warnings = append(warnings, fmt.Sprintf("mermaid#%d 渲染失败: %v", b.Index, errs[i]))
			continue
		}
		fname := fmt.Sprintf("mermaid-%d.png", b.Index)
		p := filepath.Join(imgDir, fname)
		if e := os.WriteFile(p, pngs[i], 0o644); e != nil {
			warnings = append(warnings, fmt.Sprintf("mermaid#%d 写盘失败: %v", b.Index, e))
			continue
		}
		if imgRefBase != "" {
			urls[b.Index] = strings.TrimRight(imgRefBase, "/") + "/" + fname
		} else {
			urls[b.Index] = "file://" + filepath.ToSlash(p)
		}
		ok++
	}
	h, e := MarkdownToHTML(md2)
	if e != nil {
		return "", len(blocks), ok, warnings, e
	}
	h = ReplaceMermaidPlaceholders(h, urls)
	h = InlineWechatStyles(h)
	return h, len(blocks), ok, warnings, nil
}

// TypesetOptions 排版选项。
type TypesetOptions struct {
	ChromePath string // 无头浏览器路径 (config.browser.chromePath)
	Title      string
	Author     string
	Digest     string // 摘要 (<=120 字)
	SourceURL  string // 原文链接
	// MermaidFixer 可选: 语法校验/渲染失败且启发式修复无效时, 调它(LLM)修复 mermaid。
	// 入参 (原始代码, 错误信息), 返回修正后的 mermaid (空串=放弃)。
	MermaidFixer func(code, errMsg string) string
}

// TypesetResult 排版产物。
type TypesetResult struct {
	ContentHTML  string         // 公众号安全内联 HTML (可直接作为草稿 content)
	MermaidCount int            // 检测到的 mermaid 图数
	MermaidOK    int            // 成功渲染+上传的图数
	ImgURLs      map[int]string // index -> 公众号图片 URL
	Warnings     []string
}

// Typeset 把 Markdown 排版成公众号安全内联 HTML; 期间把 mermaid 渲染成图并上传。
func (c *Client) Typeset(ctx context.Context, md string, opts TypesetOptions) (*TypesetResult, error) {
	res := &TypesetResult{ImgURLs: map[int]string{}}
	md = TightenOrderedLists(md) // 删除有序列表项之间的空行
	md2, blocks := ExtractMermaid(md)
	res.MermaidCount = len(blocks)

	codes := make([]string, len(blocks))
	for i, b := range blocks {
		codes[i] = b.Code
	}
	pngs, errs := RenderMermaidBatch(ctx, opts.ChromePath, codes, 45*time.Second, opts.MermaidFixer)
	for i, b := range blocks {
		if errs[i] != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("mermaid#%d 渲染失败: %v", b.Index, errs[i]))
			continue
		}
		if i > 0 {
			time.Sleep(1500 * time.Millisecond) // 放慢上传节奏, 规避 Tencent WAF 突发限流
		}
		u, err := c.UploadContentImage(pngs[i], fmt.Sprintf("mermaid-%d.png", b.Index))
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("mermaid#%d 上传失败: %v", b.Index, err))
			continue
		}
		res.ImgURLs[b.Index] = u
		res.MermaidOK++
	}

	h, err := MarkdownToHTML(md2)
	if err != nil {
		return nil, fmt.Errorf("markdown 渲染失败: %w", err)
	}
	h = ReplaceMermaidPlaceholders(h, res.ImgURLs)
	h = InlineWechatStyles(h)
	res.ContentHTML = h
	return res, nil
}

// buildArticle 排版 + 生成封面, 组装成 DraftArticle。
func (c *Client) buildArticle(ctx context.Context, md string, opts TypesetOptions) (DraftArticle, *TypesetResult, error) {
	res, err := c.Typeset(ctx, md, opts)
	if err != nil {
		return DraftArticle{}, res, err
	}
	thumbID, terr := c.makeCover(ctx, opts)
	if terr != nil {
		return DraftArticle{}, res, fmt.Errorf("封面生成/上传失败: %w", terr)
	}
	author := opts.Author
	if author == "" {
		author = c.cfg.Author
	}
	digest := opts.Digest
	if digest == "" {
		digest = plainExcerpt(res.ContentHTML, 100)
	}
	return DraftArticle{
		Title:            firstNonEmptyStr(opts.Title, "未命名文章"),
		Author:           author,
		Digest:           digest,
		Content:          res.ContentHTML,
		ContentSourceURL: opts.SourceURL,
		ThumbMediaID:     thumbID,
	}, res, nil
}

// PublishDraft 完整流程: 排版 → 生成封面 → 新建草稿, 返回草稿 media_id。
func (c *Client) PublishDraft(ctx context.Context, md string, opts TypesetOptions) (mediaID string, res *TypesetResult, err error) {
	a, res, err := c.buildArticle(ctx, md, opts)
	if err != nil {
		return "", res, err
	}
	mediaID, err = c.AddDraft(a)
	return mediaID, res, err
}

// UpdateDraftFromMarkdown 用新 Markdown 重排版并替换已有草稿, 返回新草稿 media_id。
//
// 注意: draft/update 端点会被 Tencent WAF 按内容(代码/SQL 关键词)拦成 501; 而 draft/add 不会。
// 故采用 add-new + delete-old 实现"更新", 规避 WAF。
func (c *Client) UpdateDraftFromMarkdown(ctx context.Context, oldMediaID, md string, opts TypesetOptions) (newMediaID string, res *TypesetResult, err error) {
	a, res, err := c.buildArticle(ctx, md, opts)
	if err != nil {
		return "", res, err
	}
	newMediaID, err = c.AddDraft(a)
	if err != nil {
		return "", res, err
	}
	if oldMediaID != "" {
		_ = c.DeleteDraft(oldMediaID) // 删旧草稿; 失败不致命
	}
	return newMediaID, res, nil
}

// makeCover 渲染一张标题封面卡片并上传为永久素材, 返回 thumb media_id。
func (c *Client) makeCover(ctx context.Context, opts TypesetOptions) (string, error) {
	page := coverHTML(firstNonEmptyStr(opts.Title, "技术解析"))
	var png []byte
	err := withBrowser(ctx, opts.ChromePath, func(allocCtx context.Context) error {
		b, e := renderElementPNG(allocCtx, page, "#cover", "#cover", 25*time.Second)
		png = b
		return e
	})
	if err != nil {
		return "", err
	}
	return c.AddImageThumb(png, "cover.png")
}

func coverHTML(title string) string {
	return `<!doctype html><html><head><meta charset="utf-8">
<style>html,body{margin:0;padding:0}
#cover{width:900px;height:383px;display:flex;align-items:center;justify-content:center;
background:linear-gradient(135deg,#0d3a6e,#1a5fb4 60%,#4a90d9);box-sizing:border-box;padding:48px}
#cover h1{color:#fff;font:700 44px/1.4 -apple-system,'PingFang SC','Microsoft YaHei',sans-serif;text-align:center;margin:0;text-shadow:0 2px 8px rgba(0,0,0,.25)}
</style></head><body><div id="cover"><h1>` + html.EscapeString(title) + `</h1></div></body></html>`
}

func plainExcerpt(htmlStr string, n int) string {
	// 极简去标签
	var b strings.Builder
	inTag := false
	for _, r := range htmlStr {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				b.WriteRune(r)
			}
		}
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	rs := []rune(s)
	if len(rs) > n {
		return string(rs[:n])
	}
	return s
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
