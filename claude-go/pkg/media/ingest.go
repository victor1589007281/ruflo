// ingest.go 多模态输入摄取: 把本地媒体文件转换为 Anthropic Messages 内容块。
//
// 闭环输入端 (对应输出端 engine.go / tool/builtin/media_gen.go):
//   - 图片: 直接 base64 → image 块 (视觉模型原生输入)
//   - 视频: ffmpeg 均匀抽帧 → 多个 image 块 + 元数据文本 (模型不收视频流, 抽帧是标准范式)
//   - 音频: ASR 转写 (faster-whisper) → 文本块; 不可用时降级为元数据说明
//
// 依赖: ffmpeg/ffprobe (视频); python3 + faster_whisper (音频, 可选)
package media

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// 视频最多抽取的帧数 (帧过多消耗上下文; gemma 类视觉模型每图 ~256-1024 token)
const maxVideoFrames = 4

// 抽帧统一缩放宽度 (像素), 控制 token 消耗
const frameScaleWidth = 640

var imageExts = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

var videoExts = map[string]struct{}{
	".mp4": {}, ".mov": {}, ".webm": {}, ".avi": {}, ".mkv": {}, ".m4v": {},
}

var audioExts = map[string]struct{}{
	".wav": {}, ".mp3": {}, ".m4a": {}, ".ogg": {}, ".flac": {}, ".aac": {}, ".opus": {},
}

// IngestFile 把一个媒体文件转换为内容块列表。
// 支持图片/视频/音频; 其他扩展名返回错误。
func IngestFile(path string) ([]types.ContentBlock, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("附件不存在: %s", path)
	}
	if mime, ok := imageExts[ext]; ok {
		return ingestImage(path, mime)
	}
	if _, ok := videoExts[ext]; ok {
		return ingestVideo(path)
	}
	if _, ok := audioExts[ext]; ok {
		return ingestAudio(path)
	}
	return nil, fmt.Errorf("不支持的附件类型 %q (支持图片 png/jpg/gif/webp、视频 mp4/mov/webm/avi/mkv、音频 wav/mp3/m4a/ogg/flac)", ext)
}

func imageBlockFromBytes(data []byte, mime string) types.ContentBlock {
	return types.ContentBlock{
		Type: types.ContentBlockImage,
		Source: &types.MediaSource{
			Type:      "base64",
			MediaType: mime,
			Data:      base64.StdEncoding.EncodeToString(data),
		},
	}
}

func ingestImage(path, mime string) ([]types.ContentBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取图片失败: %w", err)
	}
	return []types.ContentBlock{imageBlockFromBytes(data, mime)}, nil
}

// probeDuration 用 ffprobe 获取媒体时长 (秒)。
func probeDuration(path string) (float64, error) {
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json",
		"-show_format", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe 失败: %w", err)
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0, err
	}
	d, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil {
		return 0, fmt.Errorf("解析时长失败: %w", err)
	}
	return d, nil
}

// ingestVideo 均匀抽帧: 视频本身不能进模型, 抽 N 帧作为 image 块 + 时间戳说明。
func ingestVideo(path string) ([]types.ContentBlock, error) {
	dur, err := probeDuration(path)
	if err != nil {
		return nil, err
	}
	n := maxVideoFrames
	if dur < 2 {
		n = 2
	}
	tmpDir, err := os.MkdirTemp("", "claude-go-frames-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	var blocks []types.ContentBlock
	var stamps []string
	for i := 0; i < n; i++ {
		// 均匀取样, 避开首尾黑帧: (i+0.5)/n
		t := dur * (float64(i) + 0.5) / float64(n)
		framePath := filepath.Join(tmpDir, fmt.Sprintf("frame_%d.jpg", i))
		cmd := exec.Command("ffmpeg", "-y", "-ss", fmt.Sprintf("%.3f", t), "-i", path,
			"-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:-2", frameScaleWidth),
			"-q:v", "4", framePath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg 抽帧失败 (t=%.1fs): %v: %s", t, err, truncate(string(out), 300))
		}
		data, err := os.ReadFile(framePath)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, imageBlockFromBytes(data, "image/jpeg"))
		stamps = append(stamps, fmt.Sprintf("%.1fs", t))
	}
	desc := fmt.Sprintf("[视频附件 %s: 时长 %.1f 秒, 已按时间均匀抽取 %d 帧, 时间点分别为 %s。以上图片即这些帧, 请按时间顺序理解视频内容。]",
		filepath.Base(path), dur, n, strings.Join(stamps, ", "))
	blocks = append(blocks, types.ContentBlock{Type: types.ContentBlockText, Text: desc})
	return blocks, nil
}

// ingestAudio 音频 → 文本: 优先 faster-whisper 转写, 不可用时降级为元数据说明。
func ingestAudio(path string) ([]types.ContentBlock, error) {
	dur, durErr := probeDuration(path)
	transcript, asrErr := TranscribeAudio(path, 5*time.Minute)
	if asrErr == nil && strings.TrimSpace(transcript) != "" {
		text := fmt.Sprintf("[音频附件 %s: 时长 %.1f 秒, ASR 转写内容如下]\n%s",
			filepath.Base(path), dur, strings.TrimSpace(transcript))
		return []types.ContentBlock{{Type: types.ContentBlockText, Text: text}}, nil
	}
	if durErr != nil {
		return nil, fmt.Errorf("音频既无法转写也无法读取元数据: %v", asrErr)
	}
	text := fmt.Sprintf("[音频附件 %s: 时长 %.1f 秒。本机 ASR 不可用 (%v), 仅提供元数据。]",
		filepath.Base(path), dur, asrErr)
	return []types.ContentBlock{{Type: types.ContentBlockText, Text: text}}, nil
}

// TranscribeAudio 用 faster-whisper 转写音频。
// 模型可用 CLAUDE_GO_WHISPER_MODEL 覆盖 (默认 base, 首次运行自动经 HF 镜像下载)。
func TranscribeAudio(path string, timeout time.Duration) (string, error) {
	model := os.Getenv("CLAUDE_GO_WHISPER_MODEL")
	if model == "" {
		model = "base"
	}
	script := `
import sys
from faster_whisper import WhisperModel
m = WhisperModel(sys.argv[1], device="cpu", compute_type="int8")
segs, info = m.transcribe(sys.argv[2])
print("".join(s.text for s in segs))
`
	cmd := exec.Command("python3", "-c", script, model, path)
	// HF 直连被墙时走镜像下载 whisper 模型
	cmd.Env = append(os.Environ(), "HF_ENDPOINT=https://hf-mirror.com")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("ASR 转写超时 (%s)", timeout)
	}
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = truncate(string(ee.Stderr), 300)
		}
		return "", fmt.Errorf("faster-whisper 不可用: %s", msg)
	}
	return string(out), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
