package api

// 网关层重试策略 (可配置)。
//
// 背景: anthropic-gateway 的两条**原样透传**路径 (proxyPassthrough /
// proxyOpenAIFormat) 此前完全没有重试 —— 上游回一个 4xx/5xx 就直接抄给客户端。
// 而 400 在真实链路里有一大类是瞬时/可恢复的 (上游路由抖动、会话头缺失后被对端
// 短暂拒、限流网关的排队拒绝), 直接失败会把一次本可成功的调用变成用户可见的报错。
//
// 本文件提供一份**与实例无关**的策略对象: api.Client 用它做状态码判断与退避计算,
// 两个网关 (cmd/anthropic-gateway、pkg/llmgw) 用 DoWithRetry 做"写响应前"的重试。
//
// 为什么策略不塞进 api.Client: 网关的透传路径根本不构造 api.Client (它只用一个
// http.Client 原样转发)，把策略单独抽出来才能让两条路径共用同一套默认值与退避算法，
// 否则两份拷贝迟早漂移成"网关重试两次、翻译路径重试四次"这种没人能解释的差异。

import (
	"context"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"time"
)

// DefaultGatewayRetryStatuses 返回网关透传路径的默认可重试状态码集合。
//
// 包含 400 是刻意的 (用户明确要求): 与 api.Client 的历史默认 (不含 400) 分开，
// 因为翻译路径上有 cache_control 自适应、fallback 模型等更细的语义，贸然改变
// 它的默认会让老配置的请求行为变样。透传路径没有这些顾虑。
//
// 408/409/425 是"请求还没被处理完"的瞬时状态，429/5xx/529 是限流与过载。
func DefaultGatewayRetryStatuses() []int {
	return []int{400, 408, 409, 425, 429, 500, 502, 503, 504, 529}
}

// RetryPolicy 描述一次上游调用的重试与退避行为。
//
// 零值 = 不重试 (MaxRetries=0)。需要网关默认语义时用 DefaultGatewayRetryPolicy。
type RetryPolicy struct {
	// MaxRetries 是**额外**重试次数: 总尝试次数 = MaxRetries + 1。
	MaxRetries int
	// Base 是指数退避基数; <=0 时取 1s。
	Base time.Duration
	// Max 是退避上限 (jitter 之前); <=0 时取 8s。
	Max time.Duration
	// Statuses 是可重试状态码集合; 空/nil 时按 DefaultGatewayRetryStatuses 处理。
	Statuses map[int]struct{}
}

// DefaultGatewayRetryPolicy 返回网关透传路径的默认策略:
// 额外重试 2 次 (最多 3 次尝试), 基数 1s, 上限 8s, 状态码含 400。
//
// 2 次而不是更多: 透传路径大多是交互式调用 (Claude Code CLI), 退避 1s+2s 已经
// 接近用户能接受的上限; 再多的重试应该交给上层 (fallback 模型 / 用户重发)。
func DefaultGatewayRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries: 2,
		Base:       time.Second,
		Max:        8 * time.Second,
		Statuses:   statusSet(DefaultGatewayRetryStatuses()),
	}
}

// RetryPolicyFromConfig 按 modelconfig 的配置语义构造策略。
//
//	retryCount   0 → 网关默认 (2 次); -1 → 显式禁用 (0 次); >0 → 该次数
//	retryBaseSec 0 → 默认 1s
//	retryMaxSec  0 → 默认 8s
//	retryStatuses 空/nil → 默认集合 (显式空数组也按默认处理, 更安全)
func RetryPolicyFromConfig(retryCount, baseSec, maxSec int, statuses []int) RetryPolicy {
	p := DefaultGatewayRetryPolicy()
	switch {
	case retryCount < 0:
		p.MaxRetries = 0
	case retryCount > 0:
		p.MaxRetries = retryCount
	}
	if baseSec > 0 {
		p.Base = time.Duration(baseSec) * time.Second
	}
	if maxSec > 0 {
		p.Max = time.Duration(maxSec) * time.Second
	}
	if len(statuses) > 0 {
		p.Statuses = statusSet(statuses)
	}
	return p
}

// statusSet 把切片转成集合; 忽略 0 与负数 (它们不是合法 HTTP 状态码)。
func statusSet(codes []int) map[int]struct{} {
	m := make(map[int]struct{}, len(codes))
	for _, c := range codes {
		if c > 0 {
			m[c] = struct{}{}
		}
	}
	return m
}

// Retryable 判断某个状态码是否应按本策略重试。
func (p RetryPolicy) Retryable(status int) bool {
	set := p.Statuses
	if len(set) == 0 {
		set = statusSet(DefaultGatewayRetryStatuses())
	}
	_, ok := set[status]
	return ok
}

// Backoff 返回**不含 jitter** 的退避时长, 已按上限封顶。
//
// attempt 从 0 开始; status 为 0 表示"还没拿到响应"(网络错误)。
// 429/503 是"服务器请你慢点", 基数拉长 3 倍 —— 与普通 5xx 区分开 (与
// Client.retryDelay 同一口径)。
func (p RetryPolicy) Backoff(attempt, status int) time.Duration {
	base := p.Base
	if base <= 0 {
		base = time.Second
	}
	if status == 429 || status == 503 {
		base *= 3
	}
	maxD := p.Max
	if maxD <= 0 {
		maxD = 8 * time.Second
	}
	delay := time.Duration(float64(base) * math.Pow(2, float64(attempt)))
	if delay > maxD {
		delay = maxD
	}
	return delay
}

// Delay 返回本次重试实际等待的时长: Backoff 加 ±25% jitter。
//
// retryAfter > 计算值时优先尊重服务端的 Retry-After (不再加 jitter —— 那是服务端
// 给出的硬性要求, 抖动到它之前等于没遵守)。
func (p RetryPolicy) Delay(attempt, status int, retryAfter time.Duration) time.Duration {
	d := p.Backoff(attempt, status)
	if retryAfter > d {
		return retryAfter
	}
	return time.Duration(float64(d) * (0.75 + rand.Float64()*0.5))
}

// DoWithRetry 对一次上游 HTTP 调用应用重试策略, 返回**最终**响应与总尝试次数。
//
// build 每次调用都必须返回一个**全新**的 *http.Request (含全新的 body reader) ——
// http.Request 的 Body 是一次性的, 复用同一个 request 重试会发出空 body。
//
// 中间响应 (还会重试的那几次) 的 body 会被 drain 并关闭, 避免连接泄漏;
// 只有最终响应交给调用方。调用方必须在**写任何响应头/字节之前**调用它 ——
// 这正是不支持"已开始回传 2xx 流后再重试"的原因: 那时字节已经出去了。
//
// ctx 取消时立即返回 (nil, attempts, ctx.Err()), 调用方不应再写第二次响应。
func DoWithRetry(
	ctx context.Context,
	client *http.Client,
	policy RetryPolicy,
	build func() (*http.Request, error),
) (*http.Response, int, error) {
	attempts := 0
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, attempts, err
		}
		req, err := build()
		if err != nil {
			return nil, attempts, err
		}
		attempts++
		resp, err := client.Do(req)
		if err != nil {
			if attempt >= policy.MaxRetries {
				return nil, attempts, err
			}
			delay := policy.Delay(attempt, 0, 0)
			log.Printf("[api] 上游网络错误 (尝试 %d/%d): %v, %.1fs 后重试",
				attempt+1, policy.MaxRetries+1, err, delay.Seconds())
			if !sleepContext(ctx, delay) {
				return nil, attempts, ctx.Err()
			}
			continue
		}

		// 不可重试 (或重试次数已用尽): 原样交给调用方, 由它决定怎么写回。
		if !policy.Retryable(resp.StatusCode) || attempt >= policy.MaxRetries {
			if attempts > 1 || resp.StatusCode >= 400 {
				log.Printf("[api] 上游最终 status=%d attempts=%d", resp.StatusCode, attempts)
			}
			return resp, attempts, nil
		}

		// Retry-After: 标准头优先; 没有头时也接受 body 里的 retry_after 提示。
		var retryAfter time.Duration
		if sec := ParseRetryAfter(resp, nil); sec > 0 {
			retryAfter = time.Duration(sec * float64(time.Second))
		}
		delay := policy.Delay(attempt, resp.StatusCode, retryAfter)
		log.Printf("[api] 上游 status=%d (尝试 %d/%d), %.1fs 后重试",
			resp.StatusCode, attempt+1, policy.MaxRetries+1, delay.Seconds())
		drainAndClose(resp.Body)
		if !sleepContext(ctx, delay) {
			return nil, attempts, ctx.Err()
		}
	}
}

// drainAndClose 尽力读完一小段 body 再关闭。限长读取: 上游可能回一个巨大的错误页,
// 为了复用连接而全量读完会把一次重试变成一次下载。
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 256<<10))
	_ = body.Close()
}

// sleepContext 等待 d, 返回 false 表示等待期间 ctx 被取消。
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
