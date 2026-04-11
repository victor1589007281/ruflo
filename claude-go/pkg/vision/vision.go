// Package vision 基于阿里百炼 Coding Plan API 的多模态视觉能力。
// 完全复用 claude-go 的 api.Client，不独立管理 HTTP 连接。
// 通过 qwen3.6-plus / kimi-k2.5 的视觉理解 + 文本生成能力实现：
//   - 图片理解: 发送 base64 图片 → LLM 返回描述/分析
//   - 文生图: LLM 生成 SVG 源码 → 调用方可渲染为图片
//   - 图生图: 理解原图 → LLM 生成改造后的 SVG
//   - 图生视频: LLM 设计运镜脚本 → 逐帧生成 SVG → CSS 关键帧动画 HTML
package vision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// LLMClient 对 api.Client 的最小依赖接口。
type LLMClient interface {
	SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
	RawComplete(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error)
}

// Client 封装视觉能力，复用 claude-go 的 api.Client。
type Client struct {
	LLM   LLMClient
	Model string
}

// NewClient 使用已有的 api.Client 构造 Vision 客户端。
func NewClient(llm LLMClient) *Client {
	return &Client{LLM: llm, Model: "qwen3.6-plus"}
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
	HTMLPlayer string   `json:"html_player"`
	RawReply   string   `json:"raw_reply"`
}

// Understand 图片理解: 发送 base64 图片，LLM 返回分析。
func (c *Client) Understand(ctx context.Context, imageBase64, prompt string) (string, error) {
	if strings.TrimSpace(imageBase64) == "" {
		return "", fmt.Errorf("imageBase64 不能为空")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "请详细描述这张图片的内容。"
	}

	b64, mediaType := parseImageInput(imageBase64)

	contentBlocks := []map[string]any{
		{"type": "image", "source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       b64,
		}},
		{"type": "text", "text": prompt},
	}
	raw, err := json.Marshal(contentBlocks)
	if err != nil {
		return "", fmt.Errorf("序列化内容失败: %w", err)
	}
	return c.LLM.RawComplete(ctx, raw, 4096)
}

// GenerateImage 文生图: LLM 根据提示词生成 SVG 源码。
func (c *Client) GenerateImage(ctx context.Context, prompt string, style string) (*ImageResult, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt 不能为空")
	}
	if style == "" {
		style = "现代扁平设计"
	}

	sysPrompt := "你是一位专业的 SVG 图像设计师。根据用户描述生成高质量 SVG 图像。" +
		"要求: 1.输出完整SVG代码,以<svg>开头</svg>结尾 2.使用viewBox=\"0 0 800 600\" " +
		"3.色彩丰富、细节精致 4.不要输出任何解释文字,只输出SVG代码"

	userText := fmt.Sprintf("风格: %s\n描述: %s", style, prompt)

	reply, err := c.LLM.SimpleComplete(ctx, sysPrompt, userText)
	if err != nil {
		return nil, err
	}

	svg := extractSVG(reply)
	return &ImageResult{SVG: svg, RawReply: reply, Desc: prompt}, nil
}

// TransformImage 图生图: 理解原图 → 按指令改造 → 生成新 SVG。
func (c *Client) TransformImage(ctx context.Context, imageBase64, instruction string) (*ImageResult, error) {
	if strings.TrimSpace(imageBase64) == "" {
		return nil, fmt.Errorf("imageBase64 不能为空")
	}
	if strings.TrimSpace(instruction) == "" {
		instruction = "在保持原图构图的基础上，转换为矢量插画风格"
	}

	b64, mediaType := parseImageInput(imageBase64)

	contentBlocks := []map[string]any{
		{"type": "image", "source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       b64,
		}},
		{"type": "text", "text": fmt.Sprintf(`你是一位图像改造专家。请仔细观察这张图片，然后按照以下指令生成一张新的 SVG 图像:

改造指令: %s

要求:
1. 先分析原图的构图、色彩、主体
2. 根据改造指令重新设计
3. 输出完整 SVG 代码 (viewBox="0 0 800 600")
4. 保留原图的核心元素和构图比例
5. 只输出 SVG 代码，不要解释`, instruction)},
	}

	raw, err := json.Marshal(contentBlocks)
	if err != nil {
		return nil, fmt.Errorf("序列化内容失败: %w", err)
	}

	reply, err := c.LLM.RawComplete(ctx, raw, 8192)
	if err != nil {
		return nil, err
	}

	svg := extractSVG(reply)
	return &ImageResult{SVG: svg, RawReply: reply, Desc: instruction}, nil
}

// GenerateVideo 图生视频: LLM 设计运镜脚本 → 逐帧生成 SVG → CSS 关键帧动画 HTML Player。
func (c *Client) GenerateVideo(ctx context.Context, imageBase64, prompt string, frames int) (*VideoResult, error) {
	if frames <= 0 {
		frames = 4
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "从左到右缓慢平移，带有缩放效果"
	}

	// Step 1: 设计运镜脚本
	scriptSys := "你是一位动画导演。设计运镜脚本，输出JSON格式。"
	scriptUser := fmt.Sprintf(`设计一个 %d 帧的运镜脚本。
运镜指令: %s
输出JSON:
{"title":"标题","frames":[{"frame_id":1,"camera":"镜头描述","scene":"画面描述","transition":"fade/zoom/slide"}]}
只输出JSON。`, frames, prompt)

	var scriptReply string
	var err error
	if strings.TrimSpace(imageBase64) != "" {
		b64, mediaType := parseImageInput(imageBase64)
		contentBlocks := []map[string]any{
			{"type": "image", "source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       b64,
			}},
			{"type": "text", "text": scriptUser},
		}
		raw, _ := json.Marshal(contentBlocks)
		scriptReply, err = c.LLM.RawComplete(ctx, raw, 4096)
	} else {
		scriptReply, err = c.LLM.SimpleComplete(ctx, scriptSys, scriptUser)
	}
	if err != nil {
		return nil, fmt.Errorf("运镜脚本生成失败: %w", err)
	}

	// Step 2: 逐帧生成 SVG
	frameSys := "你是SVG动画帧设计师。根据运镜脚本为指定帧生成SVG。" +
		"要求: viewBox=\"0 0 800 600\"，只输出SVG代码。"

	svgFrames := make([]string, 0, frames)
	for i := 1; i <= frames; i++ {
		frameUser := fmt.Sprintf("运镜脚本:\n%s\n\n生成第 %d/%d 帧的SVG。与前后帧保持视觉连贯。只输出SVG代码。",
			scriptReply, i, frames)

		frameReply, fErr := c.LLM.SimpleComplete(ctx, frameSys, frameUser)
		if fErr != nil {
			svgFrames = append(svgFrames,
				fmt.Sprintf(`<svg viewBox="0 0 800 600" xmlns="http://www.w3.org/2000/svg"><rect fill="#333" width="800" height="600"/><text x="400" y="300" text-anchor="middle" fill="#fff" font-size="24">Frame %d (error: %s)</text></svg>`, i, fErr.Error()))
			continue
		}
		svg := extractSVG(frameReply)
		if svg == "" {
			svg = fmt.Sprintf(`<svg viewBox="0 0 800 600" xmlns="http://www.w3.org/2000/svg"><rect fill="#555" width="800" height="600"/><text x="400" y="300" text-anchor="middle" fill="#fff" font-size="20">Frame %d</text></svg>`, i)
		}
		svgFrames = append(svgFrames, svg)
	}

	htmlPlayer := buildHTMLPlayer(svgFrames)

	return &VideoResult{
		Frames:     svgFrames,
		Script:     scriptReply,
		HTMLPlayer: htmlPlayer,
		RawReply:   scriptReply,
	}, nil
}

// parseImageInput 从 data URL 或纯 base64 中提取 base64 数据和 media type。
func parseImageInput(input string) (b64 string, mediaType string) {
	if strings.HasPrefix(input, "data:") {
		// data:image/png;base64,xxxxx
		if idx := strings.Index(input, ";base64,"); idx > 0 {
			mediaType = input[5:idx] // "image/png"
			b64 = input[idx+8:]     // raw base64
			return
		}
	}
	return input, "image/png"
}

// extractSVG 从 LLM 输出中提取 <svg>...</svg> 块。
func extractSVG(s string) string {
	lower := strings.ToLower(s)
	startIdx := strings.Index(lower, "<svg")
	if startIdx < 0 {
		return ""
	}
	after := s[startIdx:]
	endIdx := strings.Index(strings.ToLower(after), "</svg>")
	if endIdx < 0 {
		return ""
	}
	return after[:endIdx+len("</svg>")]
}

// buildHTMLPlayer 将多个 SVG 帧拼接为 CSS 关键帧动画 HTML。
func buildHTMLPlayer(svgFrames []string) string {
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
