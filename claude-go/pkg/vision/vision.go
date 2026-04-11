// Package vision 基于阿里百炼 Coding Plan API (OpenAI 兼容) 的多模态视觉能力。
// 完全通过 qwen3.6-plus / kimi-k2.5 的视觉理解 + 文本生成能力实现：
//   - 图片理解: 发送 base64 图片 → LLM 返回描述/分析
//   - 文生图: LLM 生成 SVG/HTML 源码 → 调用方可渲染为图片
//   - 图生图: 理解原图 → LLM 生成改造后的 SVG/HTML
//   - 图生视频: 理解图片 → LLM 设计运镜脚本(关键帧描述) → 生成多帧 SVG → 组装为动画
package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultBaseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic"

// Client 封装阿里百炼 Coding Plan API 调用。
type Client struct {
	APIKey     string
	BaseURL    string
	Model      string
	HTTPClient *http.Client
}

// NewClient 使用阿里百炼 Coding Plan API。
func NewClient(apiKey string) *Client {
	return &Client{
		APIKey:  apiKey,
		BaseURL: defaultBaseURL,
		Model:   "qwen3.6-plus",
		HTTPClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// ImageResult 图像生成结果。
type ImageResult struct {
	SVG      string `json:"svg"`
	HTML     string `json:"html"`
	Desc     string `json:"description"`
	RawReply string `json:"raw_reply"`
}

// VideoResult 视频脚本/动画结果。
type VideoResult struct {
	Frames     []string `json:"frames"`
	Script     string   `json:"script"`
	AnimCSS    string   `json:"anim_css"`
	HTMLPlayer string   `json:"html_player"`
	RawReply   string   `json:"raw_reply"`
}

// doPost 底层 HTTP POST 调用，返回响应体。
func (c *Client) doPost(ctx context.Context, payload map[string]any) ([]byte, error) {
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/messages"

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(body, 512))
	}
	return body, nil
}

// parseTextResponse 从 Anthropic Messages 响应中提取文本。
func parseTextResponse(body []byte) (string, error) {
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("解析响应失败: %w (body=%s)", err, truncate(body, 256))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("API 错误: %s", parsed.Error.Message)
	}

	var sb strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("响应中无文本内容")
	}
	return sb.String(), nil
}

// callText 纯文本对话调用（content 为字符串）。
func (c *Client) callText(ctx context.Context, userText string) (string, error) {
	model := c.Model
	if model == "" {
		model = "qwen3.6-plus"
	}
	payload := map[string]any{
		"model":      model,
		"max_tokens": 8192,
		"messages": []map[string]any{
			{"role": "user", "content": userText},
		},
	}
	body, err := c.doPost(ctx, payload)
	if err != nil {
		return "", err
	}
	return parseTextResponse(body)
}

// callMultimodal 多模态调用（content 为 Anthropic content blocks 数组）。
func (c *Client) callMultimodal(ctx context.Context, contentBlocks []map[string]any) (string, error) {
	model := c.Model
	if model == "" {
		model = "qwen3.6-plus"
	}
	payload := map[string]any{
		"model":      model,
		"max_tokens": 8192,
		"messages": []map[string]any{
			{"role": "user", "content": contentBlocks},
		},
	}
	body, err := c.doPost(ctx, payload)
	if err != nil {
		return "", err
	}
	return parseTextResponse(body)
}

// Understand 图片理解: 发送 base64 图片，LLM 返回分析。
func (c *Client) Understand(ctx context.Context, imageBase64, prompt string) (string, error) {
	if strings.TrimSpace(imageBase64) == "" {
		return "", fmt.Errorf("imageBase64 不能为空")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "请详细描述这张图片的内容。"
	}

	dataURL := imageBase64
	if !strings.HasPrefix(dataURL, "data:") {
		dataURL = "data:image/jpeg;base64," + dataURL
	}

	contentBlocks := []map[string]any{
		{"type": "image", "source": map[string]any{"type": "url", "url": dataURL}},
		{"type": "text", "text": prompt},
	}
	return c.callMultimodal(ctx, contentBlocks)
}

// GenerateImage 文生图: LLM 根据提示词生成 SVG 源码。
func (c *Client) GenerateImage(ctx context.Context, prompt string, style string) (*ImageResult, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt 不能为空")
	}
	if style == "" {
		style = "现代扁平设计"
	}

	userText := fmt.Sprintf(`你是一位专业的 SVG 图像设计师。根据用户描述生成高质量 SVG 图像。

要求:
1. 输出完整的 SVG 代码，以 <svg> 开头 </svg> 结尾
2. 使用 viewBox="0 0 800 600"
3. 风格: %s
4. 色彩丰富、细节精致
5. 不要输出任何解释文字，只输出 SVG 代码

用户描述: %s`, style, prompt)

	reply, err := c.callText(ctx, userText)
	if err != nil {
		return nil, err
	}

	svg := extractBetween(reply, "<svg", "</svg>")
	if svg != "" {
		svg = "<svg" + svg + "</svg>"
	}

	return &ImageResult{
		SVG:      svg,
		RawReply: reply,
		Desc:     prompt,
	}, nil
}

// TransformImage 图生图: 理解原图 → 按指令改造 → 生成新 SVG。
func (c *Client) TransformImage(ctx context.Context, imageBase64, instruction string) (*ImageResult, error) {
	if strings.TrimSpace(imageBase64) == "" {
		return nil, fmt.Errorf("imageBase64 不能为空")
	}
	if strings.TrimSpace(instruction) == "" {
		instruction = "在保持原图构图的基础上，转换为矢量插画风格"
	}

	dataURL := imageBase64
	if !strings.HasPrefix(dataURL, "data:") {
		dataURL = "data:image/jpeg;base64," + dataURL
	}

	contentBlocks := []map[string]any{
		{"type": "image", "source": map[string]any{"type": "url", "url": dataURL}},
		{"type": "text", "text": fmt.Sprintf(`你是一位图像改造专家。请仔细观察这张图片，然后按照以下指令生成一张新的 SVG 图像:

改造指令: %s

要求:
1. 先分析原图的构图、色彩、主体
2. 根据改造指令重新设计
3. 输出完整 SVG 代码 (viewBox="0 0 800 600")
4. 保留原图的核心元素和构图比例
5. 只输出 SVG 代码，不要解释`, instruction)},
	}

	reply, err := c.callMultimodal(ctx, contentBlocks)
	if err != nil {
		return nil, err
	}

	svg := extractBetween(reply, "<svg", "</svg>")
	if svg != "" {
		svg = "<svg" + svg + "</svg>"
	}

	return &ImageResult{
		SVG:      svg,
		RawReply: reply,
		Desc:     instruction,
	}, nil
}

// GenerateVideo 图生视频: 理解图片 → 设计运镜脚本 → 生成多帧+CSS动画 → 输出 HTML Player。
func (c *Client) GenerateVideo(ctx context.Context, imageBase64, prompt string, frames int) (*VideoResult, error) {
	if frames <= 0 {
		frames = 6
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "从左到右缓慢平移，带有缩放效果"
	}

	// Step 1: 理解图片 + 设计运镜脚本
	scriptPrompt := fmt.Sprintf(`你是一位动画导演。请根据以下指令设计一个 %d 帧的运镜脚本。

运镜指令: %s

输出 JSON 格式的运镜脚本:
{
  "title": "动画标题",
  "duration_seconds": 总时长,
  "frames": [
    {
      "frame_id": 1,
      "time_offset": "0s",
      "camera": "描述镜头位置和角度",
      "scene_description": "这一帧的画面描述",
      "transition": "切换效果(fade/slide/zoom)"
    }
  ]
}

只输出 JSON，不要解释。`, frames, prompt)

	var scriptReply string
	var err error
	if strings.TrimSpace(imageBase64) != "" {
		dataURL := imageBase64
		if !strings.HasPrefix(dataURL, "data:") {
			dataURL = "data:image/jpeg;base64," + dataURL
		}
		contentBlocks := []map[string]any{
			{"type": "image", "source": map[string]any{"type": "url", "url": dataURL}},
			{"type": "text", "text": scriptPrompt},
		}
		scriptReply, err = c.callMultimodal(ctx, contentBlocks)
	} else {
		scriptReply, err = c.callText(ctx, scriptPrompt)
	}
	if err != nil {
		return nil, fmt.Errorf("运镜脚本生成失败: %w", err)
	}

	// Step 2: 根据运镜脚本，逐帧生成 SVG
	svgFrames := make([]string, 0, frames)
	for i := 1; i <= frames; i++ {
		framePrompt := fmt.Sprintf(`根据以下运镜脚本，生成第 %d/%d 帧的 SVG 图像。

运镜脚本:
%s

要求:
1. 输出完整 SVG 代码 (viewBox="0 0 800 600")
2. 该帧反映脚本中 frame_id=%d 的镜头描述
3. 与前后帧保持视觉连贯性
4. 只输出 SVG 代码`, i, frames, scriptReply, i)

		frameReply, err := c.callText(ctx, framePrompt)
		if err != nil {
			svgFrames = append(svgFrames, fmt.Sprintf(`<svg viewBox="0 0 800 600" xmlns="http://www.w3.org/2000/svg"><text x="400" y="300" text-anchor="middle">Frame %d (error)</text></svg>`, i))
			continue
		}

		svg := extractBetween(frameReply, "<svg", "</svg>")
		if svg != "" {
			svg = "<svg" + svg + "</svg>"
		} else {
			svg = fmt.Sprintf(`<svg viewBox="0 0 800 600" xmlns="http://www.w3.org/2000/svg"><text x="400" y="300" text-anchor="middle">Frame %d</text></svg>`, i)
		}
		svgFrames = append(svgFrames, svg)
	}

	// Step 3: 生成 HTML Player（CSS 关键帧动画拼接所有帧）
	htmlPlayer := buildHTMLPlayer(svgFrames, frames)

	return &VideoResult{
		Frames:     svgFrames,
		Script:     scriptReply,
		HTMLPlayer: htmlPlayer,
		RawReply:   scriptReply,
	}, nil
}

// buildHTMLPlayer 将多个 SVG 帧拼接为 CSS 动画 HTML。
func buildHTMLPlayer(svgFrames []string, totalFrames int) string {
	n := len(svgFrames)
	if n == 0 {
		return "<html><body><p>No frames</p></body></html>"
	}
	frameDuration := 2.0
	totalDuration := frameDuration * float64(n)
	stepPercent := 100.0 / float64(n)

	var keyframes strings.Builder
	for i := 0; i < n; i++ {
		start := stepPercent * float64(i)
		end := stepPercent * float64(i+1)
		if i == n-1 {
			end = 100.0
		}
		keyframes.WriteString(fmt.Sprintf("  %.1f%%, %.1f%% { opacity: 1; }\n", start, end-0.1))
		if i < n-1 {
			keyframes.WriteString(fmt.Sprintf("  %.1f%% { opacity: 0; }\n", end))
		}
	}

	var framesDivs strings.Builder
	for i, svg := range svgFrames {
		delay := frameDuration * float64(i)
		framesDivs.WriteString(fmt.Sprintf(
			`<div class="frame" style="animation-delay:%.1fs">%s</div>`+"\n", delay, svg))
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>AI Video Player - %d Frames</title>
<style>
body { margin:0; background:#000; display:flex; justify-content:center; align-items:center; height:100vh; }
.player { position:relative; width:800px; height:600px; }
.frame { position:absolute; top:0; left:0; width:100%%; height:100%%; opacity:0;
  animation: show %.1fs infinite; }
.frame:first-child { opacity:1; }
@keyframes show {
%s}
</style>
</head>
<body>
<div class="player">
%s</div>
</body>
</html>`, n, totalDuration, keyframes.String(), framesDivs.String())
}

// extractBetween 提取 startTag 和 endTag 之间的内容（不含 startTag 本身但含 endTag 之前的所有内容）。
func extractBetween(s, startTag, endTag string) string {
	lower := strings.ToLower(s)
	startIdx := strings.Index(lower, strings.ToLower(startTag))
	if startIdx < 0 {
		return ""
	}
	after := s[startIdx+len(startTag):]
	endIdx := strings.Index(strings.ToLower(after), strings.ToLower(endTag))
	if endIdx < 0 {
		return ""
	}
	return after[:endIdx]
}

func truncate(b []byte, max int) string {
	s := string(b)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
