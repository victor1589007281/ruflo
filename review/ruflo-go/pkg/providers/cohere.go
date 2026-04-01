// 本文件封装 Cohere Chat API v2：角色映射、解析 message.content（字符串或块数组），兼容多种响应形状。
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

const cohereChatV2 = "https://api.cohere.ai/v2/chat"

// CohereProvider 使用 Bearer 调用 https://api.cohere.ai/v2/chat。
type CohereProvider struct {
	APIKey     string       // API 密钥，可运行时从环境再读
	HTTPClient *http.Client // 可注入客户端
	Model      string       // 默认 command-r 系列等
}

// NewCohereProviderFromEnv 从 COHERE_API_KEY 初始化。
func NewCohereProviderFromEnv() *CohereProvider {
	return &CohereProvider{
		APIKey: os.Getenv("COHERE_API_KEY"),
		Model:  "command-r7b-12-2024",
	}
}

// Name 返回 api.LLMProviderCohere。
func (p *CohereProvider) Name() string { return string(api.LLMProviderCohere) }

// mapCohereRole 将通用 role 映射为 Cohere 接受的 system/user/assistant/tool。
func mapCohereRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system":
		return "system"
	case "assistant":
		return "assistant"
	case "tool":
		return "tool"
	default:
		return "user"
	}
}

// Complete 发送 messages 数组，extractCohereMessageText 聚合文本与 finish_reason。
func (p *CohereProvider) Complete(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error) {
	key := p.APIKey
	if key == "" {
		key = os.Getenv("COHERE_API_KEY")
	}
	if key == "" {
		return nil, errors.New("cohere: missing COHERE_API_KEY")
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
		model = "command-r7b-12-2024"
	}

	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		entry := map[string]any{
			"role":    mapCohereRole(m.Role),
			"content": m.Content,
		}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		msgs = append(msgs, entry)
	}

	body := map[string]any{
		"model":    model,
		"messages": msgs,
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cohereChatV2, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+key)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cohere: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	text, finish := extractCohereMessageText(b)
	return &api.LLMResponse{
		Provider:     api.LLMProviderCohere,
		Model:        model,
		Text:         text,
		FinishReason: finish,
		Usage:        api.LLMUsage{},
		Raw:          map[string]any{"body": json.RawMessage(b)},
	}, nil
}

// extractCohereMessageText 先尝试 message.content 为字符串，再尝试 [{type,text}] 块数组，最后退回顶层 text 字段。
func extractCohereMessageText(b []byte) (text string, finish string) {
	var outer struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
		Text         string `json:"text"`
	}
	if err := json.Unmarshal(b, &outer); err != nil {
		return "", ""
	}
	finish = outer.FinishReason
	// content may be string or array of {type,text}
	var asStr string
	if err := json.Unmarshal(outer.Message.Content, &asStr); err == nil && asStr != "" {
		return asStr, finish
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(outer.Message.Content, &blocks); err == nil {
		for _, bl := range blocks {
			if bl.Text != "" {
				text += bl.Text
			}
		}
		if text != "" {
			return text, finish
		}
	}
	if outer.Text != "" {
		return outer.Text, finish
	}
	return text, finish
}

// StreamComplete 未实现。
func (p *CohereProvider) StreamComplete(ctx context.Context, req api.LLMRequest) (io.ReadCloser, error) {
	return nil, errors.New("cohere: streaming not implemented")
}

// HealthCheck GET /v1/models，401/403 视为鉴权失败。
func (p *CohereProvider) HealthCheck(ctx context.Context) error {
	key := p.APIKey
	if key == "" {
		key = os.Getenv("COHERE_API_KEY")
	}
	if key == "" {
		return errors.New("cohere: no api key")
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.cohere.ai/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("cohere: auth failed: %s", resp.Status)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("cohere: server error %s", resp.Status)
	}
	return nil
}

// EstimateCost 用消息条数粗估 Token 再乘系数，供策略比较。
func (p *CohereProvider) EstimateCost(req api.LLMRequest) float64 {
	tok := len(req.Messages) * 200
	return float64(tok) * 8e-7
}
