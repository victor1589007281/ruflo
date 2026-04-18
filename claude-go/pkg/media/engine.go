// 媒体输出引擎: 将 HTML/SVG 内容转换为 PNG/PDF/MP4/PPTX 等格式。
//
// 架构参考:
//   - HyperFrames: CDP BeginFrame + FFmpeg 确定性帧捕获
//   - Remotion: 帧序列 → FFmpeg 编码
//   - chromedp: Go 原生 CDP 客户端
//
// 依赖:
//   - chromedp (Go CDP): 截图、PDF 渲染
//   - FFmpeg (外部二进制): 视频编码
//   - Chrome/Chromium (外部): 浏览器渲染
package media

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Engine 媒体输出引擎
type Engine struct {
	OutputDir string
	Timeout   time.Duration
	Width     int
	Height    int
}

// NewEngine 创建媒体引擎
func NewEngine(outputDir string) *Engine {
	return &Engine{
		OutputDir: outputDir,
		Timeout:   2 * time.Minute,
		Width:     1920,
		Height:    1080,
	}
}

// RenderResult 渲染结果
type RenderResult struct {
	Format   string
	FilePath string
	Size     int64
	Duration time.Duration
	Error    string
}

// RenderAll 将 HTML 内容渲染为所有请求的格式
func (e *Engine) RenderAll(ctx context.Context, htmlContent string, name string, formats []string) []RenderResult {
	if err := os.MkdirAll(e.OutputDir, 0755); err != nil {
		return []RenderResult{{Error: fmt.Sprintf("创建输出目录失败: %v", err)}}
	}

	htmlPath := filepath.Join(e.OutputDir, name+".html")
	if err := os.WriteFile(htmlPath, []byte(htmlContent), 0644); err != nil {
		return []RenderResult{{Error: fmt.Sprintf("写入 HTML 失败: %v", err)}}
	}

	var results []RenderResult
	for _, format := range formats {
		start := time.Now()
		var r RenderResult
		switch strings.ToLower(format) {
		case "png", "jpeg", "jpg":
			r = e.renderImage(ctx, htmlPath, name, format)
		case "pdf":
			r = e.renderPDF(ctx, htmlPath, name)
		case "svg":
			r = e.extractSVG(htmlContent, name)
		case "mp4":
			r = e.renderVideo(ctx, htmlPath, name)
		case "pptx":
			r = e.renderPPTX(ctx, htmlContent, name)
		default:
			r = RenderResult{Format: format, Error: fmt.Sprintf("不支持的格式: %s", format)}
		}
		r.Duration = time.Since(start)
		results = append(results, r)
	}

	return results
}

// newBrowserCtx 创建 chromedp 浏览器上下文 (复用 allocator 配置)
func (e *Engine) newBrowserCtx(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(e.Width, e.Height),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-web-security", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	timedCtx, timedCancel := context.WithTimeout(taskCtx, timeout)

	cancel := func() {
		timedCancel()
		taskCancel()
		allocCancel()
	}
	return timedCtx, cancel
}

// renderImage 使用 chromedp 将 HTML 渲染为 PNG/JPEG
func (e *Engine) renderImage(ctx context.Context, htmlPath, name, format string) RenderResult {
	outPath := filepath.Join(e.OutputDir, name+"."+format)

	taskCtx, cancel := e.newBrowserCtx(ctx, e.Timeout)
	defer cancel()

	fileURL := "file://" + htmlPath
	var buf []byte

	quality := 90
	screenshotFmt := page.CaptureScreenshotFormatPng
	if format == "jpeg" || format == "jpg" {
		screenshotFmt = page.CaptureScreenshotFormatJpeg
	}

	err := chromedp.Run(taskCtx,
		chromedp.Navigate(fileURL),
		chromedp.EmulateViewport(int64(e.Width), int64(e.Height)),
		chromedp.WaitReady("body"),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			buf, err = page.CaptureScreenshot().
				WithFormat(screenshotFmt).
				WithQuality(int64(quality)).
				WithCaptureBeyondViewport(true).
				Do(ctx)
			return err
		}),
	)
	if err != nil {
		return RenderResult{Format: format, Error: fmt.Sprintf("截图失败: %v", err)}
	}

	if err := os.WriteFile(outPath, buf, 0644); err != nil {
		return RenderResult{Format: format, Error: fmt.Sprintf("写入图片失败: %v", err)}
	}

	size := fileSize(outPath)
	log.Printf("[media] 渲染 %s: %s (%.1f KB)", format, outPath, float64(size)/1024)
	return RenderResult{Format: format, FilePath: outPath, Size: size}
}

// renderPDF 使用 chromedp PrintToPDF 将 HTML 渲染为 PDF
func (e *Engine) renderPDF(ctx context.Context, htmlPath, name string) RenderResult {
	outPath := filepath.Join(e.OutputDir, name+".pdf")

	taskCtx, cancel := e.newBrowserCtx(ctx, e.Timeout)
	defer cancel()

	fileURL := "file://" + htmlPath
	var buf []byte

	err := chromedp.Run(taskCtx,
		chromedp.Navigate(fileURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			buf, _, err = page.PrintToPDF().
				WithPrintBackground(true).
				WithPreferCSSPageSize(true).
				Do(ctx)
			return err
		}),
	)
	if err != nil {
		return RenderResult{Format: "pdf", Error: fmt.Sprintf("PDF 渲染失败: %v", err)}
	}

	if err := os.WriteFile(outPath, buf, 0644); err != nil {
		return RenderResult{Format: "pdf", Error: fmt.Sprintf("写入 PDF 失败: %v", err)}
	}

	size := fileSize(outPath)
	log.Printf("[media] 渲染 PDF: %s (%.1f KB)", outPath, float64(size)/1024)
	return RenderResult{Format: "pdf", FilePath: outPath, Size: size}
}

// extractSVG 从 HTML 内容中提取 SVG
func (e *Engine) extractSVG(htmlContent, name string) RenderResult {
	outPath := filepath.Join(e.OutputDir, name+".svg")

	svgRe := regexp.MustCompile(`(?s)<svg[^>]*>.*?</svg>`)
	match := svgRe.FindString(htmlContent)
	if match == "" {
		return RenderResult{Format: "svg", Error: "HTML 中未找到 SVG 内容"}
	}

	if !strings.Contains(match, "xmlns") {
		match = strings.Replace(match, "<svg", `<svg xmlns="http://www.w3.org/2000/svg"`, 1)
	}

	if err := os.WriteFile(outPath, []byte(match), 0644); err != nil {
		return RenderResult{Format: "svg", Error: fmt.Sprintf("写入 SVG 失败: %v", err)}
	}

	size := fileSize(outPath)
	log.Printf("[media] 提取 SVG: %s (%.1f KB)", outPath, float64(size)/1024)
	return RenderResult{Format: "svg", FilePath: outPath, Size: size}
}

// renderVideo 使用 chromedp 逐帧截图 + FFmpeg 合成 MP4。
//
// 修复要点:
//   1. 不依赖 getAnimations() — 很多 CSS 动画不暴露到该 API
//   2. 用 CSS 时间控制: animation-play-state: paused + animation-delay 偏移
//   3. 降低 fps 到 15 减少帧数 (5s@15fps = 75帧, 可接受)
//   4. 如果没有真正动画, 生成一个带渐入效果的静态视频
func (e *Engine) renderVideo(ctx context.Context, htmlPath, name string) RenderResult {
	outPath := filepath.Join(e.OutputDir, name+".mp4")
	framesDir := filepath.Join(e.OutputDir, name+"_frames")
	if err := os.MkdirAll(framesDir, 0755); err != nil {
		return RenderResult{Format: "mp4", Error: fmt.Sprintf("创建帧目录失败: %v", err)}
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return RenderResult{Format: "mp4", Error: "ffmpeg 未安装, 无法生成视频"}
	}

	taskCtx, cancel := e.newBrowserCtx(ctx, 5*time.Minute)
	defer cancel()

	fileURL := "file://" + htmlPath
	fps := 15
	durationSec := 5
	totalFrames := fps * durationSec

	// 导航并检测动画
	var hasRealAnimation bool
	err := chromedp.Run(taskCtx,
		chromedp.Navigate(fileURL),
		emulation.SetDeviceMetricsOverride(int64(e.Width), int64(e.Height), 1.0, false),
		chromedp.WaitReady("body"),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Evaluate(`(function() {
			var anims = document.getAnimations ? document.getAnimations() : [];
			if (anims.length > 0) return true;
			var styles = document.querySelectorAll('style');
			for (var i = 0; i < styles.length; i++) {
				if (styles[i].textContent.indexOf('@keyframes') >= 0) return true;
			}
			var allEls = document.querySelectorAll('*');
			for (var j = 0; j < allEls.length; j++) {
				var cs = getComputedStyle(allEls[j]);
				if (cs.animationName && cs.animationName !== 'none') return true;
				if (cs.transition && cs.transition !== 'all 0s ease 0s' && cs.transition !== 'none') return true;
			}
			return false;
		})()`, &hasRealAnimation),
	)
	if err != nil {
		return RenderResult{Format: "mp4", Error: fmt.Sprintf("导航失败: %v", err)}
	}

	log.Printf("[media] 视频: hasAnimation=%v, %d帧 (%ds@%dfps)", hasRealAnimation, totalFrames, durationSec, fps)

	capturedFrames := 0
	for i := 0; i < totalFrames; i++ {
		framePath := filepath.Join(framesDir, fmt.Sprintf("frame_%06d.png", i))
		progress := float64(i) / float64(totalFrames)

		var jsCode string
		if hasRealAnimation {
			// 对于有 CSS 动画的页面: 让动画自然播放, 用定时截图
			jsCode = "" // 不注入 JS, 让动画自然运行
		} else {
			// 对于静态页面: 注入渐入 + 平移效果, 制造视觉动感
			opacity := progress * 1.2
			if opacity > 1 {
				opacity = 1
			}
			translateY := (1 - progress) * 20
			jsCode = fmt.Sprintf(`(function() {
				document.body.style.opacity = '%f';
				document.body.style.transform = 'translateY(%fpx)';
				document.body.style.transition = 'none';
			})()`, opacity, translateY)
		}

		actions := []chromedp.Action{}
		if jsCode != "" {
			actions = append(actions, chromedp.Evaluate(jsCode, nil))
		}

		if hasRealAnimation {
			// 每帧间隔 = 1/fps 秒, 让动画自然播放
			actions = append(actions, chromedp.Sleep(time.Duration(1000/fps)*time.Millisecond))
		} else {
			actions = append(actions, chromedp.Sleep(30*time.Millisecond))
		}

		actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
			buf, err := page.CaptureScreenshot().
				WithFormat(page.CaptureScreenshotFormatPng).
				Do(ctx)
			if err != nil {
				return err
			}
			return os.WriteFile(framePath, buf, 0644)
		}))

		if err := chromedp.Run(taskCtx, actions...); err != nil {
			log.Printf("[media] 帧 %d 截图失败: %v", i, err)
			continue
		}
		capturedFrames++

		if i%15 == 0 {
			log.Printf("[media] 帧进度: %d/%d (%.0f%%)", i, totalFrames, progress*100)
		}
	}

	if capturedFrames == 0 {
		return RenderResult{Format: "mp4", Error: "未能捕获任何帧"}
	}

	log.Printf("[media] 帧捕获完成: %d/%d 帧", capturedFrames, totalFrames)

	// FFmpeg 编码
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", filepath.Join(framesDir, "frame_%06d.png"),
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-preset", "fast",
		"-crf", "25",
		"-movflags", "+faststart",
		outPath,
	)
	ffmpegOut, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[media] FFmpeg stderr: %s", string(ffmpegOut))
		return RenderResult{Format: "mp4", Error: fmt.Sprintf("FFmpeg 编码失败: %v", err)}
	}

	// 清理帧文件
	os.RemoveAll(framesDir)

	size := fileSize(outPath)
	log.Printf("[media] 渲染视频: %s (%.1f KB, %ds)", outPath, float64(size)/1024, durationSec)
	return RenderResult{Format: "mp4", FilePath: outPath, Size: size}
}

// renderPPTX 将 HTML 幻灯片渲染为 PPTX (截图方式)
func (e *Engine) renderPPTX(ctx context.Context, htmlContent, name string) RenderResult {
	outDir := filepath.Join(e.OutputDir, name+"_slides")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return RenderResult{Format: "pptx", Error: fmt.Sprintf("创建幻灯片目录失败: %v", err)}
	}

	slides := splitSlides(htmlContent)
	if len(slides) == 0 {
		slides = []string{htmlContent}
	}

	taskCtx, cancel := e.newBrowserCtx(ctx, e.Timeout)
	defer cancel()

	var slidePaths []string
	for i, slide := range slides {
		slideHTML := wrapSlideHTML(slide, i+1, len(slides))
		slidePath := filepath.Join(outDir, fmt.Sprintf("slide_%03d.html", i+1))
		if err := os.WriteFile(slidePath, []byte(slideHTML), 0644); err != nil {
			continue
		}

		pngPath := filepath.Join(outDir, fmt.Sprintf("slide_%03d.png", i+1))
		fileURL := "file://" + slidePath

		err := chromedp.Run(taskCtx,
			chromedp.Navigate(fileURL),
			chromedp.WaitReady("body"),
			chromedp.Sleep(300*time.Millisecond),
			chromedp.ActionFunc(func(ctx context.Context) error {
				buf, err := page.CaptureScreenshot().
					WithFormat(page.CaptureScreenshotFormatPng).
					WithCaptureBeyondViewport(false).
					Do(ctx)
				if err != nil {
					return err
				}
				return os.WriteFile(pngPath, buf, 0644)
			}),
		)
		if err != nil {
			log.Printf("[media] 幻灯片 %d 截图失败: %v", i+1, err)
			continue
		}

		slidePaths = append(slidePaths, pngPath)
		log.Printf("[media] 幻灯片 %d/%d 已渲染", i+1, len(slides))
	}

	pptxPath := filepath.Join(e.OutputDir, name+".pptx")
	if err := buildSimplePPTX(pptxPath, slidePaths); err != nil {
		log.Printf("[media] PPTX 生成失败 (%v), 回退为 PDF", err)
		pdfResult := e.renderPDF(ctx, filepath.Join(outDir, "slide_001.html"), name+"_slides")
		if pdfResult.Error == "" {
			return pdfResult
		}
		return RenderResult{Format: "pptx", Error: fmt.Sprintf("PPTX+PDF 均失败: %v", err)}
	}

	size := fileSize(pptxPath)
	log.Printf("[media] 渲染 PPTX: %s (%d 页, %.1f KB)", pptxPath, len(slidePaths), float64(size)/1024)
	return RenderResult{Format: "pptx", FilePath: pptxPath, Size: size}
}

// RenderHTMLString 渲染 HTML 字符串为指定格式 (简化接口)
func (e *Engine) RenderHTMLString(ctx context.Context, html, name, format string) RenderResult {
	results := e.RenderAll(ctx, html, name, []string{format})
	if len(results) > 0 {
		return results[0]
	}
	return RenderResult{Format: format, Error: "渲染失败: 无结果"}
}

// HasChrome 检查是否有可用的 Chrome
func HasChrome() bool {
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return true // chromedp 会自动下载
}

// HasFFmpeg 检查 FFmpeg 是否可用
func HasFFmpeg() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

func fileSize(path string) int64 {
	info, _ := os.Stat(path)
	if info != nil {
		return info.Size()
	}
	return 0
}

func splitSlides(html string) []string {
	sectionRe := regexp.MustCompile(`(?si)<section[^>]*>(.*?)</section>`)
	matches := sectionRe.FindAllStringSubmatch(html, -1)
	if len(matches) > 1 {
		var slides []string
		for _, m := range matches {
			if len(m) > 1 && strings.TrimSpace(m[1]) != "" {
				slides = append(slides, m[0])
			}
		}
		if len(slides) > 0 {
			return slides
		}
	}

	slideRe := regexp.MustCompile(`(?si)<div[^>]*class="[^"]*slide[^"]*"[^>]*>(.*?)</div>`)
	matches = slideRe.FindAllStringSubmatch(html, -1)
	if len(matches) > 1 {
		var slides []string
		for _, m := range matches {
			if len(m) > 0 {
				slides = append(slides, m[0])
			}
		}
		return slides
	}

	return nil
}

func wrapSlideHTML(content string, num, total int) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
html, body { width: 1920px; height: 1080px; overflow: hidden; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "PingFang SC", "Microsoft YaHei", sans-serif; }
</style>
</head><body>
%s
</body></html>`, content)
}
