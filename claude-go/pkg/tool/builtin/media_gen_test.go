package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

func toolHasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func TestGenerateSpeechEspeak(t *testing.T) {
	if !toolHasBin("espeak-ng") {
		t.Skip("espeak-ng 不可用")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "hello.wav")
	in, _ := json.Marshal(map[string]string{
		"text": "hello world", "output_path": out, "engine": "espeak",
	})
	res, err := NewGenerateSpeechTool().Call(context.Background(), in, &tool.ToolContext{Cwd: dir})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("合成失败: %s", res.Content)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() < 1000 {
		t.Fatalf("WAV 无效: %v", err)
	}
}

func TestGenerateSpeechChineseAutoUsesEspeak(t *testing.T) {
	if !toolHasBin("espeak-ng") {
		t.Skip("espeak-ng 不可用")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "zh.wav")
	in, _ := json.Marshal(map[string]string{"text": "你好世界", "output_path": out})
	res, err := NewGenerateSpeechTool().Call(context.Background(), in, &tool.ToolContext{Cwd: dir})
	if err != nil || res.IsError {
		t.Fatalf("中文合成失败: %v %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "espeak-ng (cmn)") {
		t.Fatalf("中文应自动用 espeak-ng cmn: %s", res.Content)
	}
}

func TestGenerateVideoPythonFFmpeg(t *testing.T) {
	if !toolHasBin("python3") || !toolHasBin("ffmpeg") || !toolHasBin("ffprobe") {
		t.Skip("python3/ffmpeg 不可用")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "test.mp4")
	// 不依赖 matplotlib: 直接用 ffmpeg 合成测试视频, 验证 "脚本→真实mp4→ffprobe校验" 链路
	code := `
import os, subprocess, sys
out = os.environ["OUTPUT_PATH"]
subprocess.run(["ffmpeg","-y","-f","lavfi","-i","testsrc=duration=2:size=320x240:rate=10",
    "-pix_fmt","yuv420p", out], check=True, capture_output=True)
`
	in, _ := json.Marshal(map[string]any{"python_code": code, "output_path": out, "timeout_sec": 60})
	res, err := NewGenerateVideoTool().Call(context.Background(), in, &tool.ToolContext{Cwd: dir})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("视频生成失败: %s", res.Content)
	}
	if !strings.Contains(res.Content, "320x240") {
		t.Fatalf("ffprobe 校验信息缺失: %s", res.Content)
	}
}

func TestGenerateImageSVG(t *testing.T) {
	if !toolHasBin("google-chrome") && !toolHasBin("chromium") {
		t.Skip("chrome 不可用")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "circle.png")
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200"><circle cx="100" cy="100" r="80" fill="red"/></svg>`
	in, _ := json.Marshal(map[string]any{"svg": svg, "output_path": out, "width": 200, "height": 200})
	res, err := NewGenerateImageTool().Call(context.Background(), in, &tool.ToolContext{Cwd: dir})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("渲染失败: %s", res.Content)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() < 500 {
		t.Fatalf("PNG 无效: %v, size=%d", err, st.Size())
	}
	fmt.Println(res.Content)
}
