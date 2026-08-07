package agent

// subagent_span.go —— 让**图外派生**对编排层可见 (design/01 §4.8 的 M4 剩余项)。
//
// ---------------------------------------------------------------------------
// 缺口是什么
// ---------------------------------------------------------------------------
//
// §4.8 的 `SpawnSubgraph` 只覆盖**图内**派生: 节点里的 agent 调
// `NodeInput.Spawn` 派生子图, 复用同一个 `rc` ⇒ 白拿 NodeID / journal / hook /
// 预算。但生产里 99% 的派生走的不是它, 而是 **Agent 工具**:
//
//	pkg/feishu/session.go:390,621  runAgentFn → sm.runNestedAgent → 裸 QueryEngine
//	cmd/claude-go/main.go:3065     runAgent   → runNestedAgent   → 裸 QueryEngine
//
// 两条路径都对编排层完全不可见 —— 一个图节点里的 agent 可以在 Agent 工具里烧掉
// 任意多 token、跑任意久, 图这一层看到的只有该节点的最终产出。
//
// ---------------------------------------------------------------------------
// 为什么不是"把它们换成 SpawnSubgraph"(设计稿的字面要求)
// ---------------------------------------------------------------------------
//
// `SpawnSubgraph` 的三样好处全部来自"复用同一个 `rc`"(`pkg/graph/spawn.go`:
// `newNodeSpawner(e, rc, scope, node, in)`)。而 `rc` 是 `Engine.Run` 内部构造的
// 运行上下文, **只在图运行期间存在**:
//
//   - 飞书会话路径压根没有图。用户在聊天里说一句话 → `QueryEngine` 直接跑,
//     没有 `Engine.Run`、没有 `runCtx`、没有 journal、没有 GraphSpec。
//   - CLI headless 同理 (`claude-go -p "..."` 那条主路径)。
//   - 即使派生发生在图节点**内部**, `rc` 也不在 ctx 上: 图侧只把 trace 四元组与
//     hook 观测者放进 ctx (`execNode` 的 `trace.With` / `withInternalHookBridge`),
//     `rc` 从未离开过 `pkg/graph`。要把它传到 Agent 工具, 就得让 `pkg/graph` 把
//     内部运行状态经 ctx 暴露给外层 —— 那正是 §4.9 拒绝过的"内核反向依赖上层"。
//
// 所以这里做的是**可见性收编**而不是执行路径收编: 派生仍由裸 QueryEngine 跑
// (行为一字不变), 但它在编排层留下**三条可查的痕迹**。真正的执行路径收编需要
// 会话路径先有图 (design/01 §4.1 "会话即单节点图"), 那是另一件事。
//
// ---------------------------------------------------------------------------
// 三条痕迹, 以及一处刻意的"不动"
// ---------------------------------------------------------------------------
//
//  1. **轨迹**: 每次派生写一条 `tracestore.KindNode` Span, `attrs.kind=subagent`。
//     用 KindNode 而不是新造第 8 种 Kind: 子代理正是"有角色、有输入、有产出、有
//     耗时"的一个执行单元 = node 语义; 而每加一种 Kind, 下游 (蒸馏/回放/操作台)
//     的 switch 都要跟着长一个分支, 漏一个就是静默丢数据。`attrs.kind` 这一槽位
//     本来装的就是 `graph.NodeKind` ("agent"/"gate"/…), 多一个 "subagent" 取值
//     不需要任何消费方改代码。
//
//  2. **hook 总线**: 经 ctx 上的 `internal_hook.Observer` 打一条事件。派生若发生
//     在图节点内, 这条事件会被 `graph_internal_bridge.go` 桥进**同一条**
//     `graph.HookBus`, 于是节点收尾日志里出现 `Subagent=N` —— "这个阶段派生了几个
//     子代理"第一次在图层可见。不在图内跑时 ctx 无观测者 ⇒ 零成本 no-op。
//
//     为什么不让 pkg/agent 直接持有 `graph.HookBus`: 总线是**每次团队运行一条**
//     (`runGraphSpec` 里造), 而 `AgentTool` 随会话构造一次长期复用 —— 挂成字段就
//     要在每次运行前后改同一个实例的状态, 多个团队并发时互相串台。这与
//     `internal_hook/bus.go` 文件头第 2 点是同一条理由, 所以复用同一个 ctx 通道。
//
//  3. **派生深度**: 经 ctx 累加并进 Span。CLI 那条路径给子代理**又注册了一次**
//     Agent 工具 (`main.go:3454` 的 `nestedReg.Register(...)`),
//     递归无限深; 飞书那条不注册 (`registryOptions{}` 的 includeAgent=false) 所以
//     天然只有一层。深度记下来才能事后区分这两种形态。
//
// ⚠️ **一处刻意的"不动": ctx 里的 trace 四元组一个字都不改。**
//
// 直觉做法是给子代理一个自己的 NodeID 并 `trace.With` 回 ctx —— 那样嵌套引擎的
// llm_call Span 就挂在子代理名下, 看起来更"干净"。**但那会把子代理的 token 从父
// 节点的台账里搬走**: `api.TokenLedger` 按 `(RunID, NodeID)` 记账 (来自
// `client.go:1110` 的 `trace.From(ctx)`), 而图侧的 `tokens` 拦截器取的正是父节点
// 执行前后在该键上的差值 (`graph_interceptors.go:340`)。改了 NodeID, 父节点的
// `BudgetManager` MaxTokens 闸就**再也看不见子代理烧的钱** —— 那恰好是 §4.8 要修的
// 那个洞, 一次"更好看的归因"会把它重新打开。
//
// 于是取舍是: **Span 上带派生 NodeID (可归因), ctx 里的 trace 四元组不动 (可计费)**。
// Span 的 `NodeID` 字段仍填父节点 (与 llm_call/tool_call 对得上), 派生身份放在
// `Name` 与 `attrs.spawn_node` 里。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// SubagentIDInfix 图外派生的命名空间中缀: `<父节点>~sa<指纹>`。
//
// 与图内派生的 `graph.SpawnIDInfix`("~sp") 并列而不复用同一个中缀: 两者可恢复性
// 完全不同 —— `~sp` 命名空间下的节点真的进 journal、真能 resume 命中缓存; `~sa`
// 只是一条轨迹标签, 重跑必然重烧。用同一个中缀会让读者 (和将来的 resume 逻辑)
// 以为它们是一类东西。
const SubagentIDInfix = "~sa"

// subagentHookName 总线上这条事件的 hook 名 (teamGraphHooks 按它聚合计数)。
const subagentHookName = "Subagent"

// subagentSpanKind 写进 `attrs.kind` 的取值, 见文件头第 1 点。
const subagentSpanKind = "subagent"

// subagentDepthKey ctx 键: 当前已经在第几层子代理里。
type subagentDepthKey struct{}

// SubagentDepth 读当前派生深度。0 = 不在任何子代理里 (主循环)。
func SubagentDepth(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	d, _ := ctx.Value(subagentDepthKey{}).(int)
	return d
}

// withSubagentDepth 把深度写进 ctx。
//
// 只加一个 ctx 值、不碰 trace 四元组 —— 见文件头"一处刻意的不动"。
func withSubagentDepth(ctx context.Context, depth int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, subagentDepthKey{}, depth)
}

// SetTraceStore 注入轨迹底座 (装配期调用; nil = 不采集, 与改造前一字不变)。
//
// 用 setter 而不是只在构造函数里传: CLI 装配处 (`main.go`) 的 `TraceStore` 是在
// 工具注册**之后**才构造出来的 (它要先算出 stateDir), 构造时传不进去。飞书侧
// 两者都可用, 走 NewAgentToolWithTrace。
func (t *AgentTool) SetTraceStore(ts *tracestore.Store) {
	if t == nil {
		return
	}
	t.trace = ts
}

// NewAgentToolWithTrace 带轨迹采集的 Agent 工具 (design/01 §4.8 图外派生可见性)。
func NewAgentToolWithTrace(runFn RunAgentFunc, ts *tracestore.Store) *AgentTool {
	return &AgentTool{runAgent: runFn, trace: ts}
}

// observeSubagent 一次图外派生的三条痕迹 (轨迹 / hook 总线 / 深度)。
//
// **fail-open 且绝不改变返回值**: 观测是观测, 派生成败与产出由 Call 原样透传。
// 这与 writeNodeSpan/writeGateSpan 同一条纪律 (graph_adapter.go:1595)。
//
// 包级函数而非 *AgentTool 方法: AgentTool 与 DelegateTool (delegate_task, 见
// delegate_tool.go) 都派生子代理, 挂一处就覆盖两条路径, 将来新增入口自动获得
// 同样的可见性。trace 可空 (未注入轨迹底座时整套采集零成本 no-op)。
func observeSubagent(ts *tracestore.Store, ctx context.Context, in agentInput, depth int,
	out string, runErr error, start time.Time, tokensBefore int64) {
	status := "success"
	errText := ""
	if runErr != nil {
		status = "error"
		errText = runErr.Error()
	}

	// ① hook 总线 (经 ctx 观测者; 不在图内跑时为 nil ⇒ 零成本)。
	// 只在**结束时**发一条而不是 pre/post 两条: teamGraphHooks 对 turn|tool 作用域
	// 只做计数, 发两条会让 "Subagent=2" 实际只是一次派生, 那种计数没人敢用。
	if obs := internal_hook.ObserverFrom(ctx); obs != nil {
		obs.Observe(ctx, internal_hook.Event{
			Scope: "tool", // 委派工具确实是一次工具调用; 与 internal_hook.scopeOf 同口径
			Phase: "SubagentDone",
			Hook:  subagentHookName,
			Err:   errText,
		})
	}

	// ② 轨迹。
	if ts == nil {
		return
	}
	ids := trace.From(ctx)
	attrs := map[string]any{
		"kind":       subagentSpanKind,
		"status":     status,
		"depth":      depth,
		"spawn_node": SubagentNodeID(ids.NodeID, in.Prompt),
		"path":       "bare-engine", // 与图内派生 (~sp) 区分: 这条不进 journal、不可 resume
	}
	if in.SubagentType != "" {
		attrs["role"] = in.SubagentType
	}
	if in.Model != "" {
		attrs["model"] = in.Model
	}
	if in.ReadOnly {
		attrs["readonly"] = true
	}
	if errText != "" {
		attrs["error"] = errText
	}
	// token 归因: 取本次派生前后父节点台账的差值。
	//
	// 台账只有在图侧 `tokens` 拦截器被显式打开时才收数 (见 graph_interceptors.go
	// 文件头"默认开的是哪些"), 未开时恒为 0 —— 那时**不写这个字段**, 而不是写 0:
	// 写 0 会让"没开台账"和"真的一个 token 都没烧"变得不可区分, 那正是本仓反复
	// 吃过的假数据形态。
	if delta := api.DefaultTokenLedger.Tokens(ids.RunID, ids.NodeID) - tokensBefore; delta > 0 {
		attrs["tokens"] = delta
	}

	ts.Write(tracestore.Span{
		TraceID: ids.RunID,
		SpanID:  trace.NewID("s"),
		// ParentID 优先挂父节点: 这样"这个节点里派生了什么"在 Span 树上是一条边,
		// 而不是要靠 attrs 反查。节点外的会话派生退回 RunID。
		ParentID:  firstNonEmpty(ids.NodeID, ids.RunID),
		Kind:      tracestore.KindNode,
		Name:      subagentSpanName(in),
		NodeID:    ids.NodeID, // 刻意=父节点, 与同一次执行的 llm_call/tool_call 对得上
		TurnID:    ids.TurnID,
		InputRef:  ts.MakeRef(in.Prompt),
		OutputRef: ts.MakeRef(out),
		Attrs:     attrs,
		TS:        start.UnixMilli(),
		DurMS:     time.Since(start).Milliseconds(),
	})
}

// subagentTokensBefore 派生前父节点的台账读数 (台账未启用时恒 0)。
func subagentTokensBefore(ctx context.Context) int64 {
	ids := trace.From(ctx)
	return api.DefaultTokenLedger.Tokens(ids.RunID, ids.NodeID)
}

// subagentSpanName Span 的可读名: 优先 subagent_type, 其次 description。
// 都没有时用固定名而不是 prompt 前缀 —— prompt 每次不同会让同类派生在按 Name
// 聚合的视图里碎成 N 个条目。
func subagentSpanName(in agentInput) string {
	return firstNonEmpty(in.SubagentType, in.Description, "subagent")
}

// SubagentNodeID 派生的限定 ID: `<父节点>~sa<prompt 指纹>`。
//
// 指纹而非序号 —— 与 `pkg/graph/spawn.go` 的 `spawnFingerprint` 同一条理由:
// 父节点重跑时序号从 0 重来, 而 agent 这次可能派生的是**另一个**任务, 同一个
// `~sa0` 下的两条轨迹会被事后分析当成"同一个子代理跑了两次"。
//
// 父节点为空 (纯会话派生) 时只返回 `~sa<指纹>`, 不硬造一个假父节点名。
func SubagentNodeID(parentNodeID, prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return parentNodeID + SubagentIDInfix + hex.EncodeToString(sum[:4])
}
