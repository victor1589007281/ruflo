// trace_compaction.go —— compaction Span 采集 (H2-1)。
//
// # 为什么单开一种 kind
//
// 折叠是**改变后续所有轮次输入**的事件: 一次 run 里前 40 轮看得见的东西, 第 41 轮
// 之后看不见了。长程任务「模型忘了前面做过什么」的归因, 第一步永远是「那件事是在
// 哪次折叠里掉出去的」。在此之前折叠路径上**没有任何一处**记录过"压了多少、省了
// 多少" —— 上下文预算打满时说不清 token 是被折叠省下来的还是被工具结果吃掉的,
// 模型答非所问时也分不清是摘要写得差还是折叠根本没发生。
//
// # 与 llm_call Span 的关系
//
// 折叠摘要本身是一次 LLM 调用, 所以它**同时**会产出一条 llm_call Span (走
// writeLLMCallSpan)。两条 Span 不重复: llm_call 记的是"那次请求/响应长什么样",
// compaction 记的是"这次折叠把上下文从 A 变成了 B"。前者的 usage 是**摘要请求**
// 的用量, 后者才是**上下文本身的**前后体量 —— 两者量纲不同, 混为一谈正是此前
// 无法回答"折叠省了多少"的原因。
package engine

import (
	"context"
	"time"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// compactionAttrs 把一次折叠读数摊成 Span attrs。
//
// **摊平而不是嵌套对象**: 消费方 (会话页 / 进化账本 / grep 轨迹文件的脚本) 读的是
// `attrs["tokens_saved"]`, 嵌一层 `attrs["compaction"]["tokens_saved"]` 会让所有
// 现成的取值代码多一跳, 而这一跳的收益 (命名空间) 在 attrs 这个扁平字典里不存在。
// llm_call Span 的 token 字段同样摊平, 保持一致。
//
// **rate 只报"没省"不报"省了百分之多少"**: 比值 (saved/before) 是消费方一秒就能
// 算出来的派生量, 而落盘一个比率会诱使下游把它当**原始读数**引用 —— 分母换了 (口径
// 从 estimateTokens 改成真实 usage) 比率就跟着变, 而读它的人不会知道。saved 的值
// 是 may be negative (折叠后更大), 这正是要如实报的事实。
func compactionAttrs(ev internal_hook.CompactionEvent) map[string]any {
	st := ev.Stats
	attrs := map[string]any{
		"stage":  ev.Stage,
		"status": "ok",

		// 条数: 折了多少条 / 摘要替掉多少条 / 尾部保护多少条。
		"messages_before":    st.MessagesBefore,
		"messages_after":     st.MessagesAfter,
		"messages_compacted": st.MessagesCompacted,
		"tail_protected":     st.TailCount,

		// token: 前后两个绝对值 + 差值。**两者都必须非零才是有效读数** ——
		// TokensBefore 为 0 说明要么消息是空的 (那不该触发折叠), 要么口径漏算
		// (例如只数 Text 不数 tool_result 体)。
		"tokens_before":  st.TokensBefore,
		"tokens_after":   st.TokensAfter,
		"tokens_saved":   st.TokensBefore - st.TokensAfter,
		"summary_tokens": st.SummaryTokens,

		// 引用保全: 折了几条 tool_result / 其中几条留下可 Read 回读的路径 /
		// 几条因自指保护刻意跳过。Folded-Refs 就是**永久丢失**的结果条数。
		//
		// 注意 fold 阶段这三个数恒为 0 且**这不是缺陷**: 被摘要掉的历史里的
		// tool_result 本就随历史消失 (折叠边界产物只有 boundary+summary+tail,
		// 不含 tool_result)。真正给出文件引用的是 micro 阶段。把它报出来正是
		// 为了让这个区别在轨迹上直接可见。
		"stub_folded":       st.Stub.Folded,
		"stub_refs":         st.Stub.Refs,
		"stub_skipped_refs": st.Stub.SkippedRefReads,

		// H2-2 硬约束自检读数。
		//
		// **constraints_ok 只在"确实有可核验的约束"时才为 true**: 没有约束时
		// 它恒为 true (空集合上无遗漏), 那种"真的通过"与"没东西可验"同形 ——
		// 所以三个基数 (constraints / verifiable / missing) 必须一起读, 单看
		// 一个布尔值会把"核验闸门根本没通电"读成"全部通过"。
		"constraints":            st.Constraints,
		"constraints_verifiable": st.ConstraintsVerifiable,
		"constraints_missing":    st.ConstraintsMissing,
		"constraints_checked":    st.ConstraintsVerifiable > 0,
		"constraints_ok":         st.ConstraintsMissing == 0,
		"recompacted":            st.Recompacted,
	}
	// 引用保全率只在 micro 阶段有意义 (fold 阶段分母恒 0)。
	if ev.Stage == internal_hook.CompactionStageMicro {
		attrs["refs_lost"] = st.Stub.Folded - st.Stub.Refs
	}
	return attrs
}

// writeCompactionSpan 写一条 compaction Span。
//
// TraceStore 未装配时零成本 no-op —— 与 writeLLMCallSpan 同一守卫。ids 由 ctx 携带
// (trace.From), 与 turn / tool_call / llm_call 同一个四元组, 这样"这条折叠发生在哪个
// run / 哪个节点的第几轮"不需要额外字段就能 join。
//
// fail-open 是硬要求: 采集是观测不是治理。TraceStore.Write 本身不返回 error, 但
// ctx 可能为 nil (折叠回调在 Engine 停机路径上也可能触发), 所以先取 ids 再判。
func (e *QueryEngine) writeCompactionSpan(ctx context.Context, ev internal_hook.CompactionEvent) {
	if e == nil || e.TraceStore == nil {
		return
	}
	ids := trace.From(ctx)
	// F6 折叠窗口门: 本函数**必须**在窗口关闭之后被调用 (pkg/compact 的
	// runCompaction 刻意把回调排在锁外), 否则读锁撞写锁死锁。这层 GateEventWrite
	// 不是防止重入, 是保证这条 Span 落在事件链的折叠后一侧 —— 与 llm_call 同一
	// 理由: 横跨折叠边界的事件链会让"折叠前/后"这个二分失去意义。
	e.Compactor.GateEventWrite(func() {
		e.TraceStore.Write(tracestore.Span{
			TraceID:  ids.RunID,
			SpanID:   internal_hook.GenerateUUID(),
			ParentID: ids.TurnID,
			Kind:     tracestore.KindCompaction,
			Name:     ev.Stage,
			NodeID:   ids.NodeID,
			TurnID:   ids.TurnID,
			Attrs:    compactionAttrs(ev),
			TS:       time.Now().UnixMilli(),
		})
	})
}

// 编译期断言: *QueryEngine 满足 internal_hook.CompactionSink。
//
// 写在采集侧而不是接口侧 —— 接口定义在 internal_hook, 而实现方向是 engine 单向
// 依赖 internal_hook (反向 import 成环)。断言放在这里让"改坏了签名"在编译期就炸,
// 而不是等到运行时 hook 静默不报数。
var _ internal_hook.CompactionSink = (*QueryEngine)(nil)

// RecordCompaction 实现 internal_hook.CompactionSink。
func (e *QueryEngine) RecordCompaction(ctx context.Context, ev internal_hook.CompactionEvent) {
	e.writeCompactionSpan(ctx, ev)
}
