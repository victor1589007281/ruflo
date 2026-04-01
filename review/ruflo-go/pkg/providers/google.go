// 本文件封装 Google Gemini generateContent：将 chat 角色映射为 user/model，system 合并为 systemInstruction，查询参数带 key。
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
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// GoogleProvider calls the Gemini generateContent API.
type GoogleProvider struct {
	client  *http.Client
	apiKey  string
	baseURL string
	model   string
}

// NewGoogleProvider reads GOOGLE_API_KEY from the environment.
func NewGoogleProvider() *GoogleProvider {
	return &GoogleProvider{
		client:  &http.Client{Timeout: 120 * time.Second},
		apiKey:  os.Getenv("GOOGLE_API_KEY"),
		baseURL: "https://generativelanguage.googleapis.com/v1beta",
		model:   "gemini-2.0-flash",
	}
}

// Name 返回 api.LLMProviderGoogle。
func (g *GoogleProvider) Name() string { return string(api.LLMProviderGoogle) }

// Complete 组装 contents 与可选 generationConfig，POST models/{model}:generateContent。
func (g *GoogleProvider) Complete(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error) {
	if g.apiKey == "" {
		return nil, errors.New("google: GOOGLE_API_KEY not set")
	}

	contents := make([]map[string]any, 0, len(req.Messages))
	sysInstruction := ""
	for _, m := range req.Messages {
		if m.Role == "system" {
			sysInstruction += m.Content
			continue
		}
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": []map[string]any{{"text": m.Content}},
		})
	}

	body := map[string]any{"contents": contents}
	if sysInstruction != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": sysInstruction}},
		}
	}
	if req.Temperature > 0 {
		body["generationConfig"] = map[string]any{"temperature": req.Temperature}
	}
	if req.MaxTokens > 0 {
		gc, ok := body["generationConfig"].(map[string]any)
		if !ok {
			gc = map[string]any{}
		}
		gc["maxOutputTokens"] = req.MaxTokens
		body["generationConfig"] = gc
	}

	model := g.model
	if req.Model != "" {
		model = req.Model
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", g.baseURL, model, g.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("google request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	content := ""
	if len(result.Candidates) > 0 && len(result.Candidates[0].Content.Parts) > 0 {
		content = result.Candidates[0].Content.Parts[0].Text
	}

	return &api.LLMResponse{
		Provider: api.LLMProviderGoogle,
		Model:    model,
		Text:     content,
		Usage: api.LLMUsage{
			InputTokens:  result.UsageMetadata.PromptTokenCount,
			OutputTokens: result.UsageMetadata.CandidatesTokenCount,
			TotalTokens:  result.UsageMetadata.TotalTokenCount,
		},
		Raw: map[string]any{"body": json.RawMessage(respBody)},
	}, nil
}

// StreamComplete 未实现。
func (g *GoogleProvider) StreamComplete(ctx context.Context, req api.LLMRequest) (io.ReadCloser, error) {
	_ = ctx
	_ = req
	return nil, errors.New("google: use Complete; streaming not implemented")
}

// HealthCheck 仅检查 apiKey 非空（不发网络请求）。
func (g *GoogleProvider) HealthCheck(ctx context.Context) error {
	_ = ctx
	if g.apiKey == "" {
		return errors.New("google: GOOGLE_API_KEY not set")
	}
	return nil
}

// EstimateCost 按内容长度/4 估输入 Token，结合 maxOutputTokens 用占位单价估算美元。
func (g *GoogleProvider) EstimateCost(req api.LLMRequest) float64 {
	tokens := 0
	for _, m := range req.Messages {
		tokens += len(m.Content) / 4
	}
	if tokens < 1 {
		tokens = 1
	}
	out := req.MaxTokens
	if out < 0 {
		out = 0
	}
	return float64(tokens)*0.00000035 + float64(out)*0.0000007
}
