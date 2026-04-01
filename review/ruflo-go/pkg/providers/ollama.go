// 本文件封装本地 Ollama /api/chat：适合离线推理，默认基址 localhost:11434，stream=false 取单条回复与 eval 计数作 Token 代理。
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

// OllamaProvider 与单机 Ollama 守护进程通信。
type OllamaProvider struct {
	client  *http.Client // HTTP 客户端，较长超时以适配本地推理
	baseURL string        // 去掉末尾 / 的基址
	model   string        // 默认模型，如 llama3
}

// NewOllamaProvider 从 OLLAMA_BASE_URL 读取基址，缺省为 http://localhost:11434。
func NewOllamaProvider() *OllamaProvider {
	base := os.Getenv("OLLAMA_BASE_URL")
	if base == "" {
		base = "http://localhost:11434"
	}
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	return &OllamaProvider{
		client:  &http.Client{Timeout: 300 * time.Second},
		baseURL: base,
		model:   "llama3",
	}
}

// Name 返回 api.LLMProviderOllama。
func (o *OllamaProvider) Name() string { return string(api.LLMProviderOllama) }

// Complete POST JSON，解析 message.content 与 prompt_eval_count/eval_count 填充 Usage。
func (o *OllamaProvider) Complete(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error) {
	msgs := make([]map[string]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}

	model := o.model
	if req.Model != "" {
		model = req.Model
	}

	body := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   false,
	}
	if req.Temperature > 0 {
		body["options"] = map[string]any{"temperature": req.Temperature}
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := o.baseURL + "/api/chat"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		EvalCount       int `json:"eval_count"`
		PromptEvalCount int `json:"prompt_eval_count"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	return &api.LLMResponse{
		Provider: api.LLMProviderOllama,
		Model:    model,
		Text:     result.Message.Content,
		Usage: api.LLMUsage{
			InputTokens:  result.PromptEvalCount,
			OutputTokens: result.EvalCount,
			TotalTokens:  result.PromptEvalCount + result.EvalCount,
		},
		Raw: map[string]any{"body": json.RawMessage(respBody)},
	}, nil
}

// StreamComplete 未实现。
func (o *OllamaProvider) StreamComplete(ctx context.Context, req api.LLMRequest) (io.ReadCloser, error) {
	_ = ctx
	_ = req
	return nil, errors.New("ollama: use Complete; streaming not implemented")
}

// HealthCheck GET /api/tags，非 200 返回错误。
func (o *OllamaProvider) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama: %s: %s", resp.Status, string(body))
	}
	return nil
}

// EstimateCost 本地推理视为零美元，便于成本策略优先选本地。
func (o *OllamaProvider) EstimateCost(req api.LLMRequest) float64 {
	_ = req
	return 0
}
