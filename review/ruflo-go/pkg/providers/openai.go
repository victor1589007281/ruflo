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

	"github.com/ruflo/ruflo-go/api"
)

const openaiChatAPI = "https://api.openai.com/v1/chat/completions"

// OpenAIProvider calls OpenAI chat completions.
type OpenAIProvider struct {
	APIKey     string
	HTTPClient *http.Client
	Model      string
}

// Name returns the provider id.
func (p *OpenAIProvider) Name() string { return string(api.LLMProviderOpenAI) }

// Complete performs a chat completion.
func (p *OpenAIProvider) Complete(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error) {
	if p.APIKey == "" {
		return nil, errors.New("openai: missing API key")
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
		model = "gpt-4o-mini"
	}

	var msgs []map[string]any
	for _, m := range req.Messages {
		entry := map[string]any{
			"role":    m.Role,
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
	if req.TopP > 0 {
		body["top_p"] = req.TopP
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, openaiChatAPI, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

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
		return nil, fmt.Errorf("openai: rate limited: %s", strings.TrimSpace(string(b)))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("openai: %s: %s", resp.Status, string(b))
	}

	var parsed struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("openai: empty choices")
	}
	ch := parsed.Choices[0]
	toolCalls := make([]api.LLMToolCall, 0, len(ch.Message.ToolCalls))
	for _, tc := range ch.Message.ToolCalls {
		var args map[string]any
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		toolCalls = append(toolCalls, api.LLMToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
			RawJSON:   tc.Function.Arguments,
		})
	}
	return &api.LLMResponse{
		Provider:     api.LLMProviderOpenAI,
		Model:        parsed.Model,
		Text:         ch.Message.Content,
		FinishReason: ch.FinishReason,
		ToolCalls:    toolCalls,
		Usage: api.LLMUsage{
			InputTokens:  parsed.Usage.PromptTokens,
			OutputTokens: parsed.Usage.CompletionTokens,
			TotalTokens:  parsed.Usage.TotalTokens,
		},
		Raw: map[string]any{"body": json.RawMessage(b)},
	}, nil
}

// StreamComplete is not implemented for OpenAI in this build.
func (p *OpenAIProvider) StreamComplete(ctx context.Context, req api.LLMRequest) (io.ReadCloser, error) {
	return nil, errors.New("openai: use Complete; streaming not implemented")
}

// HealthCheck calls models list.
func (p *OpenAIProvider) HealthCheck(ctx context.Context) error {
	if p.APIKey == "" {
		return errors.New("openai: no api key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
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
		return fmt.Errorf("openai: server error %s", resp.Status)
	}
	return nil
}

// EstimateCost returns a rough relative cost score for load balancing.
func (p *OpenAIProvider) EstimateCost(req api.LLMRequest) float64 {
	tok := len(req.Messages) * 256
	return float64(tok) * 1e-6
}
