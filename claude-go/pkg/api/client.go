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
	"log"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// LLMEventFunc 回调: LLM 调用发生重试/熔断等事件时通知外部 (如飞书)。
// eventType: "retry", "circuit_open", "circuit_close", "fatal"
type LLMEventFunc func(eventType, detail string)

// LLMCallRecord 单次 LLM 调用的可观测数据, 用于采集 token / 延迟 / 成败。
type LLMCallRecord struct {
	Model          string
	BaseURL        string
	Status         string // "success" | "error" | "retry_success"
	Stream         bool
	DurationSec    float64
	InputTokens    int
	OutputTokens   int
	CacheReadTokens     int
	CacheCreationTokens int
	TotalTokens    int
	HTTPStatus     int    // 最终的 HTTP 状态码 (可能是 200)
	Retries        int    // 本次调用内部触发的重试次数
	ErrorKind      string // "timeout"|"rate_limit"|"overloaded"|"prompt_too_long"|"refusal"|"client"|"server"|""
	ErrorMessage   string // 截断后的错误信息
	StopReason     string // "end_turn"|"max_tokens"|"tool_use"|"refusal"|...
	Timestamp      time.Time
}

// LLMMetricsHook 采集 LLM 调用指标的回调 (dashboard 在启动时注入, 避免循环依赖)。
type LLMMetricsHook func(rec LLMCallRecord)

// Client Anthropic Messages API 客户端。
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client

	RetryCount int // 最大重试次数 (429/5xx), 默认 4
	RetryBase  time.Duration // 退避基数, 默认 3s
	RetryMax   time.Duration // 退避上限, 默认 60s

	OnLLMEvent   LLMEventFunc    // 事件回调 (可选, 注入飞书通知)
	OnLLMMetrics LLMMetricsHook  // 指标回调 (可选, 注入 dashboard metrics collector)
	Guard        *RateLimitGuard // 全局准入控制器 (可选, 强烈建议设置)

	// 熔断器
	cbMu             sync.Mutex
	consecutiveFails int
	circuitOpen      bool
	circuitOpenUntil time.Time
	cbThreshold      int // 默认 5

	// 统计
	TotalRetries atomic.Int64
	TotalFails   atomic.Int64
	CircuitTrips atomic.Int64
}

// NewClient 创建 API 客户端 (内置 429/5xx 重试 + 熔断)
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Client: &http.Client{
			Timeout: 5 * time.Minute,
		},
		RetryCount:  4,
		RetryBase:   3 * time.Second,
		RetryMax:    60 * time.Second,
		cbThreshold: 5,
	}
}

// isRetryable 判断 HTTP 状态码是否可重试
func isRetryableStatus(code int) bool {
	return code == 429 || code == 503 || code == 529 || code >= 500
}

// retryDelay 计算退避时间 (指数退避 + jitter, 429 用 3x 基数)
func (c *Client) retryDelay(attempt int, statusCode int) time.Duration {
	base := c.RetryBase
	if base <= 0 {
		base = 3 * time.Second
	}
	if statusCode == 429 || statusCode == 503 {
		base = base * 3
	}
	delay := time.Duration(float64(base) * math.Pow(2, float64(attempt)))
	maxD := c.RetryMax
	if maxD <= 0 {
		maxD = 60 * time.Second
	}
	if delay > maxD {
		delay = maxD
	}
	jitter := time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
	return jitter
}

func (c *Client) fireEvent(eventType, detail string) {
	if c.OnLLMEvent != nil {
		c.OnLLMEvent(eventType, detail)
	}
}

// ── 全局 LLM 指标采集钩子 (跨 Client 共用) ────────────────────────────────────
// dashboard 启动时注册一次, 所有 api.Client (无论从哪里创建) 都会把调用统计
// 汇总到同一处, 便于统一展示 "LLM token 使用 / 质量 / 错误" 指标。
var (
	globalLLMHookMu sync.RWMutex
	globalLLMHook   LLMMetricsHook
)

// SetGlobalLLMMetricsHook 注册/覆盖全局 LLM 指标钩子。传入 nil 可禁用。
func SetGlobalLLMMetricsHook(h LLMMetricsHook) {
	globalLLMHookMu.Lock()
	globalLLMHook = h
	globalLLMHookMu.Unlock()
}

// emitLLMMetric 同时向 client 自己的 hook 和全局 hook 发送指标。
func (c *Client) emitLLMMetric(rec LLMCallRecord) {
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}
	if rec.Model == "" {
		rec.Model = c.Model
	}
	if rec.BaseURL == "" {
		rec.BaseURL = c.BaseURL
	}
	if c.OnLLMMetrics != nil {
		func() {
			defer func() { _ = recover() }()
			c.OnLLMMetrics(rec)
		}()
	}
	globalLLMHookMu.RLock()
	gh := globalLLMHook
	globalLLMHookMu.RUnlock()
	if gh != nil {
		func() {
			defer func() { _ = recover() }()
			gh(rec)
		}()
	}
}

// classifyErrorKind 粗分类错误原因 (给指标上标签)。
func classifyErrorKind(status int, errStr string) string {
	low := strings.ToLower(errStr)
	switch {
	case status == 429:
		return "rate_limit"
	case status == 503 || status == 529 || strings.Contains(low, "overloaded"):
		return "overloaded"
	case strings.Contains(low, "prompt is too long") || strings.Contains(low, "prompt_too_long"):
		return "prompt_too_long"
	case strings.Contains(low, "refusal"):
		return "refusal"
	case strings.Contains(low, "context deadline") || strings.Contains(low, "timeout"):
		return "timeout"
	case status >= 400 && status < 500:
		return "client"
	case status >= 500:
		return "server"
	}
	return ""
}

func truncateErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (c *Client) isCircuitOpen() bool {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	if !c.circuitOpen {
		return false
	}
	if time.Now().After(c.circuitOpenUntil) {
		c.circuitOpen = false
		c.consecutiveFails = 0
		c.fireEvent("circuit_close", "熔断器半开, 允许试探")
		return false
	}
	return true
}

func (c *Client) recordSuccess() {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	c.consecutiveFails = 0
	c.circuitOpen = false
}

func (c *Client) recordFailure(errMsg string) {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	c.consecutiveFails++
	threshold := c.cbThreshold
	if threshold <= 0 {
		threshold = 5
	}
	if c.consecutiveFails >= threshold && !c.circuitOpen {
		c.circuitOpen = true
		c.circuitOpenUntil = time.Now().Add(30 * time.Second)
		c.CircuitTrips.Add(1)
		c.fireEvent("circuit_open", fmt.Sprintf("连续 %d 次失败, 熔断 30s: %s", c.consecutiveFails, errMsg))
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
		startTS := time.Now()
		streamRec := LLMCallRecord{Stream: true, Status: "success"}
		streamErrMsg := ""
		streamRetries := 0
		defer func() {
			streamRec.DurationSec = time.Since(startTS).Seconds()
			streamRec.TotalTokens = streamRec.InputTokens + streamRec.OutputTokens + streamRec.CacheReadTokens + streamRec.CacheCreationTokens
			streamRec.Retries = streamRetries
			if streamErrMsg != "" {
				streamRec.Status = "error"
				streamRec.ErrorMessage = truncateErr(streamErrMsg, 256)
				if streamRec.ErrorKind == "" {
					streamRec.ErrorKind = classifyErrorKind(streamRec.HTTPStatus, streamErrMsg)
				}
			} else if streamRetries > 0 {
				streamRec.Status = "retry_success"
			}
			c.emitLLMMetric(streamRec)
		}()
		defer close(eventCh)
		defer close(errCh)

		if c.isCircuitOpen() {
			c.fireEvent("circuit_open", "熔断器开启, StreamMessage 被拒绝")
			streamErrMsg = "circuit open"
			streamRec.ErrorKind = "client"
			errCh <- fmt.Errorf("LLM 熔断器开启: 连续多次失败, 30s 后重试")
			return
		}

		// 全局准入控制
		var guardRelease func()
		if c.Guard != nil {
			guardRelease = c.Guard.Acquire()
			defer guardRelease()
		}

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

		maxRetry := c.RetryCount
		if maxRetry <= 0 {
			maxRetry = 4
		}

		var resp *http.Response
		for attempt := 0; attempt <= maxRetry; attempt++ {
			if ctx.Err() != nil {
				errCh <- ctx.Err()
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

			resp, err = c.Client.Do(httpReq)
			if err != nil {
				if attempt < maxRetry {
					delay := c.retryDelay(attempt, 0)
					c.TotalRetries.Add(1)
					streamRetries++
					c.fireEvent("retry", fmt.Sprintf("Stream 网络错误(尝试 %d/%d), %.0fs 后重试", attempt+1, maxRetry+1, delay.Seconds()))
					select {
					case <-time.After(delay):
						continue
					case <-ctx.Done():
						streamErrMsg = ctx.Err().Error()
						errCh <- ctx.Err()
						return
					}
				}
				c.recordFailure(err.Error())
				streamErrMsg = err.Error()
				errCh <- fmt.Errorf("API 请求失败: %w", err)
				return
			}
			streamRec.HTTPStatus = resp.StatusCode

			if resp.StatusCode == 200 {
				c.recordSuccess()
				if c.Guard != nil {
					c.Guard.OnSuccess()
				}
				break
			}

			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// 不可重试: prompt_too_long / refusal / 4xx (不计入熔断器)
			var apiErr types.APIError
			if json.Unmarshal(respBody, &apiErr) == nil {
				errType := apiErr.Error.Type
				if errType == "invalid_request_error" && strings.Contains(apiErr.Error.Message, "prompt is too long") {
					streamErrMsg = apiErr.Error.Message
					streamRec.ErrorKind = "prompt_too_long"
					errCh <- &PromptTooLongError{Message: apiErr.Error.Message}
					return
				}
				if errType == "refusal" {
					streamErrMsg = apiErr.Error.Message
					streamRec.ErrorKind = "refusal"
					errCh <- fmt.Errorf("模型拒绝回答 (refusal): %s", apiErr.Error.Message)
					return
				}
			}

			if !isRetryableStatus(resp.StatusCode) {
				streamErrMsg = fmt.Sprintf("API %d: %s", resp.StatusCode, string(respBody))
				errCh <- fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))
				return
			}

			// 429 分类 + Guard 通知
			retryAfterSec := ParseRetryAfter(resp, respBody)
			if resp.StatusCode == 429 {
				kind := Classify429(resp.StatusCode, string(respBody))
				if !kind.ShouldRetry() {
					errCh <- fmt.Errorf("API 429 (%s): %s", kind, string(respBody))
					return
				}
				if c.Guard != nil {
					c.Guard.On429(retryAfterSec)
				}
			} else if resp.StatusCode == 503 || resp.StatusCode == 529 {
				if c.Guard != nil {
					c.Guard.On429(retryAfterSec)
				}
			}

			// 可重试: 429 / 503 / 529 / 5xx
			if attempt < maxRetry {
				delay := c.retryDelay(attempt, resp.StatusCode)
				if retryAfterSec > 0 {
					raDelay := time.Duration(retryAfterSec*1000)*time.Millisecond + time.Duration(rand.Float64()*2000)*time.Millisecond
					if raDelay > delay {
						delay = raDelay
					}
				}
				c.TotalRetries.Add(1)
				streamRetries++
				statusHint := "服务端错误"
				if resp.StatusCode == 429 {
					statusHint = fmt.Sprintf("限流(%s)", Classify429(resp.StatusCode, string(respBody)))
				} else if resp.StatusCode == 503 || resp.StatusCode == 529 {
					statusHint = "过载"
				}
				c.fireEvent("retry", fmt.Sprintf("Stream %s(尝试 %d/%d), %.0fs 后重试", statusHint, attempt+1, maxRetry+1, delay.Seconds()))
				select {
				case <-time.After(delay):
					continue
				case <-ctx.Done():
					streamErrMsg = ctx.Err().Error()
					errCh <- ctx.Err()
					return
				}
			}

			c.recordFailure(string(respBody))
			c.TotalFails.Add(1)
			c.fireEvent("fatal", fmt.Sprintf("Stream %d 次全部失败", maxRetry+1))
			streamErrMsg = fmt.Sprintf("API %d: %s", resp.StatusCode, string(respBody))
			errCh <- fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))
			return
		}

		if resp == nil || resp.StatusCode != 200 {
			errCh <- fmt.Errorf("未获得有效响应")
			return
		}
		defer resp.Body.Close()

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

			// 采集 token 使用量 (message_start / message_delta)
			if delta.Message != nil && delta.Message.Usage != nil {
				u := delta.Message.Usage
				if u.InputTokens > 0 {
					streamRec.InputTokens = u.InputTokens
				}
				if u.OutputTokens > 0 {
					streamRec.OutputTokens = u.OutputTokens
				}
				if u.CacheReadInputTokens > 0 {
					streamRec.CacheReadTokens = u.CacheReadInputTokens
				}
				if u.CacheCreationInputTokens > 0 {
					streamRec.CacheCreationTokens = u.CacheCreationInputTokens
				}
			}
			if delta.Usage != nil {
				if delta.Usage.OutputTokens > 0 {
					streamRec.OutputTokens = delta.Usage.OutputTokens
				}
				if delta.Usage.InputTokens > 0 {
					streamRec.InputTokens = delta.Usage.InputTokens
				}
			}
			if delta.Delta != nil && delta.Delta.StopReason != "" {
				streamRec.StopReason = delta.Delta.StopReason
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

// SendMessage 非流式发送消息 (内置全局准入 + 429/5xx 自动重试 + 熔断)。
func (c *Client) SendMessage(
	ctx context.Context,
	messages []types.APIMessage,
	systemPrompt []string,
	tools []types.APITool,
	maxTokens int,
) (*types.APIResponse, error) {
	if c.isCircuitOpen() {
		c.fireEvent("circuit_open", "熔断器开启, SendMessage 被拒绝")
		return nil, fmt.Errorf("LLM 熔断器开启: 连续多次失败, 30s 后重试")
	}

	// 全局准入控制 (RPM 令牌桶 + 并发信号量)
	var guardRelease func()
	if c.Guard != nil {
		guardRelease = c.Guard.Acquire()
		defer guardRelease()
	}

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

	maxRetry := c.RetryCount
	if maxRetry <= 0 {
		maxRetry = 4
	}

	var lastErr error
	var lastStatus int
	startTS := time.Now()
	retries := 0
	for attempt := 0; attempt <= maxRetry; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
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
			lastErr = fmt.Errorf("API 请求失败: %w", err)
			if attempt < maxRetry {
				delay := c.retryDelay(attempt, 0)
				c.TotalRetries.Add(1)
				retries++
				log.Printf("[api] SendMessage 网络错误(尝试 %d/%d): %v, %.1fs 后重试", attempt+1, maxRetry+1, err, delay.Seconds())
				c.fireEvent("retry", fmt.Sprintf("网络错误(尝试 %d/%d): %v, %.0fs 后重试", attempt+1, maxRetry+1, err, delay.Seconds()))
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				continue
			}
			c.recordFailure(lastErr.Error())
			c.TotalFails.Add(1)
			c.emitLLMMetric(LLMCallRecord{
				Status:       "error",
				DurationSec:  time.Since(startTS).Seconds(),
				HTTPStatus:   0,
				Retries:      retries,
				ErrorKind:    classifyErrorKind(0, err.Error()),
				ErrorMessage: truncateErr(err.Error(), 256),
			})
			return nil, lastErr
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("读取响应失败: %w", err)
		}
		lastStatus = resp.StatusCode

		if resp.StatusCode == 200 {
			c.recordSuccess()
			if c.Guard != nil {
				c.Guard.OnSuccess()
			}
			var result types.APIResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				return nil, fmt.Errorf("解析响应失败: %w", err)
			}
			rec := LLMCallRecord{
				Status:      "success",
				DurationSec: time.Since(startTS).Seconds(),
				HTTPStatus:  200,
				Retries:     retries,
				StopReason:  result.StopReason,
			}
			if retries > 0 {
				rec.Status = "retry_success"
			}
			if result.Usage != nil {
				rec.InputTokens = result.Usage.InputTokens
				rec.OutputTokens = result.Usage.OutputTokens
				rec.CacheReadTokens = result.Usage.CacheReadInputTokens
				rec.CacheCreationTokens = result.Usage.CacheCreationInputTokens
				rec.TotalTokens = rec.InputTokens + rec.OutputTokens + rec.CacheReadTokens + rec.CacheCreationTokens
			}
			c.emitLLMMetric(rec)
			return &result, nil
		}

		// 不可重试的客户端错误 (400/401/403) — 不计入熔断器
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && !isRetryableStatus(resp.StatusCode) {
			errMsg := fmt.Sprintf("API 返回 %d: %s", resp.StatusCode, string(respBody))
			c.emitLLMMetric(LLMCallRecord{
				Status:       "error",
				DurationSec:  time.Since(startTS).Seconds(),
				HTTPStatus:   resp.StatusCode,
				Retries:      retries,
				ErrorKind:    classifyErrorKind(resp.StatusCode, string(respBody)),
				ErrorMessage: truncateErr(errMsg, 256),
			})
			return nil, fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))
		}

		// 可重试: 429 / 5xx
		lastErr = fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(respBody))

		// 解析 Retry-After + 分类 429
		retryAfterSec := ParseRetryAfter(resp, respBody)
		if resp.StatusCode == 429 {
			kind := Classify429(resp.StatusCode, string(respBody))
			if !kind.ShouldRetry() {
				return nil, fmt.Errorf("API 429 (%s): %s", kind, string(respBody))
			}
			if c.Guard != nil {
				c.Guard.On429(retryAfterSec)
			}
		} else if resp.StatusCode == 503 || resp.StatusCode == 529 {
			if c.Guard != nil {
				c.Guard.On429(retryAfterSec) // 过载也触发 AIMD 降速
			}
		}

		if attempt < maxRetry {
			delay := c.retryDelay(attempt, resp.StatusCode)
			// 优先使用 Retry-After
			if retryAfterSec > 0 {
				raDelay := time.Duration(retryAfterSec*1000) * time.Millisecond
				jitter := time.Duration(rand.Float64()*2000) * time.Millisecond
				raDelay += jitter
				if raDelay > delay {
					delay = raDelay
				}
			}
			c.TotalRetries.Add(1)
			retries++
			statusHint := "服务端错误"
			if resp.StatusCode == 429 {
				statusHint = fmt.Sprintf("限流(%s)", Classify429(resp.StatusCode, string(respBody)))
			} else if resp.StatusCode == 503 || resp.StatusCode == 529 {
				statusHint = "过载"
			}
			guardStats := ""
			if c.Guard != nil {
				guardStats = " [" + c.Guard.Stats() + "]"
			}
			log.Printf("[api] SendMessage %s(尝试 %d/%d): %.0fs 后重试%s", statusHint, attempt+1, maxRetry+1, delay.Seconds(), guardStats)
			c.fireEvent("retry", fmt.Sprintf("LLM %s(尝试 %d/%d), %.0fs 后重试%s", statusHint, attempt+1, maxRetry+1, delay.Seconds(), guardStats))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
	}

	c.recordFailure(lastErr.Error())
	c.TotalFails.Add(1)
	c.fireEvent("fatal", fmt.Sprintf("LLM 调用 %d 次全部失败: %v", maxRetry+1, lastErr))
	c.emitLLMMetric(LLMCallRecord{
		Status:       "error",
		DurationSec:  time.Since(startTS).Seconds(),
		HTTPStatus:   lastStatus,
		Retries:      retries,
		ErrorKind:    classifyErrorKind(lastStatus, lastErr.Error()),
		ErrorMessage: truncateErr(lastErr.Error(), 256),
	})
	return nil, fmt.Errorf("LLM 调用 %d 次全部失败: %w", maxRetry+1, lastErr)
}

// SimpleComplete 简单文本补全: 发送 system+user prompt, 返回回复文本。
// 实现 dreaming.LLMClient 接口。
func (c *Client) SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	messages := []types.APIMessage{{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"text","text":` + string(mustMarshalString(userPrompt)) + `}]`),
	}}
	resp, err := c.SendMessage(ctx, messages, []string{systemPrompt}, nil, 4096)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == types.ContentBlockText {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

func mustMarshalString(s string) json.RawMessage {
	data, _ := json.Marshal(s)
	return data
}

// RawComplete 发送原始 content blocks 并返回文本结果。
// contentJSON 为 Anthropic Messages API content 数组的 JSON (如 [{"type":"text","text":"..."}])。
// 支持多模态消息(图文混排)。
func (c *Client) RawComplete(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	messages := []types.APIMessage{{
		Role:    "user",
		Content: contentJSON,
	}}
	resp, err := c.SendMessage(ctx, messages, nil, nil, maxTokens)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == types.ContentBlockText {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}
