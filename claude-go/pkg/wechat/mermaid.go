package wechat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// mermaidHTML 构造页面: 先 mermaid.parse 校验语法, 通过才 render; 失败把错误写到 body[data-mmerr]。
func mermaidHTML(code string) string {
	cj, _ := json.Marshal(code)
	return `<!doctype html><html><head><meta charset="utf-8">
<script src="https://cdn.jsdelivr.net/npm/mermaid@10/dist/mermaid.min.js"></script>
<style>html,body{margin:0;padding:0;background:#ffffff}#out{display:inline-block;padding:18px;font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif}</style>
</head><body>
<div id="out"></div>
<script>
window.__code = ` + string(cj) + `;
mermaid.initialize({startOnLoad:false,theme:"default",securityLevel:"loose"});
(async function(){
  try { await mermaid.parse(window.__code); }
  catch(e){ document.body.setAttribute("data-mmerr","parse: "+e); return; }
  try {
    var r = await mermaid.render("g0", window.__code);
    document.getElementById("out").innerHTML = r.svg;
    // 高清: mermaid 默认给 svg 加 max-width 并按内容缩小, 截图会糊。
    // 放大矢量 svg 到至少 ~1200 逻辑像素宽 (矢量放大不失真), 再配合 3x DSF 截图 → 高 DPI 位图。
    var svg = document.querySelector('#out svg');
    if (svg) {
      svg.style.maxWidth = "none";
      var w = svg.getBoundingClientRect().width || 800;
      var target = Math.max(w, 1200);
      svg.style.width = target + "px";
      svg.removeAttribute("height");
      svg.style.height = "auto";
    }
  }
  catch(e){ document.body.setAttribute("data-mmerr","render: "+e); }
})();
</script>
</body></html>`
}

// validateAndFix: 用方括号标签加引号等启发式修复常见 mermaid 语法错误 (LLM 易漏引号)。
var sqBracketLabel = regexp.MustCompile(`\[([^\]\n]*?)\]`)

func repairMermaid(code string) string {
	code = strings.TrimSpace(code)
	code = strings.TrimPrefix(code, "mermaid\n")
	code = strings.ReplaceAll(code, "```", "")
	// 方括号节点标签内含 ()/:/,/; 等特殊字符且未加引号 → 自动加引号
	code = sqBracketLabel.ReplaceAllStringFunc(code, func(m string) string {
		inner := m[1 : len(m)-1]
		if strings.HasPrefix(inner, `"`) && strings.HasSuffix(inner, `"`) {
			return m
		}
		if strings.ContainsAny(inner, "()（）:：,，;；/<>") {
			return `["` + strings.ReplaceAll(inner, `"`, "") + `"]`
		}
		return m
	})
	// 全角箭头/分号等常见误用
	code = strings.ReplaceAll(code, "；", ";")
	// 失败块才会走到这里: 去掉 <br>/<br/> (stateDiagram 等的转移标签不支持 HTML, 会解析失败)
	code = brTagRe.ReplaceAllString(code, " ")
	// 转义裸的 "<" (如标签里的 "< 1秒"), 避免被当成 HTML 标签起始而解析失败。
	code = strings.ReplaceAll(code, "< ", "&lt; ")
	code = leadingLtDigit.ReplaceAllString(code, "&lt;$1")
	return code
}

var (
	leadingLtDigit = regexp.MustCompile(`<(\d)`)
	brTagRe        = regexp.MustCompile(`(?i)<br\s*/?>`)
)

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
		b, e := renderMermaidOne(allocCtx, code, timeout)
		out = b
		return e
	})
	return out, err
}

// RenderMermaidBatch 复用同一浏览器渲染多张 mermaid 图。语法校验不通过/渲染失败时:
// ①启发式修复 repairMermaid 重试; ②仍失败且 fixer!=nil 则调 LLM 修复再重试。
// fixer(原始代码, 错误信息) 返回修正后的 mermaid 代码 (空串表示放弃)。
func RenderMermaidBatch(ctx context.Context, chromePath string, codes []string, perTimeout time.Duration, fixer func(code, errMsg string) string) ([][]byte, []error) {
	pngs := make([][]byte, len(codes))
	errs := make([]error, len(codes))
	_ = withBrowser(ctx, chromePath, func(allocCtx context.Context) error {
		// 预热: 让 mermaid.js 完成首次加载, 后续走热路径
		_, _ = renderMermaidOne(allocCtx, "graph LR\n  A-->B", perTimeout)
		for i, code := range codes {
			pngs[i], errs[i] = renderMermaidOne(allocCtx, code, perTimeout)
		}
		for i := range codes {
			if errs[i] == nil {
				continue
			}
			// ① 启发式修复
			pngs[i], errs[i] = renderMermaidOne(allocCtx, repairMermaid(codes[i]), perTimeout+30*time.Second)
			// ② LLM 迭代修复 (把每次的新错误反馈给 LLM, 最多 3 轮)
			if fixer != nil {
				cur := codes[i]
				for pass := 0; pass < 3 && errs[i] != nil; pass++ {
					lf := strings.TrimSpace(fixer(cur, errs[i].Error()))
					if lf == "" || lf == cur {
						break
					}
					pngs[i], errs[i] = renderMermaidOne(allocCtx, lf, perTimeout+30*time.Second)
					cur = lf
				}
			}
		}
		return nil
	})
	return pngs, errs
}

// renderMermaidOne 渲染单张 mermaid: 轮询直到出现 svg 或语法/渲染错误; 截图 #out。
func renderMermaidOne(allocCtx context.Context, code string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = 40 * time.Second
	}
	tmp, err := os.CreateTemp("", "wxmermaid-*.html")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(mermaidHTML(code)); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()

	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()
	taskCtx, cancelTimeout := context.WithTimeout(taskCtx, timeout)
	defer cancelTimeout()

	var done bool
	var mmerr string
	var buf []byte
	err = chromedp.Run(taskCtx,
		emulation.SetDeviceMetricsOverride(1600, 1200, 3.0, false), // 3x DSF + 大视口, 高清不模糊
		chromedp.Navigate("file://"+filepath.ToSlash(tmpPath)),
		// 轮询: svg 出现 (成功) 或 body 标记了错误
		chromedp.Poll(`!!(document.querySelector('#out svg') || document.body.getAttribute('data-mmerr'))`, &done,
			chromedp.WithPollingTimeout(timeout-2*time.Second)),
		chromedp.Evaluate(`document.body.getAttribute('data-mmerr')||''`, &mmerr),
		chromedp.ActionFunc(func(ctx context.Context) error {
			if strings.TrimSpace(mmerr) != "" {
				return fmt.Errorf("mermaid 语法/渲染错误: %s", mmerr)
			}
			return nil
		}),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Screenshot("#out", &buf, chromedp.NodeVisible, chromedp.ByQuery),
	)
	if err != nil {
		return nil, err
	}
	return buf, nil
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
