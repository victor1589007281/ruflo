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
	"os"
	"os/exec"
	"path/filepath"
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

// captureFrames 把 htmlPath 渲染成 framesDir 下的 frame_%06d.png 序列。
// 返回成功捕获的帧数。优先用确定性 Web Animations seek; 无动画时回退到定时截图。
func (e *Engine) captureFrames(ctx context.Context, htmlPath, framesDir string, fps, durationSec int) (int, error) {
	if err := os.MkdirAll(framesDir, 0755); err != nil {
		return 0, fmt.Errorf("创建帧目录失败: %w", err)
	}
	if fps <= 0 {
		fps = 30
	}
	if durationSec <= 0 {
		durationSec = 5
	}
	totalFrames := fps * durationSec
	if totalFrames > maxRenderFrames {
		totalFrames = maxRenderFrames
	}

	taskCtx, cancel := e.newBrowserCtx(ctx, 8*time.Minute)
	defer cancel()

	fileURL := "file://" + htmlPath
	var deterministic bool
	if err := chromedp.Run(taskCtx,
		chromedp.Navigate(fileURL),
		emulation.SetDeviceMetricsOverride(int64(e.Width), int64(e.Height), 1.0, false),
		chromedp.WaitReady("body"),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Evaluate(detectAnimationsJS, &deterministic),
	); err != nil {
		return 0, fmt.Errorf("导航失败: %w", err)
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
	_ = n
	return RenderResult{Format: "gif", FilePath: outPath, Size: fileSize(outPath)}
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
