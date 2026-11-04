package api

// 回退观测（Moqi R-H17）。
//
// 背景：模型回退此前只写日志（log.Printf），没有任何可编程出口。日志能被人看到，
// 却进不了 trace / 账本 —— 于是"这一轮的主模型 5xx 了、最后是谁答的、为什么换"
// 这个问题在结构化数据里永远缺一半。本文件给回退加一个回调面，让 harness 能把
// 回退链落进 TurnTrace.FallbackChain 与成本账本。
//
// 两条纪律：
//
//  1. **观测面不能影响主路径**。回调是调用方代码：它 panic、阻塞、或者做了蠢事，
//     都不能让一次本来成功的请求失败。所以同步调用但用 recover 包住。
//  2. **nil 回调零开销**。未接观测时不构造 FallbackInfo、不做状态码分类，热路径
//     只多一次 nil 判断。
//
// 为什么回调在**切换发生前**同步调用：它要带 From=当前模型、To=即将尝试的模型。
// 切换之后再回调，"当前模型"已经被覆盖，From 只能靠调用方自己记 —— 而回退可能
// 连续发生多次（A→B→C），每处调用点都得维护一份影子状态，必然漏。

import (
	"fmt"
	"log"
)

// FallbackInfo 描述一次模型回退。
//
// 刻意只用值类型与字符串：它会被原样写进日志、trace 与账本，不应携带请求体、
// 错误对象或任何可能含用户数据的东西。Reason 是可读原因（脱敏由调用方负责），
// 用字符串让"为什么换"能被聚合计数，而不是埋在一条完整错误信息里。
type FallbackInfo struct {
	FromModel  string
	ToModel    string
	Reason     string // 触发原因（如 "upstream_5xx"/"quota"/可读状态文本）
	StatusCode int    // 触发回退的上游状态码；0 = 非 HTTP 原因
}

// WithFallbackObserver 返回带回调的浅副本；fn == nil 返回原 client。
//
// 浅副本而不是就地修改：Client 在 engine 里被大量共享（HTTP 客户端、限流器、
// 熔断状态），观测是调用方/装配层的关注点，不应改变一个可能被多个 goroutine
// 共用的实例的回调面。单个请求级观察者请用它造副本。
//
// 注意：返回 nil 入参的语义是"没有观察者"，因此**原样返回 c**（nil 进 nil 出），
// 不 panic，也不制造一个 nil 回调的副本 —— 后者会让调用方误以为挂上了。
func (c *Client) WithFallbackObserver(fn func(FallbackInfo)) *Client {
	if c == nil {
		return nil
	}
	if fn == nil {
		return c
	}
	clone := *c
	clone.onFallback = fn
	return &clone
}

// notifyFallback 触发回调。nil client / nil 回调时直接返回。
//
// 所有切换点都经过这里，而不是各自 `if c.onFallback != nil { c.onFallback(...) }`：
// 后者只要有一处忘了 recover，观察者 panic 就会把一次"备用模型本来能答"的请求
// 变成失败 —— 观测设施的故障不该是业务可用性的故障。
func (c *Client) notifyFallback(from, to string, status int) {
	if c == nil || c.onFallback == nil {
		return
	}
	info := FallbackInfo{
		FromModel:  from,
		ToModel:    to,
		Reason:     fallbackReason(status),
		StatusCode: status,
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				// 观察者崩了是观察者的问题；把证据留在日志里，但绝不影响请求。
				log.Printf("[api] fallback observer panic (已忽略): %v", r)
			}
		}()
		c.onFallback(info)
	}()
}

// fallbackReason 把状态码翻译成稳定的可读原因。
//
// 保留状态码本身（upstream_529 而不是笼统的 upstream_5xx）：529 与 500 的处置
// 完全不同 —— 前者是"过载，等一等/换模型"，后者更可能是网关自身 bug。把两者
// 合并成 upstream_5xx 之后，事后按原因聚合会把两类故障混成一个数。
func fallbackReason(status int) string {
	switch {
	case status == 402 || status == 403:
		return "quota"
	case status == 429:
		return "rate_limited"
	case status == 400:
		return "invalid_model"
	case status >= 500:
		return fmt.Sprintf("upstream_%d", status)
	case status > 0:
		return fmt.Sprintf("http_%d", status)
	default:
		// 非 HTTP 原因（连接失败、上游无响应等）。与 harness 侧的空原因规则保持
		// 同一个词，避免同一个语义出现两种拼写。
		return "fallback"
	}
}
