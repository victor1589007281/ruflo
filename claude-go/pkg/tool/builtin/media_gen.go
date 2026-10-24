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

// denyInPlanMode 在 Plan 模式 (只读规划期) 下拒绝生成类工具; 其他模式返回 nil 放行。
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

// NewGenerateImageTool 构造 GenerateImage 工具。
func NewGenerateImageTool() *GenerateImageTool { return &GenerateImageTool{} }

// Name 返回工具名 (GenerateImage)。
func (t *GenerateImageTool) Name() string { return GenerateImageToolName }

// Description 返回给模型的说明: 写 SVG/HTML 源码, 由无头 Chrome 渲染成真实 PNG 文件。
func (t *GenerateImageTool) Description() string {
	return `Render SVG or HTML source code into a real PNG image file using headless Chrome. ` +
		`Use this to "generate images": write SVG (preferred for drawings/diagrams) or HTML/CSS, then call this tool to produce the actual image file. ` +
		`Provide exactly one of "svg" or "html", plus "output_path" (.png).`
}

// InputSchema 返回入参 schema: svg 与 html 二选一, output_path 必填, 宽高可选。
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

// IsReadOnly 返回 false: 会写出 PNG 文件。
func (t *GenerateImageTool) IsReadOnly(json.RawMessage) bool        { return false }
// IsConcurrencySafe 返回 false: 每次调用都要起一个无头浏览器进程。
func (t *GenerateImageTool) IsConcurrencySafe(json.RawMessage) bool { return false }
// CheckPermissions 在 Plan 模式下拒绝执行, 其余交给通用权限链。
func (t *GenerateImageTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyInPlanMode(tctx, "GenerateImage")
}

// Call 把 SVG/HTML 渲染成 PNG: 归一化输出路径 → 包成完整 HTML → 无头渲染 → 校验产物非空。
//
// 算法解读：
//
//	原理：文本模型只能产出源码, 图必须由渲染器画 —— 这里把 SVG 包进一个最小 HTML 文档
//	  (清掉默认边距与背景), 交给 media.Engine 用无头 Chrome 按视口截图, 落成真实 PNG。
//	关键不变量：**渲染返回后必须 stat 到非空文件**。渲染器可能"成功返回却没写出文件"
//	  (浏览器起不来、输出目录不匹配), 只信返回值就会报一个不存在的产物。同理引擎按
//	  <dir>/<name>.<ext> 落盘, 与调用方给的 outPath 未必同名, 故显式 rename 对齐。
//	代价与边界：一次调用 = 一个浏览器进程, 百毫秒到秒级; svg 与 html 必须**恰好给一个**
//	  (都给或都不给都拒绝), 因为两者渲染出的东西完全不同。
//	设计权衡：不在这里预校验 SVG 合法性 —— 渲染器给出的报错比手写校验准确, 直接把原文
//	  回给模型让它自己改, 比猜"哪里不合法"更省一轮。
func (t *GenerateImageTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in generateImageInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "输入解析失败: " + err.Error(), IsError: true}, nil
	}
	// 异或: 必须恰好提供 svg 或 html 之一 (两个都给会渲染出不确定的东西)。
	if (in.SVG == "") == (in.HTML == "") {
		return &tool.ToolResult{Content: "必须且只能提供 svg 或 html 之一", IsError: true}, nil
	}
	outPath, err := resolveOutPath(in.OutputPath, tctx.Cwd)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	htmlContent := in.HTML
	if in.SVG != "" {
		// SVG 必须包进 HTML: 浏览器直接打开 .svg 不会按视口截图 (尺寸由 viewBox 定、底色透明)。
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

// NewGenerateVideoTool 构造 GenerateVideo 工具。
func NewGenerateVideoTool() *GenerateVideoTool { return &GenerateVideoTool{} }

// Name 返回工具名 (GenerateVideo)。
func (t *GenerateVideoTool) Name() string { return GenerateVideoToolName }

// Description 返回给模型的说明: 写 Python 脚本把视频写到 OUTPUT_PATH, 工具执行后用 ffprobe 复核。
func (t *GenerateVideoTool) Description() string {
	return `Generate a real video file (.mp4) by executing a Python script you write. ` +
		`Write python code (matplotlib.animation with FFMpegWriter, or render frames and call ffmpeg via subprocess) that saves the video to the path given in environment variable OUTPUT_PATH (also sys.argv[1]). ` +
		`matplotlib, numpy and ffmpeg are available. The tool runs the script, then verifies the output with ffprobe and reports duration/resolution.`
}

// InputSchema 返回入参 schema: python_code 与 output_path 必填, timeout_sec 可选。
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

// IsReadOnly 返回 false: 会写出 mp4 文件。
func (t *GenerateVideoTool) IsReadOnly(json.RawMessage) bool        { return false }
// IsConcurrencySafe 返回 false: 会起外部 python 进程并写文件。
func (t *GenerateVideoTool) IsConcurrencySafe(json.RawMessage) bool { return false }
// CheckPermissions 在 Plan 模式下拒绝执行。
func (t *GenerateVideoTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyInPlanMode(tctx, "GenerateVideo")
}

// Call 执行模型写的 Python 脚本产出 mp4, 并用 ffprobe 复核产物确实是一个视频。
//
// 算法解读：
//
//	原理：视频不能由文本模型直接生成, 于是把"怎么造视频"交给模型写代码 (matplotlib +
//	  ffmpeg), 工具只负责在受控环境里执行, 并**验证产物**。
//	关键不变量：**脚本退出码为 0 不等于成功**, 必须再过一道 ffprobe。脚本完全可能跑完
//	  却没写出文件, 或写出 0 字节/不含视频流的容器; 只信退出码就会把假产物当成功回给
//	  模型, 后续步骤全部建立在幻觉上。故产物校验失败与脚本失败一样判 IsError。
//	代价与边界：脚本以临时文件落到系统 tmp (defer 删除), 工作目录设为会话 cwd; 默认
//	  超时 180 秒, 超时由 ctx 杀进程。输出目录由 resolveOutPath 预先建好。
//	设计权衡：OUTPUT_PATH 同时以环境变量与 argv[1] 给出 —— 模型两种写法都常见, 给两条
//	  路比在描述里规定一条更不容易踩空。
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
	// 脚本落到系统临时目录而不是会话 cwd: 它是执行产物, 不该污染用户的工作区。
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
	// MPLBACKEND=Agg: 无显示环境下 matplotlib 必须用非交互后端, 否则 import 阶段就崩。
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
//
// 算法解读：
//
//	原理：调 ffprobe 拿 JSON, 取出 format.duration 与 streams[] 里的视频流信息。
//	关键不变量：**必须存在 codec_type=="video" 的流才算成功**。只有音频流的 mp4 与
//	  截断的容器, ffprobe 都会正常返回 —— 只看退出码会把这类文件当成视频收下。
//	代价与边界：起一个外部进程 + 解析 JSON; 只取第一个视频流, 多视频流 (画中画) 时
//	  报出的分辨率未必是主画面。
//	设计权衡：用 ffprobe 而不是自己读文件头 —— 容器格式太多, 让专业工具判更可靠。
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

// NewGenerateSpeechTool 构造 GenerateSpeech 工具。
func NewGenerateSpeechTool() *GenerateSpeechTool { return &GenerateSpeechTool{} }

// Name 返回工具名 (GenerateSpeech)。
func (t *GenerateSpeechTool) Name() string { return GenerateSpeechToolName }

// Description 返回给模型的说明: 文本转真实语音 WAV, 英文走 piper 神经 TTS, 中文走 espeak-ng。
func (t *GenerateSpeechTool) Description() string {
	return `Synthesize real speech audio (.wav) from text. ` +
		`Uses piper neural TTS for English (natural voice) and espeak-ng for Chinese/other languages. ` +
		`Provide "text" and "output_path" (.wav).`
}

// InputSchema 返回入参 schema: text 与 output_path 必填, engine 可选 (auto/piper/espeak)。
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

// IsReadOnly 返回 false: 会写出 WAV 文件。
func (t *GenerateSpeechTool) IsReadOnly(json.RawMessage) bool        { return false }
// IsConcurrencySafe 返回 false: 会起外部 TTS 进程并写文件。
func (t *GenerateSpeechTool) IsConcurrencySafe(json.RawMessage) bool { return false }
// CheckPermissions 在 Plan 模式下拒绝执行。
func (t *GenerateSpeechTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyInPlanMode(tctx, "GenerateSpeech")
}

// Call 把文本合成为 WAV: 选引擎 (auto 时按语种与可用性决定) → 起 TTS 进程 → 校验产物。
//
// 算法解读：
//
//	原理：engine=auto 的选择规则是 —— 含 CJK 走 espeak-ng (piper 的英文模型读中文只会
//	  得到空白音频); 纯英文且 piper 二进制与语音模型都在才走 piper (音质更好); 否则降级
//	  espeak-ng。
//	关键不变量：**piper 可用性 = 二进制 + 语音模型两个条件**。只判二进制会起一个缺模型的
//	  进程, 报出来的错与 TTS 毫无关系, 极难归因; 所以显式要求两者都非空。
//	代价与边界：合成超时 120 秒; 产物小于 100 字节即判失败 (一个合法 WAV 头就有一百多
//	  字节, 用它当"非空"的下界)。时长用 ffprobe 取, 取不到只是不报时长, 不影响成败。
//	设计权衡：显式 engine=piper 而 piper 不可用时**报错**而不是悄悄降级 —— 用户点名了
//	  引擎, 悄悄换掉会让他以为听到的是 piper 的音。只有 auto 才允许降级。
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
		// piper 的英文语音模型读中文会得到空白音频, 所以含 CJK 时直接走 espeak-ng。
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
		// 文本走 stdin 而不是 argv: 长文本会撞上命令行长度上限。
		cmd.Stdin = strings.NewReader(in.Text)
		// piper 的动态库与二进制同目录, 不设 LD_LIBRARY_PATH 会报找不到 libespeak-ng。
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

// ============================================================================
// GenerateGIF: HTML(CSS动画) → 动图 GIF (确定性逐帧 + 调色板编码)
// ============================================================================

const GenerateGIFToolName = "GenerateGIF"

type generateGIFInput struct {
	HTML        string `json:"html"`
	OutputPath  string `json:"output_path"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	FPS         int    `json:"fps,omitempty"`
	DurationSec int    `json:"duration_sec,omitempty"`
}

type GenerateGIFTool struct{}

// NewGenerateGIFTool 构造 GenerateGIF 工具。
func NewGenerateGIFTool() *GenerateGIFTool { return &GenerateGIFTool{} }

// Name 返回工具名 (GenerateGIF)。
func (t *GenerateGIFTool) Name() string { return GenerateGIFToolName }

// Description 返回给模型的说明: 用 CSS @keyframes 写动画, 渲染器逐帧确定性截图后编码成 GIF。
func (t *GenerateGIFTool) Description() string {
	return `Render an animated GIF from an HTML document that contains CSS @keyframes animations. ` +
		`Write HTML/CSS where the motion is expressed via CSS animations (the renderer deterministically seeks each frame, so playback speed is exact). ` +
		`Provide "html" and "output_path" (.gif). Optional: width, height, fps (default 15), duration_sec (default 4).`
}

// InputSchema 返回入参 schema: html 与 output_path 必填, 宽高/fps/时长可选。
func (t *GenerateGIFTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"html": {"type": "string", "description": "Complete HTML document with CSS @keyframes animation."},
			"output_path": {"type": "string", "description": "Output .gif file path."},
			"width": {"type": "number", "description": "Viewport width px (default 800)."},
			"height": {"type": "number", "description": "Viewport height px (default 600)."},
			"fps": {"type": "number", "description": "Frames per second (default 15)."},
			"duration_sec": {"type": "number", "description": "Animation length in seconds (default 4)."}
		},
		"required": ["html", "output_path"]
	}`)
}

// IsReadOnly 返回 false: 会写出 GIF 文件。
func (t *GenerateGIFTool) IsReadOnly(json.RawMessage) bool        { return false }
// IsConcurrencySafe 返回 false: 会起无头浏览器逐帧截图。
func (t *GenerateGIFTool) IsConcurrencySafe(json.RawMessage) bool { return false }
// CheckPermissions 在 Plan 模式下拒绝执行。
func (t *GenerateGIFTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyInPlanMode(tctx, "GenerateGIF")
}

// Call 把带 CSS 动画的 HTML 渲染成动图 GIF。
//
// 算法解读：
//
//	原理：GIF 是逐帧图, 而浏览器动画按真实时间跑 —— 直接录屏会得到不稳定的帧数与速度。
//	  这里让渲染器**确定性地 seek 每一帧** (由 fps 与 duration_sec 算出帧数与时间点),
//	  所以播放速度是精确的, 与机器快慢无关。
//	关键不变量：帧数与取帧时刻只由 fps/duration_sec 决定 (默认 15fps / 4 秒), 与渲染耗时
//	  无关 —— 否则同一个 HTML 在不同机器上会产出不同的 GIF, 无法复现。
//	代价与边界：一次调用 = 一个浏览器进程 + N 帧截图 + 一次调色板编码, 秒级到十秒级;
//	  fps/duration 传 0 时由引擎补默认值 (本函数不自己判零)。
//	设计权衡：不做"录屏式"捕捉 (帧数会随机器性能浮动, 不可复现); 代价是动画必须写成
//	  CSS @keyframes 才能被 seek —— 这条约定写进了工具描述, 由模型侧遵守。
func (t *GenerateGIFTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in generateGIFInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "输入解析失败: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(in.HTML) == "" {
		return &tool.ToolResult{Content: "html 不能为空", IsError: true}, nil
	}
	outPath, err := resolveOutPath(in.OutputPath, tctx.Cwd)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	eng := media.NewEngine(filepath.Dir(outPath))
	eng.Width = pick(in.Width, 800)
	eng.Height = pick(in.Height, 600)
	eng.FPS = in.FPS
	eng.DurationSec = in.DurationSec
	name := strings.TrimSuffix(filepath.Base(outPath), filepath.Ext(outPath))
	r := eng.RenderHTMLString(ctx, in.HTML, name, "gif")
	if r.Error != "" {
		return &tool.ToolResult{Content: "GIF 渲染失败: " + r.Error, IsError: true}, nil
	}
	rendered := filepath.Join(filepath.Dir(outPath), name+".gif")
	if rendered != outPath {
		_ = os.Rename(rendered, outPath)
	}
	st, err := os.Stat(outPath)
	if err != nil || st.Size() == 0 {
		return &tool.ToolResult{Content: "渲染后未生成有效 GIF: " + outPath, IsError: true}, nil
	}
	return &tool.ToolResult{Content: fmt.Sprintf("GIF 已生成: %s (%d bytes)", outPath, st.Size())}, nil
}

// ============================================================================
// GeneratePPTX: 结构化 slides → 可编辑 PPTX (或 HTML → 高保真整页截图)
// ============================================================================

const GeneratePPTXToolName = "GeneratePPTX"

type generatePPTXInput struct {
	Slides []media.EditableSlide `json:"slides,omitempty"` // 结构化(可编辑)模式
	HTML   string                `json:"html,omitempty"`   // 高保真截图模式
	Title  string                `json:"title,omitempty"`
	Output string                `json:"output_path"`
}

type GeneratePPTXTool struct{}

// NewGeneratePPTXTool 构造 GeneratePPTX 工具。
func NewGeneratePPTXTool() *GeneratePPTXTool { return &GeneratePPTXTool{} }

// Name 返回工具名 (GeneratePPTX)。
func (t *GeneratePPTXTool) Name() string { return GeneratePPTXToolName }

// Description 返回给模型的说明: 两种模式二选一 —— slides 产可编辑文本框, html 产高保真整页截图。
func (t *GeneratePPTXTool) Description() string {
	return `Generate a PowerPoint .pptx file. Two modes: ` +
		`(1) EDITABLE — provide "slides": [{title, bullets:[...], subtitle, notes}], producing real editable text boxes you can edit in PowerPoint/WPS/Keynote (preferred for decks the user will further edit). ` +
		`(2) HIGH-FIDELITY — provide "html" with multiple <section class="slide">...</section>, each rendered as a full-page screenshot (pixel-perfect but not editable). ` +
		`Provide exactly one of "slides" or "html", plus "output_path" (.pptx).`
}

// InputSchema 返回入参 schema: slides 与 html 二选一, output_path 必填。
func (t *GeneratePPTXTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"slides": {"type": "array", "description": "Editable mode: array of {title, bullets:[string], subtitle?, notes?}.",
				"items": {"type": "object", "properties": {
					"title": {"type": "string"},
					"bullets": {"type": "array", "items": {"type": "string"}},
					"subtitle": {"type": "string"},
					"notes": {"type": "string"}
				}}},
			"html": {"type": "string", "description": "High-fidelity mode: HTML with multiple <section class=\"slide\">."},
			"title": {"type": "string", "description": "Deck title (optional)."},
			"output_path": {"type": "string", "description": "Output .pptx file path."}
		},
		"required": ["output_path"]
	}`)
}

// IsReadOnly 返回 false: 会写出 pptx 文件。
func (t *GeneratePPTXTool) IsReadOnly(json.RawMessage) bool        { return false }
// IsConcurrencySafe 返回 false: 高保真模式会起无头浏览器, 可编辑模式会写文件。
func (t *GeneratePPTXTool) IsConcurrencySafe(json.RawMessage) bool { return false }
// CheckPermissions 在 Plan 模式下拒绝执行。
func (t *GeneratePPTXTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyInPlanMode(tctx, "GeneratePPTX")
}

// Call 生成 PPTX, 按输入分派到两种互斥模式之一。
//
// 可编辑模式 (给了 slides): 产出真实文本框, 用户能在 PowerPoint/WPS/Keynote 里继续改;
// 高保真模式 (给了 html): 每个 <section class="slide"> 截成整页图, 像素级还原但不可编辑。
// 选哪个是调用方的判断 —— 需要后续编辑的演示稿用 slides, 只求还原设计的用 html。
func (t *GeneratePPTXTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in generatePPTXInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "输入解析失败: " + err.Error(), IsError: true}, nil
	}
	// 异或: 必须恰好提供 slides(可编辑) 或 html(高保真) 之一。
	if (len(in.Slides) == 0) == (strings.TrimSpace(in.HTML) == "") {
		return &tool.ToolResult{Content: "必须且只能提供 slides(可编辑) 或 html(高保真) 之一", IsError: true}, nil
	}
	outPath, err := resolveOutPath(in.Output, tctx.Cwd)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	if len(in.Slides) > 0 {
		// 可编辑模式
		if err := media.BuildEditablePPTX(outPath, media.EditableDeck{Title: in.Title, Slides: in.Slides}); err != nil {
			return &tool.ToolResult{Content: "可编辑 PPTX 生成失败: " + err.Error(), IsError: true}, nil
		}
		st, _ := os.Stat(outPath)
		return &tool.ToolResult{Content: fmt.Sprintf("可编辑 PPTX 已生成: %s (%d 页, %d bytes, 文本可在 PowerPoint 中直接编辑)", outPath, len(in.Slides), fileSizeOf(st))}, nil
	}

	// 高保真截图模式
	eng := media.NewEngine(filepath.Dir(outPath))
	name := strings.TrimSuffix(filepath.Base(outPath), filepath.Ext(outPath))
	r := eng.RenderHTMLString(ctx, in.HTML, name, "pptx")
	if r.Error != "" {
		return &tool.ToolResult{Content: "高保真 PPTX 渲染失败: " + r.Error, IsError: true}, nil
	}
	rendered := filepath.Join(filepath.Dir(outPath), name+".pptx")
	if rendered != outPath {
		_ = os.Rename(rendered, outPath)
	}
	st, err := os.Stat(outPath)
	if err != nil || st.Size() == 0 {
		return &tool.ToolResult{Content: "渲染后未生成有效 PPTX: " + outPath, IsError: true}, nil
	}
	return &tool.ToolResult{Content: fmt.Sprintf("高保真 PPTX 已生成: %s (%d bytes, 每页为整页图、不可编辑)", outPath, st.Size())}, nil
}

// ============================================================================
// GenerateChart: ECharts option(JSON) → 图表 PNG
// ============================================================================

const GenerateChartToolName = "GenerateChart"

type generateChartInput struct {
	Option     json.RawMessage `json:"option"`
	OutputPath string          `json:"output_path"`
	Width      int             `json:"width,omitempty"`
	Height     int             `json:"height,omitempty"`
}

type GenerateChartTool struct{}

// NewGenerateChartTool 构造 GenerateChart 工具。
func NewGenerateChartTool() *GenerateChartTool { return &GenerateChartTool{} }

// Name 返回工具名 (GenerateChart)。
func (t *GenerateChartTool) Name() string { return GenerateChartToolName }

// Description 返回给模型的说明: 传一个 ECharts option 对象, 渲染成数据图表 PNG。
func (t *GenerateChartTool) Description() string {
	return `Render a data chart to PNG using Apache ECharts. ` +
		`Provide "option": a valid ECharts option object (bar/line/pie/scatter/candlestick/radar/sankey/map/...), plus "output_path" (.png). ` +
		`Use this for data visualizations; for flowcharts/diagrams prefer mermaid. Optional: width (default 1000), height (default 600).`
}

// InputSchema 返回入参 schema: option 与 output_path 必填, 宽高可选。
func (t *GenerateChartTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"option": {"type": "object", "description": "ECharts option object (the argument to setOption)."},
			"output_path": {"type": "string", "description": "Output PNG file path."},
			"width": {"type": "number", "description": "Chart width px (default 1000)."},
			"height": {"type": "number", "description": "Chart height px (default 600)."}
		},
		"required": ["option", "output_path"]
	}`)
}

// IsReadOnly 返回 false: 会写出 PNG 文件。
func (t *GenerateChartTool) IsReadOnly(json.RawMessage) bool        { return false }
// IsConcurrencySafe 返回 false: 会起无头浏览器渲染图表。
func (t *GenerateChartTool) IsConcurrencySafe(json.RawMessage) bool { return false }
// CheckPermissions 在 Plan 模式下拒绝执行。
func (t *GenerateChartTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyInPlanMode(tctx, "GenerateChart")
}

// Call 把 ECharts option(JSON) 渲染成 PNG: 包成 HTML 页面 → 无头渲染 → 对齐输出路径。
//
// 视口比图表本身各多 40px —— 留出坐标轴标签与图例的边距, 否则贴边的文字会被裁掉。
func (t *GenerateChartTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in generateChartInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "输入解析失败: " + err.Error(), IsError: true}, nil
	}
	if len(in.Option) == 0 {
		return &tool.ToolResult{Content: "option 不能为空", IsError: true}, nil
	}
	outPath, err := resolveOutPath(in.OutputPath, tctx.Cwd)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	w, h := pick(in.Width, 1000), pick(in.Height, 600)
	html := media.BuildEChartsHTML(string(in.Option), w, h)
	eng := media.NewEngine(filepath.Dir(outPath))
	eng.Width = w + 40
	eng.Height = h + 40
	name := strings.TrimSuffix(filepath.Base(outPath), filepath.Ext(outPath))
	r := eng.RenderHTMLString(ctx, html, name, "png")
	if r.Error != "" {
		return &tool.ToolResult{Content: "图表渲染失败: " + r.Error, IsError: true}, nil
	}
	rendered := filepath.Join(filepath.Dir(outPath), name+".png")
	if rendered != outPath {
		_ = os.Rename(rendered, outPath)
	}
	st, err := os.Stat(outPath)
	if err != nil || st.Size() == 0 {
		return &tool.ToolResult{Content: "渲染后未生成有效 PNG: " + outPath, IsError: true}, nil
	}
	return &tool.ToolResult{Content: fmt.Sprintf("图表已生成: %s (%d bytes, %dx%d)", outPath, st.Size(), w, h)}, nil
}

// pick 返回 v (当 v > 0), 否则返回默认值 def。
func pick(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// fileSizeOf 安全取文件大小; st 为 nil 时返回 0。
func fileSizeOf(st os.FileInfo) int64 {
	if st == nil {
		return 0
	}
	return st.Size()
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

// containsCJK 报告字符串是否含中日韩文字 (汉字/平假名/片假名/谚文)。
func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// tailStr 取字符串末尾 n 个字节, 超长时在前面补 "..." —— 保留的是报错信息的结尾部分。
func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// ============================================================================
// 检索别名 (tool.RecallAliasProvider) —— D4.2「漏召率趋零」
// ============================================================================
//
// ── 这五个工具为什么需要它 ──────────────────────────────────────────────
//
// 它们的描述**全是英文**, 而且是被 schema 术语写满的英文 (output_path / .mp4 /
// 环境变量名)。模型用中文要东西 ("做个视频"、"画张图") 时, 词面检索一个共同
// 字符都没有 —— 结构上召不回, 不是排序没排好。
//
// 别名是那条通路, 且它**不动产品面**: 描述是下发给模型的原文, 把它写长会影响
// 每一次调用; 别名只进池检索索引。
//
// ⚠️ 匹配的是**整串小写子串** (recall.go 的 textHits), 不是关键词拼盘 —— 所以
// 下面每条都写成"人可能原样打出来的说法", 而不是把词拆开。写 "视频" 与写
// "视频生成" 在这里是两件事: 后者要求查询里连着出现这五个字。

// RecallAliases 返回 GenerateImage 的检索别名 (人可能原样打出的中文说法)。
func (t *GenerateImageTool) RecallAliases() []string {
	return []string{"图片生成", "生成图片", "画图", "画一张图", "生成图片文件"}
}

// RecallAliases 返回 GenerateVideo 的检索别名。
func (t *GenerateVideoTool) RecallAliases() []string {
	return []string{"视频生成", "生成视频", "做视频", "生成一个视频", "动画文件"}
}

// RecallAliases 返回 GenerateSpeech 的检索别名。
func (t *GenerateSpeechTool) RecallAliases() []string {
	return []string{"语音合成", "文字转语音", "文本转语音", "读出来", "生成音频", "配音"}
}

// RecallAliases 返回 GenerateGIF 的检索别名。
func (t *GenerateGIFTool) RecallAliases() []string {
	return []string{"动图", "生成动图", "gif 动画", "做一个动图"}
}

// RecallAliases 返回 GenerateChart 的检索别名。
func (t *GenerateChartTool) RecallAliases() []string {
	return []string{"图表", "生成图表", "画个图", "数据可视化", "柱状图", "折线图", "饼图"}
}

// RecallAliases 返回 GeneratePPTX 的检索别名。
func (t *GeneratePPTXTool) RecallAliases() []string {
	return []string{"幻灯片", "做个PPT", "生成PPT", "演示文稿", "汇报材料"}
}
