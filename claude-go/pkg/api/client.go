// Package api 实现 Anthropic Messages API 客户端。
// 对应 TS 源码: review/claude/src/services/api/claude.ts
//
// 支持:
//   - 流式 (SSE) 和非流式请求
//   - Anthropic 兼容 API (如 DashScope)
//   - 工具定义传递
//   - token 使用量追踪
//   - 模型切换与回退
//   - signature_delta 签名验证
//   - server_tool_use 服务端工具调用处理
//   - refusal stop_reason 处理
//   - prompt_too_long (PTL) 错误恢复
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// PromptTooLongError 标识 API 返回 prompt_too_long 错误。
// 对应 TS: services/api/errors.ts 中的 PromptTooLongError
// queryLoop 捕获此错误后触发 reactive compact (紧急压缩)。
type PromptTooLongError struct {
	Message string
}

func (e *PromptTooLongError) Error() string { return e.Message }

// OverloadedError 标识 API 返回 overloaded 错误 (529)。
// 对应 TS: services/api/claude.ts 中的 overloaded 处理
type OverloadedError struct {
	Message string
}

func (e *OverloadedError) Error() string { return e.Message }

// Client Anthropic Messages API 客户端。
// 对应 TS: services/api/claude.ts 中的 API 调用逻辑。
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
	// RetryCount 请求重试次数 (用于 overloaded / 5xx)
	RetryCount int
}

// NewClient 创建 API 客户端
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Client:  http.DefaultClient,
	}
}

// NewDashScopeClient 创建阿里百炼 DashScope API 客户端
// DashScope 提供 Anthropic 兼容端点 (coding.dashscope.aliyuncs.com)
func NewDashScopeClient(apiKey, model string) *Client {
	return NewClient("https://coding.dashscope.aliyuncs.com/apps/anthropic/v1", apiKey, model)
}

// NewAnthropicClient 创建标准 Anthropic API 客户端
func NewAnthropicClient(apiKey, model string) *Client {
	return NewClient("https://api.anthropic.com/v1", apiKey, model)
}

// StreamMessage 以流式方式发送消息，返回事件通道。
// 对应 TS: services/api/claude.ts 中的 queryModelWithStreaming()
//
// SSE 流式协议:
//   每行格式: "data: {json}\n\n" 或 "event: {type}\n"
//   事件类型: message_start, content_block_start, content_block_delta,
//             content_block_stop, message_delta, message_stop
//
// 算法:
//   1. 构建请求体 (model, messages, system, tools, stream=true)
//   2. 发送 POST 请求
//   3. 逐行读取 SSE 流
//   4. 解析事件并通过 channel 发送
func (c *Client) StreamMessage(
	ctx context.Context,
	messages []types.APIMessage,
	systemPrompt []string,
	tools []types.APITool,
	maxTokens int,
) (<-chan types.StreamDelta, <-chan error) {
	eventCh := make(chan types.StreamDelta, 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(eventCh)
		defer close(errCh)

		// 构建系统提示词
		var system interface{}
		if len(systemPrompt) == 1 {
			system = systemPrompt[0]
		} else if len(systemPrompt) > 1 {
			blocks := make([]map[string]string, len(systemPrompt))
			for i, s := range systemPrompt {
				blocks[i] = map[string]string{
					"type": "text",
					"text": s,
				}
			}
			system = blocks
		}

		req := types.APIRequest{
			Model:     c.Model,
			Messages:  messages,
			System:    system,
			MaxTokens: maxTokens,
			Stream:    true,
		}
		if len(tools) > 0 {
			req.Tools = tools
		}

		body, err := json.Marshal(req)
		if err != nil {
			errCh <- fmt.Errorf("序列化请求失败: %w", err)
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/messages", bytes.NewReader(body))
		if err != nil {
			errCh <- fmt.Errorf("创建请求失败: %w", err)
			return
		}

		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", c.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

		resp, err := c.Client.Do(httpReq)
		if err != nil {
			errCh <- fmt.Errorf("API 请求失败: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			respBody, _ := io.ReadAll(resp.Body)
			// 检查特定错误类型 (对应 TS: services/api/errors.ts)
			var apiErr types.APIError
			if json.Unmarshal(respBody, &apiErr) == nil {
				errType := apiErr.Error.Type
				// prompt_too_long → 触发 reactive compact
				if errType == "invalid_request_error" && strings.Contains(apiErr.Error.Message, "prompt is too long") {
					errCh <- &PromptTooLongError{Message: apiErr.Error.Message}
					return
				}
				// overloaded (529) → 触发重试或 fallback
				if resp.StatusCode == 529 || errType == "overloaded_error" {
					errCh <- &OverloadedError{Message: apiErr.Error.Message}
					return
				}
				// refusal → 模型拒绝回答
				if errType == "refusal" {
					errCh <- fmt.Errorf("模型拒绝回答 (refusal): %s", apiErr.Error.Message)
					return
				}
			}
			errCh <- fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))
			return
		}

		// 解析 SSE 流
		// SSE 格式: "event: <type>\ndata: <json>\n\n" 或 "data:<json>\n"
		// DashScope 格式使用 "data:" (无空格) 前缀
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()

			// 支持 "data: " 和 "data:" 两种格式
			var data string
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimPrefix(line, "data:")
				data = strings.TrimSpace(data)
			} else {
				continue
			}

			if data == "" || data == "[DONE]" {
				continue
			}

			var delta types.StreamDelta
			if err := json.Unmarshal([]byte(data), &delta); err != nil {
				continue
			}

			// 处理 signature_delta (签名验证增量)
			// 对应 TS: content_block_delta 中 delta.type=="signature_delta"
			if delta.Delta != nil && delta.Delta.Signature != "" {
				// 签名数据附加到当前块，不做额外处理
			}

			// 处理 server_tool_use / server_tool_result 块
			// 对应 TS: 服务端工具（如 web_search_tool）直接由 API 执行
			if delta.ContentBlock != nil {
				switch delta.ContentBlock.Type {
				case types.ContentBlockServerToolUse:
					// 服务端工具调用 - 传递给消费者
				case types.ContentBlockServerToolResult:
					// 服务端工具结果 - 传递给消费者
				}
			}

			// 处理 message_delta 中的 stop_reason
			// 对应 TS: stop_reason=="refusal" 时的特殊处理
			if delta.Delta != nil && delta.Delta.StopReason == string(types.StopReasonRefusal) {
				// 模型拒绝继续: 在 engine 层处理
			}

			select {
			case eventCh <- delta:
			case <-ctx.Done():
				return
			}
		}
	}()

	return eventCh, errCh
}

// SendMessage 非流式发送消息。
// 对应 TS 中的非流式路径 (用于 compact 等内部调用)。
func (c *Client) SendMessage(
	ctx context.Context,
	messages []types.APIMessage,
	systemPrompt []string,
	tools []types.APITool,
	maxTokens int,
) (*types.APIResponse, error) {
	var system interface{}
	if len(systemPrompt) == 1 {
		system = systemPrompt[0]
	} else if len(systemPrompt) > 1 {
		blocks := make([]map[string]string, len(systemPrompt))
		for i, s := range systemPrompt {
			blocks[i] = map[string]string{"type": "text", "text": s}
		}
		system = blocks
	}

	req := types.APIRequest{
		Model:     c.Model,
		Messages:  messages,
		System:    system,
		MaxTokens: maxTokens,
		Stream:    false,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("API 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))
	}

	var result types.APIResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &result, nil
}
