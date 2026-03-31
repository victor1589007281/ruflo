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

// AnthropicProvider calls the Anthropic Messages API.
type AnthropicProvider struct {
	APIKey     string
	HTTPClient *http.Client
	Model      string
}

// Name returns the provider id.
func (p *AnthropicProvider) Name() string { return "anthropic" }

// Complete performs a non-streaming completion.
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
