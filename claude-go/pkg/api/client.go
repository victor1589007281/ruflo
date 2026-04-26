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

	// 追踪信息 (由 Client.Tag 或调用方 Context 注入, 支持按业务维度聚合)
	Source  string // "chat" | "feishu" | "team" | "swarm" | "dashboard" | "vision" | "compact" | ...
	Purpose string // 额外标签, 如团队名 / stage 名 / agent 角色
	Request string // 粗粒度 HTTP 方法标签 (messages / stream-messages)

	// 限流 / 熔断 观测 (用于 dashboard 速率与守护页面)
	GuardWaitSec   float64 // 本次 RateLimitGuard.Acquire 等待时长 (包含 RPM 令牌 + 退避)
	CircuitOpened  bool    // 本次调用触发了熔断 (从 closed 变 open)
	CircuitBlocked bool    // 本次调用被熔断器拒绝 (未发起真实请求)
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

	// FallbackModels 备用模型列表: 主模型不可用时按序尝试。
	// 触发条件: 模型不支持 (400 invalid model) / 超载 (529) / 配额耗尽 (402/403)。
	FallbackModels []string

	// PromptCacheEnabled 启用 Anthropic prompt caching (顶层 cache_control)。
	// "auto" = 自动检测 (默认，Anthropic 官方 API 启用，其他关闭)
	// "on"   = 强制启用
	// "off"  = 强制关闭
	PromptCacheMode string
	promptCacheDisabledByError bool // 因 API 错误自适应关闭

	// 追踪标签 (可选): 调用方通过 WithTag 或直接赋值, 标识该 Client 实例所服务的业务场景。
	// 会写入 LLMCallRecord.Source, 方便在 dashboard 里按业务维度聚合 (chat/feishu/team/...)。
	Tag string

	// 智能模型切换: 连续 429 达到阈值时自动切换备用模型, 冷却后回退主模型
	fbMu                sync.Mutex
	consecutive429      int       // 连续 429 次数
	fallbackActiveModel string    // 当前激活的备用模型 (空=使用主模型)
	fallbackActiveSince time.Time // 切换到备用模型的时刻
	fallbackCooldownMin int       // 冷却时间(分钟), 默认 5, 到期后尝试恢复主模型

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

// WithModel 返回一个共享同一 HTTP 客户端和限流器的轻量 Client 副本，仅覆盖 Model。
// 用于为不同 plan/role 指定不同模型而不影响原始 Client 的 Model 字段。
func (c *Client) WithModel(model string) *Client {
	return &Client{
		BaseURL:              c.BaseURL,
		APIKey:               c.APIKey,
		Model:                model,
		Client:               c.Client,
		Guard:                c.Guard,
		RetryCount:           c.RetryCount,
		RetryBase:            c.RetryBase,
		RetryMax:             c.RetryMax,
		OnLLMEvent:           c.OnLLMEvent,
		OnLLMMetrics:         c.OnLLMMetrics,
		FallbackModels:       c.FallbackModels,
		PromptCacheMode:      c.PromptCacheMode,
		cbThreshold:          c.cbThreshold,
		fallbackCooldownMin:  c.fallbackCooldownMin,
		Tag:                  c.Tag + ":" + model,
	}
}

// ConfiguredClone 返回一个共享 HTTP 客户端和限流器的轻量 Client 副本，
// 使用指定参数覆盖 baseURL / apiKey / model / fallbackModels。
// 用于为不同 plan 使用完全不同的 API 端点而不影响原始 Client。
func (c *Client) ConfiguredClone(baseURL, apiKey, model string, fallbackModels []string) *Client {
	clone := &Client{
		BaseURL:              strings.TrimRight(baseURL, "/"),
		APIKey:               apiKey,
		Model:                model,
		Client:               c.Client,
		Guard:                c.Guard,
		RetryCount:           c.RetryCount,
		RetryBase:            c.RetryBase,
		RetryMax:             c.RetryMax,
		OnLLMEvent:           c.OnLLMEvent,
		OnLLMMetrics:         c.OnLLMMetrics,
		PromptCacheMode:      c.PromptCacheMode,
		cbThreshold:          c.cbThreshold,
		fallbackCooldownMin:  c.fallbackCooldownMin,
		Tag:                  c.Tag,
	}
	if len(fallbackModels) > 0 {
		clone.FallbackModels = fallbackModels
	} else {
		clone.FallbackModels = c.FallbackModels
	}
	return clone
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

// SetTag 为该 Client 实例设置业务追踪标签 (chat / feishu / team / dashboard / ...),
// 该标签会写入 LLMCallRecord.Source, 方便 dashboard 按业务维度聚合 LLM 调用指标。
// 直接修改原 Client, 不做拷贝 (Client 内含 sync.Mutex / atomic, 浅拷贝会被 go vet 拒绝)。
func (c *Client) SetTag(tag string) *Client {
	if c == nil {
		return nil
	}
	c.Tag = tag
	return c
}

// CircuitSnapshot 熔断器的结构化状态, 供 dashboard / diagnose 端点使用。
type CircuitSnapshot struct {
	Open             bool      `json:"open"`             // 当前是否熔断
	OpenUntil        time.Time `json:"openUntil"`        // 本次熔断的解除时刻
	OpenSecondsLeft  float64   `json:"openSecondsLeft"`  // 距离解除的秒数 (0=正常)
	ConsecutiveFails int       `json:"consecutiveFails"` // 当前连续失败计数
	Threshold        int       `json:"threshold"`        // 触发阈值
	TotalRetries     int64     `json:"totalRetries"`     // 累计重试次数
	TotalFails       int64     `json:"totalFails"`       // 累计失败次数
	CircuitTrips     int64     `json:"circuitTrips"`     // 累计触发熔断次数
	Timestamp        time.Time `json:"timestamp"`
}

// GetCircuitSnapshot 返回该 Client 的熔断器运行态快照。
// 与 Snapshot() 配合, 供 dashboard LLM guard 页面展示。
func (c *Client) GetCircuitSnapshot() CircuitSnapshot {
	if c == nil {
		return CircuitSnapshot{Timestamp: time.Now()}
	}
	c.cbMu.Lock()
	open := c.circuitOpen
	openUntil := c.circuitOpenUntil
	fails := c.consecutiveFails
	threshold := c.cbThreshold
	c.cbMu.Unlock()
	left := time.Until(openUntil)
	if left < 0 || !open {
		left = 0
	}
	if threshold <= 0 {
		threshold = 5
	}
	return CircuitSnapshot{
		Open:             open,
		OpenUntil:        openUntil,
		OpenSecondsLeft:  left.Seconds(),
		ConsecutiveFails: fails,
		Threshold:        threshold,
		TotalRetries:     c.TotalRetries.Load(),
		TotalFails:       c.TotalFails.Load(),
		CircuitTrips:     c.CircuitTrips.Load(),
		Timestamp:        time.Now(),
	}
}

// isRetryable 判断 HTTP 状态码是否可重试
func isRetryableStatus(code int) bool {
	return code == 429 || code == 503 || code == 529 || code >= 500
}

// shouldEnablePromptCache 判断当前请求是否应启用 prompt caching。
func (c *Client) shouldEnablePromptCache() bool {
	if c.promptCacheDisabledByError {
		return false
	}
	switch strings.ToLower(c.PromptCacheMode) {
	case "on":
		return true
	case "off":
		return false
	default: // "auto" 或空
		return isAnthropicEndpoint(c.BaseURL)
	}
}

// isAnthropicEndpoint 检测是否为 Anthropic 官方 API 或兼容协议 (支持 prompt caching)。
func isAnthropicEndpoint(baseURL string) bool {
	lower := strings.ToLower(baseURL)
	return strings.Contains(lower, "anthropic.com") ||
		strings.Contains(lower, "api.claude") ||
		strings.Contains(lower, "bedrock") ||               // AWS Bedrock
		strings.Contains(lower, "dashscope.aliyuncs.com") || // 阿里云百炼
		strings.Contains(lower, "/apps/anthropic")           // Anthropic 兼容代理路径
}

// isCacheRelatedError 检测 API 错误是否与 prompt caching 相关。
func isCacheRelatedError(statusCode int, body string) bool {
	if statusCode != 400 && statusCode != 422 {
		return false
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "cache_control") ||
		strings.Contains(lower, "cache") && strings.Contains(lower, "breakpoint") ||
		strings.Contains(lower, "cache") && strings.Contains(lower, "not supported") ||
		strings.Contains(lower, "unknown") && strings.Contains(lower, "cache")
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
	if rec.Source == "" {
		rec.Source = c.Tag
	}
	// 保底: 如果调用侧完全没给出 Source, 给一个 "unknown" 而不是空串, 便于 dashboard 统计。
	if rec.Source == "" {
		rec.Source = "unknown"
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

// recordFailure 记录一次失败到熔断器状态。
// 返回 (触发本次熔断, 当前连续失败数) — 调用方可用于给 LLMCallRecord 打标签。
func (c *Client) recordFailure(errMsg string) (opened bool, fails int) {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	c.consecutiveFails++
	fails = c.consecutiveFails
	threshold := c.cbThreshold
	if threshold <= 0 {
		threshold = 5
	}
	if c.consecutiveFails >= threshold && !c.circuitOpen {
		c.circuitOpen = true
		c.circuitOpenUntil = time.Now().Add(30 * time.Second)
		c.CircuitTrips.Add(1)
		opened = true
		c.fireEvent("circuit_open", fmt.Sprintf("连续 %d 次失败, 熔断 30s: %s", c.consecutiveFails, errMsg))
	}
	return opened, fails
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
		streamRec := LLMCallRecord{
			Stream:  true,
			Status:  "success",
			Model:   c.Model,
			BaseURL: c.BaseURL,
			Source:  c.Tag,
			Request: "stream_messages",
		}
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
			streamRec.CircuitBlocked = true
			errCh <- fmt.Errorf("LLM 熔断器开启: 连续多次失败, 30s 后重试")
			return
		}

		// 全局准入控制
		var guardRelease func()
		if c.Guard != nil {
			waitStart := time.Now()
			guardRelease = c.Guard.Acquire()
			streamRec.GuardWaitSec = time.Since(waitStart).Seconds()
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
			Model:     c.effectiveModel(),
			Messages:  messages,
			System:    system,
			MaxTokens: maxTokens,
			Stream:    true,
		}
		if len(tools) > 0 {
			req.Tools = tools
		}
		if c.shouldEnablePromptCache() {
			req.CacheControl = &types.CacheControl{Type: "ephemeral"}
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
	streamRetryLoop:
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
				opened, _ := c.recordFailure(err.Error())
				streamRec.CircuitOpened = opened
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
				// 智能模型切换: 连续 429 达阈值时切换备用模型
				if newModel := c.on429OrFallback(); newModel != "" {
					req.Model = newModel
					body, _ = json.Marshal(req)
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

			// Stream 全部重试失败 — 尝试 FallbackModels
			if len(c.FallbackModels) > 0 && isFallbackEligible(resp.StatusCode, fmt.Errorf("%s", string(respBody))) {
				for _, fbModel := range c.FallbackModels {
					if fbModel == "" || fbModel == req.Model {
						continue
					}
					log.Printf("[api] Stream 主模型失败, 尝试备用模型: %s", fbModel)
					c.fireEvent("retry", fmt.Sprintf("Stream 切换备用模型 %s", fbModel))
					req.Model = fbModel
					body, _ = json.Marshal(req)
					// 用备用模型再走一轮完整重试
					goto streamRetryLoop
				}
			}

			opened, _ := c.recordFailure(string(respBody))
			streamRec.CircuitOpened = opened
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
		c.emitLLMMetric(LLMCallRecord{
			Status:         "error",
			Request:        "messages",
			DurationSec:    0,
			HTTPStatus:     0,
			ErrorKind:      "client",
			ErrorMessage:   "circuit open",
			CircuitBlocked: true,
		})
		return nil, fmt.Errorf("LLM 熔断器开启: 连续多次失败, 30s 后重试")
	}

	// 全局准入控制 (RPM 令牌桶 + 并发信号量)
	var guardRelease func()
	var guardWaitSec float64
	if c.Guard != nil {
		waitStart := time.Now()
		guardRelease = c.Guard.Acquire()
		guardWaitSec = time.Since(waitStart).Seconds()
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
		Model:     c.effectiveModel(),
		Messages:  messages,
		System:    system,
		MaxTokens: maxTokens,
		Stream:    false,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	if c.shouldEnablePromptCache() {
		req.CacheControl = &types.CacheControl{Type: "ephemeral"}
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
			opened, _ := c.recordFailure(lastErr.Error())
			c.TotalFails.Add(1)
			c.emitLLMMetric(LLMCallRecord{
				Status:        "error",
				Request:       "messages",
				DurationSec:   time.Since(startTS).Seconds(),
				HTTPStatus:    0,
				Retries:       retries,
				ErrorKind:     classifyErrorKind(0, err.Error()),
				ErrorMessage:  truncateErr(err.Error(), 256),
				GuardWaitSec:  guardWaitSec,
				CircuitOpened: opened,
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
			c.onSuccess429Reset()
			rec := LLMCallRecord{
				Status:       "success",
				Request:      "messages",
				DurationSec:  time.Since(startTS).Seconds(),
				HTTPStatus:   200,
				Retries:      retries,
				StopReason:   result.StopReason,
				GuardWaitSec: guardWaitSec,
			}
			if result.Model != "" {
				rec.Model = result.Model
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
			// 自适应关闭 prompt cache: 如果 400 错误与 cache_control 相关，禁用后重试一次
			if req.CacheControl != nil && isCacheRelatedError(resp.StatusCode, string(respBody)) {
				log.Printf("[api] prompt cache 不被支持, 自适应关闭: %s", string(respBody)[:min(len(respBody), 200)])
				c.promptCacheDisabledByError = true
				req.CacheControl = nil
				body, _ = json.Marshal(req)
				continue // 重试一次
			}
			errMsg := fmt.Sprintf("API 返回 %d: %s", resp.StatusCode, string(respBody))
			c.emitLLMMetric(LLMCallRecord{
				Status:       "error",
				Request:      "messages",
				DurationSec:  time.Since(startTS).Seconds(),
				HTTPStatus:   resp.StatusCode,
				Retries:      retries,
				ErrorKind:    classifyErrorKind(resp.StatusCode, string(respBody)),
				ErrorMessage: truncateErr(errMsg, 256),
				GuardWaitSec: guardWaitSec,
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
			// 智能模型切换: 连续 429 达阈值时切换备用模型, 用新模型重试
			if newModel := c.on429OrFallback(); newModel != "" {
				req.Model = newModel
				body, _ = json.Marshal(req)
				continue
			}
		} else if resp.StatusCode == 503 || resp.StatusCode == 529 {
			if c.Guard != nil {
				c.Guard.On429(retryAfterSec)
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

	// 主模型全部重试失败 — 尝试 FallbackModels
	if len(c.FallbackModels) > 0 && isFallbackEligible(lastStatus, lastErr) {
		for fi, fbModel := range c.FallbackModels {
			if fbModel == c.Model || fbModel == "" {
				continue
			}
			log.Printf("[api] 主模型 %s 不可用, 尝试备用模型 %d/%d: %s",
				c.Model, fi+1, len(c.FallbackModels), fbModel)
			c.fireEvent("retry", fmt.Sprintf("切换备用模型 %s", fbModel))

			req.Model = fbModel
			fbBody, err := json.Marshal(req)
			if err != nil {
				continue
			}
			httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/messages", bytes.NewReader(fbBody))
			if err != nil {
				continue
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("x-api-key", c.APIKey)
			httpReq.Header.Set("anthropic-version", "2023-06-01")
			httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
			resp, err := c.Client.Do(httpReq)
			if err != nil {
				continue
			}
			respBody, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				continue
			}
			if resp.StatusCode == 200 {
				c.recordSuccess()
				var result types.APIResponse
				if err := json.Unmarshal(respBody, &result); err != nil {
					continue
				}
				rec := LLMCallRecord{
					Status:       "retry_success",
					Request:      "messages",
					DurationSec:  time.Since(startTS).Seconds(),
					HTTPStatus:   200,
					Retries:      retries + fi + 1,
					StopReason:   result.StopReason,
					GuardWaitSec: guardWaitSec,
					Model:        fbModel,
				}
				if result.Usage != nil {
					rec.InputTokens = result.Usage.InputTokens
					rec.OutputTokens = result.Usage.OutputTokens
					rec.CacheReadTokens = result.Usage.CacheReadInputTokens
					rec.CacheCreationTokens = result.Usage.CacheCreationInputTokens
					rec.TotalTokens = rec.InputTokens + rec.OutputTokens + rec.CacheReadTokens + rec.CacheCreationTokens
				}
				c.emitLLMMetric(rec)
				log.Printf("[api] 备用模型 %s 成功", fbModel)
				return &result, nil
			}
		}
	}

	opened, _ := c.recordFailure(lastErr.Error())
	c.TotalFails.Add(1)
	c.fireEvent("fatal", fmt.Sprintf("LLM 调用 %d 次全部失败: %v", maxRetry+1, lastErr))
	c.emitLLMMetric(LLMCallRecord{
		Status:        "error",
		Request:       "messages",
		DurationSec:   time.Since(startTS).Seconds(),
		HTTPStatus:    lastStatus,
		Retries:       retries,
		ErrorKind:     classifyErrorKind(lastStatus, lastErr.Error()),
		ErrorMessage:  truncateErr(lastErr.Error(), 256),
		GuardWaitSec:  guardWaitSec,
		CircuitOpened: opened,
	})
	return nil, fmt.Errorf("LLM 调用 %d 次全部失败: %w", maxRetry+1, lastErr)
}

// isFallbackEligible 判断失败是否适合切换备用模型。
// 适用: 模型不支持(400)、配额耗尽(402/403)、超载(529)、连续限流(429 多次)。
func isFallbackEligible(httpStatus int, err error) bool {
	if err == nil {
		return false
	}
	switch httpStatus {
	case 400, 402, 403, 529:
		return true
	}
	errStr := strings.ToLower(err.Error())
	for _, kw := range []string{"not supported", "not found", "invalid model",
		"quota", "insufficient", "overloaded", "unavailable"} {
		if strings.Contains(errStr, kw) {
			return true
		}
	}
	return false
}

// effectiveModel 返回当前应使用的模型名 (考虑智能切换状态)。
// 如果备用模型冷却期已过, 自动恢复主模型。
func (c *Client) effectiveModel() string {
	if len(c.FallbackModels) == 0 {
		return c.Model
	}
	c.fbMu.Lock()
	defer c.fbMu.Unlock()

	if c.fallbackActiveModel == "" {
		return c.Model
	}

	cooldown := c.fallbackCooldownMin
	if cooldown <= 0 {
		cooldown = 5
	}
	if time.Since(c.fallbackActiveSince) > time.Duration(cooldown)*time.Minute {
		log.Printf("[api] 冷却期(%d分钟)已过, 恢复主模型 %s (从备用 %s)",
			cooldown, c.Model, c.fallbackActiveModel)
		c.fallbackActiveModel = ""
		c.consecutive429 = 0
		c.fireEvent("model_restore", fmt.Sprintf("恢复主模型 %s", c.Model))
		return c.Model
	}

	return c.fallbackActiveModel
}

// on429OrFallback 处理 429 限流, 达到阈值时切换备用模型。
// 返回切换后的模型名 (空字符串=不切换, 继续等待)。
func (c *Client) on429OrFallback() string {
	if len(c.FallbackModels) == 0 {
		return ""
	}
	c.fbMu.Lock()
	defer c.fbMu.Unlock()

	c.consecutive429++
	const threshold = 3 // 连续 3 次 429 触发切换

	if c.consecutive429 < threshold {
		return ""
	}

	// 选择第一个可用的备用模型
	for _, fb := range c.FallbackModels {
		if fb != "" && fb != c.Model && fb != c.fallbackActiveModel {
			log.Printf("[api] 连续 %d 次 429 限流, 切换到备用模型: %s → %s",
				c.consecutive429, c.Model, fb)
			c.fallbackActiveModel = fb
			c.fallbackActiveSince = time.Now()
			c.consecutive429 = 0
			c.fireEvent("model_switch", fmt.Sprintf("429 限流切换: %s → %s", c.Model, fb))
			return fb
		}
	}
	return ""
}

// onSuccess429Reset 成功调用后重置 429 计数器。
func (c *Client) onSuccess429Reset() {
	c.fbMu.Lock()
	c.consecutive429 = 0
	c.fbMu.Unlock()
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
