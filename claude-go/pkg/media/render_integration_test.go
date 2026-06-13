package media

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const animatedHTML = `<!doctype html><html><head><meta charset="utf-8">
<style>
html,body{margin:0;background:#fff}
.box{width:80px;height:80px;background:#e74c3c;position:absolute;top:60px;
  animation:move 1s linear infinite}
@keyframes move{from{left:0}to{left:400px}}
</style></head><body><div class="box"></div></body></html>`

// TestRenderGIFAndMP4 真实跑一遍 Chrome+ffmpeg, 验证确定性逐帧捕获 → GIF/MP4 端到端可用。
func TestRenderGIFAndMP4(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过浏览器集成测试")
	}
	if _, err := exec.LookPath("google-chrome"); err != nil {
		if !HasChrome() {
			t.Skip("无 Chrome, 跳过")
		}
	}
	if !HasFFmpeg() {
		t.Skip("无 ffmpeg, 跳过")
	}

	dir := t.TempDir()
	eng := NewEngine(dir)
	eng.Width, eng.Height = 500, 200
	eng.FPS, eng.DurationSec = 10, 1 // ~10 帧

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// GIF
	gif := eng.RenderHTMLString(ctx, animatedHTML, "anim", "gif")
	if gif.Error != "" {
		t.Fatalf("GIF 渲染失败: %s", gif.Error)
	}
	gifData, err := os.ReadFile(filepath.Join(dir, "anim.gif"))
	if err != nil || len(gifData) == 0 {
		t.Fatalf("GIF 产物无效: %v (%d bytes)", err, len(gifData))
	}
	if !bytes.HasPrefix(gifData, []byte("GIF8")) {
		t.Errorf("产物不是 GIF (magic=%x)", gifData[:4])
	}
	// 动画 GIF 应含多个图像帧 (Graphic Control Extension 标记 0x21 0xF9 出现 >1 次)
	if n := bytes.Count(gifData, []byte{0x21, 0xF9}); n < 2 {
		t.Errorf("GIF 帧数过少 (GCE=%d), 期望多帧动画", n)
	}

	// MP4
	mp4 := eng.RenderHTMLString(ctx, animatedHTML, "anim", "mp4")
	if mp4.Error != "" {
		t.Fatalf("MP4 渲染失败: %s", mp4.Error)
	}
	mp4Path := filepath.Join(dir, "anim.mp4")
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "format=duration:stream=codec_type,width,height",
		"-of", "default=noprint_wrappers=1", mp4Path).CombinedOutput()
	if err != nil {
		t.Fatalf("MP4 ffprobe 校验失败: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("duration=")) || !bytes.Contains(out, []byte("codec_type=video")) {
		t.Fatalf("MP4 无有效视频流: %s", out)
	}
	t.Logf("MP4 ok: %s", bytes.TrimSpace(out))
}
