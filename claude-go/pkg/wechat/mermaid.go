package wechat

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// mermaidHTML 构造一个加载 mermaid.js 并渲染单个图的页面。
func mermaidHTML(code string) string {
	return `<!doctype html><html><head><meta charset="utf-8">
<script src="https://cdn.jsdelivr.net/npm/mermaid@10/dist/mermaid.min.js"></script>
<style>html,body{margin:0;padding:0;background:#ffffff}#out{display:inline-block;padding:18px;font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif}</style>
</head><body>
<div id="out" class="mermaid">` + html.EscapeString(code) + `</div>
<script>mermaid.initialize({startOnLoad:true,theme:"default",securityLevel:"loose"});</script>
</body></html>`
}

// withBrowser 启动一个无头浏览器分配器, 在其中执行 fn (复用同一浏览器渲染多张图, 避免反复启动 chrome)。
func withBrowser(ctx context.Context, chromePath string, fn func(allocCtx context.Context) error) error {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoSandbox,
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
	)
	if chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}
	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()
	return fn(allocCtx)
}

// RenderMermaidPNG 用无头浏览器把一段 mermaid 代码渲染成 PNG (单张, 自带浏览器)。
func RenderMermaidPNG(ctx context.Context, chromePath, code string, timeout time.Duration) ([]byte, error) {
	var out []byte
	err := withBrowser(ctx, chromePath, func(allocCtx context.Context) error {
		b, e := renderElementPNG(allocCtx, mermaidHTML(code), "#out svg", "#out", timeout)
		out = b
		return e
	})
	return out, err
}

// RenderMermaidBatch 复用同一浏览器渲染多张 mermaid 图; 返回与 codes 等长的 PNG/错误切片。
func RenderMermaidBatch(ctx context.Context, chromePath string, codes []string, perTimeout time.Duration) ([][]byte, []error) {
	pngs := make([][]byte, len(codes))
	errs := make([]error, len(codes))
	_ = withBrowser(ctx, chromePath, func(allocCtx context.Context) error {
		for i, code := range codes {
			pngs[i], errs[i] = renderElementPNG(allocCtx, mermaidHTML(code), "#out svg", "#out", perTimeout)
		}
		return nil
	})
	return pngs, errs
}

// renderElementPNG 在给定分配器里新开一个标签, 加载 pageHTML, 等 waitSel 可见后截图 shotSel。
func renderElementPNG(allocCtx context.Context, pageHTML, waitSel, shotSel string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = 40 * time.Second
	}
	tmp, err := os.CreateTemp("", "wxrender-*.html")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(pageHTML); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()

	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()
	taskCtx, cancelTimeout := context.WithTimeout(taskCtx, timeout)
	defer cancelTimeout()

	var buf []byte
	err = chromedp.Run(taskCtx,
		emulation.SetDeviceMetricsOverride(1000, 800, 2.0, false), // 2x 高清
		chromedp.Navigate("file://"+filepath.ToSlash(tmpPath)),
		chromedp.WaitVisible(waitSel, chromedp.ByQuery),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Screenshot(shotSel, &buf, chromedp.NodeVisible, chromedp.ByQuery),
	)
	if err != nil {
		return nil, fmt.Errorf("渲染失败: %w", err)
	}
	return buf, nil
}
