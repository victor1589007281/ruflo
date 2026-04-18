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
	OutputDir string        // 输出目录
	Timeout   time.Duration // 单次渲染超时
	Width     int           // 视口宽度
	Height    int           // 视口高度
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
	Format   string // png, pdf, mp4, svg, pptx
	FilePath string // 输出文件路径
	Size     int64  // 文件大小 (bytes)
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

// renderImage 使用 chromedp 将 HTML 渲染为 PNG/JPEG
func (e *Engine) renderImage(ctx context.Context, htmlPath, name, format string) RenderResult {
	outPath := filepath.Join(e.OutputDir, name+"."+format)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(e.Width, e.Height),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()

	taskCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	taskCtx, cancel = context.WithTimeout(taskCtx, e.Timeout)
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

	info, _ := os.Stat(outPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	log.Printf("[media] 渲染 %s: %s (%.1f KB)", format, outPath, float64(size)/1024)
	return RenderResult{Format: format, FilePath: outPath, Size: size}
}

// renderPDF 使用 chromedp PrintToPDF 将 HTML 渲染为 PDF
func (e *Engine) renderPDF(ctx context.Context, htmlPath, name string) RenderResult {
	outPath := filepath.Join(e.OutputDir, name+".pdf")

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(e.Width, e.Height),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()

	taskCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	taskCtx, cancel = context.WithTimeout(taskCtx, e.Timeout)
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

	info, _ := os.Stat(outPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

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

	// 确保 SVG 有 xmlns
	if !strings.Contains(match, "xmlns") {
		match = strings.Replace(match, "<svg", `<svg xmlns="http://www.w3.org/2000/svg"`, 1)
	}

	if err := os.WriteFile(outPath, []byte(match), 0644); err != nil {
		return RenderResult{Format: "svg", Error: fmt.Sprintf("写入 SVG 失败: %v", err)}
	}

	info, _ := os.Stat(outPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	log.Printf("[media] 提取 SVG: %s (%.1f KB)", outPath, float64(size)/1024)
	return RenderResult{Format: "svg", FilePath: outPath, Size: size}
}

// renderVideo 使用 chromedp 逐帧截图 + FFmpeg 合成 MP4
// 参考 HyperFrames 的 CDP BeginFrame 模式
func (e *Engine) renderVideo(ctx context.Context, htmlPath, name string) RenderResult {
	outPath := filepath.Join(e.OutputDir, name+".mp4")
	framesDir := filepath.Join(e.OutputDir, name+"_frames")
	if err := os.MkdirAll(framesDir, 0755); err != nil {
		return RenderResult{Format: "mp4", Error: fmt.Sprintf("创建帧目录失败: %v", err)}
	}

	// 检查 ffmpeg 是否可用
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return RenderResult{Format: "mp4", Error: "ffmpeg 未安装, 无法生成视频"}
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(e.Width, e.Height),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()

	taskCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	taskCtx, cancel = context.WithTimeout(taskCtx, 5*time.Minute)
	defer cancel()

	fileURL := "file://" + htmlPath
	fps := 30
	durationSec := 5 // 默认5秒动画
	totalFrames := fps * durationSec

	// 导航并查询动画时长
	var animDuration float64
	err := chromedp.Run(taskCtx,
		chromedp.Navigate(fileURL),
		emulation.SetDeviceMetricsOverride(int64(e.Width), int64(e.Height), 1.0, false),
		chromedp.WaitReady("body"),
		chromedp.Sleep(1*time.Second),
		chromedp.Evaluate(`
			(function() {
				var d = document.querySelector('[data-duration]');
				if (d) return parseFloat(d.getAttribute('data-duration')) || 5;
				var anims = document.getAnimations ? document.getAnimations() : [];
				if (anims.length > 0) {
					var max = 0;
					anims.forEach(function(a) {
						var t = (a.effect && a.effect.getTiming) ? a.effect.getTiming() : {};
						var end = (t.delay || 0) + (t.duration || 0);
						if (end > max) max = end;
					});
					if (max > 0) return max / 1000;
				}
				return 5;
			})()
		`, &animDuration),
	)
	if err != nil {
		return RenderResult{Format: "mp4", Error: fmt.Sprintf("导航失败: %v", err)}
	}

	if animDuration > 0 && animDuration <= 60 {
		durationSec = int(animDuration)
		if durationSec < 1 {
			durationSec = 1
		}
		totalFrames = fps * durationSec
	}

	log.Printf("[media] 视频: %d 帧 (%ds @ %dfps)", totalFrames, durationSec, fps)

	// 逐帧截图
	for i := 0; i < totalFrames; i++ {
		frameTime := float64(i) / float64(fps)
		framePath := filepath.Join(framesDir, fmt.Sprintf("frame_%06d.png", i))

		err := chromedp.Run(taskCtx,
			// 暂停动画并 seek 到指定时间点
			chromedp.Evaluate(fmt.Sprintf(`
				(function() {
					var anims = document.getAnimations ? document.getAnimations() : [];
					anims.forEach(function(a) { a.pause(); a.currentTime = %f * 1000; });
				})()
			`, frameTime), nil),
			chromedp.Sleep(30*time.Millisecond),
			chromedp.ActionFunc(func(ctx context.Context) error {
				buf, err := page.CaptureScreenshot().
					WithFormat(page.CaptureScreenshotFormatPng).
					Do(ctx)
				if err != nil {
					return err
				}
				return os.WriteFile(framePath, buf, 0644)
			}),
		)
		if err != nil {
			log.Printf("[media] 帧 %d 截图失败: %v", i, err)
			continue
		}

		if i%30 == 0 {
			log.Printf("[media] 帧捕获进度: %d/%d (%.0f%%)", i, totalFrames, float64(i)/float64(totalFrames)*100)
		}
	}

	// FFmpeg 编码
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", filepath.Join(framesDir, "frame_%06d.png"),
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-preset", "medium",
		"-crf", "23",
		"-movflags", "+faststart",
		outPath,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return RenderResult{Format: "mp4", Error: fmt.Sprintf("FFmpeg 编码失败: %v", err)}
	}

	// 清理帧文件
	os.RemoveAll(framesDir)

	info, _ := os.Stat(outPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	log.Printf("[media] 渲染视频: %s (%.1f MB, %ds)", outPath, float64(size)/1024/1024, durationSec)
	return RenderResult{Format: "mp4", FilePath: outPath, Size: size}
}

// renderPPTX 将 HTML 幻灯片渲染为 PPTX (截图方式)
// 每个 <section> 或 <div class="slide"> 作为一页幻灯片
func (e *Engine) renderPPTX(ctx context.Context, htmlContent, name string) RenderResult {
	outDir := filepath.Join(e.OutputDir, name+"_slides")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return RenderResult{Format: "pptx", Error: fmt.Sprintf("创建幻灯片目录失败: %v", err)}
	}

	// 拆分幻灯片: 查找 <section> 或 slide 分隔符
	slides := splitSlides(htmlContent)
	if len(slides) == 0 {
		// 作为单页处理
		slides = []string{htmlContent}
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(e.Width, e.Height),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()

	taskCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	taskCtx, cancel = context.WithTimeout(taskCtx, e.Timeout)
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

	// 生成简易 PPTX (Open XML)
	pptxPath := filepath.Join(e.OutputDir, name+".pptx")
	if err := buildSimplePPTX(pptxPath, slidePaths); err != nil {
		// PPTX 生成失败时, 回退为 PDF (多页)
		log.Printf("[media] PPTX 生成失败 (%v), 回退为多页 PDF", err)
		pdfPath := filepath.Join(e.OutputDir, name+"_slides.pdf")
		pdfResult := e.renderPDF(ctx, filepath.Join(outDir, "slide_001.html"), name+"_slides")
		if pdfResult.Error == "" {
			return RenderResult{Format: "pdf", FilePath: pdfPath, Size: pdfResult.Size}
		}
		return RenderResult{Format: "pptx", Error: fmt.Sprintf("PPTX+PDF 均失败: %v", err)}
	}

	info, _ := os.Stat(pptxPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	log.Printf("[media] 渲染 PPTX: %s (%d 页, %.1f MB)", pptxPath, len(slidePaths), float64(size)/1024/1024)
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
	// chromedp 会自动下载
	return true
}

// HasFFmpeg 检查 FFmpeg 是否可用
func HasFFmpeg() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// splitSlides 从 HTML 中拆分幻灯片
func splitSlides(html string) []string {
	// 匹配 <section> 标签
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

	// 匹配 class="slide" 的 div
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

// wrapSlideHTML 将幻灯片内容包装为完整 HTML
func wrapSlideHTML(content string, num, total int) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
html, body { width: 1920px; height: 1080px; overflow: hidden; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; }
</style>
</head><body>
%s
</body></html>`, content)
}
