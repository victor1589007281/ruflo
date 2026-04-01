// 本文件封装 Anthropic Messages API（POST https://api.anthropic.com/v1/messages）。
//
// 消息格式转换（统一多轮对话 → Anthropic）：
//   - role 为 system 的消息：不进入 messages 数组，多条 system 的文本用换行拼接为顶层字段 system（Anthropic 单独字段）。
//   - 其余 role：原样写入 messages[].role 与 messages[].content（字符串形式）。
//   - max_tokens、temperature、stop_sequences 由 LLMRequest 映射；max_tokens 缺省填 1024。
// 响应侧：解析 content 数组中 type=="text" 的块拼接为单一 Text；usage 映射 input/output 并求和为 TotalTokens。
//
// 为避免与 api 包循环依赖，方法签名使用文件内 apiLLMRequest / apiLLMResponse 等类型别名；与 api 包类型布局一致，可由薄适配层转换。
package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicAPI     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
)

// AnthropicProvider 使用 x-api-key 与 anthropic-version 请求头调用 Anthropic Messages API。
type AnthropicProvider struct {
	APIKey     string       // Anthropic API 密钥
	HTTPClient *http.Client // 可注入 HTTP 客户端；nil 时使用默认超时
	Model      string       // 当请求未指定模型时的默认模型 ID
}

// Name 返回 Provider 标识 "anthropic"。
func (p *AnthropicProvider) Name() string { return "anthropic" }

// Complete 执行非流式补全：见文件头「消息格式转换」说明。
func (p *AnthropicProvider) Complete(ctx context.Context, req apiLLMRequest) (*apiLLMResponse, error) {
	return p.complete(ctx, req, false)
}

func (p *AnthropicProvider) complete(ctx context.Context, req apiLLMRequest, stream bool) (*apiLLMResponse, error) {
	_ = stream
	if p.APIKey == "" {
		return nil, errors.New("anthropic: missing API key")
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	model := req.Model
	if model == "" {
		model = p.Model
	}
	if model == "" {
		model = "claude-3-5-sonnet-20241022"
	}

	var sys string
	var msgs []map[string]any
	for _, m := range req.Messages {
		switch strings.ToLower(m.Role) {
		case "system":
			if sys != "" {
				sys += "\n"
			}
			sys += m.Content
		default:
			msgs = append(msgs, map[string]any{
				"role":    m.Role,
				"content": m.Content,
			})
		}
	}

	body := map[string]any{
		"model":      model,
		"max_tokens": req.MaxTokens,
		"messages":   msgs,
	}
	if sys != "" {
		body["system"] = sys
	}
	if req.MaxTokens <= 0 {
		body["max_tokens"] = 1024
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if len(req.Stop) > 0 {
		body["stop_sequences"] = req.Stop
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPI, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("anthropic: rate limited: %s", strings.TrimSpace(string(b)))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("anthropic: %s: %s", resp.Status, string(b))
	}

	var parsed struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	var text strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	out := &apiLLMResponse{
		Provider:     "anthropic",
		Model:        parsed.Model,
		Text:         text.String(),
		FinishReason: parsed.StopReason,
		Usage: apiLLMUsage{
			InputTokens:  parsed.Usage.InputTokens,
			OutputTokens: parsed.Usage.OutputTokens,
			TotalTokens:  parsed.Usage.InputTokens + parsed.Usage.OutputTokens,
		},
		Raw: map[string]any{"id": parsed.ID},
	}
	return out, nil
}

// StreamComplete is not fully implemented; returns error suggesting non-streaming Complete.
func (p *AnthropicProvider) StreamComplete(ctx context.Context, req apiLLMRequest) (io.ReadCloser, error) {
	return nil, errors.New("anthropic: streaming not implemented; use Complete")
}

// HealthCheck verifies the API key with a minimal request.
func (p *AnthropicProvider) HealthCheck(ctx context.Context) error {
	if p.APIKey == "" {
		return errors.New("anthropic: no api key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("anthropic: server error %s", resp.Status)
	}
	return nil
}

// EstimateCost returns a rough USD estimate (placeholder rates).
func (p *AnthropicProvider) EstimateCost(req apiLLMRequest) float64 {
	in := len(req.Messages)
	_ = in
	return 0.003
}

// Aliases to avoid circular import with api in signatures — use api types in public wrappers if needed.
type apiLLMRequest = struct {
	Provider    string
	Model       string
	Messages    []struct{ Role, Content, Name string }
	MaxTokens   int
	Temperature float64
	TopP        float64
	Stop        []string
}

type apiLLMUsage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

type apiLLMResponse struct {
	Provider     string
	Model        string
	Text         string
	FinishReason string
	Usage        apiLLMUsage
	Raw          map[string]any
}
