// 确定性视频 / GIF 渲染。
//
// 设计要点 (相对旧的墙钟 Sleep+截图):
//   - 旧法: 每帧 Sleep(1/fps) 后截图, 真实耗时 = Sleep + 截图延迟, 动画时间随墙钟漂移
//     → 成片播放偏快、帧间隔不均、重负载丢帧。
//   - 新法 (确定性): 用 Web Animations API 把页面上每个动画 pause 后, 逐帧把 currentTime
//     精确 seek 到 i/fps 时刻再截图。无论单帧截图耗时多少, 第 i 帧永远是动画第 i/fps 秒的
//     确定画面 → 回放速度精确、无漂移、可任意时长/帧率、可逆序渲染。
//
// 参考: HyperFrames / Remotion / WebVideoCreator 的"对浏览器谎报时间"思想。这里用
// getAnimations().currentTime seek 实现, 不依赖已在新版 headless 中废弃的 HeadlessExperimental
// 域; 对纯 JS(canvas/rAF) 动画无 Web Animations 时回退到定时截图。
package media

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// 逐帧确定性 seek 脚本: 暂停所有动画, 把 currentTime 设到 tMs 毫秒。
// 每帧重新 getAnimations 以纳入延迟加入的动画; 已暂停的再次 pause 无副作用。
const seekAnimationsJS = `(function(tMs){
  try {
    var anims = (document.getAnimations ? document.getAnimations() : []) || [];
    for (var i = 0; i < anims.length; i++) {
      try { anims[i].pause(); anims[i].currentTime = tMs; } catch (e) {}
    }
    return anims.length;
  } catch (e) { return -1; }
})(%f)`

// detectAnimationsJS 判断页面是否存在可 seek 的 Web Animations 或 CSS 动画。
const detectAnimationsJS = `(function(){
  try {
    var a = (document.getAnimations ? document.getAnimations() : []) || [];
    if (a.length > 0) return true;
    var styles = document.querySelectorAll('style');
    for (var i = 0; i < styles.length; i++) {
      if (styles[i].textContent.indexOf('@keyframes') >= 0) return true;
    }
    return false;
  } catch (e) { return false; }
})()`

// animationDurationJS 计算页面所有动画结束时刻的最大值 (毫秒), 即"整段动画时长"。
// getComputedTiming().endTime 已含 delay + activeDuration + endDelay; 再加上 startTime
// 兼容延迟启动的动画。无限循环动画 (endTime=Infinity) 跳过 → 由调用方回退到默认时长。
// 用途: 视频渲染按真实动画时长截全部场景, 而非固定 5s 只截到开头。
const animationDurationJS = `(function(){
  try {
    var anims = (document.getAnimations ? document.getAnimations() : []) || [];
    var maxMs = 0;
    for (var i = 0; i < anims.length; i++) {
      try {
        var a = anims[i];
        if (!a.effect || !a.effect.getComputedTiming) continue;
        var end = a.effect.getComputedTiming().endTime;
        if (typeof end !== 'number' || !isFinite(end)) continue;
        var st = (typeof a.startTime === 'number' && isFinite(a.startTime)) ? a.startTime : 0;
        if (st < 0) st = 0;
        var total = st + end;
        if (total > maxMs) maxMs = total;
      } catch (e) {}
    }
    return maxMs;
  } catch (e) { return 0; }
})()`

// durationHintJS 读取 HTML 显式声明的视频时长 (秒): <body data-duration="N"> 或
// <meta name="video-duration" content="N">。这是 html-developer 与渲染器之间的"时长契约",
// 对 JS 定时器(setTimeout)驱动的多场景动画尤其必要 (静态无法推断其总时长)。
const durationHintJS = `(function(){
  try {
    var d = 0, b = document.body;
    if (b) { var ds = b.getAttribute('data-duration'); if (ds) d = parseFloat(ds) || 0; }
    if (!d) { var m = document.querySelector('meta[name="video-duration"]'); if (m) d = parseFloat(m.getAttribute('content')) || 0; }
    return d > 0 ? d : 0;
  } catch (e) { return 0; }
})()`

// timeDrivenJS 粗判页面是否依赖 JS 定时器/帧循环推进 (setTimeout/setInterval/rAF)。
// 若是, 必须用墙钟实时回放截图 (确定性 seek 只能拨 Web Animations 的 currentTime,
// 拨不动 setTimeout → 场景不切换, 视频会卡在开头)。
const timeDrivenJS = `(function(){
  try {
    var s = document.querySelectorAll('script'), t = '';
    for (var i = 0; i < s.length; i++) { t += s[i].textContent || ''; }
    return /setTimeout|setInterval|requestAnimationFrame/.test(t);
  } catch (e) { return false; }
})()`

// captureFrames 把 htmlPath 渲染成 framesDir 下的 frame_%06d.png 序列。
// 返回成功捕获的帧数。优先用确定性 Web Animations seek; 无动画时回退到定时截图。
func (e *Engine) captureFrames(ctx context.Context, htmlPath, framesDir string, fps, durationSec int) (int, error) {
	if err := os.MkdirAll(framesDir, 0755); err != nil {
		return 0, fmt.Errorf("创建帧目录失败: %w", err)
	}
	if fps <= 0 {
		fps = 30
	}

	taskCtx, cancel := e.newBrowserCtx(ctx, 8*time.Minute)
	defer cancel()

	fileURL := "file://" + htmlPath
	var hasSeekable, timeDriven bool
	var animEndMs, durHintSec float64
	if err := chromedp.Run(taskCtx,
		chromedp.Navigate(fileURL),
		emulation.SetDeviceMetricsOverride(int64(e.Width), int64(e.Height), 1.0, false),
		chromedp.WaitReady("body"),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Evaluate(detectAnimationsJS, &hasSeekable),
		chromedp.Evaluate(animationDurationJS, &animEndMs),
		chromedp.Evaluate(durationHintJS, &durHintSec),
		chromedp.Evaluate(timeDrivenJS, &timeDriven),
	); err != nil {
		return 0, fmt.Errorf("导航失败: %w", err)
	}

	// durationSec<=0: 自动决定时长, 确保截到全部场景 (修复"固定5s只截开头、后续场景丢失")。
	// 优先级: 显式时长契约 data-duration > 时间驱动兜底 > Web Animations 总时长。
	// 注意: 时间驱动页面的 animEndMs 只覆盖局部入场/循环动画 (偏小不可信), 故排在兜底之后。
	if durationSec <= 0 {
		switch {
		case durHintSec > 0:
			durationSec = int(math.Ceil(durHintSec))
		case timeDriven:
			durationSec = timedSceneDefaultSec
			if s := int(math.Ceil(animEndMs/1000.0)) + 1; s > durationSec {
				durationSec = s
			}
		case animEndMs > 0:
			durationSec = int(math.Ceil(animEndMs/1000.0)) + 1 // +1s 收尾缓冲
		}
		if durationSec < 5 {
			durationSec = 5
		}
	}
	if durationSec > maxVideoSec {
		durationSec = maxVideoSec
	}

	// 截帧模式: 仅"纯 CSS/Web Animations 且不靠 JS 定时器推进"时用确定性 seek (精确无漂移);
	// 一旦页面靠 setTimeout/setInterval/rAF 推进, 必须墙钟实时回放, 否则场景不切换。
	deterministic := hasSeekable && !timeDriven

	totalFrames := fps * durationSec
	if totalFrames > maxRenderFrames {
		totalFrames = maxRenderFrames
	}

	frameInterval := float64(1000) / float64(fps) // 每帧对应的动画毫秒数
	captured := 0
	for i := 0; i < totalFrames; i++ {
		framePath := filepath.Join(framesDir, fmt.Sprintf("frame_%06d.png", i))
		tMs := float64(i) * frameInterval

		var actions []chromedp.Action
		if deterministic {
			// 确定性: seek 到精确时刻, 截图与墙钟无关
			actions = append(actions, chromedp.Evaluate(fmt.Sprintf(seekAnimationsJS, tMs), nil))
			actions = append(actions, chromedp.Sleep(8*time.Millisecond)) // 让 compositor 落定一帧
		} else {
			// 回退: 纯 JS/canvas 动画无法 seek, 按墙钟定时截图 (退化为旧行为)
			actions = append(actions, chromedp.Sleep(time.Duration(frameInterval)*time.Millisecond))
		}
		actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
			buf, err := page.CaptureScreenshot().
				WithFormat(page.CaptureScreenshotFormatPng).
				WithCaptureBeyondViewport(false).
				Do(ctx)
			if err != nil {
				return err
			}
			return os.WriteFile(framePath, buf, 0644)
		}))

		if err := chromedp.Run(taskCtx, actions...); err != nil {
			continue
		}
		captured++
	}
	if captured == 0 {
		return 0, fmt.Errorf("未能捕获任何帧")
	}
	return captured, nil
}

// encodeFramesToMP4 用 FFmpeg 把 PNG 帧序列编码为 H.264 MP4。
func encodeFramesToMP4(ctx context.Context, framesDir, outPath string, fps int) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg 未安装")
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", filepath.Join(framesDir, "frame_%06d.png"),
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-preset", "fast",
		"-crf", "23",
		"-movflags", "+faststart",
		outPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("FFmpeg 编码失败: %v\n%s", err, tailBytes(out, 600))
	}
	return nil
}

// encodeFramesToGIF 把 PNG 帧序列编码为高质量 GIF。
//
// 业界最佳实践:
//   - gifski (pngquant 跨帧调色板 + 时间抖动): 质量最高, 若安装则优先用。
//   - 否则用 FFmpeg 两遍调色板: palettegen 生成最优 256 色 → paletteuse 带 dithering 合成,
//     质量/体积平衡最好、零额外依赖。
func encodeFramesToGIF(ctx context.Context, framesDir, outPath string, fps int) error {
	// 优先 gifski (若可用)
	if gifski, err := exec.LookPath("gifski"); err == nil {
		frames, _ := filepath.Glob(filepath.Join(framesDir, "frame_*.png"))
		if len(frames) > 0 {
			args := []string{"--fps", fmt.Sprintf("%d", fps), "-o", outPath}
			args = append(args, frames...)
			cmd := exec.CommandContext(ctx, gifski, args...)
			if out, err := cmd.CombinedOutput(); err == nil {
				maybeOptimizeGIF(ctx, outPath)
				return nil
			} else {
				// gifski 失败则继续走 ffmpeg
				_ = out
			}
		}
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg/gifski 均不可用")
	}
	// FFmpeg 两遍调色板
	palette := filepath.Join(framesDir, "palette.png")
	input := filepath.Join(framesDir, "frame_%06d.png")
	genCmd := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", input,
		"-vf", "palettegen=stats_mode=diff",
		palette,
	)
	if out, err := genCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("palettegen 失败: %v\n%s", err, tailBytes(out, 400))
	}
	useCmd := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", input,
		"-i", palette,
		"-lavfi", "paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle",
		outPath,
	)
	if out, err := useCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("paletteuse 失败: %v\n%s", err, tailBytes(out, 400))
	}
	_ = os.Remove(palette)
	maybeOptimizeGIF(ctx, outPath)
	return nil
}

// maybeOptimizeGIF 若安装了 gifsicle, 进一步压缩 (可减 30-60% 体积)。
func maybeOptimizeGIF(ctx context.Context, gifPath string) {
	bin, err := exec.LookPath("gifsicle")
	if err != nil {
		return
	}
	tmp := gifPath + ".opt"
	cmd := exec.CommandContext(ctx, bin, "--optimize=3", "--lossy=80", gifPath, "-o", tmp)
	if err := cmd.Run(); err == nil {
		if st, e := os.Stat(tmp); e == nil && st.Size() > 0 {
			_ = os.Rename(tmp, gifPath)
			return
		}
	}
	_ = os.Remove(tmp)
}

// renderGIF 将 HTML 渲染为动图 GIF (帧序列 → 调色板编码)。
func (e *Engine) renderGIF(ctx context.Context, htmlPath, name string) RenderResult {
	outPath := filepath.Join(e.OutputDir, name+".gif")
	framesDir := filepath.Join(e.OutputDir, name+"_gifframes")
	fps := e.FPS
	if fps <= 0 {
		fps = 15 // GIF 默认低帧率, 控制体积
	}
	dur := e.DurationSec
	if dur <= 0 {
		dur = 4
	}
	n, err := e.captureFrames(ctx, htmlPath, framesDir, fps, dur)
	if err != nil {
		return RenderResult{Format: "gif", Error: err.Error()}
	}
	if err := encodeFramesToGIF(ctx, framesDir, outPath, fps); err != nil {
		os.RemoveAll(framesDir)
		return RenderResult{Format: "gif", Error: err.Error()}
	}
	os.RemoveAll(framesDir)
	return RenderResult{Format: "gif", FilePath: outPath, Size: fileSize(outPath), MediaSeconds: float64(n) / float64(fps)}
}

// SlideshowFromImages 把若干同尺寸图片合成一段幻灯片视频 (每张定长, 简单硬切)。
// 用于"文章配图 → 短视频"(可再用 MuxAudio 叠加朗读解说)。返回输出路径。
func SlideshowFromImages(ctx context.Context, imgPaths []string, outPath string, secsPer int, fps int) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg 未安装")
	}
	if len(imgPaths) == 0 {
		return fmt.Errorf("无图片")
	}
	if secsPer <= 0 {
		secsPer = 4
	}
	if fps <= 0 {
		fps = 30
	}
	// concat demuxer 列表: 每张图 duration 秒; 末张需重复一次 (ffmpeg concat 末项 duration 被忽略)。
	var lst strings.Builder
	for _, p := range imgPaths {
		ap, _ := filepath.Abs(p)
		lst.WriteString("file '" + strings.ReplaceAll(ap, "'", "'\\''") + "'\n")
		lst.WriteString(fmt.Sprintf("duration %d\n", secsPer))
	}
	last, _ := filepath.Abs(imgPaths[len(imgPaths)-1])
	lst.WriteString("file '" + strings.ReplaceAll(last, "'", "'\\''") + "'\n")

	listFile := outPath + ".concat.txt"
	if err := os.WriteFile(listFile, []byte(lst.String()), 0o644); err != nil {
		return err
	}
	defer os.Remove(listFile)

	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffmpeg", "-y", "-f", "concat", "-safe", "0", "-i", listFile,
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2,fps="+fmt.Sprintf("%d", fps),
		"-pix_fmt", "yuv420p", "-c:v", "libx264", outPath)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg 合成幻灯片失败: %v\n%s", err, string(b))
	}
	return nil
}

// MuxAudio 把音频轨混流进视频 (视频已有则替换音轨)。用于"图文→带解说短视频"闭环。
// outPath 可与 videoPath 不同; 若相同则写临时文件再替换。
func MuxAudio(ctx context.Context, videoPath, audioPath, outPath string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg 未安装")
	}
	target := outPath
	inPlace := outPath == videoPath || outPath == ""
	if inPlace {
		target = videoPath + ".muxed.mp4"
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-i", videoPath,
		"-i", audioPath,
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
		"-shortest",
		target,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("混流失败: %v\n%s", err, tailBytes(out, 400))
	}
	if inPlace {
		return os.Rename(target, videoPath)
	}
	return nil
}

func tailBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "..." + string(b[len(b)-n:])
}
