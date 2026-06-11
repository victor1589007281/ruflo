// media_gen.go 媒体生成输出工具 (闭环输出端)。
//
// 架构前提: 文本模型 (如本地 gemma4) 输出端只有文本, 不能原生生成图/视频/语音。
// 正确范式是 "模型写代码/写文本 → 专门工具渲染成真实媒体":
//   - GenerateImage : SVG/HTML → Chrome 无头渲染 → PNG
//   - GenerateVideo : Python 脚本 (matplotlib/ffmpeg) → 真实 mp4 (ffprobe 校验)
//   - GenerateSpeech: 文本 → piper (神经网络 TTS) / espeak-ng → WAV
//
// 外部依赖: google-chrome (图), python3+ffmpeg (视频), piper/espeak-ng (语音)。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/anthropic/claude-go/pkg/media"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// resolveOutPath 把相对输出路径锚定到会话 cwd, 并确保父目录存在。
func resolveOutPath(p, cwd string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("output_path 不能为空")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("创建输出目录失败: %w", err)
	}
	return p, nil
}

func denyInPlanMode(tctx *tool.ToolContext, what string) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "Plan mode: " + what + " 不可用"}
	}
	return nil
}

// ============================================================================
// GenerateImage: SVG/HTML → PNG
// ============================================================================

const GenerateImageToolName = "GenerateImage"

type generateImageInput struct {
	SVG        string `json:"svg,omitempty"`
	HTML       string `json:"html,omitempty"`
	OutputPath string `json:"output_path"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
}

type GenerateImageTool struct{}

func NewGenerateImageTool() *GenerateImageTool { return &GenerateImageTool{} }

func (t *GenerateImageTool) Name() string { return GenerateImageToolName }

func (t *GenerateImageTool) Description() string {
	return `Render SVG or HTML source code into a real PNG image file using headless Chrome. ` +
		`Use this to "generate images": write SVG (preferred for drawings/diagrams) or HTML/CSS, then call this tool to produce the actual image file. ` +
		`Provide exactly one of "svg" or "html", plus "output_path" (.png).`
}

func (t *GenerateImageTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"svg": {"type": "string", "description": "Complete SVG source (<svg ...>...</svg>)."},
			"html": {"type": "string", "description": "Complete HTML document to render."},
			"output_path": {"type": "string", "description": "Output PNG file path."},
			"width": {"type": "number", "description": "Viewport width px (default 800)."},
			"height": {"type": "number", "description": "Viewport height px (default 600)."}
		},
		"required": ["output_path"]
	}`)
}

func (t *GenerateImageTool) IsReadOnly(json.RawMessage) bool          { return false }
func (t *GenerateImageTool) IsConcurrencySafe(json.RawMessage) bool   { return false }
func (t *GenerateImageTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyInPlanMode(tctx, "GenerateImage")
}

func (t *GenerateImageTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in generateImageInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "输入解析失败: " + err.Error(), IsError: true}, nil
	}
	if (in.SVG == "") == (in.HTML == "") {
		return &tool.ToolResult{Content: "必须且只能提供 svg 或 html 之一", IsError: true}, nil
	}
	outPath, err := resolveOutPath(in.OutputPath, tctx.Cwd)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	htmlContent := in.HTML
	if in.SVG != "" {
		htmlContent = "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><style>html,body{margin:0;padding:0;background:#fff}</style></head><body>" + in.SVG + "</body></html>"
	}
	eng := media.NewEngine(filepath.Dir(outPath))
	if in.Width > 0 {
		eng.Width = in.Width
	} else {
		eng.Width = 800
	}
	if in.Height > 0 {
		eng.Height = in.Height
	} else {
		eng.Height = 600
	}
	name := strings.TrimSuffix(filepath.Base(outPath), filepath.Ext(outPath))
	r := eng.RenderHTMLString(ctx, htmlContent, name, "png")
	if r.Error != "" {
		return &tool.ToolResult{Content: "渲染失败: " + r.Error, IsError: true}, nil
	}
	// RenderHTMLString 输出到 <dir>/<name>.png, 与 outPath 对齐
	rendered := filepath.Join(filepath.Dir(outPath), name+".png")
	if rendered != outPath {
		_ = os.Rename(rendered, outPath)
	}
	st, err := os.Stat(outPath)
	if err != nil || st.Size() == 0 {
		return &tool.ToolResult{Content: "渲染后未生成有效 PNG: " + outPath, IsError: true}, nil
	}
	return &tool.ToolResult{Content: fmt.Sprintf("PNG 已生成: %s (%d bytes, %dx%d viewport)", outPath, st.Size(), eng.Width, eng.Height)}, nil
}

// ============================================================================
// GenerateVideo: Python 脚本 → mp4 (ffprobe 校验)
// ============================================================================

const GenerateVideoToolName = "GenerateVideo"

type generateVideoInput struct {
	PythonCode string `json:"python_code"`
	OutputPath string `json:"output_path"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
}

type GenerateVideoTool struct{}

func NewGenerateVideoTool() *GenerateVideoTool { return &GenerateVideoTool{} }

func (t *GenerateVideoTool) Name() string { return GenerateVideoToolName }

func (t *GenerateVideoTool) Description() string {
	return `Generate a real video file (.mp4) by executing a Python script you write. ` +
		`Write python code (matplotlib.animation with FFMpegWriter, or render frames and call ffmpeg via subprocess) that saves the video to the path given in environment variable OUTPUT_PATH (also sys.argv[1]). ` +
		`matplotlib, numpy and ffmpeg are available. The tool runs the script, then verifies the output with ffprobe and reports duration/resolution.`
}

func (t *GenerateVideoTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"python_code": {"type": "string", "description": "Python script that writes the video to OUTPUT_PATH (env var / sys.argv[1])."},
			"output_path": {"type": "string", "description": "Output .mp4 file path."},
			"timeout_sec": {"type": "number", "description": "Script timeout in seconds (default 180)."}
		},
		"required": ["python_code", "output_path"]
	}`)
}

func (t *GenerateVideoTool) IsReadOnly(json.RawMessage) bool        { return false }
func (t *GenerateVideoTool) IsConcurrencySafe(json.RawMessage) bool { return false }
func (t *GenerateVideoTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyInPlanMode(tctx, "GenerateVideo")
}

func (t *GenerateVideoTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in generateVideoInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "输入解析失败: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(in.PythonCode) == "" {
		return &tool.ToolResult{Content: "python_code 不能为空", IsError: true}, nil
	}
	outPath, err := resolveOutPath(in.OutputPath, tctx.Cwd)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	timeout := 180 * time.Second
	if in.TimeoutSec > 0 {
		timeout = time.Duration(in.TimeoutSec) * time.Second
	}
	scriptFile, err := os.CreateTemp("", "claude-go-video-*.py")
	if err != nil {
		return &tool.ToolResult{Content: "创建临时脚本失败: " + err.Error(), IsError: true}, nil
	}
	defer os.Remove(scriptFile.Name())
	if _, err := scriptFile.WriteString(in.PythonCode); err != nil {
		return &tool.ToolResult{Content: "写入脚本失败: " + err.Error(), IsError: true}, nil
	}
	_ = scriptFile.Close()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "python3", scriptFile.Name(), outPath)
	cmd.Dir = tctx.Cwd
	cmd.Env = append(os.Environ(), "OUTPUT_PATH="+outPath, "MPLBACKEND=Agg")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("脚本执行失败: %v\n%s", err, tailStr(string(out), 2000)), IsError: true}, nil
	}
	info, err := ffprobeVideo(outPath)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("脚本执行成功但输出校验失败: %v\n脚本输出: %s", err, tailStr(string(out), 1000)), IsError: true}, nil
	}
	return &tool.ToolResult{Content: fmt.Sprintf("视频已生成并通过 ffprobe 校验: %s (%s)", outPath, info)}, nil
}

// ffprobeVideo 校验视频文件并返回 "时长/分辨率/编码" 摘要。
func ffprobeVideo(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("输出文件不存在: %s", path)
	}
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", path).Output()
	if err != nil {
		return "", fmt.Errorf("ffprobe 失败: %w", err)
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return "", err
	}
	for _, s := range probe.Streams {
		if s.CodecType == "video" {
			return fmt.Sprintf("%d bytes, %ss, %dx%d, %s", st.Size(), probe.Format.Duration, s.Width, s.Height, s.CodecName), nil
		}
	}
	return "", fmt.Errorf("文件中没有视频流")
}

// ============================================================================
// GenerateSpeech: 文本 → WAV (piper 神经网络 TTS, 中文/降级用 espeak-ng)
// ============================================================================

const GenerateSpeechToolName = "GenerateSpeech"

type generateSpeechInput struct {
	Text       string `json:"text"`
	OutputPath string `json:"output_path"`
	Engine     string `json:"engine,omitempty"` // auto|piper|espeak
}

type GenerateSpeechTool struct{}

func NewGenerateSpeechTool() *GenerateSpeechTool { return &GenerateSpeechTool{} }

func (t *GenerateSpeechTool) Name() string { return GenerateSpeechToolName }

func (t *GenerateSpeechTool) Description() string {
	return `Synthesize real speech audio (.wav) from text. ` +
		`Uses piper neural TTS for English (natural voice) and espeak-ng for Chinese/other languages. ` +
		`Provide "text" and "output_path" (.wav).`
}

func (t *GenerateSpeechTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"text": {"type": "string", "description": "Text to speak."},
			"output_path": {"type": "string", "description": "Output .wav file path."},
			"engine": {"type": "string", "enum": ["auto", "piper", "espeak"], "description": "TTS engine (default auto: piper for English, espeak-ng for CJK)."}
		},
		"required": ["text", "output_path"]
	}`)
}

func (t *GenerateSpeechTool) IsReadOnly(json.RawMessage) bool        { return false }
func (t *GenerateSpeechTool) IsConcurrencySafe(json.RawMessage) bool { return false }
func (t *GenerateSpeechTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyInPlanMode(tctx, "GenerateSpeech")
}

func (t *GenerateSpeechTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in generateSpeechInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "输入解析失败: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(in.Text) == "" {
		return &tool.ToolResult{Content: "text 不能为空", IsError: true}, nil
	}
	outPath, err := resolveOutPath(in.OutputPath, tctx.Cwd)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	engine := in.Engine
	if engine == "" || engine == "auto" {
		if containsCJK(in.Text) {
			engine = "espeak"
		} else if piperBin() != "" && piperVoice() != "" {
			engine = "piper"
		} else {
			engine = "espeak"
		}
	}

	var usedDesc string
	switch engine {
	case "piper":
		bin, voice := piperBin(), piperVoice()
		if bin == "" || voice == "" {
			return &tool.ToolResult{Content: "piper 不可用 (未找到二进制或语音模型), 可改用 engine=espeak", IsError: true}, nil
		}
		runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(runCtx, bin, "-m", voice, "-f", outPath)
		cmd.Stdin = strings.NewReader(in.Text)
		cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Dir(bin))
		if out, err := cmd.CombinedOutput(); err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("piper 合成失败: %v\n%s", err, tailStr(string(out), 500)), IsError: true}, nil
		}
		usedDesc = "piper 神经网络 TTS (" + filepath.Base(voice) + ")"
	case "espeak":
		lang := "en"
		if containsCJK(in.Text) {
			lang = "cmn"
		}
		runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(runCtx, "espeak-ng", "-v", lang, "-w", outPath, in.Text)
		if out, err := cmd.CombinedOutput(); err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("espeak-ng 合成失败: %v\n%s", err, tailStr(string(out), 500)), IsError: true}, nil
		}
		usedDesc = "espeak-ng (" + lang + ")"
	default:
		return &tool.ToolResult{Content: "未知 engine: " + engine, IsError: true}, nil
	}

	st, err := os.Stat(outPath)
	if err != nil || st.Size() < 100 {
		return &tool.ToolResult{Content: "合成后未生成有效 WAV: " + outPath, IsError: true}, nil
	}
	dur := ""
	if out, err := exec.Command("ffprobe", "-v", "quiet", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", outPath).Output(); err == nil {
		dur = strings.TrimSpace(string(out)) + "s, "
	}
	return &tool.ToolResult{Content: fmt.Sprintf("语音已合成: %s (%s%d bytes, 引擎: %s)", outPath, dur, st.Size(), usedDesc)}, nil
}

// piperBin 定位 piper 二进制: 环境变量 > PATH > ~/piper/piper。
func piperBin() string {
	if v := os.Getenv("CLAUDE_GO_PIPER_BIN"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	if p, err := exec.LookPath("piper"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, "piper", "piper")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// piperVoice 定位 piper 语音模型 (.onnx): 环境变量 > ~/piper-voices/ 下第一个。
func piperVoice() string {
	if v := os.Getenv("CLAUDE_GO_PIPER_VOICE"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, "piper-voices", "*.onnx"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
