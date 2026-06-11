package media

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func TestIngestImage(t *testing.T) {
	if !hasBin("ffmpeg") {
		t.Skip("ffmpeg 不可用")
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "red.png")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=red:size=64x64",
		"-frames:v", "1", img).CombinedOutput(); err != nil {
		t.Fatalf("生成测试图片失败: %v: %s", err, out)
	}
	blocks, err := IngestFile(img)
	if err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != types.ContentBlockImage {
		t.Fatalf("期望 1 个 image 块, 得到 %+v", blocks)
	}
	src := blocks[0].Source
	if src == nil || src.Type != "base64" || src.MediaType != "image/png" || src.Data == "" {
		t.Fatalf("image source 不完整: %+v", src)
	}
}

func TestIngestVideoExtractsFrames(t *testing.T) {
	if !hasBin("ffmpeg") || !hasBin("ffprobe") {
		t.Skip("ffmpeg/ffprobe 不可用")
	}
	dir := t.TempDir()
	vid := filepath.Join(dir, "test.mp4")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=3:size=320x240:rate=10",
		"-pix_fmt", "yuv420p", vid).CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v: %s", err, out)
	}
	blocks, err := IngestFile(vid)
	if err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	var imgs, texts int
	for _, b := range blocks {
		switch b.Type {
		case types.ContentBlockImage:
			imgs++
			if b.Source == nil || b.Source.Data == "" {
				t.Fatalf("帧块缺少 base64 数据")
			}
		case types.ContentBlockText:
			texts++
			if !strings.Contains(b.Text, "视频附件") {
				t.Fatalf("视频说明文本不符: %s", b.Text)
			}
		}
	}
	if imgs != maxVideoFrames || texts != 1 {
		t.Fatalf("期望 %d 帧 + 1 说明, 得到 %d 帧 %d 说明", maxVideoFrames, imgs, texts)
	}
}

func TestIngestAudioFallbackMetadata(t *testing.T) {
	if !hasBin("ffmpeg") || !hasBin("ffprobe") {
		t.Skip("ffmpeg/ffprobe 不可用")
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "tone.wav")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		wav).CombinedOutput(); err != nil {
		t.Fatalf("生成测试音频失败: %v: %s", err, out)
	}
	blocks, err := IngestFile(wav)
	if err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != types.ContentBlockText {
		t.Fatalf("期望 1 个文本块, 得到 %+v", blocks)
	}
	// 有 ASR 时是转写文本, 无 ASR 时是元数据降级 —— 两者都必须带音频附件标头
	if !strings.Contains(blocks[0].Text, "音频附件") {
		t.Fatalf("音频块文本不符: %s", blocks[0].Text)
	}
}

func TestIngestUnsupported(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.xyz")
	if err := exec.Command("touch", p).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestFile(p); err == nil {
		t.Fatal("期望不支持的类型报错")
	}
}
