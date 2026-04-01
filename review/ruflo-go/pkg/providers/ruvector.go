// 本文件封装 RuVector 或兼容 OpenAI「传统 Completions」形态的本地/自研推理端点（POST {BaseURL}/v1/completions）。
//
// 消息格式转换（统一多轮对话 → 单 prompt 补全）：
//   - Chat 风格的 Messages 列表无法直接传入 completions API，故将每条消息格式化为「ROLE: 内容」行（role 转大写），
//     多行拼接为单一 prompt 字符串；空 prompt 报错。
//   - 请求体使用 model、prompt、max_tokens（默认 256）、可选 temperature。
// 响应侧：parseCompletionsResponse 兼容 OpenAI 风格 choices[0].text 或 choices[0].message.content，并读取 finish_reason。
// 与 Chat Completions 相比：不进行逐条 message 结构映射，适合 RuVector 侧只暴露 completions 路由的部署。
package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// RuVectorProvider 调用本地或自托管的 RuVector（OpenAI completions 兼容）端点。
type RuVectorProvider struct {
	BaseURL    string       // 服务根 URL，默认来自环境 RUVECTOR_BASE_URL 或 localhost:8080
	HTTPClient *http.Client // 可注入；nil 为默认超时
	Model      string       // 默认模型名，请求未指定时使用
}

// NewRuVectorProviderFromEnv 从 RUVECTOR_BASE_URL 构造 Provider；空则默认 http://localhost:8080。
func NewRuVectorProviderFromEnv() *RuVectorProvider {
	base := os.Getenv("RUVECTOR_BASE_URL")
	if strings.TrimSpace(base) == "" {
		base = "http://localhost:8080"
	}
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	return &RuVectorProvider{
		BaseURL: base,
		Model:   "ruvector-default",
	}
}

// Name 返回 api.LLMProviderRuvector 标识。
func (p *RuVectorProvider) Name() string { return string(api.LLMProviderRuvector) }

// Complete 向 /v1/completions 发送由 Messages 拼装的 prompt；转换规则见文件头。
func (p *RuVectorProvider) Complete(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error) {
	base := p.BaseURL
	if base == "" {
		base = strings.TrimSuffix(strings.TrimSpace(os.Getenv("RUVECTOR_BASE_URL")), "/")
	}
	if base == "" {
		base = "http://localhost:8080"
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	model := req.Model
	if model == "" {
		model = p.Model
	}

	var b strings.Builder
	for _, m := range req.Messages {
		role := strings.ToUpper(m.Role)
		if role != "" {
			b.WriteString(role)
			b.WriteString(": ")
		}
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	prompt := strings.TrimSpace(b.String())
	if prompt == "" {
		return nil, errors.New("ruvector: empty prompt")
	}

	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 256
	}
	body := map[string]any{
		"model":      model,
		"prompt":     prompt,
		"max_tokens": maxTok,
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := base + "/v1/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ruvector: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}

	text, finish := parseCompletionsResponse(respBody)
	return &api.LLMResponse{
		Provider:     api.LLMProviderRuvector,
		Model:        model,
		Text:         text,
		FinishReason: finish,
		Usage:        api.LLMUsage{},
		Raw:          map[string]any{"body": json.RawMessage(respBody)},
	}, nil
}

// parseCompletionsResponse 解析类 OpenAI completions JSON：优先 choices[0].text，否则 choices[0].message.content。
func parseCompletionsResponse(b []byte) (text, finish string) {
	var openaiStyle struct {
		Choices []struct {
			Text         string `json:"text"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &openaiStyle); err != nil {
		return "", ""
	}
	if len(openaiStyle.Choices) == 0 {
		return "", ""
	}
	ch := openaiStyle.Choices[0]
	if ch.Text != "" {
		return ch.Text, ch.FinishReason
	}
	return ch.Message.Content, ch.FinishReason
}

// StreamComplete 未实现。
func (p *RuVectorProvider) StreamComplete(ctx context.Context, req api.LLMRequest) (io.ReadCloser, error) {
	return nil, errors.New("ruvector: streaming not implemented")
}

// HealthCheck 对 BaseURL+"/" 发 GET，能连通即视为健康（轻量探活）。
func (p *RuVectorProvider) HealthCheck(ctx context.Context) error {
	base := p.BaseURL
	if base == "" {
		base = strings.TrimSuffix(strings.TrimSpace(os.Getenv("RUVECTOR_BASE_URL")), "/")
	}
	if base == "" {
		base = "http://localhost:8080"
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// EstimateCost 返回与消息条数成正比的极小分值，便于在混合路由中倾向本地/低成本后端。
func (p *RuVectorProvider) EstimateCost(req api.LLMRequest) float64 {
	return float64(len(req.Messages)) * 1e-6
}
