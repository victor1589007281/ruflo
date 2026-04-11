// Package vision 提供基于阿里云 DashScope（百炼）的多模态能力封装：
// 图像理解（OpenAI 兼容对话）、万相文生图/图生图（异步任务）、以及预留的图生视频接口。
package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// openAIChatCompletionsURL DashScope OpenAI 兼容多模态对话端点（图像理解）。
	openAIChatCompletionsURL = "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"

	// defaultOpenAICompatBase 为 Client 默认根地址（不含路径），与 openAIChatCompletionsURL 对应。
	defaultOpenAICompatBase = "https://dashscope.aliyuncs.com/compatible-mode/v1"

	// imageSynthesisEndpoint 万相文本/图像生成异步任务创建地址。
	imageSynthesisEndpoint = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text2image/image-synthesis"

	// wanxModelV1 万相文生图默认模型标识。
	wanxModelV1 = "wanx-v1"

	taskPollInterval = 3 * time.Second
	taskPollTimeout  = 5 * time.Minute
)

// ErrVideoNotSupported 表示图生视频能力尚未实现（占位，与英文提示一致便于调用方判断）。
var ErrVideoNotSupported = errors.New("not yet supported")

// ImageResult 异步图像任务完成后的结果摘要。
type ImageResult struct {
	// URL 生成图像的可访问地址（取 results 中首个成功项的 url）。
	URL string `json:"url"`
	// TaskID 异步任务 ID，可用于排查或与控制台对照。
	TaskID string `json:"task_id"`
}

// VideoResult 图生视频任务结果占位结构（当前未实现）。
type VideoResult struct {
	URL    string `json:"url"`
	TaskID string `json:"task_id"`
}

// Client 封装 DashScope 调用所需的密钥、根 URL 与 HTTP 客户端。
// BaseURL 默认为 OpenAI 兼容根路径；图像合成接口使用固定全局 endpoint，不受 BaseURL 影响。
type Client struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient 使用给定 API Key 构造客户端。
// BaseURL 默认为 https://dashscope.aliyuncs.com/compatible-mode/v1；HTTPClient 为空时使用 http.DefaultClient。
func NewClient(apiKey string) *Client {
	return &Client{
		APIKey:     apiKey,
		BaseURL:    defaultOpenAICompatBase,
		HTTPClient: nil,
	}
}

// Understand 调用 DashScope OpenAI 兼容多模态接口，对单张 base64 图像进行理解与问答。
// imageBase64 可为裸 Base64；若不含 data: 前缀，将按 JPEG 包装为 data URL。
// model 由调用方指定（例如 qwen3-vl-plus 等视觉模型），对应百炼控制台中的模型名。
func Understand(ctx context.Context, apiKey, model, imageBase64, prompt string) (string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return "", fmt.Errorf("apiKey 不能为空")
	}
	if strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("model 不能为空")
	}
	trimmedImg := strings.TrimSpace(imageBase64)
	if trimmedImg == "" {
		return "", fmt.Errorf("imageBase64 不能为空")
	}

	var dataURL string
	if strings.HasPrefix(trimmedImg, "data:") {
		dataURL = trimmedImg
	} else {
		dataURL = "data:image/jpeg;base64," + trimmedImg
	}

	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "image_url",
						"image_url": map[string]string{
							"url": dataURL,
						},
					},
					{
						"type": "text",
						"text": prompt,
					},
				},
			},
		},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("序列化请求体失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIChatCompletionsURL, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateForErr(respBody))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("解析响应 JSON 失败: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("API 错误: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("响应中无 choices")
	}

	content := parsed.Choices[0].Message.Content
	switch v := content.(type) {
	case string:
		return v, nil
	case []any:
		var b strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" {
					if tx, ok := m["text"].(string); ok {
						b.WriteString(tx)
					}
				}
			}
		}
		if b.Len() == 0 {
			return "", fmt.Errorf("多段 content 中未找到文本")
		}
		return b.String(), nil
	default:
		return "", fmt.Errorf("无法解析 message.content 类型")
	}
}

// GenerateImage 使用万相 wanx-v1 异步文生图：创建任务后轮询直至成功或失败。
// size 为空时默认 1024*1024；negativePrompt 可为空。
func GenerateImage(ctx context.Context, apiKey, prompt, negativePrompt, size string) (*ImageResult, error) {
	if strings.TrimSpace(size) == "" {
		size = "1024*1024"
	}
	input := map[string]string{
		"prompt": prompt,
	}
	if strings.TrimSpace(negativePrompt) != "" {
		input["negative_prompt"] = negativePrompt
	}
	params := map[string]any{
		"style": "<auto>",
		"size":  size,
		"n":     1,
	}
	return runImageSynthesis(ctx, apiKey, input, params)
}

// TransformImage 使用万相 API 的参考图能力（请求体字段 ref_image；强度对应 parameters.ref_strength）。
// strength 为十进制字符串（如 "0.7"），将解析为 float；无效时返回错误。
func TransformImage(ctx context.Context, apiKey, prompt, refImageURL, strength string) (*ImageResult, error) {
	if strings.TrimSpace(refImageURL) == "" {
		return nil, fmt.Errorf("refImageURL 不能为空")
	}
	strength = strings.TrimSpace(strength)
	if strength == "" {
		return nil, fmt.Errorf("strength 不能为空")
	}
	refStrength, err := strconv.ParseFloat(strength, 64)
	if err != nil {
		return nil, fmt.Errorf("解析 strength 失败: %w", err)
	}

	input := map[string]string{
		"prompt":    prompt,
		"ref_image": refImageURL,
	}
	params := map[string]any{
		"style":        "<auto>",
		"size":         "1024*1024",
		"n":            1,
		"ref_strength": refStrength,
		"ref_mode":     "repaint",
	}
	return runImageSynthesis(ctx, apiKey, input, params)
}

func runImageSynthesis(ctx context.Context, apiKey string, input map[string]string, parameters map[string]any) (*ImageResult, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("apiKey 不能为空")
	}
	payload := map[string]any{
		"model":      wanxModelV1,
		"input":      input,
		"parameters": parameters,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, imageSynthesisEndpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-DashScope-Async", "enable")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("创建图像任务失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取创建任务响应失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("创建任务 HTTP %d: %s", resp.StatusCode, truncateForErr(respBody))
	}

	taskID, err := parseCreateTaskID(respBody)
	if err != nil {
		return nil, err
	}

	imageURL, err := pollTaskResult(ctx, apiKey, taskID)
	if err != nil {
		return nil, err
	}
	return &ImageResult{URL: imageURL, TaskID: taskID}, nil
}

func parseCreateTaskID(respBody []byte) (string, error) {
	var wrap struct {
		Output struct {
			TaskID string `json:"task_id"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &wrap); err != nil {
		return "", fmt.Errorf("解析创建任务响应失败: %w", err)
	}
	if strings.TrimSpace(wrap.Output.TaskID) == "" {
		if wrap.Message != "" {
			return "", fmt.Errorf("创建任务失败: %s (%s)", wrap.Message, wrap.Code)
		}
		return "", fmt.Errorf("创建任务响应中缺少 task_id")
	}
	return wrap.Output.TaskID, nil
}

// pollTaskResult 轮询 GET /api/v1/tasks/{taskID}，每 3 秒一次，最长 5 分钟；成功时返回首张成功图片的 URL。
func pollTaskResult(ctx context.Context, apiKey, taskID string) (string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return "", fmt.Errorf("apiKey 不能为空")
	}
	if strings.TrimSpace(taskID) == "" {
		return "", fmt.Errorf("taskID 不能为空")
	}

	deadline := time.Now().Add(taskPollTimeout)
	var lastStatus string

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("轮询任务超时（%v），最后状态: %s, task_id=%s", taskPollTimeout, lastStatus, taskID)
		}

		url, status, failErr := fetchTaskStatusOnce(ctx, apiKey, taskID)
		lastStatus = status

		switch strings.ToUpper(strings.TrimSpace(status)) {
		case "SUCCEEDED":
			if url != "" {
				return url, nil
			}
			return "", fmt.Errorf("任务已成功但未解析到图像 URL")
		case "FAILED":
			if failErr != nil {
				return "", failErr
			}
			return "", fmt.Errorf("任务失败: task_id=%s", taskID)
		case "CANCELED":
			return "", fmt.Errorf("任务已取消: task_id=%s", taskID)
		case "UNKNOWN":
			return "", fmt.Errorf("任务不存在或状态未知: task_id=%s", taskID)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(taskPollInterval):
		}
	}
}

func fetchTaskStatusOnce(ctx context.Context, apiKey, taskID string) (imageURL, status string, err error) {
	endpoint := fmt.Sprintf("https://dashscope.aliyuncs.com/api/v1/tasks/%s", taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", fmt.Errorf("构造查询任务请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("查询任务失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("读取任务查询响应失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("查询任务 HTTP %d: %s", resp.StatusCode, truncateForErr(body))
	}

	var tr taskQueryEnvelope
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", "", fmt.Errorf("解析任务查询 JSON 失败: %w", err)
	}

	st := strings.TrimSpace(tr.Output.TaskStatus)
	for _, r := range tr.Output.Results {
		if strings.TrimSpace(r.URL) != "" {
			return r.URL, st, nil
		}
	}

	if strings.EqualFold(st, "FAILED") {
		msg := strings.TrimSpace(tr.Output.Message)
		if msg == "" {
			msg = strings.TrimSpace(tr.Message)
		}
		code := strings.TrimSpace(tr.Output.Code)
		if code != "" && msg != "" {
			return "", st, fmt.Errorf("%s: %s", code, msg)
		}
		if msg != "" {
			return "", st, errors.New(msg)
		}
		return "", st, fmt.Errorf("任务失败且无详细说明")
	}

	return "", st, nil
}

type taskQueryEnvelope struct {
	Output struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
		Code       string `json:"code"`
		Message    string `json:"message"`
		Results    []struct {
			URL     string `json:"url"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"results"`
	} `json:"output"`
	Message string `json:"message"`
}

// GenerateVideo 图生视频占位实现，当前固定返回 ErrVideoNotSupported。
func GenerateVideo(ctx context.Context, apiKey, imageURL, prompt string) (*VideoResult, error) {
	_ = ctx
	_ = apiKey
	_ = imageURL
	_ = prompt
	return nil, fmt.Errorf("%w", ErrVideoNotSupported)
}

func truncateForErr(b []byte) string {
	const max = 512
	s := string(b)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
