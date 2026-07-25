package api

// trace.go — LLM 调用的 trace 四元组 (design/02 §3.1 · design/03 轨迹底座)。
//
// 背景: pkg/llmgw 的访问日志早就留好了 run_id 字段 (server.go 读 X-CG-Run-ID),
// 但**全仓没有任何客户端发过这个头** —— 字段恒为空, 网关侧对不上账。本文件补上
// 出站的一半。
//
// 为什么走 context, 而不是给 StreamMessage/SendMessage/SimpleComplete 加参数,
// 也不是塞进 LLMMetricsContext:
//   - 三个出口的签名被十几个包直接依赖, 加参数要改几十处调用点, 漏一处就断链;
//   - trace 是"贯穿一次运行"的横切信息, 与 context 的取消/超时同一生命周期;
//   - LLMMetricsContext 是 QueryEngine 独占的 prompt 账本, 语义不同。混进去会让
//     "只想带个 RunID"的调用方被迫构造整份账本, 还会连带覆盖已有的
//     Source/Purpose/Role (WithLLMMetrics 是整体替换而非合并)。
//
// 只落请求头, 不改 LLMCallRecord: 本地 llm.jsonl 的字段集合是 Grafana 看板与
// /api/llm/stats 的既有契约, 不在本轮动。网关侧 access.jsonl 是新日志, 加字段安全。

import (
	"context"
	"net/http"
)

// trace 四元组的请求头名。命名沿用 pkg/llmgw/server.go 已在读的 X-CG-Run-ID,
// 其余三个按同一前缀补齐。
const (
	TraceHeaderRunID  = "X-CG-Run-ID"
	TraceHeaderNodeID = "X-CG-Node-ID"
	TraceHeaderTurnID = "X-CG-Turn-ID"
	TraceHeaderCallID = "X-CG-Call-ID"
)

// Trace 是 design/02 §3.1 要求 ChatRequest "必带"的四元组。
// 四个字段都是可选的: 只有 RunID 的调用方 (如 dashboard 的诊断作业) 照样有用,
// 网关侧能把同一次作业的多次调用归并到一起。
type Trace struct {
	RunID  string // 一次运行/作业
	NodeID string // 图节点 / 团队阶段
	TurnID string // 引擎回合
	CallID string // 单次 LLM 调用
}

// IsZero 四个字段全空。
func (t Trace) IsZero() bool {
	return t.RunID == "" && t.NodeID == "" && t.TurnID == "" && t.CallID == ""
}

type traceContextKey struct{}

// WithTrace 把 trace 四元组放进 context。
// 传入全空的 Trace 时原样返回 ctx —— 免得调用方要自己判空。
func WithTrace(ctx context.Context, t Trace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, traceContextKey{}, t)
}

// TraceFromContext 取出 trace 四元组; 未设置时返回零值。
func TraceFromContext(ctx context.Context) Trace {
	if ctx == nil {
		return Trace{}
	}
	if t, ok := ctx.Value(traceContextKey{}).(Trace); ok {
		return t
	}
	return Trace{}
}

// setTraceHeaders 把 ctx 里的 trace 写成出站请求头。
// 空字段不发头 (而不是发空值), 避免在直连 provider 的形态下多带无意义的头。
func setTraceHeaders(req *http.Request, ctx context.Context) {
	if req == nil {
		return
	}
	t := TraceFromContext(ctx)
	if t.IsZero() {
		return
	}
	for h, v := range map[string]string{
		TraceHeaderRunID:  t.RunID,
		TraceHeaderNodeID: t.NodeID,
		TraceHeaderTurnID: t.TurnID,
		TraceHeaderCallID: t.CallID,
	} {
		if v != "" {
			req.Header.Set(h, v)
		}
	}
}
