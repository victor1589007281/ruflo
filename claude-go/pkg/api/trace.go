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
	"strings"

	"github.com/anthropic/claude-go/pkg/trace"
)

// trace 四元组的请求头名。命名沿用 pkg/llmgw/server.go 已在读的 X-CG-Run-ID,
// 其余三个按同一前缀补齐。
const (
	TraceHeaderRunID  = "X-CG-Run-ID"
	TraceHeaderNodeID = "X-CG-Node-ID"
	TraceHeaderTurnID = "X-CG-Turn-ID"
	TraceHeaderCallID = "X-CG-Call-ID"
)

// SessionHeader 是 opencode zen/go 上游要求的会话头 (Claude Code CLI 的原生头名)。
//
// 它决定**提示缓存能不能命中**: opencode 用会话 ID 选路由与缓存分片, 同一个会话内
// 前缀不变就命中, 换会话就全额重算。实测同一段 4.4K token 前缀:
//
//	同会话第二次 → cache_read=4224, input=214
//	换会话第二次 → cache_read=0,    input=4438
//
// anthropic-gateway 的 opencodeSessionID() 优先透传本头、缺失才随机兜底, 所以
// **不发这个头 = 每次请求都是新会话 = 缓存命中率恒为 0**。这正是 opencode 路由下
// 全量 265 次 llm_call cache_read 全 0 的原因: 客户端从未发过它。
const SessionHeader = "X-Claude-Code-Session-Id"

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

// TraceFromContextAny 按 api.Trace → pkg/trace 的顺序找 ctx 里的 trace 四元组。
//
// 为什么要找**两个** key: 两条注入通道历史上是分开的 —— 团队/引擎路径一贯只注入
// pkg/trace (trace.With), 而 api.WithTrace 只有 llmgw 的诊断作业在用。setTraceHeaders
// 一直只认后者, 于是**团队路径的出站 X-CG-Run-ID 恒为空**, 而 llm.jsonl 里的 run_id
// 却是好的 —— 因为那个字段走的是 pkg/trace (client.go 的 stampTraceIDs)。
//
// 这个不一致在"会话头"上变成了实打实的损失: 会话头由本函数按 trace.RunID 派生,
// 团队路径喂不进来就退化成 pkg/api/sessionKeyForTrace 拿不到 RunID → **不发出站会话头**
// → 上游 opencode 侧 `opencodeSessionID()` 收不到 Claude Code 原生会话头 → 走 seed 兜底,
// 把所有行的请求压进同一个进程级会话分片。既拿不到跨 run 复用, 还让互不相干的团队
// 互相顶掉缓存条目。
//
// 顺序: api.Trace 优先 (显式调用方语义更强), 缺失字段用 pkg/trace 补 —— 不是整份
// 二选一, 否则团队路径下 NodeID/TurnID 依然传不出去。
func TraceFromContextAny(ctx context.Context) Trace {
	t := TraceFromContext(ctx)
	ids := trace.From(ctx)
	if t.RunID == "" {
		t.RunID = ids.RunID
	}
	if t.NodeID == "" {
		t.NodeID = ids.NodeID
	}
	if t.TurnID == "" {
		t.TurnID = ids.TurnID
	}
	if t.CallID == "" {
		t.CallID = ids.CallID
	}
	return t
}

// sessionKeyWithExplicit 在 sessionKeyForTrace 的基础上优先采用显式 SessionID。
//
// 分工: 本函数管"用哪个 ID 做会话键", sessionKeyForTrace 管"这个 ID 要不要再并进
// NodeID"(后者是 sessionshard.go 的 A/B 实验, 0% 时逐字节等价于只认 RunID)。
//
// 显式 SessionID 是**跨 run 稳定**的业务会话身份, 正是提示缓存要的分片粒度;
// 没给就完全退回原行为, 一个字节不差。
func sessionKeyWithExplicit(t Trace, explicit string) string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit
	}
	return sessionKeyForTrace(t)
}

// setTraceHeaders 把 ctx 里的 trace 写成出站请求头。
// 空字段不发头 (而不是发空值), 避免在直连 provider 的形态下多带无意义的头。
func setTraceHeaders(req *http.Request, ctx context.Context) {
	if req == nil {
		return
	}
	t := TraceFromContextAny(ctx)
	if t.IsZero() {
		return
	}
	// 会话头 (缓存命中与否的关键, 见 SessionHeader 注释): 会话 ID 怎么算由
	// sessionKeyForTrace 决定 —— 默认就是 RunID (同一次 run 内恒定, 所以前缀不变
	// 的那些轮能命中同一份缓存; 跨 run 会变, 而跨 run 的 system 前缀本来就不一样
	// (objective 里带 team_name), 该重建)。
	//
	// 会话 ID 在上游同时是**路由亲和键**, 所以"把 NodeID 也并进会话键"是一个有
	// 反向代价的实验 —— 见 sessionshard.go 的包注释。实验由
	// CLAUDE_GO_SESSION_SHARD 驱动, 占比 0 时本函数的行为与改造前逐字节一致。
	//
	// 放在这里而不是各调用点: 三个请求出口 (流式/非流式/降级) 都走本函数,
	// 而 applyExtraHeaders 在其后执行 —— 调用方显式注入的会话头仍能覆盖这个默认值。
	if sid := sessionKeyWithExplicit(t, trace.From(ctx).SessionID); sid != "" {
		req.Header.Set(SessionHeader, sid)
		// 臂标识随行: 归因靠显式证据, 不靠事后按哈希重算对账。
		req.Header.Set(SessionArmHeader, string(SessionArmForTrace(t)))
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
