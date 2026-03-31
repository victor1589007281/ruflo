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

// RuVectorProvider calls a local RuVector/OpenAI-compatible completions endpoint.
type RuVectorProvider struct {
	BaseURL    string
	HTTPClient *http.Client
	Model      string
}

// NewRuVectorProviderFromEnv uses RUVECTOR_BASE_URL (default http://localhost:8080).
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

// Name returns the provider id.
func (p *RuVectorProvider) Name() string { return string(api.LLMProviderRuvector) }

// Complete POSTs /v1/completions with a prompt built from messages.
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

// StreamComplete is not implemented.
func (p *RuVectorProvider) StreamComplete(ctx context.Context, req api.LLMRequest) (io.ReadCloser, error) {
	return nil, errors.New("ruvector: streaming not implemented")
}

// HealthCheck probes the base URL.
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

// EstimateCost returns a low synthetic score for local routing.
func (p *RuVectorProvider) EstimateCost(req api.LLMRequest) float64 {
	return float64(len(req.Messages)) * 1e-6
}
