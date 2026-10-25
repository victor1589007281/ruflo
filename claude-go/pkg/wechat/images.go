package wechat

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MarkdownImageResult 是「上传 Markdown 正文图片」这一步的产物。
type MarkdownImageResult struct {
	Markdown string            // 回填公众号 URL 后的 Markdown
	Total    int               // 检测到的图片数
	Uploaded int               // 成功上传数
	URLs     map[string]string // 原始 src -> 公众号图片 URL
	Warnings []string          // 失败但已跳过的图片说明
}

// mdImageRe 匹配行内图片语法 ![alt](src) / ![alt](<src>) / ![alt](src "title")。
// 参考式图片 (![alt][id]) 不处理: 公众号草稿只认最终 HTML, 参考式由上游先展开。
var mdImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(\s*(<[^>]*>|[^)\s]+)(?:\s+(?:"[^"]*"|'[^']*'))?\s*\)`)

// dataImageRe 匹配 data:image/...;base64,... 形式的内联图片。
var dataImageRe = regexp.MustCompile(`^data:image/[a-zA-Z0-9.+-]+;base64,(.*)$`)

// maxRemoteImageBytes 远程图片下载上限 (公众号正文图片本身也有大小限制)。
const maxRemoteImageBytes = 10 << 20

// UploadMarkdownImages 把 Markdown 正文里的图片上传到公众号 (media/uploadimg) 并回填 URL。
//
// 支持三类 src:
//   - http(s):// (含 // 开头的协议相对地址) —— 直接下载后上传;
//   - data:image/...;base64, —— 解码后上传;
//   - 本地路径 (绝对路径, 或相对 baseDir 的路径, 兼容 file:// 形式)。
//
// 只处理围栏代码块和行内代码之外的图片: 示例代码里的 ![](...) 不应触发网络请求。
// 单张失败只记录 warning 并保留原 src, 不中断整篇文章。
func (c *Client) UploadMarkdownImages(md, baseDir string) *MarkdownImageResult {
	res := &MarkdownImageResult{Markdown: md, URLs: map[string]string{}}

	out := transformOutsideCode(md, func(seg string) string {
		return mdImageRe.ReplaceAllStringFunc(seg, func(m string) string {
			sub := mdImageRe.FindStringSubmatch(m)
			if len(sub) < 3 {
				return m
			}
			res.Total++
			src := strings.TrimSpace(sub[2])
			src = strings.TrimPrefix(strings.TrimSuffix(src, ">"), "<")
			if src == "" {
				return m
			}
			if u, ok := res.URLs[src]; ok { // 同一张图重复出现, 复用上传结果
				return replaceImageSrc(m, sub[1], u)
			}
			data, filename, err := c.readImageSource(src, baseDir)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("图片 %s 读取失败: %v", truncate(src, 80), err))
				return m
			}
			if res.Uploaded > 0 {
				time.Sleep(1200 * time.Millisecond) // 放慢节奏, 规避 Tencent WAF 突发限流
			}
			u, err := c.UploadContentImage(data, filename)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("图片 %s 上传失败: %v", truncate(src, 80), err))
				return m
			}
			res.URLs[src] = u
			res.Uploaded++
			return replaceImageSrc(m, sub[1], u)
		})
	})
	res.Markdown = out
	return res
}

// replaceImageSrc 重建图片语法, 保留 alt 文本。
func replaceImageSrc(_ string, alt, newSrc string) string {
	return "![" + alt + "](" + newSrc + ")"
}

// readImageSource 把各种 src 读成字节, 并给出一个适合上传的文件名。
func (c *Client) readImageSource(src, baseDir string) ([]byte, string, error) {
	if m := dataImageRe.FindStringSubmatch(src); m != nil {
		b, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			return nil, "", fmt.Errorf("base64 解码失败: %w", err)
		}
		return b, "inline-" + fmt.Sprint(len(b)) + ".png", nil
	}
	lower := strings.ToLower(src)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(src, "//") {
		u := src
		if strings.HasPrefix(u, "//") {
			u = "https:" + u
		}
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.httpc.Do(req)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteImageBytes+1))
		if err != nil {
			return nil, "", err
		}
		if len(b) > maxRemoteImageBytes {
			return nil, "", fmt.Errorf("图片超过 %d 字节上限", maxRemoteImageBytes)
		}
		name := filenameFromPath(u)
		if r := resp.Header.Get("Content-Type"); extFromMime(r) != "" && filepath.Ext(name) == "" {
			name += extFromMime(r)
		}
		return b, name, nil
	}

	local := strings.TrimPrefix(src, "file://")
	if dec, err := url.PathUnescape(local); err == nil {
		local = dec
	}
	if !filepath.IsAbs(local) {
		local = filepath.Join(baseDir, local)
	}
	b, err := os.ReadFile(local)
	if err != nil {
		return nil, "", err
	}
	return b, filepath.Base(local), nil
}

// filenameFromPath 从 URL 路径取文件名, 去掉查询串/锚点。
func filenameFromPath(u string) string {
	if p, err := url.Parse(u); err == nil && p.Path != "" {
		u = p.Path
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	name := filepath.Base(u)
	if name == "." || name == "/" || name == "" {
		return "image.png"
	}
	return name
}

// extFromMime 把常见图片 Content-Type 映射成扩展名。
func extFromMime(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	switch ct {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	case "image/webp":
		return ".webp"
	}
	return ""
}

// transformOutsideCode 对 Markdown 中「围栏代码块、行内代码」以外的文本逐行应用 fn。
func transformOutsideCode(md string, fn func(string) string) string {
	lines := strings.SplitAfter(md, "\n")
	var b strings.Builder
	inFence := false
	fence := ""
	for _, ln := range lines {
		t := strings.TrimLeft(ln, " \t")
		if !inFence {
			if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
				inFence = true
				fence = t[:3]
				b.WriteString(ln)
				continue
			}
			b.WriteString(transformOutsideInlineCode(ln, fn))
			continue
		}
		b.WriteString(ln)
		if strings.HasPrefix(t, fence) {
			inFence = false
		}
	}
	return b.String()
}

// transformOutsideInlineCode 只对单行内反引号之外的片段应用 fn。
//
// 未闭合的反引号之后的内容按「代码」处理保持原样: 宁可少传一张图, 也不在代码示例里
// 发起网络请求 (代码片段常含示例 URL)。
func transformOutsideInlineCode(line string, fn func(string) string) string {
	var b strings.Builder
	rest := line
	for {
		i := strings.IndexByte(rest, '`')
		if i < 0 {
			b.WriteString(fn(rest))
			return b.String()
		}
		j := strings.IndexByte(rest[i+1:], '`')
		if j < 0 {
			b.WriteString(fn(rest[:i]))
			b.WriteString(rest[i:])
			return b.String()
		}
		b.WriteString(fn(rest[:i]))
		b.WriteString(rest[i : i+1+j+1])
		rest = rest[i+1+j+1:]
	}
}
