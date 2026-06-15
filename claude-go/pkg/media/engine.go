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

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// maxRenderFrames 单段视频/GIF 的帧数上限 (防止超长目标导致渲染失控)。
const maxRenderFrames = 1800 // 例如 60s@30fps

// maxVideoSec 自动时长的上限秒数 (与 maxRenderFrames 对齐, 30fps 下=60s)。
const maxVideoSec = 60

// timedSceneDefaultSec 当页面靠 JS 定时器推进多场景、却未声明 data-duration 时的兜底时长。
// 给模拟足够实时回放时间, 避免只截到开头几秒 (旧默认 5s 的根本问题)。
const timedSceneDefaultSec = 30

// Engine 媒体输出引擎
type Engine struct {
	OutputDir   string
	Timeout     time.Duration
	Width       int
	Height      int
	FPS         int // 视频/GIF 帧率 (0=按格式默认: 视频30, GIF15)
	DurationSec int // 视频/GIF 时长秒 (0=默认: 视频5, GIF4)
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
	Format       string
	FilePath     string
	Size         int64
	Duration     time.Duration // 渲染耗时 (墙钟), 非播放时长
	MediaSeconds float64       // 视频/GIF 实际播放时长(秒); 0 表示不适用(图片/PDF等)
	Error        string
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
		case "gif":
			r = e.renderGIF(ctx, htmlPath, name)
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

// newBrowserCtx 在共享的常驻 Chrome 分配器上新开一个 tab (见 pool.go), 避免反复冷启动 Chrome。
func (e *Engine) newBrowserCtx(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	taskCtx, taskCancel := chromedp.NewContext(sharedAllocator())
	timedCtx, timedCancel := context.WithTimeout(taskCtx, timeout)
	// 把上游 ctx 的取消传播到该 tab (allocator 根于 Background, 不会自动继承请求取消)。
	go func() {
		select {
		case <-ctx.Done():
			timedCancel()
		case <-timedCtx.Done():
		}
	}()
	cancel := func() {
		timedCancel()
		taskCancel()
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

// RenderScenesToVideo 确定性合成: 把多个"单场景 HTML"各自独立渲染为定长片段, 按序拼成一段视频。
// 每个场景动画从 0 播放、时长固定 (secsPerScene), 不依赖全局 CSS 时间线、无场景间空档——
// 这是 A3 的"确定性拼接", 配合逐场景生成可消除一次性整页生成的时间线脆弱与质量波动。
func (e *Engine) RenderScenesToVideo(ctx context.Context, sceneHTMLs []string, name string, secsPerScene int) (RenderResult, error) {
	if err := os.MkdirAll(e.OutputDir, 0755); err != nil {
		return RenderResult{Format: "mp4", Error: err.Error()}, err
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return RenderResult{Format: "mp4", Error: "ffmpeg 未安装"}, fmt.Errorf("ffmpeg missing")
	}
	fps := e.FPS
	if fps <= 0 {
		fps = 30
	}
	if secsPerScene <= 0 {
		secsPerScene = 5
	}
	combined := filepath.Join(e.OutputDir, name+"_combined_frames")
	if err := os.MkdirAll(combined, 0755); err != nil {
		return RenderResult{Format: "mp4", Error: err.Error()}, err
	}
	defer os.RemoveAll(combined)

	idx := 0
	for i, html := range sceneHTMLs {
		sceneDir := filepath.Join(e.OutputDir, fmt.Sprintf("%s_s%d_frames", name, i))
		scenePath := filepath.Join(e.OutputDir, fmt.Sprintf("%s_s%d.html", name, i))
		if err := os.WriteFile(scenePath, []byte(html), 0644); err != nil {
			continue
		}
		n, err := e.captureFrames(ctx, scenePath, sceneDir, fps, secsPerScene)
		if err != nil {
			os.RemoveAll(sceneDir)
			os.Remove(scenePath)
			continue
		}
		for f := 0; f < n; f++ {
			src := filepath.Join(sceneDir, fmt.Sprintf("frame_%06d.png", f))
			dst := filepath.Join(combined, fmt.Sprintf("frame_%06d.png", idx))
			if os.Rename(src, dst) == nil {
				idx++
			}
		}
		os.RemoveAll(sceneDir)
		os.Remove(scenePath)
	}
	if idx == 0 {
		return RenderResult{Format: "mp4", Error: "未能渲染任何场景帧"}, fmt.Errorf("no frames")
	}
	outPath := filepath.Join(e.OutputDir, name+".mp4")
	if err := encodeFramesToMP4(ctx, combined, outPath, fps); err != nil {
		return RenderResult{Format: "mp4", Error: err.Error()}, err
	}
	return RenderResult{Format: "mp4", FilePath: outPath, Size: fileSize(outPath), MediaSeconds: float64(idx) / float64(fps)}, nil
}

// renderVideo 渲染 HTML(CSS动画) 为 MP4。
//
// 采用确定性逐帧 seek (见 video.go captureFrames): 把每个 Web Animation 的 currentTime
// 精确 seek 到 i/fps 时刻再截图, 回放速度精确、无墙钟漂移。纯 JS/canvas 动画(无 Web
// Animations) 自动回退到定时截图。fps/时长由 e.FPS / e.DurationSec 控制 (默认 30fps/5s)。
func (e *Engine) renderVideo(ctx context.Context, htmlPath, name string) RenderResult {
	outPath := filepath.Join(e.OutputDir, name+".mp4")
	framesDir := filepath.Join(e.OutputDir, name+"_frames")
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return RenderResult{Format: "mp4", Error: "ffmpeg 未安装, 无法生成视频"}
	}

	fps := e.FPS
	if fps <= 0 {
		fps = 30
	}

	// e.DurationSec<=0 时传 0 给 captureFrames, 由它按页面动画总时长自动决定 (覆盖全部场景)。
	captured, err := e.captureFrames(ctx, htmlPath, framesDir, fps, e.DurationSec)
	if err != nil {
		os.RemoveAll(framesDir)
		return RenderResult{Format: "mp4", Error: err.Error()}
	}
	videoSec := float64(captured) / float64(fps)
	log.Printf("[media] 视频: 确定性捕获 %d 帧 (实际时长 %.1fs@%dfps)", captured, videoSec, fps)

	if err := encodeFramesToMP4(ctx, framesDir, outPath, fps); err != nil {
		os.RemoveAll(framesDir)
		return RenderResult{Format: "mp4", Error: err.Error()}
	}
	os.RemoveAll(framesDir)

	size := fileSize(outPath)
	log.Printf("[media] 渲染视频: %s (%.1f KB, 时长 %.1fs)", outPath, float64(size)/1024, videoSec)
	return RenderResult{Format: "mp4", FilePath: outPath, Size: size, MediaSeconds: videoSec}
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
