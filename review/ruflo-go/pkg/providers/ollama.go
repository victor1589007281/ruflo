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

// OllamaProvider calls the local Ollama /api/chat endpoint.
type OllamaProvider struct {
	client  *http.Client
	baseURL string
	model   string
}

// NewOllamaProvider uses OLLAMA_BASE_URL or http://localhost:11434.
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

// Name returns the provider id.
func (o *OllamaProvider) Name() string { return string(api.LLMProviderOllama) }

// Complete performs a non-streaming chat completion.
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

// StreamComplete is not implemented.
func (o *OllamaProvider) StreamComplete(ctx context.Context, req api.LLMRequest) (io.ReadCloser, error) {
	_ = ctx
	_ = req
	return nil, errors.New("ollama: use Complete; streaming not implemented")
}

// HealthCheck calls GET /api/tags.
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

// EstimateCost returns zero for local inference.
func (o *OllamaProvider) EstimateCost(req api.LLMRequest) float64 {
	_ = req
	return 0
}
