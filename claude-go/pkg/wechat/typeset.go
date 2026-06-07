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
	md2, blocks := ExtractMermaid(md)
	urls := map[int]string{}
	codes := make([]string, len(blocks))
	for i, b := range blocks {
		codes[i] = b.Code
	}
	pngs, errs := RenderMermaidBatch(ctx, chromePath, codes, 45*time.Second)
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
	md2, blocks := ExtractMermaid(md)
	res.MermaidCount = len(blocks)

	codes := make([]string, len(blocks))
	for i, b := range blocks {
		codes[i] = b.Code
	}
	pngs, errs := RenderMermaidBatch(ctx, opts.ChromePath, codes, 45*time.Second)
	for i, b := range blocks {
		if errs[i] != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("mermaid#%d 渲染失败: %v", b.Index, errs[i]))
			continue
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

// PublishDraft 完整流程: 排版 → 生成封面 → 建草稿, 返回草稿 media_id。
func (c *Client) PublishDraft(ctx context.Context, md string, opts TypesetOptions) (mediaID string, res *TypesetResult, err error) {
	res, err = c.Typeset(ctx, md, opts)
	if err != nil {
		return "", res, err
	}
	// 封面 (草稿必填 thumb_media_id): 渲染标题卡片为 PNG → 永久素材
	thumbID, terr := c.makeCover(ctx, opts)
	if terr != nil {
		return "", res, fmt.Errorf("封面生成/上传失败: %w", terr)
	}
	author := opts.Author
	if author == "" {
		author = c.cfg.Author
	}
	digest := opts.Digest
	if digest == "" {
		digest = plainExcerpt(res.ContentHTML, 100)
	}
	mediaID, err = c.AddDraft(DraftArticle{
		Title:            firstNonEmptyStr(opts.Title, "未命名文章"),
		Author:           author,
		Digest:           digest,
		Content:          res.ContentHTML,
		ContentSourceURL: opts.SourceURL,
		ThumbMediaID:     thumbID,
	})
	return mediaID, res, err
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
